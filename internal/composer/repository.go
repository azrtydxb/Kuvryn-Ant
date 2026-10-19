// Package composer serves local Composer 2 metadata and immutable ZIP distributions.
package composer

import (
	"bytes"
	"context"
	"crypto/sha1"
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
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Composer configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: artifact.NewUploadSlots()}, nil
}

// SetPublicURL configures the trusted external origin before serving.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid Composer public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("Composer public URL requires HTTPS or literal loopback")
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
	if len(a) < 4 || a[0] != "feeds" || a[2] != "composer" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='composer'`, p.InstanceID, a[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	route := strings.Join(a[3:], "/")
	base, _ := s.base.Load().(string)
	if base == "" {
		failure(w, artifact.ErrUnavailable)
		return
	}
	if len(a) == 7 && a[3] == "ingest" && namePattern.MatchString(a[4]+"/"+a[5]) && versionPattern.MatchString(a[6]) && r.Method == "PUT" {
		s.push(w, r, p, feed, a[4]+"/"+a[5], a[6])
		return
	}
	if len(a) == 7 && a[3] == "ingest" && namePattern.MatchString(a[4]+"/"+a[5]) && versionPattern.MatchString(a[6]) && r.Method == "DELETE" {
		// Composer has no native delete; DELETE on the ingest resource is the
		// API method, policy-gated by the shared artifact core.
		if _, e := s.service.Delete(ctx, p, artifact.Target{FeedID: feed, Namespace: a[4], Coordinate: a[4] + "/" + a[5], Version: a[6]}); e != nil {
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
	base += "/feeds/" + url.PathEscape(a[1]) + "/composer/"
	if route == "packages.json" {
		policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if e != nil {
			failure(w, e)
			return
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
			failure(w, artifact.ErrNotFound)
			return
		}
		writeJSON(w, r, map[string]any{"packages": map[string]any{}, "metadata-url": base + "p2/%package%.json"})
		return
	}
	if len(a) == 6 && a[3] == "p2" && strings.HasSuffix(a[5], ".json") {
		id := a[4] + "/" + strings.TrimSuffix(a[5], ".json")
		dev := strings.HasSuffix(id, "~dev")
		id = strings.TrimSuffix(id, "~dev")
		if namePattern.MatchString(id) {
			s.index(w, r, p, feed, id, base, dev)
			return
		}
	}
	if len(a) == 8 && a[3] == "dist" && namePattern.MatchString(a[4]+"/"+a[5]) && remoteVersionRE.MatchString(a[6]) && a[7] == "archive.zip" {
		s.download(w, r, p, artifact.Publication{FeedID: feed, Namespace: a[4], Coordinate: a[4] + "/" + a[5], Version: a[6], AssetKey: "dist.zip"})
		return
	}
	failure(w, artifact.ErrNotFound)
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
func (s *Repository) push(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, id, version string) {
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
	for i, key := range []string{"dist.zip", "metadata"} {
		selected, e := s.service.Prepare(ctx, p, feed, strings.Split(id, "/")[0], id, key)
		if e != nil {
			failure(w, e)
			return
		}
		if source != "" && source != selected {
			failure(w, artifact.ErrConflict)
			return
		}
		source = selected
		pubs[i] = artifact.Publication{ExpectedSource: source, FeedID: feed, Namespace: strings.Split(id, "/")[0], Coordinate: id, Version: version, AssetKey: key}
	}
	f, e := os.CreateTemp("", "ant-composer-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	legacy := sha1.New() // Composer dist.shasum is SHA-1; storage identity remains SHA-256.
	n, e := io.CopyBuffer(io.MultiWriter(f, h, legacy), io.LimitReader(contextReader{ctx, r.Body}, maxPackage+1), make([]byte, 64<<10))
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
	m, _, e := inspectPackage(ctx, f, n)
	if e != nil {
		failure(w, e)
		return
	}
	if m.Name != id || m.Version != version {
		failure(w, errInvalid)
		return
	}
	m.ArchiveSHA1 = hex.EncodeToString(legacy.Sum(nil))
	data, e := json.Marshal(m)
	if e != nil || len(data) > maxMetadata {
		failure(w, errInvalid)
		return
	}
	pubs[0].Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(h.Sum(nil)))}
	pubs[0].MediaType = "application/zip"
	pubs[1].Info = info(data)
	pubs[1].MediaType = "application/json"
	pubs[1].References = []artifact.Reference{{Version: version, AssetKey: "dist.zip", Info: pubs[0].Info}}
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
		// Local miss: fetch, verify and cache from the remote owner (a local
		// owner or an unknown version stays a 404).
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
		w.Header().Set("WWW-Authenticate", `Basic realm="Composer"`)
	}
	w.ResponseWriter.WriteHeader(status)
}
