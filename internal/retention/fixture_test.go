package retention

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type fixture struct {
	pool                 *pgxpool.Pool
	svc                  *Service
	disk                 *blob.Disk
	root                 string
	instance             string
	feed, source, domain string
	artifacts            *artifact.Service
	principal            identity.Principal
	lease                ownership.Lease
}

func newFixture(t *testing.T, opts Options) *fixture {
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
	schema := fmt.Sprintf("ant_retention_%d", time.Now().UnixNano())
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
	cfg.MaxConns = 16
	pool, err := database.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	owner, err := identity.Bootstrap(ctx, pool, "Retention")
	if err != nil {
		t.Fatal(err)
	}
	token, err := (identity.Tokens{DB: pool}).Issue(ctx, owner.InstanceID, owner.PrincipalID, owner.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := (identity.Tokens{DB: pool}).Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, instance: owner.InstanceID, principal: principal}
	f.feed, f.source, f.domain = f.newSource(t, "local", "local")
	f.root = filepath.Join(t.TempDir(), "blobs")
	f.disk, err = blob.NewDisk(f.root, blob.TransferBufferSize*8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.disk.Close() })
	f.lease, err = ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000001", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: f.source, Kind: routing.Local}}, DefaultTarget: f.source})
	if err != nil {
		t.Fatal(err)
	}
	f.artifacts = &artifact.Service{DB: pool, Lease: func() ownership.Lease { return f.lease }, Router: func(context.Context, string) (*routing.Router, error) { return compiled, nil }, Storage: func(context.Context, string) (blob.Store, error) { return f.disk, nil }}
	f.svc = &Service{DB: pool, InstanceID: owner.InstanceID, Options: opts, Lease: func() ownership.Lease { return f.lease }, Stores: func(_ context.Context, domain string) (blob.Store, error) {
		if domain != f.domain {
			return nil, errors.New("unexpected domain")
		}
		return f.disk, nil
	}}
	return f
}

// newSource creates a feed with one attached source and its dedup domain.
func (f *fixture) newSource(t *testing.T, slug, kind string) (feed, source, domain string) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,$2,'raw') RETURNING id::text", f.instance, slug).Scan(&feed))
	must(f.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,$2,$3,'raw') RETURNING id::text", f.instance, slug, kind).Scan(&source))
	_, err := f.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", f.instance, feed, source)
	must(err)
	must(f.pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id::text", f.instance, "source:"+source).Scan(&domain))
	return
}

func sum(data []byte) blob.Info {
	s := sha256.Sum256(data)
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(s[:])), Size: int64(len(data))}
}

func (f *fixture) publication(version string, data []byte) artifact.Publication {
	return artifact.Publication{FeedID: f.feed, Namespace: "team", Coordinate: "team/package", Version: version, AssetKey: "artifact.bin", MediaType: "application/octet-stream", Info: sum(data)}
}

func (f *fixture) publish(t *testing.T, version string, data []byte) artifact.Record {
	t.Helper()
	rec, err := f.artifacts.Publish(context.Background(), f.principal, f.publication(version, data), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fixture) hasBytes(d blob.Digest) bool {
	_, err := f.disk.Stat(context.Background(), d)
	return err == nil
}

// orphan publishes data then removes its metadata reference, leaving an
// unreferenced blob row plus bytes (what a retention delete or rollback leaves).
func (f *fixture) orphan(t *testing.T, version string, data []byte) blob.Info {
	t.Helper()
	f.publish(t, version, data)
	f.exec(t, "DELETE FROM tags")
	f.exec(t, "DELETE FROM assets WHERE version_id IN (SELECT id FROM versions WHERE version=$1)", version)
	return sum(data)
}

func (f *fixture) ageBlobs(t *testing.T, d time.Duration) {
	t.Helper()
	f.exec(t, "UPDATE blobs SET created_at=created_at-$1::interval", fmt.Sprintf("%d seconds", int(d.Seconds())))
}

func (f *fixture) ageCandidates(t *testing.T, d time.Duration) {
	t.Helper()
	f.exec(t, "UPDATE gc_candidates SET first_seen_at=first_seen_at-$1::interval", fmt.Sprintf("%d seconds", int(d.Seconds())))
}

func digests(refs []BlobRef) map[string]string {
	out := map[string]string{}
	for _, r := range refs {
		out[r.Digest] = r.Kind
	}
	return out
}
