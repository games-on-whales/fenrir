package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"time"

	// "github.com/pelletier/go-toml/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	v1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	resourcev1ac "k8s.io/client-go/applyconfigurations/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/typed/apis/v1"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

type profileGame struct {
	Profile string
	Game    string
}

type SessionControllerOptions struct {
	WolfAgentImage           string
	WolfAgentImagePullPolicy string // for debug / local testing / slow internet connections
	LBSharingKey             string
}

type WolfDRAInfo struct {
	NodeName     string
	InternalIP   string
	ExternalIP   string
	AgentPodName string
	AgentPodNS   string
	PoolName     string
}
type NodeIPInfo struct {
	NodeName   string
	InternalIP string
	ExternalIP string
}

// SessionController manages the lifecycle of a streaming session for
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
	SessionInformer generic.Informer[*direwolfv1alpha1.Session]

	AppInformer     generic.Informer[*direwolfv1alpha1.App]
	ProfileInformer generic.Informer[*direwolfv1alpha1.Profile]

	TCPRouteClient gatewayv1.TCPRouteInterface
	UDPRouteClient gatewayv1.UDPRouteInterface

	K8sClient kubernetes.Interface

	trackedSessions map[profileGame]sets.Set[string]
	trackedGames    map[string]profileGame

	controller            generic.Controller[*direwolfv1alpha1.Session]
	statefulSetController generic.Controller[*appsv1.StatefulSet]
	SessionControllerOptions

	LobbyClient v1alpha1client.LobbyInterface
	wolfDRAInfo map[string]WolfDRAInfo // nodeName -> info
}

