package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"games-on-whales.github.io/direwolf/pkg/fakeudev"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// UdevDataPath is where udev database entries are written for hotplugged
// devices. It must be a volume shared with the app container (mounted at
// /run/udev there) for libudev consumers (SDL/Steam) to see them.
const UdevDataPath = "/run/udev/data"

// Represents the controller that runs inside the Pod itself
type Agent struct {
	WolfClient wolfapi.Client
}

func NewAgent(
	wolfClient wolfapi.Client,
) *Agent {
	res := &Agent{
		WolfClient: wolfClient,
	}

	return res
}

func (a *Agent) Run(ctx context.Context) error {
	klog.Infof("Starting Agent")
	if err := a.watchEvents(ctx); err != nil {
		return err
	}

	klog.Infof("Agent started")
	return nil
}

func (a *Agent) watchEvents(ctx context.Context) error {
	ch, err := a.WolfClient.SubscribeToEvents(ctx)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok || ev == nil {
					klog.Infof("Event channel closed")
					return
				}

				klog.Infof("Received event: %s", ev.Event)
				klog.Infof("Event ID: %s", ev.ID)
				klog.Infof("Event Data: %s", ev.Data)
				klog.Infof("Event Retry: %d", ev.Retry)
				klog.Infof("Event Comment: %v", ev.Comment)

				switch wolfapi.WolfEventType(ev.Event) {
				// Wolf handles a moonlight disconnect as a "Pause".
				// When moonlight disconnects from Wolf we should reflect that
				// into the state in Kubernetes so things can be cleaned up.
				case wolfapi.PauseStreamEventType:
					var pauseEvent wolfapi.PauseStreamEvent
					if err := json.Unmarshal(ev.Data, &pauseEvent); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to unmarshal pause stream event: %w", err))
						continue
					}

					if err := a.WolfClient.StopSession(ctx, pauseEvent.SessionID); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to stop session: %w", err))
						continue
					}
				// Wolf hotplugged a virtual input device. Play the role of
				// fake-udev: publish the udev db entry and broadcast a
				// synthetic udev netlink event in the pod's network
				// namespace so the app container's SDL/Steam notices it.
				case wolfapi.PlugDeviceEventType:
					var plugEvent wolfapi.PlugDeviceEvent
					if err := json.Unmarshal(ev.Data, &plugEvent); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to unmarshal plug device event: %w", err))
						continue
					}
					a.handleDevicePlug(plugEvent)
				case wolfapi.UnplugDeviceEventType:
					var unplugEvent wolfapi.UnplugDeviceEvent
					if err := json.Unmarshal(ev.Data, &unplugEvent); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to unmarshal unplug device event: %w", err))
						continue
					}
					a.handleDeviceUnplug(unplugEvent)
				default:
					continue
				}
			}
		}
	}()

	return nil
}

func (a *Agent) handleDevicePlug(ev wolfapi.PlugDeviceEvent) {
	// Wolf reports MAJOR/MINOR as "0" when it cannot stat the device node
	// in its own container; resolve the real numbers from sysfs so both
	// the netlink event and the hwdb filename are correct.
	for _, udevEvent := range ev.UdevEvents {
		fakeudev.ResolveDevNumbers(udevEvent)
	}

	for _, entry := range ev.UdevHwDbEntries {
		filename := entry.Filename
		// Recompute "cMAJOR:MINOR" filenames that were built from the
		// zeroed-out device numbers.
		if filename == "c0:0" && len(ev.UdevEvents) > 0 {
			if maj := ev.UdevEvents[0]["MAJOR"]; maj != "" && maj != "0" {
				filename = "c" + maj + ":" + ev.UdevEvents[0]["MINOR"]
			}
		}
		if err := fakeudev.WriteHwDbEntry(UdevDataPath, filename, entry.Content); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to write udev hwdb entry %q: %w", filename, err))
		} else {
			klog.InfoS("Wrote udev hwdb entry", "file", filename, "session", ev.SessionID)
		}
	}

	for _, udevEvent := range ev.UdevEvents {
		if err := fakeudev.SendEvent(udevEvent); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to send udev event for %q: %w", udevEvent["DEVNAME"], err))
		} else {
			klog.InfoS("Sent fake udev event", "action", udevEvent["ACTION"], "devname", udevEvent["DEVNAME"], "session", ev.SessionID)
		}
	}
}

func (a *Agent) handleDeviceUnplug(ev wolfapi.UnplugDeviceEvent) {
	for _, udevEvent := range ev.UdevEvents {
		fakeudev.ResolveDevNumbers(udevEvent)
	}

	for _, entry := range ev.UdevHwDbEntries {
		filename := entry.Filename
		if filename == "c0:0" && len(ev.UdevEvents) > 0 {
			if maj := ev.UdevEvents[0]["MAJOR"]; maj != "" && maj != "0" {
				filename = "c" + maj + ":" + ev.UdevEvents[0]["MINOR"]
			}
		}
		if err := fakeudev.RemoveHwDbEntry(UdevDataPath, filename); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to remove udev hwdb entry %q: %w", filename, err))
		}
	}

	for _, udevEvent := range ev.UdevEvents {
		udevEvent["ACTION"] = "remove"
		if err := fakeudev.SendEvent(udevEvent); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to send udev remove event for %q: %w", udevEvent["DEVNAME"], err))
		} else {
			klog.InfoS("Sent fake udev remove event", "devname", udevEvent["DEVNAME"], "session", ev.SessionID)
		}
	}
}
