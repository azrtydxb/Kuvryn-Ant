package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/azrtydxb/Kuvryn-Ant/internal/storage"
)

func TestGCCommandDryRunThenTwoPhaseSweep(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	ctx := context.Background()
	owner, err := identity.Bootstrap(ctx, pool, "gc")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Retention.Grace = time.Minute
	var domain string
	if err = pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,'gc-domain') RETURNING id::text", owner.InstanceID).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.New(cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close()
	store, err := stores.Resolve(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("orphaned by a failed metadata commit")
	s := sha256.Sum256(data)
	info := blob.Info{Digest: blob.Digest(hex.EncodeToString(s[:])), Size: int64(len(data))}
	up, err := store.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = up.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err = up.Finalize(ctx, info); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key,created_at) VALUES($1,$2,$3,$4,$3,now()-interval '2 days')", owner.InstanceID, domain, info.Digest, info.Size); err != nil {
		t.Fatal(err)
	}
	run := func(dry bool) retention.Report {
		var out bytes.Buffer
		if err := runGC(ctx, pool, cfg, dry, &out); err != nil {
			t.Fatal(err)
		}
		var r retention.Report
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err, out.String())
		}
		return r
	}
	dry := run(true)
	if !dry.DryRun || len(dry.GC.Marked) != 1 {
		t.Fatalf("dry-run report %+v", dry)
	}
	var candidates int
	if err = pool.QueryRow(ctx, "SELECT count(*) FROM gc_candidates").Scan(&candidates); err != nil || candidates != 0 {
		t.Fatal("dry-run wrote state", err)
	}
	if rep := run(false); len(rep.GC.Marked) != 1 || len(rep.GC.Swept) != 0 {
		t.Fatalf("first run %+v", rep)
	}
	if _, err = pool.Exec(ctx, "UPDATE gc_candidates SET first_seen_at=first_seen_at-interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if rep := run(false); len(rep.GC.Swept) != 1 {
		t.Fatalf("second run %+v", rep)
	}
	if _, err = store.Stat(ctx, info.Digest); err == nil {
		t.Fatal("bytes survived the sweep")
	}
}

func TestGCUsageRejectsUnknownArguments(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"gc", "--force"}, {"gc", "--dry-run", "x"}, {"serve", "--dry-run"}} {
		if code := execute(context.Background(), logger, args, io.Discard); code != 1 {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestRetentionWorkerRunsUnderInstanceLease(t *testing.T) {
	pool, cfg := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner, err := identity.Bootstrap(ctx, pool, "worker")
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys, _ = secrets.New("v1", map[string][]byte{"v1": bytes.Repeat([]byte{1}, 32)})
	runtime, err := openArtifacts(ctx, pool, cfg)
	if err != nil || runtime == nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	var domain string
	if err = pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,'worker-domain') RETURNING id::text", owner.InstanceID).Scan(&domain); err != nil {
		t.Fatal(err)
	}
	digest := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if _, err = pool.Exec(ctx, "INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key,created_at) VALUES($1,$2,$3,1,$3,now()-interval '2 days')", owner.InstanceID, domain, digest); err != nil {
		t.Fatal(err)
	}
	workers, err := startWorkers(ctx, pool, runtime, cfg.SecretKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer workers.Close()
	cfg.Retention.Interval, cfg.Retention.Grace = time.Minute, time.Minute
	workers.startRetention(pool, runtime, cfg.Retention, slog.New(slog.NewTextHandler(io.Discard, nil)))
	deadline := time.Now().Add(10 * time.Second)
	for {
		var marked int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM gc_candidates WHERE digest=$1", digest).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background worker never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Disabled interval and absent runtime are inert.
	workers.startRetention(pool, nil, cfg.Retention, nil)
	cfg.Retention.Interval = 0
	workers.startRetention(pool, runtime, cfg.Retention, nil)
	(*backgroundWorkers)(nil).startRetention(pool, runtime, cfg.Retention, nil)
}
