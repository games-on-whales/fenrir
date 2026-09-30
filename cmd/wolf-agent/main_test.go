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

// TestSelfClientSendsToken guards the in-pod agent controller: it reaches
// Wolf through this process's own authenticated proxy, so it must send the token.
func TestSelfClientSendsToken(t *testing.T) {
	srv := httptest.NewTLSServer(wolfapi.RequireBearerToken("s3cret",
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

	if _, err := selfClient(port, "s3cret").ListSessions(context.Background()); err != nil {
		t.Errorf("with token: %v", err)
	}
	if _, err := selfClient(port, "wrong").ListSessions(context.Background()); err == nil {
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
	agent := httptest.NewServer(wolfapi.RequireBearerToken("s3cret", proxyHandler(&client, &ready)))
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
}
