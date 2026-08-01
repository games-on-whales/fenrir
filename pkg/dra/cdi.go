package dra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	wolfapi "games-on-whales.github.io/direwolf/pkg/wolfapi"
)

const (
	cdiVersion    = "0.8.0"
	xdgRuntimeDir = "/run/user/wolf"
	pulseSockFile = "/pulse-socket"
)

type cdiSpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"`
	Devices    []cdiDevice `json:"devices"`
}

type cdiDevice struct {
	Name           string            `json:"name"`
	ContainerEdits cdiContainerEdits `json:"containerEdits"`
}

type cdiContainerEdits struct {
	Env    []string   `json:"env,omitempty"`
	Mounts []cdiMount `json:"mounts,omitempty"`
}

type cdiMount struct {
	HostPath      string   `json:"hostPath"`
	ContainerPath string   `json:"containerPath"`
	Options       []string `json:"options,omitempty"`
}

// CDIGenerator writes CDI JSON files to the configured CDI directory.
type CDIGenerator struct {
	driverName string
	cdiDir     string
	socketsDir string
}

func NewCDIGenerator(driverName, cdiDir, socketsDir string) *CDIGenerator {
	return &CDIGenerator{
		driverName: driverName,
		cdiDir:     cdiDir,
		socketsDir: socketsDir,
	}
}

// DeviceID returns the CDI device ID for a claim. It is deterministic and
// based solely on the claim UID, so it is stable across re-preparations.
func (g *CDIGenerator) DeviceID(claimUID string) string {
	return fmt.Sprintf("%s/lobby=lobby-claim-%s", g.driverName, claimUID)
}

// GenerateLobbyCDI creates a CDI device for a specific wayland-N socket.
// extraEnv are extra driver specific env vars merged on top of the standard env vars.
func (g *CDIGenerator) GenerateLobbyCDI(
	claimUID string,
	idx int,
	lobbyID string,
	video wolfapi.LobbyVideoSettings,
	extraEnv map[string]string,
) (string, error) {
	// TODO: more restrictive permissions??
	if err := os.MkdirAll(g.cdiDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cdi: %w", err)
	}

	waylandSockFile := fmt.Sprintf("wayland-%d", idx)

	devName := "lobby-claim-" + claimUID

	env := []string{
		// UID and PGID can be handled by app spec?
		// however, they are still defined by the controller settings
		// I need some guidance over the env vars
		fmt.Sprintf("WAYLAND_DISPLAY=wayland-%d", idx),
		fmt.Sprintf("GAMESCOPE_REFRESH=%d", video.RefreshRate),
		fmt.Sprintf("GAMESCOPE_WIDTH=%d", video.Width),
		fmt.Sprintf("GAMESCOPE_HEIGHT=%d", video.Height),
		"PULSE_SOURCE=virtual_sink_" + lobbyID + ".monitor",
		"PULSE_SINK=virtual_sink_" + lobbyID,
		"PULSE_SERVER=" + xdgRuntimeDir + pulseSockFile,
		"XDG_RUNTIME_DIR=" + xdgRuntimeDir,
		"WOLF_SESSION_ID=" + lobbyID,
		"WOLF_VIDEO_BUFFER_CAPS=" + video.VideoProducerBufferCaps,
		// TODO pulse audio sinks
	}
	for k, v := range extraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	spec := cdiSpec{
		CDIVersion: cdiVersion,
		Kind:       g.driverName + "/lobby",
		Devices: []cdiDevice{
			{
				Name: devName,
				ContainerEdits: cdiContainerEdits{
					Env: env,
					Mounts: []cdiMount{
						{
							HostPath:      filepath.Join(g.socketsDir, waylandSockFile),
							ContainerPath: filepath.Join(xdgRuntimeDir, waylandSockFile),
							Options:       []string{"rw", "bind"},
						},
						// TODO pulse audio
						{
							HostPath:      filepath.Join(g.socketsDir, pulseSockFile),
							ContainerPath: filepath.Join(xdgRuntimeDir, pulseSockFile),
							Options:       []string{"rw", "bind"},
						},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal CDI spec: %w", err)
	}

	fileName := fmt.Sprintf("%s-wayland-%s.json", sanitize(g.driverName), claimUID)
	filePath := filepath.Join(g.cdiDir, fileName)
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return "", fmt.Errorf("write CDI spec: %w", err)
	}

	return g.DeviceID(claimUID), nil
}

// DeleteCDISpecs removes all CDI files for a claim.
// a missing file is not an error.
func (g *CDIGenerator) DeleteCDISpecs(claimUID string) error {
	waylandFile := fmt.Sprintf("%s-wayland-%s.json", sanitize(g.driverName), claimUID)
	if err := os.Remove(filepath.Join(g.cdiDir, waylandFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove lobby CDI: %w", err)
	}
	// PulseAudio CDI will be added here later.
	return nil
}

func sanitize(s string) string {
	return strings.ReplaceAll(s, "/", "-")
}
