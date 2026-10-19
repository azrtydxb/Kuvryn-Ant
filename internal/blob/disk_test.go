package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func content(data []byte) Info {
	sum := sha256.Sum256(data)
	return Info{Digest(hex.EncodeToString(sum[:])), int64(len(data))}
}
func disk(t *testing.T, budget int64) (*Disk, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	d, err := NewDisk(path, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}
func TestVerifiedPublishAndRanges(t *testing.T) {
	d, path := disk(t, 0)
	ctx := context.Background()
	data := bytes.Repeat([]byte("abcdef"), 30000)
	want := content(data)
	got, err := d.Put(ctx, bytes.NewReader(data), want)
	if err != nil || got != want {
		t.Fatalf("publish: %+v %v", got, err)
	}
	got, err = d.Stat(ctx, want.Digest)
	if err != nil || got != want {
		t.Fatalf("stat: %+v %v", got, err)
	}
	for _, r := range []struct{ off, length int64 }{{0, -1}, {33, 85}, {want.Size, 0}, {want.Size - 3, -1}} {
		reader, err := d.OpenRange(ctx, want.Digest, r.off, r.length)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		end := want.Size
		if r.length != -1 {
			end = r.off + r.length
		}
		if !bytes.Equal(got, data[r.off:end]) {
			t.Fatal("range mismatch")
		}
	}
	for _, r := range [][2]int64{{-1, 1}, {0, -2}, {want.Size + 1, 0}, {1, want.Size}} {
		if _, err := d.OpenRange(ctx, want.Digest, r[0], r[1]); !errors.Is(err, ErrRange) {
			t.Fatalf("range %v: %v", r, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(path, "staging"))
	if len(entries) != 0 {
		t.Fatal("staging leak")
	}
}
func TestRejectedUploadsNeverPublish(t *testing.T) {
	d, path := disk(t, TransferBufferSize)
	data := []byte("blob")
	want := content(data)
	for _, expected := range []Info{{want.Digest, 99}, {content([]byte("other")).Digest, 4}, {"../escape", 4}} {
		if _, err := d.Put(context.Background(), bytes.NewReader(data), expected); err == nil {
			t.Fatal("invalid content accepted")
		}
	}
	if _, err := d.Stat(context.Background(), want.Digest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unexpected publication: %v", err)
	}
	for _, dir := range []string{"objects", "staging"} {
		entries, _ := os.ReadDir(filepath.Join(path, dir))
		if len(entries) != 0 {
			t.Fatalf("%s leak", dir)
		}
	}
}
func TestConcurrentDedupAndExistingCorruption(t *testing.T) {
	d, path := disk(t, TransferBufferSize*8)
	data := bytes.Repeat([]byte("immutable"), 10000)
	want := content(data)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.Put(context.Background(), bytes.NewReader(data), want)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(path, "objects"))
	if len(entries) != 1 {
		t.Fatalf("objects=%d", len(entries))
	}
	object := filepath.Join(path, "objects", string(want.Digest))
	if err := os.Chmod(object, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put(context.Background(), bytes.NewReader(data), want); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt dedup accepted: %v", err)
	}
	got, _ := os.ReadFile(object)
	if string(got) != "corrupt" {
		t.Fatal("existing content overwritten")
	}
}
func TestCancellationAbortAndBudget(t *testing.T) {
	d, path := disk(t, TransferBufferSize)
	ctx, cancel := context.WithCancel(context.Background())
	upload, err := d.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	queued, cancelQueued := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelQueued()
	if _, err := d.Stage(queued); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budget did not block: %v", err)
	}
	cancel()
	if _, err := upload.Write([]byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("write cancellation: %v", err)
	}
	if _, err := upload.Finalize(context.Background(), content(nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("finalize cancellation: %v", err)
	}
	if err := upload.Abort(); err != nil {
		t.Fatal(err)
	}
	next, err := d.Stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := next.Write(nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(path, "staging"))
	if len(entries) != 0 {
		t.Fatal("cancelled upload leaked")
	}
	want := content([]byte("read"))
	if _, err := d.Put(context.Background(), bytes.NewBufferString("read"), want); err != nil {
		t.Fatal(err)
	}
	readCtx, stop := context.WithCancel(context.Background())
	reader, err := d.OpenRange(readCtx, want.Digest, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	stop()
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestRejectUnsafePaths(t *testing.T) {
	d, path := disk(t, 0)
	ctx := context.Background()
	for _, digest := range []Digest{"../escape", "/tmp/escape", Digest(string(bytes.Repeat([]byte("A"), 64))), ""} {
		if _, err := d.Stat(ctx, digest); !errors.Is(err, ErrInvalidDigest) {
			t.Fatalf("unsafe digest %q: %v", digest, err)
		}
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := content([]byte("outside")).Digest
	if err := os.Symlink(target, filepath.Join(path, "objects", string(digest))); err != nil {
		t.Fatal(err)
	}
	if _, err := d.OpenRange(ctx, digest, 0, -1); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink accepted: %v", err)
	}
	symlink := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDisk(symlink, 0); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink root accepted: %v", err)
	}
	root := filepath.Join(t.TempDir(), "unsafe")
	os.Mkdir(root, 0755)
	if _, err := NewDisk(root, 0); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("shared root accepted: %v", err)
	}
}

type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, "partial"), nil
}
func TestSourceFailureAndEmptyBlob(t *testing.T) {
	d, path := disk(t, TransferBufferSize)
	if _, err := d.Put(context.Background(), &failingReader{}, content([]byte("partial"))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	for _, dir := range []string{"staging", "objects"} {
		entries, _ := os.ReadDir(filepath.Join(path, dir))
		if len(entries) != 0 {
			t.Fatalf("source failure left %s", dir)
		}
	}
	if got, err := d.Put(context.Background(), bytes.NewReader(nil), content(nil)); err != nil || got != content(nil) {
		t.Fatalf("empty object: %+v %v", got, err)
	}
}

// checkpointContext injects cancellation immediately after a successful context
// check. This models a concurrent cancel between the last pre-publication check
// and deduplication reads, without timing-dependent sleeps or large test files.
type checkpointContext struct {
	context.Context
	cancel  context.CancelFunc
	checks  atomic.Int32
	trigger atomic.Int32
}

func (c *checkpointContext) Err() error {
	err := c.Context.Err()
	if c.checks.Add(1) == c.trigger.Load() {
		c.cancel()
	}
	return err
}
func TestFinalizeObservesStageCancellationDuringDedup(t *testing.T) {
	d, _ := disk(t, TransferBufferSize)
	data := bytes.Repeat([]byte("deduplication"), 10000)
	expected := content(data)
	if _, err := d.Put(context.Background(), bytes.NewReader(data), expected); err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	staging := &checkpointContext{Context: base, cancel: cancel}
	upload, err := d.Stage(staging)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = upload.Write(data); err != nil {
		t.Fatal(err)
	}
	staging.checks.Store(0)
	staging.trigger.Store(2)
	if _, err = upload.Finalize(context.Background(), expected); !errors.Is(err, context.Canceled) {
		t.Fatalf("stage cancellation ignored during dedup: %v", err)
	}
	// Finalize releases the slot even when it aborts duplicate verification.
	next, err := d.Stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Abort(); err != nil {
		t.Fatal(err)
	}
}

// A Kubernetes fsGroup makes the volume root group-writable (setgid, group =
// the pod's own gid); startup must accept that but still reject world access.
func TestNewDiskAcceptsFsGroupWritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	d, err := NewDisk(root, 0)
	if err != nil {
		t.Fatalf("group-writable root rejected: %v", err)
	}
	d.Close()
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDisk(root, 0); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("world-writable root accepted: %v", err)
	}
}
