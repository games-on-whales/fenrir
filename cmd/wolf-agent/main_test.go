package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"games-on-whales.github.io/direwolf/pkg/util"
	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if tok, err := readToken(write("ok", "abc123\n")); err != nil || tok != "abc123" {
		t.Errorf("readToken(ok) = %q, %v; want abc123, nil", tok, err)
	}
	for name, path := range map[string]string{
		"unset":      "",
		"missing":    filepath.Join(dir, "nope"),
		"empty":      write("empty", ""),
		"whitespace": write("ws", " \n\t"),
		"directory":  dir,
	} {
		if tok, err := readToken(path); err == nil {
			t.Errorf("readToken(%s) = %q, nil; want error", name, tok)
		}
	}
}

// testTokenFile writes token to a temp file and returns a tokenFile on it and
// a function that rewrites the file in place.
func testTokenFile(t *testing.T, token string) (f *tokenFile, write func(string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	write = func(tok string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(token)
	var err error
	if f, err = newTokenFile(path); err != nil {
		t.Fatal(err)
	}
	return f, write
}

// TestSelfClientSendsToken guards the in-pod agent controller: it reaches
// Wolf through this process's own authenticated proxy, so it must send the token.
func TestSelfClientSendsToken(t *testing.T) {
	srv := httptest.NewTLSServer(wolfapi.RequireBearerToken(wolfapi.StaticToken("s3cret"),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "sessions": []any{}})
		})))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	f, write := testTokenFile(t, "s3cret")
	client := selfClient(port, f)
	if _, err := client.ListSessions(context.Background()); err != nil {
		t.Errorf("with token: %v", err)
	}
	// The file is read per request, so a rewritten token is sent (XERK-1325).
	write("wrong")
	if _, err := client.ListSessions(context.Background()); err == nil {
		t.Error("with wrong token: expected error")
	}
}

// TestProxyDoesNotForwardToken checks the bearer token authenticates to
// wolf-agent only and is never handed to Wolf, which may log headers.
func TestProxyDoesNotForwardToken(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "w.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	gotAuth := make(chan string, 1)
	wolf := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth <- r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		})},
	}
	wolf.Start()
	defer wolf.Close()

	var ready atomic.Bool
	ready.Store(true)
	client := UnixHTTPClient(sock)
	f, write := testTokenFile(t, "s3cret")
	agent := httptest.NewServer(apiHandler(f, &client, &ready))
	defer agent.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, agent.URL+"/api/v1/sessions", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if auth := <-gotAuth; auth != "" {
		t.Errorf("Wolf received Authorization %q; want none", auth)
	}

	// The handler main serves checks the file per request (XERK-1325): after
	// a rewrite the old token is refused before reaching Wolf.
	write("rotated")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old token after rotation: status = %d, want 401", resp.StatusCode)
	}
}

// TestTokenFileRotation reproduces XERK-1325: the kubelet rewrites the token
// Secret's mount when the operator regenerates it, and the agent must accept
// the new token (and reject the old one) without a restart. Secret volumes
// swap a symlink to a fresh file, which is what this mimics.
func TestTokenFileRotation(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "token")
	swap := func(name, content string) {
		t.Helper()
		target := filepath.Join(dir, name)
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		tmp := link + ".tmp"
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, link); err != nil {
			t.Fatal(err)
		}
	}
	swap("v1", "old\n")

	f, err := newTokenFile(link)
	if err != nil {
		t.Fatal(err)
	}
	h := wolfapi.RequireBearerToken(f.Token, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	status := func(tok string) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := status("old"); got != http.StatusNoContent {
		t.Fatalf("old token before rotation: status %d", got)
	}
	// Same size as the old token, so only the file identity changes.
	swap("v2", "new\n")
	if got := status("new"); got != http.StatusNoContent {
		t.Errorf("new token after rotation: status %d, want 204", got)
	}
	if got := status("old"); got != http.StatusUnauthorized {
		t.Errorf("old token after rotation: status %d, want 401", got)
	}

	// A missing file fails closed rather than keeping the last good token.
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"new", "old"} {
		if got := status(tok); got != http.StatusUnauthorized {
			t.Errorf("missing token file, token %q: status %d, want 401", tok, got)
		}
	}
	swap("v3", "new\n")
	if got := status("new"); got != http.StatusNoContent {
		t.Fatalf("token file restored: status %d, want 204", got)
	}
	// So does an emptied one.
	swap("v3b", "")
	for _, tok := range []string{"new", "old"} {
		if got := status(tok); got != http.StatusUnauthorized {
			t.Errorf("empty token file, token %q: status %d, want 401", tok, got)
		}
	}
	// An in-place, same-size rewrite: same inode and, within one mtime tick,
	// the same stat, so only re-reading the content catches it.
	inPlace := filepath.Join(dir, "inplace")
	if err = os.WriteFile(inPlace, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	ip, err := newTokenFile(inPlace)
	if err != nil {
		t.Fatal(err)
	}
	if got := ip.Token(); got != "aaaa" {
		t.Fatalf("in-place: Token() = %q, want aaaa", got)
	}
	if err := os.WriteFile(inPlace, []byte("bbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ip.Token(); got != "bbbb" {
		t.Errorf("in-place rewrite: Token() = %q, want bbbb", got)
	}
	swap("v4", "again\n")
	if got := status("again"); got != http.StatusNoContent {
		t.Errorf("token file restored: status %d, want 204", got)
	}
}

// The operator regenerates the cert with the token and pins the new one at
// once, so the served cert must follow the files; a bad read keeps the last
// good cert rather than failing the handshake.
func TestCertFileFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write := func() []byte {
		t.Helper()
		certPEM, keyPEM, err := util.GenerateEphemeralCert("ECC")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(certPath, certPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return pair.Certificate[0]
	}
	served := func(f *certFile) []byte {
		t.Helper()
		c, err := f.GetCertificate(nil)
		if err != nil || c == nil {
			t.Fatalf("GetCertificate = %v, %v", c, err)
		}
		return c.Certificate[0]
	}

	first := write()
	initial, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	f := newCertFile(certPath, keyPath, &initial)
	if !bytes.Equal(served(f), first) {
		t.Error("not serving the initial cert")
	}

	// The server's TLS config, built before the rotation as main() builds it.
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tlsLn := tls.NewListener(ln, serverTLSConfig(f))
	go func() {
		for {
			c, acceptErr := tlsLn.Accept()
			if acceptErr != nil {
				return
			}
			_ = c.(*tls.Conn).HandshakeContext(context.Background()) //nolint:forcetypeassert // tls.NewListener yields *tls.Conn
			_ = c.Close()
		}
	}()

	second := write()
	if !bytes.Equal(served(f), second) {
		t.Error("still serving the old cert after rotation")
	}
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // inspecting the served cert
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !bytes.Equal(conn.(*tls.Conn).ConnectionState().PeerCertificates[0].Raw, second) { //nolint:forcetypeassert // tls.Dialer yields *tls.Conn
		t.Error("server's TLS config serves a stale cert")
	}

	// Half-swapped: a key that doesn't match the cert.
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(served(f), second) {
		t.Error("unreadable files did not fall back to the last good cert")
	}
}

// startServer serves handler via newServer on loopback, with timeouts
// shortened by tune, and returns its address.
func startServer(t *testing.T, handler http.Handler, tune func(*http.Server)) string {
	t.Helper()
	certPEM, keyPEM, err := util.GenerateEphemeralCert("ECC")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := newServer(0, handler, &tls.Config{Certificates: []tls.Certificate{cert}})
	tune(srv)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// assertClosedByServer fails unless the server closes conn: Read returns EOF
// (or a reset), not our own 5s timeout.
func assertClosedByServer(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue // e.g. a 401 before the close
		}
		if os.IsTimeout(err) {
			t.Fatalf("connection still open: %v", err)
		}
		return
	}
}

