// Package swiftrepo implements a local Swift Package Manager registry (SE-0292); see README.md.
package swiftrepo

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
		return nil, errors.New("invalid Swift registry configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: artifact.NewUploadSlots()}, nil
}

func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.ContainsAny(raw, "\r\n#") {
		return errors.New("invalid Swift public URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return errors.New("Swift public URL requires HTTPS or literal loopback")
	}
	s.base.Store(raw)
	return nil
}

type protoError struct {
	status int
	msg    string
}

func (e *protoError) Error() string     { return e.msg }
func perr(status int, msg string) error { return &protoError{status, msg} }

func failure(w http.ResponseWriter, e error) {
	status, detail := 503, "service unavailable"
	var pe *protoError
	switch {
	case errors.As(e, &pe):
		status, detail = pe.status, pe.msg
	case errors.Is(e, artifact.ErrDenied):
		status, detail = 403, "forbidden"
	case errors.Is(e, artifact.ErrNotFound), errors.Is(e, blob.ErrNotFound):
		status, detail = 404, "not found"
	case errors.Is(e, artifact.ErrConflict):
		status, detail = 409, "release already exists with different content"
	case errors.Is(e, errInvalid), errors.Is(e, artifact.ErrInput):
		status, detail = 422, "invalid package"
	case errors.Is(e, blob.ErrIntegrity):
		status, detail = 500, "stored content failed integrity verification"
	}
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Content-Language", "en")
	h.Set("Content-Version", "1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "title": http.StatusText(status), "detail": detail})
}

const mediaPrefix = "application/vnd.swift.registry.v"

// checkAccept enforces the registry version negotiation header.
func checkAccept(r *http.Request) error {
	v := strings.Join(r.Header.Values("Accept"), ",")
	if v == "" {
		return perr(400, "missing Accept header")
	}
	for _, part := range strings.Split(v, ",") {
		t := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
		if strings.HasPrefix(t, mediaPrefix+"1+") || t == mediaPrefix+"1" {
			return nil
		}
	}
	if strings.Contains(strings.ToLower(v), mediaPrefix) {
		return perr(415, "unsupported registry API version")
	}
	return perr(400, "invalid Accept header")
}

func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(120 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(120 * time.Second))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization")
	w.Header().Set("Content-Version", "1")
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="swift"`)
		failure(w, perr(401, "authentication required"))
		return
	}
	if _, password, ok := r.BasicAuth(); ok {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+password)
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 4 || a[0] != "feeds" || a[2] != "swift" || p.InstanceID != s.instance || strings.Contains(r.URL.EscapedPath(), "%") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='swift'`, s.instance, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	if r.Method == "DELETE" {
		if rest := a[3:]; len(rest) == 3 {
			q := &req{s: s, w: w, r: r, p: p, feed: feed, slug: a[1], scope: rest[0], name: rest[1]}
			if !q.validPackage() {
				failure(w, artifact.ErrNotFound)
				return
			}
			if !validVersion(rest[2]) {
				failure(w, perr(400, "invalid version"))
				return
			}
			q.version = rest[2]
			q.remove()
			return
		}
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		failure(w, perr(405, "method not allowed"))
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "PUT" {
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		failure(w, perr(405, "method not allowed"))
		return
	}
	if e = checkAccept(r); e != nil {
		failure(w, e)
		return
	}
	rest := a[3:]
	q := &req{s: s, w: w, r: r, p: p, feed: feed, slug: a[1]}
	read := r.Method != "PUT"
	switch {
	case len(rest) == 1 && rest[0] == "identifiers" && read:
		q.identifiers()
		return
	case len(rest) == 2 && read:
		q.scope, q.name = rest[0], strings.TrimSuffix(rest[1], ".json")
		if !q.validPackage() {
			failure(w, artifact.ErrNotFound)
			return
		}
		q.list()
		return
	case len(rest) == 3:
		q.scope, q.name = rest[0], rest[1]
		if !q.validPackage() {
			failure(w, artifact.ErrNotFound)
			return
		}
		if !read {
			if !validVersion(rest[2]) {
				failure(w, perr(400, "invalid version"))
				return
			}
			q.version = rest[2]
			q.publish()
			return
		}
		if v, ok := strings.CutSuffix(rest[2], ".zip"); ok && validVersion(v) {
			q.version = v
			q.archive()
			return
		}
		if !validVersion(rest[2]) {
			failure(w, artifact.ErrNotFound)
			return
		}
		q.version = rest[2]
		q.release()
		return
	case len(rest) == 4 && rest[3] == "Package.swift" && read && validVersion(rest[2]):
		q.scope, q.name, q.version = rest[0], rest[1], rest[2]
		if !q.validPackage() {
			failure(w, artifact.ErrNotFound)
			return
		}
		q.manifest()
		return
	}
	failure(w, artifact.ErrNotFound)
}

type req struct {
	s                    *Repository
	w                    http.ResponseWriter
	r                    *http.Request
	p                    identity.Principal
	feed, slug           string
	scope, name, version string
}

func (q *req) validPackage() bool { return validScope(q.scope) && validName(q.name) }
func (q *req) ns() string         { return strings.ToLower(q.scope) }
func (q *req) coord() string      { return strings.ToLower(q.scope) + "." + strings.ToLower(q.name) }
func (q *req) pub(version, key, media string) artifact.Publication {
	return artifact.Publication{FeedID: q.feed, Namespace: q.ns(), Coordinate: q.coord(), Version: version, AssetKey: key, MediaType: media}
}
func (q *req) url(parts ...string) string {
	b, _ := q.s.base.Load().(string)
	if b == "" {
		return ""
	}
	return b + "/feeds/" + q.slug + "/swift/" + strings.Join(parts, "/")
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

func readAsset(ctx context.Context, svc *artifact.Service, p identity.Principal, pub artifact.Publication, limit int64) (artifact.Record, []byte, error) {
	rec, body, e := svc.Open(ctx, p, pub, 0, -1)
	if e != nil {
		return rec, nil, e
	}
	defer body.Close()
	if rec.Info.Size > limit {
		return rec, nil, artifact.ErrUnavailable
	}
	b, e := io.ReadAll(io.LimitReader(body, limit+1))
	if e != nil || int64(len(b)) > limit {
		return rec, nil, artifact.ErrUnavailable
	}
	return rec, b, nil
}

// remove deletes one release through the policy-gated artifact core (delete
// authorization, feed/namespace delete policy, audit, outbox, replication
// tombstones). The Swift registry specification defines no delete; this is the
// API method on the release resource a package is PUT to.
func (q *req) remove() {
	ctx, cancel := context.WithTimeout(q.r.Context(), 15*time.Second)
	defer cancel()
	if _, e := q.s.service.Delete(ctx, q.p, artifact.Target{FeedID: q.feed, Namespace: q.ns(), Coordinate: q.coord(), Version: q.version}); e != nil {
		failure(q.w, e)
		return
	}
	q.w.WriteHeader(204)
}
