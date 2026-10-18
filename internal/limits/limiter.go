package limits

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PrincipalResolver maps a raw bearer credential to a principal ID. It is only
// consulted when principal rate limits exist.
type PrincipalResolver func(ctx context.Context, rawToken string) (principal string, ok bool)

type bucket struct {
	rate, burst float64 // tokens per second, capacity
	tokens      float64
	last        time.Time
	rule        RateRule
}

type siteRule struct {
	b        *bucket
	prefixes []netip.Prefix
}

// snapshot is an immutable compiled view of the configuration. Buckets inside
// it are mutated under Limiter.mu; the snapshot itself is swapped atomically.
type snapshot struct {
	principal  map[string]*bucket
	feed       map[string]*bucket
	namespace  map[string]map[string]*bucket // feed ID -> namespace -> bucket
	sites      []siteRule
	slugToFeed map[string]string
	hasRates   bool
	hasQuotas  bool
}

// Limiter applies token-bucket request limits per principal, feed, namespace
// and site, and carries quota rejections to the HTTP layer.
//
// debt: buckets live in process memory, so with several instances behind one
// load balancer each instance enforces the configured rate independently
// (effective limit is rate x instances). A shared (PostgreSQL/Redis) counter is
// needed for exact multi-instance limits; quotas are already exact because they
// are enforced in the database transaction.
type Limiter struct {
	DB         *pgxpool.Pool
	InstanceID string
	// Resolve identifies the caller of bearer-authenticated requests.
	Resolve PrincipalResolver
	// APIError writes a management API error (management.Error).
	APIError func(w http.ResponseWriter, r *http.Request, status int, code api.ErrorCode, message string)
	Logger   *slog.Logger

	mu   sync.Mutex
	snap atomic.Pointer[snapshot]
	now  func() time.Time
}

func (l *Limiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func newBucket(r RateRule, old *bucket, now time.Time) *bucket {
	rate := float64(r.RequestsPerMinute) / 60
	if old != nil && old.rule.RequestsPerMinute == r.RequestsPerMinute && old.rule.Burst == r.Burst {
		return old // keep accumulated state across unrelated config edits
	}
	return &bucket{rate: rate, burst: float64(r.Burst), tokens: float64(r.Burst), last: now, rule: r}
}

// Apply compiles cfg (already normalized) and slug resolution into the live
// snapshot, preserving the state of unchanged buckets.
func (l *Limiter) Apply(cfg Config, slugToFeed map[string]string) {
	now := l.clock()
	prev := l.snap.Load()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &snapshot{principal: map[string]*bucket{}, feed: map[string]*bucket{}, namespace: map[string]map[string]*bucket{}, slugToFeed: slugToFeed, hasQuotas: len(cfg.Quotas) > 0}
	for _, r := range cfg.RateLimits {
		s.hasRates = true
		switch r.Scope {
		case ScopePrincipal:
			var old *bucket
			if prev != nil {
				old = prev.principal[r.Subject]
			}
			s.principal[r.Subject] = newBucket(r, old, now)
		case ScopeFeed:
			var old *bucket
			if prev != nil {
				old = prev.feed[r.Subject]
			}
			s.feed[r.Subject] = newBucket(r, old, now)
		case ScopeNamespace:
			var old *bucket
			if prev != nil {
				old = prev.namespace[r.Subject][r.Namespace]
			}
			if s.namespace[r.Subject] == nil {
				s.namespace[r.Subject] = map[string]*bucket{}
			}
			s.namespace[r.Subject][r.Namespace] = newBucket(r, old, now)
		case ScopeSite:
			var old *bucket
			if prev != nil {
				for _, p := range prev.sites {
					if p.b.rule.Subject == r.Subject {
						old = p.b
					}
				}
			}
			site := siteRule{b: newBucket(r, old, now)}
			for _, c := range r.CIDRs {
				if p, err := netip.ParsePrefix(c); err == nil {
					site.prefixes = append(site.prefixes, p)
				}
			}
			s.sites = append(s.sites, site)
		}
	}
	l.snap.Store(s)
}

// Refresh reloads the persisted configuration.
func (l *Limiter) Refresh(ctx context.Context) error {
	var raw1, raw2 []byte
	err := l.DB.QueryRow(ctx, `SELECT rate_limits,quotas FROM limits_config WHERE instance_id=$1`, l.InstanceID).Scan(&raw1, &raw2)
	cfg := Config{}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		if err = json.Unmarshal(raw1, &cfg.RateLimits); err != nil {
			return err
		}
		if err = json.Unmarshal(raw2, &cfg.Quotas); err != nil {
			return err
		}
	}
	slugs := map[string]string{}
	rows, err := l.DB.Query(ctx, `SELECT slug,id::text FROM feeds WHERE instance_id=$1`, l.InstanceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug, id string
		if err = rows.Scan(&slug, &id); err != nil {
			return err
		}
		slugs[slug] = id
	}
	if rows.Err() != nil {
		return rows.Err()
	}
	l.Apply(cfg, slugs)
	Default.setConfig(cfg)
	return nil
}

