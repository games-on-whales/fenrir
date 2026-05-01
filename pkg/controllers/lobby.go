package controllers

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"

	"games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	v1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

var (
	WOLF_IMAGE = func() string {
		if im := os.Getenv("WOLF_IMAGE"); im != "" {
			return im
		}
		return "ghcr.io/games-on-whales/wolf:stable"
	}()
)

type K8sObject interface {
	metav1.Object
	runtime.Object
}

// LobbyControllerOptions holds configuration for the lobby controller.
type LobbyControllerOptions struct {
	WolfAgentImage           string
	WolfAgentImagePullPolicy string // for debug / local testing / slow internet connections
	LBSharingKey             string
}

// LobbyController manages the shared infrastructure for a Lobby.
// It is responsible for:
//   - 1. Setting up service, pods, PVCs, etc. for the lobby
//   - 2. Reconciling the wolf lobby state via wolf-agent API
//   - 3. Allocating ports for RTSP, ENet, Video RTP, Audio RTP
//   - 4. Cleaning up all resources when lobby is deleted (via owner references)
type LobbyController struct {
	LobbyClient   v1alpha1client.LobbyInterface
	LobbyInformer generic.Informer[*v1alpha1types.Lobby]
	AppInformer   generic.Informer[*v1alpha1types.App]
	UserInformer  generic.Informer[*v1alpha1types.User]

	K8sClient kubernetes.Interface

	controller           generic.Controller[*v1alpha1types.Lobby]
	deploymentController generic.Controller[*appsv1.Deployment]

	WolfAgentImage           string
	WolfAgentImagePullPolicy string
	LBSharingKey             string
}

// NewLobbyController creates a new lobby controller.
func NewLobbyController(
	k8sClient kubernetes.Interface,
	lobbyClient v1alpha1client.LobbyInterface,
	lobbyInformer generic.Informer[*v1alpha1types.Lobby],
	appInformer generic.Informer[*v1alpha1types.App],
	userInformer generic.Informer[*v1alpha1types.User],
	deploymentInformer generic.Informer[*appsv1.Deployment],
	options LobbyControllerOptions, // Reusing existing options struct for simplicity
) *LobbyController {
	res := &LobbyController{
		K8sClient:                k8sClient,
		LobbyClient:              lobbyClient,
		LobbyInformer:            lobbyInformer,
		AppInformer:              appInformer,
		UserInformer:             userInformer,
		WolfAgentImage:           options.WolfAgentImage,
		WolfAgentImagePullPolicy: options.WolfAgentImagePullPolicy,
		LBSharingKey:             options.LBSharingKey,
	}

	res.controller = generic.NewController(
		lobbyInformer,
		res.Reconcile,
		generic.ControllerOptions{
			Name:    "lobby-controller",
			Workers: 2,
		},
	)
	//!TODO: Also watch any udproutes, services, deployments, etc. that we create
	// and re-reconcile their sessions when they change.
	res.deploymentController = generic.NewController(
		deploymentInformer,
		func(namespace, name string, newObj *appsv1.Deployment) error {
			// Load bearing. If we pass nil it will be casted to interface and
			// not be comparable with nil :)
			if newObj == nil {
				return nil
			}
			return res.reconcileDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "lobby-controller-deployment",
			Workers: 1,
		},
	)

	return res
}

func (c *LobbyController) Run(ctx context.Context) error {
	lobbyCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(lobbyCtx.Done(), c.LobbyInformer.HasSynced) {
		return fmt.Errorf("failed to sync lobby informer")
	}

	go func() {
		defer cancel()
		err := c.deploymentController.Run(lobbyCtx)
		if err != nil {
			klog.Errorf("Failed to run deployment controller: %v", err)
		}
	}()

	return c.controller.Run(lobbyCtx)
}

func (c *LobbyController) HasSynced() bool {
	return c.LobbyInformer.HasSynced()
}

