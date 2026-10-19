// Package denyaudit writes audit_events rows with result='denied' for HTTP 401
// and 403 responses on the management API, MCP endpoint and package protocols.
//
// It is an outermost HTTP middleware so every surface is covered in one place.
// Writes are rate limited per (surface, principal-or-peer, status) and globally,
// and happen on a bounded asynchronous queue, so a credential-guessing client
// cannot flood the audit log or slow the request path. Events never include
// credentials, headers, query strings or bodies.
package denyaudit

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	Action      = "auth.denied"
	FloodAction = "auth.denied.flood"
	maxKeys     = 4096
	queueSize   = 256
)

type bucket struct {
	windowEnd  time.Time
	suppressed int
}

type event struct {
	principal string
	details   map[string]any
	action    string
}

type Recorder struct {
	DB         *pgxpool.Pool
	InstanceID string
	// Window is the per-key suppression interval (default 1 minute).
	Window time.Duration
	// GlobalLimit caps events per Window across all keys (default 120).
	GlobalLimit int
	Now         func() time.Time

	mu          sync.Mutex
	buckets     map[string]*bucket
	globalStart time.Time
	globalCount int
	flooded     bool
	dropped     int
	queue       chan event
	pending     sync.WaitGroup
	started     sync.Once
}

func (r *Recorder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Recorder) start() {
	r.started.Do(func() {
		r.queue = make(chan event, queueSize)
		go func() {
			for e := range r.queue {
				r.write(e)
				r.pending.Done()
			}
		}()
	})
}

// Wait blocks until all queued events are written (for tests and shutdown).
func (r *Recorder) Wait() { r.pending.Wait() }

func (r *Recorder) write(e event) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := json.Marshal(e.details)
	_, _ = r.DB.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,result,details) VALUES($1,NULLIF($2,'')::uuid,$3,'denied',$4::jsonb)`, r.InstanceID, e.principal, e.action, string(raw))
}

func (r *Recorder) admit(key string) (ok bool, suppressed int, flood bool) {
	window, limit := r.Window, r.GlobalLimit
	if window <= 0 {
		window = time.Minute
	}
	if limit <= 0 {
		limit = 120
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buckets == nil {
		r.buckets = map[string]*bucket{}
	}
	if now.Sub(r.globalStart) >= window {
		r.globalStart, r.globalCount, r.flooded = now, 0, false
		for k, b := range r.buckets {
			if !now.Before(b.windowEnd) && b.suppressed == 0 {
				delete(r.buckets, k)
			}
		}
	}
	b := r.buckets[key]
	if b != nil && now.Before(b.windowEnd) {
		b.suppressed++
		return false, 0, false
	}
	if r.globalCount >= limit {
		if b != nil {
			b.suppressed++
		}
		first := !r.flooded
		r.flooded = true
		return false, 0, first
	}
	if b == nil {
		if len(r.buckets) >= maxKeys {
			// Saturated: refuse new keys rather than evict active suppression state.
			r.dropped++
			return false, 0, false
		}
		b = &bucket{}
		r.buckets[key] = b
	}
	suppressed = b.suppressed
	b.suppressed, b.windowEnd = 0, now.Add(window)
	r.globalCount++
	return true, suppressed, false
}

// Record queues one denial, subject to rate limits.
func (r *Recorder) Record(surface string, status int, p identity.Principal, peer, method, path string) {
	if r == nil || r.DB == nil {
		return
	}
	r.start()
	subject := peer
	if p.PrincipalID != "" {
		subject = "p:" + p.PrincipalID
	}
	ok, suppressed, flood := r.admit(surface + "|" + subject + "|" + http.StatusText(status))
	var e event
	switch {
	case ok:
		reason := "unauthenticated"
		if status == http.StatusForbidden {
			reason = "forbidden"
		}
		d := map[string]any{"surface": surface, "status": status, "reason": reason, "method": method, "path": path, "peer": peer}
		if p.TokenID != "" {
			d["token_id"] = p.TokenID
		}
		if suppressed > 0 {
			d["suppressed_since_last"] = suppressed
		}
		e = event{principal: p.PrincipalID, details: d, action: Action}
	case flood:
		e = event{details: map[string]any{"limit_per_window": r.GlobalLimit, "note": "further denials suppressed until the window ends"}, action: FloodAction}
	default:
		return
	}
	r.pending.Add(1)
	select {
	case r.queue <- e:
	default:
		r.pending.Done()
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Surface classifies a request path; "" means the path is not audited
// (console assets, login endpoints which audit themselves, health probes).
func Surface(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/"):
		return "management"
	case path == "/mcp" || strings.HasPrefix(path, "/mcp/"):
		return "mcp"
	case strings.HasPrefix(path, "/auth/"), strings.HasPrefix(path, "/console"), path == "/healthz", path == "/readyz", path == "/metrics", path == "/":
		return ""
	}
	return "package"
}

// Middleware must be the outermost wrapper so it observes every response.
func (r *Recorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		surface := Surface(req.URL.Path)
		if r == nil || surface == "" {
			next.ServeHTTP(w, req)
			return
		}
		ctx, note := identity.WithDenialNote(req.Context())
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, req.WithContext(ctx))
		if sw.status != http.StatusUnauthorized && sw.status != http.StatusForbidden {
			return
		}
		// Registry clients probe /v2/ unauthenticated to learn the auth scheme.
		if sw.status == http.StatusUnauthorized && req.URL.Path == "/v2/" {
			return
		}
		path := req.URL.EscapedPath()
		if len(path) > 200 {
			path = path[:200]
		}
		r.Record(surface, sw.status, note.Principal(), identity.ClientKey(req), req.Method, path)
	})
}
