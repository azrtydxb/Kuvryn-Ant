// Package publishpath measures the artifact publish path (authorization,
// staging, finalize, metadata transaction) against a real PostgreSQL and a disk
// blob store, optionally with object-store latency injected into the calls the
// S3 backend makes (Stage/Finalize/Stat). It exists to find serialization in
// the publish transaction: with the per-instance writer-lease row and the feed
// row held exclusively until commit, throughput is 1/(commit-path latency) no
// matter how many publishers run; with shared locks it scales with concurrency.
//
//	ANT_BENCH_STORE_LATENCY=20ms scripts/test-postgres.sh ./bench/publishpath -run '^$' -bench Publish -benchtime 200x
//
// Results are reported as req/s. Without ANT_TEST_DATABASE_URL the benchmark
// skips.
package publishpath

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// unique numbers objects across the repeated runs the testing package makes.
var unique atomic.Int64

// slowStore adds fixed latency per object-store round trip.
type slowStore struct {
	blob.Store
	d time.Duration
}

type slowUpload struct {
	blob.Upload
	d time.Duration
}

func (s slowStore) Stage(ctx context.Context) (blob.Upload, error) {
	time.Sleep(s.d)
	u, err := s.Store.Stage(ctx)
	return slowUpload{u, s.d}, err
}
func (s slowStore) Stat(ctx context.Context, d blob.Digest) (blob.Info, error) {
	time.Sleep(s.d)
	return s.Store.Stat(ctx, d)
}
func (u slowUpload) Finalize(ctx context.Context, i blob.Info) (blob.Info, error) {
	time.Sleep(2 * u.d) // upload part + copy/complete
	return u.Upload.Finalize(ctx, i)
}

func BenchmarkPublish(b *testing.B) {
	dsn := os.Getenv("ANT_TEST_DATABASE_URL")
	if dsn == "" {
		b.Skip("PostgreSQL required (scripts/test-postgres.sh)")
	}
	latency, _ := time.ParseDuration(os.Getenv("ANT_BENCH_STORE_LATENCY"))
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	schema := "ant_bench_" + strings.ToLower(rand.Text())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		b.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 32
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		b.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Bench")
	if err != nil {
		b.Fatal(err)
	}
	var feed, source, domain string
	if err = pool.QueryRow(ctx, `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'bench','raw') RETURNING id`, owner.InstanceID).Scan(&feed); err != nil {
		b.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','raw') RETURNING id`, owner.InstanceID).Scan(&source); err != nil {
		b.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)`, owner.InstanceID, feed, source); err != nil {
		b.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id`, owner.InstanceID, "source:"+source).Scan(&domain); err != nil {
		b.Fatal(err)
	}
	disk, err := blob.NewDisk(filepath.Join(b.TempDir(), "blobs"), blob.TransferBufferSize*64)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { disk.Close() })
	var store blob.Store = disk
	if latency > 0 {
		store = slowStore{disk, latency}
	}
	lease, err := ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000001", ownership.MaxTTL)
	if err != nil {
		b.Fatal(err)
	}
	router, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: source, Kind: routing.Local}}, DefaultTarget: source})
	if err != nil {
		b.Fatal(err)
	}
	svc := &artifact.Service{DB: pool, Lease: func() ownership.Lease { return lease }, Router: func(context.Context, string) (*routing.Router, error) { return router, nil }, Storage: func(context.Context, string) (blob.Store, error) { return store, nil }}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	principal := identity.Principal{InstanceID: owner.InstanceID, PrincipalID: owner.PrincipalID, TokenID: token.ID, Scopes: []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}}

	for _, workers := range []int{1, 4, 16} {
		b.Run(fmt.Sprintf("c%d", workers), func(b *testing.B) {
			var seq, failed atomic.Int64
			// ANT_BENCH_WAITS=1 samples what the database sessions wait on
			// (Lock/transactionid/tuple/relation mean serialization on rows).
			waits := map[string]int{}
			stopWaits := make(chan struct{})
			sampled := make(chan struct{})
			go func() {
				defer close(sampled)
				if os.Getenv("ANT_BENCH_WAITS") == "" {
					return
				}
				for {
					select {
					case <-stopWaits:
						return
					case <-time.After(20 * time.Millisecond):
					}
					rows, err := admin.Query(ctx, `SELECT coalesce(wait_event_type,'')||'/'||coalesce(wait_event,'') FROM pg_stat_activity WHERE datname=current_database() AND state='active' AND pid<>pg_backend_pid() AND query NOT LIKE 'SELECT coalesce(wait_event_type%'`)
					if err != nil {
						return
					}
					for rows.Next() {
						var w string
						if rows.Scan(&w) == nil {
							waits[w]++
						}
					}
					rows.Close()
				}
			}()
			b.SetBytes(64 << 10)
			b.ResetTimer()
			start := time.Now()
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					body := make([]byte, 64<<10)
					for {
						n := seq.Add(1)
						if n > int64(b.N) {
							return
						}
						_, _ = rand.Read(body)
						sum := sha256.Sum256(body)
						name := fmt.Sprintf("obj/%d", unique.Add(1))
						_, err := svc.Publish(ctx, principal, artifact.Publication{FeedID: feed, Coordinate: name, Version: "1", AssetKey: name, MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(body))}}, bytes.NewReader(body))
						if err != nil {
							if failed.Add(1) == 1 {
								b.Logf("first failure: %v", err)
							}
						}
					}
				}()
			}
			wg.Wait()
			close(stopWaits)
			<-sampled
			if len(waits) > 0 {
				b.Logf("active session wait samples: %v", waits)
			}
			b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "req/s")
			if failed.Load() > 0 {
				b.Fatalf("%d publishes failed", failed.Load())
			}
		})
	}
}
