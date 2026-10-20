package retention

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// staleImage stores a manifest used idleHours ago whose config and layer were
// never read individually since they were stored (stale timestamps).
func (r *ociRepo) staleImage(name string, ageHours, idleHours int) (manifest string, blobs []string) {
	cfg, layer := []byte("stale config of "+name), []byte("stale layer of "+name)
	r.put(string(sum(cfg).Digest), "blob", cfg, 500, 500)
	r.put(string(sum(layer).Digest), "blob", layer, 500, 500)
	m := []byte(fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"sha256:%s"},"layers":[{"digest":"sha256:%s"}]}`, sum(cfg).Digest, sum(layer).Digest))
	manifest = string(sum(m).Digest)
	r.put(manifest, "manifest", m, ageHours, idleHours)
	return manifest, []string{string(sum(cfg).Digest), string(sum(layer).Digest)}
}

// link records version_refs the way a cache-feed publish does.
func (r *ociRepo) link(manifest string, blobs ...string) {
	for _, b := range blobs {
		r.f.exec(r.t, `INSERT INTO version_refs(component_id,parent_version_id,ref_version) SELECT $1,v.id,$3 FROM versions v WHERE v.component_id=$1 AND v.version=$2`, r.component, manifest, b)
	}
}

func (r *ociRepo) hasVersion(version string) bool {
	return r.f.count(r.t, `SELECT count(*) FROM versions WHERE version=$1`, version) == 1
}

// gc runs retention's garbage collection twice with the grace aged out.
func (r *ociRepo) gc() {
	r.t.Helper()
	r.f.ageBlobs(r.t, 5*time.Hour)
	for range 2 {
		if _, err := r.f.svc.RunOnce(context.Background(), false); err != nil {
			r.t.Fatal(err)
		}
		r.f.ageCandidates(r.t, 5*time.Hour)
	}
}

func (r *ociRepo) readable(blobs ...string) {
	r.t.Helper()
	for _, b := range blobs {
		if !r.f.hasBytes(blobDigest(b)) {
			r.t.Fatalf("bytes of referenced blob %s are gone", b)
		}
	}
}

func TestMaxIdleNeverEvictsLayersOfAKeptManifest(t *testing.T) {
	r := newOCIRepo(t, "local")
	ctx := context.Background()
	live, liveBlobs := r.staleImage("live", 500, 1) // pulled an hour ago
	dead, deadBlobs := r.staleImage("dead", 500, 500)
	orphan := []byte("orphan layer nobody names")
	r.put(string(sum(orphan).Digest), "blob", orphan, 500, 500)
	if _, err := r.f.svc.SetPolicy(ctx, Policy{SourceID: r.src, MaxIdle: 48 * time.Hour, MinKeep: 0, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	dry, err := r.f.svc.ApplyRetention(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.f.svc.ApplyRetention(ctx, false)
	if err != nil || len(rep.Deleted) != len(dry.Deleted) {
		t.Fatalf("dry %d real %d %v", len(dry.Deleted), len(rep.Deleted), err)
	}
	if !r.hasVersion(live) {
		t.Fatal("kept manifest evicted")
	}
	for _, b := range liveBlobs {
		if !r.hasVersion(b) {
			t.Fatalf("layer %s of a kept manifest was evicted on its own idle time", b)
		}
	}
	if r.hasVersion(dead) || r.hasVersion(deadBlobs[0]) || r.hasVersion(deadBlobs[1]) {
		t.Fatal("idle manifest and the blobs only it named should be released together")
	}
	if r.hasVersion(string(sum(orphan).Digest)) {
		t.Fatal("orphan blob-only row should be evicted by idle")
	}
	r.gc()
	r.readable(liveBlobs...)
	if r.f.hasBytes(sum(orphan).Digest) {
		t.Fatal("orphan bytes not collected")
	}
}

func TestMaxAgeReleasesLayersOnlyWithTheirManifest(t *testing.T) {
	r := newOCIRepo(t, "local")
	ctx := context.Background()
	r.staleImage("old", 500, 0) // manifest created long ago but read just now
	if _, err := r.f.svc.SetPolicy(ctx, Policy{SourceID: r.src, KeepLast: 5, MaxAge: 48 * time.Hour, MinKeep: 0, AllowReleaseDelete: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// The manifest itself is older than max_age and is the only one beyond min_keep=0, so it goes;
	// its layers go with it, never before it.
	rep, err := r.f.svc.ApplyRetention(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Deleted) != 3 || r.f.count(t, `SELECT count(*) FROM versions`) != 0 {
		t.Fatalf("manifest and its two blobs should go together: %+v", rep.Deleted)
	}
}

func TestCacheFeedIdleAndMaxBytesKeepLayersOfKeptManifests(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(fmt.Sprintf("version_refs=%v", linked), func(t *testing.T) {
			r := newOCIRepo(t, "local")
			ctx := context.Background()
			r.f.exec(t, `UPDATE feeds SET purpose='cache',cache_max_idle_seconds=$1,cache_max_bytes=NULL`, int64(48*time.Hour/time.Second))
			live, liveBlobs := r.staleImage("live", 500, 1)
			dead, deadBlobs := r.staleImage("dead", 500, 500)
			if linked {
				r.link(live, liveBlobs...)
				r.link(dead, deadBlobs...)
			}
			rep, err := r.f.svc.ApplyRetention(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			_ = rep
			for _, b := range liveBlobs {
				if !r.hasVersion(b) {
					t.Fatalf("idle cache eviction removed layer %s of a kept manifest", b)
				}
			}
			if !r.hasVersion(live) || r.hasVersion(dead) {
				t.Fatalf("manifests: live=%v dead=%v", r.hasVersion(live), r.hasVersion(dead))
			}
			// A second pass releases whatever the dead manifest's blobs still hold.
			if _, err = r.f.svc.ApplyRetention(ctx, false); err != nil {
				t.Fatal(err)
			}
			for _, b := range deadBlobs {
				if r.hasVersion(b) {
					t.Fatalf("blob %s of an evicted manifest lingers", b)
				}
			}
			r.gc()
			r.readable(liveBlobs...)

			// max_bytes: make the feed too big; the least recently used image goes whole.
			newer, newerBlobs := r.staleImage("newer", 10, 0)
			if linked {
				r.link(newer, newerBlobs...)
			}
			var used int64
			if err = r.f.pool.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0) FROM blobs b WHERE EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id)`).Scan(&used); err != nil {
				t.Fatal(err)
			}
			r.f.exec(t, `UPDATE feeds SET cache_max_bytes=$1`, used-1)
			if _, err = r.f.svc.ApplyRetention(ctx, false); err != nil {
				t.Fatal(err)
			}
			survivors := 0
			for _, m := range []struct {
				name  string
				blobs []string
			}{{live, liveBlobs}, {newer, newerBlobs}} {
				if r.hasVersion(m.name) {
					survivors++
					for _, b := range m.blobs {
						if !r.hasVersion(b) {
							t.Fatalf("manifest %s kept but its blob %s was evicted", m.name, b)
						}
					}
				}
			}
			if survivors == 0 {
				t.Fatal("max_bytes evicted everything")
			}
			r.gc()
			for _, m := range []struct {
				name  string
				blobs []string
			}{{live, liveBlobs}, {newer, newerBlobs}} {
				if r.hasVersion(m.name) {
					r.readable(m.blobs...)
				}
			}
		})
	}
}

