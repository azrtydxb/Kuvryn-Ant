// Package nuget serves authenticated local NuGet V3 resources and V2 push.
package nuget

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
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid NuGet configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4)}, nil
}

// SetPublicURL configures the trusted external origin before serving.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid NuGet public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("NuGet public URL requires HTTPS or literal loopback")
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
	w.Header().Set("Vary", "Authorization, X-NuGet-ApiKey")
	// Explicitly replace any browser principal. Only machine credentials enter.
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	if r.Method == "PUT" {
		keys := r.Header.Values("X-NuGet-ApiKey")
		if len(keys) != 1 || keys[0] == "" || len(keys[0]) > 1000 || strings.ContainsAny(keys[0], " \t\r\n") {
			w.Header().Set("WWW-Authenticate", `Basic realm="NuGet"`)
			http.Error(w, "API key required", 401)
			return
		}
		r.Header.Set("Authorization", "Bearer "+keys[0])
	} else if len(r.Header.Values("Authorization")) == 1 {
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(&challengeWriter{w}, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "nuget" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='nuget'`, p.InstanceID, a[1]).Scan(&feed)
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
	if (route == "v2/package" || route == "v2/package/") && r.Method == "PUT" {
		s.push(w, r, p, feed)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method unsupported", 405)
		return
	}
	base += "/feeds/" + url.PathEscape(a[1]) + "/nuget/"
	if route == "v3/index.json" {
		policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if e != nil {
			failure(w, e)
			return
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
			failure(w, artifact.ErrNotFound)
			return
		}
		resources := []map[string]string{{"@id": base + "v3/flatcontainer/", "@type": "PackageBaseAddress/3.0.0"}, {"@id": base + "v3/registration/", "@type": "RegistrationsBaseUrl/3.6.0"}, {"@id": base + "v2/package", "@type": "PackagePublish/2.0.0"}}
		writeJSON(w, r, map[string]any{"version": "3.0.0", "resources": resources})
		return
	}
	if len(a) < 7 || a[3] != "v3" || !idPattern.MatchString(a[5]) {
		failure(w, artifact.ErrNotFound)
		return
	}
	id := strings.ToLower(a[5])
	tail := a[6:]
	if a[4] == "flatcontainer" {
		if len(tail) == 1 && tail[0] == "index.json" {
			s.index(w, r, p, feed, id, base, false)
			return
		}
		if len(tail) == 2 {
			v, e := normalizeVersion(tail[0])
			if e == nil {
				asset := ""
				switch strings.ToLower(tail[1]) {
				case id + "." + v + ".nupkg":
					asset = "nupkg"
				case id + ".nuspec":
					asset = "nuspec"
				}
				if asset != "" {
					s.download(w, r, p, artifact.Publication{FeedID: feed, Coordinate: id, Version: v, AssetKey: asset})
					return
				}
			}
		}
	}
	if a[4] == "registration" && len(tail) == 1 {
		if tail[0] == "index.json" {
			s.index(w, r, p, feed, id, base, true)
			return
		}
		if strings.HasSuffix(tail[0], ".json") {
			v, e := normalizeVersion(strings.TrimSuffix(tail[0], ".json"))
			if e == nil {
				s.leaf(w, r, p, feed, id, v, base)
				return
			}
		}
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
func (s *Repository) push(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, artifact.ErrUnavailable)
		return
	}
	// The ID lives inside ZIP metadata; spool to a bounded private temporary file
	// before resolving package-specific write policy, never to permanent storage.
	mr, e := r.MultipartReader()
	if e != nil {
		failure(w, errInvalid)
		return
	}
	part, e := mr.NextPart()
	if e != nil {
		failure(w, errInvalid)
		return
	}
	f, e := os.CreateTemp("", "ant-nuget-*")
	if e != nil {
		failure(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(contextReader{r.Context(), part}, maxPackage+1), make([]byte, 64<<10))
	if e != nil || n > maxPackage {
		failure(w, errInvalid)
		return
	}
	m, manifest, e := inspectPackage(r.Context(), f, n)
	if e != nil {
		failure(w, e)
		return
	}
	data, _ := json.Marshal(m)
	keys := []string{"nupkg", "nuspec", "metadata"}
	source := ""
	pubs := make([]artifact.Publication, 3)
	for i, key := range keys {
		selected, e := s.service.Prepare(r.Context(), p, feed, "", m.ID, key)
		if e != nil {
			failure(w, e)
			return
		}
		if source != "" && source != selected {
			failure(w, artifact.ErrConflict)
			return
		}
		source = selected
		pubs[i] = artifact.Publication{ExpectedSource: source, FeedID: feed, Coordinate: m.ID, Version: m.Version, AssetKey: key}
	}
	pubs[0].Info = blob.Info{Size: n, Digest: blob.Digest(hex.EncodeToString(h.Sum(nil)))}
	pubs[0].MediaType = "application/octet-stream"
	pubs[1].Info = info(manifest)
	pubs[1].MediaType = "application/xml"
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
}
func (s *Repository) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
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
		w.Header().Set("WWW-Authenticate", `Basic realm="NuGet"`)
	}
	w.ResponseWriter.WriteHeader(status)
}