// NewSessionController creates a new session controller.
func NewSessionController(
	k8sClient kubernetes.Interface,
	tcpRouteClient gatewayv1.TCPRouteInterface,
	udpRouteClient gatewayv1.UDPRouteInterface,
	sessionClient v1alpha1client.SessionInterface,
	lobbyClient v1alpha1client.LobbyInterface,
	sessionInformer generic.Informer[*direwolfv1alpha1.Session],
	appInformer generic.Informer[*direwolfv1alpha1.App],
	profileInformer generic.Informer[*direwolfv1alpha1.Profile],
	statefulSetInformer generic.Informer[*appsv1.StatefulSet],
	options SessionControllerOptions,
) *SessionController {
	res := &SessionController{
		K8sClient:                k8sClient,
		TCPRouteClient:           tcpRouteClient,
		UDPRouteClient:           udpRouteClient,
		SessionClient:            sessionClient,
		LobbyClient:              lobbyClient,
		SessionInformer:          sessionInformer,
		AppInformer:              appInformer,
		ProfileInformer:          profileInformer,
		trackedSessions:          make(map[profileGame]sets.Set[string]),
		trackedGames:             make(map[string]profileGame),
		wolfDRAInfo:              make(map[string]WolfDRAInfo),
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

	//!TODO: Also watch any udproutes, services, statefulsets, etc. that we create
	// and re-reconcile their sessions when they change.
	res.statefulSetController = generic.NewController(
		statefulSetInformer,
		func(_, _ string, newObj *appsv1.StatefulSet) error {
			// Load bearing. If we pass nil it will be casted to interface and
			// not be comparable with nil :)
			if newObj == nil {
				return nil
			}
			return res.reconcileDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "session-controller-statefulset",
			Workers: 2,
		},
	)

	return res
}

func (c *SessionController) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(sessionCtx.Done(), c.SessionInformer.HasSynced) {
		return errors.New("failed to sync session informer")
	}
	// Log DRA agent topology after caches are synced
	c.discoverWolfDRAInfo(sessionCtx)
	// Build initial listing of sessions
	sessions, err := c.SessionInformer.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	for _, session := range sessions {
		pg := profileGame{
			Game:    session.Spec.GameReference.Name,
			Profile: session.Spec.ProfileReference.Name,
		}
		if existing, ok := c.trackedSessions[pg]; ok {
			existing.Insert(session.Name)
		} else {
			c.trackedSessions[pg] = sets.New(session.Name)
		}

		c.trackedGames[session.Name] = pg
	}

	go func() {
		defer cancel()
		err := c.statefulSetController.Run(sessionCtx)
		if err != nil {
			klog.Errorf("Failed to run statefulset controller: %v", err)
		}
	}()

	// Wrap the error from the interface method
	if err := c.controller.Run(sessionCtx); err != nil {
		return fmt.Errorf("failed to run session controller: %w", err)
	}
	return nil
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

	if _, ok := obj.GetLabels()[direwolfv1alpha1.LabelProfile]; !ok {
		return nil
	}

	if _, ok := obj.GetLabels()[direwolfv1alpha1.LabelApp]; !ok {
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

func (c *SessionController) Reconcile(namespace, name string, newObj *direwolfv1alpha1.Session) error {
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
		if err != nil && !kerrors.IsNotFound(err) {
			klog.Errorf("Failed to delete session %s/%s: %v", newObj.Namespace, newObj.Name, err)
			return fmt.Errorf("session wasn't deleted: %w", err)
		}
		return nil
	}
	pg := profileGame{
		Game:    newObj.Spec.GameReference.Name,
		Profile: newObj.Spec.ProfileReference.Name,
	}

	if existing, ok := c.trackedSessions[pg]; ok {
		existing.Insert(newObj.Name)
	} else {
		c.trackedSessions[pg] = sets.New(newObj.Name)
	}
	c.trackedGames[newObj.Name] = pg
	oldStatus := newObj.Status.DeepCopy()
	//TODO: this needs a rewrite
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

	// Service must exist before StatefulSet so ServiceName is populated
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

	if podError := c.reconcileStatefulSet(context.TODO(), newObj); podError != nil {
		klog.Errorf("Failed to reconcile statefulset: %s", podError)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "StatefulSetCreated",
			Status:  metav1.ConditionFalse,
			Reason:  "StatefulSetCreationFailed",
			Message: podError.Error(),
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "StatefulSetCreated",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	// Node placement discovery via ResourceClaim (no pod listing needed)
	if nodeName, err := c.getSessionNodeFromClaim(context.TODO(), newObj); err == nil {
		klog.Infof("Session %s/%s allocated to node %s", namespace, name, nodeName)
		if info, ok := c.wolfDRAInfo[nodeName]; ok {
			klog.Infof("Session %s/%s node IP info: InternalIP=%s ExternalIP=%s",
				namespace, name, info.InternalIP, info.ExternalIP)
		} else {
			klog.V(2).Infof("Session %s/%s node %s not yet in IP cache", namespace, name, nodeName)
		}
	} else {
		klog.V(2).Infof("Could not determine node for session %s/%s from claim: %v", namespace, name, err)
	}
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
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to Update status: %w", err)
		}
	}

	//!TODO: figure our retry logic. Some of these errors surely are retriable
	return nil
}

