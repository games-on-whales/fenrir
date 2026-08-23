package dra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	direwolf "games-on-whales.github.io/direwolf/pkg/generated/clientset/versioned"
	v1alpha1lister "games-on-whales.github.io/direwolf/pkg/generated/listers/api/v1alpha1"
	wolfapi "games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// Driver implements kubeletplugin.DRAPlugin.
type Driver struct {
	driverName   string
	nodeName     string
	socketsDir   string
	wolfSockPath string
	kubeClient   kubernetes.Interface

	state     *State
	allocator *Allocator
	cdiGen    *CDIGenerator

	wolfClient wolfapi.Client

	socketMu         sync.Mutex // protects socket allocation + lobby session ops
	nodeIPMu         sync.Mutex
	cachedInternalIP string
	cachedExternalIP string
	queueTimeout     time.Duration
	extraEnv         map[string]string

	cancelCtx context.CancelCauseFunc

	direwolfClient   direwolf.Interface
	sessionInformer  cache.SharedIndexInformer
	sessionLister    v1alpha1lister.SessionLister
	sessionWorkqueue workqueue.TypedRateLimitingInterface[string]

	// For when HDR is ready and we can just create a session with it's gst pipeline params
	apps         []wolfapi.App
	defaultAppID string
}

func NewDriver(
	driverName, nodeName, socketsDir, wolfSockPath, cdiDir string,
	maxSockets int,
	queueTimeout time.Duration,
	extraEnv map[string]string,
	kubeClient kubernetes.Interface,
	direwolfClient direwolf.Interface,
) (*Driver, error) {
	if cdiDir == "" {
		cdiDir = "/var/run/cdi"
	}

	wolfClient := wolfapi.NewClient(
		"http://wolf.sock",
		&http.Client{
			Timeout: 1 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", wolfSockPath)
				},
			},
		},
	)

	d := &Driver{
		driverName:     driverName,
		nodeName:       nodeName,
		socketsDir:     socketsDir,
		wolfSockPath:   wolfSockPath,
		kubeClient:     kubeClient,
		direwolfClient: direwolfClient,
		state:          NewState(),
		allocator:      NewAllocator(maxSockets),
		cdiGen:         NewCDIGenerator(driverName, cdiDir, socketsDir),
		wolfClient:     wolfClient,
		queueTimeout:   queueTimeout,
		extraEnv:       extraEnv,
	}

	d.allocator.SyncFromState(d.state)
	if apps, err := wolfClient.ListApps(context.Background()); err == nil && len(apps) > 0 {
		d.apps = apps
		d.defaultAppID = apps[0].ID
	}
	return d, nil
}

func (d *Driver) SetCancelFunc(fn context.CancelCauseFunc) {
	d.cancelCtx = fn
}

// SetSessionInformer wires the Session CRD informer into the driver.
// Called from main after the informer factory has been initialised.
func (d *Driver) SetSessionInformer(
	informer cache.SharedIndexInformer,
	lister v1alpha1lister.SessionLister,
	queue workqueue.TypedRateLimitingInterface[string],
) {
	d.sessionInformer = informer
	d.sessionLister = lister
	d.sessionWorkqueue = queue
}

// ReconcileWithWolf rebuilds in-memory state by comparing existing CDI
// files on disk with Wolf's active lobbies. This is called at startup
// to recover from driver crashes and prevent dropping active streams.
func (d *Driver) ReconcileWithWolf(ctx context.Context) {
	cdiClaims := d.cdiGen.ListLobbySpecs()

	lobbies, err := d.wolfClient.ListLobbies(ctx)
	if err != nil {
		klog.Warningf("ListLobbies failed during reconciliation: %v. Restoring from CDI files only.", err)
		for uid, st := range cdiClaims {
			if _, exists := d.state.Get(uid); !exists {
				klog.Infof("Recovered state from CDI for claim %s (Wolf unreachable)", uid)
				d.restoreClaimState(uid, st)
			}
		}
		return
	}

	wolfLobbies := make(map[string]wolfapi.Lobby)
	for _, l := range lobbies {
		wolfLobbies[l.ID] = l
	}

	for uid, st := range cdiClaims {
		if _, ok := wolfLobbies[st.LobbyID]; ok {
			if _, exists := d.state.Get(uid); !exists {
				klog.Infof("Recovered active claim %s (lobby %s, wayland-%d)", uid, st.LobbyID, st.WaylandIndex)
				d.restoreClaimState(uid, st)
			}
		} else {
			d.cleanupDeadClaim(uid, st)
		}
	}
}

