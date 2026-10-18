package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func data(s string) []byte { return []byte(s + strings.Repeat("-payload", 8)) }

// link configures a peer and a job on dst that replicates src's feed into destFeed.
func link(t *testing.T, src, dst *site, srv *peerServer, svc *Service, srcFeed, destFeed string, in JobInput) Job {
	t.Helper()
	ctx := context.Background()
	token := src.grantReplicate(fmt.Sprintf("peer-%d", time.Now().UnixNano()), srcFeed)
	peer, err := svc.CreatePeer(ctx, dst.instance, dst.admin.PrincipalID, PeerInput{Name: fmt.Sprintf("site-%d", time.Now().UnixNano()), URL: srv.URL, CARef: "peer-ca", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	in.PeerID, in.RemoteFeedID, in.DestFeedID = peer.ID, srcFeed, destFeed
	if in.Mode == "" {
		in.Mode = "manual"
	}
	if in.Name == "" {
		in.Name = fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	job, err := svc.CreateJob(ctx, dst.instance, dst.admin.PrincipalID, in)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (s *site) read(feed, ns, coord, version, key string) ([]byte, error) {
	_, r, err := s.publisher.Open(context.Background(), s.admin, artifact.Publication{FeedID: feed, Namespace: ns, Coordinate: coord, Version: version, AssetKey: key}, 0, -1)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
func (s *site) head() int {
	return s.count("SELECT COALESCE(max(seq),0) FROM replication_heads")
}
func status(t *testing.T, svc *Service, dst *site, id string) Status {
	t.Helper()
	j, err := svc.GetJob(context.Background(), dst.instance, id)
	if err != nil {
		t.Fatal(err)
	}
	return j.Status
}
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
func (s *site) replicaAssets(job Job) int {
	return s.count(`SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=$1`, job.ReplicaSourceID)
}

func TestSeedIncrementalLagAndSourceOutage(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "team", "team/app", "1.0", "app-1.0.bin", "latest", data("app-1.0"))
	src.publish(src.publisher, src.feed, "team", "team/app", "1.1", "app-1.1.bin", "stable", data("app-1.1"))
	src.publish(src.publisher, src.feed, "team", "team/lib", "2.0.0", "lib.bin", "", data("lib"))
	src.publish(src.publisher, src.feed, "other", "other/tool", "0.1", "tool.bin", "", data("tool"))
	cache, cacheDomain := src.newSource("cache", "remote")
	src.attach(src.feed, cache, 1)
	src.insertAsset(cache, cacheDomain, "", "cached/pkg", "9.9", "pkg.bin", data("cached"), "{}")

	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})

	// The replica source is attached read-only: no publish path can write to it.
	var kind string
	dst.count("SELECT 1")
	if err := dst.pool.QueryRow(ctx, "SELECT kind FROM sources WHERE id=$1", job.ReplicaSourceID).Scan(&kind); err != nil || kind != "replica" {
		t.Fatalf("replica source kind=%q err=%v", kind, err)
	}
	if _, err := dst.publisherFor(dst.feed, job.ReplicaSourceID).Publish(ctx, dst.admin, artifact.Publication{FeedID: dst.feed, Coordinate: "x", Version: "1", AssetKey: "x", MediaType: "text/plain", Info: digestOf([]byte("x"))}, strings.NewReader("x")); err == nil {
		t.Fatal("replica source accepted a publication")
	}

	// Full seed, including cached pull-through content by default.
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	st := status(t, svc, dst, job.ID)
	if st.Phase != "incremental" || st.Checkpoint != int64(src.head()) || st.LastSuccessAt == nil || st.LastError != "" {
		t.Fatalf("unexpected status after seed: %+v", st)
	}
	if got := dst.replicaAssets(job); got != 5 {
		t.Fatalf("replicated %d assets, want 5", got)
	}
	if st.BlobsTransferred != 5 || st.AssetsApplied != 5 {
		t.Fatalf("counters: %+v", st)
	}
	for _, c := range []struct{ ns, coord, ver, key, body string }{
		{"team", "team/app", "1.0", "app-1.0.bin", "app-1.0"}, {"team", "team/app", "1.1", "app-1.1.bin", "app-1.1"},
		{"team", "team/lib", "2.0.0", "lib.bin", "lib"}, {"other", "other/tool", "0.1", "tool.bin", "tool"}, {"", "cached/pkg", "9.9", "pkg.bin", "cached"},
	} {
		got, err := dst.read(dst.feed, c.ns, c.coord, c.ver, c.key)
		if err != nil || !bytes.Equal(got, data(c.body)) {
			t.Fatalf("destination cannot serve %s: %v", c.coord, err)
		}
	}
	if n := dst.count(`SELECT count(*) FROM assets WHERE provenance->>'cached'='true' AND provenance->>'replication_job'=$1`, job.ID); n != 1 {
		t.Fatalf("cached provenance count=%d", n)
	}
	if n := dst.count(`SELECT count(*) FROM tags t JOIN components c ON c.id=t.component_id WHERE c.source_id=$1`, job.ReplicaSourceID); n != 2 {
		t.Fatalf("replicated tags=%d", n)
	}

	// Incremental: new publication, a tag move and a metadata/index update.
	src.publish(src.publisher, src.feed, "team", "team/app", "1.2", "app-1.2.bin", "next", data("app-1.2"))
	src.exec(`UPDATE tags SET version_id=(SELECT v.id FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='team/app' AND v.version='1.1') WHERE name='latest'`)
	src.exec(`UPDATE components SET metadata='{"description":"library"}' WHERE coordinate='team/lib'`)
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := dst.read(dst.feed, "team", "team/app", "1.2", "app-1.2.bin"); err != nil || !bytes.Equal(got, data("app-1.2")) {
		t.Fatalf("incremental asset missing: %v", err)
	}
	var tagged, meta string
	if err := dst.pool.QueryRow(ctx, `SELECT v.version FROM tags t JOIN versions v ON v.id=t.version_id JOIN components c ON c.id=t.component_id WHERE c.source_id=$1 AND t.name='latest'`, job.ReplicaSourceID).Scan(&tagged); err != nil || tagged != "1.1" {
		t.Fatalf("tag move not replicated: %q %v", tagged, err)
	}
	if err := dst.pool.QueryRow(ctx, `SELECT metadata->>'description' FROM components WHERE source_id=$1 AND coordinate='team/lib'`, job.ReplicaSourceID).Scan(&meta); err != nil || meta != "library" {
		t.Fatalf("metadata update not replicated: %q %v", meta, err)
	}
	st = status(t, svc, dst, job.ID)
	if st.ChangesBehind == nil || *st.ChangesBehind != 0 || st.LagSeconds == nil || *st.LagSeconds != 0 || st.Checkpoint != int64(src.head()) {
		t.Fatalf("caught-up lag report wrong: %+v", st)
	}

	// Lag and failure reporting: the source moves on while transfers fail.
	for i := range 3 {
		time.Sleep(20 * time.Millisecond)
		src.publish(src.publisher, src.feed, "team", "team/lib", fmt.Sprintf("2.1.%d", i), "lib.bin", "", data(fmt.Sprintf("lib-%d", i)))
	}
	srv.mu.Lock()
	srv.failAt = srv.total
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected transfer failure")
	}
	st = status(t, svc, dst, job.ID)
	if st.ChangesBehind == nil || *st.ChangesBehind < 3 || st.LagSeconds == nil || *st.LagSeconds <= 0 || st.LastError == "" || st.ConsecutiveFailures != 1 || st.NextRunAt == nil || st.LastSuccessAt == nil {
		t.Fatalf("lag/failure report wrong: %+v", st)
	}
	srv.mu.Lock()
	srv.failAt = -1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	st = status(t, svc, dst, job.ID)
	if *st.ChangesBehind != 0 || st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Fatalf("recovery not reported: %+v", st)
	}

	// Source outage: a fully seeded destination serves independently.
	srv.Close()
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected peer failure")
	}
	// An unreachable peer cannot refresh the head, so lag must grow from the
	// last success instead of staying at the caught-up 0.
	dst.exec(`UPDATE replication_jobs SET last_success_at=clock_timestamp()-interval '5 minutes' WHERE id=$1`, job.ID)
	st = status(t, svc, dst, job.ID)
	if st.ConsecutiveFailures == 0 || st.LagSeconds == nil || *st.LagSeconds < 299 || *st.LagSeconds > 330 {
		t.Fatalf("lag did not grow while the peer is unreachable: %+v", st)
	}
	if got, err := dst.read(dst.feed, "team", "team/app", "1.0", "app-1.0.bin"); err != nil || !bytes.Equal(got, data("app-1.0")) {
		t.Fatalf("destination cannot serve during source outage: %v", err)
	}
	if got, err := dst.read(dst.feed, "", "cached/pkg", "9.9", "pkg.bin"); err != nil || !bytes.Equal(got, data("cached")) {
		t.Fatalf("cached entry unavailable during outage: %v", err)
	}
}

