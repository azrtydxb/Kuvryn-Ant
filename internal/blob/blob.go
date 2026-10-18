// Package blob provides content-addressed storage. Reachability, authorization,
// retention and garbage collection belong to the service layer.
package blob

import (
	"context"
	"errors"
	"io"
	"regexp"
)

var (
	ErrBackend       = errors.New("blob backend operation failed")
	ErrInvalidDigest = errors.New("invalid SHA-256 digest")
	ErrIntegrity     = errors.New("blob integrity verification failed")
	ErrNotFound      = errors.New("blob not found")
	ErrRange         = errors.New("invalid blob range")
	ErrClosed        = errors.New("upload is closed")
	ErrUnsafePath    = errors.New("unsafe blob directory or object")
)

// Digest is a lowercase SHA-256 hexadecimal content address, never a package path.
type Digest string

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (d Digest) Valid() bool { return digestPattern.MatchString(string(d)) }

type Info struct {
	Digest Digest
	Size   int64
}

// Store publishes only verified content. OpenRange uses length=-1 for the rest
// of the object. Implementations must not return bytes beyond the requested range.
// Deletion and resumable transfers are intentionally absent until service-level
// reference checks and persistent upload checkpoints are implemented.
type Store interface {
	Stage(context.Context) (Upload, error)
	Stat(context.Context, Digest) (Info, error)
	OpenRange(context.Context, Digest, int64, int64) (io.ReadCloser, error)
}

type Upload interface {
	io.Writer
	Finalize(context.Context, Info) (Info, error)
	Abort() error
}
