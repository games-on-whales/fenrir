package controllers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	v1alpha1client "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned/typed/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
)

type profileGame struct {
	Profile string
	Game    string
}

type SessionControllerOptions struct {
	LBSharingKey     string
	PreferInternalIP bool
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
// It is responsible for:
//   - 1. Watching Lobbies and binding Sessions to them (session.spec.lobbyName)
//   - 2. Polling the DRA topology cache to build stream URLs
//   - 3. Updating session status (stream URL, node, conditions)
//   - 4. Cleaning up tracking maps when sessions are deleted
//
// Infrastructure (StatefulSet, ResourceClaim) is owned by the LobbyController.
// TODO: re-implement gateway?
type SessionController struct {
	SessionClient   v1alpha1client.SessionInterface
	SessionInformer generic.Informer[*direwolfv1alpha1.Session]
	LobbyInformer   generic.Informer[*direwolfv1alpha1.Lobby]

	AppInformer     generic.Informer[*direwolfv1alpha1.App]
	ProfileInformer generic.Informer[*direwolfv1alpha1.Profile]

	K8sClient kubernetes.Interface

	trackedSessions map[profileGame]sets.Set[string]
	trackedGames    map[string]profileGame

	controller      generic.Controller[*direwolfv1alpha1.Session]
	lobbyController generic.Controller[*direwolfv1alpha1.Lobby]

	SessionControllerOptions

	wolfDRAInfo   map[string]WolfDRAInfo
	wolfDRAInfoMu sync.RWMutex
}

// NewSessionController creates a new session controller.
func NewSessionController(
	k8sClient kubernetes.Interface,
	sessionClient v1alpha1client.SessionInterface,
	sessionInformer generic.Informer[*direwolfv1alpha1.Session],
	lobbyInformer generic.Informer[*direwolfv1alpha1.Lobby],
	appInformer generic.Informer[*direwolfv1alpha1.App],
	profileInformer generic.Informer[*direwolfv1alpha1.Profile],
	options SessionControllerOptions,
) *SessionController {
	res := &SessionController{
		K8sClient:                k8sClient,
		SessionClient:            sessionClient,
		SessionInformer:          sessionInformer,
		LobbyInformer:            lobbyInformer,
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

	res.lobbyController = generic.NewController(
		lobbyInformer,
		func(_, _ string, newObj *direwolfv1alpha1.Lobby) error {
			if newObj == nil {
				return nil
			}
			return res.reconcileLobbyDependant(newObj)
		},
		generic.ControllerOptions{
			Name:    "session-controller-lobby",
			Workers: 2,
		},
	)

	return res
}

func (c *SessionController) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if !cache.WaitForCacheSync(sessionCtx.Done(), c.SessionInformer.HasSynced, c.LobbyInformer.HasSynced) {
		return errors.New("failed to sync informers")
	}

	c.discoverWolfDRAInfo(sessionCtx)
	// Start background refresh of DRA agent topology
	// This should will replaced with a wolf node crd once the initial implementation of "wolf multi-node" is underway
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
		err := c.lobbyController.Run(sessionCtx)
		if err != nil {
			klog.Errorf("Failed to run lobby dependant controller: %v", err)
		}
	}()

	if err := c.controller.Run(sessionCtx); err != nil {
		return fmt.Errorf("failed to run session controller: %w", err)
	}
	return nil
}

func (c *SessionController) HasSynced() bool {
	return c.SessionInformer.HasSynced()
}

func (c *SessionController) reconcileLobbyDependant(lobby *direwolfv1alpha1.Lobby) error {
	if lobby == nil {
		return nil
	}
	sessions, err := c.SessionInformer.List(labels.SelectorFromSet(labels.Set{
		direwolfv1alpha1.LabelApp:     lobby.Labels[direwolfv1alpha1.LabelApp],
		direwolfv1alpha1.LabelProfile: lobby.Labels[direwolfv1alpha1.LabelProfile],
	}))
	if err != nil {
		return fmt.Errorf("failed to list sessions for lobby %s/%s: %w", lobby.Namespace, lobby.Name, err)
	}
	for _, session := range sessions {
		klog.V(2).Infof("Enqueuing session %s/%s because lobby %s/%s changed",
			session.Namespace, session.Name, lobby.Namespace, lobby.Name)
		c.controller.Enqueue(session.Namespace, session.Name)
	}
	return nil
}

