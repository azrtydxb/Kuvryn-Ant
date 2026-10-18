package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// registry is a fake token-authenticated registry with its own realm.
type registry struct {
	srv           *httptest.Server
	cdn           *httptest.Server
	tokenCalls    atomic.Int32
	unauthorized  atomic.Int32
	manifestCalls atomic.Int32
	cdnAuth       atomic.Value // last Authorization header seen by the CDN
	realmBasic    atomic.Value // last Authorization header seen by the realm
	tokenStatus   atomic.Int32 // non-zero forces the token endpoint status
	expiresIn     int
	generation    atomic.Int32
	status        atomic.Int32 // non-zero forces the manifest status
}

func newRegistry(t *testing.T) *registry {
	r := &registry{expiresIn: 60}
	r.cdnAuth.Store("")
	r.realmBasic.Store("")
	r.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.cdnAuth.Store(req.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "blob-bytes")
	}))
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		r.tokenCalls.Add(1)
		r.realmBasic.Store(req.Header.Get("Authorization"))
		if s := r.tokenStatus.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		scope := req.URL.Query().Get("scope")
		if req.URL.Query().Get("service") != "fake-registry" || scope == "" {
			w.WriteHeader(400)
			return
		}
		n := r.generation.Add(1)
		_, _ = io.WriteString(w, `{"token":"tok-`+strconv.Itoa(int(n))+`:`+scope+`","expires_in":`+strconv.Itoa(r.expiresIn)+`}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		if s := r.status.Load(); s != 0 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(int(s))
			return
		}
		want := "Bearer tok-"
		got := req.Header.Get("Authorization")
		const repo = "/v2/library/app/"
		scope := "repository:library/app:pull"
		valid := len(got) > len(want) && got[:len(want)] == want && got[len(got)-len(scope):] == scope
		if !valid {
			r.unauthorized.Add(1)
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.srv.URL+`/token",service="fake-registry",scope="`+scope+`"`)
			w.WriteHeader(401)
			return
		}
		switch req.URL.Path {
		case repo + "manifests/1":
			r.manifestCalls.Add(1)
			_, _ = io.WriteString(w, "manifest")
		case repo + "blobs/sha256:aa":
			http.Redirect(w, req, r.cdn.URL+"/blob", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, req)
		}
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	t.Cleanup(r.cdn.Close)
	return r
}

