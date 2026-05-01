package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

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

	// Tracks which sessions are currently initializing for our app
	// Will need to change this for when wolf-agent moves into daemonsets
	pendingSessions := make(map[string]bool)
	targetAppID := os.Getenv("DIREWOLF_APP_ID") // App UUID in Wolf

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

				switch wolfapi.WolfEventType(ev.Event) {
				case wolfapi.StreamSessionEventType:
					var streamEvent wolfapi.StreamSessionEvent
					if err := json.Unmarshal(ev.Data, &streamEvent); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to unmarshal stream session event: %w", err))
						continue
					}

					// Only track sessions intended for this Pod's application
					if streamEvent.AppID == targetAppID || targetAppID == "" {
						klog.Infof("Marking session %s as pending for lobby join", streamEvent.ClientID)
						pendingSessions[streamEvent.ClientID] = true
					}

				case wolfapi.ResumeStreamEventType:
					var resumeEvent wolfapi.ResumeStreamEvent
					if err := json.Unmarshal(ev.Data, &resumeEvent); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to unmarshal resume stream event: %w", err))
						continue
					}

					if !pendingSessions[resumeEvent.SessionID] {
						continue
					}

					klog.Infof("RTSP PLAY received for session %s, joining lobby...", resumeEvent.SessionID)

					// I'm putting this here as the easiest method to let Wolf finish internal RTSP state transition
					// Hopefully, next iteration doesn't have this issue
					time.Sleep(200 * time.Millisecond)

					lobbies, err := a.WolfClient.ListLobbies(ctx)
					if err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to list lobbies: %w", err))
						continue
					}

					targetAppName := os.Getenv("DIREWOLF_APP")
					var lobbyID string
					for _, l := range lobbies {
						if l.Name == targetAppName {
							lobbyID = l.ID
							break
						}
					}

					if lobbyID == "" {
						utilruntime.HandleError(fmt.Errorf("no lobby found for app %s", targetAppName))
						continue
					}

					klog.Infof("Attempting to join session %s to lobby %s", resumeEvent.SessionID, lobbyID)

					// Retry join with backoff
					var joinErr error
					for attempt := 0; attempt < 5; attempt++ {
						if attempt > 0 {
							backoff := time.Duration(attempt*200) * time.Millisecond
							klog.Infof("Retrying lobby join for session %s after %v (attempt %d)", resumeEvent.SessionID, backoff, attempt+1)
							time.Sleep(backoff)
						}
						joinErr = a.WolfClient.JoinLobby(ctx, wolfapi.JoinLobbyRequest{
							LobbyID:            lobbyID,
							MoonlightSessionID: resumeEvent.SessionID,
						})
						if joinErr == nil {
							break
						}
						klog.Warningf("Join lobby attempt %d failed for session %s: %v", attempt+1, resumeEvent.SessionID, joinErr)
					}

					if joinErr != nil {
						utilruntime.HandleError(fmt.Errorf("agent failed to join lobby after all retries: %w", joinErr))
					} else {
						klog.Infof("Successfully joined session %s to lobby %s", resumeEvent.SessionID, lobbyID)
						delete(pendingSessions, resumeEvent.SessionID)
					}
				case wolfapi.PauseStreamEventType:
					var pauseEvent wolfapi.PauseStreamEvent
					if err := json.Unmarshal(ev.Data, &pauseEvent); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to unmarshal pause stream event: %w", err))
						continue
					}

					delete(pendingSessions, pauseEvent.SessionID)
					if err := a.WolfClient.StopSession(ctx, pauseEvent.SessionID); err != nil {
						utilruntime.HandleError(fmt.Errorf("failed to stop session: %w", err))
						continue
					}
				}
			}
		}
	}()

	return nil
}
