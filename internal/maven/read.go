package maven

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (r *Repository) get(w http.ResponseWriter, req *http.Request, p identity.Principal, pub artifact.Publication) {
	rec, err := r.service.Lookup(req.Context(), p, pub)
	if err != nil {
		fail(w, err)
		return
	}
	content := &rangeReader{size: rec.Info.Size, open: func(offset int64) (io.ReadCloser, error) {
		current, reader, err := r.service.Open(req.Context(), p, pub, offset, -1)
		if err != nil {
			return nil, err
		}
		if current.ID != rec.ID || current.Info != rec.Info || current.MediaType != rec.MediaType {
			reader.Close()
			return nil, artifact.ErrConflict
		}
		return reader, nil
	}}
	content.reader, err = content.open(0)
	if err != nil {
		fail(w, err)
		return
	}
	defer content.Close()
	w.Header().Set("ETag", etag(rec))
	w.Header().Set("X-Checksum-Sha256", string(rec.Info.Digest))
	w.Header().Set("Content-Type", rec.MediaType)
	w.Header().Set("Content-Disposition", "attachment")
	// RFC 9110 precondition order, strong If-Match, weak If-None-Match,
	// If-Range, suffix/open/multipart ranges and HEAD are handled by net/http.
	if req.Method == http.MethodHead {
		req = req.Clone(req.Context())
		req.Header.Del("Range")
		req.Header.Del("If-Range")
	}
	http.ServeContent(w, req, "", time.Time{}, content)
}

type rangeReader struct {
	size, pos int64
	reader    io.ReadCloser
	open      func(int64) (io.ReadCloser, error)
}

func (r *rangeReader) Close() error {
	if r.reader != nil {
		err := r.reader.Close()
		r.reader = nil
		return err
	}
	return nil
}
func (r *rangeReader) Seek(offset int64, whence int) (int64, error) {
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.pos
	case io.SeekEnd:
		base = r.size
	default:
		return 0, errors.New("invalid seek")
	}
	if offset < -base || offset > r.size-base {
		return 0, errors.New("seek outside object")
	}
	if base+offset != r.pos {
		r.Close()
		r.pos = base + offset
	}
	return r.pos, nil
}
func (r *rangeReader) Read(p []byte) (int, error) {
	if r.pos == r.size {
		return 0, io.EOF
	}
	if r.reader == nil {
		var err error
		r.reader, err = r.open(r.pos)
		if err != nil {
			return 0, err
		}
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	n, err := r.reader.Read(p)
	r.pos += int64(n)
	return n, err
}