// Run refreshes the configuration periodically until ctx ends. Management
// writes call Refresh directly so changes apply immediately on this instance.
func (l *Limiter) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := l.Refresh(ctx); err != nil && ctx.Err() == nil && l.Logger != nil {
			l.Logger.Warn("limits refresh failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// request describes what the middleware could determine cheaply.
type request struct {
	slug, feedID, namespace string
}

func classify(path string) (req request, kind int) {
	var rest string
	switch {
	case strings.HasPrefix(path, "/feeds/"):
		rest, kind = path[len("/feeds/"):], 1
	case strings.HasPrefix(path, "/v2/"):
		rest, kind = path[len("/v2/"):], 1
	case strings.HasPrefix(path, "/api/"):
		if r, ok := strings.CutPrefix(path, "/api/v1/feeds/"); ok {
			id, _, _ := strings.Cut(r, "/")
			req.feedID = id
		}
		return req, 2
	default:
		return req, 0
	}
	slug, tail, _ := strings.Cut(rest, "/")
	req.slug = slug
	req.namespace, _, _ = strings.Cut(tail, "/")
	return req, kind
}

func clientAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// take consumes one token from every bucket or none. It returns the scope that
// throttled and the wait until a token is available.
func (l *Limiter) take(buckets []*bucket, scopes []string) (scope string, wait time.Duration, ok bool) {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, b := range buckets {
		if el := now.Sub(b.last).Seconds(); el > 0 {
			b.tokens = math.Min(b.burst, b.tokens+el*b.rate)
			b.last = now
		}
		if b.tokens < 1 {
			w := time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
			if w > wait {
				wait, scope = w, scopes[i]
			}
		}
	}
	if scope != "" {
		return scope, wait, false
	}
	for _, b := range buckets {
		b.tokens--
	}
	return "", 0, true
}

// Middleware is shared by package-protocol (/feeds/, /v2/) and management
// (/api/) traffic. Probes, metrics and the console are never limited. Install
// it inside the browser-session middleware so session principals are visible.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := l.snap.Load()
		if s == nil || !s.hasRates && !s.hasQuotas {
			next.ServeHTTP(w, r)
			return
		}
		req, kind := classify(r.URL.Path)
		if kind == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if s.hasRates {
			if feed, ok := s.slugToFeed[req.slug]; ok && kind == 1 {
				req.feedID = feed
			}
			buckets := make([]*bucket, 0, 4)
			scopes := make([]string, 0, 4)
			add := func(b *bucket, scope string) { buckets, scopes = append(buckets, b), append(scopes, scope) }
			if len(s.sites) > 0 {
				addr := clientAddr(r.RemoteAddr)
				for _, site := range s.sites {
					if len(site.prefixes) == 0 {
						add(site.b, ScopeSite)
						continue
					}
					for _, p := range site.prefixes {
						if addr.IsValid() && p.Contains(addr) {
							add(site.b, ScopeSite)
							break
						}
					}
				}
			}
			if req.feedID != "" {
				if b := s.feed[req.feedID]; b != nil {
					add(b, ScopeFeed)
				}
				if b := s.namespace[req.feedID][req.namespace]; b != nil && kind == 1 && req.namespace != "" {
					add(b, ScopeNamespace)
				}
			}
			if len(s.principal) > 0 {
				if id := l.principal(r); id != "" {
					if b := s.principal[id]; b != nil {
						add(b, ScopePrincipal)
					}
				}
			}
			if len(buckets) > 0 {
				if scope, wait, ok := l.take(buckets, scopes); !ok {
					l.throttle(w, r, kind, scope, wait)
					return
				}
			}
		}
		if s.hasQuotas && kind == 1 && r.Method != http.MethodGet && r.Method != http.MethodHead {
			ctx, note := withNote(r.Context())
			qw := &quotaWriter{ResponseWriter: w, note: note}
			next.ServeHTTP(qw, r.WithContext(ctx))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) principal(r *http.Request) string {
	if p, ok := identity.PrincipalFromContext(r.Context()); ok {
		return p.PrincipalID
	}
	if l.Resolve == nil {
		return ""
	}
	token := ""
	// Package clients send the service token either as a bearer credential or
	// as the password of HTTP Basic (the adapters accept both).
	if _, password, ok := r.BasicAuth(); ok {
		token = password
	} else if fields := strings.Fields(r.Header.Get("Authorization")); len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") {
		token = fields[1]
	}
	if token == "" || len(token) > 1024 {
		return ""
	}
	id, _ := l.Resolve(r.Context(), token)
	return id
}

func (l *Limiter) throttle(w http.ResponseWriter, r *http.Request, kind int, scope string, wait time.Duration) {
	Default.throttled(scope)
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("X-RateLimit-Scope", scope)
	const msg = "rate limit exceeded"
	if kind == 2 && l.APIError != nil {
		l.APIError(w, r, http.StatusTooManyRequests, api.ErrorCodeRateLimited, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": "rate_limited", "message": msg, "errors": []map[string]string{{"code": "TOOMANYREQUESTS", "message": msg}}})
}

// quotaWriter replaces any error response with 507 when the commit path
// reported a quota rejection through the request context.
type quotaWriter struct {
	http.ResponseWriter
	note    *Note
	discard bool
	wrote   bool
}

func (w *quotaWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *quotaWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	if qe := w.note.get(); qe != nil && status >= 400 {
		h := w.Header()
		h.Del("Content-Length")
		h.Del("Content-Encoding")
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		w.ResponseWriter.WriteHeader(http.StatusInsufficientStorage)
		msg := qe.Error()
		_ = json.NewEncoder(w.ResponseWriter).Encode(map[string]any{"code": "quota_exceeded", "message": msg, "errors": []map[string]string{{"code": "DENIED", "message": msg}}})
		w.discard = true
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *quotaWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.discard {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}
func (w *quotaWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok && !w.discard {
		f.Flush()
	}
}

// TokenResolver resolves bearer credentials to principals with a small TTL
// cache so the rate-limit hot path rarely reaches the database. A revoked
// credential keeps its rate-limit identity for at most the TTL; authorization
// is never decided here.
func TokenResolver(auth identity.Authenticator) PrincipalResolver {
	type entry struct {
		id      string
		expires time.Time
	}
	var mu sync.Mutex
	cache := map[[32]byte]entry{}
	return func(ctx context.Context, raw string) (string, bool) {
		key := sha256.Sum256([]byte(raw))
		now := time.Now()
		mu.Lock()
		e, ok := cache[key]
		mu.Unlock()
		if ok && now.Before(e.expires) {
			return e.id, e.id != ""
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		p, err := auth.Authenticate(cctx, raw)
		e = entry{expires: now.Add(30 * time.Second)}
		if err == nil {
			e.id = p.PrincipalID
		} else {
			e.expires = now.Add(5 * time.Second)
		}
		mu.Lock()
		if len(cache) >= 8192 {
			clear(cache)
		}
		cache[key] = e
		mu.Unlock()
		return e.id, e.id != ""
	}
}
