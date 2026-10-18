package identity

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func mustProxies(t *testing.T, s string) []netip.Prefix {
	t.Helper()
	p, err := ParseTrustedProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientAddrTrustedProxies(t *testing.T) {
	trusted := mustProxies(t, "10.0.0.0/8, 192.168.1.5")
	cases := []struct {
		name, remote, xff, real, want string
		trust                         bool
	}{
		{"untrusted peer ignores headers", "203.0.113.9:1", "1.2.3.4", "5.6.7.8", "203.0.113.9", true},
		{"no trusted set ignores headers", "10.1.1.1:1", "1.2.3.4", "", "10.1.1.1", false},
		{"trusted peer uses xff", "10.1.1.1:1", "198.51.100.7", "", "198.51.100.7", true},
		{"spoofed prefix skipped from right", "10.1.1.1:1", "9.9.9.9, 198.51.100.7, 10.2.2.2", "", "198.51.100.7", true},
		{"single bare address trusted", "192.168.1.5:1", "198.51.100.8", "", "198.51.100.8", true},
		{"x-real-ip fallback", "10.1.1.1:1", "", "198.51.100.9", "198.51.100.9", true},
		{"malformed chain falls back to peer", "10.1.1.1:1", "garbage", "", "10.1.1.1", true},
		{"all hops trusted gives peer", "10.1.1.1:1", "10.3.3.3", "", "10.1.1.1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if c.real != "" {
				r.Header.Set("X-Real-IP", c.real)
			}
			var set []netip.Prefix
			if c.trust {
				set = trusted
			}
			if got := clientAddr(r, set).String(); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
	if _, err := ParseTrustedProxies("nonsense"); err == nil {
		t.Fatal("invalid entry accepted")
	}
}

func TestBearerThrottleBehindIngress(t *testing.T) {
	SetTrustedProxies(mustProxies(t, "10.0.0.0/8"))
	t.Cleanup(func() { SetTrustedProxies(nil) })
	calls := 0
	b := NewBearer(authFunc(func(context.Context, string) (Principal, error) { calls++; return Principal{}, ErrUnauthenticated }))
	b.maxFailures = 2
	h := b.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	do := func(client, auth string) int {
		r := httptest.NewRequest("GET", "/v2/", nil)
		r.RemoteAddr = "10.0.0.7:4000" // the ingress
		r.Header.Set("X-Forwarded-For", client)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	// Challenge probes without credentials never count, however many there are.
	for range 50 {
		if do("198.51.100.1", "") != 401 {
			t.Fatal("probe not challenged")
		}
	}
	if calls != 0 {
		t.Fatal("anonymous probe reached authenticator")
	}
	// One client exhausts its own budget...
	for range 2 {
		do("198.51.100.1", "Bearer bad")
	}
	if do("198.51.100.1", "Bearer bad") != 429 {
		t.Fatal("offender not throttled")
	}
	// ...but another client behind the same ingress is unaffected,
	if do("198.51.100.2", "Bearer bad") != 401 {
		t.Fatal("other client behind ingress throttled")
	}
	// and so is the same client under a different username.
	if do("198.51.100.1", "Basic "+b64("alice:bad")) != 401 {
		t.Fatal("different principal from same client throttled")
	}
}

func TestBearerSpoofedHeaderFromUntrustedPeerDoesNotEvade(t *testing.T) {
	SetTrustedProxies(mustProxies(t, "10.0.0.0/8"))
	t.Cleanup(func() { SetTrustedProxies(nil) })
	b := NewBearer(authFunc(func(context.Context, string) (Principal, error) { return Principal{}, ErrUnauthenticated }))
	b.maxFailures = 1
	h := b.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	code := func(spoof string) int {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "203.0.113.50:1"
		r.Header.Set("X-Forwarded-For", spoof)
		r.Header.Set("Authorization", "Bearer bad")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	code("1.1.1.1")
	if code("2.2.2.2") != 429 {
		t.Fatal("untrusted peer rotated identity via X-Forwarded-For")
	}
}

func TestBearerUsernameRotationHitsAggregate(t *testing.T) {
	b := NewBearer(authFunc(func(context.Context, string) (Principal, error) { return Principal{}, ErrUnauthenticated }))
	b.maxFailures = 1
	h := b.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	limited := false
	for i := range 40 {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "203.0.113.60:1"
		r.Header.Set("Authorization", "Basic "+b64("user"+strconv.Itoa(i)+":bad"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == 429 {
			limited = true
		}
	}
	if !limited {
		t.Fatal("rotating usernames evaded the aggregate budget")
	}
}
