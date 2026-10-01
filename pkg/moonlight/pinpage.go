package moonlight

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"mime"
	"net/http"
	"net/netip"
	"regexp"
	"time"

	"k8s.io/klog/v2"

	v1alpha1types "games-on-whales.github.io/direwolf/pkg/api/v1alpha1"
	"games-on-whales.github.io/direwolf/pkg/generic"

	_ "embed"
)

//go:embed pin.html
var pinHTML string

var pinTemplate = template.Must(template.New("pin").Parse(pinHTML))

// Moonlight always displays a 4-digit PIN.
var pinPattern = regexp.MustCompile(`^\d{4}$`)

// DefaultPinUserHeader is the header Authentik's proxy outpost sets to the
// authenticated username.
const DefaultPinUserHeader = "X-Authentik-Username"

// PinProxySecretHeader carries the secret shared with the proxy. Authentik
// sets it through a property mapping on the provider (additionalHeaders).
const PinProxySecretHeader = "X-Direwolf-Proxy-Secret"

// MinPinProxySecretLen is the shortest ProxySecret accepted, so a placeholder
// or truncated secret file fails at startup instead of being guessable.
const MinPinProxySecretLen = 32

type PinPageOptions struct {
	// Port the pairing page listens on. 0 disables it.
	Port int

	// TrustedProxies are the source ranges allowed to assert an identity via
	// UserHeader (the Authentik outpost). Requests from anywhere else are
	// refused outright: the header is trivially forged by a direct client.
	// Only the TCP peer address is checked; X-Forwarded-For is ignored.
	TrustedProxies []netip.Prefix

	// ProxySecret must arrive in PinProxySecretHeader on every request, as
	// well as the peer being in TrustedProxies. The source check alone admits
	// every pod on a trusted node: moonlight-proxy is host-networked, so the
	// outpost's traffic arrives masqueraded to its node's address. Empty
	// refuses every request.
	ProxySecret []byte

	// UserHeader carries the authenticated username. Its value must be the
	// name of a User in the proxy's namespace. Defaults to
	// DefaultPinUserHeader.
	UserHeader string
}

type pinPageHandler struct {
	manager    *PairingManager
	users      generic.NamespacedLister[*v1alpha1types.User]
	trusted    []netip.Prefix
	secret     []byte
	userHeader string
}

// NewPinPageHandler serves the page where an authenticated user enters the PIN
// Moonlight shows during pairing. The resulting Pairing belongs to that user.
func NewPinPageHandler(
	manager *PairingManager,
	users generic.NamespacedLister[*v1alpha1types.User],
	opts PinPageOptions,
) http.Handler {
	if opts.UserHeader == "" {
		opts.UserHeader = DefaultPinUserHeader
	}
	h := &pinPageHandler{manager: manager, users: users, trusted: opts.TrustedProxies, secret: opts.ProxySecret, userHeader: opts.UserHeader}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /pin/", h.page)
	mux.HandleFunc("POST /pin/", h.submit)
	mux.Handle("GET /{$}", http.RedirectHandler("/pin/", http.StatusFound))
	return mux
}

// authenticate returns the User the request was authenticated as by the
// trusted proxy, or writes an error response and returns nil.
func (h *pinPageHandler) authenticate(w http.ResponseWriter, r *http.Request) *v1alpha1types.User {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !h.isTrusted(peer.Addr().Unmap()) {
		klog.Warningf("Pairing page: refusing request from untrusted peer %s", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil
	}
	if !h.hasProxySecret(r) {
		klog.Warningf("Pairing page: refusing request from %s without the proxy secret", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil
	}

	username := r.Header.Get(h.userHeader)
	if username == "" {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return nil
	}

	user, err := h.users.Get(username)
	if err != nil {
		klog.Warningf("Pairing page: no User %q: %s", username, err)
		http.Error(w, "no direwolf user for "+username, http.StatusForbidden)
		return nil
	}
	return user
}

func (h *pinPageHandler) hasProxySecret(r *http.Request) bool {
	if len(h.secret) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(PinProxySecretHeader)), h.secret) == 1
}

func (h *pinPageHandler) isTrusted(addr netip.Addr) bool {
	for _, p := range h.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (h *pinPageHandler) page(w http.ResponseWriter, r *http.Request) {
	user := h.authenticate(w, r)
	if user == nil {
		return
	}

	type pendingView struct {
		Secret string
		Client string
		Age    string
	}
	var pending []pendingView
	for _, p := range h.manager.PendingPairings() {
		pending = append(pending, pendingView{
			Secret: p.Secret,
			Client: p.Client,
			Age:    time.Since(p.Received).Round(time.Second).String(),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
	if err := pinTemplate.Execute(w, struct {
		User    string
		Pending []pendingView
	}{user.Name, pending}); err != nil {
		klog.Errorf("Pairing page: render: %s", err)
	}
}

func (h *pinPageHandler) submit(w http.ResponseWriter, r *http.Request) {
	user := h.authenticate(w, r)
	if user == nil {
		return
	}

	// CSRF: the Authentik session cookie rides along on cross-site requests,
	// so without this a hostile page could submit the PIN of the attacker's
	// own Moonlight client and pair it to the victim. A JSON content type
	// cannot be sent cross-origin without a CORS preflight, which we never
	// answer; Sec-Fetch-Site catches browsers that report it.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}

	var req struct {
		Pin    string `json:"pin"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !pinPattern.MatchString(req.Pin) || req.Secret == "" {
		http.Error(w, "PIN must be 4 digits", http.StatusBadRequest)
		return
	}

	if err := h.manager.PostPin(req.Secret, req.Pin, user.Name); err != nil {
		if errors.Is(err, errNoPendingPin) {
			http.Error(w, "that pairing request is no longer waiting; start pairing again in Moonlight", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte("PIN sent. Moonlight will finish pairing."))
}
