package rawrepo

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"context"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// anonymous_read must open raw feed reads (and only reads) through the shared
// client-authentication middleware.
func TestAnonymousReadHonored(t *testing.T) {
	f := newPublishFixture(t)
	if w := f.request("PUT", "doc/a.txt", "hello", map[string]string{"X-Checksum-Sha256": checksum("hello")}); w.Code != 201 && w.Code != 200 && w.Code != 204 {
		t.Fatalf("publish %d %s", w.Code, w.Body)
	}
	var instance string
	if err := f.pool.QueryRow(context.Background(), "SELECT instance_id::text FROM feeds WHERE id=$1", f.feed).Scan(&instance); err != nil {
		t.Fatal(err)
	}
	auth := &identity.ClientAuth{DB: f.pool, InstanceID: instance, CacheTTL: -1}
	h := auth.Middleware(f.repo)
	anon := func(method string) int {
		req := httptest.NewRequest(method, "/feeds/artifacts/raw/doc/a.txt", strings.NewReader(""))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := anon("GET"); code != 401 {
		t.Fatalf("private feed anonymous GET: %d", code)
	}
	if _, err := f.pool.Exec(context.Background(), "UPDATE feeds SET anonymous_read=true WHERE id=$1", f.feed); err != nil {
		t.Fatal(err)
	}
	if code := anon("GET"); code != 200 {
		t.Fatalf("anonymous_read GET: %d", code)
	}
	if code := anon("HEAD"); code != 200 {
		t.Fatalf("anonymous_read HEAD: %d", code)
	}
	if code := anon("PUT"); code != 401 {
		t.Fatalf("anonymous PUT must stay denied: %d", code)
	}
}

// An upload without a declared digest is spooled in the configured spool
// directory (the data volume), never in the container's temp directory, and a
// large body still succeeds.
func TestDigestlessPutSpoolsToSpoolDir(t *testing.T) {
	f := newPublishFixture(t)
	spool := filepath.Join(t.TempDir(), "spool")
	f.repo.SetSpoolDir(spool)
	t.Setenv("TMPDIR", t.TempDir()) // must not be used
	const size = 24 << 20
	spooled := -1
	body := &probeReader{remaining: size, probe: func() {
		entries, _ := os.ReadDir(spool)
		spooled = 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "ant-raw-") {
				spooled++
			}
		}
	}}
	req := httptest.NewRequest("PUT", "/feeds/artifacts/raw/big.bin", body)
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+f.secret)
	w := httptest.NewRecorder()
	f.repo.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("digest-less upload: %d %s", w.Code, w.Body)
	}
	if spooled != 1 {
		t.Fatalf("expected one spool file in %s during the upload, saw %d", spool, spooled)
	}
	if left, _ := os.ReadDir(spool); len(left) != 0 {
		t.Fatalf("spool not cleaned: %d files", len(left))
	}
	if left, _ := os.ReadDir(os.Getenv("TMPDIR")); len(left) != 0 {
		t.Fatal("upload touched the container temp directory")
	}
}

type probeReader struct {
	remaining int
	read      int
	probe     func()
}

func (p *probeReader) Read(b []byte) (int, error) {
	if p.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(b), p.remaining)
	for i := range b[:n] {
		b[i] = byte(p.read + i)
	}
	p.remaining -= n
	p.read += n
	if p.read >= 8<<20 && p.probe != nil {
		p.probe()
		p.probe = nil
	}
	return n, nil
}