func (r *registry) fetcher(t *testing.T, c *clock, mutate func(*Config)) *Fetcher {
	t.Helper()
	cfg := Config{Origin: r.srv.URL, AllowHTTP: true, AllowPrivate: true, RedirectOrigins: []string{r.cdn.URL}, Now: c.now}
	if mutate != nil {
		mutate(&cfg)
	}
	f, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func get(t *testing.T, f *Fetcher, path string) (int, string) {
	t.Helper()
	resp, err := f.Fetch(context.Background(), "GET", path, nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestBearerHandshakeCachesPerScopeAndExpires(t *testing.T) {
	r := newRegistry(t)
	c := &clock{t: time.Unix(1_000_000, 0)}
	f := r.fetcher(t, c, nil)
	if s, b := get(t, f, "/v2/library/app/manifests/1"); s != 200 || b != "manifest" {
		t.Fatalf("first: %d %s", s, b)
	}
	if r.tokenCalls.Load() != 1 || r.unauthorized.Load() != 1 {
		t.Fatalf("tokens=%d 401s=%d", r.tokenCalls.Load(), r.unauthorized.Load())
	}
	// Second request uses the cached token preemptively: no extra 401 or token.
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 200 {
		t.Fatal("second")
	}
	if r.tokenCalls.Load() != 1 || r.unauthorized.Load() != 1 {
		t.Fatalf("token not reused: tokens=%d 401s=%d", r.tokenCalls.Load(), r.unauthorized.Load())
	}
	// After expiry a new token is requested.
	c.advance(2 * time.Minute)
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 200 {
		t.Fatal("after expiry")
	}
	if r.tokenCalls.Load() != 2 {
		t.Fatalf("expired token not refreshed: %d", r.tokenCalls.Load())
	}
}

func TestBearerRevokedTokenIsRefetchedOnce(t *testing.T) {
	r := newRegistry(t)
	c := &clock{t: time.Unix(1_000_000, 0)}
	f := r.fetcher(t, c, nil)
	get(t, f, "/v2/library/app/manifests/1")
	// Poison the cache: the registry now rejects the cached token's scope text.
	f.tokens.mu.Lock()
	for k, v := range f.tokens.tokens {
		v.value = "stale"
		f.tokens.tokens[k] = v
	}
	f.tokens.mu.Unlock()
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 200 {
		t.Fatalf("revoked token not recovered: %d", s)
	}
	if r.tokenCalls.Load() != 2 {
		t.Fatalf("tokens=%d", r.tokenCalls.Load())
	}
}

func TestBearerNeverLeaksAcrossOrigins(t *testing.T) {
	r := newRegistry(t)
	c := &clock{t: time.Unix(1_000_000, 0)}
	f := r.fetcher(t, c, func(cfg *Config) {
		cfg.Credentials = http.Header{"Authorization": {"Basic dXNlcjpwdw=="}}
	})
	if s, b := get(t, f, "/v2/library/app/blobs/sha256:aa"); s != 200 || b != "blob-bytes" {
		t.Fatalf("blob via CDN redirect: %d %s", s, b)
	}
	if got := r.cdnAuth.Load().(string); got != "" {
		t.Fatalf("Authorization leaked to the CDN origin: %q", got)
	}
	// The configured basic credential went to the (allowed) realm, never as-is
	// to the CDN, and the registry saw a bearer token, not the basic secret.
	if got := r.realmBasic.Load().(string); got != "Basic dXNlcjpwdw==" {
		t.Fatalf("realm credential: %q", got)
	}
}

func TestBearerRealmMustBeAnAllowedOrigin(t *testing.T) {
	r := newRegistry(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Error("token requested from a realm outside the source's origins")
	}))
	defer other.Close()
	rogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+other.URL+`/token",service="x"`)
		w.WriteHeader(401)
	}))
	defer rogue.Close()
	c := &clock{t: time.Unix(1_000_000, 0)}
	f, err := New(Config{Origin: rogue.URL, AllowHTTP: true, AllowPrivate: true, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_ = r
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 401 {
		t.Fatalf("status %d", s)
	}
}

func TestBearerAuthFailureIsNotCachedLong(t *testing.T) {
	r := newRegistry(t)
	c := &clock{t: time.Unix(1_000_000, 0)}
	f := r.fetcher(t, c, nil)
	r.tokenStatus.Store(401)
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 401 {
		t.Fatalf("denied token must surface the 401, got %d", s)
	}
	// Immediately retrying is damped (no token hammering)...
	before := r.tokenCalls.Load()
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 401 || r.tokenCalls.Load() != before {
		t.Fatalf("failure not briefly damped: %d calls=%d/%d", s, r.tokenCalls.Load(), before)
	}
	// ...but a corrected upstream works within seconds.
	r.tokenStatus.Store(0)
	c.advance(authFailureTTL + time.Second)
	if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 200 {
		t.Fatalf("recovery: %d", s)
	}
}

func TestBearerTokenFetchesAreCoalesced(t *testing.T) {
	r := newRegistry(t)
	c := &clock{t: time.Unix(1_000_000, 0)}
	f := r.fetcher(t, c, nil)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s, _ := get(t, f, "/v2/library/app/manifests/1"); s != 200 {
				t.Errorf("status %d", s)
			}
		}()
	}
	wg.Wait()
	if n := r.tokenCalls.Load(); n > 2 {
		t.Fatalf("token fetches not coalesced: %d", n)
	}
}

func TestRateLimitIsSurfacedNotRetried(t *testing.T) {
	r := newRegistry(t)
	c := &clock{t: time.Unix(1_000_000, 0)}
	f := r.fetcher(t, c, nil)
	r.status.Store(429)
	_, err := f.Fetch(context.Background(), "GET", "/v2/library/app/manifests/1", nil)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("want RateLimitError(7s), got %v", err)
	}
	if f.State().Failures != 0 {
		t.Fatal("a rate limit must not trip the circuit breaker")
	}
}

func TestParseChallenge(t *testing.T) {
	c, ok := parseChallenge([]string{`Basic realm="x"`, `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull,push"`})
	if !ok || c.realm != "https://auth.docker.io/token" || c.service != "registry.docker.io" || c.scope != "repository:library/alpine:pull,push" {
		t.Fatalf("%+v %v", c, ok)
	}
	if _, ok := parseChallenge([]string{`Basic realm="x"`}); ok {
		t.Fatal("basic is not a bearer challenge")
	}
	if c, ok := parseChallenge([]string{`bearer realm="r", service=s`}); !ok || c.service != "s" {
		t.Fatalf("%+v", c)
	}
}

func TestLibraryPath(t *testing.T) {
	for _, tc := range []struct{ host, in, out string }{
		{"registry-1.docker.io", "/v2/alpine/manifests/3", "/v2/library/alpine/manifests/3"},
		{"registry-1.docker.io", "/v2/library/alpine/manifests/3", "/v2/library/alpine/manifests/3"},
		{"registry-1.docker.io", "/v2/grafana/grafana/manifests/3", "/v2/grafana/grafana/manifests/3"},
		{"registry-1.docker.io", "/v2/alpine/blobs/sha256:aa", "/v2/library/alpine/blobs/sha256:aa"},
		{"registry.k8s.io", "/v2/pause/manifests/3.9", "/v2/pause/manifests/3.9"},
		{"ghcr.io", "/v2/alpine/manifests/3", "/v2/alpine/manifests/3"},
	} {
		if got := libraryPath(tc.host, tc.in); got != tc.out {
			t.Errorf("%s %s: %s", tc.host, tc.in, got)
		}
	}
}
