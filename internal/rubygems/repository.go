// Package rubygems implements local RubyGems publication and consumption.
package rubygems

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid RubyGems configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4)}, nil
}

// SetPublicURL configures the trusted external origin before serving.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid RubyGems public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("RubyGems public URL requires HTTPS or literal loopback")
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
	http.Error(w, http.StatusText(status), status)
}
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(r.Context())
	r.Body = &progressBody{r.Body, c}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	// Explicitly replace any browser principal. Only machine credentials enter.
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	if len(r.Header.Values("Authorization")) == 1 {
		key := r.Header.Get("Authorization")
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		} else if key != "" && len(key) < 1000 && !strings.ContainsAny(key, " \t\r\n") {
			r.Header.Set("Authorization", "Bearer "+key)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(&challengeWriter{w}, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "rubygems" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='rubygems'`, p.InstanceID, a[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	// RubyGems push appends /api/v1/gems even to a trailing-slash host.
	route := strings.TrimPrefix(strings.Join(a[3:], "/"), "/")
	if route == "api/v1/gems" && r.Method == "POST" {
		s.push(w, r, p, feed)
		return
	}
	if route == "api/v1/gems/yank" && r.Method == "DELETE" {
		s.yank(w, r, p, feed)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		http.Error(w, "unsupported operation", 405)
		return
	}
	s.read(w, r, p, feed, route)
}
func (s *Repository) push(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	f, e := os.CreateTemp("", "ant-rubygems-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{r.Context(), r.Body}, maxPackage+1), make([]byte, 64<<10))
	if e != nil || n > maxPackage {
		failure(w, errInvalid)
		return
	}
	_, e = f.Seek(0, io.SeekStart)
	if e != nil {
		failure(w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	m, e := inspectPackage(ctx, f)
	if e != nil {
		failure(w, e)
		return
	}
	m.SHA256 = hex.EncodeToString(h.Sum(nil))
	data, _ := json.Marshal(m)
	manifest := quickSpec(m)
	keys := []string{"gem", "gemspec", "metadata"}
	source := ""
	pubs := make([]artifact.Publication, 3)
	for i, key := range keys {
		selected, e := s.service.Prepare(r.Context(), p, feed, "", m.Name, key)
		if e != nil {
			failure(w, e)
			return
		}
		if source != "" && source != selected {
			failure(w, artifact.ErrConflict)
			return
		}
		source = selected
		pubs[i] = artifact.Publication{ExpectedSource: source, FeedID: feed, Coordinate: m.Name, Version: m.key(), AssetKey: key}
	}
	pubs[0].Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(h.Sum(nil)))}
	pubs[0].MediaType = "application/octet-stream"
	pubs[1].Info = info(manifest)
	pubs[1].MediaType = "application/octet-stream"
	pubs[2].Info = info(data)
	pubs[2].MediaType = "application/json"
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		failure(w, e)
		return
	}
	_, e = s.service.PublishBundle(r.Context(), p, pubs, []io.Reader{f, bytes.NewReader(manifest), bytes.NewReader(data)})
	if e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
	_, _ = w.Write([]byte("Successfully registered gem: " + m.Name + " (" + m.Version + ")\n"))
}
func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	rec, e := s.service.Lookup(r.Context(), p, pub)
	if e != nil {
		failure(w, e)
		return
	}
	etag := `"` + string(rec.Info.Digest) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag || r.Header.Get("If-None-Match") == "*" {
		w.WriteHeader(304)
		return
	}
	start, length := int64(0), rec.Info.Size
	status := 200
	if raw := r.Header.Get("Range"); raw != "" && (r.Header.Get("If-Range") == "" || r.Header.Get("If-Range") == etag) {
		var ok bool
		start, length, ok = singleRange(raw, rec.Info.Size)
		if !ok {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(rec.Info.Size, 10))
			w.WriteHeader(416)
			return
		}
		status = 206
	}
	record, body, e := s.service.Open(r.Context(), p, pub, start, length)
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	if record.SourceID != rec.SourceID || record.Info != rec.Info {
		failure(w, artifact.ErrUnavailable)
		return
	}
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if status == 206 {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(start+length-1, 10)+"/"+strconv.FormatInt(rec.Info.Size, 10))
	}
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, contextReader{r.Context(), body}, make([]byte, 64<<10))
	}
}
func singleRange(raw string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(raw, "bytes=") || strings.Contains(raw, ",") || size <= 0 {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(raw, "bytes="), "-")
	if !ok {
		return 0, 0, false
	}
	if a == "" {
		n, e := strconv.ParseInt(b, 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}
	start, e := strconv.ParseInt(a, 10, 64)
	if e != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if b != "" {
		end, e = strconv.ParseInt(b, 10, 64)
		if e != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end - start + 1, true
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
		w.Header().Set("WWW-Authenticate", `Basic realm="RubyGems"`)
	}
	w.ResponseWriter.WriteHeader(status)
}

var gemNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// yank implements `gem yank NAME -v VERSION [--platform P]` (DELETE
// /api/v1/gems/yank with form fields). A yank removes the version through the
// shared policy-gated core: delete authorization, feed/namespace delete policy,
// audit, outbox and replication tombstone. A policy that protects the version
// (immutable release, unlist-only, denied) refuses the yank; bytes are reclaimed
// later by garbage collection.
func (s *Repository) yank(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	// net/http does not parse DELETE bodies; the gem client sends a form body.
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
	form, pe := url.ParseQuery(string(raw))
	if e != nil || pe != nil {
		failure(w, errInvalid)
		return
	}
	for k, v := range r.URL.Query() {
		if _, set := form[k]; !set {
			form[k] = v
		}
	}
	name, version, platform := form.Get("gem_name"), form.Get("version"), form.Get("platform")
	if !gemNamePattern.MatchString(name) || version == "" || len(version) > 128 || len(platform) > 128 || strings.ContainsAny(version+platform, "\x00\r\n/") {
		failure(w, errInvalid)
		return
	}
	key := version
	if platform != "" && platform != "ruby" {
		key += "-" + platform
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, e := s.service.Delete(ctx, p, artifact.Target{FeedID: feed, Coordinate: name, Version: key}); e != nil {
		failure(w, e)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Successfully yanked gem: " + name + " (" + version + ")\n"))
}