func (c *SessionController) reconcileService(ctx context.Context, session *direwolfv1alpha1.Session) error {
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return errors.New("waiting for PortsAllocated")
	}

	clampString := func(s string, maxLength int) string {
		if len(s) > maxLength {
			return s[:maxLength]
		}
		return s
	}

	session.Status.ServiceName = clampString(session.Name, 56) + "-rtp"

	// HACK: Delete all direwolf-worker services that dont match the service name
	// This is until we can control the ports in wolf
	allServices, err := c.K8sClient.CoreV1().Services(session.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=direwolf-worker", //nolint
	})

	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	for _, svc := range allServices.Items {
		if svc.Name != session.Status.ServiceName {
			klog.Infof("Deleting service %s/%s", svc.Namespace, svc.Name)
			err := c.K8sClient.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Failed to delete service %s/%s: %s", svc.Namespace, svc.Name, err)
				return fmt.Errorf("failed to delete service %s/%s: %w", svc.Namespace, svc.Name, err)
			}
		}
	}

	// 1. Use the set up a service with correct ports pointing to the pods
	_, err = c.K8sClient.CoreV1().
		Services(session.Namespace).
		Apply(
			ctx,
			v1ac.Service(session.Status.ServiceName, session.Namespace).
				WithAnnotations(map[string]string{
					// Try to support popular service LoadBalancer implementation
					// sharing key annotations.
					"lbipam.cilium.io/sharing-key":        c.LBSharingKey,
					"metallb.universe.tf/allow-shared-ip": c.LBSharingKey,
				}).
				WithLabels(
					map[string]string{
						"app":                         "direwolf-worker", //nolint
						direwolfv1alpha1.LabelApp:     session.Spec.GameReference.Name,
						direwolfv1alpha1.LabelProfile: session.Spec.ProfileReference.Name,
					},
				).
				WithOwnerReferences(metav1ac.OwnerReference().
					WithName(session.Name).
					WithAPIVersion(direwolfv1alpha1.GroupVersion.String()).
					WithKind("Session").
					WithUID(session.UID).
					WithController(true)).
				WithSpec(
					v1ac.ServiceSpec().
						WithType(corev1.ServiceTypeLoadBalancer).
						WithSelector(
							map[string]string{
								direwolfv1alpha1.LabelApp:     session.Spec.GameReference.Name,
								direwolfv1alpha1.LabelProfile: session.Spec.ProfileReference.Name,
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
		return fmt.Errorf("failed to apply service: %w", err)
	}
	return nil
}

func (c *SessionController) reconcileStatefulSet(ctx context.Context, session *direwolfv1alpha1.Session) error {
	//!TODO: Just allocate a ton of ports on the container, we wont be able to
	// change them while its running if another user connects
	if !meta.IsStatusConditionPresentAndEqual(session.Status.Conditions, "PortsAllocated", metav1.ConditionTrue) {
		return errors.New("waiting for PortsAllocated")
	}

	if session.Status.ServiceName == "" {
		return errors.New("waiting for ServiceName")
	}
	claimName, err := c.reconcileResourceClaim(ctx, session)
	if err != nil {
		return fmt.Errorf("resource claim reconciliation failed: %w", err)
	}
	// Get the profile object to access resource policies
	profile, err := c.ProfileInformer.Namespaced(session.Namespace).Get(session.Spec.ProfileReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get profile %s: %w", session.Spec.ProfileReference.Name, err)
	}

	lobbyName := session.Spec.LobbyName
	if lobbyName == "" {
		lobbyName = c.statefulSetName(session)
	}
	lobby, err := c.LobbyClient.Get(ctx, lobbyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get lobby %s for statefulset ownership: %w", lobbyName, err)
	}

	owners := []metav1.OwnerReference{
		{
			APIVersion:         direwolfv1alpha1.GroupVersion.String(),
			Kind:               "Lobby",
			Name:               lobby.Name,
			UID:                lobby.UID,
			Controller:         ptr.To(true),
			BlockOwnerDeletion: ptr.To(true),
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

	// If statefulset already exists, just skip
	statefulSetName := c.statefulSetName(session)

	// Use API server directly, instead of informer cache because it gets stale on rapid session creation / deletion
	existingStatefulSet, err := c.K8sClient.AppsV1().StatefulSets(session.Namespace).Get(ctx, statefulSetName, metav1.GetOptions{})
	if err == nil {
		// StatefulSet is being garbage-collected from previous session; don't try to adopt it
		if existingStatefulSet.DeletionTimestamp != nil {
			return fmt.Errorf("statefulset %s/%s is being deleted, will retry", session.Namespace, statefulSetName)
		}
		klog.Infof("StatefulSet %s/%s already exists, just updating metadata", session.Namespace, statefulSetName)
		if _, err := c.K8sClient.AppsV1().StatefulSets(session.Namespace).Apply(
			ctx,
			appsv1ac.StatefulSet(statefulSetName, session.Namespace).
				WithOwnerReferences(ownerApply...),
			metav1.ApplyOptions{
				FieldManager: "direwolf-session-controller-statefulset-owners",
				Force:        true,
			},
		); err != nil {
			return fmt.Errorf("failed to apply owner references to statefulset %s/%s: %w", session.Namespace, statefulSetName, err)
		}

		return nil
	} else if !kerrors.IsNotFound(err) {
		return fmt.Errorf("failed to check for existing statefulset %s/%s: %w", session.Namespace, statefulSetName, err)
	}
	// Fall through to create statefulset if not found

	// Create pod from pod template
	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}
	// TODO: add teardown timer
	// terminationGracePeriodSeconds?
	// Alternatively, tell moonlight to report session / lobby termination instead of pod termination
	var podToCreate corev1.PodTemplateSpec
	if len(app.Spec.Template.Spec.Containers) > 0 {
		podToCreate.ObjectMeta = app.Spec.Template.ObjectMeta
		podToCreate.Spec = *app.Spec.Template.Spec.DeepCopy()
	}

	if podToCreate.Labels == nil {
		podToCreate.Labels = map[string]string{}
	}

	podToCreate.Labels["app"] = "direwolf-worker" //nolint
	podToCreate.Labels[direwolfv1alpha1.LabelApp] = session.Spec.GameReference.Name
	podToCreate.Labels[direwolfv1alpha1.LabelProfile] = session.Spec.ProfileReference.Name

	// Build VolumeClaimTemplates from app spec with Profile ownership for GC
	var volumeClaimTemplates []corev1.PersistentVolumeClaim
	for _, template := range app.Spec.VolumeClaimTemplates {
		claim := *template.DeepCopy()

		// Apply same defaults as the old reconcilePVC
		if len(claim.Spec.AccessModes) == 0 {
			claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		}
		if claim.Spec.Resources.Requests == nil {
			claim.Spec.Resources.Requests = make(corev1.ResourceList)
		}
		if _, ok := claim.Spec.Resources.Requests[corev1.ResourceStorage]; !ok {
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("5Gi")
		}

		// Profile ownership so PVCs are deleted when profile is deleted
		claim.OwnerReferences = append(claim.OwnerReferences, metav1.OwnerReference{
			APIVersion: direwolfv1alpha1.GroupVersion.String(),
			Kind:       "Profile",
			Name:       profile.Name,
			UID:        profile.UID,
			Controller: new(true),
		})

		volumeClaimTemplates = append(volumeClaimTemplates, claim)
	}
	// Inject DRA ResourceClaim into pod template using the exact v0.36.3 field names
	podToCreate.Spec.ResourceClaims = []corev1.PodResourceClaim{
		{
			Name:              "lobby",
			ResourceClaimName: &claimName,
		},
	}
	// Should this actually be included here?
	for i := range podToCreate.Spec.Containers {
		podToCreate.Spec.Containers[i].Resources.Claims = append(
			podToCreate.Spec.Containers[i].Resources.Claims,
			corev1.ResourceClaim{
				Name: "lobby",
			},
		)
	}
	// Create StatefulSet scaled to 1 for this pod
	statefulSet := appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName,
			Namespace: session.Namespace,
			Labels: map[string]string{
				"app":                         "direwolf-worker", //nolint
				direwolfv1alpha1.LabelApp:     session.Spec.GameReference.Name,
				direwolfv1alpha1.LabelProfile: session.Spec.ProfileReference.Name,
			},
			OwnerReferences: owners,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: session.Status.ServiceName,
			Replicas:    ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					direwolfv1alpha1.LabelApp:     session.Spec.GameReference.Name,
					direwolfv1alpha1.LabelProfile: session.Spec.ProfileReference.Name,
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

	// NOTE: Kinda dumb cuz its just gona get serialized again....
	// could just use dynamic client
	var statefulSetApplyConfig appsv1ac.StatefulSetApplyConfiguration
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredStatefulSet, &statefulSetApplyConfig)
	if err != nil {
		return fmt.Errorf("failed to convert unstructured to statefulset: %w", err)
	}

	_, err = c.K8sClient.AppsV1().StatefulSets(session.Namespace).Apply(
		ctx,
		&statefulSetApplyConfig,
		metav1.ApplyOptions{
			FieldManager: "direwolf-session-controller-statefulset",
		})

	if err != nil {
		return fmt.Errorf("failed to apply statefulset: %w", err)
	}
	// Apply the LobbyName into the session, for the dra to pick it up
	if session.Spec.LobbyName == "" {
		session.Spec.LobbyName = statefulSetName + "-0"
		if _, err := c.SessionClient.Update(ctx, session, metav1.UpdateOptions{
			FieldManager: "direwolf-session-controller",
		}); err != nil {
			return fmt.Errorf("failed to update session with LobbyName: %w", err)
		}
	}
	return nil
}

func (c *SessionController) statefulSetName(session *direwolfv1alpha1.Session) string {
	return fmt.Sprintf("%s-%s", session.Spec.ProfileReference.Name, session.Spec.GameReference.Name)
}

func (c *SessionController) allocatePorts(
	_ context.Context,
	session *direwolfv1alpha1.Session,

) error { //nolint
	// This will be reimplemented during DRA
	//!TODO: Take lock if multiple workers are running

	// 0. Allocate ports for this streaming session to use
	// 1. List all listeners for the gateway
	// 2. List all routes attached to the gateway
	// 3. Subtract used ports
	// 4. Choose a port for RTSP, Enet, Video RTP, Audio RTP

	//!TODO: Implement this properly once wolf lets us assign ports. For now, just
	// hardcode some ports.
	session.Status.Ports = direwolfv1alpha1.SessionPorts{
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
	session *direwolfv1alpha1.Session,
) error {
	statefulSetName := c.statefulSetName(session)

	// !TODO: Use informer for cache reads instead?
	statefulSet, err := c.K8sClient.AppsV1().StatefulSets(session.Namespace).Get(ctx, statefulSetName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get statefulset: %w", err)
	}

	if statefulSet.Status.ObservedGeneration != statefulSet.Generation ||
		statefulSet.Status.ReadyReplicas != *statefulSet.Spec.Replicas {
		return fmt.Errorf("statefulset %s/%s not ready (Observed %d, Latest %d) (%d/%d)",
			session.Namespace, statefulSetName,
			statefulSet.Status.ObservedGeneration, statefulSet.Generation,
			statefulSet.Status.ReadyReplicas, *statefulSet.Spec.Replicas)
	}

	// Get service for the statefulset
	service, err := c.K8sClient.CoreV1().Services(session.Namespace).Get(ctx, session.Status.ServiceName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get service: %w", err)
	}

	// List all the "sessions".
	// Ensure they match each of our k8s sessions. Hash on AESKey/IV
	// In the future it might make sense to just match on ClientID/ClientCertFingerprint
	// but that is hardcoded for now :)
	wolfclient := wolfapi.NewClient("https://"+net.JoinHostPort(service.Spec.ClusterIP, "8443"), &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint
		},
	})

	// Retry wolf-agent calls
	var sessions []wolfapi.Session
	for i := range 5 {
		sessions, err = wolfclient.ListSessions(ctx)
		if err == nil {
			break
		}
		klog.Warningf("wolf-agent list sessions failed (attempt %d/5) for %s/%s: %v", i+1, session.Namespace, session.Name, err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled: %w", ctx.Err())
		case <-time.After(time.Duration(i+1) * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("failed to list sessions: %w", err)
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
		// Delete the session
		if err := c.SessionClient.Delete(ctx, session.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("failed to delete invalid session %s/%s: %w", session.Namespace, session.Name, err)
		}
		return nil
	}

	app, err := c.AppInformer.Namespaced(session.Namespace).Get(session.Spec.GameReference.Name)
	if err != nil {
		return fmt.Errorf("failed to get app: %w", err)
	}

	if !found && app != nil {
		clientIP := "10.128.1.0"
		if session.Spec.Config.ClientIP != "" {
			clientIP = session.Spec.Config.ClientIP
		}
		// This is temporary, since sometimes session creation fails on kind cluster
		var sessionID string
		for i := range 5 {
			sessionID, err = wolfclient.AddSession(ctx, wolfapi.Session{
				VideoWidth:       session.Spec.Config.VideoWidth,
				VideoHeight:      session.Spec.Config.VideoHeight,
				VideoRefreshRate: session.Spec.Config.VideoRefreshRate,
				// AppID:             appID,
				AudioChannelCount: 2,        // !TODO: parse from audio info
				ClientIP:          clientIP, // In the future, this will be acquired dynamically
				// If this isn't present it crashes
				// so, I'll keep it here until I figure out a way to pass off from moonlight client
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
				// ClientID:   "4193251087262667199",
				RTSPFakeIP: service.Spec.ClusterIP,
			})
			if err == nil {
				break
			}
			klog.Warningf("wolf-agent add session failed (attempt %d/5) for %s/%s: %v", i+1, session.Namespace, session.Name, err)
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled: %w", ctx.Err())
			case <-time.After(time.Duration(i+1) * time.Second):
			}
		}
		if err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
		session.Status.WolfSessionID = sessionID
	}
	// else {
	//!TODO: Update wolf API to include session ID in list so we can update
	// these details/validate discrepencies
	// assert wolf session ID non-empty and matches what we expect
	// }

	session.Status.StreamURL = "rtsp://" + net.JoinHostPort(
		service.Spec.ClusterIP,
		strconv.FormatInt(int64(session.Status.Ports.RTSP), 10),
	)

	return nil
}

// discoverWolfDRAInfo lists all wolf.dra.io ResourceSlices, logs node and
// device information, resolves node IPs, and correlates agent pods on that node.
// TODO: add
func (c *SessionController) discoverWolfDRAInfo(ctx context.Context) {
	klog.Info("Discovering wolf.dra.io DRA agents...")

	slices, err := c.K8sClient.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to list ResourceSlices: %v", err)
		return
	}

	agentPods, err := c.K8sClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: "app=wolf-agent",
	})
	if err != nil {
		klog.Warningf("Failed to list wolf-agent pods: %v", err)
	}
	nodeToAgent := make(map[string]corev1.Pod)
	for _, pod := range agentPods.Items {
		if pod.Spec.NodeName != "" {
			nodeToAgent[pod.Spec.NodeName] = pod
		}
	}

	c.wolfDRAInfo = make(map[string]WolfDRAInfo)

	found := false
	for _, slice := range slices.Items {
		if slice.Spec.Driver != "wolf.dra.io" {
			continue
		}
		found = true

		if slice.Spec.NodeName == nil {
			continue
		}
		nodeName := *slice.Spec.NodeName

		klog.Infof("=== Wolf DRA Agent ===")
		klog.Infof("ResourceSlice: %s", slice.Name)
		klog.Infof("Node:          %s", nodeName)
		klog.Infof("Driver:        %s", slice.Spec.Driver)
		klog.Infof("Pool:          %s (gen: %d, slices: %d)",
			slice.Spec.Pool.Name,
			slice.Spec.Pool.Generation,
			slice.Spec.Pool.ResourceSliceCount,
		)

		for i, dev := range slice.Spec.Devices {
			klog.Infof("Device[%d]:    %s", i, dev.Name)
			klog.Infof("  Details:     %+v", dev)
		}

		info := WolfDRAInfo{
			NodeName: nodeName,
			PoolName: slice.Spec.Pool.Name,
		}

		node, err := c.K8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Failed to get node %s: %v", nodeName, err)
		} else {
			for _, addr := range node.Status.Addresses {
				klog.Infof("Node Address [%s]: %s", addr.Type, addr.Address)
				switch addr.Type {
				case corev1.NodeInternalIP:
					info.InternalIP = addr.Address
				case corev1.NodeExternalIP:
					info.ExternalIP = addr.Address
				}
			}
		}

		if agent, ok := nodeToAgent[nodeName]; ok {
			info.AgentPodName = agent.Name
			info.AgentPodNS = agent.Namespace
			klog.Infof("Agent Pod:     %s/%s", agent.Namespace, agent.Name)
			klog.Infof("  Pod IP:      %s", agent.Status.PodIP)
			klog.Infof("  Host IP:     %s", agent.Status.HostIP)
			klog.Infof("  Phase:       %s", agent.Status.Phase)
		}

		c.wolfDRAInfo[nodeName] = info
	}

	if !found {
		klog.Warning("No wolf.dra.io ResourceSlices found in cluster")
	}
}

