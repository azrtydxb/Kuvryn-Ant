package remoteruntime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azrtydxb/Kuvryn-Ant/internal/upstream"
)

const healthRing = 128

// Health records upstream outcomes per remote source. Recording is a short
// mutex-guarded in-memory update (no I/O); Run persists changed sources
// periodically so health survives restarts. It stores only status codes,
// coarse error classes and latencies, never URLs, headers or credentials.
type Health struct {
	mu      sync.Mutex
	sources map[string]*sourceHealth
}
type sourceHealth struct {
	lastSuccess, lastError time.Time
	lastLog                time.Time
	lastStatus             int
	lastErr                string
	ring                   [healthRing]int32
	filled, pos            int
	requests, failures     int64
	pendingReq, pendingErr int64
	dirty                  bool
}

// HealthSnapshot is a point-in-time view of one source.
type HealthSnapshot struct {
	SourceID      string
	LastSuccessAt *time.Time
	LastErrorAt   *time.Time
	LastStatus    int
	LastError     string
	P95MS         int
	Requests      int64
	Failures      int64
}

func NewHealth() *Health { return &Health{sources: map[string]*sourceHealth{}} }

// failureStatus reports whether an HTTP status means the upstream is unusable
// for this source (as opposed to a normal negative answer such as 404).
func failureStatus(status int) bool {
	return status == 0 || status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests
}

func errorClass(err error, status int) string {
	var fe *upstream.FetchError
	switch {
	case errors.As(err, &fe):
		if fe.Class == "status" {
			return "upstream_error"
		}
		return fe.Class
	case errors.Is(err, upstream.ErrCircuitOpen):
		return "circuit_open"
	case errors.Is(err, upstream.ErrPolicy):
		return "policy"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case err != nil:
		return "connection"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "upstream_error"
	}
}

// Record notes one upstream exchange. status is 0 when no response was received.
func (h *Health) Record(source string, status int, latency time.Duration, err error) {
	if h == nil || source == "" || errors.Is(err, context.Canceled) || errors.Is(err, upstream.ErrCircuitOpen) {
		// A cancelled client or a fast-failed request while the breaker is open
		// never reached the upstream, so neither says anything about its health.
		return
	}
	failed := err != nil || failureStatus(status)
	now := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sources[source]
	if s == nil {
		if len(h.sources) >= 4096 {
			return
		}
		s = &sourceHealth{}
		h.sources[source] = s
	}
	s.requests++
	s.pendingReq++
	s.lastStatus = status
	ms := latency.Milliseconds()
	if ms > 1<<30 {
		ms = 1 << 30
	}
	s.ring[s.pos] = int32(ms)
	s.pos = (s.pos + 1) % healthRing
	if s.filled < healthRing {
		s.filled++
	}
	if failed {
		s.failures++
		s.pendingErr++
		s.lastError = now
		s.lastErr = errorClass(err, status)
		if now.Sub(s.lastLog) >= 30*time.Second {
			s.lastLog = now
			attrs := []any{"source_id", source, "class", s.lastErr, "status", status, "latency_ms", latency.Milliseconds()}
			var fe *upstream.FetchError
			if errors.As(err, &fe) && fe.Host != "" {
				attrs = append(attrs, "host", fe.Host)
			}
			slog.Warn("upstream request failed", attrs...)
		}
	} else {
		s.lastSuccess = now
	}
	s.dirty = true
}

func (s *sourceHealth) snapshot(id string) HealthSnapshot {
	out := HealthSnapshot{SourceID: id, LastStatus: s.lastStatus, LastError: s.lastErr, Requests: s.requests, Failures: s.failures}
	if !s.lastSuccess.IsZero() {
		t := s.lastSuccess
		out.LastSuccessAt = &t
	}
	if !s.lastError.IsZero() {
		t := s.lastError
		out.LastErrorAt = &t
	}
	if s.filled > 0 {
		values := make([]int32, s.filled)
		copy(values, s.ring[:s.filled])
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		idx := (s.filled*95 + 99) / 100
		if idx < 1 {
			idx = 1
		}
		out.P95MS = int(values[idx-1])
	}
	return out
}

// Snapshot returns the in-memory view for one source, if it has been used since start.
func (h *Health) Snapshot(source string) (HealthSnapshot, bool) {
	if h == nil {
		return HealthSnapshot{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sources[source]
	if s == nil {
		return HealthSnapshot{}, false
	}
	return s.snapshot(source), true
}

// Flush upserts changed sources. Rows for deleted sources are skipped by the
// INSERT..SELECT guard.
func (h *Health) Flush(ctx context.Context, db *pgxpool.Pool, instance string) error {
	if h == nil || db == nil {
		return nil
	}
	type item struct {
		snap     HealthSnapshot
		req, err int64
	}
	h.mu.Lock()
	var batch []item
	for id, s := range h.sources {
		if !s.dirty {
			continue
		}
		batch = append(batch, item{s.snapshot(id), s.pendingReq, s.pendingErr})
		s.dirty, s.pendingReq, s.pendingErr = false, 0, 0
	}
	h.mu.Unlock()
	var first error
	for _, it := range batch {
		var status any
		if it.snap.LastStatus > 0 {
			status = it.snap.LastStatus
		}
		_, err := db.Exec(ctx, `INSERT INTO remote_source_health(instance_id,source_id,last_success_at,last_error_at,last_status,last_error,p95_ms,requests,failures)
 SELECT $1,s.id,$3,$4,$5,NULLIF($6,''),$7,$8,$9 FROM sources s WHERE s.instance_id=$1 AND s.id=$2
 ON CONFLICT(instance_id,source_id) DO UPDATE SET last_success_at=COALESCE(excluded.last_success_at,remote_source_health.last_success_at),
 last_error_at=COALESCE(excluded.last_error_at,remote_source_health.last_error_at),last_status=excluded.last_status,
 last_error=CASE WHEN excluded.last_error_at IS NOT NULL THEN excluded.last_error ELSE remote_source_health.last_error END,
 p95_ms=excluded.p95_ms,requests=remote_source_health.requests+excluded.requests,failures=remote_source_health.failures+excluded.failures,updated_at=now()`,
			instance, it.snap.SourceID, it.snap.LastSuccessAt, it.snap.LastErrorAt, status, it.snap.LastError, it.snap.P95MS, it.req, it.err)
		if err != nil {
			if first == nil {
				first = err
			}
			h.mu.Lock()
			if s := h.sources[it.snap.SourceID]; s != nil {
				s.dirty, s.pendingReq, s.pendingErr = true, s.pendingReq+it.req, s.pendingErr+it.err
			}
			h.mu.Unlock()
		}
	}
	return first
}

// Run flushes every interval until ctx ends, then once more.
func (h *Health) Run(ctx context.Context, db *pgxpool.Pool, instance string, interval time.Duration) {
	if h == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = h.Flush(flush, db, instance)
			cancel()
			return
		case <-t.C:
			flush, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = h.Flush(flush, db, instance)
			cancel()
		}
	}
}

