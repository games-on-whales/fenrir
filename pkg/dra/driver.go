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

	wolfapi "games-on-whales.github.io/direwolf/pkg/wolfapi"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

const (
	lobbyNamePrefix = "dra-"
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

	createLobbyMu sync.Mutex // TODO: Remove after implementing socket information in the SSE

	queueTimeout time.Duration
	extraEnv     map[string]string

	cancelCtx context.CancelCauseFunc
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
			if !strings.HasPrefix(dev.Name, "wayland-claim-") {
				continue
			}
			claimUID := strings.TrimPrefix(dev.Name, "wayland-claim-")

			var lobbyID string
			var waylandDisplay string
			for _, env := range dev.ContainerEdits.Env {
				if strings.HasPrefix(env, "WOLF_SESSION_ID=") {
					lobbyID = strings.TrimPrefix(env, "WOLF_SESSION_ID=")
				}
				if strings.HasPrefix(env, "WAYLAND_DISPLAY=") {
					waylandDisplay = strings.TrimPrefix(env, "WAYLAND_DISPLAY=")
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
				LobbyID:           lobbyID,
				WaylandIndex:      idx,
				WaylandSocketName: waylandDisplay,
				CreatedAt:         time.Now(),
			}
		}
	}

	lobbies, err := d.wolfClient.ListLobbies(ctx)
	if err != nil {
		// If this actually happens, wolf has crashed, and we need to restart the pods and clean up the socket files?
		klog.Warningf("ListLobbies failed during reconciliation: %v. Restoring from CDI files only.", err)
		for uid, st := range cdiClaims {
			if _, exists := d.state.Get(uid); !exists {
				klog.Infof("Recovered state from CDI for claim %s (Wolf unreachable)", uid)
				d.state.Set(uid, st)
				d.allocator.MarkUsed(st.WaylandIndex)
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
						DeviceName:   "wayland-pool",
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
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("no wayland sockets available")}
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
	d.createLobbyMu.Lock()
	defer d.createLobbyMu.Unlock()

	if !d.allocator.Available() {
		klog.InfoS("No wayland sockets available (after lock)", "uid", uid)
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("no wayland sockets available")}
	}

	req := wolfapi.LobbyCreateRequest{
		ProfileID:              "default",
		Name:                   fmt.Sprintf("%s%s", lobbyNamePrefix, uidStr),
		StopWhenEveryoneLeaves: false,
		ClientSettings:         params.ClientSettings,
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
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("create lobby returned empty ID")}
	}
	klog.V(2).InfoS("Lobby created", "lobbyID", lobbyID, "claimUID", uid)

	idx, sockName, err := d.discoverNewWaylandSocket(ctx, beforeSnapshot)
	if err != nil {
		klog.ErrorS(err, "Wayland socket discovery failed", "lobbyID", lobbyID)
		_ = d.wolfClient.StopLobby(ctx, wolfapi.StopLobbyRequest{LobbyID: lobbyID})
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("discover socket: %w", err)}
	}

	d.allocator.MarkUsed(idx)

	d.state.Set(uidStr, &WolfResourceState{
		ClaimUID:          uidStr,
		LobbyID:           lobbyID,
		WaylandIndex:      idx,
		WaylandSocketName: sockName,
		CreatedAt:         time.Now(),
	})

	cdiID, err := d.cdiGen.GenerateWaylandCDI(uidStr, idx, lobbyID, params.VideoSettings, d.extraEnv)
	if err != nil {
		klog.ErrorS(err, "CDI generation failed", "claimUID", uid)
		_ = d.wolfClient.StopLobby(ctx, wolfapi.StopLobbyRequest{LobbyID: lobbyID})
		d.allocator.Release(idx)
		d.state.Delete(uidStr)
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("cdi spec: %w", err)}
	}

	klog.InfoS("Claim prepared", "claimUID", uid, "waylandIndex", idx, "cdi", cdiID)

	return kubeletplugin.PrepareResult{
		Devices: []kubeletplugin.Device{
			{
				PoolName:     d.nodeName,
				DeviceName:   "wayland-pool",
				CDIDeviceIDs: []string{cdiID},
			},
		},
	}
}

func (d *Driver) UnprepareResourceClaims(
	ctx context.Context,
	claims []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	klog.InfoS("UnprepareResourceClaims", "count", len(claims))
	results := make(map[types.UID]error)

	for _, claim := range claims {
		uid := string(claim.UID)
		klog.InfoS("Unpreparing claim", "uid", uid)

		if err := d.cdiGen.DeleteCDISpecs(uid); err != nil {
			klog.ErrorS(err, "Failed to delete CDI specs", "uid", uid)
		}

		st, ok := d.state.Get(uid)
		if !ok {
			klog.V(2).InfoS("Claim not in state, nothing to unprepare", "uid", uid)
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

func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
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
func (d *Driver) discoverNewWaylandSocket(ctx context.Context, before map[int]time.Time) (int, string, error) {
	timeout := time.After(2 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	known := d.allocator.Used()

	for {
		select {
		case <-ctx.Done():
			return 0, "", ctx.Err()
		case <-timeout:
			return 0, "", fmt.Errorf("timeout waiting for wayland socket")
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
