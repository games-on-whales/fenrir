package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	resourcev1ac "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// LobbyController manages the lifecycle of lobbies for
// a game, of a given profile.
// It is responsible for:
//   - 1. Creating the statefulset and their lobby resourceClaims
//   - 2. Updating the statefulset, and lobby readiness
//
// The statefulset readiness is what allows the session Controller to prepare the streamUrl for the user
type LobbyController struct {
	LobbyClient     v1alpha1client.LobbyInterface
	LobbyInformer   generic.Informer[*direwolfv1alpha1.Lobby]
	AppInformer     generic.Informer[*direwolfv1alpha1.App]
	ProfileInformer generic.Informer[*direwolfv1alpha1.Profile]
	K8sClient       kubernetes.Interface

	controller            generic.Controller[*direwolfv1alpha1.Lobby]
	statefulSetController generic.Controller[*appsv1.StatefulSet]

	SessionControllerOptions
}

// NewLobbyController creates a new lobby controller.
func NewLobbyController(
	k8sClient kubernetes.Interface,
	lobbyClient v1alpha1client.LobbyInterface,
	lobbyInformer generic.Informer[*direwolfv1alpha1.Lobby],
	appInformer generic.Informer[*direwolfv1alpha1.App],
	profileInformer generic.Informer[*direwolfv1alpha1.Profile],
	statefulSetInformer generic.Informer[*appsv1.StatefulSet],
	options SessionControllerOptions,
) *LobbyController {
	res := &LobbyController{
		K8sClient:                k8sClient,
		LobbyClient:              lobbyClient,
		LobbyInformer:            lobbyInformer,
		AppInformer:              appInformer,
		ProfileInformer:          profileInformer,
		SessionControllerOptions: options,
	}

	res.controller = generic.NewController(
		lobbyInformer,
		res.Reconcile,
		generic.ControllerOptions{
			Name:    "lobby-controller",
			Workers: 2,
		},
	)

	res.statefulSetController = generic.NewController(
		statefulSetInformer,
		func(_, _ string, newObj *appsv1.StatefulSet) error {
			if newObj == nil {
				return nil
			}
			return res.reconcileDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "lobby-controller-statefulset",
			Workers: 2,
		},
	)

	return res
}

func (c *LobbyController) Run(ctx context.Context) error {
	lobbyCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(lobbyCtx.Done(), c.LobbyInformer.HasSynced) {
		return errors.New("failed to sync lobby informer")
	}

	go func() {
		defer cancel()
		err := c.statefulSetController.Run(lobbyCtx)
		if err != nil {
			klog.Errorf("Failed to run statefulset controller: %v", err)
		}
	}()

	if err := c.controller.Run(lobbyCtx); err != nil {
		return fmt.Errorf("failed to run lobby controller: %w", err)
	}
	return nil
}

func (c *LobbyController) HasSynced() bool {
	return c.LobbyInformer.HasSynced()
}

func (c *LobbyController) Reconcile(namespace, name string, newObj *direwolfv1alpha1.Lobby) error {
	klog.Infof("Reconciling lobby %s/%s", namespace, name)
	defer klog.Infof("Finished reconciling lobby %s/%s", namespace, name)

	if newObj == nil {
		// Deletion is handled by Kubernetes GC via owner references.
		return nil
	}

	oldStatus := newObj.Status.DeepCopy()

	// 1. ResourceClaim (owned by Lobby)
	claimName, err := c.reconcileResourceClaim(context.TODO(), newObj)
	if err != nil {
		klog.Errorf("Failed to reconcile resource claim: %v", err)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "ResourceClaimCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "ResourceClaimCreationFailed",
			Message: err.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "ResourceClaimCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	// 2. StatefulSet (owned by Lobby)
	if err == nil {
		if ssErr := c.reconcileStatefulSet(context.TODO(), newObj, claimName); ssErr != nil {
			klog.Errorf("Failed to reconcile statefulset: %v", ssErr)
			meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
				Type:    "StatefulSetCreated",
				Status:  metav1.ConditionFalse,
				Reason:  "StatefulSetCreationFailed",
				Message: ssErr.Error(),
			})
		} else {
			meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
				Type:   "StatefulSetCreated",
				Status: metav1.ConditionTrue,
				Reason: "Success",
			})
		}
	}

	// 3. Pod / Node discovery via Pod API (correct across all claims: GPU, DRA, PVC, etc.)
	statefulSetName := c.statefulSetName(newObj)
	podName := statefulSetName + "-0"

	newObj.Status.StatefulSetName = statefulSetName
	newObj.Status.PodName = podName

	ready := false
	nodeName := ""

	if err == nil {
		ss, ssErr := c.K8sClient.AppsV1().StatefulSets(namespace).Get(context.TODO(), statefulSetName, metav1.GetOptions{})
		if ssErr == nil &&
			ss.Status.ObservedGeneration == ss.Generation &&
			ss.Status.ReadyReplicas == *ss.Spec.Replicas {
			pod, podErr := c.K8sClient.CoreV1().Pods(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
			if podErr == nil && pod.Spec.NodeName != "" {
				nodeName = pod.Spec.NodeName
				ready = true
			}
		}
	}

	newObj.Status.NodeName = nodeName
	newObj.Status.StatefulSetReady = ready
	newObj.Status.LobbyNode = nodeName

	if ready {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "Ready",
			Status: metav1.ConditionTrue,
			Reason: "PodRunning",
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "Ready",
			Status:  metav1.ConditionFalse,
			Reason:  "PodNotReady",
			Message: "Lobby pod is not yet running and ready",
		})
	}

	// 4. Write status
	if !reflect.DeepEqual(newObj.Status, oldStatus) {
		_, err := c.LobbyClient.UpdateStatus(
			context.TODO(),
			newObj,
			metav1.UpdateOptions{
				FieldManager: "lobby-controller-status",
			},
		)
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to update lobby status: %w", err)
		}
	}

	return nil
}

