package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func fixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("ANT_REQUIRE_INTEGRATION") == "1" {
			t.Fatal("ANT_TEST_DATABASE_URL required")
		}
		t.Skip("PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("cannot open fixture")
	}
	schema := fmt.Sprintf("ant_test_%d", time.Now().UnixNano())
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
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
func TestMigrationsConcurrentRestartAndIntegrity(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	failures := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- Migrate(ctx, pool) }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration count %d: %v", count, err)
	}
	// A fresh pool models process restart against the existing schema.
	restarted, err := Open(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, restarted); err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if _, err = pool.Exec(ctx, "UPDATE schema_migrations SET checksum='tampered'"); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum refusal: %v", err)
	}
}
func TestMigrationsRollbackAndFutureVersion(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	definitions, err := loadMigrations(migrations)
	if err != nil {
		t.Fatal(err)
	}
	broken := append(definitions, migration{2, "002_test.sql", "CREATE TABLE rollback_probe(id integer); SELECT definitely_missing_function();", "test"})
	if err := applyMigrations(ctx, pool, broken); err == nil {
		t.Fatal("expected migration failure")
	}
	var missing bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('rollback_probe') IS NULL").Scan(&missing); err != nil || !missing {
		t.Fatalf("DDL was not rolled back: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal("previous schema no longer usable", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations(version,name,checksum) VALUES(2,'future','unknown')"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected downgrade refusal: %v", err)
	}
}
func TestDomainInvariants(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var a, b, feed, source, component, version, domain, blob string
	insert := func(dest *string, sql string, args ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	insert(&a, "INSERT INTO instances(name) VALUES('a') RETURNING id")
	insert(&b, "INSERT INTO instances(name) VALUES('b') RETURNING id")
	insert(&feed, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','raw') RETURNING id", a)
	insert(&source, "INSERT INTO sources(instance_id,name,kind) VALUES($1,'local','local') RETURNING id", a)
	insert(&component, "INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'example') RETURNING id", a, source)
	insert(&version, "INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1.0') RETURNING id", a, component)
	insert(&domain, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,'default') RETURNING id", a)
	insert(&blob, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,10,'blob') RETURNING id", a, domain, strings.Repeat("a", 64))
	if _, err := pool.Exec(ctx, "INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'archive',$3)", a, version, blob); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, sql string
		args      []any
	}{
		{"duplicate feed", "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','raw')", []any{a}},
		{"cross instance membership", "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", []any{b, feed, source}},
		{"duplicate coordinate", "INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'example')", []any{a, source}},
		{"duplicate version", "INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,'1.0')", []any{a, component}},
		{"duplicate asset", "INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'archive',$3)", []any{a, version, blob}},
		{"cross instance asset", "INSERT INTO assets(instance_id,version_id,asset_key,blob_id) VALUES($1,$2,'other',$3)", []any{b, version, blob}},
		{"referenced blob deletion", "DELETE FROM blobs WHERE id=$1", []any{blob}},
		{"invalid digest", "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,'bad',1,'bad')", []any{a, domain}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, tt.sql, tt.args...); err == nil {
				t.Fatal("invariant accepted invalid data")
			}
		})
	}
	// Instance-local naming remains independent.
	if _, err := pool.Exec(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','raw')", b); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO audit_events(instance_id,action) VALUES($1,'rollback-test')", a); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'packages','raw')", a); err == nil {
		t.Fatal("expected duplicate")
	}
	_ = tx.Rollback(ctx)
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='rollback-test'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("audit transaction did not roll back: %v", err)
	}
}
