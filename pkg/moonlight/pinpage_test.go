package moonlight

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"
	"games-on-whales.github.io/direwolf/pkg/util"
)

const (
	testNamespace = "direwolf"
	trustedPeer   = "10.0.0.5:40000"
	proxySecret   = "0123456789abcdef0123456789abcdef"
)

func newTestPinPage(t *testing.T, users ...string) (*PairingManager, http.Handler) {
	t.Helper()
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, name := range users {
		if err := indexer.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewPairingManager(tls.Certificate{}, nil)
	handler := NewPinPageHandler(
		manager,
		generic.NewLister[*v1alpha1types.User](indexer).Namespaced(testNamespace),
		PinPageOptions{
			TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
			ProxySecret:    []byte(proxySecret),
		},
	)
	return manager, handler
}

// startPhase1 runs pairPhase1 the way /pair would and waits until it is
// blocked on the pairing page. The returned channel yields its response.
func startPhase1(ctx context.Context, t *testing.T, m *PairingManager, cacheKey string, salt []byte) (secret string, done <-chan PairingResponse) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certHex := hex.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

	resp := make(chan PairingResponse, 1)
	go func() { resp <- m.pairPhase1(ctx, cacheKey, hex.EncodeToString(salt), certHex) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range m.PendingPairings() {
			if p.Client == cacheKey {
				return p.Secret, resp
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pairing request never became pending")
	return "", nil
}

func postPin(h http.Handler, peer, user, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/pin/", strings.NewReader(body))
	req.RemoteAddr = peer
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PinProxySecretHeader, proxySecret)
	if user != "" {
		req.Header.Set(DefaultPinUserHeader, user)
	}
	for _, f := range mutate {
		f(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPinHandoffMapsPairingToAuthenticatedUser(t *testing.T) {
	m, h := newTestPinPage(t, "alice")
	salt := bytes.Repeat([]byte{0x42}, 16)
	secret, done := startPhase1(t.Context(), t, m, "client@192.0.2.10", salt)

	// The page lists the waiting request for the signed-in user.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/pin/", http.NoBody)
	req.RemoteAddr = trustedPeer
	req.Header.Set(PinProxySecretHeader, proxySecret)
	req.Header.Set(DefaultPinUserHeader, "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("GET /pin/ = %d, body missing secret or user:\n%s", rec.Code, rec.Body)
	}

	if rec := postPin(h, trustedPeer, "alice", `{"pin":"1234","secret":"`+secret+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST /pin/ = %d %s", rec.Code, rec.Body)
	}

	select {
	case resp := <-done:
		if resp.Paired != 1 {
			t.Fatalf("phase 1 failed: %+v", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("phase 1 did not resume after the PIN was posted")
	}

	v, ok := m.PairingCache.Load("client@192.0.2.10")
	if !ok {
		t.Fatal("no pairing cache entry after phase 1")
	}
	entry, ok := v.(pendingPairCacheEntry)
	if !ok {
		t.Fatalf("pairing cache entry has type %T", v)
	}
	if entry.Username != "alice" {
		t.Errorf("Username = %q, want alice", entry.Username)
	}
	if want := util.Hash(salt, []byte("1234"))[:16]; !bytes.Equal(entry.AESKey, want) {
		t.Errorf("AESKey not derived from the posted PIN")
	}

	// The request is consumed: a second PIN for it is refused.
	if rec := postPin(h, trustedPeer, "alice", `{"pin":"9999","secret":"`+secret+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("second POST = %d, want 404", rec.Code)
	}
}

func TestPinPageRejects(t *testing.T) {
	cases := []struct {
		name   string
		peer   string
		user   string
		body   string // %s is replaced by the pending secret
		mutate func(*http.Request)
		want   int
	}{
		{name: "untrusted peer forging the header", peer: "192.0.2.99:1234", user: "alice", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		{name: "forwarded-for does not make a peer trusted", peer: "192.0.2.99:1234", user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set("X-Forwarded-For", "10.0.0.5") }, want: http.StatusForbidden},
		{name: "trusted peer without the proxy secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Del(PinProxySecretHeader) }, want: http.StatusForbidden},
		{name: "trusted peer with a wrong proxy secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set(PinProxySecretHeader, proxySecret[:31]+"x") }, want: http.StatusForbidden},
		{name: "trusted peer with a truncated proxy secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set(PinProxySecretHeader, proxySecret[:31]) }, want: http.StatusForbidden},
		{name: "proxy secret from an untrusted peer", peer: "192.0.2.99:1234", user: "alice", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		{name: "no identity header", peer: trustedPeer, body: `{"pin":"1234","secret":"%s"}`, want: http.StatusUnauthorized},
		{name: "identity without a User", peer: trustedPeer, user: "mallory", body: `{"pin":"1234","secret":"%s"}`, want: http.StatusForbidden},
		{name: "form post (CSRF)", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
		{name: "cross-site fetch", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"%s"}`,
			mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, want: http.StatusForbidden},
		{name: "non-numeric pin", peer: trustedPeer, user: "alice", body: `{"pin":"12a4","secret":"%s"}`, want: http.StatusBadRequest},
		{name: "unknown secret", peer: trustedPeer, user: "alice", body: `{"pin":"1234","secret":"deadbeef"}`, want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, h := newTestPinPage(t, "alice")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			secret, done := startPhase1(ctx, t, m, "client@192.0.2.10", make([]byte, 16))

			var mutate []func(*http.Request)
			if tc.mutate != nil {
				mutate = append(mutate, tc.mutate)
			}
			body := strings.ReplaceAll(tc.body, "%s", secret)
			if rec := postPin(h, tc.peer, tc.user, body, mutate...); rec.Code != tc.want {
				t.Fatalf("POST = %d %s, want %d", rec.Code, rec.Body, tc.want)
			}

			// The PIN must not have reached the handshake.
			if len(m.PendingPairings()) != 1 {
				t.Fatal("rejected request consumed the pending pairing")
			}
			cancel()
			if resp := <-done; resp.Paired != 0 {
				t.Fatalf("phase 1 completed after a rejected PIN: %+v", resp)
			}
		})
	}
}

func TestPinPageWithoutProxySecretRefusesEverything(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	// alice exists, so a 403 here can only come from the secret check.
	if err := indexer.Add(&v1alpha1types.User{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: testNamespace}}); err != nil {
		t.Fatal(err)
	}
	h := NewPinPageHandler(
		NewPairingManager(tls.Certificate{}, nil),
		generic.NewLister[*v1alpha1types.User](indexer).Namespaced(testNamespace),
		PinPageOptions{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}},
	)
	for _, sent := range []string{"", proxySecret} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/pin/", http.NoBody)
		req.RemoteAddr = trustedPeer
		req.Header.Set(PinProxySecretHeader, sent)
		req.Header.Set(DefaultPinUserHeader, "alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("secret %q: GET /pin/ = %d, want 403", sent, rec.Code)
		}
	}
}

func TestPendingPairingDroppedWhenClientGivesUp(t *testing.T) {
	m, _ := newTestPinPage(t)
	ctx, cancel := context.WithCancel(t.Context())
	_, done := startPhase1(ctx, t, m, "client@192.0.2.10", make([]byte, 16))

	cancel()
	if resp := <-done; resp.Paired != 0 {
		t.Fatalf("phase 1 succeeded without a PIN: %+v", resp)
	}
	if p := m.PendingPairings(); len(p) != 0 {
		t.Fatalf("pending pairings after cancel: %+v", p)
	}
}
