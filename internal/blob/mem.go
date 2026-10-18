package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"sort"
	"sync"
	"time"
)

// Mem is an in-memory Store for tests and local development. It stands in for a
// remote object store (for example S3) where a real endpoint is unavailable. It
// verifies content like the real stores do and supports failure injection.
type Mem struct {
	mu      sync.Mutex
	objects map[Digest]memObject
	// FailFinalize, when set, is consulted before an object is published.
	FailFinalize func(Digest) error
	// FailRead, when set, is consulted before an object is opened.
	FailRead func(Digest) error
}

type memObject struct {
	data []byte
	mod  time.Time
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem { return &Mem{objects: map[Digest]memObject{}} }

// Len reports the number of published objects.
func (m *Mem) Len() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.objects) }

// Put publishes data directly and returns its identity.
func (m *Mem) Put(data []byte) Info {
	sum := sha256.Sum256(data)
	i := Info{Digest(hex.EncodeToString(sum[:])), int64(len(data))}
	m.mu.Lock()
	m.objects[i.Digest] = memObject{bytes.Clone(data), time.Now()}
	m.mu.Unlock()
	return i
}

// Corrupt replaces stored bytes without changing the key (integrity tests).
func (m *Mem) Corrupt(d Digest, data []byte) {
	m.mu.Lock()
	m.objects[d] = memObject{bytes.Clone(data), time.Now()}
	m.mu.Unlock()
}

func (m *Mem) Stage(ctx context.Context) (Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &memUpload{m: m, h: sha256.New()}, nil
}

type memUpload struct {
	m      *Mem
	buf    bytes.Buffer
	h      hash.Hash
	closed bool
}

func (u *memUpload) Write(p []byte) (int, error) {
	if u.closed {
		return 0, ErrClosed
	}
	u.h.Write(p)
	return u.buf.Write(p)
}

func (u *memUpload) Finalize(ctx context.Context, want Info) (Info, error) {
	if u.closed {
		return Info{}, ErrClosed
	}
	u.closed = true
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if !want.Digest.Valid() {
		return Info{}, ErrInvalidDigest
	}
	got := Info{Digest(hex.EncodeToString(u.h.Sum(nil))), int64(u.buf.Len())}
	if got != want {
		return Info{}, ErrIntegrity
	}
	if f := u.m.FailFinalize; f != nil {
		if err := f(got.Digest); err != nil {
			return Info{}, err
		}
	}
	u.m.mu.Lock()
	defer u.m.mu.Unlock()
	if _, ok := u.m.objects[got.Digest]; !ok {
		u.m.objects[got.Digest] = memObject{bytes.Clone(u.buf.Bytes()), time.Now()}
	}
	return got, nil
}

func (u *memUpload) Abort() error { u.closed = true; return nil }

func (m *Mem) Stat(ctx context.Context, d Digest) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if !d.Valid() {
		return Info{}, ErrInvalidDigest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[d]
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{d, int64(len(o.data))}, nil
}

func (m *Mem) OpenRange(ctx context.Context, d Digest, offset, length int64) (io.ReadCloser, error) {
	info, err := m.Stat(ctx, d)
	if err != nil {
		return nil, err
	}
	if f := m.FailRead; f != nil {
		if err = f(d); err != nil {
			return nil, err
		}
	}
	if offset < 0 || length < -1 || offset > info.Size || (length >= 0 && length > info.Size-offset) {
		return nil, ErrRange
	}
	if length == -1 {
		length = info.Size - offset
	}
	m.mu.Lock()
	data := bytes.Clone(m.objects[d].data[offset : offset+length])
	m.mu.Unlock()
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *Mem) Delete(ctx context.Context, d Digest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !d.Valid() {
		return ErrInvalidDigest
	}
	m.mu.Lock()
	delete(m.objects, d)
	m.mu.Unlock()
	return nil
}

func (m *Mem) List(ctx context.Context, after Digest, limit int) ([]Object, error) {
	if limit < 1 {
		return nil, ErrRange
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.objects))
	for d := range m.objects {
		if d > after {
			names = append(names, string(d))
		}
	}
	sort.Strings(names)
	var out []Object
	for _, n := range names {
		o := m.objects[Digest(n)]
		out = append(out, Object{Info{Digest(n), int64(len(o.data))}, o.mod})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

var (
	_ Store   = (*Mem)(nil)
	_ Deleter = (*Mem)(nil)
	_ Lister  = (*Mem)(nil)
)
