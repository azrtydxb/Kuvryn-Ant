package oci

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
)

const testPart = 1024

// durableStore is a blob.Store whose resumable staging behaves like S3
// multipart: only whole parts survive a restart. All state lives in dir, so a
// second durableStore over the same dir models a restarted process.
type durableStore struct {
	*blob.Disk
	dir string
}

func newDurableStore(t *testing.T, disk *blob.Disk, dir string) *durableStore {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &durableStore{Disk: disk, dir: dir}
}

func (s *durableStore) StageResumable(ctx context.Context, id string) (blob.ResumableUpload, error) {
	path := filepath.Join(s.dir, id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	size := info.Size() - info.Size()%testPart // a torn part is not durable
	if err = f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}
	return &durableUpload{store: s, f: f, path: path, persisted: size}, nil
}

type durableUpload struct {
	store     *durableStore
	f         *os.File
	path      string
	persisted int64
	buffer    []byte
	closed    bool
}

func (u *durableUpload) Offset() int64 { return u.persisted }

func (u *durableUpload) Write(p []byte) (int, error) {
	u.buffer = append(u.buffer, p...)
	for len(u.buffer) >= testPart {
		if _, err := u.f.WriteAt(u.buffer[:testPart], u.persisted); err != nil {
			return 0, err
		}
		u.persisted += testPart
		u.buffer = u.buffer[testPart:]
	}
	return len(p), nil
}

func (u *durableUpload) Suspend() error {
	if u.closed {
		return nil
	}
	u.closed = true
	u.buffer = nil
	return u.f.Close()
}

func (u *durableUpload) Abort() error {
	if !u.closed {
		u.closed = true
		_ = u.f.Close()
	}
	return os.Remove(u.path)
}

func (u *durableUpload) Finalize(ctx context.Context, expected blob.Info) (blob.Info, error) {
	if u.closed {
		return blob.Info{}, blob.ErrClosed
	}
	defer u.Abort()
	if _, err := u.f.WriteAt(u.buffer, u.persisted); err != nil {
		return blob.Info{}, err
	}
	total := u.persisted + int64(len(u.buffer))
	up, err := u.store.Disk.Stage(ctx)
	if err != nil {
		return blob.Info{}, err
	}
	if _, err = io.Copy(up, io.NewSectionReader(u.f, 0, total)); err != nil {
		_ = up.Abort()
		return blob.Info{}, err
	}
	return up.Finalize(ctx, expected)
}

func resumableFixture(t *testing.T) (fixture, string, func() *durableStore) {
	t.Helper()
	f := setup(t)
	disk, err := blob.NewDisk(filepath.Join(t.TempDir(), "blobs"), blob.TransferBufferSize*4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { disk.Close() })
	dir := filepath.Join(t.TempDir(), "multipart")
	var current *durableStore
	restart := func() *durableStore {
		current = newDurableStore(t, disk, dir)
		f.service.Storage = func(context.Context, string) (blob.Store, error) { return current, nil }
		return current
	}
	restart()
	return f, dir, restart
}

