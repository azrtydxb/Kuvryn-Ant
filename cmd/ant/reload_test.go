package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// A server started on an empty database must start serving the data routes as
// soon as an instance is bootstrapped, without a process restart.
func TestServerServesDataRoutesAfterBootstrap(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	old := bootstrapPoll
	bootstrapPoll = 100 * time.Millisecond
	t.Cleanup(func() { bootstrapPoll = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := filepath.Join(t.TempDir(), "root-key")
	if err := os.WriteFile(key, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid test DSN")
		}
		query := parsed.Query()
		query.Set("search_path", cfg.Database.ConnConfig.RuntimeParams["search_path"])
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + cfg.Database.ConnConfig.RuntimeParams["search_path"]
	}
	t.Setenv("ANT_DATABASE_URL", dsn)
	t.Setenv("ANT_ROOT_KEY_FILE", key)
	t.Setenv("ANT_DATA_DIR", cfg.Storage.Directory)
	t.Setenv("ANT_LISTEN", "127.0.0.1:0")
	t.Setenv("ANT_STORAGE_BACKEND", "disk")
	t.Setenv("ANT_SHUTDOWN_TIMEOUT", "1s")
	addresses := make(chan string, 8)
	done := make(chan int, 1)
	go func() { done <- execute(ctx, slog.New(listenLog{addresses}), nil, io.Discard) }()
	var first string
	select {
	case first = <-addresses:
	case code := <-done:
		t.Fatalf("startup failed: %d", code)
	case <-time.After(15 * time.Second):
		t.Fatal("startup timeout")
	}
	get := func(addr string) (int, string) {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/feeds/none/raw/x")
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if status, body := get(first); status != 404 || !strings.Contains(body, "page not found") {
		t.Fatalf("empty database should have no data routes: %d %q", status, body)
	}
	if _, err := identity.Bootstrap(ctx, pool, "Reload"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case second := <-addresses:
			// Routes are mounted: the adapter, not the mux, answers.
			status, body := get(second)
			if strings.Contains(body, "page not found") {
				t.Fatalf("data routes still missing after reload: %d %q", status, body)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown timeout")
			}
			return
		case code := <-done:
			t.Fatalf("server exited %d instead of reloading", code)
		case <-time.After(time.Until(deadline)):
			t.Fatal("server did not reload after bootstrap")
		}
	}
}