func (c *LobbyController) reconcileDependant(obj metav1.Object) error {
	if obj.GetLabels() == nil {
		klog.V(2).Infof("Dependant %s/%s has no labels, skipping", obj.GetNamespace(), obj.GetName())
		return nil
	}

	appName, hasApp := obj.GetLabels()[direwolfv1alpha1.LabelApp]
	profileName, hasProfile := obj.GetLabels()[direwolfv1alpha1.LabelProfile]
	if !hasApp || !hasProfile {
		klog.V(2).Infof("Dependant %s/%s missing direwolf labels (app=%v, profile=%v), skipping",
			obj.GetNamespace(), obj.GetName(), hasApp, hasProfile)
		return nil
	}

	// First: try direct Lobby ownership via ownerReferences
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == "Lobby" {
			klog.V(2).Infof("Dependant %s/%s owned by Lobby %s, enqueuing",
				obj.GetNamespace(), obj.GetName(), owner.Name)
			c.controller.Enqueue(obj.GetNamespace(), owner.Name)
			return nil
		}
	}

	// Fallback: find Lobby by app+profile labels
	lobbies, err := c.LobbyInformer.List(labels.SelectorFromSet(labels.Set{
		direwolfv1alpha1.LabelApp:     appName,
		direwolfv1alpha1.LabelProfile: profileName,
	}))
	if err != nil {
		klog.Errorf("Failed to list lobbies for dependant %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		return fmt.Errorf("failed to list lobbies: %w", err)
	}
	if len(lobbies) == 0 {
		klog.V(2).Infof("No lobbies found matching app=%s profile=%s for dependant %s/%s",
			appName, profileName, obj.GetNamespace(), obj.GetName())
		return nil
	}

	for _, lobby := range lobbies {
		klog.V(2).Infof("Enqueuing lobby %s/%s because dependant %s/%s changed",
			lobby.Namespace, lobby.Name, obj.GetNamespace(), obj.GetName())
		c.controller.Enqueue(lobby.Namespace, lobby.Name)
	}
	return nil
}

func (c *LobbyController) statefulSetName(lobby *direwolfv1alpha1.Lobby) string {
	return fmt.Sprintf("%s-%s", lobby.Spec.ProfileReference.Name, lobby.Spec.AppReference.Name)
}

