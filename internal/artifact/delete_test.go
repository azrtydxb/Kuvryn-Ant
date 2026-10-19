package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
)

func (f publishFixture) deleter(t *testing.T) identity.Principal {
	t.Helper()
	ctx := context.Background()
	tok, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err := (identity.Tokens{DB: f.pool}).Authenticate(ctx, tok.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f publishFixture) publish(t *testing.T, version, key string, data []byte) {
	t.Helper()
	pub := f.publication(data)
	pub.Version, pub.AssetKey = version, key
	if _, err := f.service.Publish(context.Background(), f.principal, pub, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func (f publishFixture) setPolicy(t *testing.T, namespace string, mode DeleteMode, immutable bool) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO delete_policies(instance_id,feed_id,namespace,mode,immutable_releases) VALUES($1,$2,$3,$4,$5) ON CONFLICT(feed_id,namespace) DO UPDATE SET mode=excluded.mode,immutable_releases=excluded.immutable_releases`, f.principal.InstanceID, f.feed, namespace, string(mode), immutable); err != nil {
		t.Fatal(err)
	}
}

func (f publishFixture) target(version, key string) Target {
	return Target{FeedID: f.feed, Namespace: "team", Coordinate: "team/package", Version: version, AssetKey: key}
}

func TestDeletePolicyMatrix(t *testing.T) {
	cases := []struct {
		name      string
		mode      DeleteMode
		immutable bool
		version   string
		del       error
		unlist    error
	}{
		{"allowed", DeleteAllowed, false, "1.0", nil, nil},
		{"denied", DeleteDenied, false, "1.0", ErrPolicy, ErrPolicy},
		{"unlist-only", DeleteUnlistOnly, false, "1.0", ErrUnlistOnly, nil},
		{"immutable release", DeleteAllowed, true, "1.0", ErrImmutable, nil},
		{"immutable snapshot is deletable", DeleteAllowed, true, "1.0-SNAPSHOT", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPublishFixture(t)
			ctx := context.Background()
			p := f.deleter(t)
			f.publish(t, c.version, "artifact.bin", []byte("payload "+c.name))
			f.setPolicy(t, "", c.mode, c.immutable)
			if _, err := f.service.Unlist(ctx, p, f.target(c.version, "")); !errors.Is(err, c.unlist) || (c.unlist == nil && err != nil) {
				t.Fatalf("unlist: got %v want %v", err, c.unlist)
			}
			if _, err := f.service.Delete(ctx, p, f.target(c.version, "")); !errors.Is(err, c.del) || (c.del == nil && err != nil) {
				t.Fatalf("delete: got %v want %v", err, c.del)
			}
			wantVersions := 1
			if c.del == nil {
				wantVersions = 0
			}
			if n := f.count(t, "SELECT count(*) FROM versions"); n != wantVersions {
				t.Fatalf("versions=%d want %d", n, wantVersions)
			}
		})
	}
}

func TestNamespacePolicyOverridesFeedPolicy(t *testing.T) {
	f := newPublishFixture(t)
	p := f.deleter(t)
	f.publish(t, "1.0", "artifact.bin", []byte("ns override"))
	f.setPolicy(t, "", DeleteDenied, false)
	f.setPolicy(t, "team", DeleteAllowed, false)
	if _, err := f.service.Delete(context.Background(), p, f.target("1.0", "")); err != nil {
		t.Fatalf("namespace policy should override: %v", err)
	}
}

func TestDeleteAuthorizationDenied(t *testing.T) {
	f := newPublishFixture(t)
	f.publish(t, "1.0", "artifact.bin", []byte("authz"))
	// The fixture token carries read+publish only.
	if _, err := f.service.Delete(context.Background(), f.principal, f.target("1.0", "")); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v want ErrDenied", err)
	}
	// Unlist stands in for removal in some adapters, so it needs delete too...
	if _, err := f.service.Unlist(context.Background(), f.principal, f.target("1.0", "")); !errors.Is(err, ErrDenied) {
		t.Fatalf("unlist got %v want ErrDenied", err)
	}
	// ...except where the protocol defines a publisher-level yank (cargo).
	yank := f.target("1.0", "")
	yank.PublisherMayUnlist = true
	if _, err := f.service.Unlist(context.Background(), f.principal, yank); err != nil {
		t.Fatalf("publisher yank: %v", err)
	}
	if _, err := f.service.Relist(context.Background(), f.principal, yank); err != nil {
		t.Fatalf("publisher unyank: %v", err)
	}
	if _, err := f.service.Delete(context.Background(), f.principal, yank); !errors.Is(err, ErrDenied) {
		t.Fatalf("delete with yank flag got %v want ErrDenied", err)
	}
	if f.count(t, "SELECT count(*) FROM assets") != 1 || f.count(t, "SELECT count(*) FROM audit_events WHERE action LIKE 'artifact.delete'") != 0 {
		t.Fatal("denied delete changed state")
	}
}

func TestDeleteEmitsAuditOutboxAndTombstoneAndGCReclaims(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	p := f.deleter(t)
	data := []byte("reclaim me later")
	f.publish(t, "1.0", "artifact.bin", data)
	f.publish(t, "1.0", "artifact.pom", []byte("pom"))
	if _, err := f.pool.Exec(ctx, "INSERT INTO tags(component_id,name,version_id) SELECT component_id,'latest',id FROM versions"); err != nil {
		t.Fatal(err)
	}
	var changed []Change
	f.service.Changed = func(c Change) { changed = append(changed, c) }

	// Asset scope first: the version survives while another asset remains.
	out, err := f.service.Delete(ctx, p, f.target("1.0", "artifact.pom"))
	if err != nil || out.AssetsRemoved != 1 || out.VersionRemoved {
		t.Fatalf("asset delete: %+v %v", out, err)
	}
	if _, err = f.service.Delete(ctx, p, f.target("1.0", "missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing asset: %v", err)
	}
	out, err = f.service.Delete(ctx, p, f.target("1.0", ""))
	if err != nil || !out.VersionRemoved || !out.ComponentRemoved || out.TagsRemoved != 1 || out.AssetsRemoved != 1 {
		t.Fatalf("version delete: %+v %v", out, err)
	}
	if len(changed) != 2 || changed[1].Action != ActionDelete {
		t.Fatalf("invalidation hook: %+v", changed)
	}
	if n := f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.delete' AND details->>'coordinate'='team/package'"); n != 2 {
		t.Fatalf("audit events=%d", n)
	}
	if n := f.count(t, "SELECT count(*) FROM outbox_events WHERE topic='artifact.delete' AND payload->>'version'='1.0'"); n != 2 {
		t.Fatalf("outbox events=%d", n)
	}
	if n := f.count(t, "SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='asset' AND coordinate='team/package' AND seq IS NOT NULL"); n != 2 {
		t.Fatalf("asset tombstones=%d", n)
	}
	if f.count(t, "SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='tag'") != 1 || f.count(t, "SELECT count(*) FROM replication_changes WHERE op='delete' AND kind='component'") != 1 {
		t.Fatal("tag/component tombstones missing")
	}

	// Bytes survive the delete and are reclaimed only by garbage collection.
	digest := f.publication(data).Info.Digest
	if _, err = f.disk.Stat(ctx, digest); err != nil {
		t.Fatalf("bytes removed inline: %v", err)
	}
	gc := &retention.Service{DB: f.pool, InstanceID: f.principal.InstanceID, Lease: f.service.Lease, Options: retention.Options{Grace: time.Hour}, Stores: func(context.Context, string) (blob.Store, error) { return f.disk, nil }}
	age := func(table, col string) {
		if _, e := f.pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET %s=%s-interval '2 hours'", table, col, col)); e != nil {
			t.Fatal(e)
		}
	}
	age("blobs", "created_at")
	if _, err = gc.CollectGarbage(ctx, false); err != nil {
		t.Fatal(err)
	}
	age("gc_candidates", "first_seen_at")
	rep, err := gc.CollectGarbage(ctx, false)
	if err != nil || len(rep.Swept) != 2 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if _, err = f.disk.Stat(ctx, digest); err == nil {
		t.Fatal("gc did not reclaim bytes")
	}
}

func TestUnlistRelistIsIdempotentAndAudited(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	p := f.deleter(t)
	f.publish(t, "1.0", "artifact.bin", []byte("unlist"))
	out, err := f.service.Unlist(ctx, p, f.target("1.0", ""))
	if err != nil || out.Unchanged {
		t.Fatalf("%+v %v", out, err)
	}
	var meta string
	if err = f.pool.QueryRow(ctx, "SELECT metadata::text FROM components").Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta != `{"unlisted": ["1.0"]}` {
		t.Fatalf("metadata=%s", meta)
	}
	if out, err = f.service.Unlist(ctx, p, f.target("1.0", "")); err != nil || !out.Unchanged {
		t.Fatalf("repeat unlist: %+v %v", out, err)
	}
	if f.count(t, "SELECT count(*) FROM audit_events WHERE action='artifact.unlist'") != 1 || f.count(t, "SELECT count(*) FROM outbox_events WHERE topic='artifact.unlist'") != 1 {
		t.Fatal("unlist audit/outbox")
	}
	// The unlist replicates as a component metadata change.
	if f.count(t, "SELECT count(*) FROM replication_changes WHERE kind='component' AND op='put' AND seq IS NOT NULL") < 1 {
		t.Fatal("unlist not in change feed")
	}
	if out, err = f.service.Relist(ctx, p, f.target("1.0", "")); err != nil || out.Unchanged {
		t.Fatalf("relist: %+v %v", out, err)
	}
	if f.count(t, "SELECT count(*) FROM versions") != 1 || f.count(t, "SELECT count(*) FROM assets") != 1 {
		t.Fatal("unlist removed data")
	}
	if _, err = f.service.Unlist(ctx, p, f.target("9.9", "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown version: %v", err)
	}
}

func TestDeleteRejectsForeignNamespaceAndRemoteSources(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	p := f.deleter(t)
	f.publish(t, "1.0", "artifact.bin", []byte("ns"))
	bad := f.target("1.0", "")
	bad.Namespace = "other"
	if _, err := f.service.Delete(ctx, p, bad); !errors.Is(err, ErrNotFound) {
		t.Fatalf("namespace mismatch: %v", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE sources SET kind='remote'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Delete(ctx, p, f.target("1.0", "")); !errors.Is(err, ErrConflict) {
		t.Fatalf("remote-owned: %v", err)
	}
}
