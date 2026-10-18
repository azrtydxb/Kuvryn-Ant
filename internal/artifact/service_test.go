package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type publishFixture struct {
	pool                 *pgxpool.Pool
	service              *Service
	principal            identity.Principal
	feed, source, domain string
	disk                 *blob.Disk
}

func newPublishFixture(t *testing.T) publishFixture {
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
	schema := fmt.Sprintf("ant_publish_%d", time.Now().UnixNano())
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
	owner, err := identity.Bootstrap(ctx, pool, "Artifacts")
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
	f := publishFixture{pool: pool, principal: principal}
	if err = pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'artifacts','raw') RETURNING id", owner.InstanceID).Scan(&f.feed); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'local','local','raw') RETURNING id", owner.InstanceID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,0)", owner.InstanceID, f.feed, f.source); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, "INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) RETURNING id", owner.InstanceID, "source:"+f.source).Scan(&f.domain); err != nil {
		t.Fatal(err)
	}
	lease, err := ownership.Acquire(ctx, pool, owner.InstanceID, "00000000-0000-4000-8000-000000000001", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.disk, err = blob.NewDisk(filepath.Join(t.TempDir(), "blobs"), blob.TransferBufferSize*8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.disk.Close(); err != nil {
			t.Error(err)
		}
	})
	compiled, err := routing.Compile(routing.Config{Sources: []routing.Source{{ID: f.source, Kind: routing.Local}}, DefaultTarget: f.source})
	if err != nil {
		t.Fatal(err)
	}
	f.service = &Service{DB: pool, Lease: func() ownership.Lease { return lease }, Router: func(context.Context, string) (*routing.Router, error) { return compiled, nil }, Storage: func(_ context.Context, domain string) (blob.Store, error) {
		if domain != f.domain {
			return nil, errors.New("unexpected domain")
		}
		return f.disk, nil
	}}
	return f
}
func (f publishFixture) publication(data []byte) Publication {
	sum := sha256.Sum256(data)
	return Publication{FeedID: f.feed, Namespace: "team", Coordinate: "team/package", Version: "1.0", AssetKey: "artifact.bin", MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(hex.EncodeToString(sum[:])), Size: int64(len(data))}}
}
func (f publishFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (f publishFixture) assertInvisible(t *testing.T) {
	t.Helper()
	for _, table := range []string{"components", "versions", "assets", "blobs"} {
		if f.count(t, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()) != 0 {
			t.Fatalf("partial metadata in %s", table)
		}
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.publish'") != 0 {
		t.Fatal("partial publication audit")
	}
}

func TestPublishConcurrentImmutableIdempotency(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	data := []byte("verified immutable artifact")
	publication := f.publication(data)
	results := make(chan Record, 6)
	failures := make(chan error, 6)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, err := f.service.Publish(ctx, f.principal, publication, bytes.NewReader(data))
			results <- record
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	fresh := 0
	id := ""
	for record := range results {
		if id == "" {
			id = record.ID
		}
		if record.ID != id || record.SourceID != f.source || record.DomainID != f.domain || record.Info != publication.Info {
			t.Fatal("retry returned different record")
		}
		if !record.Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh publications=%d", fresh)
	}
	for _, query := range []string{"SELECT count(*) FROM assets", "SELECT count(*) FROM blobs", "SELECT count(*) FROM audit_events WHERE action='artifact.publish'", "SELECT count(*) FROM outbox_events WHERE topic='artifact.publish'"} {
		if f.count(t, query) != 1 {
			t.Fatal("retry duplicated state", query)
		}
	}
	conflicting := []byte("different immutable artifact")
	if _, err := f.service.Publish(ctx, f.principal, f.publication(conflicting), bytes.NewReader(conflicting)); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting immutable bytes accepted", err)
	}
	if f.count(t, "SELECT count(*) FROM assets") != 1 || f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.publish'") != 1 {
		t.Fatal("conflict changed metadata")
	}
	reader, err := f.disk.OpenRange(ctx, publication.Info.Digest, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("published bytes changed", err)
	}
}

