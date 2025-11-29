package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"time"

	"games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
	"github.com/pelletier/go-toml/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	v1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	gatewayv1alpha2 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1alpha2"
)

var (
	WOLF_IMAGE = func() string {
		if im := os.Getenv("WOLF_IMAGE"); im != "" {
			return im
		}

		return "ghcr.io/games-on-whales/wolf:stable"
	}()
)

type userGame struct {
	User string
	Game string
}

type SessionControllerOptions struct {
	WolfAgentImage string
	LBSharingKey   string
}

// Session Controller manages the lifecycle of a streaming session for
// a given game, of a given user.
// If is responsible for:
//   - 1. Setting up port forwards via Gateway API
//   - 2. Setting up service, pods, etc. for session
//   - 3. Polling the pods wolf-agent to find when session is complete, cleaning up
//   - 4. Calling fake-udev to set up the controllers for the game (wolf-agent instead, probably)
//   - 5. Cleaning up all resources when session is complete
//
// Watchers lists of users and games to:
//   - 1. Delete sessions for games that were deleted
type SessionController struct {
	SessionClient   v1alpha1client.SessionInterface
	SessionInformer generic.Informer[*v1alpha1types.Session]

	AppInformer  generic.Informer[*v1alpha1types.App]
	UserInformer generic.Informer[*v1alpha1types.User]

	TCPRouteClient gatewayv1alpha2.TCPRouteInterface
	UDPRouteClient gatewayv1alpha2.UDPRouteInterface

	K8sClient kubernetes.Interface

	trackedSessions map[userGame]sets.Set[string]
	trackedGames    map[string]userGame

	controller           generic.Controller[*v1alpha1types.Session]
	deploymentController generic.Controller[*appsv1.Deployment]
	SessionControllerOptions
}

// NewSessionController creates a new session controller.
func NewSessionController(
	k8sClient kubernetes.Interface,
	tcpRouteClient gatewayv1alpha2.TCPRouteInterface,
	udpRouteClient gatewayv1alpha2.UDPRouteInterface,
	sessionClient v1alpha1client.SessionInterface,
	sessionInformer generic.Informer[*v1alpha1types.Session],
	appInformer generic.Informer[*v1alpha1types.App],
	userInformer generic.Informer[*v1alpha1types.User],
	deploymentInformer generic.Informer[*appsv1.Deployment],
	options SessionControllerOptions,
) *SessionController {
	res := &SessionController{
		K8sClient:                k8sClient,
		TCPRouteClient:           tcpRouteClient,
		UDPRouteClient:           udpRouteClient,
		SessionClient:            sessionClient,
		SessionInformer:          sessionInformer,
		AppInformer:              appInformer,
		UserInformer:             userInformer,
		trackedSessions:          make(map[userGame]sets.Set[string]),
		trackedGames:             make(map[string]userGame),
		SessionControllerOptions: options,
	}

	res.controller = generic.NewController(
		sessionInformer,
		res.Reconcile,
		generic.ControllerOptions{
			Name:    "session-controller",
			Workers: 2,
		},
	)

	//!TODO: Also watch any udproutes, services, deployments, etc. that we create
	// and re-reconcile their sessions when they change.
	res.deploymentController = generic.NewController(
		deploymentInformer,
		func(namepace, name string, newObj *appsv1.Deployment) error {
			// Load bearing. If we pass nil it will be casted to interface and
			// not be comparable with nil :)
			if newObj == nil {
				return nil
			}
			return res.reconcileDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "session-controller-deployment",
			Workers: 1,
		},
	)

	return res
}

func (c *SessionController) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(sessionCtx.Done(), c.SessionInformer.HasSynced) {
		return fmt.Errorf("failed to sync session informer")
	}

	// Build initial listing of sessions
	sessions, err := c.SessionInformer.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %v", err)
	}

	for _, session := range sessions {
		ug := userGame{
			Game: session.Spec.GameReference.Name,
			User: session.Spec.UserReference.Name,
		}
		if existing, ok := c.trackedSessions[ug]; ok {
			existing.Insert(session.Name)
		} else {
			c.trackedSessions[ug] = sets.New(session.Name)
		}

		c.trackedGames[session.Name] = ug
	}

	go func() {
		defer cancel()
		err := c.deploymentController.Run(sessionCtx)
		if err != nil {
			klog.Errorf("Failed to run deployment controller: %v", err)
		}
	}()

	return c.controller.Run(sessionCtx)
}

func (c *SessionController) HasSynced() bool {
	return c.SessionInformer.HasSynced()
}

type K8sObject interface {
	metav1.Object
	runtime.Object
}

func (c *SessionController) reconcileDependant(obj K8sObject) error {
	// If object doesnt have direwolf/user and direwolf/app labels, skip
	if obj.GetLabels() == nil {
		return nil
	}

	if _, ok := obj.GetLabels()["direwolf/user"]; !ok {
		return nil
	}

	if _, ok := obj.GetLabels()["direwolf/app"]; !ok {
		return nil
	}

	klog.Infof("Reconciling dependant %s %s/%s", obj.GetObjectKind().GroupVersionKind().String(), obj.GetNamespace(), obj.GetName())

	// Lookup sessions associated with his object
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == "Session" {
			klog.Infof("Found owner %s/%s", owner.Name, owner.UID)
			c.controller.Enqueue(obj.GetNamespace(), owner.Name)
		}
	}

	return nil
}

