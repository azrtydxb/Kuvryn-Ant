package terraform

import (
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
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// SetPublicURL configures the trusted external origin before serving. Host and
// forwarding headers never select the URLs handed to Terraform clients.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid Terraform public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("Terraform public URL requires HTTPS or literal loopback")
	}
	s.base.Store(raw)
	return nil
}

// feedURL returns the externally visible mount of one feed, or "" when unset.
func (s *Repository) feedURL(slug string) string {
	b, _ := s.base.Load().(string)
	if b == "" {
		return ""
	}
	return b + "/feeds/" + slug + "/terraform"
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
	case errors.Is(e, errInvalid), errors.Is(e, artifact.ErrInput):
		status = 400
	case errors.Is(e, blob.ErrIntegrity):
		// Stored bytes disagree with their recorded identity: a server-side
		// integrity failure, never served to the client.
		status = 502
	}
	http.Error(w, http.StatusText(status), status)
}

// ServeHTTP serves the feed-scoped registry mounted at /feeds/{slug}/terraform/.
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(r.Context())
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	if slug, ok := discoveryPath(r.URL.Path); ok {
		// Terraform sends no credentials to service discovery. The document
		// holds only this deployment's trusted URLs, no feed data.
		s.discovery(w, r, slug)
		return
	}
	if s.serveSigned(w, r) {
		return
	}
	// Explicitly replace any browser principal. Only machine credentials enter.
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	if len(r.Header.Values("Authorization")) == 1 {
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(&challengeWriter{w}, r)
}

func discoveryPath(p string) (string, bool) {
	a := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(a) == 5 && a[0] == "feeds" && a[2] == "terraform" && a[3] == ".well-known" && a[4] == "terraform.json" && slugPattern.MatchString(a[1]) {
		return a[1], true
	}
	return "", false
}

// discovery serves the Terraform remote service discovery document.
func (s *Repository) discovery(w http.ResponseWriter, r *http.Request, slug string) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(405)
		return
	}
	base := s.feedURL(slug)
	if base == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	writeJSON(w, r, map[string]string{"modules.v1": base + "/v1/modules/", "providers.v1": base + "/v1/providers/"})
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
		w.Header().Set("WWW-Authenticate", `Bearer realm="terraform"`)
	}
	w.ResponseWriter.WriteHeader(status)
}
