package aptrepo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
)

// SetPublicURL records the trusted external origin. Host and forwarding headers
// never select URLs; the value is currently advisory (indexes use relative paths).
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid APT public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("APT public URL requires HTTPS or literal loopback")
	}
	s.base.Store(raw)
	return nil
}

// SetKeyring supplies the externally loaded key ring that protects per-feed
// signing keys. Without it, publish and delete fail closed with 503.
func (s *Repository) SetKeyring(k *secrets.Keyring) { s.keys.Store(k) }

func failure(w http.ResponseWriter, e error) {
	status := 503
	switch {
	case errors.Is(e, artifact.ErrDenied):
		status = 403
	case errors.Is(e, artifact.ErrNotFound), errors.Is(e, blob.ErrNotFound):
		status = 404
	case errors.Is(e, artifact.ErrConflict):
		status = 409
	case errors.Is(e, errInvalid), errors.Is(e, artifact.ErrInput):
		status = 400
	case errors.Is(e, blob.ErrIntegrity), errors.Is(e, errCorrupt):
		status = 502
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": strconv.Itoa(status), "message": http.StatusText(status)}})
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
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
