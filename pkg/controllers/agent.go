package controllers

import (
	"context"
	"encoding/json"
	"fmt"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

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
				default:
					continue
				}
			}
		}
	}()

	return nil
}