// getSessionNodeFromClaim finds the ResourceClaim reserved for this session's
// pod and returns the node name from the allocation result (pool) or nodeSelector.
func (c *SessionController) getSessionNodeFromClaim(ctx context.Context, session *direwolfv1alpha1.Session) (string, error) {
	statefulSetName := c.statefulSetName(session)
	podName := statefulSetName + "-0"

	claims, err := c.K8sClient.ResourceV1().ResourceClaims(session.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list resource claims: %w", err)
	}

	for _, claim := range claims.Items {
		for _, ref := range claim.Status.ReservedFor {
			if ref.Resource == "pods" && ref.Name == podName {
				if claim.Status.Allocation != nil {
					// wolf.dra.io uses the node name as the pool name
					for _, res := range claim.Status.Allocation.Devices.Results {
						if res.Driver == "wolf.dra.io" && res.Pool != "" {
							return res.Pool, nil
						}
					}
					// Fallback: nodeSelector matchFields
					if claim.Status.Allocation.NodeSelector != nil {
						for _, term := range claim.Status.Allocation.NodeSelector.NodeSelectorTerms {
							for _, expr := range term.MatchFields {
								if expr.Key == "metadata.name" && len(expr.Values) > 0 {
									return expr.Values[0], nil
								}
							}
						}
					}
				}
				return "", fmt.Errorf("claim %s reserved for pod %s but allocation not ready", claim.Name, podName)
			}
		}
	}

	return "", fmt.Errorf("no resource claim reserved for pod %s", podName)
}

