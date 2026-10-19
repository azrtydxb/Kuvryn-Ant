package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
)

// ociCache turns the fixture feed into an OCI cache feed (the artifact core is
// format-neutral: it only needs the feed's purpose and format).
func (f publishFixture) ociCache(t *testing.T, purpose string) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		"UPDATE feeds SET format='oci'",
		"UPDATE sources SET format='oci'",
	} {
		if _, err := f.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(ctx, "UPDATE feeds SET purpose=$1", purpose); err != nil {
		t.Fatal(err)
	}
}

func digestOf(data string) blob.Info {
	s := sha256.Sum256([]byte(data))
	return blob.Info{Digest: blob.Digest(hex.EncodeToString(s[:])), Size: int64(len(data))}
}

// pushBlob publishes a layer or config as the OCI adapter does: a version named
// by its digest with a "blob" asset.
func (f publishFixture) pushBlob(t *testing.T, repo, content string) blob.Info {
	t.Helper()
	info := digestOf(content)
	_, err := f.service.Publish(context.Background(), f.principal, Publication{FeedID: f.feed, Coordinate: repo, Version: string(info.Digest), AssetKey: "blob", MediaType: "application/octet-stream", Info: info}, bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// pushManifest publishes a manifest naming the given blobs, optionally tagged.
func (f publishFixture) pushManifest(t *testing.T, repo, tag, body string, refs ...blob.Info) (blob.Info, error) {
	t.Helper()
	info := digestOf(body)
	var references []Reference
	for _, r := range refs {
		references = append(references, Reference{Version: string(r.Digest), AssetKey: "blob", Info: r})
	}
	_, err := f.service.Publish(context.Background(), f.principal, Publication{FeedID: f.feed, Coordinate: repo, Version: string(info.Digest), AssetKey: "manifest", MediaType: "application/vnd.oci.image.manifest.v1+json", Tag: tag, References: references, Info: info}, bytes.NewReader([]byte(body)))
	return info, err
}

func (f publishFixture) tagVersion(t *testing.T, repo, tag string) string {
	t.Helper()
	var v string
	err := f.pool.QueryRow(context.Background(), `SELECT v.version FROM tags t JOIN versions v ON v.id=t.version_id JOIN components c ON c.id=t.component_id WHERE c.coordinate=$1 AND t.name=$2`, repo, tag).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

func (f publishFixture) hasVersion(t *testing.T, repo string, info blob.Info) bool {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate=$1 AND v.version=$2`, repo, string(info.Digest)) == 1
}

func (f publishFixture) gcService(grace time.Duration) *retention.Service {
	return &retention.Service{DB: f.pool, InstanceID: f.principal.InstanceID, Lease: f.service.Lease, Options: retention.Options{Grace: 24 * time.Hour, PressureSweepGrace: grace},
		Stores: func(context.Context, string) (blob.Store, error) { return f.disk, nil }}
}

// Overwriting a tag in a cache feed releases the superseded manifest and the
// layers nothing else names, with the short sweep wait; shared layers stay.
// The same overwrite in an ordinary feed is still a conflict.
func TestCacheTagOverwriteReleasesSupersededLayers(t *testing.T) {
	f := newPublishFixture(t)
	f.ociCache(t, "cache")
	f.service.CacheSweepGrace = time.Millisecond
	ctx := context.Background()
	const repo = "app"
	shared, only1 := f.pushBlob(t, repo, "layer shared"), f.pushBlob(t, repo, "layer only in the first build")
	cfg1 := f.pushBlob(t, repo, "config one")
	m1, err := f.pushManifest(t, repo, "main-amd64", "manifest one", cfg1, shared, only1)
	if err != nil {
		t.Fatal(err)
	}
	only2, cfg2 := f.pushBlob(t, repo, "layer only in the second build"), f.pushBlob(t, repo, "config two")
	// A different tag that points at nothing else: unrelated content must survive.
	other := f.pushBlob(t, repo, "layer of another branch")
	mOther, err := f.pushManifest(t, repo, "feature-amd64", "manifest of another branch", f.pushBlob(t, repo, "config other"), other)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := f.pushManifest(t, repo, "main-amd64", "manifest two", cfg2, shared, only2)
	if err != nil {
		t.Fatalf("overwriting a tag in a cache feed failed: %v", err)
	}
	if got := f.tagVersion(t, repo, "main-amd64"); got != string(m2.Digest) {
		t.Fatalf("tag points at %s, want the new manifest", got)
	}
	for name, gone := range map[string]blob.Info{"first manifest": m1, "its config": cfg1, "its only layer": only1} {
		if f.hasVersion(t, repo, gone) {
			t.Fatalf("%s was not released", name)
		}
	}
	for name, kept := range map[string]blob.Info{"new manifest": m2, "shared layer": shared, "new layer": only2, "new config": cfg2, "other branch manifest": mOther, "other branch layer": other} {
		if !f.hasVersion(t, repo, kept) {
			t.Fatalf("%s was released", name)
		}
	}
	// Freed blobs are queued for the short sweep; no byte is deleted inline.
	var queued int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM gc_candidates WHERE sweep_after IS NOT NULL AND sweep_after < first_seen_at + interval '1 minute'`).Scan(&queued); err != nil || queued != 3 {
		t.Fatalf("queued for the short sweep: %d (%v), want 3 (manifest, config, layer)", queued, err)
	}
	if _, err = f.disk.Stat(ctx, only1.Digest); err != nil {
		t.Fatalf("bytes removed inline: %v", err)
	}
	// The ordinary sweep (row lock, reference re-check) then reclaims them, and only them.
	time.Sleep(20 * time.Millisecond)
	if _, err = f.gcService(time.Millisecond).CacheTick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.disk.Stat(ctx, only1.Digest); err == nil {
		t.Fatal("superseded layer bytes were not swept")
	}
	for _, kept := range []blob.Info{shared, only2, cfg2, other} {
		if _, err = f.disk.Stat(ctx, kept.Digest); err != nil {
			t.Fatalf("referenced bytes were swept: %v", err)
		}
	}

	// Ordinary feeds keep immutable tags.
	g := newPublishFixture(t)
	g.ociCache(t, "artifacts")
	lay := g.pushBlob(t, repo, "layer")
	if _, err = g.pushManifest(t, repo, "v1", "manifest a", lay); err != nil {
		t.Fatal(err)
	}
	if _, err = g.pushManifest(t, repo, "v1", "manifest b", lay); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwriting a tag in an artifacts feed: %v, want a conflict", err)
	}
}

// A manifest named by an index is not released when a tag moves.
func TestCacheTagOverwriteKeepsManifestsAnIndexNames(t *testing.T) {
	f := newPublishFixture(t)
	f.ociCache(t, "cache")
	const repo = "app"
	child := f.pushBlob(t, repo, "child layer")
	childManifest, err := f.pushManifest(t, repo, "arm64", "child manifest", f.pushBlob(t, repo, "child config"), child)
	if err != nil {
		t.Fatal(err)
	}
	// An index tagged "multi" names the child manifest; the child's own tag then moves.
	index := digestOf("index document")
	if _, err = f.service.Publish(context.Background(), f.principal, Publication{FeedID: f.feed, Coordinate: repo, Version: string(index.Digest), AssetKey: "manifest", MediaType: "application/vnd.oci.image.index.v1+json", Tag: "multi",
		References: []Reference{{Version: string(childManifest.Digest), AssetKey: "manifest", Info: childManifest}}, Info: index}, bytes.NewReader([]byte("index document"))); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pushManifest(t, repo, "arm64", "replacement manifest", f.pushBlob(t, repo, "new config"), f.pushBlob(t, repo, "new layer")); err != nil {
		t.Fatal(err)
	}
	if !f.hasVersion(t, repo, childManifest) || !f.hasVersion(t, repo, child) {
		t.Fatal("a manifest named by an index was released")
	}
}

func (f publishFixture) principalWith(t *testing.T, actions ...identity.Action) identity.Principal {
	t.Helper()
	ctx := context.Background()
	tok, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{FeedID: f.feed, Actions: actions}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err := (identity.Tokens{DB: f.pool}).Authenticate(ctx, tok.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCleanCacheOCIByTagPrefix(t *testing.T) {
	f := newPublishFixture(t)
	f.ociCache(t, "cache")
	f.service.CacheSweepGrace = time.Millisecond
	ctx := context.Background()
	for _, repo := range []string{"app", "other"} {
		for _, tag := range []string{"main-amd64", "main-arm64", "feat-x-amd64"} {
			if _, err := f.pushManifest(t, repo, tag, repo+tag+" manifest", f.pushBlob(t, repo, repo+tag+" layer")); err != nil {
				t.Fatal(err)
			}
		}
	}
	ci := f.principalWith(t, identity.Read, identity.Delete)
	res, err := f.service.CleanCache(ctx, ci, CacheClean{FeedID: f.feed, Prefix: "app:main-"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tags != 2 || res.Versions != 4 || res.Matched != 1 || res.Bytes == 0 || res.Truncated {
		t.Fatalf("cleanup result %+v, want 2 tags and 4 versions (2 manifests + 2 layers)", res)
	}
	if f.tagVersion(t, "app", "feat-x-amd64") == "" || f.tagVersion(t, "other", "main-amd64") == "" || f.tagVersion(t, "app", "main-amd64") != "" {
		t.Fatal("cleanup removed the wrong tags")
	}
	// Idempotent.
	if again, err := f.service.CleanCache(ctx, ci, CacheClean{FeedID: f.feed, Prefix: "app:main-"}); err != nil || again.Tags != 0 || again.Versions != 0 || again.Matched != 0 {
		t.Fatalf("repeating the cleanup: %+v %v", again, err)
	}
	// Without a tag prefix every tag of the repository goes, nothing of other repositories.
	res, err = f.service.CleanCache(ctx, ci, CacheClean{FeedID: f.feed, Prefix: "app"})
	if err != nil || res.Tags != 1 || res.Components != 1 {
		t.Fatalf("whole repository cleanup %+v %v", res, err)
	}
	if f.count(t, `SELECT count(*) FROM components WHERE coordinate='app'`) != 0 || f.count(t, `SELECT count(*) FROM tags`) != 3 {
		t.Fatal("repository not fully cleaned or others touched")
	}
	// Audited: one summary per call plus one per component touched.
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action='cache.clean'`); n != 3 {
		t.Fatalf("%d cache.clean audit events, want 3", n)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action='cache.clean.component'`); n != 2 {
		t.Fatalf("%d component audit events, want 2", n)
	}
}

func TestCleanCacheRawByPathPrefix(t *testing.T) {
	f := newPublishFixture(t) // raw feed
	if _, err := f.pool.Exec(context.Background(), "UPDATE feeds SET purpose='cache'"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ci/main/a/1", "ci/main/b/2", "ci/feat/c/3", "other/x"} {
		data := []byte("cache entry " + key)
		pub := f.publication(data)
		pub.Coordinate, pub.Namespace, pub.Version, pub.AssetKey = key, "", "first", key
		if _, err := f.service.Publish(context.Background(), f.principal, pub, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	ci := f.principalWith(t, identity.Read, identity.Delete)
	res, err := f.service.CleanCache(context.Background(), ci, CacheClean{FeedID: f.feed, Prefix: "ci/main/"})
	if err != nil || res.Matched != 2 || res.Versions != 2 || res.Components != 2 {
		t.Fatalf("raw cleanup %+v %v", res, err)
	}
	if f.count(t, "SELECT count(*) FROM components") != 2 {
		t.Fatal("raw cleanup removed the wrong files")
	}
}

func TestCleanCacheAuthorization(t *testing.T) {
	f := newPublishFixture(t)
	ctx := context.Background()
	req := CacheClean{FeedID: f.feed, Prefix: "app"}
	if _, err := f.service.CleanCache(ctx, f.principalWith(t, identity.Read, identity.Delete), req); !errors.Is(err, ErrNotCache) {
		t.Fatalf("artifacts feed: %v, want ErrNotCache", err)
	}
	if _, err := f.pool.Exec(ctx, "UPDATE feeds SET purpose='cache'"); err != nil {
		t.Fatal(err)
	}
	// Publish (a CI job that may write the cache) is not enough: delete is required.
	if _, err := f.service.CleanCache(ctx, f.principalWith(t, identity.Read, identity.Publish), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("publisher without delete: %v, want ErrDenied", err)
	}
	// A principal that cannot read the feed learns nothing about it.
	if _, err := f.service.CleanCache(ctx, f.principalWith(t, identity.Delete), req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete without read: %v, want ErrNotFound", err)
	}
	// A token scoped to another feed is denied (scope narrowing).
	var other string
	if err := f.pool.QueryRow(ctx, "INSERT INTO feeds(instance_id,slug,format) VALUES($1,'other','raw') RETURNING id::text", f.principal.InstanceID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	tok, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{FeedID: other, Actions: []identity.Action{identity.Read, identity.Delete}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := (identity.Tokens{DB: f.pool}).Authenticate(ctx, tok.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.CleanCache(ctx, scoped, req); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token scoped to another feed: %v, want ErrNotFound", err)
	}
	// An explicit deny policy on the feed is honored.
	f.setPolicy(t, "", DeleteDenied, false)
	if _, err = f.service.CleanCache(ctx, f.principalWith(t, identity.Read, identity.Delete), req); !errors.Is(err, ErrPolicy) {
		t.Fatalf("delete policy denied: %v, want ErrPolicy", err)
	}
	if _, err = f.service.CleanCache(ctx, f.principalWith(t, identity.Read, identity.Delete), CacheClean{FeedID: f.feed}); !errors.Is(err, ErrInput) {
		t.Fatalf("empty prefix: %v, want ErrInput", err)
	}
}