func (d *Driver) restoreClaimState(uid string, st *WolfResourceState) {
	d.state.Set(uid, st)
	d.allocator.MarkUsed(st.WaylandIndex)
}

func (d *Driver) cleanupDeadClaim(uid string, st *WolfResourceState) {
	klog.Infof("Cleaning up dead claim %s (lobby %s no longer in Wolf)", uid, st.LobbyID)
	deadSockFile := filepath.Join(d.socketsDir, st.WaylandSocketName)
	if err := os.Remove(deadSockFile); err != nil {
		klog.Infof("cleaning up %s failed", deadSockFile)
	}
	if err := d.cdiGen.DeleteCDISpecs(uid); err != nil {
		klog.Infof("cleaning up CDI specs for %s failed", uid)
	}
}

func (d *Driver) PrepareResourceClaims(
	ctx context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.InfoS("PrepareResourceClaims", "count", len(claims))

	results := make(map[types.UID]kubeletplugin.PrepareResult)
	for _, claim := range claims {
		results[claim.UID] = d.prepareResourceClaim(ctx, claim)
	}
	return results, nil
}

func (d *Driver) prepareResourceClaim(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
) kubeletplugin.PrepareResult {
	uid := claim.UID
	uidStr := string(uid)
	klog.InfoS("Preparing claim", "uid", uid, "ns", claim.Namespace, "name", claim.Name)

	if st, ok := d.state.Get(uidStr); ok {
		if socketExists(d.socketsDir, st.WaylandSocketName) {
			klog.V(2).InfoS("Claim already prepared, returning existing CDI device",
				"uid", uid, "waylandIndex", st.WaylandIndex)
			return kubeletplugin.PrepareResult{
				Devices: []kubeletplugin.Device{
					{
						PoolName:     d.nodeName,
						DeviceName:   "lobby-pool",
						CDIDeviceIDs: []string{d.cdiGen.DeviceID(uidStr)},
					},
				},
			}
		}
		klog.InfoS("Wayland socket no longer exists, cleaning up stale state",
			"uid", uid, "socket", st.WaylandSocketName)
		_ = d.wolfClient.StopLobby(ctx, wolfapi.StopLobbyRequest{LobbyID: st.LobbyID})
		d.allocator.Release(st.WaylandIndex)
		d.state.Delete(uidStr)
	}

	if !d.allocator.Available() {
		klog.InfoS("No wayland sockets available", "uid", uid)
		return kubeletplugin.PrepareResult{Err: errors.New("no wayland sockets available")}
	}
	className := getDeviceClassName(claim)
	var class *resourceapi.DeviceClass
	if className != "" {
		var err error
		class, err = d.kubeClient.ResourceV1().DeviceClasses().Get(ctx, className, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Failed to get DeviceClass %q: %v", className, err)
			// Continue without class config; claim-level config will still be used.
		}
	}

	params, err := ParseClaimParams(claim, class, d.driverName)
	if err != nil {
		klog.ErrorS(err, "Failed to parse claim params", "uid", uid, "name", claim.Name)
		return kubeletplugin.PrepareResult{Err: err}
	}
	// we create a lock to monitor the wayland socket creation inside of the directory
	// not the best method, but it'll do for now
	d.socketMu.Lock()
	defer d.socketMu.Unlock()

	if !d.allocator.Available() {
		klog.InfoS("No wayland sockets available (after lock)", "uid", uid)
		return kubeletplugin.PrepareResult{Err: errors.New("no wayland sockets available")}
	}
	podName := ""
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource == "pods" {
			podName = ref.Name
			break
		}
	}
	if podName == "" {
		klog.Warningf("Claim %s/%s has no pod reservation yet, using claim name as fallback", claim.Namespace, claim.Name)
		podName = claim.Name
	}
	lobbyName := claim.Namespace + "/" + podName
	req := wolfapi.LobbyCreateRequest{
		ProfileID:              "default",
		Name:                   lobbyName,
		StopWhenEveryoneLeaves: false,
		ClientSettings:         params.ClientSettings,
		PinRequired:            params.PinRequired,
		Pin:                    params.Pin,
		MultiUser:              params.MultiUser,
		Runner: wolfapi.Runner{
			Type:   "process",
			RunCmd: "sleep inf",
		},
		VideoSettings: params.VideoSettings,
		AudioSettings: params.AudioSettings,
	}

	klog.V(2).InfoS("Creating lobby",
		"claimUID", uid,
		"width", params.VideoSettings.Width,
		"height", params.VideoSettings.Height,
		"refresh", params.VideoSettings.RefreshRate,
		"renderNode", params.VideoSettings.WaylandRenderNode)

	// Take a snapshot of existing sockets and their ModTimes right before
	// calling CreateLobby. This allows us to reliably identify the new socket,
	// even if Wolf reuses an index from a recently deleted zombie socket.
	beforeSnapshot := d.snapshotSockets()

	resp, err := d.wolfClient.CreateLobby(ctx, req)
	if err != nil {
		klog.ErrorS(err, "CreateLobby failed", "claimUID", uid, "Wolf Response: ", resp)
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("create lobby: %w", err)}
	}

	lobbyID := resp.LobbyID
	if lobbyID == "" {
		klog.ErrorS(nil, "CreateLobby returned empty lobby ID", "claimUID", uid)
		return kubeletplugin.PrepareResult{Err: errors.New("create lobby returned empty ID")}
	}
	klog.V(2).InfoS("Lobby created", "lobbyID", lobbyID, "claimUID", uid)

	idx, sockName, err := d.discoverNewWaylandSocket(ctx, beforeSnapshot)
	if err != nil {
		klog.ErrorS(err, "Wayland socket discovery failed", "lobbyID", lobbyID)
		_ = d.wolfClient.StopLobby(ctx, wolfapi.StopLobbyRequest{LobbyID: lobbyID})
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("discover socket: %w", err)}
	}

	d.allocator.MarkUsed(idx)
	wolfState := &WolfResourceState{
		ClaimUID:          uidStr,
		ClaimName:         claim.Name,
		ClaimNamespace:    claim.Namespace,
		LobbyID:           lobbyID,
		LobbyName:         lobbyName,
		WaylandIndex:      idx,
		WaylandSocketName: sockName,
		CreatedAt:         time.Now(),
	}
	d.state.Set(uidStr, wolfState)

	go d.processPendingSessionsForLobby(context.WithoutCancel(ctx), lobbyName, uidStr)
	cdiID, err := d.cdiGen.GenerateLobbyCDI(wolfState, idx, params.VideoSettings, d.extraEnv)
	if err != nil {
		klog.ErrorS(err, "CDI generation failed", "claimUID", uid)
		_ = d.wolfClient.StopLobby(ctx, wolfapi.StopLobbyRequest{LobbyID: lobbyID})
		d.allocator.Release(idx)
		d.state.Delete(uidStr)
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("cdi spec: %w", err)}
	}

	klog.InfoS("Claim prepared", "claimUID", uid, "waylandIndex", idx, "cdi", cdiID)

	// update status with the lobby id
	if err := d.patchDeviceStatus(ctx, claim, lobbyID); err != nil {
		klog.ErrorS(err, "Failed to patch claim device status", "claimUID", uid)
	}

	return kubeletplugin.PrepareResult{
		Devices: []kubeletplugin.Device{
			{
				PoolName:     d.nodeName,
				DeviceName:   "lobby-pool",
				CDIDeviceIDs: []string{cdiID},
			},
		},
	}
}

