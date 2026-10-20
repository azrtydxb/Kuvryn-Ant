package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// hubDenying mimics Docker Hub for a repository that does not exist (or that
// the caller may not pull): the token realm happily issues a token, and the
// registry then answers 401 instead of 404.
func hubDenying(t *testing.T, challengeError, body string) (*httptest.Server, *httptest.Server, *atomic.Int32) {
	t.Helper()
	tokens := &atomic.Int32{}
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		tokens.Add(1)
		_, _ = io.WriteString(w, `{"token":"anon-token","expires_in":300}`)
	}))
	t.Cleanup(auth.Close)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		scope := `repository:nobody/missing:pull`
		challenge := `Bearer realm="` + auth.URL + `/token",service="registry.docker.io",scope="` + scope + `"`
		if req.Header.Get("Authorization") == "Bearer anon-token" {
			if challengeError != "" {
				challenge += `,error="` + challengeError + `"`
			}
			w.Header().Set("WWW-Authenticate", challenge)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(401)
	}))
	t.Cleanup(registry.Close)
	return registry, auth, tokens
}

const hubUnauthorizedBody = `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required","detail":[{"Type":"repository","Class":"","Name":"nobody/missing","Action":"pull"}]}]}`

func hubFetcher(t *testing.T, registry, auth *httptest.Server) *Fetcher {
	t.Helper()
	f, err := New(Config{Origin: registry.URL, AllowHTTP: true, AllowPrivate: true, RedirectOrigins: []string{auth.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

// Docker Hub answers 401 (not 404) for a repository that does not exist, even
// after a valid token handshake. That is a negative answer about the
// repository, not an authentication failure of the source: it must surface as
// 404 so resolution falls through and the source is not marked unhealthy.
func TestBearerScopeDenialAfterValidTokenIsNotFound(t *testing.T) {
	for name, tc := range map[string]struct{ challengeError, body string }{
		"insufficient_scope":  {"insufficient_scope", hubUnauthorizedBody},
		"UNAUTHORIZED body":   {"", hubUnauthorizedBody},
		"scope error no body": {"insufficient_scope", ""},
	} {
		t.Run(name, func(t *testing.T) {
			registry, auth, tokens := hubDenying(t, tc.challengeError, tc.body)
			f := hubFetcher(t, registry, auth)
			resp, err := f.Fetch(t.Context(), "GET", "/v2/nobody/missing/manifests/latest", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status %d, want 404", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") != "" {
				t.Fatal("upstream challenge must not be forwarded")
			}
			if tokens.Load() != 1 {
				t.Fatalf("token requests: %d", tokens.Load())
			}
		})
	}
}

// Real credential failures must keep surfacing as 401 so they still count
// against the source's health: a rejected token request, and a token the
// registry reports as invalid or expired.
func TestBearerCredentialFailuresStayUnauthorized(t *testing.T) {
	t.Run("realm rejects credential", func(t *testing.T) {
		registry, auth, _ := dockerHubShaped(t)
		f, err := New(Config{Origin: registry.URL, AllowHTTP: true, AllowPrivate: true, RedirectOrigins: []string{auth.URL}, Credentials: http.Header{"Authorization": {"Basic d3Jvbmc6Y3JlZA=="}}})
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if s, _ := get(t, f, "/v2/library/alpine/manifests/3.20"); s != 401 {
			t.Fatalf("status %d, want 401", s)
		}
	})
	t.Run("invalid_token", func(t *testing.T) {
		registry, auth, _ := hubDenying(t, "invalid_token", hubUnauthorizedBody)
		f := hubFetcher(t, registry, auth)
		if s, _ := get(t, f, "/v2/nobody/missing/manifests/latest"); s != 401 {
			t.Fatalf("status %d, want 401", s)
		}
	})
	t.Run("non repository path", func(t *testing.T) {
		registry, auth, _ := hubDenying(t, "insufficient_scope", hubUnauthorizedBody)
		f := hubFetcher(t, registry, auth)
		if s, b := get(t, f, "/v2/_catalog"); s != 401 || !strings.Contains(b, "UNAUTHORIZED") {
			t.Fatalf("status %d %q", s, b)
		}
	})
}
