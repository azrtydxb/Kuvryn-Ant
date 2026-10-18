package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrResumeBusy reports that a partial transfer with the same identifier is
// already open in this process.
var ErrResumeBusy = errors.New("partial blob transfer is already in progress")

const (
	resumePrefix = "resume-"
	// ResumeMaxAge is how long an abandoned partial disk transfer is kept.
	ResumeMaxAge = 7 * 24 * time.Hour
)

// Suspender is an optional capability of a ResumableUpload: Suspend releases
// the upload (buffers, admission slot) but keeps the durable partial progress
// for a later StageResumable of the same identifier. Abort discards it.
type Suspender interface {
	Suspend() error
}

// StageResumable implements ResumableStager for the disk store. The id is the
// blob's digest: the partial bytes live in a private staging file named by it
// and are never visible as an object until Finalize has verified the full
// digest and size. The existing partial is re-hashed on open, so a corrupted or
// truncated leftover cannot bypass the final check.
func (d *Disk) StageResumable(ctx context.Context, id string) (ResumableUpload, error) {
	digest := Digest(id)
	if !digest.Valid() {
		return nil, ErrInvalidDigest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-d.slots }
	d.resumeMu.Lock()
	if d.resuming == nil {
		d.resuming = map[Digest]bool{}
	}
	if d.resuming[digest] {
		d.resumeMu.Unlock()
		release()
		return nil, ErrResumeBusy
	}
	d.resuming[digest] = true
	d.resumeMu.Unlock()
	unmark := func() {
		d.resumeMu.Lock()
		delete(d.resuming, digest)
		d.resumeMu.Unlock()
	}
	fail := func(err error) (ResumableUpload, error) {
		unmark()
		release()
		return nil, err
	}
	if err := d.safeDir("staging"); err != nil {
		return fail(err)
	}
	name := filepath.Join("staging", resumePrefix+string(digest))
	if info, err := d.root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		if err := d.root.Remove(name); err != nil {
			return fail(err)
		}
	}
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	buffer := make([]byte, TransferBufferSize)
	n, err := io.CopyBuffer(h, contextReader{ctx, f}, buffer)
	if err != nil {
		_ = f.Close()
		return fail(err)
	}
	// The read left the offset at the end of the file, so writes append.
	return &diskUpload{disk: d, ctx: ctx, file: f, name: name, digest: h, size: n, buffer: buffer, onClose: unmark}, nil
}

func (u *diskUpload) Offset() int64 { u.mu.Lock(); defer u.mu.Unlock(); return u.size }

// Suspend keeps the partial file. It is a no-op after Finalize or Abort.
func (u *diskUpload) Suspend() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	if u.size == 0 {
		// Nothing worth keeping: behave like Abort so empty files do not pile up.
		return u.cleanup()
	}
	u.closed = true
	syncErr := u.file.Sync()
	closeErr := u.file.Close()
	u.buffer = nil
	<-u.disk.slots
	if u.onClose != nil {
		u.onClose()
	}
	return errors.Join(syncErr, closeErr)
}

// sweepResumable removes partial transfers abandoned for longer than ResumeMaxAge.
func (d *Disk) sweepResumable(now time.Time) {
	dir, err := d.root.Open("staging")
	if err != nil {
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), resumePrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > ResumeMaxAge {
			_ = d.root.Remove(filepath.Join("staging", e.Name()))
		}
	}
}

// Suspend keeps the persisted multipart progress (the resume state file and the
// uploaded parts) and releases the in-process buffer and admission slot. Bytes
// after the last persisted part boundary were only in the buffer and must be
// sent again, exactly as after a restart.
func (r resumableS3) Suspend() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.buffer = nil
	<-r.store.slots
	return nil
}

var (
	_ ResumableStager = (*Disk)(nil)
	_ Suspender       = (*diskUpload)(nil)
	_ Suspender       = resumableS3{}
)
