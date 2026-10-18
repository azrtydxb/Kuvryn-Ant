// Package npmregistry implements the bounded local subset of the F14 contract.
package npmregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
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
	transfers chan struct{}
	publicURL string
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Registry, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid npm registry configuration")
	}
	return &Registry{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 2)}, nil
}

// SetPublicURL configures the trusted external origin before serving requests.
// It must not be called concurrently with ServeHTTP.
func (s *Registry) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 2048 || u.Hostname() == "" || strings.Contains(raw, "#") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return errors.New("public URL must be an origin")
	}
	if u.Scheme != "https" {
		ip, e := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || e != nil || !ip.IsLoopback() {
			return errors.New("public URL requires HTTPS or literal loopback HTTP")
		}
	}
	if strings.ContainsAny(u.Host, "\\\r\n") {
		return errors.New("invalid public origin")
	}
	s.publicURL = u.String()
	return nil
}
func failure(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(status), "reason": msg})
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
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r.Body = &progressBody{ReadCloser: r.Body, controller: c}
	w = &progressWriter{ResponseWriter: w, controller: c}
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Authorization")
	// Require explicit npm Bearer credentials, even if browser middleware supplied a principal.
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="npm"`)
		failure(w, 401, "npm bearer token required")
		return
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}
func splitRoute(route string) (name, tail string) {
	parts := strings.Split(route, "/")
	n := 1
	if strings.HasPrefix(route, "@") {
		n = 2
	}
	if len(parts) < n {
		return "", ""
	}
	name = strings.Join(parts[:n], "/")
	if !validName(name) {
		return "", ""
	}
	return name, strings.Join(parts[n:], "/")
}
func (s *Registry) serve(w http.ResponseWriter, r *http.Request) {
	if s.publicURL == "" {
		failure(w, 503, "public origin not configured")
		return
	}
	p, _ := identity.PrincipalFromContext(r.Context())
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 4)
	if len(parts) != 4 || parts[0] != "feeds" || parts[2] != "npm" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") {
		failure(w, 404, "route not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='npm'`, p.InstanceID, parts[1]).Scan(&feed)
	if errors.Is(e, pgx.ErrNoRows) {
		failure(w, 404, "feed not found")
		return
	}
	if e != nil {
		failService(w, e)
		return
	}
	route := parts[3]
	tags := strings.HasPrefix(route, "-/package/")
	if tags {
		route = strings.TrimPrefix(route, "-/package/")
	}
	name, tail := splitRoute(route)
	if name == "" {
		failure(w, 404, "route not found")
		return
	}
	if tags && tail != "dist-tags" && !strings.HasPrefix(tail, "dist-tags/") {
		failure(w, 404, "route not found")
		return
	}
	if r.Method == "DELETE" || (tags && r.Method != "GET" && r.Method != "HEAD") {
		failure(w, 501, "mutable dist-tags and withdrawals require shared CAS lifecycle support")
		return
	}
	if !tags && tail == "" && r.Method == "PUT" {
		s.publish(w, r, p, feed, name)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		failure(w, 405, "unsupported npm operation")
		return
	}
	if strings.HasPrefix(tail, "-/") {
		file := strings.TrimPrefix(tail, "-/")
		prefix := pathBase(name) + "-"
		if !strings.HasPrefix(file, prefix) || !strings.HasSuffix(file, ".tgz") {
			failure(w, 404, "tarball not found")
			return
		}
		v := strings.TrimSuffix(strings.TrimPrefix(file, prefix), ".tgz")
		if !validVersion(v) {
			failure(w, 404, "tarball not found")
			return
		}
		s.download(w, r, p, artifact.Publication{FeedID: feed, Namespace: namespace(name), Coordinate: name, Version: v, AssetKey: "tarball"})
		return
	}
	s.packument(w, r, p, feed, name, parts[1], tail, tags)
}
func pathBase(s string) string { a := strings.Split(s, "/"); return a[len(a)-1] }
func (s *Registry) publish(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name string) {
	source, e := s.service.Prepare(r.Context(), p, feed, namespace(name), name, "metadata")
	if e != nil {
		failService(w, e)
		return
	}
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		failure(w, 503, "publish capacity exhausted")
		return
	}
	if r.ContentLength > maxPublish {
		failure(w, 413, "npm publish JSON exceeds 64 MiB")
		return
	}
	parsed, e := parsePublish(r.Context(), http.MaxBytesReader(w, r.Body, maxPublish), name)
	if e != nil {
		failService(w, e)
		return
	}
	pubs := []artifact.Publication{{FeedID: feed, Namespace: namespace(name), Coordinate: name, Version: parsed.version, AssetKey: "tarball", MediaType: "application/octet-stream", Info: info(parsed.tarball), ExpectedSource: source}, {FeedID: feed, Namespace: namespace(name), Coordinate: name, Version: parsed.version, AssetKey: "metadata", MediaType: "application/json", Info: info(parsed.metadata), ExpectedSource: source, Tag: parsed.tag, References: []artifact.Reference{{Version: parsed.version, AssetKey: "tarball", Info: info(parsed.tarball)}}}}
	_, e = s.service.PublishBundle(r.Context(), p, pubs, []io.Reader{bytes.NewReader(parsed.tarball), bytes.NewReader(parsed.metadata)})
	if e != nil {
		failService(w, e)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(201)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": name})
}
func (s *Registry) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if e != nil {
		failService(w, e)
		return
	}
	defer body.Close()
	if source := r.URL.Query().Get("source"); source != "" && source != record.SourceID {
		failure(w, 409, "package owner changed")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, body, make([]byte, 64<<10))
	}
}
func (s *Registry) packument(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, name, slug, tail string, tagsOnly bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version,COALESCE((SELECT json_object_agg(t.name,v.version) FROM tags t WHERE t.component_id=o.id AND t.version_id=v.id),'{}'::json) FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key='metadata' ORDER BY v.version LIMIT 10001`, p.InstanceID, feed, name)
	if e != nil {
		failService(w, e)
		return
	}
	type entry struct {
		source, version string
		tags            map[string]string
	}
	entries := []entry{}
	for rows.Next() {
		var ent entry
		if e = rows.Scan(&ent.source, &ent.version, &ent.tags); e != nil {
			break
		}
		entries = append(entries, ent)
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
		failure(w, 404, "package not found")
		return
	}
	if _, e = s.service.Lookup(ctx, p, artifact.Publication{FeedID: feed, Namespace: namespace(name), Coordinate: name, Version: entries[0].version, AssetKey: "metadata"}); e != nil {
		failService(w, e)
		return
	}
	if len(entries) > 10000 {
		failure(w, 503, "packument too large")
		return
	}
	versions := map[string]any{}
	tags := map[string]string{}
	total := 0
	for _, ent := range entries {
		pub := artifact.Publication{FeedID: feed, Namespace: namespace(name), Coordinate: name, Version: ent.version, AssetKey: "metadata"}
		record, body, e := s.service.Open(ctx, p, pub, 0, -1)
		if e != nil {
			failService(w, e)
			return
		}
		if record.SourceID != ent.source || record.Info.Size > maxMetadata || record.Info.Size > int64((16<<20)-total) {
			body.Close()
			failure(w, 503, "package owner changed or metadata too large")
			return
		}
		data, e := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: body}, maxMetadata+1))
		body.Close()
		total += len(data)
		if e != nil || len(data) > maxMetadata || total > 16<<20 {
			failure(w, 503, "packument unavailable")
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			failure(w, 503, "invalid stored metadata")
			return
		}
		dist, ok := m["dist"].(map[string]any)
		if !ok {
			failure(w, 503, "invalid stored metadata")
			return
		}
		dist["tarball"] = s.publicURL + "/feeds/" + url.PathEscape(slug) + "/npm/" + name + "/-/" + pathBase(name) + "-" + ent.version + ".tgz?source=" + url.QueryEscape(ent.source)
		versions[ent.version] = m
		for tag, v := range ent.tags {
			tags[tag] = v
		}
	}
	var result any = map[string]any{"name": name, "_id": name, "versions": versions, "dist-tags": tags}
	if tagsOnly {
		if tail != "dist-tags" {
			failure(w, 404, "route not found")
			return
		}
		result = tags
	} else if tail != "" {
		v := tail
		if mapped, ok := tags[v]; ok {
			v = mapped
		}
		var ok bool
		result, ok = versions[v]
		if !ok {
			failure(w, 404, "version not found")
			return
		}
	}
	data, _ := json.Marshal(result)
	if len(data) > 16<<20 {
		failure(w, 503, "packument too large")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	etag := `"` + string(info(data).Digest) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	if r.Method != "HEAD" {
		_, _ = w.Write(data)
	}
}
