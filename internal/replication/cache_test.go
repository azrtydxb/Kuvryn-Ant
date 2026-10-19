package replication

import (
	"context"
	"errors"
	"testing"
)

// Cache feeds are never replicated: not as a destination, not as an exported
// source, and their churn is not recorded in the change sequence.
func TestCacheFeedsAreExcludedFromReplication(t *testing.T) {
	src := newSite(t, "cachesrc", "libs")
	dst := newSite(t, "cachedst", "libs")
	ctx := context.Background()
	srv := src.serve(t)
	svc := dst.service(t, srv, nil)

	// Destination: a job into a cache feed is refused with a clear reason.
	cacheFeed, cacheSource, _ := dst.newFeed("buildcache", "local")
	dst.exec(`UPDATE feeds SET purpose='cache' WHERE id=$1`, cacheFeed)
	peer, err := svc.CreatePeer(ctx, dst.instance, dst.admin.PrincipalID, PeerInput{Name: "site", URL: srv.URL, CARef: "peer-ca", Token: src.grantReplicate("peer", src.feed)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateJob(ctx, dst.instance, dst.admin.PrincipalID, JobInput{Name: "into-cache", PeerID: peer.ID, RemoteFeedID: src.feed, DestFeedID: cacheFeed, Mode: "manual"})
	if !errors.Is(err, ErrCacheFeed) {
		t.Fatalf("job into a cache feed: %v, want ErrCacheFeed", err)
	}
	if dst.count(`SELECT count(*) FROM replication_jobs`) != 0 || dst.count(`SELECT count(*) FROM sources WHERE kind='replica'`) != 0 {
		t.Fatal("a refused job left rows behind")
	}

	// Source: exporting a cache feed is refused on every export call.
	srcCache, srcCacheSource, srcCacheDomain := src.newFeed("srccache", "local")
	_ = srcCacheSource
	_ = srcCacheDomain
	src.exec(`UPDATE feeds SET purpose='cache' WHERE id=$1`, srcCache)
	e := Exporter{DB: src.pool, Storage: src.stores.Resolve}
	if _, err = e.Head(ctx, src.instance, srcCache); !errors.Is(err, ErrCacheFeed) {
		t.Fatalf("Head of a cache feed: %v", err)
	}
	if _, err = e.Manifest(ctx, src.instance, srcCache, ManifestQuery{Kind: manifestAssets, IncludeCached: true}); !errors.Is(err, ErrCacheFeed) {
		t.Fatalf("Manifest of a cache feed: %v", err)
	}
	if _, err = e.Changes(ctx, src.instance, srcCache, ChangeQuery{}); !errors.Is(err, ErrCacheFeed) {
		t.Fatalf("Changes of a cache feed: %v", err)
	}
	if _, _, err = e.OpenBlob(ctx, src.instance, srcCache, "0000000000000000000000000000000000000000000000000000000000000000", true, 0, -1); !errors.Is(err, ErrCacheFeed) {
		t.Fatalf("OpenBlob of a cache feed: %v", err)
	}
	if _, err = e.Head(ctx, src.instance, src.feed); err != nil {
		t.Fatalf("an artifacts feed must stay exportable: %v", err)
	}

	// Cache churn does not enter the change sequence; release content does.
	before := src.head()
	cachePub := src.publisherFor(srcCache, srcCacheSource)
	src.publish(cachePub, srcCache, "", "ci/main/key1", "first", "ci/main/key1", "", data("cache entry"))
	src.exec(`DELETE FROM assets WHERE version_id IN (SELECT v.id FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='ci/main/key1')`)
	if src.head() != before || src.count(`SELECT count(*) FROM replication_changes WHERE source_id=$1`, srcCacheSource) != 0 {
		t.Fatal("cache feed changes were recorded for replication")
	}
	src.publish(src.publisher, src.feed, "", "lib", "1.0", "lib.bin", "", data("release"))
	if src.head() <= before {
		t.Fatal("release content stopped being recorded")
	}

	// A feed that already takes part in replication cannot become a cache feed
	// (management checks this; the trigger guard is the data-level backstop).
	if dst.count(`SELECT count(*) FROM feed_sources WHERE source_id=$1`, cacheSource) != 1 {
		t.Fatal("fixture drift")
	}
}