func restartRegistry(t *testing.T, f *fixture) {
	t.Helper()
	r, err := New(f.pool, f.service, f.root)
	if err != nil {
		t.Fatal(err)
	}
	old := f.registry
	f.registry = r
	t.Cleanup(func() { r.Close() })
	_ = old.Close()
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func rangeHeader(from, to int) map[string]string {
	return map[string]string{"Content-Range": fmt.Sprintf("%d-%d", from, to)}
}

func (f fixture) stagingMode(t *testing.T, id string) string {
	t.Helper()
	var staging *string
	if err := f.pool.QueryRow(context.Background(), `SELECT staging FROM oci_uploads WHERE id=$1`, id).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	if staging == nil {
		return ""
	}
	return *staging
}

func (f fixture) assertBlob(t *testing.T, repo string, data []byte) {
	t.Helper()
	read := f.request("GET", "/v2/images/"+repo+"/blobs/"+digest(data), nil, nil)
	assertCode(t, read, 200)
	if !bytes.Equal(read.Body.Bytes(), data) {
		t.Fatal("stored blob differs from uploaded content")
	}
}

func TestResumableUploadSurvivesRestart(t *testing.T) {
	f, _, restart := resumableFixture(t)
	data := randomBytes(t, 5*testPart+321)
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")

	first := f.request("PATCH", location, data[:2*testPart+100], rangeHeader(0, 2*testPart+99))
	assertCode(t, first, 202)
	if got := first.Header().Get("Range"); got != fmt.Sprintf("0-%d", 2*testPart+99) {
		t.Fatalf("range %s", got)
	}
	if f.stagingMode(t, id) != "resumable" {
		t.Fatal("session did not use resumable staging")
	}

	// Process restart: new blob store and registry over the same state.
	restart()
	restartRegistry(t, &f)
	status := f.request("GET", location, nil, nil)
	assertCode(t, status, 204)
	if got := status.Header().Get("Range"); got != fmt.Sprintf("0-%d", 2*testPart+99) {
		t.Fatalf("restart lost offset: %s", got)
	}
	// A client that resumes at the wrong offset is told where to continue.
	assertCode(t, f.request("PATCH", location, []byte("x"), rangeHeader(0, 0)), 416)
	second := f.request("PATCH", location, data[2*testPart+100:4*testPart+7], rangeHeader(2*testPart+100, 4*testPart+6))
	assertCode(t, second, 202)

	restart()
	restartRegistry(t, &f)
	final := f.request("PUT", location+"?digest="+digest(data), data[4*testPart+7:], rangeHeader(4*testPart+7, len(data)-1))
	assertCode(t, final, 201)
	f.assertBlob(t, "team/app", data)
	// Retrying the completed PUT stays idempotent and leaves no staging behind.
	assertCode(t, f.request("PUT", location+"?digest="+digest(data), nil, nil), 201)
	entries, _ := os.ReadDir(f.root)
	if len(entries) != 0 {
		t.Fatalf("staging left behind: %v", entries)
	}
}

func TestResumableUploadWrongDigestRejected(t *testing.T) {
	f, dir, _ := resumableFixture(t)
	data := randomBytes(t, 3*testPart)
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	location := start.Header().Get("Location")
	assertCode(t, f.request("PATCH", location, data[:testPart+1], nil), 202)
	wrong := f.request("PUT", location+"?digest="+digest([]byte("other")), data[testPart+1:], nil)
	if wrong.Code != 400 {
		t.Fatalf("wrong digest: %d %s", wrong.Code, wrong.Body.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("multipart not aborted: %v", entries)
	}
}

func TestResumableInterruptedChunkKeepsReceivedBytes(t *testing.T) {
	f, _, restart := resumableFixture(t)
	data := randomBytes(t, 4*testPart)
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	location := start.Header().Get("Location")

	// The connection dies after 1.5 parts were received.
	cut := testPart + testPart/2
	body := io.MultiReader(bytes.NewReader(data[:cut]), errReader{})
	req := httptest.NewRequest("PATCH", location, body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("service", f.token)
	broken := httptest.NewRecorder()
	f.registry.ServeHTTP(broken, req)
	if broken.Code != 400 || broken.Header().Get("Range") != fmt.Sprintf("0-%d", cut-1) {
		t.Fatalf("interrupted chunk: %d range=%q", broken.Code, broken.Header().Get("Range"))
	}

	restart()
	restartRegistry(t, &f)
	status := f.request("GET", location, nil, nil)
	if got := status.Header().Get("Range"); got != fmt.Sprintf("0-%d", cut-1) {
		t.Fatalf("offset after restart %s", got)
	}
	final := f.request("PUT", location+"?digest="+digest(data), data[cut:], rangeHeader(cut, len(data)-1))
	assertCode(t, final, 201)
	f.assertBlob(t, "team/app", data)
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestResumableLostMultipartStateRewinds(t *testing.T) {
	f, dir, restart := resumableFixture(t)
	data := randomBytes(t, 3*testPart)
	start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, start, 202)
	location := start.Header().Get("Location")
	id := start.Header().Get("Docker-Upload-UUID")
	// Two whole parts are in multipart and the tail was rotated past them.
	assertCode(t, f.request("PATCH", location, data[:2*testPart+10], nil), 202)

	// The object store lost the multipart upload (for example a lifecycle rule).
	if err := os.Remove(filepath.Join(dir, id)); err != nil {
		t.Fatal(err)
	}
	restart()
	restartRegistry(t, &f)
	resp := f.request("PATCH", location, data[2*testPart+10:], rangeHeader(2*testPart+10, len(data)-1))
	assertCode(t, resp, 416)
	if resp.Header().Get("Range") != "0-0" {
		t.Fatalf("expected rewind to empty, got %q", resp.Header().Get("Range"))
	}
	// The client restarts from zero and completes.
	assertCode(t, f.request("PATCH", location, data[:testPart], rangeHeader(0, testPart-1)), 202)
	assertCode(t, f.request("PUT", location+"?digest="+digest(data), data[testPart:], rangeHeader(testPart, len(data)-1)), 201)
	f.assertBlob(t, "team/app", data)
}

func TestResumableConcurrentSessions(t *testing.T) {
	f, _, _ := resumableFixture(t)
	const sessions = 6
	blobs := make([][]byte, sessions)
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := range blobs {
		blobs[i] = randomBytes(t, 3*testPart+i*37+5)
	}
	for i := range blobs {
		wg.Add(1)
		go func(data []byte) {
			defer wg.Done()
			start := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
			if start.Code != 202 {
				errs <- fmt.Errorf("start %d", start.Code)
				return
			}
			location := start.Header().Get("Location")
			a, b := testPart+11, 2*testPart+3
			if r := f.request("PATCH", location, data[:a], rangeHeader(0, a-1)); r.Code != 202 {
				errs <- fmt.Errorf("patch1 %d %s", r.Code, r.Body.String())
				return
			}
			if r := f.request("PATCH", location, data[a:b], rangeHeader(a, b-1)); r.Code != 202 {
				errs <- fmt.Errorf("patch2 %d %s", r.Code, r.Body.String())
				return
			}
			if r := f.request("PUT", location+"?digest="+digest(data), data[b:], rangeHeader(b, len(data)-1)); r.Code != 201 {
				errs <- fmt.Errorf("put %d %s", r.Code, r.Body.String())
			}
		}(blobs[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, data := range blobs {
		f.assertBlob(t, "team/app", data)
	}
}

func TestResumableAbandonedSessionsAreReaped(t *testing.T) {
	f, dir, _ := resumableFixture(t)
	data := randomBytes(t, 2*testPart+9)
	expired := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, expired, 202)
	assertCode(t, f.request("PATCH", expired.Header().Get("Location"), data, nil), 202)
	cancelled := f.request("POST", "/v2/images/team/app/blobs/uploads/", nil, nil)
	assertCode(t, cancelled, 202)
	assertCode(t, f.request("PATCH", cancelled.Header().Get("Location"), data, nil), 202)
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("expected 2 multipart uploads, have %d", len(entries))
	}
	assertCode(t, f.request("DELETE", cancelled.Header().Get("Location"), nil, nil), 204)
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("cancel did not abort multipart: %d left", len(entries))
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE oci_uploads SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := f.registry.ReapExpired(context.Background(), 200); err != nil || n != 2 {
		t.Fatalf("reap %d %v", n, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("reap left multipart uploads: %d", len(entries))
	}
	if entries, _ := os.ReadDir(f.root); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("reap left staging: %s", strings.Join(names, ","))
	}
}