// UnprepareResourceClaims removes the lobby wayland socket and all associated sessions from the pod
func (d *Driver) UnprepareResourceClaims(
	ctx context.Context,
	claims []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	klog.InfoS("UnprepareResourceClaims", "count", len(claims))
	results := make(map[types.UID]error)

	for _, claim := range claims {
		uid := string(claim.UID)
		klog.InfoS("Unpreparing claim", "uid", uid, "ns", claim.Namespace, "name", claim.Name)

		// Collect every Wolf session ID that must be stopped.
		sessionIDs := make(map[string]struct{})
		// Sessions tracked in local state.
		localSessions := d.state.GetSessionsForClaim(uid)
		for _, ss := range localSessions {
			if ss.WolfSessionID != "" {
				sessionIDs[ss.WolfSessionID] = struct{}{}
			}
		}

		// 3. Stop every session before tearing down the lobby.
		for sid := range sessionIDs {
			if err := d.wolfClient.StopSession(ctx, sid); err != nil {
				klog.ErrorS(err, "StopSession failed during unprepare", "sessionID", sid, "claimUID", uid)
			} else {
				klog.V(2).InfoS("Stopped session during unprepare", "sessionID", sid, "claimUID", uid)
			}
		}

		// 4. Clean up local session state and release their wayland indices.
		for _, ss := range localSessions {
			d.state.DeleteSession(ss.SessionUID)
			d.allocator.Release(ss.WaylandIndex)
		}

		if err := d.cdiGen.DeleteCDISpecs(uid); err != nil {
			klog.ErrorS(err, "Failed to delete CDI specs", "uid", uid)
		}

		st, ok := d.state.Get(uid)
		if !ok {
			klog.V(2).InfoS("Claim not in state, nothing more to unprepare", "uid", uid)
			results[claim.UID] = nil
			continue
		}

		if err := d.wolfClient.StopLobby(ctx, wolfapi.StopLobbyRequest{LobbyID: st.LobbyID}); err != nil {
			klog.ErrorS(err, "StopLobby failed, cleaning up local state anyway",
				"lobbyID", st.LobbyID)
		}

		d.allocator.Release(st.WaylandIndex)
		d.state.Delete(uid)
		results[claim.UID] = nil
		klog.InfoS("Claim unprepared", "uid", uid)
	}

	return results, nil
}

