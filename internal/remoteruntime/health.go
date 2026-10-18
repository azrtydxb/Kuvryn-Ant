package remoteruntime

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	switch {
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
	if h == nil || source == "" || errors.Is(err, context.Canceled) {
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

// Probe performs one bounded GET of the source origin using its configured
// credentials, CA and egress policy. It records the outcome in Health.
func (m *Manager) Probe(ctx context.Context, source string) (ProbeResult, error) {
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
	response, err := e.fetcher.Fetch(ctx, http.MethodGet, target(e, Request{Path: ""}), http.Header{})
	latency := time.Since(start)
	if err != nil {
		m.health.Record(source, 0, latency, err)
		return ProbeResult{LatencyMS: latency.Milliseconds(), Error: errorClass(err, 0)}, nil
	}
	defer response.Body.Close()
	m.health.Record(source, response.StatusCode, latency, nil)
	res := ProbeResult{Reachable: true, StatusCode: response.StatusCode, LatencyMS: latency.Milliseconds()}
	res.Authenticated = response.StatusCode != http.StatusUnauthorized && response.StatusCode != http.StatusForbidden
	if failureStatus(response.StatusCode) {
		res.Error = errorClass(nil, response.StatusCode)
	}
	return res, nil
}

// Health returns the manager's recorder.
func (m *Manager) Health() *Health { return m.health }

// SetCacheObserver installs a callback invoked once per metadata lookup (hit
// reports whether it was served without contacting the upstream) and once per
// artifact fetch (always a miss). It must be cheap and non-blocking and must be
// set before serving starts.
func (m *Manager) SetCacheObserver(f func(source string, hit bool)) { m.cacheObserver = f }

func (m *Manager) observeCache(source string, hit bool) {
	if f := m.cacheObserver; f != nil {
		f(source, hit)
	}
}
