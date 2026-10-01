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
// network and Wolf. token is called on every request so a rotated token takes
// effect without a restart; an empty token rejects every request (fail closed).
func RequireBearerToken(token func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := []byte(token())
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
// "Authorization: Bearer <Token()>" to every request.
type BearerTokenTransport struct {
	// Token is called per request, so a rotated token is picked up.
	Token func() string
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
	req.Header.Set("Authorization", "Bearer "+t.Token())
	return base.RoundTrip(req) //nolint:wrapcheck // RoundTrippers pass errors through; http.Client wraps them
}

// StaticToken returns a token source that always yields token.
func StaticToken(token string) func() string {
	return func() string { return token }
}