func (c *LobbyController) reconcileDependant(obj K8sObject) error {
	// If object doesnt have direwolf/user and direwolf/app labels, skip
	if obj.GetLabels() == nil {
		return nil
	}

	if _, ok := obj.GetLabels()["direwolf/lobby"]; !ok {
		return nil
	}
	klog.Infof("Reconciling dependant %s %s/%s", obj.GetObjectKind().GroupVersionKind().String(), obj.GetNamespace(), obj.GetName())
	// Lookup sessions associated with his object
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == "Lobby" {
			klog.Infof("Found owner %s/%s", owner.Name, owner.UID)
			c.controller.Enqueue(obj.GetNamespace(), owner.Name)
		}
	}
	return nil
}

// Reconcile handles the main reconciliation loop for a Lobby object.
func (c *LobbyController) Reconcile(namespace, name string, newObj *v1alpha1types.Lobby) error {
	klog.Infof("Reconciling lobby %s/%s", namespace, name)
	defer klog.Infof("Finished Reconciling lobby %s/%s", namespace, name)

	if newObj == nil {
		// Session was deleted. Stuff will be garbage collected by Kubernetes
		// due to owner references. Nothing to do.
		return nil
	}

	// Make a deep copy to compare later for status updates
	oldStatus := newObj.Status.DeepCopy()

	if err := c.allocatePorts(newObj); err != nil {
		klog.Errorf("Failed to allocate ports: %v", err)
		return err
	}

	if err := c.reconcilePVC(context.TODO(), newObj); err != nil {
		klog.Errorf("Failed to reconcile pvc: %v", err)
		return err
	}

	if err := c.reconcilePod(context.TODO(), newObj); err != nil {
		klog.Errorf("Failed to reconcile pod: %v", err)
		return err
	}

	if err := c.reconcileService(context.TODO(), newObj); err != nil {
		klog.Errorf("Failed to reconcile service: %v", err)
		return err
	}
	// Keeping this here for later
	// Gateway not yet supported
	// if gatewayError := c.reconcileGateway(context.TODO(), newObj); gatewayError != nil {
	// 	klog.Errorf("Failed to reconcile gateway: %s", gatewayError)
	// 	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
	// 		Type:    "RoutesCreated",
	// 		Status:  metav1.ConditionFalse,
	// 		Reason:  "GatewayConfigurationFailed",
	// 		Message: gatewayError.Error(),
	// 	})
	// } else {
	// 	meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
	// 		Type:   "RoutesCreated",
	// 		Status: metav1.ConditionTrue,
	// 		Reason: "Success",
	// 	})
	// }

	if err := c.reconcileWolfLobby(context.TODO(), newObj); err != nil {
		klog.Errorf("Failed to reconcile wolf lobby: %v", err)
		return err
	}

	// Update status if changed
	if !reflect.DeepEqual(newObj.Status, *oldStatus) {
		_, err := c.LobbyClient.UpdateStatus(
			context.TODO(),
			newObj,
			metav1.UpdateOptions{
				FieldManager: "lobby-controller-status",
			},
		)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func (c *LobbyController) deploymentName(lobby *v1alpha1types.Lobby) string {
	return lobby.Name
}

// allocatePorts assigns fixed ports for the lobby's streaming endpoints.
// !TODO: Implement this properly once wolf lets us assign ports. For now, just
// hardcode some ports.
func (c *LobbyController) allocatePorts(lobby *v1alpha1types.Lobby) error {
	// Only assign if they are not assigned yet.
	if lobby.Status.Ports.RTSP == 0 {
		lobby.Status.Ports = v1alpha1types.SessionPorts{
			RTSP:     48010,
			Control:  47999,
			VideoRTP: 48100,
			AudioRTP: 48200,
		}
	}
	return nil
}

// reconcilePVC ensures a PersistentVolumeClaim exists for the lobby if the
// App defines a VolumeClaimTemplate.
func (c *LobbyController) reconcilePVC(ctx context.Context, lobby *v1alpha1types.Lobby) error {
	user, err := c.UserInformer.Namespaced(lobby.Namespace).Get(lobby.Spec.ProfileReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user: %w", err)
	}
	app, err := c.AppInformer.Namespaced(lobby.Namespace).Get(lobby.Spec.AppReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}

	// Check if the user defined a volume claim template. If not, return nil.
	if app.Spec.VolumeClaimTemplate == nil {
		return nil
	}

	pvcName := c.deploymentName(lobby)
	templateSpec := app.Spec.VolumeClaimTemplate.Spec.DeepCopy()

	// Default Access Mode: RWO
	if len(templateSpec.AccessModes) == 0 {
		templateSpec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}

	// Default Storage: 5Gi
	if templateSpec.Resources.Requests == nil {
		templateSpec.Resources.Requests = make(corev1.ResourceList)
	}
	if _, ok := templateSpec.Resources.Requests[corev1.ResourceStorage]; !ok {
		templateSpec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("5Gi")
	}

	pvcSpec := v1ac.PersistentVolumeClaimSpec().
		WithAccessModes(templateSpec.AccessModes...).
		WithResources(v1ac.VolumeResourceRequirements().
			WithLimits(templateSpec.Resources.Limits).
			WithRequests(templateSpec.Resources.Requests))

	if templateSpec.StorageClassName != nil {
		pvcSpec.WithStorageClassName(*templateSpec.StorageClassName)
	}

	_, err = c.K8sClient.CoreV1().PersistentVolumeClaims(lobby.Namespace).Apply(
		ctx,
		v1ac.PersistentVolumeClaim(pvcName, lobby.Namespace).
			WithLabels(map[string]string{
				"app":            "direwolf-worker",
				"direwolf/lobby": lobby.Name,
				"direwolf/app":   lobby.Spec.AppReference.Name,
				"direwolf/user":  lobby.Spec.ProfileReference.Name,
			}).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithName(user.Name).
				WithAPIVersion(v1alpha1.GroupVersion.String()).
				WithKind("User").
				WithUID(user.UID).
				WithController(true)).
			WithSpec(pvcSpec),
		metav1.ApplyOptions{
			FieldManager: "direwolf-lobby-controller-pvc",
		},
	)

	return err
}

// reconcileService ensures a LoadBalancer Service exists for the lobby's
// streaming ports (wolf-agent, RTSP, ENet, Video RTP, Audio RTP).
func (c *LobbyController) reconcileService(ctx context.Context, lobby *v1alpha1types.Lobby) error {
	clampString := func(s string, max int) string {
		if len(s) > max {
			return s[:max]
		}
		return s
	}

	lobby.Status.ServiceName = fmt.Sprintf("%s-rtp", clampString(lobby.Name, 56))

	// 1. Use the set up a service with correct ports pointing to the pods
	_, err := c.K8sClient.CoreV1().
		Services(lobby.Namespace).
		Apply(
			ctx,
			v1ac.Service(lobby.Status.ServiceName, lobby.Namespace).
				WithAnnotations(map[string]string{
					// Try to support popular service LoadBalancer implementation
					// sharing key annotations.
					"lbipam.cilium.io/sharing-key":        c.LBSharingKey,
					"metallb.universe.tf/allow-shared-ip": c.LBSharingKey,
				}).
				WithLabels(
					map[string]string{
						"app":            "direwolf-worker",
						"direwolf/lobby": lobby.Name,
					},
				).
				WithOwnerReferences(metav1ac.OwnerReference().
					WithName(lobby.Name).
					WithAPIVersion(v1alpha1.GroupVersion.String()).
					WithKind("Lobby").
					WithUID(lobby.UID).
					WithController(true)).
				WithSpec(
					v1ac.ServiceSpec().
						WithType(corev1.ServiceTypeLoadBalancer).
						WithSelector(
							map[string]string{
								"direwolf/lobby": lobby.Name,
							}).
						WithPorts(
							v1ac.ServicePort().WithName("wa").WithPort(8443),
							v1ac.ServicePort().WithName("rtsp").WithPort(lobby.Status.Ports.RTSP),
							v1ac.ServicePort().WithName("enet").WithProtocol(corev1.ProtocolUDP).WithPort(lobby.Status.Ports.Control),
							v1ac.ServicePort().WithName("video").WithProtocol(corev1.ProtocolUDP).WithPort(lobby.Status.Ports.VideoRTP),
							v1ac.ServicePort().WithName("audio").WithProtocol(corev1.ProtocolUDP).WithPort(lobby.Status.Ports.AudioRTP),
						),
				),
			metav1.ApplyOptions{
				FieldManager: "direwolf-lobby-controller-svc",
			})
	return err
}

// reconcilePod builds and applies the Deployment for the lobby. It composes
// the user's app template with the required wolf sidecars (wolf-agent,
// pulseaudio, wolf).
func (c *LobbyController) reconcilePod(ctx context.Context, lobby *v1alpha1types.Lobby) error {
	user, err := c.UserInformer.Namespaced(lobby.Namespace).Get(lobby.Spec.ProfileReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user %s: %w", lobby.Spec.ProfileReference.Name, err)
	}

	app, err := c.AppInformer.Namespaced(lobby.Namespace).Get(lobby.Spec.AppReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}

	deploymentName := c.deploymentName(lobby)
	ownerApply := []*metav1ac.OwnerReferenceApplyConfiguration{
		metav1ac.OwnerReference().
			WithName(lobby.Name).
			WithAPIVersion(v1alpha1.GroupVersion.String()).
			WithKind("Lobby").
			WithUID(lobby.UID).
			WithController(true),
	}

	// Update existing metadata if it already exists
	if _, err := c.deploymentController.Informer().Namespaced(lobby.Namespace).Get(deploymentName); err == nil {
		_, err := c.K8sClient.AppsV1().Deployments(lobby.Namespace).Apply(
			ctx,
			appsv1ac.Deployment(deploymentName, lobby.Namespace).
				WithOwnerReferences(ownerApply...),
			metav1.ApplyOptions{
				FieldManager: "direwolf-lobby-controller-deployment-owners",
			})
		return err
	}

	// Prepare environment variables for the wolf container
	// commenting these out was the main reason nvidia stuff wasn't working.
	// specifically the NVIDIA_VISIBLE_DEVICES
	// I need a better method of injecting env vars / configs to the pod
	// TODO
	wolfEnvVars := map[string]string{
		"PUID":                   "1000",
		"PGID":                   "1000",
		"UNAME":                  "ubuntu",
		"XDG_RUNTIME_DIR":        "/tmp/.X11-unix",
		"PULSE_SERVER":           "unix:/tmp/.X11-unix/pulse-socket",
		"HOST_APPS_STATE_FOLDER": "/mnt/data/wolf",
		"WOLF_SOCKET_PATH":       "/etc/wolf/wolf.sock",
		"WOLF_LOG_LEVEL":         "TRACE",
	}

	// Check if runtime variables are defined in the App spec and override defaults
	if app.Spec.WolfConfig.RuntimeVariables != nil {
		runtimeVars := app.Spec.WolfConfig.RuntimeVariables
		if runtimeVars.TimeZone != "" {
			wolfEnvVars["TZ"] = runtimeVars.TimeZone
		}
		if runtimeVars.RenderNode != "" {
			wolfEnvVars["WOLF_RENDER_NODE"] = runtimeVars.RenderNode
		}
	}

	// Create pod from pod template
	var podToCreate corev1.PodTemplateSpec
	if app.Spec.Template != nil {
		podToCreate.ObjectMeta = app.Spec.Template.ObjectMeta
		podToCreate.Spec = *app.Spec.Template.Spec.DeepCopy()
	}

	if podToCreate.Labels == nil {
		podToCreate.Labels = map[string]string{}
	}

	podToCreate.Labels["app"] = "direwolf-worker"
	podToCreate.Labels["direwolf/lobby"] = lobby.Name
	podToCreate.Labels["direwolf/app"] = lobby.Spec.AppReference.Name
	podToCreate.Labels["direwolf/user"] = lobby.Spec.ProfileReference.Name

	// Ensure slice order is deterministic by sorting map keys
	keys := make([]string, 0, len(wolfEnvVars))
	for k := range wolfEnvVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	wolfEnvVarsSlice := make([]corev1.EnvVar, 0, len(wolfEnvVars))
	for _, k := range keys {
		wolfEnvVarsSlice = append(wolfEnvVarsSlice, corev1.EnvVar{Name: k, Value: wolfEnvVars[k]})
	}

	// Inject volume mounts and env vars into existing containers
	for i := range podToCreate.Spec.Containers {
		podToCreate.Spec.Containers[i].VolumeMounts = append(podToCreate.Spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "wolf-runtime",
				MountPath: "/tmp/.X11-unix",
			},
			corev1.VolumeMount{
				Name:      "wolf-data",
				MountPath: "/home/retro",
				SubPath:   fmt.Sprintf("state/%s", app.Name),
			},
		)

		podToCreate.Spec.Containers[i].Env = append(podToCreate.Spec.Containers[i].Env, []corev1.EnvVar{
			// Standard GOW envars
			{Name: "DISPLAY", Value: ":0"},
			{Name: "WAYLAND_DISPLAY", Value: "wayland-1"}, //should be acquired dynamically later on
			{Name: "TZ", Value: wolfEnvVars["TZ"]},
			{Name: "UNAME", Value: "retro"},
			{Name: "XDG_RUNTIME_DIR", Value: "/tmp/.X11-unix"},
			{Name: "PULSE_SERVER", Value: "unix:/tmp/.X11-unix/pulse-socket"},
			// Assorted NVIDIA. Unsure if required. Probably not.
			{Name: "LIBVA_DRIVER_NAME", Value: "nvidia"},
			{Name: "LD_LIBRARY_PATH", Value: "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib"},
			{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "all"},
			{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
			{Name: "GST_VAAPI_ALL_DRIVERS", Value: "1"},
			{Name: "GST_DEBUG", Value: "2"},
			// Gamescope envar injection. Ham-handed. Why not.
			{Name: "GAMESCOPE_WIDTH", Value: fmt.Sprint(lobby.Spec.VideoSettings.Width)},
			{Name: "GAMESCOPE_HEIGHT", Value: fmt.Sprint(lobby.Spec.VideoSettings.Height)},
			{Name: "GAMESCOPE_REFRESH", Value: fmt.Sprint(lobby.Spec.VideoSettings.RefreshRate)},
		}...)

		// Validate the main app container's resources against the user's policy.
		validatedResources, err := validateAppResources(podToCreate.Spec.Containers[i].Resources, user.Spec.Resources)
		if err != nil {
			return fmt.Errorf("resource validation for main app container failed: %w", err)
		}
		podToCreate.Spec.Containers[i].Resources = validatedResources
	}

	// Define default resources for sidecars
	wolfAgentDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("100Mi")},
	}
	pulseAudioDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("100Mi")},
	}
	wolfDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("100Mi")},
	}

	wolfAgentResources := wolfAgentDefaultResources
	pulseAudioResources := pulseAudioDefaultResources
	wolfResources := wolfDefaultResources

	var wolfAgentEnv, pulseAudioEnv, wolfEnv []corev1.EnvVar
	var wolfAgentVolumeMounts, pulseAudioVolumeMounts, wolfVolumeMounts []corev1.VolumeMount
	var wolfAgentSecurityContext, pulseAudioSecurityContext, wolfSecurityContext *corev1.SecurityContext
	var podHostIPC bool

	// Create a set of valid volume names for quick lookup
	validVolumes := make(map[string]struct{})
	for _, volume := range user.Spec.Volumes {
		validVolumes[volume.Name] = struct{}{}
	}

	// Prepare sidecar policies
	if user.Spec.SidecarPolicies != nil {
		policies := user.Spec.SidecarPolicies
		if policies.WolfAgent != nil {
			if err := validateVolumeMounts(policies.WolfAgent.VolumeMounts, validVolumes, "wolfAgent"); err != nil {
				return err
			}
			wolfAgentEnv = policies.WolfAgent.Env
			wolfAgentResources = mergeResourceRequirements(wolfAgentDefaultResources, policies.WolfAgent.Resources)
			wolfAgentVolumeMounts = policies.WolfAgent.VolumeMounts
			wolfAgentSecurityContext = policies.WolfAgent.SecurityContext
			if policies.WolfAgent.HostIPC != nil && *policies.WolfAgent.HostIPC {
				podHostIPC = true
			}
		}
		if policies.PulseAudio != nil {
			if err := validateVolumeMounts(policies.PulseAudio.VolumeMounts, validVolumes, "pulseAudio"); err != nil {
				return err
			}
			pulseAudioEnv = policies.PulseAudio.Env
			pulseAudioResources = mergeResourceRequirements(pulseAudioDefaultResources, policies.PulseAudio.Resources)
			pulseAudioVolumeMounts = policies.PulseAudio.VolumeMounts
			pulseAudioSecurityContext = policies.PulseAudio.SecurityContext
			if policies.PulseAudio.HostIPC != nil && *policies.PulseAudio.HostIPC {
				podHostIPC = true
			}
		}
		if policies.Wolf != nil {
			if err := validateVolumeMounts(policies.Wolf.VolumeMounts, validVolumes, "wolf"); err != nil {
				return err
			}
			wolfEnv = policies.Wolf.Env
			wolfResources = mergeResourceRequirements(wolfDefaultResources, policies.Wolf.Resources)
			wolfVolumeMounts = policies.Wolf.VolumeMounts
			wolfSecurityContext = policies.Wolf.SecurityContext
			if policies.Wolf.HostIPC != nil && *policies.Wolf.HostIPC {
				podHostIPC = true
			}
		}
	}

	// Apply HostIPC setting to the pod spec if requested by any sidecar policy
	podToCreate.Spec.HostIPC = podHostIPC

	podToCreate.Spec.Containers = append(podToCreate.Spec.Containers,
		corev1.Container{
			Name:            "wolf-agent",
			Image:           c.WolfAgentImage,
			ImagePullPolicy: corev1.PullPolicy(c.WolfAgentImagePullPolicy),
			Args:            []string{"--socket=/etc/wolf/wolf.sock", "--port=8443"},
			Ports:           []corev1.ContainerPort{{Name: "wa", ContainerPort: 8443}},
			Env: append([]corev1.EnvVar{
				{Name: "XDG_RUNTIME_DIR", Value: "/tmp/.X11-unix"},
				{Name: "WOLF_SOCKET_PATH", Value: "/etc/wolf/wolf.sock"},
				{Name: "DIREWOLF_USER", Value: lobby.Spec.ProfileReference.Name},
				{Name: "DIREWOLF_APP", Value: lobby.Spec.AppReference.Name},
				{
					Name:      "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
				},
				{
					Name:      "POD_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
				},
			}, wolfAgentEnv...),
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(8443), Scheme: corev1.URISchemeHTTPS},
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Path: "/livez", Port: intstr.FromInt(8443), Scheme: corev1.URISchemeHTTPS},
				},
			},
			Resources:       wolfAgentResources,
			SecurityContext: wolfAgentSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: "wolf-cfg", MountPath: "/etc/wolf"},
				{Name: "wolf-runtime", MountPath: "/tmp/.X11-unix"},
			}, wolfAgentVolumeMounts...),
		},
		corev1.Container{
			Name:  "pulseaudio",
			Image: "ghcr.io/games-on-whales/pulseaudio:edge",
			Env: append([]corev1.EnvVar{
				{Name: "TZ", Value: wolfEnvVars["TZ"]},
				{Name: "UNAME", Value: "retro"},
				{Name: "XDG_RUNTIME_DIR", Value: "/tmp/pulse"},
			}, pulseAudioEnv...),
			Resources:       pulseAudioResources,
			SecurityContext: pulseAudioSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: "wolf-runtime", MountPath: "/tmp/pulse"},
			}, pulseAudioVolumeMounts...),
		},
		corev1.Container{
			Name:  "wolf",
			Image: WOLF_IMAGE,
			Env:   append(wolfEnvVarsSlice, wolfEnv...),
			Ports: []corev1.ContainerPort{
				{Name: "http", ContainerPort: 48989},
				{Name: "https", ContainerPort: 48984},
				{Name: "rtsp", ContainerPort: lobby.Status.Ports.RTSP},
				{Name: "enet", ContainerPort: lobby.Status.Ports.Control},
				{Name: "video", ContainerPort: lobby.Status.Ports.VideoRTP},
				{Name: "audio", ContainerPort: lobby.Status.Ports.AudioRTP},
			},
			Resources:       wolfResources,
			SecurityContext: wolfSecurityContext,
			VolumeMounts: append([]corev1.VolumeMount{
				{Name: "wolf-cfg", MountPath: "/etc/wolf"},
				{Name: "wolf-runtime", MountPath: "/tmp/.X11-unix"},
				{Name: "wolf-data", MountPath: "/mnt/data/wolf"},
			}, wolfVolumeMounts...),
		},
	)

	var wolfDataVolumeSource corev1.VolumeSource
	if app.Spec.VolumeClaimTemplate != nil {
		wolfDataVolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: c.deploymentName(lobby),
			},
		}
	} else {
		wolfDataVolumeSource = corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}
	}

	podToCreate.Spec.Volumes = append(podToCreate.Spec.Volumes,
		corev1.Volume{Name: "wolf-cfg", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "wolf-runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "wolf-data", VolumeSource: wolfDataVolumeSource},
	)

	// Add volumes from the user spec
	if len(user.Spec.Volumes) > 0 {
		podToCreate.Spec.Volumes = append(podToCreate.Spec.Volumes, user.Spec.Volumes...)
	}

	// Create deployment scaled to 1 for this pod
	// Should use deployment so that changes in spec aren't rejected.
	deployment := appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: lobby.Namespace,
			Labels: map[string]string{
				"app":            "direwolf-worker",
				"direwolf/lobby": lobby.Name,
				"direwolf/app":   lobby.Spec.AppReference.Name,
				"direwolf/user":  lobby.Spec.ProfileReference.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: v1alpha1.GroupVersion.String(),
					Kind:       "Lobby",
					Name:       lobby.Name,
					UID:        lobby.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"direwolf/lobby": lobby.Name,
				},
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			RevisionHistoryLimit:    ptr.To[int32](1),
			ProgressDeadlineSeconds: ptr.To[int32](10),
			Template:                podToCreate,
		},
	}

	unstructuredDeployment, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
	if err != nil {
		return fmt.Errorf("failed to convert deployment to unstructured: %w", err)
	}

	var deploymentApplyConfig appsv1ac.DeploymentApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredDeployment, &deploymentApplyConfig)
	if err != nil {
		return fmt.Errorf("failed to convert unstructured to deployment: %w", err)
	}

	_, err = c.K8sClient.AppsV1().Deployments(lobby.Namespace).Apply(
		ctx,
		&deploymentApplyConfig,
		metav1.ApplyOptions{
			FieldManager: "direwolf-lobby-controller-deployment",
		})

	return err
}