func TestInterruptedSeedResumesWithoutRetransfer(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	for i := range 6 {
		src.publish(src.publisher, src.feed, "n", fmt.Sprintf("n/pkg%d", i), "1.0", "f.bin", "", data(fmt.Sprintf("blob-%d", i)))
	}
	// Same bytes under a second key: one digest, two assets.
	src.publish(src.publisher, src.feed, "n", "n/pkg0", "1.0", "alias.bin", "", data("blob-0"))

	srv := src.serve(t)
	// Sequential transfers with a failure after the 4th blob: pages 1-2 commit
	// (3 blobs), page 3 stores one verified blob and then fails before commit.
	srv.failAt = 4
	svc := dst.service(t, srv, func(c *Config) { c.BlobConcurrency = 1 })
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	if err := svc.RunNow(ctx, job.ID); err == nil {
		t.Fatal("expected interrupted seed")
	}
	st := status(t, svc, dst, job.ID)
	if st.Phase != "seed_assets" || st.SeedDone == 0 || st.LastError == "" {
		t.Fatalf("seed checkpoint not durable: %+v", st)
	}
	partial := dst.replicaAssets(job)
	if partial == 0 || partial >= 7 {
		t.Fatalf("partial seed applied %d assets", partial)
	}

	srv.mu.Lock()
	srv.failAt = -1
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got := dst.replicaAssets(job); got != 7 {
		t.Fatalf("resumed seed has %d assets, want 7", got)
	}
	if srv.maxPerDigest() != 1 || srv.successes() != 6 {
		t.Fatalf("blobs were re-transferred: served=%v", srv.served)
	}
	st = status(t, svc, dst, job.ID)
	if st.Phase != "incremental" || st.BlobsTransferred != 5 || st.BlobsDeduplicated != 1 {
		t.Fatalf("resume counters wrong: %+v", st)
	}

	// Replaying from checkpoint zero is idempotent and fetches nothing.
	dst.exec("UPDATE replication_jobs SET checkpoint=0 WHERE id=$1", job.ID)
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if srv.successes() != 6 || dst.replicaAssets(job) != 7 || dst.count("SELECT count(*) FROM blobs") != 6 {
		t.Fatal("replay duplicated work or state")
	}
}

