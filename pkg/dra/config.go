package dra

import (
	"cmp"
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
	MaxLobbies          int               `json:"maxLobbies,omitempty"`
	QueueTimeoutSeconds int               `json:"queueTimeoutSeconds,omitempty"`
	ExtraEnv            map[string]string `json:"extraEnv,omitempty"`
}

func (c *WolfDriverConfig) Defaults() *WolfDriverConfig {
	c.SocketsDir = cmp.Or(c.SocketsDir, "/var/run/wolf-sockets")
	c.WolfSocketPath = cmp.Or(c.WolfSocketPath, "/var/run/wolf.sock")
	c.MaxLobbies = cmp.Or(c.MaxLobbies, 10)
	c.QueueTimeoutSeconds = cmp.Or(c.QueueTimeoutSeconds, 30)
	return c
}

// ClaimParams are the resource claim parameters
// they're supposed to be passed from moonlight to operator
// and finally to the wolf-dra to create the lobby
type ClaimParams struct {
	VideoSettings  wolfapi.LobbyVideoSettings `json:"video_settings"`
	AudioSettings  wolfapi.LobbyAudioSettings `json:"audio_settings"`
	ClientSettings wolfapi.ClientSettings     `json:"client_settings"`
	PinRequired    bool                       `json:"pin_required,omitempty"`
	Pin            []int                      `json:"pin,omitempty"`
	MultiUser      bool                       `json:"multi_user,omitempty"`
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
	// This is Acquired from the DeviceClass
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
	// applyDefaults fills in sensible defaults for any missing fields.
	base.VideoSettings.Width = cmp.Or(override.VideoSettings.Width, base.VideoSettings.Width)
	base.VideoSettings.Height = cmp.Or(override.VideoSettings.Height, base.VideoSettings.Height)
	base.VideoSettings.RefreshRate = cmp.Or(override.VideoSettings.RefreshRate, base.VideoSettings.RefreshRate)
	base.VideoSettings.WaylandRenderNode = cmp.Or(override.VideoSettings.WaylandRenderNode, base.VideoSettings.WaylandRenderNode)
	base.VideoSettings.RunnerRenderNode = cmp.Or(override.VideoSettings.RunnerRenderNode, base.VideoSettings.RunnerRenderNode)
	base.VideoSettings.VideoProducerBufferCaps = cmp.Or(override.VideoSettings.VideoProducerBufferCaps, base.VideoSettings.VideoProducerBufferCaps)
	base.AudioSettings.ChannelCount = cmp.Or(override.AudioSettings.ChannelCount, base.AudioSettings.ChannelCount)
	base.ClientSettings.HScrollAcceleration = cmp.Or(override.ClientSettings.HScrollAcceleration, base.ClientSettings.HScrollAcceleration)
	base.ClientSettings.MouseAcceleration = cmp.Or(override.ClientSettings.MouseAcceleration, base.ClientSettings.MouseAcceleration)
	base.ClientSettings.RunGID = cmp.Or(override.ClientSettings.RunGID, base.ClientSettings.RunGID)
	base.ClientSettings.RunUID = cmp.Or(override.ClientSettings.RunUID, base.ClientSettings.RunUID)
	base.ClientSettings.VScrollAcceleration = cmp.Or(override.ClientSettings.VScrollAcceleration, base.ClientSettings.VScrollAcceleration)
}

func (p *ClaimParams) applyDefaults() {
	p.VideoSettings.Width = cmp.Or(p.VideoSettings.Width, 1920)
	p.VideoSettings.Height = cmp.Or(p.VideoSettings.Height, 1080)
	p.VideoSettings.RefreshRate = cmp.Or(p.VideoSettings.RefreshRate, 60)
	p.VideoSettings.WaylandRenderNode = cmp.Or(p.VideoSettings.WaylandRenderNode, "/dev/dri/renderD128")
	p.VideoSettings.RunnerRenderNode = cmp.Or(p.VideoSettings.RunnerRenderNode, "/dev/dri/renderD128")
	// TODO: Learn how to automatically find buffercaps from wolf instead of hardcoding it for now.
	p.VideoSettings.VideoProducerBufferCaps = cmp.Or(p.VideoSettings.VideoProducerBufferCaps, "video/x-raw(memory:DMABuf), drm-format={NV12,YV12,YU12,P012,YUYV,YU24,AB24,AR24,XB24,XR24}")
	p.AudioSettings.ChannelCount = cmp.Or(p.AudioSettings.ChannelCount, 2)
	p.ClientSettings.HScrollAcceleration = cmp.Or(p.ClientSettings.HScrollAcceleration, 1)
	p.ClientSettings.MouseAcceleration = cmp.Or(p.ClientSettings.MouseAcceleration, 1)
	p.ClientSettings.RunGID = cmp.Or(p.ClientSettings.RunGID, 1000)
	p.ClientSettings.RunUID = cmp.Or(p.ClientSettings.RunUID, 1000)
	p.ClientSettings.VScrollAcceleration = cmp.Or(p.ClientSettings.VScrollAcceleration, 1)
}
