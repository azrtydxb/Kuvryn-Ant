package oci

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Resumable sessions keep their bytes in the blob store's persistent staging
// (S3 multipart with a state store) instead of one local file per session.
// A multipart upload only makes whole parts durable, so the session file
// becomes a "tail": an 8-byte big-endian base offset followed by every byte at
// or after that base that has been received. Together, the persisted multipart
// prefix [0,P) and the tail cover the whole upload. After a process restart the
// store reports P, the tail is replayed from P into a fresh buffer, and the
// session continues at the durable end. The tail is rotated (rewritten from the
// new P) whenever a part boundary is crossed, so it stays smaller than a part.
const tailHeader = 8

type stageTail struct {
	root       *os.Root
	name       string
	syncRoot   func() error
	f          *os.File
	base, size int64 // logical offset of the first byte; bytes after the header
}

func openTail(root *os.Root, name string, syncRoot func() error) (*stageTail, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("upload staging unavailable")
	}
	f, err := root.OpenFile(name, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	t := &stageTail{root: root, name: name, syncRoot: syncRoot, f: f}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	var header [tailHeader]byte
	if st.Size() >= tailHeader {
		if _, err = f.ReadAt(header[:], 0); err == nil {
			if base := int64(binary.BigEndian.Uint64(header[:])); base >= 0 {
				t.base, t.size = base, st.Size()-tailHeader
				return t, nil
			}
		}
	}
	if err = t.reset(0); err != nil {
		f.Close()
		return nil, err
	}
	return t, nil
}

func (t *stageTail) end() int64 { return t.base + t.size }

func (t *stageTail) close() { _ = t.f.Close() }

// reset discards the content and restarts the tail at base.
func (t *stageTail) reset(base int64) error {
	if err := t.f.Truncate(0); err != nil {
		return err
	}
	var header [tailHeader]byte
	binary.BigEndian.PutUint64(header[:], uint64(base))
	if _, err := t.f.WriteAt(header[:], 0); err != nil {
		return err
	}
	if err := t.f.Sync(); err != nil {
		return err
	}
	t.base, t.size = base, 0
	return nil
}

func (t *stageTail) append(p []byte) error {
	n, err := t.f.WriteAt(p, tailHeader+t.size)
	t.size += int64(n)
	return err
}

func (t *stageTail) sync() error { return t.f.Sync() }

// reader returns the bytes from logical offset from to the end.
func (t *stageTail) reader(from int64) *io.SectionReader {
	return io.NewSectionReader(t.f, tailHeader+(from-t.base), t.end()-from)
}

// rotate drops the bytes before newBase (now durable in the multipart prefix)
// by writing a fresh file and renaming it over the tail atomically.
func (t *stageTail) rotate(newBase int64) error {
	if newBase <= t.base {
		return nil
	}
	if newBase > t.end() {
		return t.reset(newBase)
	}
	tmpName := t.name + ".tmp"
	tmp, err := t.root.OpenFile(tmpName, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = tmp.Close()
		_ = t.root.Remove(tmpName)
		return err
	}
	var header [tailHeader]byte
	binary.BigEndian.PutUint64(header[:], uint64(newBase))
	if _, err = tmp.Write(header[:]); err != nil {
		return fail(err)
	}
	remaining := t.end() - newBase
	if _, err = io.Copy(tmp, t.reader(newBase)); err != nil {
		return fail(err)
	}
	if err = tmp.Sync(); err != nil {
		return fail(err)
	}
	if err = t.root.Rename(tmpName, t.name); err != nil {
		return fail(err)
	}
	_ = t.f.Close()
	t.f = tmp
	t.base, t.size = newBase, remaining
	return t.syncRoot()
}

// teeSink writes every byte to the durable tail first, then to the multipart
// upload, and trims the tail whenever the multipart prefix advanced.
type teeSink struct {
	tail *stageTail
	up   blob.ResumableUpload
}

func (s *teeSink) Write(p []byte) (int, error) {
	if err := s.tail.append(p); err != nil {
		return 0, err
	}
	if _, err := s.up.Write(p); err != nil {
		return len(p), err
	}
	return len(p), s.tail.rotate(s.up.Offset())
}

// abortResumable discards the resumable staging of one session (best effort).
func (r *Registry) abortResumable(ctx context.Context, instanceID, source, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if up, err := r.service.ResumableStage(ctx, instanceID, source, id); err == nil {
		_ = up.Abort()
	}
	r.removeStaging(id)
}

func (r *Registry) removeStaging(id string) {
	_ = r.root.Remove(id)
	_ = r.root.Remove(id + ".tmp")
	_ = r.syncRoot()
}

