package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type authFunc func(context.Context, string) (Principal, error)

func (f authFunc) Authenticate(c context.Context, s string) (Principal, error) { return f(c, s) }
func request(h http.Handler, remote, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/protected", nil)
	r.RemoteAddr = remote
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Forwarded-For", "spoofed")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestBearerRedactionAndContext(t *testing.T) {
	calls := 0
	b := NewBearer(authFunc(func(ctx context.Context, raw string) (Principal, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded authentication")
		}
		if raw != "valid" {
			return Principal{}, errors.New("sensitive " + raw)
		}
		return Principal{InstanceID: "instance", PrincipalID: "user", TokenID: "token"}, nil
	}))
	h := b.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFromContext(r.Context())
		if !ok || p.PrincipalID != "user" {
			t.Error("missing principal")
		}
		w.WriteHeader(204)
	}))
	for _, token := range []string{"wrong", "expired", "revoked", ""} {
		w := request(h, "127.0.0.1:1000", token)
		if w.Code != 401 || strings.Contains(w.Body.String(), "sensitive") || (token != "" && strings.Contains(w.Body.String(), token)) || w.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("invalid rejection: %d %s", w.Code, w.Body.String())
		}
	}
	if w := request(h, "127.0.0.1:1000", "valid"); w.Code != 204 {
		t.Fatalf("valid rejected: %d", w.Code)
	}
	if calls != 4 {
		t.Fatalf("calls: %d", calls)
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Fatal("ambient principal")
	}
}
func TestBearerThrottleWindowAndBoundedMemory(t *testing.T) {
	calls := 0
	now := time.Unix(100, 0)
	b := NewBearer(authFunc(func(context.Context, string) (Principal, error) { calls++; return Principal{}, ErrUnauthenticated }))
	b.maxFailures = 2
	b.maxEntries = 2
	b.now = func() time.Time { return now }
	h := b.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthenticated handler") }))
	for range 2 {
		if request(h, "127.0.0.1:1", "bad").Code != 401 {
			t.Fatal("attempt rejected early")
		}
	}
	if w := request(h, "127.0.0.1:9999", "bad"); w.Code != 429 || calls != 2 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("missing per-IP throttle: %d calls=%d", w.Code, calls)
	}
	now = now.Add(time.Minute)
	if request(h, "127.0.0.1:1", "bad").Code != 401 {
		t.Fatal("window did not reset")
	}
	for _, peer := range []string{"127.0.0.2:1", "127.0.0.3:1", "127.0.0.4:1"} {
		request(h, peer, "bad")
	}
	if len(b.attempts) > 2 {
		t.Fatal("unbounded source map")
	}
}
func TestBearerConcurrentReservationAndHeaderValidation(t *testing.T) {
	b := NewBearer(authFunc(func(context.Context, string) (Principal, error) {
		t.Fatal("malformed auth reached backend")
		return Principal{}, nil
	}))
	b.maxFailures = 1
	b.maxEntries = 1
	e, ok := b.reserve("one")
	if !ok {
		t.Fatal("first reservation denied")
	}
	if _, ok := b.reserve("one"); ok {
		t.Fatal("concurrent attempts bypassed throttle")
	}
	if _, ok := b.reserve("two"); ok {
		t.Fatal("evicted active source")
	}
	b.finish(e, true)
	h := b.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("malformed accepted") }))
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Add("Authorization", "Bearer one")
	r.Header.Add("Authorization", "Bearer two")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("duplicate header status: %d", w.Code)
	}
}

func TestBearerUnavailableDoesNotConsumeFailureBudget(t *testing.T) {
	b := NewBearer(authFunc(func(context.Context, string) (Principal, error) { return Principal{}, ErrUnavailable }))
	b.maxFailures = 1
	h := b.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unavailable authenticator authorized request") }))
	for range 3 {
		w := request(h, "127.0.0.1:1", "private-token")
		if w.Code != 503 || strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), ErrUnavailable.Error()) {
			t.Fatalf("unavailable response leaked details or consumed budget: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestTrustedBrowserPrincipalCannotOverrideAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		p            Principal
		want         int
	}{
		{"browser", "", Principal{InstanceID: "i", PrincipalID: "u"}, 204},
		{"missing instance", "", Principal{PrincipalID: "u"}, 401},
		{"token context", "", Principal{InstanceID: "i", PrincipalID: "u", TokenID: "t"}, 401},
		{"scoped context", "", Principal{InstanceID: "i", PrincipalID: "u", Scopes: []Scope{}}, 401},
		{"invalid explicit bearer", "Bearer invalid", Principal{InstanceID: "i", PrincipalID: "u"}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewBearer(authFunc(func(context.Context, string) (Principal, error) { return Principal{}, errors.New("invalid") })).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			r := httptest.NewRequest("GET", "/api/v1/feeds", nil).WithContext(WithAuthenticatedPrincipal(context.Background(), tc.p))
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
}
