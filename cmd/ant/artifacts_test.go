package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func runtimeFixture(t *testing.T) (*pgxpool.Pool, config.Config) {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture required")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_runtime_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid fixture DSN")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool, config.Config{Database: cfg, Storage: storage.Config{Directory: t.TempDir()}, RootKey: make([]byte, 32), ShutdownTimeout: time.Second, MaxRequests: 16}
}
func TestArtifactRuntimeBootstrapLeaseAndDrainLifetime(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	absent, err := openArtifacts(ctx, pool, cfg)
	if err != nil || absent != nil {
		t.Fatal("unbootstrapped instance should not activate artifact serving", err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Runtime")
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := openArtifacts(ctx, pool, cfg)
	if err != nil || runtime == nil {
		t.Fatal("bootstrap did not activate artifact serving", err)
	}
	defer runtime.Close()
	lease := runtime.manager.Lease()
	if lease.InstanceID != owner.InstanceID || lease.OwnerID == "" {
		t.Fatal("wrong serving lease")
	}
	mux := http.NewServeMux()
	runtime.Register(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if response.Code != 401 || response.Header().Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatal("artifact route not registered or unprotected")
	}
	if duplicate, err := openArtifacts(ctx, pool, cfg); !errors.Is(err, ownership.ErrHeld) || duplicate != nil {
		t.Fatal("second process acquired same instance", err)
	}
	// Signal cancellation starts HTTP drain. The writer lease must remain valid
	// until the runtime is explicitly closed after that drain completes.
	cancel()
	select {
	case <-runtime.manager.Done():
		t.Fatal("signal cancellation released ownership before drain")
	default:
	}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = ownership.Require(context.Background(), tx, lease); err != nil {
		tx.Rollback(context.Background())
		t.Fatal("writer lost during drain", err)
	}
	tx.Rollback(context.Background())
	runtime.Close()
	successor, err := ownership.Acquire(context.Background(), pool, owner.InstanceID, "00000000-0000-4000-8000-000000000099", time.Second)
	if err != nil || successor.Epoch <= lease.Epoch {
		t.Fatal("post-drain release failed", err)
	}
}
func TestArtifactRuntimeStartupFailureReleasesOwnership(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	ctx := context.Background()
	owner, err := identity.Bootstrap(ctx, pool, "Runtime")
	if err != nil {
		t.Fatal(err)
	}
	bad := cfg
	bad.Storage.Backend = "invalid"
	if runtime, err := openArtifacts(ctx, pool, bad); err == nil || runtime != nil {
		t.Fatal("invalid storage startup accepted")
	}
	cfg.InstanceID = strings.ToUpper(owner.InstanceID)
	runtime, err := openArtifacts(ctx, pool, cfg)
	if err != nil {
		t.Fatal("failed startup retained writer lease", err)
	}
	runtime.Close()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if runtime, err := openArtifacts(canceled, pool, cfg); err == nil || runtime != nil {
		t.Fatal("canceled startup activated writer")
	}
	lease, err := ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000099", time.Second)
	if err != nil {
		t.Fatal("canceled startup retained lease", err)
	}
	if err = ownership.Release(ctx, pool, lease); err != nil {
		t.Fatal(err)
	}
}
func TestServingStopsWhenInstanceLeaseIsLost(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := identity.Bootstrap(ctx, pool, "Runtime")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "root-key")
	if err = os.WriteFile(key, make([]byte, 32), 0600); err != nil {
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
	t.Setenv("ANT_INSTANCE_ID", owner.InstanceID)
	t.Setenv("ANT_STORAGE_BACKEND", "disk")
	t.Setenv("ANT_SHUTDOWN_TIMEOUT", "1s")
	address := make(chan string, 1)
	done := make(chan int, 1)
	go func() { done <- execute(ctx, slog.New(listenLog{address}), nil, io.Discard) }()
	select {
	case <-address:
	case code := <-done:
		t.Fatalf("startup failed: %d", code)
	case <-time.After(10 * time.Second):
		t.Fatal("startup timeout")
	}
	if _, err = pool.Exec(ctx, "UPDATE instance_writer_leases SET epoch=epoch+1,owner_id='00000000-0000-4000-8000-000000000099',expires_at=clock_timestamp()+interval '1 minute' WHERE instance_id=$1", owner.InstanceID); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("lease loss exited %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serving did not stop after ownership loss")
	}
}
