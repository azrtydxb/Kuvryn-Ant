package limits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
)

const (
	feedA     = "11111111-1111-4111-8111-111111111111"
	feedB     = "22222222-2222-4222-8222-222222222222"
	principal = "33333333-3333-4333-8333-333333333333"
	other     = "44444444-4444-4444-8444-444444444444"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newLimiter(t *testing.T, cfg Config) (*Limiter, *clock) {
	t.Helper()
	norm, err := Normalize(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{time.Unix(1_700_000_000, 0)}
	l := &Limiter{now: c.now}
	l.APIError = func(w http.ResponseWriter, r *http.Request, status int, code api.ErrorCode, msg string) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"code":"` + string(code) + `"}`))
	}
	l.Apply(norm, map[string]string{"feed-a": feedA, "feed-b": feedB})
	return l, c
}

func do(h http.Handler, method, path, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

func TestFeedLimitIsPerFeedWithRetryAfter(t *testing.T) {
	l, clk := newLimiter(t, Config{RateLimits: []RateRule{{Scope: ScopeFeed, Subject: feedA, RequestsPerMinute: 60, Burst: 2}}})
	h := l.Middleware(ok)
	for i := 0; i < 2; i++ {
		if w := do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1"); w.Code != 204 {
			t.Fatalf("burst request %d: %d", i, w.Code)
		}
	}
	w := do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1")
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" || w.Header().Get("X-RateLimit-Scope") != "feed" {
		t.Fatalf("expected 429 with Retry-After=1: %d %v", w.Code, w.Header())
	}
	if !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatalf("body: %s", w.Body.String())
	}
	// Another feed and unrelated traffic are unaffected.
	if do(h, "GET", "/feeds/feed-b/x", "10.0.0.1:1").Code != 204 || do(h, "GET", "/health/live", "10.0.0.1:1").Code != 204 {
		t.Fatal("limit leaked to other traffic")
	}
	clk.advance(1100 * time.Millisecond)
	if do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1").Code != 204 {
		t.Fatal("bucket did not refill")
	}
}

func TestRetryAfterReflectsRate(t *testing.T) {
	l, _ := newLimiter(t, Config{RateLimits: []RateRule{{Scope: ScopeSite, Subject: "everyone", RequestsPerMinute: 6, Burst: 1}}})
	h := l.Middleware(ok)
	do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1")
	w := do(h, "GET", "/api/v1/feeds", "10.0.0.1:1")
	if w.Code != 429 {
		t.Fatalf("management traffic is limited too: %d", w.Code)
	}
	if got, _ := strconv.Atoi(w.Header().Get("Retry-After")); got != 10 {
		t.Fatalf("Retry-After=%q, want 10", w.Header().Get("Retry-After"))
	}
	if !strings.Contains(w.Body.String(), "rate_limited") {
		t.Fatal("management 429 must use the API error shape")
	}
}

func TestSiteCIDRGroupAndNamespace(t *testing.T) {
	l, _ := newLimiter(t, Config{RateLimits: []RateRule{
		{Scope: ScopeSite, Subject: "build-farm", CIDRs: []string{"10.1.0.0/16", "2001:db8::/32"}, RequestsPerMinute: 60, Burst: 1},
		{Scope: ScopeNamespace, Subject: feedA, Namespace: "@acme", RequestsPerMinute: 60, Burst: 1},
	}})
	h := l.Middleware(ok)
	if do(h, "GET", "/feeds/feed-b/x", "10.1.2.3:1").Code != 204 {
		t.Fatal("first request from site")
	}
	if do(h, "GET", "/feeds/feed-b/x", "10.1.9.9:1").Code != 429 {
		t.Fatal("site members share one bucket")
	}
	if do(h, "GET", "/feeds/feed-b/x", "[2001:db8::5]:1").Code != 429 {
		t.Fatal("IPv6 members of the site share the bucket")
	}
	if do(h, "GET", "/feeds/feed-b/x", "10.2.0.1:1").Code != 204 || do(h, "GET", "/feeds/feed-b/x", "10.2.0.1:1").Code != 204 {
		t.Fatal("clients outside the site are not limited")
	}
	if do(h, "GET", "/feeds/feed-a/@acme/pkg", "10.2.0.1:1").Code != 204 {
		t.Fatal("namespace first request")
	}
	if w := do(h, "GET", "/feeds/feed-a/@acme/pkg2", "10.2.0.2:1"); w.Code != 429 || w.Header().Get("X-RateLimit-Scope") != "namespace" {
		t.Fatalf("namespace limit: %d %v", w.Code, w.Header())
	}
	if do(h, "GET", "/feeds/feed-a/@other/pkg", "10.2.0.2:1").Code != 204 {
		t.Fatal("other namespaces unaffected")
	}
}

func TestRejectionConsumesNoTokens(t *testing.T) {
	l, clk := newLimiter(t, Config{RateLimits: []RateRule{
		{Scope: ScopeFeed, Subject: feedA, RequestsPerMinute: 60, Burst: 10},
		{Scope: ScopeSite, Subject: "tight", RequestsPerMinute: 60, Burst: 1},
	}})
	h := l.Middleware(ok)
	do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1")
	for i := 0; i < 5; i++ {
		if do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1").Code != 429 {
			t.Fatal("site bucket must reject")
		}
	}
	clk.advance(time.Second)
	if do(h, "GET", "/feeds/feed-a/x", "10.0.0.1:1").Code != 204 {
		t.Fatal("rejected requests drained the feed bucket")
	}
}

func TestPrincipalLimitUsesResolver(t *testing.T) {
	l, _ := newLimiter(t, Config{RateLimits: []RateRule{{Scope: ScopePrincipal, Subject: principal, RequestsPerMinute: 60, Burst: 1}}})
	l.Resolve = func(_ context.Context, token string) (string, bool) {
		if token == "alice" {
			return principal, true
		}
		return other, true
	}
	h := l.Middleware(ok)
	req := func(token string) int {
		r := httptest.NewRequest("GET", "/api/v1/feeds", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if req("alice") != 204 || req("alice") != 429 {
		t.Fatal("principal limit not applied per principal")
	}
	if req("bob") != 204 || req("bob") != 204 {
		t.Fatal("unlimited principal affected")
	}
	// Package clients authenticate with the token as the Basic password.
	basic := httptest.NewRequest("GET", "/feeds/feed-a/pkg", nil)
	basic.SetBasicAuth("ci", "alice")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, basic)
	if w.Code != 429 {
		t.Fatalf("basic-auth principal not limited: %d", w.Code)
	}
}

func TestQuotaWriterReports507(t *testing.T) {
	l, _ := newLimiter(t, Config{Quotas: []QuotaRule{{Scope: ScopeFeed, FeedID: feedA, MaxBytes: ptr(10)}}})
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noteQuota(r.Context(), &QuotaError{Scope: ScopeFeed, FeedID: feedA, Dimension: "bytes", Limit: 10, Used: 8, Requested: 5})
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("no"))
	}))
	w := do(h, "PUT", "/feeds/feed-a/x", "10.0.0.1:1")
	if w.Code != 507 || !strings.Contains(w.Body.String(), "quota_exceeded") || !strings.Contains(w.Body.String(), "bytes limit 10, used 8") || strings.Contains(w.Body.String(), "no\n") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	// Without a quota note the adapter's own status passes through.
	pass := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) }))
	if do(pass, "PUT", "/feeds/feed-a/x", "10.0.0.1:1").Code != 409 {
		t.Fatal("unrelated conflicts must not be rewritten")
	}
}

func TestNormalizeRejectsBadRules(t *testing.T) {
	bad := []Config{
		{RateLimits: []RateRule{{Scope: "nope", Subject: "x", RequestsPerMinute: 1, Burst: 1}}},
		{RateLimits: []RateRule{{Scope: ScopeFeed, Subject: "not-a-uuid", RequestsPerMinute: 1, Burst: 1}}},
		{RateLimits: []RateRule{{Scope: ScopeFeed, Subject: feedA, RequestsPerMinute: 0, Burst: 1}}},
		{RateLimits: []RateRule{{Scope: ScopeSite, Subject: "s", CIDRs: []string{"300.0.0.0/8"}, RequestsPerMinute: 1, Burst: 1}}},
		{RateLimits: []RateRule{{Scope: ScopeNamespace, Subject: feedA, RequestsPerMinute: 1, Burst: 1}}},
		{RateLimits: []RateRule{{Scope: ScopeFeed, Subject: feedA, RequestsPerMinute: 1, Burst: 1}, {Scope: ScopeFeed, Subject: feedA, RequestsPerMinute: 2, Burst: 1}}},
		{Quotas: []QuotaRule{{Scope: ScopeFeed, FeedID: feedA}}},
		{Quotas: []QuotaRule{{Scope: ScopeNamespace, FeedID: feedA, MaxBytes: ptr(1)}}},
		{Quotas: []QuotaRule{{Scope: ScopeFeed, FeedID: feedA, MaxBytes: ptr(-1)}}},
	}
	for i, c := range bad {
		if _, err := Normalize(c); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	got, err := Normalize(Config{RateLimits: []RateRule{{Scope: ScopeSite, Subject: "s", CIDRs: []string{"10.1.2.3/16"}, RequestsPerMinute: 1, Burst: 1}}})
	if err != nil || got.RateLimits[0].CIDRs[0] != "10.1.0.0/16" {
		t.Fatalf("cidr not canonicalized: %v %v", got, err)
	}
}

func TestMetricsExposition(t *testing.T) {
	var b strings.Builder
	Default.observeQuota(QuotaRule{Scope: ScopeFeed, FeedID: feedA, MaxBytes: ptr(100)}, 40, 2)
	Default.WriteMetrics(&b)
	for _, want := range []string{"ant_ratelimit_throttled_total{scope=\"site\"}", "ant_quota_rejections_total", "ant_quota_usage{scope=\"feed\",feed=\"" + feedA + "\",namespace=\"\",unit=\"bytes\"} 40", "ant_quota_limit{scope=\"feed\""} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q in\n%s", want, b.String())
		}
	}
}

func ptr(v int64) *int64 { return &v }
