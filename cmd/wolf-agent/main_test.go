package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

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
