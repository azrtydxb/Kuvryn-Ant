package main

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type addrLog struct{ addrs chan [2]string }

func (h addrLog) Enabled(context.Context, slog.Level) bool { return true }
func (h addrLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "service listening" || r.Message == "federation listener" {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "address" {
				h.addrs <- [2]string{r.Message, a.Value.String()}
			}
			return true
		})
	}
	return nil
}
func (h addrLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h addrLog) WithGroup(string) slog.Handler      { return h }

// The federation listener is a separate TLS-only server: the public mux never
// serves peer routes, and an instance with federation configured but not
// founded serves and reports ready normally.
func TestFederationListenerIsSeparateAndOptional(t *testing.T) {
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
	// Federation needs artifact serving, which needs a bootstrapped instance
	// (an earlier test may already have bootstrapped this database).
	_ = execute(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"bootstrap", "federation-test"}, io.Discard)
	t.Setenv("ANT_FEDERATION_LISTEN", "127.0.0.1:0")
	t.Setenv("ANT_FEDERATION_PUBLIC_URL", "https://127.0.0.1:8444")
	addrs := make(chan [2]string, 4)
	done := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- run(ctx, slog.New(addrLog{addrs})) }()
	var public, federation string
	for public == "" || federation == "" {
		select {
		case a := <-addrs:
			if a[0] == "service listening" {
				public = a[1]
			} else {
				federation = a[1]
			}
		case code := <-done:
			t.Fatalf("startup failed: %d", code)
		case <-time.After(20 * time.Second):
			t.Fatal("startup timeout")
		}
	}
	if public == federation {
		t.Fatal("federation listener shares the public listener")
	}
	client := http.Client{Timeout: 3 * time.Second}
	get := func(url string) int {
		r, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.StatusCode
	}
	if c := get("http://" + public + "/health/ready"); c != 200 {
		t.Fatalf("readiness with federation configured: %d", c)
	}
	if c := get("http://" + public + "/federation/v1/heads"); c == 200 {
		t.Fatal("peer route served on the public listener")
	}
	if c := get("http://" + public + "/api/v1/federation/status"); c != 401 {
		t.Fatalf("federation status without credentials: %d", c)
	}
	// Plain HTTP is not served on the federation port, and TLS fails until the
	// instance founds or joins a federation (no member certificate yet).
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", federation, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}) //nolint:gosec // probing only
	if err == nil {
		conn.Close()
		t.Fatal("federation listener completed a handshake without a member certificate")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("shutdown exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown timeout")
	}
}
