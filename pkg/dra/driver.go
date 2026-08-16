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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	direwolfv1alpha1 "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
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

	socketMu sync.Mutex // protects socket allocation + lobby session ops

	queueTimeout time.Duration
	extraEnv     map[string]string

	cancelCtx context.CancelCauseFunc

	sessionInformer  cache.SharedIndexInformer
	sessionLister    v1alpha1lister.SessionLister
	sessionWorkqueue workqueue.TypedRateLimitingInterface[string]
}

func NewDriver(
	driverName, nodeName, socketsDir, wolfSockPath, cdiDir string,
	maxSockets int,
	queueTimeout time.Duration,
	extraEnv map[string]string,
	kubeClient kubernetes.Interface,
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
		driverName:   driverName,
		nodeName:     nodeName,
		socketsDir:   socketsDir,
		wolfSockPath: wolfSockPath,
		kubeClient:   kubeClient,
		state:        NewState(),
		allocator:    NewAllocator(maxSockets),
		cdiGen:       NewCDIGenerator(driverName, cdiDir, socketsDir),
		wolfClient:   wolfClient,
		queueTimeout: queueTimeout,
		extraEnv:     extraEnv,
	}

	d.allocator.SyncFromState(d.state)
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
	cdiClaims := make(map[string]*WolfResourceState)
	files, err := os.ReadDir(d.cdiGen.cdiDir)
	if err != nil {
		klog.Warningf("Failed to read CDI directory for reconciliation: %v", err)
	} else {
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(d.cdiGen.cdiDir, file.Name()))
			if err != nil {
				continue
			}

			var spec cdiSpec
			if err := json.Unmarshal(data, &spec); err != nil {
				continue
			}
			if len(spec.Devices) == 0 {
				continue
			}

			dev := spec.Devices[0]
			if !strings.HasPrefix(dev.Name, "lobby-claim-") {
				continue
			}
			claimUID := strings.TrimPrefix(dev.Name, "lobby-claim-")

			var lobbyID string
			var waylandDisplay string
			var lobbyName string
			var ClaimName string
			var ClaimNamespace string
			for _, env := range dev.ContainerEdits.Env {
				if after, ok := strings.CutPrefix(env, "WOLF_SESSION_ID="); ok {
					lobbyID = after
				}
				if after, ok := strings.CutPrefix(env, "WAYLAND_DISPLAY="); ok {
					waylandDisplay = after
				}
				if after, ok := strings.CutPrefix(env, "WOLF_LOBBY_NAME="); ok {
					lobbyName = after
				}
				if after, ok := strings.CutPrefix(env, "CLAIM_NAME="); ok {
					ClaimName = after
				}
				if after, ok := strings.CutPrefix(env, "NAMESPACE="); ok {
					ClaimNamespace = after
				}
			}

			if lobbyID == "" || waylandDisplay == "" {
				continue
			}

			idxStr := strings.TrimPrefix(waylandDisplay, "wayland-")
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				continue
			}

			cdiClaims[claimUID] = &WolfResourceState{
				ClaimUID:          claimUID,
				ClaimName:         ClaimName,
				ClaimNamespace:    ClaimNamespace,
				LobbyID:           lobbyID,
				LobbyName:         lobbyName,
				WaylandIndex:      idx,
				WaylandSocketName: waylandDisplay,
				CreatedAt:         time.Now(),
			}
		}
	}

	lobbies, err := d.wolfClient.ListLobbies(ctx)
	if err != nil {
		klog.Warningf("ListLobbies failed during reconciliation: %v. Restoring from CDI files only.", err)
		for uid, st := range cdiClaims {
			if _, exists := d.state.Get(uid); !exists {
				klog.Infof("Recovered state from CDI for claim %s (Wolf unreachable)", uid)
				d.state.Set(uid, st)
				d.allocator.MarkUsed(st.WaylandIndex)
				d.recoverSessionsFromClaimStatus(ctx, uid, st.ClaimName, st.ClaimNamespace, st.LobbyName)
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
				d.state.Set(uid, st)
				d.allocator.MarkUsed(st.WaylandIndex)
				d.recoverSessionsFromClaimStatus(ctx, uid, st.ClaimName, st.ClaimNamespace, st.LobbyName)
			}
		} else {
			klog.Infof("Cleaning up dead claim %s (lobby %s no longer in Wolf)", uid, st.LobbyID)
			deadSockFile := filepath.Join(d.socketsDir, st.WaylandSocketName)
			klog.Infof("cleaning up %s", deadSockFile)
			err = os.Remove(deadSockFile)
			if err != nil {
				klog.Infof("cleaning up %s failed", deadSockFile)
			}
			err = d.cdiGen.DeleteCDISpecs(uid)
			if err != nil {
				klog.Infof("cleaning up %s failed", deadSockFile)
			}
		}
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
	if err := d.patchDeviceStatus(ctx, claim, lobbyID, []SessionStatusInfo{}); err != nil {
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

		// 1. Sessions still recorded in the ResourceClaim device status.
		if claim.Namespace != "" && claim.Name != "" {
			rc, err := d.kubeClient.ResourceV1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
			if err == nil {
				if data := d.extractDriverData(rc); data != nil {
					for _, s := range data.Sessions {
						if s.SessionID != "" {
							sessionIDs[s.SessionID] = struct{}{}
						}
					}
				}
			} else {
				klog.Warningf("Failed to get claim %s/%s for unprepare: %v", claim.Namespace, claim.Name, err)
			}
		}

		// 2. Sessions tracked in local state (in case state diverged from status).
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

type SessionStatusInfo struct {
	UID          string `json:"uid"`
	Name         string `json:"name"`
	SessionID    string `json:"session_id"`
	WaylandIndex int    `json:"wayland_index"`
}

// deviceStatusData is the shape we store in status.devices[].data.
type deviceStatusData struct {
	LobbyID  string              `json:"lobby_id"`
	Sessions []SessionStatusInfo `json:"sessions"`
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

// extractDriverData returns this driver's status.data, if present and valid.
func (d *Driver) extractDriverData(claim *resourceapi.ResourceClaim) *deviceStatusData {
	for i := range claim.Status.Devices {
		dev := &claim.Status.Devices[i]
		if dev.Driver != d.driverName || dev.Data == nil || len(dev.Data.Raw) == 0 {
			continue
		}
		var dd deviceStatusData
		if err := json.Unmarshal(dev.Data.Raw, &dd); err != nil {
			klog.V(2).ErrorS(err, "Failed to unmarshal device status data", "claim", claim.Name)
			return nil
		}
		return &dd
	}
	return nil
}

// patchDeviceStatus updates status.devices with driver-specific data.
func (d *Driver) patchDeviceStatus(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	lobbyID string,
	sessions []SessionStatusInfo,
) error {
	r := d.findOurAllocation(claim)
	if r == nil {
		return nil
	}

	raw, err := json.Marshal(deviceStatusData{
		LobbyID:  lobbyID,
		Sessions: sessions,
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

// updateClaimStatusForClaimUID recomputes the session list for a claim and patches its status.
func (d *Driver) updateClaimStatusForClaimUID(ctx context.Context, claimUID string) error {
	st, ok := d.state.Get(claimUID)
	if !ok {
		return nil
	}
	if st.ClaimName == "" || st.ClaimNamespace == "" {
		return nil
	}

	claim, err := d.kubeClient.ResourceV1().ResourceClaims(st.ClaimNamespace).Get(ctx, st.ClaimName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get claim for status update: %w", err)
	}

	sessions := d.state.GetSessionsForClaim(claimUID)
	sessionInfos := make([]SessionStatusInfo, 0, len(sessions))
	for _, ss := range sessions {
		if ss.WolfSessionID != "" {
			sessionInfos = append(sessionInfos, SessionStatusInfo{
				UID:          ss.SessionUID,
				Name:         ss.SessionName,
				SessionID:    ss.WolfSessionID,
				WaylandIndex: ss.WaylandIndex,
			})
		}
	}

	return d.patchDeviceStatus(ctx, claim, st.LobbyID, sessionInfos)
}

// RunClaimWatcher starts a background informer that reacts to session list
// changes in ResourceClaim status.devices[].data.
func (d *Driver) RunClaimWatcher(ctx context.Context) {
	lw := &cache.ListWatch{
		ListFunc: func(opts metav1.ListOptions) (runtime.Object, error) {
			return d.kubeClient.ResourceV1().ResourceClaims(metav1.NamespaceAll).List(ctx, opts)
		},
		WatchFunc: func(opts metav1.ListOptions) (watch.Interface, error) {
			return d.kubeClient.ResourceV1().ResourceClaims(metav1.NamespaceAll).Watch(ctx, opts)
		},
	}

	// Set resync to 0: we only want to trigger on actual API updates, no point in periodic resyncs.
	informer := cache.NewSharedInformer(lw, &resourceapi.ResourceClaim{}, 0)
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			go d.handleClaimUpdate(ctx, oldObj, newObj)
		},
	})
	if err != nil {
		klog.Errorf("failed to add event handler: %v", err)
		return
	}

	klog.InfoS("Starting ResourceClaim session watcher")
	informer.Run(ctx.Done())
}

func (d *Driver) handleClaimUpdate(ctx context.Context, oldObj, newObj any) {
	newClaim, ok := newObj.(*resourceapi.ResourceClaim)
	if !ok {
		klog.Errorf("expected *resourceapi.ResourceClaim, got %T", newObj)
		return
	}

	newData := d.extractDriverData(newClaim)
	if newData == nil || newData.LobbyID == "" {
		return
	}

	// If another node manages this claim and has adopted this session
	// remove it from our queue.
	if _, managed := d.state.Get(string(newClaim.UID)); !managed {
		pendingList := d.state.GetPendingList()
		if len(pendingList) == 0 {
			return
		}
		pendingSet := make(map[string]struct{}, len(pendingList))
		for _, uid := range pendingList {
			pendingSet[uid] = struct{}{}
		}
		for _, s := range newData.Sessions {
			if s.UID == "" {
				continue
			}
			if _, ok := pendingSet[s.UID]; ok {
				d.state.RemovePending(s.UID)
				klog.V(2).InfoS("Removed pending session adopted by another node",
					"sessionUID", s.UID, "claimUID", newClaim.UID)
			}
		}
		return
	}

	// managed claim path
	oldClaim, ok := oldObj.(*resourceapi.ResourceClaim)
	if !ok {
		klog.Errorf("expected *resourceapi.ResourceClaim, got %T", oldObj)
		return
	}

	var oldSessions []string
	if oldData := d.extractDriverData(oldClaim); oldData != nil {
		for _, s := range oldData.Sessions {
			oldSessions = append(oldSessions, s.SessionID)
		}
	}

	var newSessions []string
	for _, s := range newData.Sessions {
		newSessions = append(newSessions, s.SessionID)
	}

	d.syncSessions(ctx, newClaim, newData.LobbyID, oldSessions, newSessions)
}

func sliceToSet(ss []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		out[s] = struct{}{}
	}
	return out
}

// syncSessions reconciles Wolf lobby membership with a desired session list.
// It validates each session ID against Wolf's active sessions (via ListSessions),
// prevents duplicate joins by using the claim's status diff, and removes
// failed, unavailable, or duplicate session IDs from the ResourceClaim status.
// TODO: find a better way to prevent duplication
func (d *Driver) syncSessions(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	lobbyID string,
	oldSessions, newSessions []string,
) {
	// Fetch all available sessions from Wolf
	wolfSessions, err := d.wolfClient.ListSessions(ctx)
	if err != nil {
		klog.Warningf("ListSessions failed, skipping sync: %v", err)
		return
	}

	// Build a set of valid session client IDs.
	// In Wolf, the ClientID is the Moonlight session ID.
	availableSet := make(map[string]struct{}, len(wolfSessions))
	for _, s := range wolfSessions {
		if s.ClientID != "" {
			availableSet[s.ClientID] = struct{}{}
		}
	}

	// Use sets to deduplicate and easily diff old vs new
	oldSet := sliceToSet(oldSessions)
	newSet := sliceToSet(newSessions)

	// Track whether the session list in the claim needs to be patched.
	// This happens if there were duplicates in the new sessions, or if
	// some sessions were unavailable or failed to join.
	needsPatch := len(newSet) != len(newSessions)
	validNewSessions := make([]string, 0, len(newSet))

	// Process desired sessions: join newly added ones
	for sid := range newSet {
		// Session no longer exists in Wolf
		// remove it from status.
		if _, available := availableSet[sid]; !available {
			klog.Warningf("Session %s not found in available sessions, removing from claim", sid)
			needsPatch = true
			// If it was previously joined, attempt to leave the lobby.
			if _, wasInOld := oldSet[sid]; wasInOld {
				if err := d.wolfClient.LeaveLobby(ctx, wolfapi.LeaveLobbyRequest{
					LobbyID:            lobbyID,
					MoonlightSessionID: sid,
				}); err != nil {
					klog.Warningf("LeaveLobby failed for unavailable session: lobby=%s session=%s err=%v",
						lobbyID, sid, err)
				} else {
					klog.V(2).Infof("Session %s left lobby %s (unavailable)", sid, lobbyID)
				}
			}
			continue
		}

		// Already managed by the Session informer skip duplicate JoinLobby,
		// but keep it in validNewSessions so we don't patch it out.
		if _, managed := d.state.GetSessionByWolfID(sid); managed {
			validNewSessions = append(validNewSessions, sid)
			continue
		}

		// Only attempt to join sessions that are newly added.
		// This prevents duplicate joins since we rely on the claim's status diff.
		if _, wasInOld := oldSet[sid]; !wasInOld {
			// Not sure if I should put a retry here or not
			// In case the session is not ready or haven't been created
			// This is to be handled during the operator updates alongside session stuff
			if err := d.wolfClient.JoinLobby(ctx, wolfapi.JoinLobbyRequest{
				LobbyID:            lobbyID,
				MoonlightSessionID: sid,
			}); err != nil {
				klog.Warningf("JoinLobby failed: lobby=%s session=%s err=%v", lobbyID, sid, err)
				needsPatch = true
				continue // Don't add to validNewSessions since it failed to join
			}
			klog.V(2).Infof("Session %s joined lobby %s", sid, lobbyID)
		}

		validNewSessions = append(validNewSessions, sid)
	}

	// Process removed sessions: leave the lobby
	for sid := range oldSet {
		if _, stillInNew := newSet[sid]; stillInNew {
			continue
		}
		// Already gone from Wolf (e.g., informer's leaveSession already stopped it).
		if _, available := availableSet[sid]; !available {
			continue
		}
		if err := d.wolfClient.LeaveLobby(ctx, wolfapi.LeaveLobbyRequest{
			LobbyID:            lobbyID,
			MoonlightSessionID: sid,
		}); err != nil {
			klog.Warningf("LeaveLobby failed: lobby=%s session=%s err=%v", lobbyID, sid, err)
		} else {
			klog.V(2).Infof("Session %s left lobby %s", sid, lobbyID)
		}
	}

	// If we found duplicates, unavailable sessions, or failed joins,
	// patch the claim to reflect the actual valid session list.
	if needsPatch {
		sort.Strings(validNewSessions)
		sessionInfos := make([]SessionStatusInfo, 0, len(validNewSessions))
		for _, sid := range validNewSessions {
			if ss, ok := d.state.GetSessionByWolfID(sid); ok {
				sessionInfos = append(sessionInfos, SessionStatusInfo{
					UID:          ss.SessionUID,
					Name:         ss.SessionName,
					SessionID:    ss.WolfSessionID,
					WaylandIndex: ss.WaylandIndex,
				})
			} else {
				sessionInfos = append(sessionInfos, SessionStatusInfo{SessionID: sid})
			}
		}
		klog.Infof("Patching claim %s to update sessions: %v", claim.UID, validNewSessions)
		if err := d.patchDeviceStatus(ctx, claim, lobbyID, sessionInfos); err != nil {
			klog.Warningf("Failed to patch claim after filtering sessions: %v", err)
		}
	}
}

// Session CRD event handlers

// HandleSessionAdd is called by the Session informer when a Session CRD is added.
// TODO: Handle concurrent session adding?
// it is very unlikely, but the probability is never zero.
func (d *Driver) HandleSessionAdd(ctx context.Context, obj any) {
	session, ok := obj.(*direwolfv1alpha1.Session)
	if !ok {
		klog.Errorf("expected *v1alpha1.Session, got %T", obj)
		return
	}

	if session.Spec.LobbyName == "" {
		return
	}

	// Ignore if already active (informer resync, duplicate event, etc.).
	if _, active := d.state.GetSession(string(session.UID)); active {
		return
	}
	lobbyName := session.Namespace + "/" + session.Spec.LobbyName

	claimUID, ok := d.state.GetClaimByLobbyName(lobbyName)
	if ok {
		d.addSessionToLobby(ctx, claimUID, session)
	} else {
		d.state.AddPending(string(session.UID))
		klog.V(2).InfoS("Session pending, no matching lobby yet", "sessionName", session.Name,
			"sessionUID", session.UID, "lobbyName", lobbyName)
	}
}

// HandleSessionDelete is called by the Session informer when a Session CRD is deleted.
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

	idx, ok := d.allocator.Allocate()
	if !ok {
		klog.ErrorS(nil, "No wayland sockets available for session",
			"sessionUID", session.UID, "claimUID", claimUID)
		return
	}

	wolfSession := wolfapi.Session{
		// TODO: after profile informer add clientID
		ClientIP:         session.Spec.Config.ClientIP,
		AESKey:           session.Spec.Config.AESKey,
		AESIV:            session.Spec.Config.AESIV,
		VideoWidth:       session.Spec.Config.VideoWidth,
		VideoHeight:      session.Spec.Config.VideoHeight,
		VideoRefreshRate: session.Spec.Config.VideoRefreshRate,
		// placeholders
		// these should be acquired from operator / profile
		RTSPFakeIP: "10.96.64.123",
		ClientSettings: wolfapi.ClientSettings{
			ControllersOverride:      []string{"XBOX"},
			MotionControllerOverride: "AUTO",
			HScrollAcceleration:      1,
			MouseAcceleration:        1,
			RunGID:                   1000,
			RunUID:                   1000,
			VScrollAcceleration:      1,
		},
	}
	// Derive audio channel count from surround flags if present, else default to stereo.
	if session.Spec.Config.SurroundAudioFlags > 0 {
		wolfSession.AudioChannelCount = session.Spec.Config.SurroundAudioFlags
	} else {
		wolfSession.AudioChannelCount = 2
	}

	wolfSessionID, err := d.wolfClient.AddSession(ctx, wolfSession)
	if err != nil {
		klog.ErrorS(err, "AddSession failed", "sessionUID", session.UID)
		d.allocator.Release(idx)
		return
	}
	if err := d.wolfClient.JoinLobby(ctx, wolfapi.JoinLobbyRequest{
		LobbyID:            claimState.LobbyID,
		MoonlightSessionID: wolfSessionID,
	}); err != nil {
		klog.ErrorS(err, "JoinLobby failed", "sessionUID", session.UID,
			"lobbyID", claimState.LobbyID, "wolfSessionID", wolfSessionID)
		_ = d.wolfClient.StopSession(ctx, wolfSessionID)
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

	if err := d.updateClaimStatusForClaimUID(ctx, claimUID); err != nil {
		klog.ErrorS(err, "Failed to update claim status after adding session",
			"claimUID", claimUID, "sessionUID", session.UID)
	}

	d.state.RemovePending(string(session.UID))
	klog.InfoS("Session added to lobby",
		"sessionUID", session.UID,
		"wolfSessionID", wolfSessionID,
		"lobbyID", claimState.LobbyID,
		"waylandIndex", idx)
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

	if claimOK {
		if err := d.updateClaimStatusForClaimUID(ctx, ss.ClaimUID); err != nil {
			klog.ErrorS(err, "Failed to update claim status after removing session",
				"claimUID", ss.ClaimUID, "sessionUID", sessionUID)
		}
	}

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

	for _, sessionUID := range pending {
		// Lister caches by namespace/name; we must list all and match by UID.
		allSessions, err := d.sessionLister.List(labels.Everything())
		if err != nil {
			klog.ErrorS(err, "Failed to list sessions from cache", "sessionUID", sessionUID)
			continue
		}

		var session *direwolfv1alpha1.Session
		for _, s := range allSessions {
			if string(s.UID) == sessionUID {
				session = s
				break
			}
		}
		if session == nil {
			klog.V(2).InfoS("Pending session not found in cache, skipping", "sessionUID", sessionUID)
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

// recoverSessionsFromClaimStatus reads the ResourceClaim device status and
// reconstructs SessionState entries for any sessions still attached to the claim.
// This prevents the Session informer from re-creating them after a wolf-dra restart.
func (d *Driver) recoverSessionsFromClaimStatus(ctx context.Context, claimUID, claimName, claimNamespace, lobbyName string) {
	if claimNamespace == "" || claimName == "" {
		return
	}

	claim, err := d.kubeClient.ResourceV1().ResourceClaims(claimNamespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to get claim %s/%s during session recovery: %v", claimNamespace, claimName, err)
		return
	}

	data := d.extractDriverData(claim)
	if data == nil || len(data.Sessions) == 0 {
		return
	}

	for _, s := range data.Sessions {
		if s.UID == "" || s.SessionID == "" || s.WaylandIndex <= 0 {
			continue
		}
		if _, active := d.state.GetSession(s.UID); active {
			continue
		}

		sockName := fmt.Sprintf("wayland-%d", s.WaylandIndex)
		if !socketExists(d.socketsDir, sockName) {
			klog.V(2).InfoS("Skipping session recovery, socket missing",
				"sessionUID", s.UID, "socket", sockName)
			continue
		}

		ss := &SessionState{
			ClaimUID:      claimUID,
			SessionUID:    s.UID,
			SessionName:   s.Name,
			LobbyName:     lobbyName,
			WolfSessionID: s.SessionID,
			WaylandIndex:  s.WaylandIndex,
			WaylandSocket: sockName,
			CreatedAt:     time.Now(),
		}
		d.state.AddSession(ss)
		d.allocator.MarkUsed(s.WaylandIndex)
		klog.InfoS("Recovered session from claim status",
			"sessionUID", s.UID,
			"sessionName", s.Name,
			"wolfSessionID", s.SessionID,
			"waylandIndex", s.WaylandIndex,
			"claimUID", claimUID)
	}
}

// HandleSessionUpdate reacts to Session CRD updates. The only transition we
// care about is the initial patch of LobbyName by the session controller
// (empty → non-empty). Everything else is ignored.
func (d *Driver) HandleSessionUpdate(ctx context.Context, newObj any) {
	newSession, ok := newObj.(*direwolfv1alpha1.Session)
	if !ok {
		klog.Errorf("expected *v1alpha1.Session, got %T", newObj)
		return
	}
	klog.Infof("Detected Change in %s", newSession.Name)
	// We can only change lobby name for now
	// until I figure out how wolf changes the stream quality
	// without tearing down the lobby
	if newSession.Spec.LobbyName == "" {
		klog.Infof("New Lobby Name Empty, skipping... : %s", newSession.Spec.LobbyName)
		return
	}

	// Already active (e.g. handled by a previous resync or race).
	if _, active := d.state.GetSession(string(newSession.UID)); active {
		klog.Infof("Session: %s is active, skipping...", newSession.Name)
		return
	}

	lobbyName := newSession.Namespace + "/" + newSession.Spec.LobbyName
	claimUID, ok := d.state.GetClaimByLobbyName(lobbyName)
	if ok {
		klog.Infof("Updating State with new session: %s", newSession.Name)
		d.addSessionToLobby(ctx, claimUID, newSession)
	} else {
		d.state.AddPending(string(newSession.UID))
		klog.V(2).InfoS("Session pending after LobbyName patch",
			"sessionName", newSession.Name,
			"sessionUID", newSession.UID,
			"lobbyName", lobbyName)
	}
}