func TestPublishIntegrityBeforeVisibility(t *testing.T) {
	for _, scenario := range []string{"short", "long", "wrong-digest"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishFixture(t)
			data := []byte("artifact")
			publication := f.publication(data)
			switch scenario {
			case "short":
				publication.Info.Size++
			case "long":
				publication.Info.Size--
			case "wrong-digest":
				publication.Info.Digest = f.publication([]byte("other")).Info.Digest
			}
			if _, err := f.service.Publish(context.Background(), f.principal, publication, bytes.NewReader(data)); !errors.Is(err, blob.ErrIntegrity) {
				t.Fatal("invalid content accepted", err)
			}
			f.assertInvisible(t)
		})
	}
}

type countedReader struct{ reads atomic.Int32 }

func (r *countedReader) Read([]byte) (int, error) { r.reads.Add(1); return 0, io.EOF }
func TestPublishDeniedBeforeBodyConsumption(t *testing.T) {
	f := newPublishFixture(t)
	publication := f.publication([]byte("data"))
	ctx := context.Background()
	for _, scenario := range []string{"read-only", "cross-instance", "detached"} {
		t.Run(scenario, func(t *testing.T) {
			p := f.principal
			if scenario == "read-only" {
				p.Scopes = []identity.Scope{{Actions: []identity.Action{identity.Read}}}
			}
			if scenario == "cross-instance" {
				if err := f.pool.QueryRow(ctx, "INSERT INTO instances(name) VALUES('other') RETURNING id").Scan(&p.InstanceID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "detached" {
				if _, err := f.pool.Exec(ctx, "DELETE FROM feed_sources WHERE feed_id=$1", f.feed); err != nil {
					t.Fatal(err)
				}
			}
			reader := &countedReader{}
			if _, err := f.service.Publish(ctx, p, publication, reader); err == nil {
				t.Fatal("unauthorized publication accepted")
			}
			if reader.reads.Load() != 0 {
				t.Fatal("consumed unauthorized body")
			}
			f.assertInvisible(t)
		})
	}
}

type finalizeHookStore struct {
	blob.Store
	hook func() error
}

func (s finalizeHookStore) Stage(ctx context.Context) (blob.Upload, error) {
	upload, err := s.Store.Stage(ctx)
	if err != nil {
		return nil, err
	}
	return &finalizeHookUpload{Upload: upload, hook: s.hook}, nil
}

type finalizeHookUpload struct {
	blob.Upload
	hook func() error
}

func (u *finalizeHookUpload) Finalize(ctx context.Context, expected blob.Info) (blob.Info, error) {
	info, err := u.Upload.Finalize(ctx, expected)
	if err != nil {
		return info, err
	}
	if err = u.hook(); err != nil {
		return blob.Info{}, err
	}
	return info, nil
}
func TestPublishRejectsChangesAfterByteFinalization(t *testing.T) {
	for _, scenario := range []string{"lost-lease", "feed-revision", "audit-failure", "revoked-token"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublishFixture(t)
			ctx := context.Background()
			data := []byte("verified but not visible")
			publication := f.publication(data)
			hook := func() error {
				var err error
				switch scenario {
				case "revoked-token":
					_, err = f.pool.Exec(ctx, "UPDATE service_tokens SET revoked_at=clock_timestamp() WHERE id=$1", f.principal.TokenID)
				case "lost-lease":
					_, err = f.pool.Exec(ctx, "UPDATE instance_writer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE instance_id=$1", f.principal.InstanceID)
				case "feed-revision":
					_, err = f.pool.Exec(ctx, "UPDATE feeds SET revision=revision+1 WHERE id=$1", f.feed)
				case "audit-failure":
					_, err = f.pool.Exec(ctx, "ALTER TABLE audit_events ADD CONSTRAINT reject_publication CHECK(action<>'artifact.publish')")
				}
				return err
			}
			f.service.Storage = func(context.Context, string) (blob.Store, error) {
				return finalizeHookStore{Store: f.disk, hook: hook}, nil
			}
			_, err := f.service.Publish(ctx, f.principal, publication, bytes.NewReader(data))
			expected := ErrUnavailable
			if scenario == "revoked-token" {
				expected = ErrDenied
			}
			if scenario == "lost-lease" {
				expected = ownership.ErrLost
			}
			if scenario == "feed-revision" {
				expected = ErrConflict
			}
			if !errors.Is(err, expected) {
				t.Fatalf("expected %v, got %v", expected, err)
			}
			f.assertInvisible(t)
			if _, err = f.disk.Stat(ctx, publication.Info.Digest); err != nil {
				t.Fatal("fixture did not reach byte finalization", err)
			}
		})
	}
}

