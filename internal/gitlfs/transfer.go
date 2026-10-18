package gitlfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, source string) {
	rec, err := s.service.Lookup(r.Context(), p, pub)
	if err != nil {
		fail(w, err)
		return
	}
	if rec.SourceID != source || rec.Info != pub.Info {
		fail(w, artifact.ErrConflict)
		return
	}
	etag := strconv.Quote(string(rec.Info.Digest))
	offset, length, partial := int64(0), rec.Info.Size, false
	if r.Method == "GET" && r.Header.Get("Range") != "" && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		offset, length, err = parseRange(r.Header.Get("Range"), rec.Info.Size)
		if err != nil {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", rec.Info.Size))
			writeError(w, 416)
			return
		}
		partial = true
	}
	var reader io.ReadCloser
	if r.Method != "HEAD" {
		var current artifact.Record
		current, reader, err = s.service.Open(r.Context(), p, pub, offset, length)
		if err != nil {
			fail(w, err)
			return
		}
		defer reader.Close()
		if current.ID != rec.ID || current.SourceID != rec.SourceID || current.Info != rec.Info {
			fail(w, artifact.ErrConflict)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	code := 200
	if partial {
		code = 206
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, rec.Info.Size))
	}
	w.WriteHeader(code)
	if reader != nil {
		_, _ = io.CopyBuffer(w, &contextReader{reader, r.Context()}, make([]byte, 64<<10))
	}
}

// Single ranges are sufficient for native LFS resume. Multipart ranges fail
// explicitly, keeping CPU, backend opens and response expansion bounded.
func parseRange(raw string, size int64) (int64, int64, error) {
	bad := artifact.ErrInput
	if !strings.HasPrefix(raw, "bytes=") || len(raw) > 128 || size <= 0 {
		return 0, 0, bad
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
	if !ok {
		return 0, 0, bad
	}
	number := func(s string) (int64, error) {
		if s == "" {
			return 0, bad
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, bad
			}
		}
		return strconv.ParseInt(s, 10, 64)
	}
	if a == "" {
		n, e := number(b)
		if e != nil || n <= 0 {
			return 0, 0, bad
		}
		if n > size {
			n = size
		}
		return size - n, n, nil
	}
	start, e := number(a)
	if e != nil || start >= size {
		return 0, 0, bad
	}
	end := size - 1
	if b != "" {
		end, e = number(b)
		if e != nil || end < start {
			return 0, 0, bad
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, nil
}

type contextReader struct {
	io.Reader
	ctx context.Context
}

func (r *contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.Reader.Read(p)
}

type progressReader struct {
	io.ReadCloser
	c   *http.ResponseController
	ctx context.Context
}

func (r *progressReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	_ = r.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	return r.ReadCloser.Read(p)
}

type progressWriter struct {
	http.ResponseWriter
	c *http.ResponseController
}

func (w *progressWriter) Write(p []byte) (int, error) {
	_ = w.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.ResponseWriter.Write(p)
}
func (w *progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