func (c *LobbyController) reconcileResourceClaim(ctx context.Context, lobby *direwolfv1alpha1.Lobby) (string, error) {
	claimName := c.statefulSetName(lobby) + "-lobby-claim"
	// TODO: Figure out channel count vs moonlight configs
	channelCount := 2 // lobby.Spec.AudioSettings.ChannelCount
	if channelCount <= 0 {
		channelCount = 2
	}
	// TODO: create a struct for this object in the wolfapi
	// Or just use wolfapi.lobbyCreateRequest????
	params := struct {
		VideoSettings  wolfapi.LobbyVideoSettings `json:"video_settings"`
		AudioSettings  wolfapi.LobbyAudioSettings `json:"audio_settings"`
		ClientSettings wolfapi.ClientSettings     `json:"client_settings"`
		MultiUser      bool                       `json:"multi_user"`
	}{
		VideoSettings: wolfapi.LobbyVideoSettings{
			Width:       lobby.Spec.VideoSettings.Width,
			Height:      lobby.Spec.VideoSettings.Height,
			RefreshRate: lobby.Spec.VideoSettings.RefreshRate,
		},
		AudioSettings: wolfapi.LobbyAudioSettings{
			ChannelCount: channelCount,
		},
		// TODO: Figure out where to even get these parameters
		ClientSettings: wolfapi.ClientSettings{
			HScrollAcceleration: 1,
			MouseAcceleration:   1,
			RunGID:              1000,
			RunUID:              1000,
			VScrollAcceleration: 1,
		},
		MultiUser: lobby.Spec.MultiUser,
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal opaque params: %w", err)
	}

	_, err = c.K8sClient.ResourceV1().ResourceClaims(lobby.Namespace).Apply(
		ctx,
		resourcev1ac.ResourceClaim(claimName, lobby.Namespace).
			WithLabels(map[string]string{
				direwolfv1alpha1.LabelApp:     lobby.Spec.AppReference.Name,
				direwolfv1alpha1.LabelProfile: lobby.Spec.ProfileReference.Name,
			}).
			WithOwnerReferences(metav1ac.OwnerReference().
				WithAPIVersion(direwolfv1alpha1.GroupVersion.String()).
				WithKind("Lobby").
				WithName(lobby.Name).
				WithUID(lobby.UID).
				WithController(true).
				WithBlockOwnerDeletion(true)).
			WithSpec(resourcev1ac.ResourceClaimSpec().
				WithDevices(resourcev1ac.DeviceClaim().
					WithConfig(resourcev1ac.DeviceClaimConfiguration().
						WithOpaque(resourcev1ac.OpaqueDeviceConfiguration().
							WithDriver("wolf.dra.io").
							WithParameters(runtime.RawExtension{Raw: rawParams})).
						WithRequests("lobby")).
					WithRequests(resourcev1ac.DeviceRequest().
						WithName("lobby").
						WithExactly(resourcev1ac.ExactDeviceRequest().
							WithAllocationMode("ExactCount").
							WithCount(1).
							WithDeviceClassName("default-wolf").
							WithCapacity(
								resourcev1ac.CapacityRequirements().
									WithRequests(map[resourceapi.QualifiedName]resource.Quantity{
										"slots": resource.MustParse("1"),
									}),
							))))),
		metav1.ApplyOptions{
			FieldManager: "direwolf-lobby-controller",
			Force:        true,
		},
	)
	if err != nil {
		return "", fmt.Errorf("apply resource claim %s: %w", claimName, err)
	}

	klog.Infof("Applied ResourceClaim %s/%s for lobby %s", lobby.Namespace, claimName, lobby.Name)
	return claimName, nil
}