func TestCorruptBlobRejected(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	good := data("good")
	victim := data("victim")
	src.publish(src.publisher, src.feed, "n", "n/a", "1", "good.bin", "", good)
	src.publish(src.publisher, src.feed, "n", "n/b", "1", "victim.bin", "", victim)
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{})
	vd := digestOf(victim)
	for name, bad := range map[string][]byte{
		"flipped":   append(bytes.Clone(victim[:len(victim)-1]), victim[len(victim)-1]^0xff),
		"truncated": victim[:len(victim)-3],
	} {
		srv.mu.Lock()
		srv.tamper = map[string][]byte{string(vd.Digest): bad}
		srv.mu.Unlock()
		err := svc.RunNow(ctx, job.ID)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("%s blob not rejected: %v", name, err)
		}
		store, _ := dst.stores.Resolve(ctx, mustDomain(t, dst, job))
		if _, err := store.Stat(ctx, vd.Digest); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("%s: corrupt blob was stored (%v)", name, err)
		}
		if dst.count("SELECT count(*) FROM blobs WHERE digest=$1", string(vd.Digest)) != 0 || dst.count(`SELECT count(*) FROM components c WHERE c.source_id=$1 AND c.coordinate='n/b'`, job.ReplicaSourceID) != 0 {
			t.Fatalf("%s: metadata for a corrupt blob became visible", name)
		}
	}
	st := status(t, svc, dst, job.ID)
	if st.ConsecutiveFailures != 2 || !strings.Contains(st.LastError, "digest") && !strings.Contains(st.LastError, "bytes") {
		t.Fatalf("corruption not reported: %+v", st)
	}
	srv.mu.Lock()
	srv.tamper = map[string][]byte{}
	srv.mu.Unlock()
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := dst.read(dst.feed, "n", "n/b", "1", "victim.bin"); err != nil || !bytes.Equal(got, victim) {
		t.Fatalf("recovery after corruption failed: %v", err)
	}
}
func mustDomain(t *testing.T, s *site, job Job) string {
	t.Helper()
	var d string
	if err := s.pool.QueryRow(context.Background(), "SELECT id::text FROM dedup_domains WHERE name='source:'||$1::text", job.ReplicaSourceID).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeletionPolicyDryRunAndDestinationOnlyProtection(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "team", "team/a", "1", "a.bin", "", data("src-a"))
	src.publish(src.publisher, src.feed, "team", "team/b", "1", "b.bin", "tagb", data("src-b"))
	// Destination-only content in the same feed, with a colliding identity.
	localBytes := data("destination-local-a")
	dst.publish(dst.publisher, dst.feed, "team", "team/a", "1", "a.bin", "", localBytes)

	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	job := link(t, src, dst, srv, svc, src.feed, dst.feed, JobInput{}) // policy defaults to ignore
	if job.DeletionPolicy != "ignore" {
		t.Fatalf("default deletion policy %q", job.DeletionPolicy)
	}
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}

	// Source deletes: ignored by policy, but remembered.
	src.exec("DELETE FROM assets WHERE asset_key='a.bin'")
	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	st := status(t, svc, dst, job.ID)
	if st.DeletionsApplied != 0 || st.DeletionsHeld != 1 || dst.replicaAssets(job) != 2 {
		t.Fatalf("ignore policy deleted or lost a tombstone: %+v", st)
	}

	preview, err := svc.PreviewDeletions(ctx, dst.instance, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applies || len(preview.Entries) != 1 || preview.Entries[0].Source != "held" || preview.Entries[0].Disposition != "would_delete" {
		t.Fatalf("held preview wrong: %+v", preview)
	}

	// Destination-only content inside the replica source, plus a put/delete race
	// at the source for the same identity.
	protectedBytes := data("destination-only")
	dst.insertAsset(job.ReplicaSourceID, mustDomain(t, dst, job), "team", "protected/pkg", "1", "x.bin", protectedBytes, "{}")
	src.publish(src.publisher, src.feed, "team", "protected/pkg", "1", "x.bin", "", data("src-x"))
	src.exec("DELETE FROM assets WHERE asset_key='x.bin'")
	src.exec("DELETE FROM assets WHERE asset_key='b.bin'")
	src.exec("DELETE FROM tags WHERE name='tagb'")

	updated, err := svc.UpdateJob(ctx, dst.instance, dst.admin.PrincipalID, job.ID, JobPatch{Revision: job.Revision, DeletionPolicy: ptr("propagate")})
	if err != nil {
		t.Fatal(err)
	}
	before := dst.count("SELECT count(*) FROM assets")
	for range 2 { // dry run is repeatable and changes nothing
		preview, err = svc.PreviewDeletions(ctx, dst.instance, job.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !preview.Applies || preview.Counts["protected"] != 1 || preview.Counts["would_delete"] < 3 {
		t.Fatalf("dry run counts wrong: %+v", preview)
	}
	for _, e := range preview.Entries {
		if e.Coordinate == "protected/pkg" && e.Disposition != "protected" {
			t.Fatalf("destination-only content not classified protected: %+v", e)
		}
	}
	if dst.count("SELECT count(*) FROM assets") != before || status(t, svc, dst, job.ID).DeletionsApplied != 0 || status(t, svc, dst, job.ID).DeletionsHeld != 1 {
		t.Fatal("dry run changed state")
	}

	if err := svc.RunNow(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	st = status(t, svc, dst, job.ID)
	if st.DeletionsApplied < 3 || st.DeletionsHeld != 0 || st.Conflicts < 1 {
		t.Fatalf("propagation counters wrong: %+v", st)
	}
	for _, coord := range []string{"team/a", "team/b"} {
		if n := dst.count(`SELECT count(*) FROM components WHERE source_id=$1 AND coordinate=$2`, job.ReplicaSourceID, coord); n != 0 {
			t.Fatalf("%s not propagated (components=%d)", coord, n)
		}
	}
	if got, err := dst.read(dst.feed, "team", "protected/pkg", "1", "x.bin"); err != nil || !bytes.Equal(got, protectedBytes) {
		t.Fatalf("destination-only replica content was deleted or overwritten: %v", err)
	}
	if got, err := dst.read(dst.feed, "team", "team/a", "1", "a.bin"); err != nil || !bytes.Equal(got, localBytes) {
		t.Fatalf("destination-local content was touched: %v", err)
	}
	if dst.count(`SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=$1`, dst.source) != 1 {
		t.Fatal("local source content changed")
	}
	_ = updated
}

func ptr[T any](v T) *T { return &v }

func TestSelectiveFiltersAndCachedExclusion(t *testing.T) {
	ctx := context.Background()
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "team", "team/a", "1.0", "a.bin", "", data("a10"))
	src.publish(src.publisher, src.feed, "team", "team/a", "2.0", "a.bin", "", data("a20"))
	src.publish(src.publisher, src.feed, "other", "other/b", "1.0", "b.bin", "", data("b10"))
	cache, cd := src.newSource("cache", "remote")
	src.attach(src.feed, cache, 1)
	src.insertAsset(cache, cd, "", "cached/c", "1.0", "c.bin", data("c10"), "{}")
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)

	selFeed, _, _ := dst.newFeed("selective", "local")
	sel := link(t, src, dst, srv, svc, src.feed, selFeed, JobInput{Filters: &Filters{Namespaces: []string{"team"}, VersionPrefixes: []string{"1."}}})
	noCacheFeed, _, _ := dst.newFeed("nocache", "local")
	noCache := link(t, src, dst, srv, svc, src.feed, noCacheFeed, JobInput{IncludeCached: ptr(false)})
	pkgFeed, _, _ := dst.newFeed("pkgonly", "local")
	pkg := link(t, src, dst, srv, svc, src.feed, pkgFeed, JobInput{Filters: &Filters{Packages: []string{"other/b"}}})
	for _, j := range []Job{sel, noCache, pkg} {
		if err := svc.RunNow(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := dst.replicaAssets(sel); n != 1 {
		t.Fatalf("selective job replicated %d assets, want 1", n)
	}
	if n := dst.replicaAssets(noCache); n != 3 {
		t.Fatalf("cache-excluding job replicated %d assets, want 3", n)
	}
	if n := dst.replicaAssets(pkg); n != 1 {
		t.Fatalf("package-filter job replicated %d assets, want 1", n)
	}
	if n := dst.count(`SELECT count(*) FROM components WHERE source_id=$1 AND coordinate='cached/c'`, noCache.ReplicaSourceID); n != 0 {
		t.Fatal("cached entry replicated despite exclusion")
	}

	// Incrementals honor the same filters.
	src.publish(src.publisher, src.feed, "team", "team/a", "1.1", "a.bin", "", data("a11"))
	src.publish(src.publisher, src.feed, "team", "team/a", "3.0", "a.bin", "", data("a30"))
	src.insertAsset(cache, cd, "", "cached/d", "1.0", "d.bin", data("d10"), "{}")
	for _, j := range []Job{sel, noCache} {
		if err := svc.RunNow(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := dst.replicaAssets(sel); n != 2 {
		t.Fatalf("selective incremental has %d assets, want 2", n)
	}
	if n := dst.replicaAssets(noCache); n != 5 {
		t.Fatalf("cache-excluding incremental has %d assets, want 5", n)
	}
}

func TestChangeSequenceIsDurableGaplessAndCommitOrdered(t *testing.T) {
	ctx := context.Background()
	src := newSite(t, "src", "artifacts")
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src.publish(src.publisher, src.feed, "n", fmt.Sprintf("n/p%d", i), "1", "f.bin", "", data(fmt.Sprintf("p%d", i)))
		}()
	}
	wg.Wait()
	n := src.count("SELECT count(*) FROM replication_changes WHERE seq IS NOT NULL")
	if n != 12 || src.count("SELECT count(DISTINCT seq) FROM replication_changes") != n || src.count("SELECT max(seq)-min(seq)+1 FROM replication_changes") != n || src.head() != n {
		t.Fatalf("sequence not gapless: rows=%d head=%d", n, src.head())
	}
	if src.count("SELECT count(*) FROM replication_changes WHERE seq IS NULL") != 0 {
		t.Fatal("unassigned sequence left behind")
	}
	// Aborted transactions consume no sequence numbers.
	tx, err := src.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO components(instance_id,source_id,coordinate) VALUES($1,$2,'n/rolledback')`, src.instance, src.source); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE components SET metadata='{"x":1}' WHERE coordinate='n/rolledback'`); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if src.head() != n {
		t.Fatal("rolled-back change advanced the head")
	}
	// Replica-kind sources are never exported or recorded.
	replica, rd := src.newSource("some-replica", "replica")
	src.insertAsset(replica, rd, "n", "n/replicated", "1", "f.bin", data("r"), "{}")
	if src.head() != n {
		t.Fatal("replica-source write was recorded")
	}

	e := Exporter{DB: src.pool, Storage: src.stores.Resolve}
	var after int64
	var seen []int64
	for {
		page, err := e.Changes(ctx, src.instance, src.feed, ChangeQuery{After: after, Limit: 5, IncludeCached: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Changes {
			if c.Kind != "asset" || c.Op != "put" || c.Asset == nil {
				t.Fatalf("unexpected change %+v", c)
			}
			seen = append(seen, c.Seq)
		}
		after = page.Next
		if !page.More {
			break
		}
	}
	if len(seen) != n || after != int64(n) {
		t.Fatalf("change pages returned %d changes cursor %d", len(seen), after)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]+1 {
			t.Fatalf("changes out of order: %v", seen)
		}
	}
	// Tombstones keep their identity after the row is gone.
	src.exec("DELETE FROM assets WHERE asset_key='f.bin' AND version_id IN (SELECT v.id FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='n/p0')")
	page, err := e.Changes(ctx, src.instance, src.feed, ChangeQuery{After: after, Limit: 10, IncludeCached: true})
	if err != nil || len(page.Changes) != 1 || page.Changes[0].Op != "delete" || page.Changes[0].Coordinate != "n/p0" || page.Changes[0].Key != "f.bin" || page.Changes[0].Version != "1" {
		t.Fatalf("tombstone wrong: %+v %v", page, err)
	}
}

var callSeq int

func call(t *testing.T, c *http.Client, method, url, token string, body any, headers ...string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if method != "GET" && method != "HEAD" {
		callSeq++
		req.Header.Set("Idempotency-Key", fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), callSeq))
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestExportAuthorization(t *testing.T) {
	src := newSite(t, "src", "artifacts")
	otherFeed, otherSource, _ := src.newFeed("other", "local")
	secret := data("secret-of-other-feed")
	src.publish(src.publisherFor(otherFeed, otherSource), otherFeed, "n", "n/secret", "1", "s.bin", "", secret)
	body := data("shared")
	src.publish(src.publisher, src.feed, "n", "n/shared", "1", "s.bin", "", body)
	srv := src.serve(t)
	c := srv.Client()
	base := srv.URL + "/api/v1/replication/export/feeds/"
	token := src.grantReplicate("peer", src.feed)

	if code, _ := call(t, c, "GET", base+src.feed+"/head", token, nil); code != 200 {
		t.Fatalf("authorized head: %d", code)
	}
	if code, _ := call(t, c, "GET", base+src.feed+"/head", "", nil); code != 401 {
		t.Fatalf("anonymous head: %d", code)
	}
	if code, _ := call(t, c, "GET", base+otherFeed+"/head", token, nil); code != 403 {
		t.Fatalf("token scoped to another feed: %d", code)
	}
	readOnly, err := (identity.Tokens{DB: src.pool}).Issue(context.Background(), src.instance, src.admin.PrincipalID, src.admin.PrincipalID, []identity.Scope{{FeedID: src.feed, Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/head", "/manifest?kind=asset", "/changes?after=0"} {
		if code, _ := call(t, c, "GET", base+src.feed+path, readOnly.Secret, nil); code != 403 {
			t.Fatalf("read-only token reached %s: %d", path, code)
		}
	}
	// A blob referenced only by another feed is indistinguishable from missing.
	if code, _ := call(t, c, "GET", base+src.feed+"/blobs/"+string(digestOf(secret).Digest), token, nil); code != 404 {
		t.Fatalf("cross-feed blob: %d", code)
	}
	if code, _ := call(t, c, "GET", base+src.feed+"/blobs/"+strings.Repeat("0", 64), token, nil); code != 404 {
		t.Fatalf("unknown blob: %d", code)
	}
	if code, _ := call(t, c, "GET", base+src.feed+"/blobs/not-a-digest", token, nil); code != 400 {
		t.Fatalf("malformed digest: %d", code)
	}
	if code, _ := call(t, c, "GET", base+src.feed+"/manifest?kind=bogus", token, nil); code != 400 {
		t.Fatalf("bad manifest kind: %d", code)
	}
	code, got := call(t, c, "GET", base+src.feed+"/blobs/"+string(digestOf(body).Digest), token, nil, "Range", "bytes=3-")
	if code != 206 || !bytes.Equal(got, body[3:]) {
		t.Fatalf("range request: %d", code)
	}
	code, got = call(t, c, "GET", base+src.feed+"/manifest?kind=asset", token, nil)
	var page ManifestPage
	if code != 200 || json.Unmarshal(got, &page) != nil || len(page.Items) != 1 || page.Total == nil || *page.Total != 1 || page.Items[0].Coordinate != "n/shared" {
		t.Fatalf("manifest leaked or wrong: %d %s", code, got)
	}
}

func TestManagementAPIAndScheduler(t *testing.T) {
	src, dst := newSite(t, "src", "artifacts"), newSite(t, "dst", "mirror")
	src.publish(src.publisher, src.feed, "n", "n/one", "1", "f.bin", "", data("one"))
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)
	mux := http.NewServeMux()
	svc.Register(mux, identity.Tokens{DB: dst.pool})
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	c := api.Client()
	adm := dst.adminTok
	peerToken := src.grantReplicate("peer", src.feed)

	// A principal that can replicate/configure the feed but is not an instance admin.
	limited := dst.grantReplicate("limited", dst.feed)
	if code, _ := call(t, c, "POST", api.URL+"/api/v1/replication/peers", limited, PeerInput{Name: "x", URL: srv.URL, Token: strings.Repeat("a", 20)}); code != 403 {
		t.Fatalf("non-admin created a peer: %d", code)
	}
	if code, _ := call(t, c, "POST", api.URL+"/api/v1/replication/peers", adm, PeerInput{Name: "bad", URL: "http://example.com", Token: peerToken}); code != 400 {
		t.Fatalf("plaintext non-loopback peer accepted: %d", code)
	}
	if code, _ := call(t, c, "POST", api.URL+"/api/v1/replication/peers", adm, PeerInput{Name: "bad", URL: srv.URL, CARef: "not-configured", Token: peerToken}); code != 400 {
		t.Fatalf("unknown CA reference accepted: %d", code)
	}
	code, body := call(t, c, "POST", api.URL+"/api/v1/replication/peers", adm, PeerInput{Name: "site-a", URL: srv.URL, CARef: "peer-ca", Token: peerToken})
	var peer Peer
	if code != 201 || json.Unmarshal(body, &peer) != nil || strings.Contains(string(body), peerToken) {
		t.Fatalf("create peer: %d %s", code, body)
	}
	if code, body = call(t, c, "GET", api.URL+"/api/v1/replication/peers", adm, nil); code != 200 || strings.Contains(string(body), peerToken) {
		t.Fatalf("list peers leaked or failed: %d", code)
	}
	if n := dst.count("SELECT count(*) FROM encrypted_secrets WHERE envelope::text LIKE '%' || $1 || '%'", peerToken); n != 0 {
		t.Fatal("peer token stored in plaintext")
	}

	jobIn := JobInput{Name: "mirror-one", PeerID: peer.ID, RemoteFeedID: src.feed, DestFeedID: dst.feed, Mode: "continuous"}
	if code, _ = call(t, c, "POST", api.URL+"/api/v1/replication/jobs", limited, jobIn); code != 403 {
		t.Fatalf("job creation without configure on the feed: %d", code)
	}
	code, body = call(t, c, "POST", api.URL+"/api/v1/replication/jobs", adm, jobIn)
	var job Job
	if code != 201 || json.Unmarshal(body, &job) != nil || job.DeletionPolicy != "ignore" || job.Status.Phase != "new" {
		t.Fatalf("create job: %d %s", code, body)
	}
	if code, _ = call(t, c, "POST", api.URL+"/api/v1/replication/jobs", adm, jobIn); code != 409 {
		t.Fatalf("duplicate job: %d", code)
	}

	// The scheduler (continuous mode) seeds and then follows the source.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "seed via scheduler", func() bool { return status(t, svc, dst, job.ID).Phase == "incremental" && dst.replicaAssets(job) == 1 })
	src.publish(src.publisher, src.feed, "n", "n/two", "1", "f.bin", "", data("two"))
	waitFor(t, "continuous incremental", func() bool { return dst.replicaAssets(job) == 2 })

	code, body = call(t, c, "GET", api.URL+"/api/v1/replication/jobs/"+job.ID+"/status", adm, nil)
	var st Status
	if code != 200 || json.Unmarshal(body, &st) != nil || st.LastSuccessAt == nil || st.ChangesBehind == nil {
		t.Fatalf("status: %d %s", code, body)
	}
	// Limited principal may inspect (replicate on the feed) but not reconfigure.
	if code, _ = call(t, c, "GET", api.URL+"/api/v1/replication/jobs/"+job.ID+"/status", limited, nil); code != 200 {
		t.Fatalf("limited status: %d", code)
	}
	if code, _ = call(t, c, "PATCH", api.URL+"/api/v1/replication/jobs/"+job.ID, limited, JobPatch{Revision: job.Revision, Mode: ptr("manual")}); code != 404 {
		t.Fatalf("limited reconfigure: %d", code)
	}
	if code, body = call(t, c, "GET", api.URL+"/api/v1/replication/status", adm, nil); code != 200 || !strings.Contains(string(body), job.ID) {
		t.Fatalf("overall status: %d", code)
	}
	if code, body = call(t, c, "GET", api.URL+"/api/v1/replication/jobs/"+job.ID+"/deletions", adm, nil); code != 200 || !strings.Contains(string(body), `"policy":"ignore"`) {
		t.Fatalf("deletion preview: %d %s", code, body)
	}
	// Optimistic concurrency, manual mode, trigger.
	if code, _ = call(t, c, "PATCH", api.URL+"/api/v1/replication/jobs/"+job.ID, adm, JobPatch{Revision: job.Revision + 5, Mode: ptr("manual")}); code != 409 {
		t.Fatalf("stale revision: %d", code)
	}
	if code, _ = call(t, c, "PATCH", api.URL+"/api/v1/replication/jobs/"+job.ID, adm, JobPatch{Revision: job.Revision, Mode: ptr("manual")}); code != 200 {
		t.Fatalf("switch to manual: %d", code)
	}
	// If-Match carries the revision like the rest of the API; the body field
	// stays supported; disagreement and malformed values are rejected.
	if !job.DestinationReadOnly {
		t.Fatal("new job is not a read-only replica by default")
	}
	code, body = call(t, c, "GET", api.URL+"/api/v1/replication/jobs/"+job.ID, adm, nil)
	var cur Job
	if code != 200 || json.Unmarshal(body, &cur) != nil {
		t.Fatalf("get job: %d", code)
	}
	quoted := func(n int64) string { return fmt.Sprintf(`"%d"`, n) }
	jobURL := api.URL + "/api/v1/replication/jobs/" + job.ID
	if code, _ = call(t, c, "PATCH", jobURL, adm, JobPatch{DestinationReadOnly: ptr(false)}, "If-Match", quoted(cur.Revision+7)); code != 409 {
		t.Fatalf("stale If-Match: %d", code)
	}
	for _, bad := range []string{"3", `W/"3"`, `"x"`, `"0"`, `"03"`} {
		if code, _ = call(t, c, "PATCH", jobURL, adm, JobPatch{DestinationReadOnly: ptr(false)}, "If-Match", bad); code != 400 {
			t.Fatalf("malformed If-Match %s: %d", bad, code)
		}
	}
	if code, _ = call(t, c, "PATCH", jobURL, adm, JobPatch{Revision: cur.Revision + 1, DestinationReadOnly: ptr(false)}, "If-Match", quoted(cur.Revision)); code != 400 {
		t.Fatalf("If-Match and body disagree: %d", code)
	}
	code, body = call(t, c, "PATCH", jobURL, adm, JobPatch{DestinationReadOnly: ptr(false)}, "If-Match", quoted(cur.Revision))
	var updated Job
	if code != 200 || json.Unmarshal(body, &updated) != nil || updated.DestinationReadOnly || updated.Revision != cur.Revision+1 {
		t.Fatalf("If-Match patch: %d %s", code, body)
	}
	if code, _ = call(t, c, "PATCH", jobURL, adm, JobPatch{Revision: updated.Revision, DestinationReadOnly: ptr(true)}, "If-Match", quoted(updated.Revision)); code != 200 {
		t.Fatalf("matching header and body: %d", code)
	}
	if code, _ = call(t, c, "PATCH", api.URL+"/api/v1/replication/peers/"+peer.ID, adm, PeerPatch{CARef: ptr("peer-ca")}, "If-Match", quoted(peer.Revision)); code != 200 {
		t.Fatalf("peer If-Match: %d", code)
	}
	waitFor(t, "idle", func() bool { return !status(t, svc, dst, job.ID).Running })
	src.publish(src.publisher, src.feed, "n", "n/three", "1", "f.bin", "", data("three"))
	time.Sleep(400 * time.Millisecond)
	if dst.replicaAssets(job) != 2 {
		t.Fatal("manual job synced without a trigger")
	}
	if code, _ = call(t, c, "POST", api.URL+"/api/v1/replication/jobs/"+job.ID+"/runs", adm, nil); code != 202 {
		t.Fatalf("trigger: %d", code)
	}
	waitFor(t, "manual run", func() bool { return dst.replicaAssets(job) == 3 })
	cancel()
	<-done
	if code, _ = call(t, c, "DELETE", api.URL+"/api/v1/replication/peers/"+peer.ID, adm, nil); code != 409 {
		t.Fatalf("peer with jobs deleted: %d", code)
	}
	if code, _ = call(t, c, "DELETE", api.URL+"/api/v1/replication/jobs/"+job.ID, adm, nil); code != 204 {
		t.Fatalf("delete job: %d", code)
	}
	if code, _ = call(t, c, "DELETE", api.URL+"/api/v1/replication/peers/"+peer.ID, adm, nil); code != 204 {
		t.Fatalf("delete peer: %d", code)
	}
	// Replicated content outlives the job so the destination keeps serving it.
	if got, err := dst.read(dst.feed, "n", "n/three", "1", "f.bin"); err != nil || !bytes.Equal(got, data("three")) {
		t.Fatalf("content lost with job: %v", err)
	}
}

func TestPeerURLValidation(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://peer.example.com": true, "https://peer.example.com/base/": true, "http://127.0.0.1:8080": true, "http://localhost": true,
		"http://peer.example.com": false, "ftp://x": false, "https://u:p@peer.example.com": false, "https://peer.example.com?x=1": false, "https://": false,
	} {
		if _, err := ValidatePeerURL(raw); (err == nil) != ok {
			t.Errorf("ValidatePeerURL(%q) ok=%v want %v", raw, err == nil, ok)
		}
	}
}

func TestFilters(t *testing.T) {
	ns := ptr("team")
	f := Filters{Namespaces: []string{"team"}, Packages: []string{"team/a"}, VersionPrefixes: []string{"1.", "2."}}
	for _, c := range []struct {
		ns         *string
		coord, ver string
		want       bool
	}{{ns, "team/a", "1.2", true}, {ns, "team/a", "3.0", false}, {ns, "team/b", "1.0", false}, {ptr("other"), "team/a", "1.0", false}, {nil, "team/a", "1.0", false}, {ns, "team/a", "", true}} {
		if got := f.Match(c.ns, c.coord, c.ver); got != c.want {
			t.Errorf("Match(%v,%s,%s)=%v", c.ns, c.coord, c.ver, got)
		}
	}
	if !(Filters{}).Match(nil, "any", "1") {
		t.Error("empty filter must match everything")
	}
}
