package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func resumeFixture(t *testing.T, dir string, f *s3Fixture, clock func() time.Time) *S3 {
	t.Helper()
	s, err := newS3(S3Config{Region: "r", Bucket: "test-bucket", Prefix: "ant/test", PartSize: s3MinimumPartSize, BufferBudget: s3MinimumPartSize * 4, StateStore: &FileStateStore{Dir: dir}, Now: clock}, f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3ResumeAfterProcessRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := &s3Fixture{objects: map[string][]byte{}, uploads: map[string]*fixtureMultipart{}}
	data := bytes.Repeat([]byte("0123456789abcdef"), (2*s3MinimumPartSize+12345)/16+1)
	sum := sha256.Sum256(data)
	want := Info{Digest(hex.EncodeToString(sum[:])), int64(len(data))}

	// First "process": write 1.5 parts, then die without Finalize or Abort.
	first := resumeFixture(t, dir, f, nil)
	u, err := first.StageResumable(ctx, "upload-1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Offset() != 0 {
		t.Fatal("fresh offset")
	}
	if _, err = u.Write(data[:s3MinimumPartSize+777]); err != nil {
		t.Fatal(err)
	}
	if u.Offset() != s3MinimumPartSize {
		t.Fatalf("offset %d", u.Offset())
	}
	calls := f.partCalls

	// Second "process": a new store over the same state directory and bucket.
	second := resumeFixture(t, dir, f, nil)
	r, err := second.StageResumable(ctx, "upload-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Offset() != s3MinimumPartSize {
		t.Fatalf("resumed offset %d", r.Offset())
	}
	if _, err = r.Write(data[r.Offset():]); err != nil {
		t.Fatal(err)
	}
	got, err := r.Finalize(ctx, want)
	if err != nil || got != want {
		t.Fatalf("finalize %+v %v", got, err)
	}
	// Only the missing parts were uploaded: part 1 was not re-sent.
	if f.partCalls-calls != 2 {
		t.Fatalf("uploaded %d parts after resume, want 2", f.partCalls-calls)
	}
	if st, ok, _ := (&FileStateStore{Dir: dir}).Load(ctx, "upload-1"); ok {
		t.Fatalf("state not removed: %+v", st)
	}
	stored := f.objects["ant/test/objects/"+string(want.Digest)]
	if !bytes.Equal(stored, data) {
		t.Fatal("stored content differs")
	}
}

func TestS3ResumeDiscardsStateWhenRemoteUploadGone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := &s3Fixture{objects: map[string][]byte{}, uploads: map[string]*fixtureMultipart{}}
	s := resumeFixture(t, dir, f, nil)
	u, err := s.StageResumable(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.Write(make([]byte, s3MinimumPartSize)); err != nil {
		t.Fatal(err)
	}
	f.uploads = map[string]*fixtureMultipart{} // remote upload expired
	r, err := resumeFixture(t, dir, f, nil).StageResumable(ctx, "gone")
	if err != nil {
		t.Fatal(err)
	}
	if r.Offset() != 0 {
		t.Fatal("must restart from zero")
	}
	_ = r.Abort()
	if _, err := s.StageResumable(ctx, "../bad"); !errors.Is(err, ErrInvalidUploadID) {
		t.Fatal(err)
	}
}

func TestS3AbortStaleUploads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	f := &s3Fixture{objects: map[string][]byte{}, uploads: map[string]*fixtureMultipart{}, now: clock}
	s := resumeFixture(t, dir, f, clock)
	if _, err := s.StageResumable(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	// A leaked, non-resumable staging upload.
	leaked, err := s.Stage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaked.(*s3Upload).ensureMultipart(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(23 * time.Hour)
	fresh, err := s.StageResumable(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // old and leaked are 25h idle; fresh is 2h
	n, err := s.AbortStaleUploads(ctx, 24*time.Hour, true)
	if err != nil || n != 2 || len(f.uploads) != 3 {
		t.Fatalf("dry run n=%d uploads=%d err=%v", n, len(f.uploads), err)
	}
	n, err = s.AbortStaleUploads(ctx, 24*time.Hour, false)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(f.uploads) != 1 {
		t.Fatalf("uploads left %d", len(f.uploads))
	}
	if _, ok, _ := s.config.StateStore.Load(ctx, "old"); ok {
		t.Fatal("stale state kept")
	}
	if _, ok, _ := s.config.StateStore.Load(ctx, "fresh"); !ok {
		t.Fatal("fresh state removed")
	}
	_ = fresh.Abort()
}
