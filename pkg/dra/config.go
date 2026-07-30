package dra

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/davecgh/go-spew/spew"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	wolfapi "games-on-whales.github.io/direwolf/pkg/wolfapi"
)

type WolfDriverConfig struct {
	SocketsDir          string            `json:"socketsDir,omitempty"`
	WolfSocketPath      string            `json:"wolfSocketPath,omitempty"`
	MaxWaylandSockets   int               `json:"maxWaylandSockets,omitempty"`
	QueueTimeoutSeconds int               `json:"queueTimeoutSeconds,omitempty"`
	ExtraEnv            map[string]string `json:"extraEnv,omitempty"`
}

func (c *WolfDriverConfig) Defaults() *WolfDriverConfig {
	if c.SocketsDir == "" {
		c.SocketsDir = "/var/run/wolf-sockets"
	}
	if c.WolfSocketPath == "" {
		c.WolfSocketPath = "/var/run/wolf.sock"
	}
	if c.MaxWaylandSockets == 0 {
		c.MaxWaylandSockets = 10
	}
	if c.QueueTimeoutSeconds == 0 {
		c.QueueTimeoutSeconds = 30
	}
	return c
}

type ClaimParams struct {
	VideoSettings  wolfapi.LobbyVideoSettings `json:"video_settings"`
	AudioSettings  wolfapi.LobbyAudioSettings `json:"audio_settings"`
	ClientSettings wolfapi.ClientSettings     `json:"client_settings"`
}

// ParseClaimParams extracts video/audio settings (should it include more?) from the claim's
// DeviceClaim.Config slice and the DeviceClass.Config slice.
// DeviceClass values override ResourceClaim values.
// Render nodes are validated on disk before defaults are applied.
func ParseClaimParams(claim *resourceapi.ResourceClaim, class *resourceapi.DeviceClass, driverName string) (*ClaimParams, error) {
	params := &ClaimParams{}
	found := false

	// 1. Parse claim-level config first.
	for _, cfg := range claim.Spec.Devices.Config {
		if cfg.Opaque == nil || cfg.Opaque.Driver != driverName {
			klog.Info("skipping claim params due to invalid Driver name")
			continue
		}
		found = true
		if len(cfg.Opaque.Parameters.Raw) == 0 {
			klog.Info("skipping claim params due to empty opaque configs")
			continue
		}

		if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, params); err != nil {
			return nil, fmt.Errorf("unmarshal claim params for driver %s: %w", driverName, err)
		}
	}

	// 2. Apply DeviceClass override (class takes precedence over claim).
	if class != nil {
		for _, cfg := range class.Spec.Config {
			if cfg.Opaque == nil || cfg.Opaque.Driver != driverName {
				klog.Info("skipping driver params due to invalid Driver name")
				continue
			}
			found = true
			if len(cfg.Opaque.Parameters.Raw) == 0 {
				klog.Info("skipping driver params due to empty opaque configs")
				continue
			}

			var classParams ClaimParams
			if err := json.Unmarshal(cfg.Opaque.Parameters.Raw, &classParams); err != nil {
				return nil, fmt.Errorf("unmarshal device class params for driver %s: %w", driverName, err)
			}
			spew.Dump(cfg.Opaque.Parameters.Raw)
			mergeClaimParams(params, &classParams)
		}
	}

	if !found {
		klog.InfoS("No matching opaque config found, using defaults", "driver", driverName, "claim", claim.Name)
	}

	// 3. Validate render nodes before applying defaults.
	if params.VideoSettings.WaylandRenderNode != "" {
		if _, err := os.Stat(params.VideoSettings.WaylandRenderNode); err != nil {
			klog.Warningf("WaylandRenderNode %q does not exist, falling back to default", params.VideoSettings.WaylandRenderNode)
			params.VideoSettings.WaylandRenderNode = ""
		}
	}
	// do i need to set this?
	if params.VideoSettings.RunnerRenderNode != "" {
		if _, err := os.Stat(params.VideoSettings.RunnerRenderNode); err != nil {
			klog.Warningf("RunnerRenderNode %q does not exist, falling back to default", params.VideoSettings.RunnerRenderNode)
			params.VideoSettings.RunnerRenderNode = ""
		}
	}

	params.applyDefaults()
	// need better logging
	klog.V(2).InfoS("Parsed claim params",
		"claim", claim.Name,
		"driver", driverName,
		"width", params.VideoSettings.Width,
		"height", params.VideoSettings.Height,
		"refresh", params.VideoSettings.RefreshRate,
		"renderNode", params.VideoSettings.WaylandRenderNode)

	return params, nil
}