func (c *SessionController) Reconcile(namespace, name string, newObj *v1alpha1types.Session) error {
	klog.Infof("Reconciling session %s/%s", namespace, name)
	defer klog.Infof("Finished Reconciling session %s/%s", namespace, name)

	if newObj == nil {
		// Session was deleted. Stuff will be garbage collected by Kubernetes
		// due to owner references. Nothing to do.
		if gam, ok := c.trackedGames[name]; ok {
			if existing, ok := c.trackedSessions[gam]; ok {
				existing.Delete(name)
				if existing.Len() == 0 {
					delete(c.trackedSessions, gam)
				}
			}

			delete(c.trackedGames, name)
		}
		return nil
	} else if newObj.Status.WolfSessionID == "" && newObj.CreationTimestamp.Add(1*time.Minute).Before(time.Now()) {
		klog.Infof("Session %s/%s is older than 1 minute and has no wolf session ID, deleting", newObj.Namespace, newObj.Name)
		err := c.SessionClient.Delete(context.TODO(), newObj.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			klog.Errorf("Failed to delete session %s/%s: %v", newObj.Namespace, newObj.Name, err)
			return err
		}
		return nil
	}
	ug := userGame{
		Game: newObj.Spec.GameReference.Name,
		User: newObj.Spec.UserReference.Name,
	}

	if existing, ok := c.trackedSessions[ug]; ok {
		existing.Insert(newObj.Name)
	} else {
		c.trackedSessions[ug] = sets.New(newObj.Name)
	}

	oldStatus := newObj.Status.DeepCopy()
	portsError := c.allocatePorts(context.TODO(), newObj)

	if portsError != nil {
		klog.Errorf("Failed to allocate ports: %s", portsError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "PortsAllocated",
			Status:  metav1.ConditionFalse,
			Reason:  "PortsAllocationFailed",
			Message: portsError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "PortsAllocated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	configError := c.reconcileConfigMap(context.TODO(), newObj)
	if configError != nil {
		klog.Errorf("Failed to reconcile configmap: %s", configError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "ConfigMapCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "ConfigMapCreationFailed",
			Message: configError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "ConfigMapCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	if pvcError := c.reconcilePVC(context.TODO(), newObj); pvcError != nil {
		klog.Errorf("Failed to reconcile pvc: %s", pvcError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "VolumeCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "PVCAllocationFailed",
			Message: pvcError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "VolumeCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	if podError := c.reconcilePod(context.TODO(), newObj); podError != nil {
		klog.Errorf("Failed to reconcile pod: %s", podError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "DeploymentCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "PodCreationFailed",
			Message: podError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "DeploymentCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	if serviceError := c.reconcileService(context.TODO(), newObj); serviceError != nil {
		klog.Errorf("Failed to reconcile service: %s", serviceError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "ServiceCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "ServiceCreationFailed",
			Message: serviceError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "ServiceCreated",
			Status: metav1.ConditionTrue,
			Reason: "ServiceCreated",
		})
	}

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

	if streamError := c.reconcileActiveStreams(context.TODO(), newObj); streamError != nil {
		klog.Errorf("Failed to reconcile active streams: %s", streamError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "StreamStarted",
			Status:  metav1.ConditionFalse,
			Reason:  "StreamStartFailed",
			Message: streamError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "StreamStarted",
			Status: metav1.ConditionTrue,
			Reason: "WaitForPing", //!TOOD: use actual current stream status?
		})
	}

	// Set the new status, if it is changed
	if !reflect.DeepEqual(newObj.Status, oldStatus) {
		_, err := c.SessionClient.UpdateStatus(
			context.TODO(),
			newObj,
			metav1.UpdateOptions{
				FieldManager: "session-controller-status",
			},
		)

		// Failed to update status....nothing to do but try again with
		// exponential backoff. Could be API server issue. Depends on response
		// code?
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	//!TODO: figure our retry logic. Some of these errors surely are retriable
	return nil
}

// // !TODO: Unused. Part of testing gateway implementation. The final idea is for
// // Direwolf to dynamically set up port forwards / UDPRoutes via Kubernetes
// // Gateway API for RTSP, ENet, Video RTP, Audio RTP.
// func (c *SessionController) reconcileGateway(ctx context.Context, session *v1alpha1types.Session) error {
// 	// 1. Decide the ports this session will use for RTSP, Enet, Video RTP, Audio RTP
// 	// 2. Create TCPRoute for RTSP, UDP routes for Enet, RTP via Gateway API
// 	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
// 		return fmt.Errorf("waiting for PortsAllocated")
// 	}

// 	_, err := c.UDPRouteClient.Apply(
// 		ctx,
// 		gatewayv1alpha2ac.UDPRoute(session.Name, session.Namespace).
// 			WithOwnerReferences(metav1ac.OwnerReference().
// 				WithName(session.Name).
// 				WithAPIVersion(v1alpha1.GroupVersion.String()).
// 				WithKind("Session").
// 				WithUID(session.UID).
// 				WithController(true)).
// 			WithLabels(
// 				map[string]string{
// 					"app":           "direwolf-worker",
// 					"direwolf/app":  session.Spec.GameReference.Name,
// 					"direwolf/user": session.Spec.UserReference.Name,
// 				}).
// 			WithSpec(
// 				gatewayv1alpha2ac.UDPRouteSpec().
// 					WithParentRefs(gatewayv1ac.ParentReference().
// 						WithKind("Gateway").
// 						WithGroup("gateway.networking.k8s.io").
// 						WithName(gatewayv1.ObjectName(session.Spec.GatewayReference.Name)).
// 						WithNamespace(gatewayv1.Namespace(session.Spec.GatewayReference.Namespace))).
// 					WithRules(
// 						gatewayv1alpha2ac.UDPRouteRule().
// 							WithName(gatewayv1.SectionName(session.Name)).
// 							WithBackendRefs(
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.Control)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.VideoRTP)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.AudioRTP)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 							),
// 					),
// 			),
// 		metav1.ApplyOptions{
// 			FieldManager: "direwolf-session-controller-udp-route",
// 			Force:        true,
// 		},
// 	)
// 	if err != nil {
// 		return fmt.Errorf("failed to apply udp route: %s", err)
// 	}

// 	_, err = c.TCPRouteClient.Apply(
// 		ctx,
// 		gatewayv1alpha2ac.TCPRoute(session.Name, session.Namespace).
// 			WithOwnerReferences(metav1ac.OwnerReference().
// 				WithName(session.Name).
// 				WithAPIVersion(v1alpha1.GroupVersion.String()).
// 				WithKind("Session").
// 				WithUID(session.UID).
// 				WithController(true)).
// 			WithLabels(
// 				map[string]string{
// 					"app":           "direwolf-worker",
// 					"direwolf/app":  session.Spec.GameReference.Name,
// 					"direwolf/user": session.Spec.UserReference.Name,
// 				}).
// 			WithSpec(
// 				gatewayv1alpha2ac.TCPRouteSpec().
// 					WithParentRefs(gatewayv1ac.ParentReference().
// 						WithKind("Gateway").
// 						WithGroup("gateway.networking.k8s.io").
// 						WithName(gatewayv1.ObjectName(session.Spec.GatewayReference.Name)).
// 						WithNamespace(gatewayv1.Namespace(session.Spec.GatewayReference.Namespace))).
// 					WithRules(
// 						gatewayv1alpha2ac.TCPRouteRule().
// 							WithName(gatewayv1.SectionName(session.Name)).
// 							WithBackendRefs(
// 								gatewayv1ac.BackendRef().
// 									WithPort(gatewayv1.PortNumber(session.Status.Ports.RTSP)).
// 									WithKind(gatewayv1.Kind("Service")).
// 									WithName(gatewayv1.ObjectName(session.Namespace)).
// 									WithNamespace(gatewayv1.Namespace(session.Namespace)),
// 							),
// 					),
// 			),
// 		metav1.ApplyOptions{
// 			FieldManager: "direwolf-session-controller-TCP-route",
// 			Force:        true,
// 		},
// 	)
// 	if err != nil {
// 		return fmt.Errorf("failed to apply TCP route: %s", err)
// 	}

// 	return nil
// }

