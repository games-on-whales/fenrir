package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
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
	PreferInternalIP         bool
}

type WolfDRAInfo struct {
	NodeName     string
	InternalIP   string
	ExternalIP   string
	AgentPodName string
	AgentPodNS   string
	PoolName     string
}

// SessionController manages the lifecycle of a streaming session for
// a given game, of a given user.
// If is responsible for:
//   - 1. Setting up port forwards via Gateway API
//   - 2. Setting up pods, etc. for session
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

	LobbyClient   v1alpha1client.LobbyInterface
	wolfDRAInfo   map[string]WolfDRAInfo // nodeName -> info
	wolfDRAInfoMu sync.RWMutex
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

	//!TODO: Also watch any udproutes, statefulsets, etc. that we create
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
	// Start background refresh of DRA agent topology
	// This should be replaced with a wolf node crd or something
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				c.discoverWolfDRAInfo(sessionCtx)
			}
		}
	}()
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

	// First: try direct Session ownership via ownerReferences
	enqueued := false
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Kind == "Session" {
			klog.Infof("Dependant %s/%s owned by Session %s, enqueuing",
				obj.GetNamespace(), obj.GetName(), owner.Name)
			c.controller.Enqueue(obj.GetNamespace(), owner.Name)
			enqueued = true
		}
	}
	if enqueued {
		return nil
	}

	// Second: the dependant is owned by something else (e.g. Lobby).
	// Find all Sessions in this namespace with matching app+profile labels.
	klog.Infof("Dependant %s/%s (app=%s, profile=%s) not owned by Session, looking up matching sessions",
		obj.GetNamespace(), obj.GetName(), appName, profileName)

	sessions, err := c.SessionInformer.List(labels.SelectorFromSet(labels.Set{
		direwolfv1alpha1.LabelApp:     appName,
		direwolfv1alpha1.LabelProfile: profileName,
	}))
	if err != nil {
		klog.Errorf("Failed to list sessions for dependant %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		return fmt.Errorf("failed to list sessions: %w", err)
	}

	if len(sessions) == 0 {
		klog.V(2).Infof("No sessions found matching app=%s profile=%s for dependant %s/%s",
			appName, profileName, obj.GetNamespace(), obj.GetName())
		return nil
	}

	for _, session := range sessions {
		klog.Infof("Enqueuing session %s/%s because dependant %s/%s changed",
			session.Namespace, session.Name, obj.GetNamespace(), obj.GetName())
		c.controller.Enqueue(session.Namespace, session.Name)
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

	// 2. StatefulSet
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

	// 3. Node placement discovery via ResourceClaim (no pod listing needed)
	if nodeName, err := c.getSessionNodeFromClaim(context.TODO(), newObj); err == nil {
		klog.Infof("Session %s/%s allocated to node %s", namespace, name, nodeName)
		newObj.Status.NodeName = nodeName
		c.wolfDRAInfoMu.RLock()
		info, ok := c.wolfDRAInfo[nodeName]
		c.wolfDRAInfoMu.RUnlock()
		if ok {
			klog.Infof("Session %s/%s node IP info: InternalIP=%s ExternalIP=%s",
				namespace, name, info.InternalIP, info.ExternalIP)
		} else {
			klog.V(2).Infof("Session %s/%s node %s not yet in IP cache", namespace, name, nodeName)
		}
	} else {
		klog.V(2).Infof("Could not determine node for session %s/%s from claim: %v", namespace, name, err)
	}

	// 4. Stream URL — gated on both StatefulSet readiness AND WolfSessionID
	klog.Infof("Session %s/%s: beginning stream URL setup", namespace, name)

	statefulSetName := c.statefulSetName(newObj)
	statefulSet, err := c.K8sClient.AppsV1().StatefulSets(newObj.Namespace).Get(context.TODO(), statefulSetName, metav1.GetOptions{})

	streamReady := false

	if err == nil &&
		statefulSet.Status.ObservedGeneration == statefulSet.Generation &&
		statefulSet.Status.ReadyReplicas == *statefulSet.Spec.Replicas &&
		newObj.Status.WolfSessionID != "" {

		nodeName := newObj.Status.NodeName
		if nodeName == "" {
			if nodeName, err = c.getSessionNodeFromClaim(context.TODO(), newObj); err == nil {
				newObj.Status.NodeName = nodeName
			}
		}

		if nodeName != "" {
			c.wolfDRAInfoMu.RLock()
			info, ok := c.wolfDRAInfo[nodeName]
			c.wolfDRAInfoMu.RUnlock()

			if ok {
				streamIP := info.InternalIP
				if streamIP == "" {
					streamIP = info.ExternalIP
				}
				if streamIP != "" {
					newObj.Status.StreamURL = "rtsp://" + net.JoinHostPort(
						streamIP,
						strconv.FormatInt(int64(newObj.Status.Ports.RTSP), 10),
					)
					streamReady = true
				}
			}
		}
	}

	newObj.Status.StreamStarted = streamReady
	klog.Infof("Session %s/%s: stream ready=%v", namespace, name, streamReady)

	// 5. Write status if anything changed
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
func (c *SessionController) reconcileStatefulSet(ctx context.Context, session *direwolfv1alpha1.Session) error {
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
	// the default termination period for a statefulset pod is 30s
	// so, the moonlight client will just throw an error before the pod disappears
	// and the app updates
	// I have no idea what this will cause.
	podToCreate.Spec.TerminationGracePeriodSeconds = ptr.To[int64](3)
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
			// ServiceName: session.Status.ServiceName,
			Replicas: ptr.To[int32](1),
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

// discoverWolfDRAInfo reads wolf.dra.io ResourceSlices and pulls node
// identifying information (IPs, agent pod reference) directly from the
// device attributes published by the agent. No Node or Pod API lookups
// are needed.
func (c *SessionController) discoverWolfDRAInfo(ctx context.Context) {
	klog.V(2).Info("Refreshing wolf.dra.io DRA agent info from ResourceSlices...")

	slices, err := c.K8sClient.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("Failed to list ResourceSlices: %v", err)
		return
	}

	newInfo := make(map[string]WolfDRAInfo)
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

		info := WolfDRAInfo{
			NodeName: nodeName,
			PoolName: slice.Spec.Pool.Name,
		}

		for _, dev := range slice.Spec.Devices {
			for qname, attr := range dev.Attributes {
				if attr.StringValue == nil {
					continue
				}
				val := *attr.StringValue
				switch string(qname) {
				case "wolf.dra.io/nodeInternalIP":
					info.InternalIP = val
				case "wolf.dra.io/nodeExternalIP":
					info.ExternalIP = val
				case "wolf.dra.io/agentPodName":
					info.AgentPodName = val
				case "wolf.dra.io/agentPodNamespace":
					info.AgentPodNS = val
				}
			}
		}

		klog.V(2).Infof("Wolf DRA agent on %s: internalIP=%s externalIP=%s agent=%s/%s pool=%s",
			nodeName, info.InternalIP, info.ExternalIP, info.AgentPodNS, info.AgentPodName, info.PoolName)

		newInfo[nodeName] = info
	}

	if !found {
		klog.Warning("No wolf.dra.io ResourceSlices found in cluster")
	}

	c.wolfDRAInfoMu.Lock()
	c.wolfDRAInfo = newInfo
	c.wolfDRAInfoMu.Unlock()
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
	// TODO: Figure out how to use this for node selection?
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

	// TODO: Figure out channel count and audio / video streaming
	channelCount := 2 //session.Spec.Config.SurroundAudioFlags
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
