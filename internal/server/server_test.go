package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
)

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func start(t *testing.T, db Pinger, limit int, timeout time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(db, limit, timeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(cancel)
	return "http://" + ln.Addr().String(), cancel, done
}
func getCode(t *testing.T, url string) int {
	t.Helper()
	c := http.Client{Timeout: 3 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	_, _ = io.Copy(io.Discard, r.Body)
	return r.StatusCode
}
func wait(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(4 * time.Second):
		t.Fatal("server failed to stop")
		return nil
	}
}
func TestHealth(t *testing.T) {
	url, cancel, done := start(t, pingFunc(func(context.Context) error { return errors.New("secret database detail") }), 2, time.Second)
	if got := getCode(t, url+"/health/live"); got != 200 {
		t.Fatalf("live: %d", got)
	}
	if got := getCode(t, url+"/health/ready"); got != 503 {
		t.Fatalf("ready: %d", got)
	}
	if got := getCode(t, url+"/unknown"); got != 404 {
		t.Fatalf("unknown: %d", got)
	}
	cancel()
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrencyAndGracefulDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	url, cancel, done := start(t, pingFunc(func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), 1, time.Second)
	response := make(chan error, 1)
	go func() {
		r, err := http.Get(url + "/health/ready")
		if err == nil {
			defer r.Body.Close()
			_, _ = io.Copy(io.Discard, r.Body)
			if r.StatusCode != 200 {
				err = errors.New("inflight request failed")
			}
		}
		response <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	if got := getCode(t, url+"/health/live"); got != 200 {
		t.Fatalf("liveness must remain available during overload, got %d", got)
	}
	if got := getCode(t, url+"/health/ready"); got != 503 {
		t.Fatalf("expected overload rejection, got %d", got)
	}
	client := http.Client{Timeout: time.Second}
	busy, err := client.Get(url + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	var body api.Error
	err = json.NewDecoder(busy.Body).Decode(&body)
	busy.Body.Close()
	if err != nil || busy.StatusCode != http.StatusServiceUnavailable || body.Code != api.ErrorCodeUnavailable || body.RequestId == "" || body.RequestId != busy.Header.Get("X-Request-ID") || busy.Header.Get("Retry-After") != "1" {
		t.Fatalf("invalid overload response: status=%d body=%+v error=%v", busy.StatusCode, body, err)
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("did not drain: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := wait(t, response); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}
func TestShutdownDeadlineCancelsRequests(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	url, cancel, done := start(t, pingFunc(func(ctx context.Context) error { close(entered); <-ctx.Done(); close(exited); return ctx.Err() }), 1, 30*time.Millisecond)
	go func() {
		r, err := http.Get(url + "/health/ready")
		if err == nil {
			r.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	if err := wait(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected drain deadline, got %v", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("handler not cancelled")
	}
}

func TestMetricsIntegrationAndRedaction(t *testing.T) {
	var logs bytes.Buffer
	svc := New(pingFunc(func(context.Context) error { return errors.New("private-db-detail") }), 2, time.Second, slog.New(slog.NewJSONHandler(&logs, nil)))
	for _, path := range []string{"/health/ready", "/health/live", "/api/v1/missing-secret-id?token=query-secret"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer bearer-secret")
		r.Header.Set("X-Request-ID", "caller-secret")
		svc.http.Handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	w := httptest.NewRecorder()
	svc.http.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("metrics response %d", w.Code)
	}
	for _, secret := range []string{"private-db-detail", "missing-secret-id", "query-secret", "bearer-secret", "caller-secret"} {
		if strings.Contains(w.Body.String(), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	if !strings.Contains(w.Body.String(), "ant_database_readiness_failures_total 1") || !strings.Contains(w.Body.String(), `route="/health/ready",method="GET",status_class="5xx"} 1`) {
		t.Fatalf("missing readiness telemetry %s", w.Body.String())
	}
}

func TestMetricsAdmissionAndSaturation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	svc := New(pingFunc(func(context.Context) error { close(entered); <-release; return nil }), 1, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.http.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health/ready", nil))
	}()
	<-entered
	live := httptest.NewRecorder()
	svc.http.Handler.ServeHTTP(live, httptest.NewRequest("GET", "/health/live", nil))
	if live.Code != 200 {
		t.Error("liveness limited during saturation")
	}
	blocked := httptest.NewRecorder()
	svc.http.Handler.ServeHTTP(blocked, httptest.NewRequest("GET", "/metrics", nil))
	if blocked.Code != 503 {
		t.Errorf("metrics bypassed request admission: %d", blocked.Code)
	}
	close(release)
	<-done
	metrics := httptest.NewRecorder()
	svc.http.Handler.ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if metrics.Code != 200 || !strings.Contains(metrics.Body.String(), "ant_http_saturation_total 1") {
		t.Fatalf("missing saturation counter: %d %s", metrics.Code, metrics.Body.String())
	}
}
