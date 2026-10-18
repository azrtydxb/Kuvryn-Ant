package ownership

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ownerA = "00000000-0000-4000-8000-000000000001"
const ownerB = "00000000-0000-4000-8000-000000000002"

func fixture(t *testing.T) (*pgxpool.Pool, string) {
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
	schema := fmt.Sprintf("ant_ownership_%d", time.Now().UnixNano())
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
	var instance string
	if err = pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('writer') RETURNING id").Scan(&instance); err != nil {
		t.Fatal(err)
	}
	return pool, instance
}
func expire(t *testing.T, pool *pgxpool.Pool, instance string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "UPDATE instance_writer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1", instance); err != nil {
		t.Fatal(err)
	}
}
func require(t *testing.T, pool *pgxpool.Pool, lease Lease) error {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	return Require(context.Background(), tx, lease)
}

func TestWriterLeaseLifecycleAndTakeover(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	lease, err := Acquire(ctx, pool, strings.ToUpper(instance), ownerA, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if lease.InstanceID != instance || lease.Epoch < 1 || lease.OwnerID != ownerA {
		t.Fatal("invalid acquired lease")
	}
	var valid bool
	if err = pool.QueryRow(ctx, "SELECT $1::timestamptz>clock_timestamp() AND $1::timestamptz<=clock_timestamp()+interval '30 seconds'", lease.ExpiresAt).Scan(&valid); err != nil || !valid {
		t.Fatal("lease expiry not database bounded", err)
	}
	if err = require(t, pool, lease); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{ownerA, ownerB} {
		if _, err = Acquire(ctx, pool, instance, owner, 30*time.Second); !errors.Is(err, ErrHeld) {
			t.Fatal("live lease acquired twice", err)
		}
	}
	renewed, err := Renew(ctx, pool, lease, time.Minute)
	if err != nil || renewed.Epoch != lease.Epoch || !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatal("renewal changed fencing identity", err)
	}
	forged := renewed
	forged.OwnerID = ownerB
	if err = require(t, pool, forged); !errors.Is(err, ErrLost) {
		t.Fatal("wrong owner fenced incorrectly", err)
	}
	forged = renewed
	forged.Epoch++
	if err = require(t, pool, forged); !errors.Is(err, ErrLost) {
		t.Fatal("wrong epoch accepted", err)
	}
	if err = Release(ctx, pool, renewed); err != nil {
		t.Fatal(err)
	}
	if err = require(t, pool, renewed); !errors.Is(err, ErrLost) {
		t.Fatal("released lease valid", err)
	}
	newer, err := Acquire(ctx, pool, instance, ownerB, 30*time.Second)
	if err != nil || newer.Epoch <= renewed.Epoch {
		t.Fatal("takeover did not advance epoch", err)
	}
	if _, err = Renew(ctx, pool, renewed, time.Minute); !errors.Is(err, ErrLost) {
		t.Fatal("stale holder renewed", err)
	}
	if err = Release(ctx, pool, renewed); !errors.Is(err, ErrLost) {
		t.Fatal("stale holder released newer lease", err)
	}
	expire(t, pool, instance)
	newer.ExpiresAt = time.Now().Add(time.Hour)
	if err = require(t, pool, newer); !errors.Is(err, ErrLost) {
		t.Fatal("caller expiry trusted", err)
	}
	latest, err := Acquire(ctx, pool, instance, ownerB, 30*time.Second)
	if err != nil || latest.Epoch <= newer.Epoch {
		t.Fatal("same owner reacquisition reused epoch", err)
	}
}

func TestWriterLeaseConcurrentAcquireAndExpiredRenew(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	results := make(chan error, 8)
	leases := make(chan Lease, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := Acquire(ctx, pool, instance, ownerA, 30*time.Second)
			results <- err
			if err == nil {
				leases <- lease
			}
		}()
	}
	wg.Wait()
	close(results)
	close(leases)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrHeld) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("acquisition winners=%d", wins)
	}
	old := <-leases
	expire(t, pool, instance)
	var renewalErr, takeoverErr error
	var current Lease
	wg.Add(2)
	go func() { defer wg.Done(); _, renewalErr = Renew(ctx, pool, old, time.Minute) }()
	go func() { defer wg.Done(); current, takeoverErr = Acquire(ctx, pool, instance, ownerB, time.Minute) }()
	wg.Wait()
	if !errors.Is(renewalErr, ErrLost) || takeoverErr != nil || current.Epoch <= old.Epoch {
		t.Fatalf("expired renewal/takeover race: %v %v", renewalErr, takeoverErr)
	}
}

func TestWriterLeaseTransactionFenceBlocksTakeover(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	lease, err := Acquire(ctx, pool, instance, ownerA, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = Require(ctx, tx, lease); err != nil {
		t.Fatal(err)
	}
	// Expire under our already-held fence lock, modeling time passing during the
	// metadata transaction without sleeps. A new writer must still wait to commit.
	if _, err = tx.Exec(ctx, "UPDATE instance_writer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1", instance); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'fenced','raw')", instance); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err = Acquire(waitCtx, pool, instance, ownerB, time.Minute); !errors.Is(err, ErrUnavailable) {
		t.Fatal("takeover bypassed transaction fence", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	taken, err := Acquire(ctx, pool, instance, ownerB, time.Minute)
	if err != nil || taken.Epoch <= lease.Epoch {
		t.Fatal("takeover after metadata commit failed", err)
	}
	if err = require(t, pool, lease); !errors.Is(err, ErrLost) {
		t.Fatal("old transaction identity usable after takeover", err)
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM feeds WHERE slug='fenced'").Scan(&count); err != nil || count != 1 {
		t.Fatal("fenced metadata not committed", err)
	}
}

func TestWriterLeaseRejectsInputsAndMissingLease(t *testing.T) {
	pool, instance := fixture(t)
	ctx := context.Background()
	for _, ttl := range []time.Duration{0, time.Second - time.Nanosecond, MaxTTL + time.Second, time.Second + time.Nanosecond} {
		if _, err := Acquire(ctx, pool, instance, ownerA, ttl); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid TTL accepted")
		}
	}
	if _, err := Acquire(ctx, pool, instance, "secret-invalid-owner", time.Second); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "secret-invalid") {
		t.Fatal("invalid UUID accepted or leaked")
	}
	if err := require(t, pool, Lease{InstanceID: instance, OwnerID: ownerA, Epoch: 1}); !errors.Is(err, ErrLost) {
		t.Fatal("missing lease accepted", err)
	}
}
