package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResumableStagingKeepsPartialAndVerifiesAtEnd(t *testing.T) {
	d, path := disk(t, 0)
	ctx := context.Background()
	data := bytes.Repeat([]byte("resume-me"), 20000)
	info := content(data)
	up, err := d.StageResumable(ctx, string(info.Digest))
	if err != nil || up.Offset() != 0 {
		t.Fatalf("first stage: %v", err)
	}
	if _, err = up.Write(data[:70000]); err != nil {
		t.Fatal(err)
	}
	if _, err = d.StageResumable(ctx, string(info.Digest)); !errors.Is(err, ErrResumeBusy) {
		t.Fatalf("concurrent resume: %v", err)
	}
	if err = suspend(up); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Stat(ctx, info.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial must not be an object: %v", err)
	}
	up, err = d.StageResumable(ctx, string(info.Digest))
	if err != nil || up.Offset() != 70000 {
		t.Fatalf("resume: %v", err)
	}
	if _, err = up.Write(data[70000:]); err != nil {
		t.Fatal(err)
	}
	if got, err := up.Finalize(ctx, info); err != nil || got != info {
		t.Fatalf("finalize: %+v %v", got, err)
	}
	r, err := d.OpenRange(ctx, info.Digest, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(b, data) {
		t.Fatal("content mismatch")
	}
	if _, err = os.Stat(filepath.Join(path, "staging", resumePrefix+string(info.Digest))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial left behind: %v", err)
	}
}

func TestResumableCorruptPartialFailsAndIsDiscarded(t *testing.T) {
	d, _ := disk(t, 0)
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 5000)
	info := content(data)
	up, _ := d.StageResumable(ctx, string(info.Digest))
	_, _ = up.Write(bytes.Repeat([]byte("y"), 2000)) // wrong prefix
	_ = suspend(up)
	up, err := d.StageResumable(ctx, string(info.Digest))
	if err != nil || up.Offset() != 2000 {
		t.Fatalf("resume: %v", err)
	}
	_, _ = up.Write(data[2000:])
	if _, err = up.Finalize(ctx, info); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("want integrity error, got %v", err)
	}
	up, err = d.StageResumable(ctx, string(info.Digest))
	if err != nil || up.Offset() != 0 {
		t.Fatalf("corrupt partial must be discarded: %v", err)
	}
	_ = up.Abort()
}

func TestSweepResumableRemovesOnlyStalePartials(t *testing.T) {
	d, path := disk(t, 0)
	ctx := context.Background()
	fresh := content([]byte("fresh"))
	stale := content([]byte("stale"))
	for _, i := range []Info{fresh, stale} {
		up, _ := d.StageResumable(ctx, string(i.Digest))
		_, _ = up.Write([]byte("part"))
		_ = suspend(up)
	}
	old := time.Now().Add(-2 * ResumeMaxAge)
	staleName := filepath.Join(path, "staging", resumePrefix+string(stale.Digest))
	if err := os.Chtimes(staleName, old, old); err != nil {
		t.Fatal(err)
	}
	d.sweepResumable(time.Now())
	if _, err := os.Stat(staleName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale partial kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "staging", resumePrefix+string(fresh.Digest))); err != nil {
		t.Fatalf("fresh partial removed: %v", err)
	}
}

func suspend(up ResumableUpload) error { return up.(Suspender).Suspend() }