// ProbeResult is the outcome of an operator-initiated connection test.
type ProbeResult struct {
	Reachable     bool
	Authenticated bool
	StatusCode    int
	LatencyMS     int64
	Error         string
}

// ProbePath returns the origin-relative path used to test a source of the
// given feed format. Registry roots frequently redirect off-origin or answer
// 4xx, so formats with a well-known liveness endpoint use it.
func ProbePath(format string) string {
	switch format {
	case "oci", "oci-artifacts":
		return "v2/"
	case "nuget":
		return "v3/index.json"
	case "npm":
		return "-/ping"
	default:
		return ""
	}
}

// Probe performs one bounded GET of the format's liveness endpoint under the
// source's configured credentials, CA and egress policy. Only outcomes that
// prove something about the upstream are recorded in Health: a bad probe path
// (redirect off origin, 4xx other than auth, an anonymous registry challenge)
// reports reachability to the operator without poisoning the health that real
// traffic records.
func (m *Manager) Probe(ctx context.Context, source, format string) (ProbeResult, error) {
	admit, err := m.admission(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	defer admit()
	e, err := m.acquire(ctx, source)
	if err != nil {
		return ProbeResult{}, err
	}
	defer m.release(e)
	ctx, cancel := operationContext(ctx, e)
	defer cancel()
	if err = m.networkSlot(ctx); err != nil {
		return ProbeResult{}, safeError(err)
	}
	defer func() { <-m.network }()
	start := time.Now()
	response, err := e.fetcher.Fetch(ctx, http.MethodGet, target(e, Request{Path: ProbePath(format)}), http.Header{})
	latency := time.Since(start)
	var redirect *upstream.RedirectError
	if errors.As(err, &redirect) {
		// The origin answered with a redirect we will not follow: it is up.
		return ProbeResult{Reachable: true, Authenticated: true, StatusCode: redirect.Status, LatencyMS: latency.Milliseconds()}, nil
	}
	if err != nil {
		m.health.Record(source, 0, latency, err)
		return ProbeResult{LatencyMS: latency.Milliseconds(), Error: errorClass(err, 0)}, nil
	}
	defer response.Body.Close()
	res := probeOutcome(format, response.StatusCode, response.Header.Get("Www-Authenticate") != "")
	res.LatencyMS = latency.Milliseconds()
	if res.record {
		m.health.Record(source, response.StatusCode, latency, nil)
	}
	return res.ProbeResult, nil
}

type probeDecision struct {
	ProbeResult
	record bool
}

// probeOutcome interprets a probe response. record reports whether the status
// is evidence about the source's health (and so belongs in Health).
func probeOutcome(format string, status int, challenge bool) probeDecision {
	res := ProbeResult{Reachable: true, StatusCode: status, Authenticated: true}
	switch {
	case status == http.StatusUnauthorized && challenge && (format == "oci" || format == "oci-artifacts"):
		// Registries answer /v2/ with a challenge to anonymous clients; that is
		// the normal handshake, not a credential failure.
		return probeDecision{res, false}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		res.Authenticated = false
		res.Error = errorClass(nil, status)
		return probeDecision{res, true}
	case status >= 500 || status == http.StatusTooManyRequests:
		res.Error = errorClass(nil, status)
		return probeDecision{res, true}
	case status >= 400:
		// 400/404 from a probe path says the host is up, not that real traffic
		// works; do not record it either way.
		return probeDecision{res, false}
	}
	return probeDecision{res, true}
}

// Health returns the manager's recorder.
func (m *Manager) Health() *Health { return m.health }

// CacheOutcome classifies one remote lookup for cache-efficiency metrics.
type CacheOutcome int

const (
	// CacheHit was answered from the metadata cache without contacting upstream.
	CacheHit CacheOutcome = iota
	// CacheRevalidated contacted upstream, which answered 304 Not Modified.
	CacheRevalidated
	// CacheMiss transferred a document or artifact from upstream.
	CacheMiss
)

// SetCacheObserver installs a callback invoked once per successful metadata
// lookup and once per artifact fetch that returns content. Not-found answers
// and upstream failures are not cache outcomes and are not reported. It must be
// cheap and non-blocking and must be set before serving starts.
func (m *Manager) SetCacheObserver(f func(source string, outcome CacheOutcome)) { m.cacheObserver = f }

func (m *Manager) observeCache(source string, outcome CacheOutcome) {
	if f := m.cacheObserver; f != nil {
		f(source, outcome)
	}
}
