// Package cargo implements authenticated local Cargo sparse registries.
// Git transport, remote hydration, owner management and yank mutations are not
// advertised until their shared lifecycle primitives are available.
package cargo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Registry struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	publicURL string
	transfers chan struct{}
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Registry, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Cargo registry configuration")
	}
	return &Registry{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 4)}, nil
}

type authResponse struct {
	header http.Header
	status int
}

func (w *authResponse) Header() http.Header         { return w.header }
func (w *authResponse) WriteHeader(n int)           { w.status = n }
func (w *authResponse) Write(b []byte) (int, error) { return len(b), nil }
func failure(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"detail": msg}}})
}
func failService(w http.ResponseWriter, e error) {
	status := 503
	switch {
	case errors.Is(e, artifact.ErrDenied):
		status = 403
	case errors.Is(e, artifact.ErrNotFound), errors.Is(e, blob.ErrNotFound):
		status = 404
	case errors.Is(e, artifact.ErrConflict):
		status = 409
	case errors.Is(e, artifact.ErrInput), errors.Is(e, errInvalid), errors.Is(e, blob.ErrIntegrity):
		status = 400
	}
	failure(w, status, http.StatusText(status))
}
func (s *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r.Body = &progressBody{ReadCloser: r.Body, controller: controller}
	w = &progressWriter{ResponseWriter: w, controller: controller}
	req := r.Clone(r.Context())
	if values := req.Header.Values("Authorization"); len(values) == 1 && !strings.ContainsAny(values[0], " \t\r\n") {
		req.Header.Set("Authorization", "Bearer "+values[0])
	}
	capture := &authResponse{header: make(http.Header)}
	allowed := false
	s.auth.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { allowed = true; s.serve(w, r) })).ServeHTTP(capture, req)
	if !allowed {
		status := capture.status
		if status == 0 {
			status = 401
		}
		if status == 401 && r.Header.Get("Authorization") != "" {
			status = 403
		}
		for _, h := range []string{"Retry-After", "X-Request-ID"} {
			if v := capture.header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		if status == 401 {
			w.Header().Set("WWW-Authenticate", `Cargo`)
		}
		failure(w, status, "Cargo authentication required")
	}
}
func (s *Registry) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "feeds" || parts[2] != "cargo" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		failure(w, 404, "route not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='cargo'`, p.InstanceID, parts[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		failure(w, 404, "feed not found")
		return
	}
	if e != nil {
		failService(w, e)
		return
	}
	route := strings.Join(parts[3:], "/")
	if route == "config.json" && (r.Method == "GET" || r.Method == "HEAD") {
		policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
		if e != nil {
			failService(w, e)
			return
		}
		if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.ArtifactResource}, identity.Read) {
			failure(w, 404, "feed not found")
			return
		}
		if s.publicURL == "" {
			failService(w, artifact.ErrUnavailable)
			return
		}
		base := s.publicURL + "/feeds/" + parts[1] + "/cargo"
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-cache")
		if r.Method != "HEAD" {
			_ = json.NewEncoder(w).Encode(map[string]any{"dl": base + "/api/v1/crates", "api": base, "auth-required": true})
		}
		return
	}
	if route == "api/v1/crates/new" && r.Method == "PUT" {
		s.publish(w, r, p, feed)
		return
	}
	if strings.HasPrefix(route, "api/v1/crates/") {
		a := strings.Split(strings.TrimPrefix(route, "api/v1/crates/"), "/")
		if len(a) == 3 && remoteNameRE.MatchString(a[0]) && validVersion(a[1]) {
			pub := artifact.Publication{FeedID: feed, Coordinate: strings.ToLower(a[0]), Version: versionKey(a[1]), AssetKey: "crate"}
			if a[2] == "download" && (r.Method == "GET" || r.Method == "HEAD") {
				s.download(w, r, p, pub, a[0], a[1])
				return
			}
			if (a[2] == "yank" && r.Method == "DELETE") || (a[2] == "unyank" && r.Method == "PUT") {
				if _, e = s.service.Prepare(r.Context(), p, feed, "", a[0], "index"); e != nil {
					failService(w, e)
					return
				}
				failure(w, 403, "registry policy disables yank and unyank")
				return
			}
		}
		failure(w, 405, "Cargo operation unsupported")
		return
	}
	if r.Method == "GET" || r.Method == "HEAD" {
		name := parts[len(parts)-1]
		if remoteNameRE.MatchString(name) && indexPath(name) == route {
			s.index(w, r, p, feed, strings.ToLower(name))
			return
		}
	}
	failure(w, 404, "route not found")
}
func (s *Registry) publish(w http.ResponseWriter, r *http.Request, p identity.Principal, feed string) {
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, 503, "upload capacity exhausted")
		return
	}
	m, e := readMetadata(r.Body)
	if e != nil {
		failService(w, e)
		return
	}
	source, e := s.service.Prepare(r.Context(), p, feed, "", m.Name, "crate")
	if e != nil {
		failService(w, e)
		return
	}
	indexSource, e := s.service.Prepare(r.Context(), p, feed, "", m.Name, "index")
	if e != nil {
		failService(w, e)
		return
	}
	if source != indexSource {
		failService(w, artifact.ErrConflict)
		return
	}
	f, info, e := readCrateContext(r.Context(), r.Body, m)
	if e != nil {
		failService(w, e)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	data := m.index(info)
	pubs := []artifact.Publication{{ExpectedSource: source, FeedID: feed, Coordinate: m.Name, Version: versionKey(m.Version), AssetKey: "crate", MediaType: "application/octet-stream", Info: info}, {ExpectedSource: source, FeedID: feed, Coordinate: m.Name, Version: versionKey(m.Version), AssetKey: "index", MediaType: "application/json", Info: bytesInfo(data)}}
	_, e = s.service.PublishBundle(r.Context(), p, pubs, []io.Reader{f, bytes.NewReader(data)})
	if e != nil {
		failService(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"warnings":{"invalid_categories":[],"invalid_badges":[],"other":[]}}`)
}
func (s *Registry) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, name, version string) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if errors.Is(e, artifact.ErrNotFound) {
		retry, served, pe := s.pullThrough(w, r, p, pub, name, version)
		switch {
		case served:
			return
		case pe != nil:
			if !errors.Is(pe, artifact.ErrNotFound) {
				failService(w, pe)
				return
			}
		case retry:
			record, body, e = s.service.Open(r.Context(), p, pub, 0, -1)
		}
	}
	if e != nil {
		failService(w, e)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}
func (s *Registry) index(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name string) {
	// Remote-owned (or unowned) crates are answered from upstream; a local
	// owner, or an upstream outage, falls through to the local composition.
	served, remoteErr := s.serveRemoteIndex(w, r, p, feed, name)
	if served {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if remoteErr != nil {
		w.Header().Set("X-Kuvryn-Cache", "offline")
	}
	// Only enumerate the selected package owner. Open re-authorizes every asset;
	// source equality below prevents an ownership change from mixing generations.
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='index' ORDER BY v.version LIMIT 10001`, p.InstanceID, feed, name)
	if e != nil {
		failService(w, e)
		return
	}
	type entry struct{ source, version string }
	entries := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.source, &v.version); e != nil {
			break
		}
		entries = append(entries, v)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		failService(w, e)
		return
	}
	if len(entries) == 0 {
		if remoteErr != nil {
			w.Header().Del("X-Kuvryn-Cache")
			failure(w, 503, "upstream unavailable")
			return
		}
		failure(w, 404, "crate not found")
		return
	}
	if len(entries) > 10000 {
		failure(w, 503, "crate index exceeds configured limit")
		return
	}
	var result bytes.Buffer
	for _, v := range entries {
		record, body, e := s.service.Open(ctx, p, artifact.Publication{FeedID: feed, Coordinate: name, Version: v.version, AssetKey: "index"}, 0, -1)
		if e != nil {
			failService(w, e)
			return
		}
		if record.SourceID != v.source || record.Info.Size > maxMetadata || result.Len()+int(record.Info.Size) > 16<<20 {
			body.Close()
			failure(w, 503, "crate index changed or exceeds configured limit")
			return
		}
		_, e = io.Copy(&result, io.LimitReader(body, maxMetadata+1))
		body.Close()
		if e != nil {
			failService(w, e)
			return
		}
	}
	etag := `"` + string(bytesInfo(result.Bytes()).Digest) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(result.Len()))
	if r.Method != "HEAD" {
		_, _ = w.Write(result.Bytes())
	}
}

// Refresh deadlines on actual transfer progress, including chunked uploads.
type progressBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (p *progressBody) Read(b []byte) (int, error) {
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	_ = p.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	return p.ReadCloser.Read(b)
}

type progressWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
}

func (p *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		_ = p.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
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
func (p *progressWriter) WriteHeader(n int) {
	_ = p.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	p.ResponseWriter.WriteHeader(n)
}

// SetPublicURL sets the trusted external origin before serving requests.
// Forwarded headers and request Host never determine credential-bearing URLs.
func (s *Registry) SetPublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return errors.New("invalid Cargo public URL")
	}
	if u.Scheme != "https" {
		ip, e := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || e != nil || !ip.IsLoopback() {
			return errors.New("invalid Cargo public URL")
		}
	}
	s.publicURL = strings.TrimSuffix(u.String(), "/")
	return nil
}
