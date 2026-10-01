package moonlight

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"k8s.io/klog/v2"
)

// captureKlog sends klog output to a buffer for the rest of the test.
func captureKlog(t *testing.T) *lockedBuffer {
	t.Helper()
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	for k, v := range map[string]string{"logtostderr": "false", "alsologtostderr": "false", "stderrthreshold": "FATAL"} {
		if err := fs.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	buf := &lockedBuffer{}
	klog.SetOutput(buf)
	t.Cleanup(func() {
		klog.Flush()
		klog.SetOutput(os.Stderr)
		_ = fs.Set("logtostderr", "true")
		_ = fs.Set("stderrthreshold", "ERROR")
	})
	return buf
}

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

// Every secret query parameter is redacted in what loggingMiddleware logs,
// while the handler still sees the real value.
func TestLoggingMiddlewareRedactsSecrets(t *testing.T) {
	buf := captureKlog(t)
	// Spelled out, not secretQueryParams: dropping a name from that list
	// must fail this test.
	secrets := []string{"rikey", "rikeyid", "salt", "clientcert", "clientchallenge", "serverchallengeresp", "clientpairingsecret", "pin"}
	q := url.Values{"appid": {"firefox"}}
	for i, k := range secrets {
		q.Set(k, "SECRETVALUE"+string(rune('A'+i)))
	}

	var seen url.Values
	h := loggingMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Query() }))
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/launch?"+q.Encode(), http.NoBody)
	h.ServeHTTP(httptest.NewRecorder(), r)
	klog.Flush()

	logged := buf.String()
	if !strings.Contains(logged, "appid:[firefox]") {
		t.Fatalf("request not logged with its query: %q", logged)
	}
	for _, k := range secrets {
		if strings.Contains(logged, q.Get(k)) {
			t.Errorf("log contains %s's value: %q", k, logged)
		}
		if seen.Get(k) != q.Get(k) {
			t.Errorf("handler saw %s=%q, want the real value", k, seen.Get(k))
		}
	}
}

// Response bodies (the server's pairing secrets) are not logged.
func TestSendXMLDoesNotLogBody(t *testing.T) {
	buf := captureKlog(t)
	sendXML(httptest.NewRecorder(), PairingResponse{Response: Response{StatusCode: 200}, PairingSecret: "SERVERSECRET"})
	klog.Flush()
	if strings.Contains(buf.String(), "SERVERSECRET") {
		t.Errorf("response body logged: %q", buf.String())
	}
}