func TestPressureProjectionCountsChildrenAndBlobs(t *testing.T) {
	r := newOCIRepo(t, "remote")
	ctx := context.Background()
	r.multiArch("old", 300, 300)
	r.multiArch("mid", 200, 200)
	r.multiArch("new", 5, 0)
	var used int64
	if err := r.f.pool.QueryRow(ctx, `SELECT sum(size_bytes) FROM blobs`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	// Free about 30%: the old image (a third of the bytes) is enough, so exactly one index is evicted
	// and the projection (children and blobs included) matches what is freed.
	r.f.pressureOn(t, used*2, 40, 35)
	dry, err := r.f.svc.RunPressure(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Evicted) != 1 {
		t.Fatalf("dry run picks %d top-level versions, want 1 (children and blobs counted toward the target)", len(dry.Evicted))
	}
	rep, err := r.f.svc.RunPressure(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	vis := r.visible()
	if len(vis) != 10 {
		t.Fatalf("visible after pressure: %d, want the middle and newer images whole (10)", len(vis))
	}
	if !slices.Equal(evicted(rep)[:1], evicted(dry)[:1]) {
		t.Fatalf("live run differs from dry run: %v vs %v", evicted(rep), evicted(dry))
	}
	r.wholeOrNothing()
}

func blobDigest(h string) blob.Digest { return blob.Digest(h) }
