// Package gitlfs implements authenticated Git LFS batch and basic transfers.
package gitlfs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mediaType = "application/vnd.git-lfs+json"
const actionTTL = 5 * time.Minute
const maxActions = 4096
const maxObjectSize = 1 << 40 // 1 TiB; streams are never buffered in memory.

type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	mu        sync.Mutex
	grants    map[string]grant
	transfers chan struct{}
}
type grant struct {
	token, principal, feed, oid, operation, source string
	size                                           int64
	expires                                        time.Time
	// remote marks a download of an object that is not stored yet but that an
	// upstream LFS server holds; it is fetched, verified and cached on use.
	remote bool
}

func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Git LFS configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, grants: make(map[string]grant), transfers: artifact.NewUploadSlots()}, nil
}

// SetPublicURL sets a trusted external origin; request Host and proxy headers
// never participate in action URLs. Configure before handing out actions.
func (s *Repository) SetPublicURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\r\n#") {
		return artifact.ErrInput
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && (u.Scheme != "http" || ip == nil || !ip.IsLoopback()) {
		return artifact.ErrInput
	}
	s.base.Store(raw)
	return nil
}
func (s *Repository) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := http.NewResponseController(w)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	r = r.Clone(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{}))
	r.Body = &progressReader{r.Body, c, r.Context()}
	w = &progressWriter{w, c}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("LFS-Authenticate", `Basic realm="Git LFS"`)
	if len(r.Header.Values("Authorization")) == 1 {
		if _, password, ok := r.BasicAuth(); ok {
			r.Header.Set("Authorization", "Bearer "+password)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, r)
}
func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(r.URL.Path, "/")
	if p.InstanceID != s.instance || len(a) < 5 || a[0] != "" || a[1] != "feeds" || a[2] == "" || a[3] != "git-lfs" || len(r.URL.Path) > 1024 || r.URL.RawPath != "" || strings.ContainsAny(r.URL.Path, "\\\x00") {
		fail(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	err := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='git-lfs'`, p.InstanceID, a[2]).Scan(&feed)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		err = artifact.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	if len(a) == 6 && a[4] == "objects" && a[5] == "batch" {
		if r.Method != "POST" {
			writeError(w, 405)
			return
		}
		s.batch(w, r, p, feed, a[2])
		return
	}
	if len(a) == 6 && a[4] == "objects" && blob.Digest(a[5]).Valid() && r.Method == "DELETE" {
		// The Git LFS batch API defines no delete. DELETE objects/{oid} is a
		// Kuvryn Ant management method, policy-gated by the artifact core; the
		// bytes are reclaimed later by garbage collection.
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if _, e := s.service.Delete(ctx, p, artifact.Target{FeedID: feed, Namespace: "git-lfs", Coordinate: a[5], Version: "sha256"}); e != nil {
			fail(w, e)
			return
		}
		w.WriteHeader(204)
		return
	}
	if len(a) != 7 || a[4] != "objects" || !blob.Digest(a[5]).Valid() {
		fail(w, artifact.ErrNotFound)
		return
	}
	operation := a[6]
	if (operation == "upload" && r.Method != "PUT") || (operation == "verify" && r.Method != "POST") || (operation == "download" && r.Method != "GET" && r.Method != "HEAD") {
		writeError(w, 405)
		return
	}
	if operation != "upload" && operation != "verify" && operation != "download" {
		fail(w, artifact.ErrNotFound)
		return
	}
	keys := r.Header.Values("X-LFS-Action")
	if len(keys) != 1 || len(keys[0]) > 100 {
		fail(w, artifact.ErrDenied)
		return
	}
	s.mu.Lock()
	g, ok := s.grants[keys[0]]
	s.mu.Unlock()
	if !ok || !time.Now().Before(g.expires) || g.token != p.TokenID || g.principal != p.PrincipalID || g.feed != feed || g.oid != a[5] || g.operation != operation {
		fail(w, artifact.ErrDenied)
		return
	}
	// Expiry also cancels a transfer already in progress.
	ctx, cancel = context.WithDeadline(r.Context(), g.expires)
	defer cancel()
	r = r.WithContext(ctx)
	select {
	case s.transfers <- struct{}{}:
		defer func() { <-s.transfers }()
	default:
		writeError(w, 429)
		return
	}
	pub := publication(feed, g.oid, g.size)
	pub.ExpectedSource = g.source
	switch operation {
	case "upload":
		if r.ContentLength >= 0 && r.ContentLength != g.size {
			writeError(w, 422)
			return
		}
		_, err = s.service.Publish(ctx, p, pub, &contextReader{r.Body, ctx})
		if err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(200)
	case "verify":
		var object object
		if decode(w, r, &object, 4096) != nil || object.Size == nil || object.OID != g.oid || *object.Size != g.size {
			writeError(w, 422)
			return
		}
		rec, e := s.service.Lookup(ctx, p, pub)
		if e != nil {
			fail(w, e)
			return
		}
		if rec.SourceID != g.source || rec.Info != pub.Info {
			fail(w, artifact.ErrConflict)
			return
		}
		w.WriteHeader(200)
	case "download":
		if g.remote {
			s.downloadRemote(w, r, p, pub)
			return
		}
		s.download(w, r, p, pub, g.source)
	}
}
func publication(feed, oid string, size int64) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: "git-lfs", Coordinate: oid, Version: "sha256", AssetKey: "object", MediaType: "application/octet-stream", Info: blob.Info{Digest: blob.Digest(oid), Size: size}}
}

type object struct {
	OID  string `json:"oid"`
	Size *int64 `json:"size"`
}
type action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header"`
	ExpiresIn int               `json:"expires_in"`
}
type objectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type objectResult struct {
	OID           string            `json:"oid"`
	Size          int64             `json:"size"`
	Authenticated bool              `json:"authenticated"`
	Actions       map[string]action `json:"actions,omitempty"`
	Error         *objectError      `json:"error,omitempty"`
}

func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	mt, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || mt != mediaType {
		return artifact.ErrInput
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if e = d.Decode(v); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return artifact.ErrInput
	}
	return nil
}
func (s *Repository) batch(w http.ResponseWriter, r *http.Request, p identity.Principal, feed, slug string) {
	base, _ := s.base.Load().(string)
	if base == "" {
		fail(w, artifact.ErrUnavailable)
		return
	}
	var req struct {
		Operation string   `json:"operation"`
		Transfers []string `json:"transfers"`
		Objects   []object `json:"objects"`
		HashAlgo  string   `json:"hash_algo"`
	}
	if decode(w, r, &req, 1<<20) != nil {
		writeError(w, 400)
		return
	}
	if req.Operation != "upload" && req.Operation != "download" {
		writeError(w, 422)
		return
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		writeError(w, 409)
		return
	}
	basic := len(req.Transfers) == 0
	for _, v := range req.Transfers {
		basic = basic || v == "basic"
	}
	if !basic {
		writeError(w, 422)
		return
	}
	if len(req.Objects) == 0 || len(req.Objects) > 100 {
		writeError(w, 413)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out := make([]objectResult, 0, len(req.Objects))
	for _, obj := range req.Objects {
		result := objectResult{OID: obj.OID, Authenticated: true}
		if obj.Size != nil {
			result.Size = *obj.Size
		}
		var err error
		if obj.Size == nil || result.Size < 0 || result.Size > maxObjectSize || !blob.Digest(obj.OID).Valid() {
			err = artifact.ErrInput
		} else {
			pub := publication(feed, obj.OID, result.Size)
			var source string
			remote := false
			if req.Operation == "upload" {
				source, err = s.service.Prepare(ctx, p, feed, pub.Namespace, pub.Coordinate, pub.AssetKey)
			}
			if err == nil {
				rec, e := s.service.Lookup(ctx, p, pub)
				if e == nil && rec.Info != pub.Info {
					e = artifact.ErrConflict
				}
				if req.Operation == "download" {
					err = e
					if err == nil {
						source = rec.SourceID
					} else if errors.Is(e, artifact.ErrNotFound) {
						// Not stored: ask the upstream LFS server of a remote source.
						switch re := s.remoteKnown(ctx, p, feed, obj.OID, result.Size); {
						case re == nil:
							err, remote = nil, true
						case !errors.Is(re, artifact.ErrNotFound):
							err = re
						}
					}
				} else if e == nil {
					// Only an authorized, visible object permits existence advertisement.
					out = append(out, result)
					continue
				} else if !errors.Is(e, artifact.ErrNotFound) {
					err = e
				}
				if err == nil {
					operations := []string{req.Operation}
					if req.Operation == "upload" {
						operations = append(operations, "verify")
					}
					result.Actions = make(map[string]action)
					for _, op := range operations {
						g := grant{token: p.TokenID, principal: p.PrincipalID, feed: feed, oid: obj.OID, size: result.Size, source: source, operation: op, expires: time.Now().Add(actionTTL), remote: remote}
						key, e := s.issue(g)
						if e != nil {
							err = e
							break
						}
						result.Actions[op] = action{Href: base + "/feeds/" + url.PathEscape(slug) + "/git-lfs/objects/" + obj.OID + "/" + op, Header: map[string]string{"Authorization": r.Header.Get("Authorization"), "X-LFS-Action": key}, ExpiresIn: int(actionTTL.Seconds())}
					}
				}
			}
		}
		if err != nil {
			code := status(err)
			result.Actions = nil
			result.Error = &objectError{code, http.StatusText(code)}
		}
		out = append(out, result)
	}
	w.Header().Set("Content-Type", mediaType)
	_ = json.NewEncoder(w).Encode(struct {
		Transfer string         `json:"transfer"`
		Objects  []objectResult `json:"objects"`
		HashAlgo string         `json:"hash_algo"`
	}{"basic", out, "sha256"})
}
func (s *Repository) issue(g grant) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.grants {
		if !now.Before(v.expires) {
			delete(s.grants, k)
		}
	}
	if len(s.grants) >= maxActions {
		return "", artifact.ErrUnavailable
	}
	key := rand.Text()
	s.grants[key] = g
	return key, nil
}
func status(e error) int {
	switch {
	case errors.Is(e, artifact.ErrNotFound), errors.Is(e, blob.ErrNotFound):
		return 404
	case errors.Is(e, artifact.ErrDenied):
		return 403
	case errors.Is(e, artifact.ErrConflict):
		return 409
	case errors.Is(e, artifact.ErrInput), errors.Is(e, blob.ErrIntegrity):
		return 422
	}
	return 503
}
func fail(w http.ResponseWriter, e error) { writeError(w, status(e)) }
func writeError(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": http.StatusText(code)})
}
