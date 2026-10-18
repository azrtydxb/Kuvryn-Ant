package observability

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func scrape(h *HTTP) string {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}
func TestCountersHistogramAndNoSensitiveLabels(t *testing.T) {
	h := NewHTTP()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := h.Wrap(logger, func(*http.Request) string { return "GET /api/v1/feeds/{id}" }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	raw := "b5ca1026-f9c2-4a24-bf71-2f1c57810da6"
	r := httptest.NewRequest("GET", "/api/v1/feeds/"+raw+"?token=query-secret", nil)
	r.Header.Set("Authorization", "Bearer authorization-secret")
	r.Header.Set("X-Request-ID", "caller-secret")
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "server-generated-id")
	handler.ServeHTTP(w, r)
	h.Saturated()
	h.ReadinessFailed()
	metrics := scrape(h)
	for _, secret := range []string{raw, "query-secret", "authorization-secret", "caller-secret"} {
		if strings.Contains(metrics, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("telemetry leaked %s", secret)
		}
	}
	for _, line := range []string{
		`ant_http_requests_total{route="/api/v1/feeds/{id}",method="GET",status_class="4xx"} 1`,
		`ant_http_request_duration_seconds_count{route="/api/v1/feeds/{id}",method="GET",status_class="4xx"} 1`,
		`ant_http_request_duration_seconds_bucket{route="/api/v1/feeds/{id}",method="GET",status_class="4xx",le="+Inf"} 1`,
		"ant_http_active_requests 0", "ant_http_saturation_total 1", "ant_database_readiness_failures_total 1",
	} {
		if !strings.Contains(metrics, line) {
			t.Fatalf("missing metric %s", line)
		}
	}
	if !strings.Contains(logs.String(), `"request_id":"server-generated-id"`) || !strings.Contains(logs.String(), `"status":404`) {
		t.Fatalf("missing completion log: %s", logs.String())
	}
}
func TestLabelsBoundedAndHealthSilent(t *testing.T) {
	h := NewHTTP()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	unknown := h.Wrap(logger, func(r *http.Request) string { return r.URL.Path }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	for i := range 100 {
		r := httptest.NewRequest(fmt.Sprintf("CUSTOM%d", i), fmt.Sprintf("/unknown/%d", i), nil)
		unknown.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(h.requests) != 1 {
		t.Fatalf("unbounded label series: %d", len(h.requests))
	}
	for _, route := range []string{"GET /health/live", "GET /health/ready", "GET /metrics"} {
		handler := h.Wrap(logger, func(*http.Request) string { return route }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
		before := logs.Len()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		if logs.Len() != before {
			t.Fatal("successful probe produced info logs")
		}
	}
}
func TestActiveConcurrentAndPanicAccounting(t *testing.T) {
	h := NewHTTP()
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := h.Wrap(nil, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	<-entered
	if !strings.Contains(scrape(h), "ant_http_active_requests 1") {
		t.Fatal("active request missing")
	}
	close(release)
	wg.Wait()
	panicHandler := h.Wrap(nil, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("private panic") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		panicHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	metrics := scrape(h)
	if !strings.Contains(metrics, `status_class="5xx"} 1`) || !strings.Contains(metrics, "ant_http_active_requests 0") {
		t.Fatal("panic accounting missing")
	}
}