// saveProgress commits the durable offset of a resumable session and ends tx.
func (r *Registry) saveProgress(ctx context.Context, tx pgx.Tx, p identity.Principal, id string, durable int64) error {
	ctx = context.WithoutCancel(ctx)
	if err := r.requireLease(ctx, tx, p); err != nil {
		return err
	}
	if _, err := execute(ctx, tx, "UPDATE oci_uploads SET offset_bytes=$2,staging='resumable' WHERE id=$1", id, durable); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// uploadResumable serves PATCH/PUT for a session staged in resumable storage.
// It owns ru and tx's fate: tx is committed or left for the caller's rollback.
func (r *Registry) uploadResumable(ctx context.Context, w http.ResponseWriter, req *http.Request, tx pgx.Tx, p identity.Principal, rep repository, id, source string, dbOffset int64, ru blob.ResumableUpload) {
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if s, ok := ru.(blob.Suspender); ok {
			_ = s.Suspend()
		} else {
			_ = ru.Abort()
		}
	}
	defer release()

	digest := blob.Digest("")
	if req.Method == "PUT" {
		values := req.URL.Query()["digest"]
		if len(values) != 1 {
			failure(w, 400, "DIGEST_INVALID", "one digest required")
			return
		}
		var valid bool
		if digest, valid = parseDigest(values[0]); !valid {
			failure(w, 400, "DIGEST_INVALID", "invalid digest")
			return
		}
	}
	tail, err := openTail(r.root, id, r.syncRoot)
	if err != nil {
		failure(w, 503, "UNKNOWN", "upload staging unavailable")
		return
	}
	defer func() { tail.close() }()

	// Reconcile the multipart prefix with the tail. Bytes the tail holds beyond
	// the prefix are replayed; if the store lost progress the upload rewinds to
	// what it still has.
	prefix := ru.Offset()
	if prefix < tail.base || tail.end() < prefix {
		if err = tail.reset(prefix); err != nil {
			failService(w, err)
			return
		}
	}
	durable := tail.end()
	if durable > MaxBlob {
		failure(w, 503, "UNKNOWN", "invalid upload state")
		return
	}
	if durable > prefix {
		if _, err = io.Copy(ru, tail.reader(prefix)); err != nil {
			failService(w, err)
			return
		}
		if err = tail.rotate(ru.Offset()); err != nil {
			failService(w, err)
			return
		}
	}

	rangeMismatch := func(msg string) {
		if err := r.saveProgress(ctx, tx, p, id, durable); err != nil {
			failService(w, err)
			return
		}
		uploadHeaders(w, rep, id, durable)
		w.Header().Del("Content-Length")
		failure(w, 416, "RANGE_INVALID", msg)
	}
	expectedChunk := int64(-1)
	ranges := req.Header.Values("Content-Range")
	switch {
	case len(ranges) > 1:
		failure(w, 416, "RANGE_INVALID", "invalid upload range")
		return
	case len(ranges) == 1:
		parts := strings.Split(ranges[0], "-")
		if len(parts) != 2 {
			failure(w, 416, "RANGE_INVALID", "invalid upload range")
			return
		}
		start, e1 := strconv.ParseInt(parts[0], 10, 64)
		end, e2 := strconv.ParseInt(parts[1], 10, 64)
		if e1 != nil || e2 != nil || start != durable || end < start || end == MaxBlob || (req.ContentLength >= 0 && end-start+1 != req.ContentLength) {
			rangeMismatch("upload offset mismatch")
			return
		}
		expectedChunk = end - start + 1
	case durable != dbOffset && !(req.Method == "PUT" && req.ContentLength == 0):
		// The client does not know the offset we recovered; it must ask first.
		rangeMismatch("upload offset mismatch")
		return
	}

	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	limited := http.MaxBytesReader(w, req.Body, MaxBlob-durable)
	n, err := io.CopyBuffer(&teeSink{tail: tail, up: ru}, &progressReader{ctx: ctx, reader: limited, controller: controller}, make([]byte, 64<<10))
	_ = controller.SetReadDeadline(time.Time{})
	// Whatever reached the tail is durable and counted, even from a broken request.
	if serr := tail.sync(); serr != nil && err == nil {
		err = serr
	}
	durable = tail.end()
	partial := func(status int, code, msg string) {
		release()
		if serr := r.saveProgress(ctx, tx, p, id, durable); serr != nil {
			failService(w, serr)
			return
		}
		uploadHeaders(w, rep, id, durable)
		w.Header().Del("Content-Length")
		failure(w, status, code, msg)
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			partial(413, "SIZE_INVALID", "upload exceeds addressable size")
		} else {
			partial(400, "BLOB_UPLOAD_INVALID", "upload chunk interrupted")
		}
		return
	}
	if expectedChunk >= 0 && n != expectedChunk {
		partial(416, "RANGE_INVALID", "upload range length mismatch")
		return
	}
	if req.Method == "PATCH" {
		release()
		if err = r.saveProgress(ctx, tx, p, id, durable); err != nil {
			failService(w, err)
			return
		}
		uploadHeaders(w, rep, id, durable)
		w.WriteHeader(202)
		return
	}

	publication := rep.publication(string(digest), "blob")
	publication.ExpectedSource = source
	publication.MediaType = "application/octet-stream"
	publication.Info = blob.Info{Digest: digest, Size: durable}
	_, consumed, err := r.service.PublishStaged(ctx, p, publication, ru)
	if consumed {
		released = true // Finalize released the upload whatever the outcome.
	}
	if err != nil {
		if consumed {
			// The staged object is gone; the client must start a new session.
			bg := context.WithoutCancel(ctx)
			if r.requireLease(bg, tx, p) == nil {
				if _, e := execute(bg, tx, "UPDATE oci_uploads SET state='cancelled' WHERE id=$1", id); e == nil {
					_ = commit(bg, tx)
				}
			}
			r.removeStaging(id)
		} else {
			release()
			_ = r.saveProgress(ctx, tx, p, id, durable)
		}
		failService(w, err)
		return
	}
	if err = r.requireLease(ctx, tx, p); err != nil {
		failService(w, err)
		return
	}
	if _, err = execute(ctx, tx, "UPDATE oci_uploads SET state='complete',digest=$2,offset_bytes=$3,staging='resumable' WHERE id=$1", id, string(digest), durable); err != nil {
		failService(w, err)
		return
	}
	if err = commit(ctx, tx); err != nil {
		failService(w, err)
		return
	}
	tail.close()
	r.removeStaging(id)
	blobCreated(w, rep, string(digest))
}