func TestPublishReferencesAndImmutableTags(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	layer := []byte("layer bytes")
	layerPub := f.publication(layer)
	layerPub.Version = "layer-v1"
	layerPub.AssetKey = "layer.bin"
	if _, err := f.service.Publish(ctx, f.principal, layerPub, bytes.NewReader(layer)); err != nil {
		t.Fatal(err)
	}
	manifest := []byte("manifest bytes")
	manifestPub := f.publication(manifest)
	manifestPub.Version = "manifest-v1"
	manifestPub.AssetKey = "manifest.json"
	manifestPub.Tag = "latest"
	manifestPub.References = []Reference{{Version: layerPub.Version, AssetKey: layerPub.AssetKey, Info: layerPub.Info}}
	broken := manifestPub
	broken.References = append([]Reference(nil), manifestPub.References...)
	broken.References[0].Version = "missing"
	if _, err := f.service.Publish(ctx, f.principal, broken, bytes.NewReader(manifest)); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing closure accepted", err)
	}
	broken.References[0] = manifestPub.References[0]
	broken.References[0].Info.Size++
	if _, err := f.service.Publish(ctx, f.principal, broken, bytes.NewReader(manifest)); !errors.Is(err, blob.ErrIntegrity) {
		t.Fatal("incorrect closure size accepted", err)
	}
	if f.count(t, "SELECT count(*) FROM assets") != 1 || f.count(t, "SELECT count(*) FROM tags") != 0 {
		t.Fatal("failed manifest became visible")
	}
	record, err := f.service.Publish(ctx, f.principal, manifestPub, bytes.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	newTag := manifestPub
	newTag.Tag = "stable"
	replay, err := f.service.Publish(ctx, f.principal, newTag, bytes.NewReader(manifest))
	if err != nil || !replay.Replayed || replay.ID != record.ID {
		t.Fatal("adding tag duplicated artifact", err)
	}
	if _, err = f.service.Publish(ctx, f.principal, newTag, bytes.NewReader(manifest)); err != nil {
		t.Fatal(err)
	}
	if f.count(t, "SELECT count(*) FROM tags") != 2 || f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.tag'") != 1 || f.count(t, "SELECT count(*) FROM outbox_events WHERE topic='artifact.tag'") != 1 {
		t.Fatal("tag retry duplicated or omitted audit")
	}
	conflict := manifestPub
	conflict.Version = "manifest-v2"
	if _, err = f.service.Publish(ctx, f.principal, conflict, bytes.NewReader(manifest)); !errors.Is(err, ErrConflict) {
		t.Fatal("immutable tag changed target", err)
	}
	if f.count(t, "SELECT count(*) FROM versions") != 2 || f.count(t, "SELECT count(*) FROM assets") != 2 {
		t.Fatal("tag conflict left metadata")
	}
}

