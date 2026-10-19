package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/azrtydxb/Kuvryn-Ant/internal/safedir"
	"time"
)

const TransferBufferSize = 64 << 10
const DefaultBufferBudget = 1 << 30

// Disk uses a private directory on a local filesystem supporting hard links and
// directory fsync. The application account must be its only writer. Each active
// upload reserves a fixed transfer-buffer slot; this bounds Put's total buffers.
// Callers using Stage directly own and budget their caller-provided write buffers.
type Disk struct {
	root  *os.Root
	slots chan struct{}

	resumeMu sync.Mutex
	resuming map[Digest]bool
}

func NewDisk(path string, bufferBudget int64) (*Disk, error) {
	if bufferBudget == 0 {
		bufferBudget = DefaultBufferBudget
	}
	if bufferBudget < TransferBufferSize {
		return nil, fmt.Errorf("buffer budget must be at least %d", TransferBufferSize)
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !safedir.Private(info) {
		return nil, ErrUnsafePath
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	d := &Disk{root: root, slots: make(chan struct{}, bufferBudget/TransferBufferSize)}
	for _, name := range []string{"staging", "objects"} {
		if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			root.Close()
			return nil, err
		}
		info, err := root.Lstat(name)
		if err != nil || !safedir.Private(info) {
			root.Close()
			return nil, ErrUnsafePath
		}
	}
	if err := d.syncDir("."); err != nil {
		root.Close()
		return nil, err
	}
	d.sweepResumable(time.Now())
	return d, nil
}

// Close requires callers to first finish or abort every active upload and reader.
func (d *Disk) Close() error { return d.root.Close() }
func (d *Disk) syncDir(name string) error {
	f, err := d.root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (d *Disk) safeDir(name string) error {
	i, err := d.root.Lstat(name)
	if err != nil {
		return err
	}
	if !safedir.Private(i) {
		return ErrUnsafePath
	}
	return nil
}
func (d *Disk) Stage(ctx context.Context) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := d.safeDir("staging"); err != nil {
		<-d.slots
		return nil, err
	}
	name := filepath.Join("staging", rand.Text())
	f, err := d.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		<-d.slots
		return nil, err
	}
	return &diskUpload{disk: d, ctx: ctx, file: f, name: name, digest: sha256.New(), buffer: make([]byte, TransferBufferSize)}, nil
}

// Put streams without buffering the object. Cancellation is checked between
// reads; the caller must unblock a source whose Read is itself stuck.
func (d *Disk) Put(ctx context.Context, source io.Reader, expected Info) (Info, error) {
	upload, err := d.Stage(ctx)
	if err != nil {
		return Info{}, err
	}
	defer upload.Abort()
	_, err = io.CopyBuffer(upload, contextReader{ctx, source}, upload.(*diskUpload).buffer)
	if err != nil {
		return Info{}, err
	}
	return upload.Finalize(ctx, expected)
}

type diskUpload struct {
	mu     sync.Mutex
	disk   *Disk
	ctx    context.Context
	file   *os.File
	name   string
	digest hash.Hash
	size   int64
	closed bool
	buffer []byte
	// onClose runs once when the upload is finalized, aborted or suspended.
	onClose func()
}

func (u *diskUpload) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return 0, ErrClosed
	}
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := u.file.Write(p)
	u.digest.Write(p[:n])
	u.size += int64(n)
	return n, err
}
func (u *diskUpload) cleanup() error {
	if u.closed {
		return nil
	}
	u.closed = true
	closeErr := u.file.Close()
	removeErr := u.disk.root.Remove(u.name)
	syncErr := u.disk.syncDir("staging")
	u.buffer = nil
	<-u.disk.slots
	if u.onClose != nil {
		u.onClose()
	}
	return errors.Join(closeErr, removeErr, syncErr)
}
func (u *diskUpload) Abort() error { u.mu.Lock(); defer u.mu.Unlock(); return u.cleanup() }
func (u *diskUpload) Finalize(ctx context.Context, expected Info) (result Info, resultErr error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return Info{}, ErrClosed
	}
	defer func() { resultErr = errors.Join(resultErr, u.cleanup()) }()
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := u.ctx.Err(); err != nil {
		return Info{}, err
	}
	// Finalization may use a different deadline from staging. Cancellation of
	// either context must interrupt duplicate-object integrity verification.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(u.ctx, cancel)
	defer cancel()
	defer stop()
	if !expected.Digest.Valid() {
		return Info{}, ErrInvalidDigest
	}
	actual := Info{Digest: Digest(hex.EncodeToString(u.digest.Sum(nil))), Size: u.size}
	if expected != actual {
		return Info{}, ErrIntegrity
	}
	if err := u.file.Chmod(0400); err != nil {
		return Info{}, err
	}
	if err := u.file.Sync(); err != nil {
		return Info{}, err
	}
	if err := u.disk.safeDir("objects"); err != nil {
		return Info{}, err
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := u.ctx.Err(); err != nil {
		return Info{}, err
	}
	name := filepath.Join("objects", string(actual.Digest))
	// Hard-link creation publishes atomically without replacing an existing name.
	if err := u.disk.root.Link(u.name, name); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return Info{}, err
		}
		f, err := u.disk.open(actual.Digest)
		if err != nil {
			return Info{}, err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.CopyBuffer(h, contextReader{ctx, f}, u.buffer)
		if err != nil {
			return Info{}, err
		}
		if n != actual.Size || hex.EncodeToString(h.Sum(nil)) != string(actual.Digest) {
			return Info{}, ErrIntegrity
		}
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := u.ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := u.disk.syncDir("objects"); err != nil {
		return Info{}, err
	}
	return actual, nil
}
func (d *Disk) open(digest Digest) (*os.File, error) {
	if !digest.Valid() {
		return nil, ErrInvalidDigest
	}
	if err := d.safeDir("objects"); err != nil {
		return nil, err
	}
	name := filepath.Join("objects", string(digest))
	info, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrUnsafePath
	}
	return d.root.Open(name)
}
func (d *Disk) Stat(ctx context.Context, digest Digest) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	f, err := d.open(digest)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	return Info{digest, info.Size()}, nil
}
func (d *Disk) OpenRange(ctx context.Context, digest Digest, offset, length int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < -1 {
		return nil, ErrRange
	}
	f, err := d.open(digest)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if offset > info.Size() || (length >= 0 && length > info.Size()-offset) {
		f.Close()
		return nil, ErrRange
	}
	if length == -1 {
		length = info.Size() - offset
	}
	return &rangeReader{contextReader{ctx, io.NewSectionReader(f, offset, length)}, f}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type rangeReader struct {
	contextReader
	file *os.File
}

func (r *rangeReader) Close() error { return r.file.Close() }

var _ Store = (*Disk)(nil)
