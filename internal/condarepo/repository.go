// Package conda serves local Conda 2 metadata and immutable ZIP distributions.
package condarepo

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
	// indexes holds the spooled digest indexes of remote repodata too large to cache.
	indexes indexSet
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Conda configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: artifact.NewUploadSlots()}, nil
}

// SetPublicURL configures the trusted external origin before serving.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid Conda public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("Conda public URL requires HTTPS or literal loopback")
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
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(&challengeWriter{w}, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "conda" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='conda'`, p.InstanceID, a[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	if len(a) == 6 && a[3] == "ingest" && validSubdir(a[4]) && validFilename(a[5]) && r.Method == "PUT" {
		s.push(w, r, p, feed, a[4], a[5])
		return
	}
	if len(a) == 6 && a[3] == "ingest" && validSubdir(a[4]) && validFilename(a[5]) && r.Method == "DELETE" {
		// Conda has no client-side delete; DELETE on the ingest resource (like
		// Quetz/anaconda.org API removal) is policy-gated by the artifact core.
		// repodata.json is generated from stored packages, so it updates at once.
		pub := publication(feed, a[4], a[5], "archive")
		if _, e := s.service.Delete(ctx, p, artifact.Target{FeedID: feed, Namespace: pub.Namespace, Coordinate: pub.Coordinate, Version: pub.Version}); e != nil {
			failure(w, e)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method unsupported", 405)
		return
	}
	if len(a) == 5 && validSubdir(a[3]) {
		if a[4] == "repodata.json" || a[4] == "current_repodata.json" {
			s.index(w, r, p, feed, a[3], a[4])
			return
		}
		if validFilename(a[4]) {
			s.download(w, r, p, publication(feed, a[3], a[4], "archive"))
			return
		}
	}
	failure(w, artifact.ErrNotFound)
}
func publication(feed, subdir, filename, key string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: subdir, Coordinate: "conda/" + subdir, Version: filename, AssetKey: key}
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
func (s *Repository) push(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, subdir, filename string) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	pubs := make([]artifact.Publication, 2)
	source := ""
	for i, key := range []string{"archive", "metadata"} {
		selected, e := s.service.Prepare(ctx, p, feed, subdir, "conda/"+subdir, key)
		if e != nil {
			failure(w, e)
			return
		}
		if source != "" && source != selected {
			failure(w, artifact.ErrConflict)
			return
		}
		source = selected
		pubs[i] = publication(feed, subdir, filename, key)
		pubs[i].ExpectedSource = source
	}
	f, e := os.CreateTemp("", "ant-conda-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{ctx, r.Body}, maxPackage+1), make([]byte, 64<<10))
	if e != nil || n > maxPackage {
		failure(w, errInvalid)
		return
	}
	if values := r.Header.Values("X-Checksum-Sha256"); len(values) > 0 {
		if len(values) != 1 || values[0] != hex.EncodeToString(h.Sum(nil)) {
			failure(w, blob.ErrIntegrity)
			return
		}
	}
	_, e = f.Seek(0, io.SeekStart)
	if e != nil {
		failure(w, e)
		return
	}
	m, e := inspectPackage(ctx, f, n)
	if e != nil {
		failure(w, e)
		return
	}
	if m.Subdir != subdir || m.Name+"-"+m.Version+"-"+m.Build+".tar.bz2" != filename {
		failure(w, errInvalid)
		return
	}
	data, e := json.Marshal(m)
	if e != nil || len(data) > maxMetadata {
		failure(w, errInvalid)
		return
	}
	pubs[0].Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(h.Sum(nil)))}
	pubs[0].MediaType = "application/x-bzip2"
	pubs[1].Info = info(data)
	pubs[1].MediaType = "application/json"
	pubs[1].References = []artifact.Reference{{Version: filename, AssetKey: "archive", Info: pubs[0].Info}}
	if _, e = f.Seek(0, io.SeekStart); e != nil {
		failure(w, e)
		return
	}
	if _, e = s.service.PublishBundle(ctx, p, pubs, []io.Reader{f, bytes.NewReader(data)}); e != nil {
		failure(w, e)
		return
	}
	w.WriteHeader(201)
}
func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if errors.Is(e, artifact.ErrNotFound) {
		if retry, pe := s.pullThrough(r.Context(), p, pub); pe != nil {
			if !errors.Is(pe, artifact.ErrNotFound) {
				failure(w, pe)
				return
			}
		} else if retry {
			record, body, e = s.service.Open(r.Context(), p, pub, 0, -1)
		}
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
		w.Header().Set("WWW-Authenticate", `Basic realm="Conda"`)
	}
	w.ResponseWriter.WriteHeader(status)
}
