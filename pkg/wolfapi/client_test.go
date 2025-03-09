package wolfapi_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

var testURL = "https://10.128.3.0:8443"

func TestSubscribe(t *testing.T) {
	client := wolfapi.NewClient(testURL, &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	})
	if client == nil {
		t.Fatal("client is nil")
	}

	t.Log("Client created")
	ch, err := client.SubscribeToEvents(context.Background())
	if err != nil {
		t.Fatalf("SubscribeToEvents failed: %v", err)
	}

	t.Log("Subscribed to events")
	for {
		select {
		case ev := <-ch:
			t.Log("Received event")
			t.Logf("Event ID: %s", ev.ID)
			t.Logf("Event Type: %s", ev.Event)
			t.Logf("Event Data: %s", ev.Data)
			t.Logf("Event Retry: %d", ev.Retry)
			t.Logf("Event Comment: %v", ev.Comment)

			switch WolfEventType(ev.Event) {
			case PauseStreamEventType:
				t.Log("Received PauseStreamEvent")
				var pauseEvent PauseStreamEvent
				if err := json.Unmarshal([]byte(ev.Data), &pauseEvent); err != nil {
					t.Fatalf("Failed to unmarshal PauseStreamEvent: %v", err)
					continue
				}

			}

		case <-time.After(5 * time.Minute):
			t.Fatal("Timeout waiting for event")
		}
	}
}