func TestArtifactReadOwnerIsolationAndScopes(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	data := []byte("readable artifact")
	publication := f.publication(data)
	if _, err := f.service.Publish(ctx, f.principal, publication, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	record, reader, err := f.service.Open(ctx, f.principal, publication, 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(actual, data[1:5]) || record.SourceID != f.source {
		t.Fatal("scoped range read failed", err)
	}
	denied := f.principal
	denied.Scopes = []identity.Scope{{Actions: []identity.Action{identity.Publish}}}
	var resolutions atomic.Int32
	originalStorage := f.service.Storage
	f.service.Storage = func(ctx context.Context, domain string) (blob.Store, error) {
		resolutions.Add(1)
		return originalStorage(ctx, domain)
	}
	if _, reader, err = f.service.Open(ctx, denied, publication, 0, -1); !errors.Is(err, ErrNotFound) || reader != nil || resolutions.Load() != 0 {
		t.Fatal("unauthorized read touched storage", err)
	}
	// A higher-priority source containing this package owns all its versions,
	// even if it has no matching version/asset. Never fall back to the lower source.
	var preferred string
	if err = f.pool.QueryRow(ctx, "INSERT INTO sources(instance_id,name,kind,format) VALUES($1,'preferred','local','raw') RETURNING id", f.principal.InstanceID).Scan(&preferred); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, "INSERT INTO feed_sources(instance_id,feed_id,source_id,priority) VALUES($1,$2,$3,-1)", f.principal.InstanceID, f.feed, preferred); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, "INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,$3)", f.principal.InstanceID, preferred, publication.Coordinate); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Lookup(ctx, f.principal, publication); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing owner version fell through", err)
	}
	if _, reader, err = f.service.Open(ctx, f.principal, publication, 0, -1); !errors.Is(err, ErrNotFound) || reader != nil || resolutions.Load() != 0 {
		t.Fatal("fallback exposed lower source", err)
	}
}

func TestPublishPolicyCASAndDestinationValidation(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	policies := Policies{DB: f.pool, InstanceID: f.principal.InstanceID}
	// The token scope is the intersection even though its principal has admin role.
	if _, err := policies.Set(ctx, f.principal, f.feed, 1, f.source, nil); !errors.Is(err, ErrDenied) {
		t.Fatal("publish token configured policy", err)
	}
	token, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Configure}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	administrator, err := (identity.Tokens{DB: f.pool}).Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policies.Set(ctx, administrator, f.feed, 1, "00000000-0000-4000-8000-000000000099", nil); err == nil {
		t.Fatal("unattached destination accepted")
	}
	if f.count(t, "SELECT count(*) FROM publish_policies") != 0 {
		t.Fatal("invalid policy persisted")
	}
	rules := []routing.Rule{{ID: "deny-team", Deny: true, Selector: routing.Selector{Namespace: "team"}}}
	revision, err := policies.Set(ctx, administrator, f.feed, 1, f.source, rules)
	if err != nil || revision != 2 {
		t.Fatal("policy write failed", err)
	}
	if _, err = policies.Set(ctx, administrator, f.feed, 1, f.source, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("stale policy write accepted", err)
	}
	compiled, err := policies.Load(ctx, f.feed)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := compiled.Resolve(routing.Request{Namespace: "team"})
	if err != nil || decision.Allowed || decision.Reason != routing.DeniedByRule {
		t.Fatal("persisted deny lost", err)
	}
	f.service.Router = policies.Load
	reader := &countedReader{}
	if _, err = f.service.Publish(ctx, f.principal, f.publication([]byte("data")), reader); !errors.Is(err, ErrDenied) || reader.reads.Load() != 0 {
		t.Fatal("persisted policy deny bypassed", err)
	}
	revision, err = policies.Set(ctx, administrator, f.feed, 2, f.source, nil)
	if err != nil || revision != 3 {
		t.Fatal("policy update failed", err)
	}
	data := []byte("allowed")
	if _, err = f.service.Publish(ctx, f.principal, f.publication(data), bytes.NewReader(data)); err != nil {
		t.Fatal("updated policy not used", err)
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action='feed.publish_policy'") != 2 || f.count(t, "SELECT count(*) FROM outbox_events WHERE topic='feed.publish_policy'") != 2 {
		t.Fatal("policy audit count mismatch")
	}
}
