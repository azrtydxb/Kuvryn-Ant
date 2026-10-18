package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type listenLog struct{ address chan string }

func (h listenLog) Enabled(context.Context, slog.Level) bool { return true }
func (h listenLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "service listening" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "address" {
				h.address <- a.Value.String()
			}
			return true
		})
	}
	return nil
}
func (h listenLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h listenLog) WithGroup(string) slog.Handler      { return h }

func TestMissingSecretRejectsStartup(t *testing.T) {
	t.Setenv("ANT_DATABASE_URL", "postgres://user:secret@127.0.0.1/unreachable")
	t.Setenv("ANT_ROOT_KEY_FILE", "")
	if code := run(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil))); code != 1 {
		t.Fatalf("exit %d", code)
	}
}
func TestServiceStartupAndRestart(t *testing.T) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("integration DSN required")
		}
		t.Skip("real PostgreSQL fixture required")
	}
	path := filepath.Join(t.TempDir(), "root.key")
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANT_DATABASE_URL", dsn)
	t.Setenv("ANT_ROOT_KEY_FILE", path)
	t.Setenv("ANT_LISTEN", "127.0.0.1:0")
	for range 2 {
		address := make(chan string, 1)
		done := make(chan int, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { done <- run(ctx, slog.New(listenLog{address})) }()
		var addr string
		select {
		case addr = <-address:
		case code := <-done:
			t.Fatalf("startup failed: %d", code)
		case <-time.After(15 * time.Second):
			t.Fatal("startup timeout")
		}
		client := http.Client{Timeout: 3 * time.Second}
		r, err := client.Get("http://" + addr + "/health/ready")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("readiness %d", r.StatusCode)
		}
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("shutdown exit %d", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("shutdown timeout")
		}
	}
}