func (c *SessionController) findLobbyForSession(session *direwolfv1alpha1.Session) (*direwolfv1alpha1.Lobby, error) {
	lobbies, err := c.LobbyInformer.List(labels.SelectorFromSet(labels.Set{
		direwolfv1alpha1.LabelApp:     session.Spec.GameReference.Name,
		direwolfv1alpha1.LabelProfile: session.Spec.ProfileReference.Name,
	}))
	if err != nil {
		return nil, fmt.Errorf("failed to list lobbies: %w", err)
	}
	if len(lobbies) == 0 {
		return nil, fmt.Errorf("no lobby found for app=%s profile=%s",
			session.Spec.GameReference.Name, session.Spec.ProfileReference.Name)
	}
	// There should be exactly one lobby per app+profile. If multiple exist, pick the first.
	return lobbies[0], nil
}

func (c *SessionController) Reconcile(namespace, name string, newObj *direwolfv1alpha1.Session) error {
	klog.Infof("Reconciling session %s/%s", namespace, name)
	defer klog.Infof("Finished Reconciling session %s/%s", namespace, name)

	if newObj == nil {
		// Session was deleted. Infrastructure is owned by Lobby; tracking maps only.
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

	// 1. Find the lobby for this session
	lobby, err := c.findLobbyForSession(newObj)
	if err != nil {
		klog.V(2).Infof("Session %s/%s waiting for lobby: %v", namespace, name, err)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "LobbyReady",
			Status:  metav1.ConditionFalse,
			Reason:  "LobbyNotFound",
			Message: err.Error(),
		})
		if !reflect.DeepEqual(newObj.Status, oldStatus) {
			if _, updateErr := c.SessionClient.UpdateStatus(context.TODO(), newObj, metav1.UpdateOptions{
				FieldManager: "session-controller-status",
			}); updateErr != nil && !kerrors.IsNotFound(updateErr) {
				return fmt.Errorf("failed to update status: %w", updateErr)
			}
		}
		return nil
	}

	// 2. Bind session to lobby pod name (wolf-agent DRA watches this field)
	if newObj.Spec.LobbyName == "" {
		if lobby.Status.PodName == "" {
			klog.V(2).Infof("Session %s/%s waiting for lobby pod name", namespace, name)
			return nil
		}
		newObj.Spec.LobbyName = lobby.Status.PodName
		_, err := c.SessionClient.Update(context.TODO(), newObj, metav1.UpdateOptions{
			FieldManager: "direwolf-session-controller",
		})
		if err != nil {
			return fmt.Errorf("failed to update session with LobbyName: %w", err)
		}
		// Process the updated session on the next reconcile.
		return nil
	}

	// 3. Check lobby readiness
	if !lobby.Status.StatefulSetReady {
		klog.V(2).Infof("Session %s/%s waiting for lobby to be ready", namespace, name)
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:    "LobbyReady",
			Status:  metav1.ConditionFalse,
			Reason:  "LobbyNotReady",
			Message: "Lobby infrastructure is not yet ready",
		})
	} else {
		meta.SetStatusCondition(&newObj.Status.Conditions, metav1.Condition{
			Type:   "LobbyReady",
			Status: metav1.ConditionTrue,
			Reason: "Success",
		})
	}

	// 4. Node placement from lobby status (single source of truth across all claims)
	nodeName := lobby.Status.NodeName
	if nodeName != "" {
		newObj.Status.NodeName = nodeName
	}

	// 5. Stream URL — gated on lobby readiness AND WolfSessionID
	klog.Infof("Session %s/%s: beginning stream URL setup", namespace, name)

	streamReady := false
	if lobby.Status.StatefulSetReady && newObj.Status.WolfSessionID != "" && nodeName != "" {
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
		} else {
			klog.V(2).Infof("Session %s/%s node %s not yet in IP cache", namespace, name, nodeName)
		}
	}

	newObj.Status.StreamStarted = streamReady
	klog.Infof("Session %s/%s: stream ready=%v", namespace, name, streamReady)

	// 6. Write status if anything changed
	if !reflect.DeepEqual(newObj.Status, oldStatus) {
		_, err := c.SessionClient.UpdateStatus(
			context.TODO(),
			newObj,
			metav1.UpdateOptions{
				FieldManager: "session-controller-status",
			},
		)
		if err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("failed to Update status: %w", err)
		}
	}

	return nil
}

// discoverWolfDRAInfo reads wolf.dra.io ResourceSlices and pulls node
// identifying information directly from the device attributes.
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