// reconcileWolfLobby calls out to the wolf-agent API to create the lobby
// once the Deployment and Service are ready.
func (c *LobbyController) reconcileWolfLobby(ctx context.Context, lobby *v1alpha1types.Lobby) error {
	// If the lobby is already created, skip
	if lobby.Status.LobbyID != "" {
		return nil
	}

	deploymentName := c.deploymentName(lobby)
	deployment, err := c.K8sClient.AppsV1().Deployments(lobby.Namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	// Strictly wait for Pod to be Ready to avoid EOF/Connection Refused on the wolf API
	if deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.ReadyReplicas != *deployment.Spec.Replicas ||
		deployment.Status.ReadyReplicas == 0 {
		return fmt.Errorf("waiting for deployment %s to be ready", deploymentName)
	}

	service, err := c.K8sClient.CoreV1().Services(lobby.Namespace).Get(ctx, lobby.Status.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	wolfclient := wolfapi.NewClient(fmt.Sprintf("https://%s:8443", service.Spec.ClusterIP), &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	})

	runCmd := "sleep infinity"
	if lobby.Spec.RunnerOverride != "" {
		runCmd = lobby.Spec.RunnerOverride
	}

	req := wolfapi.LobbyCreateRequest{
		ProfileID:              lobby.Spec.ProfileReference.Name,
		Name:                   lobby.Spec.AppReference.Name,
		MultiUser:              lobby.Spec.MultiUser,
		StopWhenEveryoneLeaves: lobby.Spec.StopWhenEveryoneLeaves,
		RunnerStateFolder:      fmt.Sprintf("profile-data/%s/Wolf%s", lobby.Spec.ProfileReference.Name, lobby.Spec.AppReference.Name),
		Runner: wolfapi.Runner{
			Type:   "process",
			RunCmd: runCmd,
		},
		ClientSettings: wolfapi.LobbyClientSettings{
			ControllersOverride: []string{},
			HScrollAcceleration: 1.0,
			MouseAcceleration:   1.0,
			RunGID:              1000,
			RunUID:              1000,
			VScrollAcceleration: 1.0,
		},
		VideoSettings: wolfapi.LobbyVideoSettings{
			Width:                   lobby.Spec.VideoSettings.Width,
			Height:                  lobby.Spec.VideoSettings.Height,
			RefreshRate:             lobby.Spec.VideoSettings.RefreshRate,
			WaylandRenderNode:       "/dev/dri/renderD128", // Needs to be parameterized later
			RunnerRenderNode:        "/dev/dri/renderD128", // Needs to be parameterized later
			VideoProducerBufferCaps: "video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}",
		},
		AudioSettings: wolfapi.LobbyAudioSettings{
			ChannelCount: lobby.Spec.AudioSettings.ChannelCount,
		},
	}

	resp, err := wolfclient.CreateLobby(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create lobby in wolf backend: %w", err)
	}

	lobby.Status.LobbyID = resp.LobbyID
	return nil
}

// mergeResourceRequirements merges a default and an override ResourceRequirements object for sidecars.
// It gives precedence to the values specified in the overrides.
func mergeResourceRequirements(defaults corev1.ResourceRequirements, overrides *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if overrides == nil {
		return defaults
	}
	merged := defaults.DeepCopy()
	if merged.Limits == nil {
		merged.Limits = make(corev1.ResourceList)
	}
	if merged.Requests == nil {
		merged.Requests = make(corev1.ResourceList)
	}
	for resourceName, quantity := range overrides.Limits {
		merged.Limits[resourceName] = quantity
	}
	for resourceName, quantity := range overrides.Requests {
		merged.Requests[resourceName] = quantity
	}
	return *merged
}

// validateAppResources checks if the app's resource requirements are within the user's policy.
// It returns an error if any app request/limit exceeds the user policy.
// If the policy is nil, it allows any resources.
func validateAppResources(appResources corev1.ResourceRequirements, userPolicy *corev1.ResourceRequirements) (corev1.ResourceRequirements, error) {
	if userPolicy == nil {
		return appResources, nil
	}
	for resourceName, appLimit := range appResources.Limits {
		if userLimit, ok := userPolicy.Limits[resourceName]; ok {
			if appLimit.Cmp(userLimit) > 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf("app limit exceeds user policy")
			}
		}
	}
	for resourceName, appRequest := range appResources.Requests {
		if userRequest, ok := userPolicy.Requests[resourceName]; ok {
			if appRequest.Cmp(userRequest) > 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf("app request exceeds user policy")
			}
		}
	}
	return appResources, nil
}

// validateVolumeMounts checks if all volume mounts in the provided slice
// correspond to a volume defined in the validVolumes map.
func validateVolumeMounts(mounts []corev1.VolumeMount, validVolumes map[string]struct{}, sidecarName string) error {
	for _, vm := range mounts {
		if _, ok := validVolumes[vm.Name]; !ok {
			return fmt.Errorf("volumeMount %q in %s sidecar policy refers to an undefined volume", vm.Name, sidecarName)
		}
	}
	return nil
}