func (c *SessionController) reconcileService(ctx context.Context, session *v1alpha1types.Session) error {
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return fmt.Errorf("waiting for PortsAllocated")
	}

	clampString := func(s string, max int) string {
		if len(s) > max {
			return s[:max]
		}
		return s
	}

	session.Status.ServiceName = fmt.Sprintf("%s-rtp", clampString(session.Name, 56))

	// HACK: Delete all direwolf-worker services that dont match the service name
	// This is until we can control the ports in wolf
	allServices, err := c.K8sClient.CoreV1().Services(session.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=direwolf-worker",
	})

	if err != nil {
		return fmt.Errorf("failed to list services: %s", err)
	}

	for _, svc := range allServices.Items {
		if svc.Name != session.Status.ServiceName {
			klog.Infof("Deleting service %s/%s", svc.Namespace, svc.Name)
			err := c.K8sClient.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Failed to delete service %s/%s: %s", svc.Namespace, svc.Name, err)
				return fmt.Errorf("failed to delete service %s/%s: %s", svc.Namespace, svc.Name, err)
			}
		}
	}

	// 1. Use the set up a service with correct ports pointing to the pods
	_, err = c.K8sClient.CoreV1().
		Services(session.Namespace).
		Apply(
			context.Background(),
			v1ac.Service(session.Status.ServiceName, session.Namespace).
				WithAnnotations(map[string]string{
					// Try to support popular service LoadBalancer implementation
					// sharing key annotations.
					"lbipam.cilium.io/sharing-key":        c.LBSharingKey,
					"metallb.universe.tf/allow-shared-ip": c.LBSharingKey,
				}).
				WithLabels(
					map[string]string{
						"app":           "direwolf-worker",
						"direwolf/app":  session.Spec.GameReference.Name,
						"direwolf/user": session.Spec.UserReference.Name,
					},
				).
				WithOwnerReferences(metav1ac.OwnerReference().
					WithName(session.Name).
					WithAPIVersion(v1alpha1.GroupVersion.String()).
					WithKind("Session").
					WithUID(session.UID).
					WithController(true)).
				WithSpec(
					v1ac.ServiceSpec().
						WithType(corev1.ServiceTypeLoadBalancer).
						WithSelector(
							map[string]string{
								"direwolf/app":  session.Spec.GameReference.Name,
								"direwolf/user": session.Spec.UserReference.Name,
							}).
						WithPorts(
							v1ac.ServicePort().
								WithName("wa"). // wolf-agent
								WithPort(8443),
							v1ac.ServicePort().
								WithName("rtsp"). // moonlight-rtsp
								WithPort(session.Status.Ports.RTSP),
							v1ac.ServicePort().
								WithName("enet"). // moonlight-enet
								WithProtocol(corev1.ProtocolUDP).
								WithPort(session.Status.Ports.Control),
							v1ac.ServicePort().
								WithName("video"). // moonlight-video
								WithProtocol(corev1.ProtocolUDP).
								WithPort(session.Status.Ports.VideoRTP),
							v1ac.ServicePort().
								WithName("audio"). // moonlight-audio
								WithProtocol(corev1.ProtocolUDP).
								WithPort(session.Status.Ports.AudioRTP),
						),
				),
			metav1.ApplyOptions{
				FieldManager: "direwolf-session-controller-svc",
			})
	if err != nil {
		return fmt.Errorf("failed to apply service: %s", err)
	}
	return nil
}

// mergeResourceRequirements merges a default and an override ResourceRequirements object for sidecars.
// It gives precedence to the values specified in the overrides.
func mergeResourceRequirements(defaults corev1.ResourceRequirements, overrides *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if overrides == nil {
		return defaults
	}

	// Start with a copy of the defaults
	merged := defaults.DeepCopy()

	// Ensure maps are initialized
	if merged.Limits == nil {
		merged.Limits = make(corev1.ResourceList)
	}
	if merged.Requests == nil {
		merged.Requests = make(corev1.ResourceList)
	}

	// Override limits
	for resourceName, quantity := range overrides.Limits {
		merged.Limits[resourceName] = quantity
	}

	// Override requests
	for resourceName, quantity := range overrides.Requests {
		merged.Requests[resourceName] = quantity
	}

	return *merged
}

// validateAppResources checks if the app's resource requirements are within the user's policy.
// It returns an error if any app request/limit exceeds the user policy.
// If the policy is nil, it allows any resources.
func validateAppResources(appResources corev1.ResourceRequirements, userPolicy *corev1.ResourceRequirements) (corev1.ResourceRequirements, error) {
	// If there's no policy, the app's resources are inherently valid.
	if userPolicy == nil {
		return appResources, nil
	}

	// Validate Limits
	for resourceName, appLimit := range appResources.Limits {
		if userLimit, ok := userPolicy.Limits[resourceName]; ok {
			// Cmp returns 1 if appLimit > userLimit
			if appLimit.Cmp(userLimit) > 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf(
					"app limit for resource %q (%s) exceeds user policy limit (%s)",
					resourceName, appLimit.String(), userLimit.String(),
				)
			}
		}
	}

	// Validate Requests, I'm not sure if this is needed because we could just limit using... limits.
	for resourceName, appRequest := range appResources.Requests {
		if userRequest, ok := userPolicy.Requests[resourceName]; ok {
			// Cmp returns 1 if appRequest > userRequest
			if appRequest.Cmp(userRequest) > 0 {
				return corev1.ResourceRequirements{}, fmt.Errorf(
					"app request for resource %q (%s) exceeds user policy request (%s)",
					resourceName, appRequest.String(), userRequest.String(),
				)
			}
		}
	}

	// All checks passed. The app's requested resources are valid.
	return appResources, nil
}

// validateVolumeMounts checks if all volume mounts in the provided slice
// correspond to a volume defined in the validVolumes map.
func validateVolumeMounts(mounts []corev1.VolumeMount, validVolumes map[string]struct{}, sidecarName string) error {
	for _, vm := range mounts {
		if _, ok := validVolumes[vm.Name]; !ok {
			return fmt.Errorf("validation failed: volumeMount %q in %s sidecar policy refers to a volume that is not defined in the UserSpec.volumes", vm.Name, sidecarName)
		}
	}
	return nil
}

