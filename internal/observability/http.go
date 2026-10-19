// Package observability records bounded, operational HTTP telemetry. It never
// records request paths, queries, credential headers, principals or resource IDs.
package observability

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/tracing"
)

var bounds = [...]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

type labels struct{ route, method, class string }
type observation struct {
	count   uint64
	seconds float64
	buckets [len(bounds)]uint64
}
type HTTP struct {
	mu                sync.Mutex
	requests          map[labels]observation
	active            atomic.Int64
	saturated         atomic.Uint64
	readinessFailures atomic.Uint64
}

func NewHTTP() *HTTP             { return &HTTP{requests: make(map[labels]observation)} }
func (h *HTTP) Saturated()       { h.saturated.Add(1) }
func (h *HTTP) ReadinessFailed() { h.readinessFailures.Add(1) }

// Route accepts registered ServeMux patterns, never raw URLs. Unknown patterns
// collapse to one label so adding a route requires an explicit telemetry decision.
func Route(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	switch pattern {
	case "/v2/", "/api/v1/sources", "/api/v1/sources/{id}", "/api/v1/feeds/{id}/sources", "/api/v1/feeds/{id}/sources/{source_id}", "/api/v1/feeds/{id}/publish-policy", "/health/live", "/health/ready", "/metrics", "/api/v1/status", "/api/v1/feeds", "/api/v1/feeds/{id}":
		return pattern
	default:
		return "unmatched"
	}
}
func method(value string) string {
	switch value {
	case "GET", "HEAD", "POST", "PATCH", "PUT", "DELETE", "OPTIONS":
		return value
	default:
		return "OTHER"
	}
}
func statusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "other"
	}
}

// Wrap must run inside the server-owned request ID middleware. Resolve should
// return a registered mux pattern; Route still whitelists it defensively.
func (h *HTTP) Wrap(logger *slog.Logger, resolve func(*http.Request) string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := "unmatched"
		if resolve != nil {
			route = RouteFor(resolve(r), r)
		}
		verb := method(r.Method)
		started := time.Now()
		h.active.Add(1)
		recorder := &response{ResponseWriter: w}
		defer func() {
			failure := recover()
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
				if failure != nil {
					status = http.StatusInternalServerError
				}
			}
			elapsed := time.Since(started).Seconds()
			key := labels{route, verb, statusClass(status)}
			h.mu.Lock()
			entry := h.requests[key]
			entry.count++
			entry.seconds += elapsed
			for i, bound := range bounds {
				if elapsed <= bound {
					entry.buckets[i]++
				}
			}
			h.requests[key] = entry
			h.mu.Unlock()
			h.active.Add(-1)
			// Health/scrape success traffic is intentionally silent. Failure logs retain
			// only fixed labels and the generated correlation ID, never caller input.
			if logger != nil && (status >= 400 || (route != "/health/live" && route != "/health/ready" && route != "/metrics")) {
				args := []any{"request_id", w.Header().Get("X-Request-ID"), "route", route, "method", verb, "status", status, "duration_seconds", elapsed}
				if id := tracing.TraceID(r.Context()); id != "" {
					args = append(args, "trace_id", id)
				}
				logger.InfoContext(r.Context(), "HTTP request completed", args...)
			}
			if failure != nil {
				panic(failure)
			}
		}()
		next.ServeHTTP(recorder, r)
	})
}

type response struct {
	http.ResponseWriter
	status int
}

func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *response) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *response) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// ServeHTTP exposes operational counters only. Deployments should restrict
// network access to the metrics endpoint using their ingress/network controls.
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	h.mu.Lock()
	snapshot := make(map[labels]observation, len(h.requests))
	keys := make([]labels, 0, len(h.requests))
	for key, value := range h.requests {
		snapshot[key] = value
		keys = append(keys, key)
	}
	h.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.route != b.route {
			return a.route < b.route
		}
		if a.method != b.method {
			return a.method < b.method
		}
		return a.class < b.class
	})
	fmt.Fprintln(w, "# HELP ant_http_requests_total Completed HTTP requests.\n# TYPE ant_http_requests_total counter")
	for _, key := range keys {
		fmt.Fprintf(w, "ant_http_requests_total{route=%q,method=%q,status_class=%q} %d\n", key.route, key.method, key.class, snapshot[key].count)
	}
	fmt.Fprintln(w, "# HELP ant_http_request_duration_seconds HTTP request duration.\n# TYPE ant_http_request_duration_seconds histogram")
	for _, key := range keys {
		entry := snapshot[key]
		label := fmt.Sprintf("route=%q,method=%q,status_class=%q", key.route, key.method, key.class)
		for i, bound := range bounds {
			fmt.Fprintf(w, "ant_http_request_duration_seconds_bucket{%s,le=%q} %d\n", label, fmt.Sprintf("%g", bound), entry.buckets[i])
		}
		fmt.Fprintf(w, "ant_http_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", label, entry.count)
		fmt.Fprintf(w, "ant_http_request_duration_seconds_sum{%s} %g\n", label, entry.seconds)
		fmt.Fprintf(w, "ant_http_request_duration_seconds_count{%s} %d\n", label, entry.count)
	}
	fmt.Fprintln(w, "# HELP ant_http_active_requests HTTP requests currently executing.\n# TYPE ant_http_active_requests gauge")
	fmt.Fprintf(w, "ant_http_active_requests %d\n", h.active.Load())
	fmt.Fprintln(w, "# HELP ant_http_saturation_total Requests rejected by the admission limit.\n# TYPE ant_http_saturation_total counter")
	fmt.Fprintf(w, "ant_http_saturation_total %d\n", h.saturated.Load())
	fmt.Fprintln(w, "# HELP ant_database_readiness_failures_total Failed PostgreSQL readiness checks.\n# TYPE ant_database_readiness_failures_total counter")
	fmt.Fprintf(w, "ant_database_readiness_failures_total %d\n", h.readinessFailures.Load())
	writeCollectors(w)
}
