package retention

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Blob-only OCI carriers keep taking part in idle eviction and garbage
// collection: hiding them from version listings must not make their bytes
// immortal.
func TestBlobOnlyCarriersAreStillEvictedAndFreed(t *testing.T) {
	f := newFixture(t, Options{Grace: time.Hour})
	ctx := context.Background()
	f.anyDomain()
	_, src, dom := f.newSource(t, "cache", "remote")
	stale := f.seedSized(t, src, dom, "team/app", "stale-blob", sized{size: 10, idleHours: -1, ageHours: 24 * 90})
	f.seedSized(t, src, dom, "team/app", "fresh-blob", sized{size: 12, idleHours: 0, ageHours: 1})
	f.exec(t, `UPDATE versions SET blob_only=true`)
	f.configure(t, func(s *Settings) { s.Default = DefaultPolicy{Enabled: true, MaxIdle: 30 * 24 * time.Hour, MinKeep: 1} })
	rep, err := f.svc.ApplyRetention(ctx, false)
	if err != nil || !slices.Equal(names(rep.Deleted), []string{"team/app@stale-blob"}) {
		t.Fatalf("idle eviction of blob-only carrier: %+v %v", rep, err)
	}
	f.ageBlobs(t, 5*time.Hour)
	for range 2 {
		if _, err = f.svc.RunOnce(ctx, false); err != nil {
			t.Fatal(err)
		}
		f.ageCandidates(t, 5*time.Hour)
	}
	if f.hasBytes(stale.Digest) {
		t.Fatal("evicted blob bytes were not freed")
	}
}