func (c *SessionController) reconcilePod(ctx context.Context, session *v1alpha1types.Session) error {
	//!TODO: Just allocate a ton of ports on the container, we wont be able to
	// change them while its running if another user connects
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return fmt.Errorf("waiting for PortsAllocated")
	}

	// Get the user object to access resource policies
	user, err := c.UserInformer.Namespaced(session.Namespace).Get(session.Spec.UserReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user %s: %w", session.Spec.UserReference.Name, err)
	}

	ug := userGame{
		Game: session.Spec.GameReference.Name,
		User: session.Spec.UserReference.Name,
	}

	var owners []metav1.OwnerReference
	var ownerApply []*metav1ac.OwnerReferenceApplyConfiguration
	if sessions, ok := c.trackedSessions[ug]; ok {
		for name := range sessions {
			sess, err := c.SessionInformer.Namespaced(session.Namespace).Get(name)
			if err != nil {
				klog.Errorf("Failed to get session %s/%s: %s", session.Namespace, name, err)
				continue
			}
			owner := metav1.OwnerReference{
				APIVersion: v1alpha1.GroupVersion.String(),
				Kind:       "Session",
				Name:       name,
				UID:        sess.UID,
				Controller: ptr.To(true),
			}
			owners = append(owners, owner)
			ownerApply = append(ownerApply, metav1ac.OwnerReference().
				WithName(name).
				WithAPIVersion(v1alpha1.GroupVersion.String()).
				WithKind("Session").
				WithUID(session.UID).
				WithController(true))
		}
	}

	// If deployment already exists, just skip
	deploymentName := c.deploymentName(session)
	if _, err := c.deploymentController.Informer().Namespaced(session.Namespace).Get(deploymentName); err == nil {
		klog.Infof("Deployment %s/%s already exists, just updating metadata", session.Namespace, deploymentName)
		c.K8sClient.AppsV1().Deployments(session.Namespace).Apply(
			context.Background(),
			appsv1ac.Deployment(deploymentName, session.Namespace).
				WithOwnerReferences(ownerApply...),
			metav1.ApplyOptions{
				FieldManager: "direwolf-session-controller-deployment-owners",
			})

		return nil
	}

	// Create pod from pod template
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %s", err)
	}

	var podToCreate corev1.PodTemplateSpec
	if app.Spec.Template != nil {
		podToCreate.ObjectMeta = app.Spec.Template.ObjectMeta
		podToCreate.Spec = *app.Spec.Template.Spec.DeepCopy()
	}

	if podToCreate.Labels == nil {
		podToCreate.Labels = map[string]string{}
	}

	podToCreate.Labels["app"] = "direwolf-worker"
	podToCreate.Labels["direwolf/app"] = session.Spec.GameReference.Name
	podToCreate.Labels["direwolf/user"] = session.Spec.UserReference.Name

	// if podToCreate.Spec.SecurityContext == nil {
	// 	podToCreate.Spec.SecurityContext = &corev1.PodSecurityContext{}
	// }

	// if podToCreate.Spec.SecurityContext.SeccompProfile == nil {
	// 	podToCreate.Spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{
	// 		Type: corev1.SeccompProfileTypeUnconfined,
	// 	}
	// }

	// if podToCreate.Spec.SecurityContext.AppArmorProfile == nil {
	// 	podToCreate.Spec.SecurityContext.AppArmorProfile = &corev1.AppArmorProfile{
	// 		Type: corev1.AppArmorProfileTypeUnconfined,
	// 	}
	// }

	mapToEnvApplyList := func(m map[string]string) []corev1.EnvVar {
		var res []corev1.EnvVar
		for k, v := range m {
			res = append(res, corev1.EnvVar{
				Name:  k,
				Value: v,
			})
		}
		return res
	}

	// Inject volume mounts into existing containers
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

		podToCreate.Spec.Containers[i].Env = append(podToCreate.Spec.Containers[i].Env, mapToEnvApplyList(map[string]string{
			// Standard GOW envars
			"DISPLAY": ":0",
			// Container must have extra logic to wait for this to be set up
			// unfortunately.
			"WAYLAND_DISPLAY": "wayland-1",
			"TZ":              "America/Chicago",
			"UNAME":           "retro",
			"XDG_RUNTIME_DIR": "/tmp/.X11-unix",
			"UID":             "1000",
			"GID":             "1000",
			"PULSE_SERVER":    "unix:/tmp/.X11-unix/pulse-socket",
			// PULSE_SINK & PULSE_SOURCE set at runtime calculated based off session ID.
			// But would be nice if unnecessary

			// Assorted NVIDIA. Unsure if required. Probabky not.
			// just gonna uncomment to make sure that this is not the reason firefox keeps crashing and failing to play videos.
			// yeah now no audio, probably because i'm developing on an integrated amd gpu.
			// "LIBVA_DRIVER_NAME":          "nvidia",
			// "LD_LIBRARY_PATH":            "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib",
			// "NVIDIA_DRIVER_CAPABILITIES": "all",
			// "NVIDIA_VISIBLE_DEVICES":     "all",
			// "GST_VAAPI_ALL_DRIVERS":      "1",
			// "GST_DEBUG":                  "2",

			// Gamescape envar injection. Ham-handed. Why not.
			"GAMESCOPE_WIDTH":   fmt.Sprint(session.Spec.Config.VideoWidth),
			"GAMESCOPE_HEIGHT":  fmt.Sprint(session.Spec.Config.VideoHeight),
			"GAMESCOPE_REFRESH": fmt.Sprint(session.Spec.Config.VideoRefreshRate),
		})...)

		// Validate the main app container's resources against the user's policy.
		validatedResources, err := validateAppResources(podToCreate.Spec.Containers[i].Resources, user.Spec.Resources)
		if err != nil {
			// The error will be handled by the main Reconcile loop to update the session status.
			return fmt.Errorf("resource validation for main app container failed: %w", err)
		}
		podToCreate.Spec.Containers[i].Resources = validatedResources
	}

	podToCreate.Spec.InitContainers = append(podToCreate.Spec.InitContainers,
		corev1.Container{
			Name:  "init",
			Image: "ghcr.io/games-on-whales/base:edge",
			Command: []string{
				"sh", "-c", `
				chown 1000:1000 /mnt/data/wolf
				chmod 777 /mnt/data/wolf
				chown -R ubuntu:ubuntu /tmp/.X11-unix
				chmod 1777 -R /tmp/.X11-unix
				mkdir -p /etc/wolf/cfg
				cp -LR /cfg/* /etc/wolf/cfg
				chown -R ubuntu:ubuntu /etc/wolf
				chmod 777 -R /etc/wolf
			`,
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-data",
					MountPath: "/mnt/data/wolf",
				},
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/.X11-unix",
				},
				{
					Name:      "config",
					MountPath: "/cfg",
				},
			},
		},
	)

	// Define default resources for sidecars
	wolfAgentDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}
	pulseAudioDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}
	wolfDefaultResources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}

	// Prepare sidecar policies
	var wolfAgentResources, pulseAudioResources, wolfResources corev1.ResourceRequirements
	var wolfAgentVolumeMounts, pulseAudioVolumeMounts, wolfVolumeMounts []corev1.VolumeMount

	// Set defaults first
	wolfAgentResources = wolfAgentDefaultResources
	pulseAudioResources = pulseAudioDefaultResources
	wolfResources = wolfDefaultResources

	// Create a set of valid volume names for quick lookup
	validVolumes := make(map[string]struct{})
	for _, volume := range user.Spec.Volumes {
		validVolumes[volume.Name] = struct{}{}
	}

	if user.Spec.SidecarPolicies != nil {
		policies := user.Spec.SidecarPolicies
		if policies.WolfAgent != nil {
			if err := validateVolumeMounts(policies.WolfAgent.VolumeMounts, validVolumes, "wolfAgent"); err != nil {
				return err
			}
			wolfAgentResources = mergeResourceRequirements(wolfAgentDefaultResources, policies.WolfAgent.Resources)
			wolfAgentVolumeMounts = policies.WolfAgent.VolumeMounts
		}
		if policies.PulseAudio != nil {
			if err := validateVolumeMounts(policies.PulseAudio.VolumeMounts, validVolumes, "pulseAudio"); err != nil {
				return err
			}
			pulseAudioResources = mergeResourceRequirements(pulseAudioDefaultResources, policies.PulseAudio.Resources)
			pulseAudioVolumeMounts = policies.PulseAudio.VolumeMounts
		}
		if policies.Wolf != nil {
			if err := validateVolumeMounts(policies.Wolf.VolumeMounts, validVolumes, "wolf"); err != nil {
				return err
			}
			wolfResources = mergeResourceRequirements(wolfDefaultResources, policies.Wolf.Resources)
			wolfVolumeMounts = policies.Wolf.VolumeMounts
		}
	}

	podToCreate.Spec.Containers = append(podToCreate.Spec.Containers,
		corev1.Container{
			Name:            "wolf-agent",
			Image:           c.WolfAgentImage,
			ImagePullPolicy: corev1.PullAlways,
			Args: []string{
				"--socket=/etc/wolf/wolf.sock",
				"--port=8443",
			},
			Ports: []corev1.ContainerPort{
				{
					Name:          "wa",
					ContainerPort: 8443,
				},
			},
			Env: []corev1.EnvVar{
				{
					Name:  "XDG_RUNTIME_DIR",
					Value: "/tmp/.X11-unix",
				},
				{
					Name:  "PUID",
					Value: "1000",
				},
				{
					Name:  "PGID",
					Value: "1000",
				},
				{
					Name:  "WOLF_SOCKET_PATH",
					Value: "/etc/wolf/wolf.sock",
				},
				{
					Name:  "DIREWOLF_USER",
					Value: session.Spec.UserReference.Name,
				},
				{
					Name:  "DIREWOLF_APP",
					Value: session.Spec.GameReference.Name,
				},
				{
					Name: "POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				},
				{
					Name: "POD_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.namespace",
						},
					},
				},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/readyz",
						Port:   intstr.FromInt(8443),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path:   "/livez",
						Port:   intstr.FromInt(8443),
						Scheme: corev1.URISchemeHTTPS,
					},
				},
			},
			Resources: wolfAgentResources,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/.X11-unix",
				},
			}, wolfAgentVolumeMounts...),
		},
		corev1.Container{
			Name:  "pulseaudio",
			Image: "ghcr.io/games-on-whales/pulseaudio:edge",
			Env: mapToEnvApplyList(map[string]string{
				"TZ":              "America/Chicago",
				"UNAME":           "retro",
				"XDG_RUNTIME_DIR": "/tmp/pulse",
				"UID":             "1000",
				"GID":             "1000",
			}),
			Resources: pulseAudioResources,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/pulse",
				},
			}, pulseAudioVolumeMounts...),
		},
		corev1.Container{
			Name:  "wolf",
			Image: WOLF_IMAGE,
			Env: mapToEnvApplyList(map[string]string{
				"PUID":                       "1000",
				"PGID":                       "1000",
				"TZ":                         "America/Chicago",
				"UNAME":                      "ubuntu",
				"XDG_RUNTIME_DIR":            "/tmp/.X11-unix",
				"PULSE_SERVER":               "unix:/tmp/.X11-unix/pulse-socket",
				"HOST_APPS_STATE_FOLDER":     "/mnt/data/wolf",
				"WOLF_LOG_LEVEL":             "DEBUG",
				"WOLF_STREAM_CLIENT_IP":      "10.128.1.0", //Need to find the correct streaming id / ingress, later.
				"WOLF_SOCKET_PATH":           "/etc/wolf/wolf.sock",
				"WOLF_CFG_FILE":              "/etc/wolf/cfg/config.toml",
				"WOLF_PULSE_IMAGE":           "ghcr.io/games-on-whales/pulseaudio:master",
				"WOLF_CFG_FOLDER":            "/etc/wolf/cfg",
				"WOLF_RENDER_NODE":           "SOFTWARE",
				"GST_VAAPI_ALL_DRIVERS":      "1",
				"GST_DEBUG":                  "2",
				"__GL_SYNC_TO_VBLANK":        "0",
				"NVIDIA_VISIBLE_DEVICES":     "all",
				"NVIDIA_DRIVER_CAPABILITIES": "all",
				"LIBVA_DRIVER_NAME":          "nvidia",
				"LD_LIBRARY_PATH":            "/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/lib",
			}),
			// Note: Container Ports list is strictly informational. As long
			// as process is listening on 0.0.0.0 it can be bound by a service.
			Ports: []corev1.ContainerPort{
				{
					Name:          "http",
					ContainerPort: 48989,
				},
				{
					Name:          "https",
					ContainerPort: 48984,
				},
				{
					Name:          "rtsp",
					ContainerPort: session.Status.Ports.RTSP,
				},
				{
					Name:          "enet",
					ContainerPort: session.Status.Ports.Control,
				},
				{
					Name:          "video",
					ContainerPort: session.Status.Ports.VideoRTP,
				},
				{
					Name:          "audio",
					ContainerPort: session.Status.Ports.AudioRTP,
				},
			},
			Resources: wolfResources,
			VolumeMounts: append([]corev1.VolumeMount{
				{
					Name:      "wolf-cfg",
					MountPath: "/etc/wolf",
				},
				{
					Name:      "wolf-runtime",
					MountPath: "/tmp/.X11-unix",
				},
				{
					Name:      "wolf-data",
					MountPath: "/mnt/data/wolf",
				},
				// {
				// 	Name:      "dev-input",
				// 	MountPath: "/dev/input",
				// },
				// {
				// 	Name:      "dev-uinput",
				// 	MountPath: "/dev/uinput",
				// },
				// {
				// 	Name:      "host-udev",
				// 	MountPath: "/run/udev", //Need to find a more secure way to mount this
				// },
			}, wolfVolumeMounts...),
		},
	)

	var wolfDataVolumeSource corev1.VolumeSource
	if app.Spec.VolumeClaimTemplate != nil {
		wolfDataVolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: c.deploymentName(session),
			},
		}
	} else {
		wolfDataVolumeSource = corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}
	}

	podToCreate.Spec.Volumes = append(podToCreate.Spec.Volumes,
		corev1.Volume{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: c.deploymentName(session),
					},
				},
			},
		},
		corev1.Volume{
			Name: "wolf-cfg",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: "wolf-runtime",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name:         "wolf-data",
			VolumeSource: wolfDataVolumeSource,
		},
		// corev1.Volume{ //Needs to be changed into something more secure, without host path
		// 	Name: "dev-input",
		// 	VolumeSource: corev1.VolumeSource{
		// 		HostPath: &corev1.HostPathVolumeSource{
		// 			Path: "/dev/input",
		// 			Type: ptr.To(corev1.HostPathDirectory),
		// 		},
		// 	},
		// },
		// I'm moving this to volumeConfig
		// corev1.Volume{
		// 	Name: "dev-uinput",
		// 	VolumeSource: corev1.VolumeSource{
		// 		HostPath: &corev1.HostPathVolumeSource{
		// 			Path: "/dev/uinput",
		// 			Type: ptr.To(corev1.HostPathFile),
		// 		},
		// 	},
		// },
		// corev1.Volume{ //Needs to be changed into something more secure, without host path
		// 	Name: "host-udev",
		// 	VolumeSource: corev1.VolumeSource{
		// 		HostPath: &corev1.HostPathVolumeSource{
		// 			Path: "/run/udev",
		// 			Type: ptr.To(corev1.HostPathDirectory),
		// 		},
		// 	},
		// },
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
			Name:      c.deploymentName(session),
			Namespace: session.Namespace,
			Labels: map[string]string{
				"app":           "direwolf-worker",
				"direwolf/app":  session.Spec.GameReference.Name,
				"direwolf/user": session.Spec.UserReference.Name,
			},
			OwnerReferences: owners,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"direwolf/app":  session.Spec.GameReference.Name,
					"direwolf/user": session.Spec.UserReference.Name,
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
		return fmt.Errorf("failed to convert deployment to unstructured: %s", err)
	}

	// NOTE: Kinda dumb cuz its just gona get serialized again....
	// could just use dynamic client
	var deploymentApplyConfig appsv1ac.DeploymentApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredDeployment, &deploymentApplyConfig)
	if err != nil {
		return fmt.Errorf("failed to convert unstructured to deployment: %s", err)
	}

	_, err = c.K8sClient.AppsV1().Deployments(session.Namespace).Apply(
		ctx,
		&deploymentApplyConfig,
		metav1.ApplyOptions{
			FieldManager: "direwolf-session-controller-deployment",
		})

	if err != nil {
		return fmt.Errorf("failed to apply deployment: %s", err)
	}

	return nil
}