func dialTLS(t *testing.T, addr string) *tls.Conn {
	t.Helper()
	d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test server's ephemeral cert
	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn.(*tls.Conn) //nolint:forcetypeassert // tls.Dialer always returns *tls.Conn
}

// A client that connects and never completes the TLS handshake (no token
// needed) must be dropped, or connections pile up until fds run out.
func TestServerDropsSilentConnections(t *testing.T) {
	addr := startServer(t, http.NotFoundHandler(), func(s *http.Server) { s.ReadHeaderTimeout = 100 * time.Millisecond })
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertClosedByServer(t, conn)
}

// A request declaring a body it never sends must be dropped too, on any path
// (net/http drains an unread body after the handler, e.g. after a 401).
func TestServerDropsWithheldBody(t *testing.T) {
	unauthorized := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	addr := startServer(t, unauthorized, func(s *http.Server) { s.ReadTimeout = 100 * time.Millisecond })

	for name, req := range map[string]string{
		"content-length": "POST /api/v1/sessions/add HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n",
		"chunked":        "POST /livez HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			conn := dialTLS(t, addr)
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			assertClosedByServer(t, conn)
		})
	}
}

// ReadTimeout bounds reading the request only: a response may stream for much
// longer (as /api/v1/events does).
func TestServerStreamsPastReadTimeout(t *testing.T) {
	stream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("reading body: %v", err)
			return
		}
		for i := range 6 {
			fmt.Fprintf(w, "event %d\n", i)
			w.(http.Flusher).Flush() //nolint:forcetypeassert // net/http's writer flushes
			select {
			case <-r.Context().Done():
				t.Errorf("request canceled mid-stream after event %d", i)
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	addr := startServer(t, stream, func(s *http.Server) { s.ReadTimeout = 50 * time.Millisecond })

	// HTTP/2, where a read deadline firing resets the stream.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test server's ephemeral cert
		ForceAttemptHTTP2: true,
	}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://"+addr+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("served %s, want HTTP/2", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream cut off: %v (got %q)", err, body)
	}
	if got := strings.Count(string(body), "event"); got != 6 {
		t.Errorf("got %d events, want 6: %q", got, body)
	}
}

func TestServerTimeouts(t *testing.T) {
	srv := newServer(1, http.NotFoundHandler(), &tls.Config{})
	if srv.ReadHeaderTimeout <= 0 || srv.ReadHeaderTimeout > 30*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want (0, 30s]", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 || srv.ReadTimeout > 30*time.Second {
		t.Errorf("ReadTimeout = %v, want (0, 30s]", srv.ReadTimeout)
	}
	if srv.IdleTimeout <= 0 || srv.IdleTimeout > 5*time.Minute {
		t.Errorf("IdleTimeout = %v, want (0, 5m]", srv.IdleTimeout)
	}
	if idle := selfTransport().IdleConnTimeout; idle <= 0 || idle >= srv.IdleTimeout {
		t.Errorf("loopback client idles %v, want (0, server's %v)", idle, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Error("WriteTimeout would cut off the /api/v1/events stream")
	}
}
