package controllers

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r3labs/sse/v2"
	"k8s.io/klog/v2"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

// lockedBuffer is a bytes.Buffer safe for klog's writer and the test reader.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // io.Writer passthrough
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeEventsClient serves events to SubscribeToEvents; nothing else is used.
type fakeEventsClient struct {
	wolfapi.Client
	events chan *sse.Event
}

func (f *fakeEventsClient) SubscribeToEvents(context.Context) (<-chan *sse.Event, error) {
	return f.events, nil
}

// Wolf's session events carry the stream's AES key and IV; the agent's event
// loop must never log them.
func TestAgentEventLoopOmitsEventData(t *testing.T) {
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	for k, v := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL"} {
		if err := fs.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	out := &lockedBuffer{}
	klog.SetOutput(out)
	t.Cleanup(func() {
		klog.Flush()
		klog.SetOutput(os.Stderr)
		_ = fs.Set("logtostderr", "true")
		_ = fs.Set("stderrthreshold", "ERROR")
	})

	client := &fakeEventsClient{events: make(chan *sse.Event)}
	agent := NewAgent(client)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := agent.Run(ctx); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"aes_key":"SECRETKEY0123","aes_iv":"SECRETIV42"}`)
	client.events <- &sse.Event{Event: []byte("StreamSession"), Data: data}
	// A second event is only received once the first has been logged.
	client.events <- &sse.Event{Event: []byte("Done")}

	var logged string
	for range 200 {
		klog.Flush()
		if logged = out.String(); strings.Contains(logged, "Done") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(logged, "StreamSession") {
		t.Fatalf("event not logged at all: %q", logged)
	}
	// Also in the encodings a format verb would print []byte in.
	// "83 69 67 82 69 84" is %v of "SECRET".
	for _, secret := range []string{"SECRETKEY0123", "SECRETIV42", hex.EncodeToString([]byte("SECRETKEY0123")), "83 69 67 82 69 84"} {
		if strings.Contains(logged, secret) {
			t.Errorf("event data logged (%q): %q", secret, logged)
		}
	}
}