func (c *SessionController) reconcileConfigMap(
	ctx context.Context,
	session *v1alpha1types.Session,
) error {
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %s", err)
	}

	user, err := c.UserInformer.Namespaced(session.Namespace).Get(session.Spec.UserReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user: %s", err)
	}

	wolfConfig, err := GenerateWolfConfig(app)
	if err != nil {
		return fmt.Errorf("failed to generate wolf config: %s", err)
	}
	deploymentName := c.deploymentName(session)

	_, err = c.K8sClient.CoreV1().
		ConfigMaps(session.Namespace).
		Apply(
			context.Background(),
			v1ac.ConfigMap(deploymentName, session.Namespace).
				WithLabels(
					map[string]string{
						"app":           "direwolf-worker",
						"direwolf/app":  session.Spec.GameReference.Name,
						"direwolf/user": session.Spec.UserReference.Name,
					}).
				WithOwnerReferences(
					metav1ac.OwnerReference().
						WithName(app.Name).
						WithAPIVersion(v1alpha1.GroupVersion.String()).
						WithKind("App").
						WithUID(app.UID).
						WithController(true),
					metav1ac.OwnerReference().
						WithName(user.Name).
						WithAPIVersion(v1alpha1.GroupVersion.String()).
						WithKind("User").
						WithUID(user.UID),
				).
				WithData(map[string]string{
					"config.toml": wolfConfig,
				}),
			metav1.ApplyOptions{
				FieldManager: "direwolf-session-controller",
			})
	if err != nil {
		return fmt.Errorf("failed to apply configmap: %s", err)
	}
	return nil
}