func (d *Driver) HandleError(_ context.Context, err error, msg string) {
	klog.ErrorS(err, "DRA plugin error", "msg", msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) {
		klog.ErrorS(err, "Fatal DRA plugin error, initiating shutdown")
		if d.cancelCtx != nil {
			d.cancelCtx(fmt.Errorf("fatal background error: %w", err))
		}
	}
}

// snapshotSockets records the modification times of existing wayland sockets.
func (d *Driver) snapshotSockets() map[int]time.Time {
	snapshot := make(map[int]time.Time)
	entries, _ := os.ReadDir(d.socketsDir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "wayland-") {
			continue
		}
		idxStr := strings.TrimPrefix(e.Name(), "wayland-")
		if idx, err := strconv.Atoi(idxStr); err == nil {
			if info, err := e.Info(); err == nil {
				snapshot[idx] = info.ModTime()
			}
		}
	}
	return snapshot
}

// discoverNewWaylandSocket looks for a wayland-N file that is either brand new
// or has a ModTime newer than the snapshot taken before CreateLobby.
// this will be removed in the future after wolf returns wayland socket information in the sse
func (d *Driver) discoverNewWaylandSocket(
	ctx context.Context,
	before map[int]time.Time,
) (idx int, socketName string, err error) {
	timeout := time.After(2 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	known := d.allocator.Used()

	for {
		select {
		case <-ctx.Done():
			return 0, "", fmt.Errorf("context cancelled: %w", ctx.Err())
		case <-timeout:
			return 0, "", errors.New("timeout waiting for wayland socket")
		case <-tick.C:
			entries, _ := os.ReadDir(d.socketsDir)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), "wayland-") {
					continue
				}
				idxStr := strings.TrimPrefix(e.Name(), "wayland-")
				idx, err := strconv.Atoi(idxStr)
				if err != nil {
					continue
				}

				if _, used := known[idx]; used {
					continue
				}

				info, err := e.Info()
				if err != nil {
					continue
				}

				if info.Mode()&os.ModeSocket == 0 {
					continue
				}

				oldModTime, existed := before[idx]
				if !existed || info.ModTime().After(oldModTime) {
					return idx, e.Name(), nil
				}
			}
		}
	}
}
func socketExists(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func getDeviceClassName(claim *resourceapi.ResourceClaim) string {
	for _, req := range claim.Spec.Devices.Requests {
		if req.Exactly != nil && req.Exactly.DeviceClassName != "" {
			return req.Exactly.DeviceClassName
		}
	}
	return ""
}

// deviceStatusData is the shape we store in status.devices[].data.
type deviceStatusData struct {
	LobbyID string `json:"lobby_id"`
	// Sessions []SessionStatusInfo `json:"sessions"`
}

// findOurAllocation returns this driver's allocation result, if any.
func (d *Driver) findOurAllocation(claim *resourceapi.ResourceClaim) *resourceapi.DeviceRequestAllocationResult {
	if claim.Status.Allocation == nil {
		return nil
	}
	for i := range claim.Status.Allocation.Devices.Results {
		if r := &claim.Status.Allocation.Devices.Results[i]; r.Driver == d.driverName {
			return r
		}
	}
	return nil
}

// patchDeviceStatus updates status.devices with driver-specific data.
func (d *Driver) patchDeviceStatus(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	lobbyID string,
) error {
	r := d.findOurAllocation(claim)
	if r == nil {
		return nil
	}

	raw, err := json.Marshal(deviceStatusData{
		LobbyID: lobbyID,
	})
	if err != nil {
		return fmt.Errorf("marshal device status data: %w", err)
	}

	status := resourceapi.AllocatedDeviceStatus{
		Driver: r.Driver,
		Pool:   r.Pool,
		Device: r.Device,
		Data:   &runtime.RawExtension{Raw: raw},
	}
	if r.ShareID != nil {
		s := string(*r.ShareID)
		status.ShareID = &s
	}

	patch := struct {
		Status struct {
			Devices []resourceapi.AllocatedDeviceStatus `json:"devices"`
		} `json:"status"`
	}{
		Status: struct {
			Devices []resourceapi.AllocatedDeviceStatus `json:"devices"`
		}{
			Devices: []resourceapi.AllocatedDeviceStatus{status},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}

	_, err = d.kubeClient.ResourceV1().ResourceClaims(claim.Namespace).Patch(
		ctx, claim.Name, types.MergePatchType, patchBytes,
		metav1.PatchOptions{}, "status",
	)
	if err != nil {
		return fmt.Errorf("failed to patch lobby id into resourceClaim status: %w", err)
	}
	return nil
}

// Session CRD event handlers

// GetNodeIPs returns this node's InternalIP and ExternalIP, cached after
// the first lookup. Exposed so the agent can publish them on the
// ResourceSlice and the session controller can read them from there.
func (d *Driver) GetNodeIPs(ctx context.Context) (internal, external string) {
	d.nodeIPMu.Lock()
	defer d.nodeIPMu.Unlock()

	if d.cachedInternalIP == "" && d.cachedExternalIP == "" {
		node, err := d.kubeClient.CoreV1().Nodes().Get(ctx, d.nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Warningf("Failed to get node %s for IP discovery: %v", d.nodeName, err)
			return "", ""
		}
		for _, addr := range node.Status.Addresses {
			switch addr.Type {
			case corev1.NodeInternalIP:
				d.cachedInternalIP = addr.Address
			case corev1.NodeExternalIP:
				d.cachedExternalIP = addr.Address
			}
		}
	}
	return d.cachedInternalIP, d.cachedExternalIP
}

// buildWolfSession converts the Session CRD config into a wolfapi.Session.
// TODO: ClientSettings & AudioChannelCount should come from the Profile and Session respectively.
func (d *Driver) buildWolfSession(session *direwolfv1alpha1.Session, rtspFakeIP string) wolfapi.Session {
	sess := wolfapi.Session{
		ClientIP:          session.Spec.Config.ClientIP,
		AESKey:            session.Spec.Config.AESKey,
		AESIV:             session.Spec.Config.AESIV,
		VideoWidth:        session.Spec.Config.VideoWidth,
		VideoHeight:       session.Spec.Config.VideoHeight,
		VideoRefreshRate:  session.Spec.Config.VideoRefreshRate,
		RTSPFakeIP:        rtspFakeIP,
		AudioChannelCount: 2,
		ClientSettings: wolfapi.ClientSettings{
			ControllersOverride:      session.Spec.Config.ClientSettings.ControllersOverride,
			MotionControllerOverride: session.Spec.Config.ClientSettings.MotionControllerOverride,
			MouseAcceleration:        session.Spec.Config.ClientSettings.MouseAcceleration,
			HScrollAcceleration:      session.Spec.Config.ClientSettings.HScrollAcceleration,
			VScrollAcceleration:      session.Spec.Config.ClientSettings.VScrollAcceleration,
			RunGID:                   1000,
			RunUID:                   1000,
		},
	}
	if d.defaultAppID != "" {
		sess.AppID = d.defaultAppID
	}
	return sess
}

// addSessionToLobby creates a Wolf session, joins it to the claim's lobby,
// records local state, and updates the ResourceClaim status.
func (d *Driver) addSessionToLobby(ctx context.Context, claimUID string, session *direwolfv1alpha1.Session) {
	if _, active := d.state.GetSession(string(session.UID)); active {
		d.state.RemovePending(string(session.UID))
		return
	}
	d.socketMu.Lock()
	defer d.socketMu.Unlock()

	claimState, ok := d.state.Get(claimUID)
	if !ok {
		klog.InfoS("Claim no longer managed, dropping session",
			"sessionUID", session.UID, "claimUID", claimUID)
		d.state.RemovePending(string(session.UID))
		return
	}
	// If the session already has a WolfSessionID, it was created by a previous
	// driver instance. Re-adopt it into local state instead of creating a duplicate.
	if session.Status.WolfSessionID != "" {
		d.readoptSession(ctx, claimUID, claimState, session)
		return
	}
	idx, ok := d.allocator.Allocate()
	if !ok {
		klog.ErrorS(nil, "No wayland sockets available for session",
			"sessionUID", session.UID, "claimUID", claimUID)
		return
	}

	// Resolve the node IP so RTSPFakeIP matches what Moonlight connects to.
	internalIP, _ := d.GetNodeIPs(ctx)
	rtspFakeIP := internalIP
	if rtspFakeIP == "" {
		rtspFakeIP = session.Spec.Config.ClientIP
		if rtspFakeIP == "" {
			rtspFakeIP = "127.0.0.1"
		}
	}
	wolfSession := d.buildWolfSession(session, rtspFakeIP)

	wolfSessionID, err := d.wolfClient.AddSession(ctx, wolfSession)
	if err != nil {
		klog.ErrorS(err, "AddSession failed", "sessionUID", session.UID)
		d.allocator.Release(idx)
		return
	}
	d.state.AddSession(&SessionState{
		ClaimUID:      claimUID,
		SessionUID:    string(session.UID),
		SessionName:   session.Name,
		LobbyName:     claimState.LobbyName,
		WolfSessionID: wolfSessionID,
		WaylandIndex:  idx,
		WaylandSocket: fmt.Sprintf("wayland-%d", idx),
		CreatedAt:     time.Now(),
	})

	// TODO: Implement better session feedback into the dra
	if d.direwolfClient != nil {
		sessionCopy := session.DeepCopy()
		sessionCopy.Status.WolfSessionID = wolfSessionID
		if _, err := d.direwolfClient.DirewolfV1alpha1().Sessions(session.Namespace).UpdateStatus(
			ctx, sessionCopy, metav1.UpdateOptions{},
		); err != nil {
			klog.ErrorS(err, "Failed to write WolfSessionID back to Session status",
				"session", session.Name, "namespace", session.Namespace, "wolfSessionID", wolfSessionID)
		} else {
			klog.V(2).InfoS("Updated Session status with WolfSessionID",
				"session", session.Name, "namespace", session.Namespace, "wolfSessionID", wolfSessionID)
		}
	} else {
		klog.ErrorS(nil, "direwolfClient is nil; cannot write WolfSessionID back to Session status",
			"session", session.Name, "wolfSessionID", wolfSessionID)
	}
	d.state.RemovePending(string(session.UID))
	klog.InfoS("Session added to lobby",
		"sessionUID", session.UID,
		"wolfSessionID", wolfSessionID,
		"lobbyID", claimState.LobbyID,
		"waylandIndex", idx,
		"rtspFakeIP", rtspFakeIP)
}

// readoptSession restores an existing Wolf session into local state after a
// driver restart, without calling AddSession again.
func (d *Driver) readoptSession(
	ctx context.Context,
	claimUID string,
	claimState *WolfResourceState,
	session *direwolfv1alpha1.Session,
) {
	idx, ok := d.allocator.Allocate()
	if !ok {
		klog.ErrorS(nil, "No wayland sockets available for session re-adoption",
			"sessionUID", session.UID, "claimUID", claimUID)
		return
	}

	joined := session.Status.StreamStarted
	d.state.AddSession(&SessionState{
		ClaimUID:      claimUID,
		SessionUID:    string(session.UID),
		SessionName:   session.Name,
		LobbyName:     claimState.LobbyName,
		WolfSessionID: session.Status.WolfSessionID,
		WaylandIndex:  idx,
		WaylandSocket: fmt.Sprintf("wayland-%d", idx),
		CreatedAt:     time.Now(),
		JoinedLobby:   joined,
	})
	d.state.RemovePending(string(session.UID))

	klog.InfoS("Re-adopted existing session",
		"sessionUID", session.UID,
		"wolfSessionID", session.Status.WolfSessionID,
		"lobbyID", claimState.LobbyID,
		"waylandIndex", idx,
		"joined", joined)
}

// leaveSession stops a Wolf session, removes it from the lobby, releases its
// wayland index, and updates the ResourceClaim status.
func (d *Driver) leaveSession(ctx context.Context, sessionUID string) {
	ss, ok := d.state.GetSession(sessionUID)
	if !ok {
		// Not active — might just be pending.
		d.state.RemovePending(sessionUID)
		klog.V(2).InfoS("Removed pending session", "sessionUID", sessionUID)
		return
	}

	claimState, claimOK := d.state.Get(ss.ClaimUID)
	if claimOK {
		if err := d.wolfClient.LeaveLobby(ctx, wolfapi.LeaveLobbyRequest{
			LobbyID:            claimState.LobbyID,
			MoonlightSessionID: ss.WolfSessionID,
		}); err != nil {
			klog.ErrorS(err, "LeaveLobby failed", "sessionUID", sessionUID,
				"wolfSessionID", ss.WolfSessionID, "lobbyID", claimState.LobbyID)
		}

		if err := d.wolfClient.StopSession(ctx, ss.WolfSessionID); err != nil {
			klog.ErrorS(err, "StopSession failed", "sessionUID", sessionUID,
				"wolfSessionID", ss.WolfSessionID)
		}
	}

	d.allocator.Release(ss.WaylandIndex)
	d.state.DeleteSession(sessionUID)

	klog.InfoS("Session left lobby", "SessionName", ss.SessionName, "sessionUID", sessionUID,
		"wolfSessionID", ss.WolfSessionID, "lobbyID", claimState.LobbyID)
}

// processPendingSessionsForLobby iterates over the pending set and adopts any
// sessions whose LobbyName matches the newly-created lobby.
func (d *Driver) processPendingSessionsForLobby(ctx context.Context, lobbyName, claimUID string) {
	pending := d.state.GetPendingList()
	if len(pending) == 0 {
		return
	}

	allSessions, err := d.sessionLister.List(labels.Everything())
	if err != nil {
		klog.ErrorS(err, "Failed to list sessions from cache")
		return
	}

	pendingSet := make(map[string]struct{}, len(pending))
	for _, uid := range pending {
		pendingSet[uid] = struct{}{}
	}
	for _, session := range allSessions {
		if _, ok := pendingSet[string(session.UID)]; !ok {
			continue
		}
		sessionLobbyName := session.Namespace + "/" + session.Spec.LobbyName
		if sessionLobbyName != lobbyName {
			continue
		}

		d.addSessionToLobby(ctx, claimUID, session)
		// On success addSessionToLobby removes from pending; on error it stays
	}
}

// ensureSession is the unified entry point for both add and update events.
// It filters by node ownership, creates the Wolf session if needed, and
// triggers lobby join when the stream is ready.
func (d *Driver) ensureSession(ctx context.Context, session *direwolfv1alpha1.Session) {
	// Only process sessions assigned to this node.
	if session.Status.NodeName != d.nodeName {
		return
	}

	if session.Spec.LobbyName == "" {
		return
	}

	sessionUID := string(session.UID)

	// Already active — check if we need to join the lobby now.
	if ss, active := d.state.GetSession(sessionUID); active {
		if !ss.JoinedLobby && session.Status.StreamStarted {
			go func() {
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
					d.joinSessionToLobby(context.WithoutCancel(ctx), ss)
				}
			}()
		}
		return
	}

	// Remove from pending if it was there (e.g. informer resync after adoption).
	d.state.RemovePending(sessionUID)

	lobbyName := session.Namespace + "/" + session.Spec.LobbyName
	claimUID, ok := d.state.GetClaimByLobbyName(lobbyName)
	if ok {
		d.addSessionToLobby(ctx, claimUID, session)
	} else {
		d.state.AddPending(sessionUID)
		klog.V(2).InfoS("Session pending, no matching lobby yet", "sessionName", session.Name,
			"sessionUID", session.UID, "lobbyName", lobbyName)
	}
}

func (d *Driver) HandleSessionAdd(ctx context.Context, obj any) {
	session, ok := obj.(*direwolfv1alpha1.Session)
	if !ok {
		klog.Errorf("expected *v1alpha1.Session, got %T", obj)
		return
	}
	d.ensureSession(ctx, session)
}

func (d *Driver) HandleSessionDelete(ctx context.Context, obj any) {
	session, ok := obj.(*direwolfv1alpha1.Session)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("expected *v1alpha1.Session or DeletedFinalStateUnknown, got %T", obj)
			return
		}
		session, ok = tombstone.Obj.(*direwolfv1alpha1.Session)
		if !ok {
			klog.Errorf("expected *v1alpha1.Session in tombstone, got %T", tombstone.Obj)
			return
		}
	}
	d.leaveSession(ctx, string(session.UID))
}