func (c *SessionController) reconcileResourceClaim(ctx context.Context, session *direwolfv1alpha1.Session) (string, error) {
	claimName := c.statefulSetName(session) + "-lobby-claim"

	lobbyName := session.Spec.LobbyName
	if lobbyName == "" {
		lobbyName = c.statefulSetName(session)
	}
	lobby, err := c.LobbyClient.Get(ctx, lobbyName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get lobby %s for resource claim ownership: %w", lobbyName, err)
	}

	channelCount := session.Spec.Config.SurroundAudioFlags
	if channelCount <= 0 {
		channelCount = 2
	}
	// TODO: Make a struct for this in type.go for this
	params := struct {
		VideoSettings  wolfapi.LobbyVideoSettings `json:"video_settings"`
		AudioSettings  wolfapi.LobbyAudioSettings `json:"audio_settings"`
		ClientSettings wolfapi.ClientSettings     `json:"client_settings"`
		MultiUser      bool                       `json:"multi_user"`
	}{
		VideoSettings: wolfapi.LobbyVideoSettings{
			Width:       session.Spec.Config.VideoWidth,
			Height:      session.Spec.Config.VideoHeight,
			RefreshRate: session.Spec.Config.VideoRefreshRate,
		},
		AudioSettings: wolfapi.LobbyAudioSettings{
			ChannelCount: channelCount,
		},
		// TODO: Figure out what to do with this
		ClientSettings: wolfapi.ClientSettings{
			HScrollAcceleration: 1,
			MouseAcceleration:   1,
			RunGID:              1000,
			RunUID:              1000,
			VScrollAcceleration: 1,
		},
		MultiUser: true,
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("marshal opaque params: %w", err)
	}

	_, err = c.K8sClient.ResourceV1().ResourceClaims(session.Namespace).Apply(
		ctx,
		resourcev1ac.ResourceClaim(claimName, session.Namespace).
			WithLabels(map[string]string{
				direwolfv1alpha1.LabelApp:     session.Spec.GameReference.Name,
				direwolfv1alpha1.LabelProfile: session.Spec.ProfileReference.Name,
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
			FieldManager: "direwolf-session-controller",
			Force:        true,
		},
	)
	if err != nil {
		return "", fmt.Errorf("apply resource claim %s: %w", claimName, err)
	}

	klog.Infof("Applied ResourceClaim %s/%s for session %s", session.Namespace, claimName, session.Name)
	return claimName, nil
}