func (c *SessionController) reconcilePVC(ctx context.Context, session *v1alpha1types.Session) error {
	user, err := c.UserInformer.Namespaced(session.Namespace).Get(session.Spec.UserReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user: %s", err)
	}
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get user: %s", err)
	}

	// Check if the user defined a volume claim template. If not, return nil.
	if app.Spec.VolumeClaimTemplate == nil {
		klog.Infof("App %s does not define a VolumeClaimTemplate, skipping PVC creation.", app.Name)
		return nil
	}

	pvcName := c.deploymentName(session)
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
	// Note: Default storage class is handled by Kubernetes if StorageClassName is nil.

	// Build the PVC spec apply configuration from the defaulted spec
	pvcSpec := v1ac.PersistentVolumeClaimSpec().
		WithAccessModes(templateSpec.AccessModes...).
		WithResources(v1ac.VolumeResourceRequirements().
			WithLimits(templateSpec.Resources.Limits).
			WithRequests(templateSpec.Resources.Requests))

	if templateSpec.Selector != nil {
		selectorConfig := metav1ac.LabelSelector()
		if len(templateSpec.Selector.MatchLabels) > 0 {
			selectorConfig.WithMatchLabels(templateSpec.Selector.MatchLabels)
		}
		if len(templateSpec.Selector.MatchExpressions) > 0 {
			var expressions []*metav1ac.LabelSelectorRequirementApplyConfiguration
			for _, req := range templateSpec.Selector.MatchExpressions {
				expressions = append(expressions, metav1ac.LabelSelectorRequirement().
					WithKey(req.Key).
					WithOperator(req.Operator).
					WithValues(req.Values...))
			}
			selectorConfig.WithMatchExpressions(expressions...)
		}
		pvcSpec.WithSelector(selectorConfig)
	}
	if templateSpec.StorageClassName != nil {
		pvcSpec.WithStorageClassName(*templateSpec.StorageClassName)
	}
	if templateSpec.VolumeMode != nil {
		pvcSpec.WithVolumeMode(*templateSpec.VolumeMode)
	}
	if templateSpec.DataSource != nil {
		dsConfig := v1ac.TypedLocalObjectReference().
			WithKind(templateSpec.DataSource.Kind).
			WithName(templateSpec.DataSource.Name)
		if templateSpec.DataSource.APIGroup != nil {
			dsConfig.WithAPIGroup(*templateSpec.DataSource.APIGroup)
		}
		pvcSpec.WithDataSource(dsConfig)
	}
	if templateSpec.DataSourceRef != nil {
		dsrConfig := v1ac.TypedObjectReference().
			WithKind(templateSpec.DataSourceRef.Kind).
			WithName(templateSpec.DataSourceRef.Name)
		if templateSpec.DataSourceRef.APIGroup != nil {
			dsrConfig.WithAPIGroup(*templateSpec.DataSourceRef.APIGroup)
		}
		if templateSpec.DataSourceRef.Namespace != nil {
			dsrConfig.WithNamespace(*templateSpec.DataSourceRef.Namespace)
		}
		pvcSpec.WithDataSourceRef(dsrConfig)
	}

	_, err = c.K8sClient.CoreV1().PersistentVolumeClaims(session.Namespace).Apply(
		ctx,
		v1ac.PersistentVolumeClaim(pvcName, session.Namespace).
			WithLabels(map[string]string{
				"app":           "direwolf-worker",
				"direwolf/app":  session.Spec.GameReference.Name,
				"direwolf/user": session.Spec.UserReference.Name,
			}).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithName(user.Name).
				WithAPIVersion(v1alpha1.GroupVersion.String()).
				WithKind("User").
				WithUID(user.UID).
				WithController(true)).
			WithSpec(pvcSpec),
		metav1.ApplyOptions{
			FieldManager: "direwolf-session-controller-pvc",
		},
	)

	if err != nil {
		return fmt.Errorf("failed to apply PVC %s: %w", pvcName, err)
	}

	return nil
}
func (c *SessionController) deploymentName(session *v1alpha1types.Session) string {
	return fmt.Sprintf("%s-%s", session.Spec.UserReference.Name, session.Spec.GameReference.Name)
}

func (c *SessionController) allocatePorts(
	ctx context.Context,
	session *v1alpha1types.Session,
) error {
	//!TODO: Take lock if multiple workers are running

	// 0. Allocate ports for this streaming session to use
	// 1. List all listeners for the gateway
	// 2. List all routes attached to the gateway
	// 3. Subtract used ports
	// 4. Choose a port for RTSP, Enet, Video RTP, Audio RTP

	//!TODO: Implement this properly once wolf lets us assign ports. For now, just
	// hardcode some ports.
	session.Status.Ports = v1alpha1types.SessionPorts{
		RTSP:     48010,
		Control:  47999,
		VideoRTP: 48100,
		AudioRTP: 48200,
	}

	return nil
}

// reconcileActiveStreams calls out to wolf-agent on the running pod to ensure
// that wolf is configured in the correct state and listening for streams on the
// correct ports for each session trying to connect to the Pod.
func (c *SessionController) reconcileActiveStreams(
	ctx context.Context,
	session *v1alpha1types.Session,
) error {
	deploymentName := c.deploymentName(session)

	// !TODO: Use informer for cache reads instead?
	deployment, err := c.K8sClient.AppsV1().Deployments(session.Namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %s", err)
	}

	if deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.ReadyReplicas != deployment.Status.Replicas {
		return fmt.Errorf("deployment %s/%s not ready (Observed %d, Latest %d) (%d/%d)", session.Namespace, deploymentName, deployment.Status.ObservedGeneration, deployment.Generation, deployment.Status.ReadyReplicas, deployment.Status.Replicas)
	}

	// Get service for the deployment
	service, err := c.K8sClient.CoreV1().Services(session.Namespace).Get(ctx, session.Status.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service: %s", err)
	}

	// List all the "sessions".
	// Ensure they match each of our k8s sessions. Hash on AESKey/IV
	// In the future it might make sense to just match on ClientID/ClientCertFingerprint
	// but that is hardcoded for now :)
	wolfclient := wolfapi.NewClient(fmt.Sprintf("https://%s:8443", service.Spec.ClusterIP), &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	})
	sessions, err := wolfclient.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sessions: %s", err)
	}

	keyIVHash := util.Hash([]byte(session.Spec.Config.AESKey), []byte(session.Spec.Config.AESIV))
	var found bool
	for _, s := range sessions {
		sHash := util.Hash([]byte(s.AESKey), []byte(s.AESIV))
		if bytes.Equal(sHash, keyIVHash) {
			found = true
			break
		}
	}

	if found != (session.Status.WolfSessionID != "") {
		klog.Infof("Session %s/%s found: %v, status: %v", session.Namespace, session.Name, found, session.Status.WolfSessionID)
		// Either the session was already added but not in the list, or
		// the session was already in the list without being added.
		//
		// Either scenario is invalid. Delete the session
		return c.SessionClient.Delete(ctx, session.Name, metav1.DeleteOptions{})
	}

	if !found {
		//!TODO: Add the ports into the request for this to support multiple
		// sessions per Gateway.
		//
		// Create the session
		sessionID, err := wolfclient.AddSession(ctx, wolfapi.Session{
			VideoWidth:        session.Spec.Config.VideoWidth,
			VideoHeight:       session.Spec.Config.VideoHeight,
			VideoRefreshRate:  session.Spec.Config.VideoRefreshRate,
			AppID:             "1",
			AudioChannelCount: 2, // !TODO: parse from audio info
			ClientIP:          "10.128.1.0",
			ClientSettings: wolfapi.ClientSettings{
				RunGID:              1000,
				RunUID:              1000,
				ControllersOverride: []string{"XBOX"},
				MouseAcceleration:   1.0,
				VScrollAcceleration: 1.0,
				HScrollAcceleration: 1.0,
			},
			AESKey: session.Spec.Config.AESKey,
			AESIV:  session.Spec.Config.AESIV,
			//!TODO: not this. This is the hash of the client cert we are
			// hardcoding into wolf config. Should call pair endpoint to genuinely
			// add it. Though not really needed since user doesnt connect via HTTPS
			// to wolf, we just need a client ID wolf accepts for this specific
			// pairing/client...
			ClientID:   "4193251087262667199",
			RTSPFakeIP: service.Spec.ClusterIP,
		})
		if err != nil {
			return fmt.Errorf("failed to create session: %s", err)
		}

		session.Status.WolfSessionID = sessionID
	} else {
		//!TODO: Update wolf API to include session ID in list so we can update
		// these details/validate discrepencies
		// assert wolf session ID non-empty and matches what we expect
	}

	session.Status.StreamURL = fmt.Sprintf("rtsp://%s:%d", service.Spec.ClusterIP, session.Status.Ports.RTSP)
	return nil
}

