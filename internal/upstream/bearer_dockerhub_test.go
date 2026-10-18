package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// dockerHubShaped mimics registry-1.docker.io + auth.docker.io: the registry
// and the token realm are different origins, the realm demands the configured
// Basic credential, and the registry only accepts the issued bearer token.
func dockerHubShaped(t *testing.T) (registry, auth *httptest.Server, tokenScope *atomic.Value) {
	t.Helper()
	tokenScope = &atomic.Value{}
	auth = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Basic dXNlcjpwYXQ=" {
			w.WriteHeader(401)
			return
		}
		tokenScope.Store(req.URL.Query().Get("scope"))
		_, _ = io.WriteString(w, `{"token":"hub-token","access_token":"hub-token","expires_in":300}`)
	}))
	t.Cleanup(auth.Close)
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer hub-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+auth.URL+`/token",service="registry.docker.io"`)
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, "alpine-manifest")
	}))
	t.Cleanup(registry.Close)
	return registry, auth, tokenScope
}

func TestBearerDockerHubShapedRealmWithBasicCredential(t *testing.T) {
	registry, auth, scope := dockerHubShaped(t)
	f, err := New(Config{
		Origin: registry.URL, AllowHTTP: true, AllowPrivate: true,
		RedirectOrigins: []string{auth.URL},
		Credentials:     http.Header{"Authorization": {"Basic dXNlcjpwYXQ="}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if s, b := get(t, f, "/v2/library/alpine/manifests/3.20"); s != 200 || b != "alpine-manifest" {
		t.Fatalf("got %d %q", s, b)
	}
	if got, _ := scope.Load().(string); got != "repository:library/alpine:pull" {
		t.Fatalf("scope %q", got)
	}
}

// Without the token realm listed as an allowed origin the handshake is refused
// by policy (surfaced as the registry's 401) and the credential never reaches
// the realm. This is the misconfiguration behind the conformance harness'
// Docker Hub 503s: its source listed only the blob CDN, not auth.docker.io.
func TestBearerDockerHubShapedRealmNotConfiguredIsRefused(t *testing.T) {
	registry, _, scope := dockerHubShaped(t)
	f, err := New(Config{
		Origin: registry.URL, AllowHTTP: true, AllowPrivate: true,
		Credentials: http.Header{"Authorization": {"Basic dXNlcjpwYXQ="}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if s, _ := get(t, f, "/v2/library/alpine/manifests/3.20"); s != 401 {
		t.Fatalf("status %d", s)
	}
	if scope.Load() != nil {
		t.Fatal("realm was contacted although not an allowed origin")
	}
}
