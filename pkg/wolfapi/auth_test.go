package wolfapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"games-on-whales.github.io/direwolf/pkg/wolfapi"
)

func TestRequireBearerToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	tests := []struct {
		name       string
		token      string
		header     string
		wantStatus int
	}{
		{"valid token", "s3cret", "Bearer s3cret", http.StatusTeapot},
		{"scheme is case-insensitive", "s3cret", "bearer s3cret", http.StatusTeapot},
		{"missing header", "s3cret", "", http.StatusUnauthorized},
		{"wrong token", "s3cret", "Bearer nope", http.StatusUnauthorized},
		{"token prefix", "s3cret", "Bearer s3cre", http.StatusUnauthorized},
		{"token with suffix", "s3cret", "Bearer s3cretX", http.StatusUnauthorized},
		{"wrong scheme", "s3cret", "Basic s3cret", http.StatusUnauthorized},
		{"bare token", "s3cret", "s3cret", http.StatusUnauthorized},
		{"empty bearer", "s3cret", "Bearer ", http.StatusUnauthorized},
		{"empty configured token fails closed", "", "Bearer ", http.StatusUnauthorized},
		{"empty configured token rejects anything", "", "Bearer x", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/add", http.NoBody)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			wolfapi.RequireBearerToken(wolfapi.StaticToken(tt.token), ok).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without WWW-Authenticate header")
			}
		})
	}
}

// TestBearerTokenTransportAuthenticates checks the client and server halves
// agree end to end, and that the transport does not mutate the caller's request.
func TestBearerTokenTransportAuthenticates(t *testing.T) {
	srv := httptest.NewServer(wolfapi.RequireBearerToken(wolfapi.StaticToken("s3cret"),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})))
	defer srv.Close()

	for _, tc := range []struct {
		token string
		want  int
	}{{"s3cret", http.StatusNoContent}, {"wrong", http.StatusUnauthorized}} {
		client := &http.Client{Transport: &wolfapi.BearerTokenTransport{Token: wolfapi.StaticToken(tc.token)}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/sessions", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("token %q: status = %d, want %d", tc.token, resp.StatusCode, tc.want)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("transport mutated the caller's request headers")
		}
	}
}