func GenerateWolfConfig(
	app *v1alpha1.App,
) (string, error) {
	config := app.Spec.WolfConfig

	if len(config.Title) == 0 {
		config.Title = app.Spec.Title
	}

	if config.StartAudioServer == nil {
		config.StartAudioServer = ptr.To(true)
	}

	if config.StartVirtualCompositor == nil {
		config.StartVirtualCompositor = ptr.To(true)
	}

	if config.Runner == nil {
		config.Runner = &v1alpha1.WolfRunnerConfig{
			Type:       "process",
			RunCommand: "sh -c \"while :; do echo 'running...'; sleep 10; done\"",
		}
	}

	var gstreamerConfig = map[string]interface{}{
		"audio": map[string]interface{}{
			"default_source":       `interpipesrc listen-to={session_id}_audio is-live=true stream-sync=restart-ts max-bytes=0 max-buffers=3 block=false`,
			"default_audio_params": `queue max-size-buffers=3 leaky=downstream ! audiorate ! audioconvert`,
			"default_opus_encoder": `opusenc bitrate={bitrate} bitrate-type=cbr frame-size={packet_duration} bandwidth=fullband audio-type=restricted-lowdelay max-payload-size=1400`,
			"default_sink": `rtpmoonlightpay_audio name=moonlight_pay packet_duration={packet_duration} encrypt={encrypt} aes_key="{aes_key}" aes_iv="{aes_iv}" !
	appsink name=wolf_udp_sink`,
		},
		"video": map[string]interface{}{
			"default_source": `interpipesrc listen-to={session_id}_video is-live=true stream-sync=restart-ts max-bytes=0 max-buffers=1 leaky-type=downstream`,
			"default_sink": `rtpmoonlightpay_video name=moonlight_pay payload_size={payload_size} fec_percentage={fec_percentage} min_required_fec_packets={min_required_fec_packets} !
	appsink sync=false name=wolf_udp_sink`,
			"defaults": map[string]interface{}{
				"nvcodec": map[string]interface{}{
					"video_params": `cudaupload !
	cudaconvertscale !
	video/x-raw(memory:CUDAMemory), width={width}, height={height}, chroma-site={color_range}, format=NV12, colorimetry={color_space}, pixel-aspect-ratio=1/1`,
					"video_params_zero_copy": `glupload !
	glcolorconvert !
	video/x-raw(memory:GLMemory),format=NV12, width={width}, height={height}, chroma-site={color_range}, colorimetry={color_space}, pixel-aspect-ratio=1/1`,
				},
				"qsv": map[string]interface{}{
					"video_params": `videoconvertscale !
	video/x-raw, chroma-site={color_range}, width={width}, height={height}, format=NV12, colorimetry={color_space}, pixel-aspect-ratio=1/1`,
					"video_params_zero_copy": `vapostproc !
	video/x-raw(memory:VAMemory),format=NV12, chroma-site={color_range}, colorimetry={color_space}, pixel-aspect-ratio=1/1`,
				},
				"va": map[string]interface{}{
					"video_params": `vapostproc !
	video/x-raw, chroma-site={color_range}, width={width}, height={height}, format=NV12, colorimetry={color_space}, pixel-aspect-ratio=1/1`,
					"video_params_zero_copy": `vapostproc !
	video/x-raw(memory:VAMemory), format=NV12, chroma-site={color_range}, width={width}, height={height}, colorimetry={color_space}, pixel-aspect-ratio=1/1`,
				},
			},
			"hevc_encoders": []map[string]interface{}{
				{
					"plugin_name":    "nvcodec",
					"check_elements": []string{"nvh265enc", "cudaconvertscale", "cudaupload"},
					"encoder_pipeline": `nvh265enc gop-size=-1 bitrate={bitrate} aud=false rc-mode=cbr zerolatency=true preset=p1 tune=ultra-low-latency multi-pass=two-pass-quarter !
	h265parse !
	video/x-h265, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "qsv",
					"check_elements": []string{"qsvh265enc", "vapostproc"},
					"encoder_pipeline": `qsvh265enc b-frames=0 gop-size=0 idr-interval=1 ref-frames=1 bitrate={bitrate} rate-control=cbr low-latency=1 target-usage=6 !
	h265parse !
	video/x-h265, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "va",
					"check_elements": []string{"vah265enc", "vapostproc"},
					"encoder_pipeline": `vah265enc aud=false b-frames=0 ref-frames=1 num-slices={slices_per_frame} bitrate={bitrate} cpb-size={bitrate} key-int-max=1024 rate-control=cqp target-usage=6 !
	h265parse !
	video/x-h265, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "va",
					"check_elements": []string{"vah265lpenc", "vapostproc"},
					"encoder_pipeline": `vah265lpenc aud=false b-frames=0 ref-frames=1 num-slices={slices_per_frame} bitrate={bitrate} cpb-size={bitrate} key-int-max=1024 rate-control=cqp target-usage=6 !
	h265parse !
	video/x-h265, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "x265",
					"check_elements": []string{"x265enc"},
					"video_params": `videoconvertscale !
	videorate !
	video/x-raw, width={width}, height={height}, framerate={fps}/1, format=I420, chroma-site={color_range}, colorimetry={color_space}`,
					"video_params_zero_copy": `videoconvertscale !
	videorate !
	video/x-raw, width={width}, height={height}, framerate={fps}/1, format=I420, chroma-site={color_range}, colorimetry={color_space}`,
					"encoder_pipeline": `x265enc tune=zerolatency speed-preset=superfast bitrate={bitrate} option-string="info=0:keyint=-1:qp=28:repeat-headers=1:slices={slices_per_frame}:aud=0:annexb=1:log-level=3:open-gop=0:bframes=0:intra-refresh=0" !
	video/x-h265, profile=main, stream-format=byte-stream`,
				},
			},
			"h264_encoders": []map[string]interface{}{
				{
					"plugin_name":    "nvcodec",
					"check_elements": []string{"nvh264enc", "cudaconvertscale", "cudaupload"},
					"encoder_pipeline": `nvh264enc preset=low-latency-hq zerolatency=true gop-size=0 rc-mode=cbr-ld-hq bitrate={bitrate} aud=false !
	h264parse !
	video/x-h264, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "qsv",
					"check_elements": []string{"qsvh264enc", "vapostproc"},
					"encoder_pipeline": `qsvh264enc b-frames=0 gop-size=0 idr-interval=1 ref-frames=1 bitrate={bitrate} rate-control=cbr target-usage=6 !
	h264parse !
	video/x-h264, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "va",
					"check_elements": []string{"vah264enc", "vapostproc"},
					"encoder_pipeline": `vah264enc aud=false b-frames=0 ref-frames=1 num-slices={slices_per_frame} bitrate={bitrate} cpb-size={bitrate} key-int-max=1024 rate-control=cqp target-usage=6 !
	h264parse !
	video/x-h264, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "va",
					"check_elements": []string{"vah264lpenc", "vapostproc"},
					"encoder_pipeline": `vah264lpenc aud=false b-frames=0 ref-frames=1 num-slices={slices_per_frame} bitrate={bitrate} cpb-size={bitrate} key-int-max=1024 rate-control=cqp target-usage=6 !
	h264parse !
	video/x-h264, profile=main, stream-format=byte-stream`,
				},
				{
					"plugin_name":    "x264",
					"check_elements": []string{"x264enc"},
					"encoder_pipeline": `x264enc pass=qual tune=zerolatency speed-preset=superfast b-adapt=false bframes=0 ref=1 sliced-threads=true threads={slices_per_frame} option-string="slices={slices_per_frame}:keyint=infinite:open-gop=0" b-adapt=false bitrate={bitrate} aud=false !
	video/x-h264, profile=high, stream-format=byte-stream`,
				},
			},
			"av1_encoders": []map[string]interface{}{
				{
					"plugin_name":    "nvcodec",
					"check_elements": []string{"nvav1enc", "cudaconvertscale", "cudaupload"},
					"encoder_pipeline": `nvav1enc gop-size=-1 bitrate={bitrate} rc-mode=cbr zerolatency=true preset=p1 tune=ultra-low-latency multi-pass=two-pass-quarter !
	av1parse !
	video/x-av1, stream-format=obu-stream, alignment=frame, profile=main`,
				},
				{
					"plugin_name":    "qsv",
					"check_elements": []string{"qsvav1enc", "vapostproc"},
					"encoder_pipeline": `qsvav1enc gop-size=0 ref-frames=1 bitrate={bitrate} rate-control=cbr low-latency=1 target-usage=6 !
	av1parse !
	video/x-av1, stream-format=obu-stream, alignment=frame, profile=main`,
				},
				{
					"plugin_name":    "va",
					"check_elements": []string{"vaav1enc", "vapostproc"},
					"encoder_pipeline": `vaav1enc ref-frames=1 bitrate={bitrate} cpb-size={bitrate} key-int-max=1024 rate-control=cqp target-usage=6 !
	av1parse !
	video/x-av1, stream-format=obu-stream, alignment=frame, profile=main`,
				},
				{
					"plugin_name":    "va",
					"check_elements": []string{"vaav1lpenc", "vapostproc"},
					"encoder_pipeline": `vaav1lpenc ref-frames=1 bitrate={bitrate} cpb-size={bitrate} key-int-max=1024 rate-control=cqp target-usage=6 !
	av1parse !
	video/x-av1, stream-format=obu-stream, alignment=frame, profile=main`,
				},
				{
					"plugin_name":    "aom",
					"check_elements": []string{"av1enc"},
					"video_params": `videoconvertscale !
	videorate !
	video/x-raw, width={width}, height={height}, framerate={fps}/1, format=I420, chroma-site={color_range}, colorimetry={color_space}`,
					"video_params_zero_copy": `videoconvertscale !
	videorate !
	video/x-raw, width={width}, height={height}, framerate={fps}/1, format=I420, chroma-site={color_range}, colorimetry={color_space}`,
					"encoder_pipeline": `av1enc usage-profile=realtime end-usage=vbr target-bitrate={bitrate} !
	av1parse !
	video/x-av1, stream-format=obu-stream, alignment=frame, profile=main`,
				},
			},
		},
	}

	configMap := map[string]interface{}{
		"config_version": 4,
		"hostname":       "Direwolf",
		"uuid":           "dd7c60f6-4b88-4ef1-be07-eeec72f96080",
		"apps":           []any{config},

		//!TODO: Send PR to wolf to populate the default gstreamer config
		// if its not provided? Or start with empty wolf and use api to
		// populate application
		"gstreamer": gstreamerConfig,

		//!TODO: Doesnt really matter since we handle moonlight. But
		// we need a client to associate to streams. It would be better though
		// to actually mirror the real moonlight clients into wolf via API
		"paired_clients": []interface{}{
			map[string]interface{}{
				"app_state_folder": "state",
				"client_cert": `-----BEGIN CERTIFICATE-----
MIICvzCCAaegAwIBAgIBADANBgkqhkiG9w0BAQsFADAjMSEwHwYDVQQDDBhOVklE
SUEgR2FtZVN0cmVhbSBDbGllbnQwHhcNMjQxMjE2MDgzMTE4WhcNNDQxMjExMDgz
MTE4WjAjMSEwHwYDVQQDDBhOVklESUEgR2FtZVN0cmVhbSBDbGllbnQwggEiMA0G
CSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCXcEKK/Desa7EcvntGHxtA3ercOxbd
kUtkPPacz7mKVZBKayZmbfTMAQV5dS2yqeyZOId+X4JEPO7DeuMkkr9INnvl3etB
WIj0q8FTuBrGAb+XozFTb3Tvo3plezpLXecl4mquvXA2mEtVILnm6NltdJ+GYNkT
UBbOjneZIdBfFjIP+0k2JZD+VmnxmwCDlPryMa2nFi8rNAkYSfIWdyIlOzUJfZ/i
8Hw4wLtSGy5W7YZ3kbQQJzPkW6pLFbREcNmslprwxZduo3mDAyDndj9qsVqopJOF
Oj3Nlzj53kNRozgB9b/wkNcxi3lvOoQrNGJNTp39WNDmPFVzfBvCKVDJAgMBAAEw
DQYJKoZIhvcNAQELBQADggEBAA+Rh4KAuTYtcH8X5RdUstjGXiYbONMmEuKl/kE/
hj8ddefXA4pjf1Vozx6NunMlC4g0QQAZrsxn1NBVe/5L3gxrwyYLn/2kDJUw7P5o
aTXnL5xYzhcPjQOER9+36S4aUTpwR/rURK0MyOmZOVk3Ex4rAnyetKg3Dd9v6uL3
zaycOje4fxJpVH713NbFaGLMeKPW61lW+Lh9WlXOKrd0EABVBPmSYlk8gYnPrXxA
dxohk8q/MqUqcm/k8ZGKYMc998ix6ldXJm5xaPTXSQcSC/xycoLjnUCkcv+sfh1T
nKI+KlDXa1HikPGT/uB/b+SS6v9bD8kU03Ci4ahdKb6Mw7Q=
-----END CERTIFICATE-----
`,
			},
		},
	}

	data, err := toml.Marshal(configMap)
	if err != nil {
		return "", fmt.Errorf("failed to marshal toml: %s", err)
	}

	return string(data), nil
}
