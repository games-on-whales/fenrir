package controllers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// stoppingClient serves events and records StopSession calls.
type stoppingClient struct {
	fakeEventsClient
	stopped chan string
	err     error
}

func (f *stoppingClient) StopSession(_ context.Context, id string) error {
	f.stopped <- id
	return f.err
}

func hotplugAgent(t *testing.T, client wolfapi.Client) *Agent {
	t.Helper()
	a := NewAgent(client)
	a.inputDevPath, a.udevDataPath = t.TempDir(), t.TempDir()
	return a
}

// Wolf destroys a stopped stream's devices without unplug events, so a
// pause must drop their nodes and udev entries itself.
func TestAgentPauseClearsDevices(t *testing.T) {
	testAgentPause(t, nil)
}

// A stream Wolf did not stop still has its devices.
func TestAgentFailedPauseKeepsDevices(t *testing.T) {
	testAgentPause(t, errors.New("wolf refused"))
}

func testAgentPause(t *testing.T, stopErr error) {
	t.Helper()
	client := &stoppingClient{
		fakeEventsClient: fakeEventsClient{events: make(chan *sse.Event)},
		stopped:          make(chan string, 1),
		err:              stopErr,
	}
	a := hotplugAgent(t, client)
	for _, f := range []string{filepath.Join(a.inputDevPath, "event5"), filepath.Join(a.udevDataPath, "c13:69")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	client.events <- &sse.Event{Event: []byte(wolfapi.PauseStreamEventType), Data: []byte(`{"session_id":"42"}`)}
	// The loop is unbuffered: once this is taken, the pause is handled.
	client.events <- &sse.Event{Event: []byte("Done")}
	select {
	case id := <-client.stopped:
		if id != "42" {
			t.Errorf("stopped %q, want 42", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not stop the Wolf session")
	}
	want := 0
	if stopErr != nil {
		want = 1
	}
	for _, dir := range []string{a.inputDevPath, a.udevDataPath} {
		if entries, _ := os.ReadDir(dir); len(entries) != want {
			t.Errorf("%s has %d entries, want %d", dir, len(entries), want)
		}
	}
}

func TestAgentPlugPublishesDevice(t *testing.T) {
	a := hotplugAgent(t, nil)
	ev := wolfapi.PlugDeviceEvent{
		SessionID: "42",
		UdevEvents: []map[string]string{
			{"ACTION": "add", "DEVPATH": "/devices/virtual/input/input9"},
			{"ACTION": "add", "DEVNAME": "/dev/input/event69", "MAJOR": "13", "MINOR": "69", "DEVPATH": "/devices/virtual/input/input9/event69"},
		},
		UdevHwDbEntries: []wolfapi.UdevHwDbEntry{{Filename: "c13:69", Content: []string{"E:ID_INPUT_JOYSTICK=1"}}},
	}
	a.handleDevicePlug(ev)
	if _, err := os.Stat(filepath.Join(a.udevDataPath, "c13:69")); err != nil {
		t.Errorf("udev entry: %v", err)
	}
	node := filepath.Join(a.inputDevPath, "event69")
	st, err := os.Stat(node)
	switch {
	case errors.Is(err, os.ErrNotExist) && os.Geteuid() != 0:
		t.Skip("node creation needs CAP_MKNOD")
	case err != nil:
		t.Fatalf("node: %v", err)
	case st.Mode()&os.ModeCharDevice == 0:
		t.Errorf("%s is %v, want a char device", node, st.Mode())
	}

	a.handleDeviceUnplug(wolfapi.UnplugDeviceEvent(ev))
	if _, err := os.Stat(node); !os.IsNotExist(err) {
		t.Errorf("node after unplug: %v", err)
	}
}
