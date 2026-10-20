package rawrepo

import (
	"context"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

// Bytes that are gone behind live metadata (cache content dropped by a
// best-effort backup or lost to a sweep race) answer 404, a cache miss for the
// client, not a server error. Re-uploading the same content heals it.
func TestRawMissingBytesAreACacheMiss(t *testing.T) {
	f := newPublishFixture(t)
	body := "cache object"
	if w := f.request("PUT", "team/c.bin", body, nil); w.Code != 201 {
		t.Fatalf("put %d", w.Code)
	}
	if err := f.disk.Delete(context.Background(), blob.Digest(checksum(body))); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"GET", "HEAD"} {
		if w := f.request(m, "team/c.bin", "", nil); w.Code == 503 || w.Code >= 500 {
			t.Fatalf("%s missing bytes: %d", m, w.Code)
		}
	}
	if w := f.request("GET", "team/c.bin", "", nil); w.Code != 404 {
		t.Fatalf("GET missing bytes: %d", w.Code)
	}
}
