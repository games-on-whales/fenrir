package wolfapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// RequireBearerToken wraps next so that only requests carrying
// "Authorization: Bearer <token>" reach it. Everything else gets a 401.
//
// wolf-agent proxies Wolf's unauthenticated HTTP API, which can start
// arbitrary containers, so this is the only thing standing between the
// network and Wolf. An empty token rejects every request (fail closed).
func RequireBearerToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || len(want) == 0 || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wolf-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// BearerTokenTransport is an http.RoundTripper that adds
// "Authorization: Bearer <Token>" to every request.
type BearerTokenTransport struct {
	Token string
	// Base is the underlying transport; http.DefaultTransport if nil.
	Base http.RoundTripper
}

func (t *BearerTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	// RoundTrippers must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.Token)
	return base.RoundTrip(req) //nolint:wrapcheck // RoundTrippers pass errors through; http.Client wraps them
}
