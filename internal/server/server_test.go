package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
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
	if got := getCode(t, url+"/health/live"); got != 503 {
		t.Fatalf("expected overload rejection, got %d", got)
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
