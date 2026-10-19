package rpmrepo

import (
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SetPublicURL configures the trusted external origin before serving.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid RPM public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("RPM public URL requires HTTPS or literal loopback")
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
	case errors.Is(e, errInvalid), errors.Is(e, errMalformed), errors.Is(e, artifact.ErrInput), errors.Is(e, blob.ErrIntegrity):
		status = 400
	}
	http.Error(w, http.StatusText(status), status)
}

type progressBody struct {
	io.ReadCloser
	c *http.ResponseController
}

func (p *progressBody) Read(b []byte) (int, error) {
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	_ = p.c.SetReadDeadline(time.Now().Add(ioWindow))
	return p.ReadCloser.Read(b)
}

type progressWriter struct {
	http.ResponseWriter
	c *http.ResponseController
}

func (p *progressWriter) WriteHeader(n int) {
	_ = p.c.SetWriteDeadline(time.Now().Add(ioWindow))
	p.ResponseWriter.WriteHeader(n)
}
func (p *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		_ = p.c.SetWriteDeadline(time.Now().Add(ioWindow))
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
		w.Header().Set("WWW-Authenticate", `Basic realm="rpm"`)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (p *progressWriter) Unwrap() http.ResponseWriter  { return p.ResponseWriter }
func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// awaitUpstream lifts the per-write deadline for a response that first has to
// be fetched from the remote origin. Without it the 30 second deadline set when
// the request arrived expires while the fetch is still running, and an HTTP/2
// server then resets the stream with INTERNAL_ERROR before the first byte is
// written. Every subsequent write re-arms the short deadline.
func awaitUpstream(w http.ResponseWriter, d time.Duration) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
}

// ioWindow is how long a single read or write of a client connection may stall.
var ioWindow = 30 * time.Second