func (c *LobbyController) reconcileStatefulSet(ctx context.Context, lobby *direwolfv1alpha1.Lobby, claimName string) error {
	app, err := c.AppInformer.Namespaced(lobby.Namespace).Get(lobby.Spec.AppReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}
	profile, err := c.ProfileInformer.Namespaced(lobby.Namespace).Get(lobby.Spec.ProfileReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile %s: %w", lobby.Spec.ProfileReference.Name, err)
	}

	owners := []metav1.OwnerReference{
		{
			APIVersion:         direwolfv1alpha1.GroupVersion.String(),
			Kind:               "Lobby",
			Name:               lobby.Name,
			UID:                lobby.UID,
			Controller:         new(true),
			BlockOwnerDeletion: new(true),
		},
	}
	ownerApply := []*metav1ac.OwnerReferenceApplyConfiguration{
		metav1ac.OwnerReference().
			WithName(lobby.Name).
			WithAPIVersion(direwolfv1alpha1.GroupVersion.String()).
			WithKind("Lobby").
			WithUID(lobby.UID).
			WithController(true).
			WithBlockOwnerDeletion(true),
	}

	statefulSetName := c.statefulSetName(lobby)

	// Use API server directly instead of informer cache because it gets stale on rapid creation / deletion.
	// Question: Is this still the case?
	existingStatefulSet, err := c.K8sClient.AppsV1().StatefulSets(lobby.Namespace).Get(ctx, statefulSetName, metav1.GetOptions{})
	if err == nil {
		if existingStatefulSet.DeletionTimestamp != nil {
			return fmt.Errorf("statefulset %s/%s is being deleted, will retry", lobby.Namespace, statefulSetName)
		}
		klog.V(2).Infof("StatefulSet %s/%s already exists, just updating metadata", lobby.Namespace, statefulSetName)
		if _, err := c.K8sClient.AppsV1().StatefulSets(lobby.Namespace).Apply(
			ctx,
			appsv1ac.StatefulSet(statefulSetName, lobby.Namespace).
				WithOwnerReferences(ownerApply...),
			metav1.ApplyOptions{
				FieldManager: "direwolf-lobby-controller-statefulset-owners",
				Force:        true,
			},
		); err != nil {
			return fmt.Errorf("failed to apply owner references to statefulset %s/%s: %w", lobby.Namespace, statefulSetName, err)
		}
		return nil
	} else if !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing statefulset %s/%s: %w", lobby.Namespace, statefulSetName, err)
	}

	var podToCreate corev1.PodTemplateSpec
	if len(app.Spec.Template.Spec.Containers) > 0 {
		podToCreate.ObjectMeta = app.Spec.Template.ObjectMeta
		podToCreate.Spec = *app.Spec.Template.Spec.DeepCopy()
	}

	if podToCreate.Labels == nil {
		podToCreate.Labels = map[string]string{}
	}
	podToCreate.Labels["app"] = "direwolf-worker" //nolint
	podToCreate.Labels[direwolfv1alpha1.LabelApp] = lobby.Spec.AppReference.Name
	podToCreate.Labels[direwolfv1alpha1.LabelProfile] = lobby.Spec.ProfileReference.Name
	podToCreate.Spec.TerminationGracePeriodSeconds = ptr.To[int64](3)

	var volumeClaimTemplates []corev1.PersistentVolumeClaim
	for _, template := range app.Spec.VolumeClaimTemplates {
		claim := *template.DeepCopy()
		if len(claim.Spec.AccessModes) == 0 {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		}
		if claim.Spec.Resources.Requests == nil {
			claim.Spec.Resources.Requests = make(corev1.ResourceList)
		}
		if _, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("5Gi")
		}
		claim.OwnerReferences = append(claim.OwnerReferences, metav1.OwnerReference{
			APIVersion: direwolfv1alpha1.GroupVersion.String(),
			Kind:       "Profile",
			Name:       profile.Name,
			UID:        profile.UID,
			Controller: new(true),
		})
		volumeClaimTemplates = append(volumeClaimTemplates, claim)
	}

	podToCreate.Spec.ResourceClaims = []corev1.PodResourceClaim{
		{
			Name:              "lobby",
			ResourceClaimName: &claimName,
		},
	}
	for i := range podToCreate.Spec.Containers {
		podToCreate.Spec.Containers[i].Resources.Claims = append(
			podToCreate.Spec.Containers[i].Resources.Claims,
			corev1.ResourceClaim{
				Name: "lobby",
			},
		)
	}

	statefulSet := appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName,
			Namespace: lobby.Namespace,
			Labels: map[string]string{
				"app":                         "direwolf-worker", //nolint
				direwolfv1alpha1.LabelApp:     lobby.Spec.AppReference.Name,
				direwolfv1alpha1.LabelProfile: lobby.Spec.ProfileReference.Name,
			},
			OwnerReferences: owners,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					direwolfv1alpha1.LabelApp:     lobby.Spec.AppReference.Name,
					direwolfv1alpha1.LabelProfile: lobby.Spec.ProfileReference.Name,
				},
			},
			Template:             podToCreate,
			VolumeClaimTemplates: volumeClaimTemplates,
		},
	}

	unstructuredStatefulSet, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&statefulSet)
	if err != nil {
		return fmt.Errorf("failed to convert statefulset to unstructured: %w", err)
	}
	var statefulSetApplyConfig appsv1ac.StatefulSetApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredStatefulSet, &statefulSetApplyConfig)
	if err != nil {
		return fmt.Errorf("failed to convert unstructured to statefulset: %w", err)
	}

	_, err = c.K8sClient.AppsV1().StatefulSets(lobby.Namespace).Apply(
		ctx,
		&statefulSetApplyConfig,
		metav1.ApplyOptions{
			FieldManager: "direwolf-lobby-controller-statefulset",
		})
	if err != nil {
		return fmt.Errorf("failed to apply statefulset: %w", err)
	}

	return nil
}