func (d *Driver) HandleSessionUpdate(ctx context.Context, newObj any) {
	newSession, ok := newObj.(*direwolfv1alpha1.Session)
	if !ok {
		klog.Errorf("expected *v1alpha1.Session, got %T", newObj)
		return
	}
	d.ensureSession(ctx, newSession)
}

func (d *Driver) joinSessionToLobby(ctx context.Context, ss *SessionState) {
	claimState, ok := d.state.Get(ss.ClaimUID)
	if !ok {
		klog.InfoS("Claim no longer managed, cannot join session",
			"sessionUID", ss.SessionUID, "claimUID", ss.ClaimUID)
		return
	}
	klog.Infof("Joining lobby: %s to session: %s", claimState.LobbyID, ss.WolfSessionID)
	if err := d.wolfClient.JoinLobby(ctx, wolfapi.JoinLobbyRequest{
		LobbyID:            claimState.LobbyID,
		MoonlightSessionID: ss.WolfSessionID,
	}); err != nil {
		klog.ErrorS(err, "JoinLobby failed, cleaning up session",
			"sessionUID", ss.SessionUID, "lobbyID", claimState.LobbyID, "wolfSessionID", ss.WolfSessionID)
		_ = d.wolfClient.StopSession(ctx, ss.WolfSessionID)
		d.allocator.Release(ss.WaylandIndex)
		d.state.DeleteSession(ss.SessionUID)
		return
	}

	updated := *ss
	updated.JoinedLobby = true
	d.state.AddSession(&updated)

	klog.InfoS("Session joined lobby",
		"sessionUID", ss.SessionUID,
		"wolfSessionID", ss.WolfSessionID,
		"lobbyID", claimState.LobbyID)
}
