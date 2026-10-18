package ansiblerepo

import (
	"encoding/json"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid Galaxy public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("Galaxy public URL requires HTTPS or literal loopback")
	}
	s.base.Store(raw)
	return nil
}
func failure(w http.ResponseWriter, e error) {
	status := 503
	switch {
	case errors.Is(e, artifact.ErrDenied):
		status = 403
	case errors.Is(e, artifact.ErrNotFound), errors.Is(e, blob.ErrNotFound):
		status = 404
	case errors.Is(e, artifact.ErrConflict):
		status = 409
	case errors.Is(e, errInvalid), errors.Is(e, artifact.ErrInput), errors.Is(e, blob.ErrIntegrity):
		status = 400
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": strconv.Itoa(status), "message": http.StatusText(status)}})
}
func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, e := json.Marshal(v)
	if e != nil || len(b) > 16<<20 {
		failure(w, artifact.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	if r.Method != "HEAD" {
		_, _ = w.Write(b)
	}
}
func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if errors.Is(e, artifact.ErrNotFound) {
		retry, served, pe := s.pullThrough(w, r, p, pub)
		if served {
			return
		}
		if !retry {
			failure(w, pe)
			return
		}
		record, body, e = s.service.Open(r.Context(), p, pub, 0, -1)
	}
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

type progressBody struct {
	io.ReadCloser
	c *http.ResponseController
}

func (p *progressBody) Read(b []byte) (int, error) {
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	_ = p.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	return p.ReadCloser.Read(b)
}

type progressWriter struct {
	http.ResponseWriter
	c *http.ResponseController
}

func (p *progressWriter) WriteHeader(n int) {
	_ = p.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	p.ResponseWriter.WriteHeader(n)
}
func (p *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		_ = p.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		n, e := p.ResponseWriter.Write(chunk)
		total += n
		b = b[n:]
		if e != nil {
			return total, e
		}
		if n != len(chunk) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

type challengeWriter struct{ http.ResponseWriter }

func (w *challengeWriter) WriteHeader(status int) {
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ansible"`)
	}
	w.ResponseWriter.WriteHeader(status)
}