// mergeClaimParams copies non-zero / non-empty fields from override into base.
// need to find a cleaner way to implement this
// most likely will add defaults to the crd itself
func mergeClaimParams(base, override *ClaimParams) {
	klog.Infof("resource class params: %v", base)
	klog.Infof("device class params: %v", override)
	if override.VideoSettings.Width != 0 {
		base.VideoSettings.Width = override.VideoSettings.Width
	}
	if override.VideoSettings.Height != 0 {
		base.VideoSettings.Height = override.VideoSettings.Height
	}
	if override.VideoSettings.RefreshRate != 0 {
		base.VideoSettings.RefreshRate = override.VideoSettings.RefreshRate
	}
	if override.VideoSettings.WaylandRenderNode != "" {
		base.VideoSettings.WaylandRenderNode = override.VideoSettings.WaylandRenderNode
	}
	if override.VideoSettings.RunnerRenderNode != "" {
		base.VideoSettings.RunnerRenderNode = override.VideoSettings.RunnerRenderNode
	}
	if override.VideoSettings.VideoProducerBufferCaps != "" {
		base.VideoSettings.VideoProducerBufferCaps = override.VideoSettings.VideoProducerBufferCaps
	}
	if override.AudioSettings.ChannelCount != 0 {
		base.AudioSettings.ChannelCount = override.AudioSettings.ChannelCount
	}
	if override.ClientSettings.HScrollAcceleration != 0 {
		base.ClientSettings.HScrollAcceleration = override.ClientSettings.HScrollAcceleration
	}
	if override.ClientSettings.MouseAcceleration != 0 {
		base.ClientSettings.MouseAcceleration = override.ClientSettings.MouseAcceleration
	}
	if override.ClientSettings.RunGID != 0 {
		base.ClientSettings.RunGID = override.ClientSettings.RunGID
	}
	if override.ClientSettings.RunUID != 0 {
		base.ClientSettings.RunUID = override.ClientSettings.RunUID
	}
	if override.ClientSettings.VScrollAcceleration != 0 {
		base.ClientSettings.VScrollAcceleration = override.ClientSettings.VScrollAcceleration
	}
}

// applyDefaults fills in sensible defaults for any missing fields.
func (p *ClaimParams) applyDefaults() {
	if p.VideoSettings.Width == 0 {
		p.VideoSettings.Width = 1920
	}
	if p.VideoSettings.Height == 0 {
		p.VideoSettings.Height = 1080
	}
	if p.VideoSettings.RefreshRate == 0 {
		p.VideoSettings.RefreshRate = 60
	}
	if p.VideoSettings.WaylandRenderNode == "" {
		p.VideoSettings.WaylandRenderNode = "/dev/dri/renderD128"
	}
	if p.VideoSettings.RunnerRenderNode == "" {
		p.VideoSettings.RunnerRenderNode = "/dev/dri/renderD128"
	}
	if p.VideoSettings.VideoProducerBufferCaps == "" {
		p.VideoSettings.VideoProducerBufferCaps = "video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}"
	}
	if p.AudioSettings.ChannelCount == 0 {
		p.AudioSettings.ChannelCount = 2
	}
	if p.ClientSettings.HScrollAcceleration == 0 {
		p.ClientSettings.HScrollAcceleration = 1
	}
	if p.ClientSettings.MouseAcceleration == 0 {
		p.ClientSettings.MouseAcceleration = 1
	}
	if p.ClientSettings.RunGID == 0 {
		p.ClientSettings.RunGID = 1000
	}
	if p.ClientSettings.RunUID == 0 {
		p.ClientSettings.RunUID = 1000
	}
	if p.ClientSettings.VScrollAcceleration == 0 {
		p.ClientSettings.VScrollAcceleration = 1
	}
}
