// Package oci implements the local OCI/Docker distribution upload and pull
// foundation. Remote/replica behavior, mutable tags and conformance certification
// remain separate delivery work. Tokens are the shared scoped service tokens.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteconfig"
	"github.com/azrtydxb/Kuvryn-Ant/internal/safedir"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxBlob = int64(math.MaxInt64)

var ErrConfiguration = errors.New("invalid OCI registry configuration")

type Registry struct {
	db               *pgxpool.Pool
	service          *artifact.Service
	root             *os.Root
	auth             *identity.Bearer
	transfers        chan struct{}
	instance, rootID string
	mirror           Mirror
	anonMu           sync.Mutex
	anon             identity.Principal
}

func New(db *pgxpool.Pool, service *artifact.Service, uploadRoot string) (*Registry, error) {
	if db == nil || service == nil || uploadRoot == "" || db.Config().MaxConns < 4 || service.Lease == nil || service.Lease().InstanceID == "" {
		return nil, ErrConfiguration
	}
	if err := os.MkdirAll(uploadRoot, 0700); err != nil {
		return nil, ErrConfiguration
	}
	info, err := os.Lstat(uploadRoot)
	if err != nil || !safedir.Private(info) {
		return nil, ErrConfiguration
	}
	root, err := os.OpenRoot(uploadRoot)
	if err != nil {
		return nil, ErrConfiguration
	}
	absolute, err := filepath.Abs(uploadRoot)
	if err != nil {
		root.Close()
		return nil, ErrConfiguration
	}
	rootHash := sha256.Sum256([]byte(absolute))
	// Uploads hold database connections: leave three quarters of the pool for the rest.
	budget := min(int(db.Config().MaxConns/4), artifact.UploadSlots())
	return &Registry{db: db, service: service, root: root, auth: identity.NewBearer(identity.Tokens{DB: db}), transfers: make(chan struct{}, budget), instance: service.Lease().InstanceID, rootID: hex.EncodeToString(rootHash[:])}, nil
}
func (r *Registry) Close() error { return r.root.Close() }

// ServeHTTP accepts Basic(username,service-token) or a service-token Bearer
// header. Credential validation, revocation and failed-attempt limits are shared.
func (r *Registry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	// OCI clients discover authentication with an initial credential-free probe.
	// This cheap challenge does not represent a failed credential verification.
	// containers/image (CRI-O, podman, skopeo) answers a Basic challenge with
	// empty credentials when it has none; that is the same as sending nothing
	// and is never a failed login.
	if values := request.Header.Values("Authorization"); len(values) == 0 || len(values) == 1 && identity.EmptyBasic(values[0]) {
		// Feeds with anonymous_read (typically a pull-through mirror used by
		// dockerd, which never sends credentials to a mirror) are served without
		// a challenge. Everything else, and every write, still gets the 401.
		// The bare /v2/ probe without ?ns= always challenges: it names no
		// feed, and CRI-O only sends a pull secret after a challenge.
		// When the client-authentication middleware already applied the feed
		// policy, it alone decides anonymous access.
		if !identity.PolicyEnforced(request.Context()) && r.serveAnonymous(w, request) {
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Kuvryn Ant"`)
		failure(w, http.StatusUnauthorized, "UNAUTHORIZED", "registry authentication required")
		return
	}
	req := request.Clone(request.Context())
	if len(req.Header.Values("Authorization")) == 1 {
		if username, password, ok := req.BasicAuth(); ok {
			req.Header.Set("Authorization", "Bearer "+password)
			req = req.WithContext(identity.WithUsernameHint(req.Context(), username))
		}
	}
	capture := &authResponse{header: make(http.Header)}
	capture.header.Set("X-Request-ID", w.Header().Get("X-Request-ID"))
	allowed := false
	r.auth.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, authenticated *http.Request) { allowed = true; r.serve(w, authenticated) })).ServeHTTP(capture, req)
	if !allowed {
		status := capture.status
		if status == 0 {
			status = 401
		}
		code := "UNAUTHORIZED"
		if status == 429 {
			code = "TOOMANYREQUESTS"
		}
		if status >= 500 {
			code = "UNKNOWN"
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Kuvryn Ant"`)
		for _, name := range []string{"Retry-After", "X-Request-ID"} {
			if value := capture.header.Get(name); value != "" {
				w.Header().Set(name, value)
			}
		}
		failure(w, status, code, "registry authentication required")
	}
}

type authResponse struct {
	header http.Header
	status int
}

func (w *authResponse) Header() http.Header         { return w.header }
func (w *authResponse) WriteHeader(status int)      { w.status = status }
func (w *authResponse) Write(p []byte) (int, error) { return len(p), nil }
func failure(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}
func failService(w http.ResponseWriter, err error) {
	var limited *artifact.RateLimitedError
	switch {
	case errors.As(err, &limited):
		after := int(limited.RetryAfter.Seconds())
		if after < 1 {
			after = 30
		}
		w.Header().Set("Retry-After", strconv.Itoa(after))
		failure(w, 429, "TOOMANYREQUESTS", "upstream registry rate limit reached")
	case errors.Is(err, artifact.ErrDenied):
		failure(w, 403, "DENIED", "repository access denied")
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, blob.ErrNotFound):
		failure(w, 404, "BLOB_UNKNOWN", "content not found")
	case errors.Is(err, artifact.ErrConflict):
		failure(w, 409, "DENIED", artifact.ConflictMessage(err, "immutable content conflicts"))
	case errors.Is(err, blob.ErrIntegrity):
		failure(w, 400, "DIGEST_INVALID", "content digest does not match")
	case errors.Is(err, artifact.ErrInput):
		failure(w, 400, "NAME_INVALID", "invalid repository request")
	default:
		failure(w, 503, "UNKNOWN", "registry unavailable")
	}
}

var namePattern = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(\/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

type repository struct {
	feed, name, namespace, full string
	mirror, anonymous           bool
}

// Mirror configures serving an OCI feed without the feed slug prefix so the
// registry works as a pull-through mirror for dockerd, buildkit and containerd.
// A prefixed path (/v2/<feed-slug>/...) always wins when its first segment is an
// existing OCI feed slug. Otherwise the whole name is the upstream repository
// and the feed is chosen by the containerd "ns" query parameter (Namespaces),
// or DefaultFeed when no ns is sent. Mirror routes are read-only.
type Mirror struct {
	DefaultFeed string            // feed slug used when the client sends no ns
	Namespaces  map[string]string // upstream registry host (ns=) -> feed slug
}

// SetMirror enables mirror routing. It must be called before serving.
func (r *Registry) SetMirror(m Mirror) {
	ns := make(map[string]string, len(m.Namespaces))
	for host, slug := range m.Namespaces {
		ns[strings.ToLower(host)] = slug
	}
	r.mirror = Mirror{DefaultFeed: m.DefaultFeed, Namespaces: ns}
}

// mirrorSlug picks the mirror feed for a request. With a namespace map, an
// unlisted ns is refused rather than silently answered from the default feed.
func (r *Registry) mirrorSlug(ns string) string {
	ns = strings.ToLower(ns)
	if ns != "" {
		if slug, ok := r.mirror.Namespaces[ns]; ok {
			return slug
		}
		if len(r.mirror.Namespaces) > 0 {
			return ""
		}
	}
	return r.mirror.DefaultFeed
}

// pingSlug is the feed a /v2/ probe addresses. Only a mirror namespace
// (containerd's ?ns=) names one; a bare probe addresses no feed.
func (r *Registry) pingSlug(ns string) string {
	if ns == "" {
		return ""
	}
	return r.mirrorSlug(ns)
}

func (r *Registry) feedBySlug(ctx context.Context, instance, slug string) (id string, anonymous bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.db.QueryRow(ctx, `SELECT id,anonymous_read FROM feeds WHERE instance_id=$1 AND slug=$2 AND format IN ('oci','oci-artifacts')`, instance, slug).Scan(&id, &anonymous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, artifact.ErrNotFound
	}
	if err != nil {
		return "", false, artifact.ErrUnavailable
	}
	return id, anonymous, nil
}

// route resolves a client-visible repository name to a feed.
func (r *Registry) route(ctx context.Context, instance, name, ns string) (repository, error) {
	if len(name) > 256 || len(ns) > 255 || !namePattern.MatchString(name) {
		return repository{}, artifact.ErrInput
	}
	if slug, repo, ok := strings.Cut(name, "/"); ok {
		id, anonymous, err := r.feedBySlug(ctx, instance, slug)
		if err == nil {
			result := repository{feed: id, name: repo, full: name, anonymous: anonymous}
			if i := strings.LastIndex(repo, "/"); i >= 0 {
				result.namespace = repo[:i]
			}
			return result, nil
		}
		if !errors.Is(err, artifact.ErrNotFound) {
			return repository{}, err
		}
	}
	slug := r.mirrorSlug(ns)
	if slug == "" {
		return repository{}, artifact.ErrNotFound
	}
	id, anonymous, err := r.feedBySlug(ctx, instance, slug)
	if err != nil {
		return repository{}, err
	}
	result := repository{feed: id, name: name, full: name, mirror: true, anonymous: anonymous}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		result.namespace = name[:i]
	}
	return result, nil
}

func (r *Registry) repository(ctx context.Context, p identity.Principal, name string) (repository, error) {
	if p.InstanceID != r.instance {
		return repository{}, artifact.ErrNotFound
	}
	return r.route(ctx, p.InstanceID, name, "")
}

// anonymousPrincipal returns the internal anonymous-reader principal.
func (r *Registry) anonymousPrincipal(ctx context.Context) (identity.Principal, error) {
	r.anonMu.Lock()
	defer r.anonMu.Unlock()
	if r.anon.PrincipalID != "" {
		return r.anon, nil
	}
	p, err := (identity.Roles{DB: r.db}).EnsureAnonymous(ctx, r.instance)
	if err != nil {
		return identity.Principal{}, err
	}
	r.anon = p
	return p, nil
}

// splitName separates "/v2/<name>/<action>/<reference>".
func splitName(path string) (name, action, reference string) {
	path = strings.TrimPrefix(path, "/v2/")
	// Some clients omit the trailing slash when starting an upload.
	if strings.HasSuffix(path, "/blobs/uploads") && len(path) > len("/blobs/uploads") {
		path += "/"
	}
	bestIndex := 0
	for _, candidate := range []string{"/blobs/uploads/", "/manifests/", "/referrers/", "/blobs/", "/tags/list"} {
		if index := strings.LastIndex(path, candidate); index > bestIndex {
			name = path[:index]
			action = candidate
			reference = path[index+len(candidate):]
			bestIndex = index
		}
	}
	return
}

// serveAnonymous serves an unauthenticated read of an anonymous_read feed. It
// reports false (nothing written) when the caller must authenticate instead.
func (r *Registry) serveAnonymous(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	if !strings.HasPrefix(req.URL.Path, "/v2/") || strings.ContainsAny(req.URL.Path, "\\\x00") {
		return false
	}
	ns := req.URL.Query().Get("ns")
	anonymous := false
	if req.URL.Path == "/v2/" {
		slug := r.pingSlug(ns)
		if slug == "" {
			return false
		}
		_, a, err := r.feedBySlug(req.Context(), r.instance, slug)
		anonymous = err == nil && a
	} else {
		name, _, _ := splitName(req.URL.Path)
		if name == "" {
			return false
		}
		rep, err := r.route(req.Context(), r.instance, name, ns)
		anonymous = err == nil && rep.anonymous
	}
	if !anonymous {
		return false
	}
	p, err := r.anonymousPrincipal(req.Context())
	if err != nil {
		failure(w, 503, "UNKNOWN", "registry unavailable")
		return true
	}
	r.serve(w, req.WithContext(identity.WithAuthenticatedPrincipal(req.Context(), p)))
	return true
}
func (rep repository) publication(version, key string) artifact.Publication {
	return artifact.Publication{FeedID: rep.feed, Namespace: rep.namespace, Coordinate: rep.name, Version: version, AssetKey: key}
}
func parseDigest(raw string) (blob.Digest, bool) {
	algorithm, value, ok := strings.Cut(raw, ":")
	d := blob.Digest(value)
	return d, ok && algorithm == "sha256" && d.Valid()
}
func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	p, _ := identity.PrincipalFromContext(req.Context())
	if req.URL.Path == "/v2/" && (req.Method == "GET" || req.Method == "HEAD") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if req.Method != "HEAD" {
			_, _ = io.WriteString(w, "{}")
		}
		return
	}
	if !strings.HasPrefix(req.URL.Path, "/v2/") || strings.ContainsAny(req.URL.Path, "\\\x00") || strings.Contains(strings.ToLower(req.URL.RawPath), "%2f") || strings.Contains(strings.ToLower(req.URL.RawPath), "%5c") {
		failure(w, 404, "NAME_UNKNOWN", "repository not found")
		return
	}
	name, action, reference := splitName(req.URL.Path)
	if name == "" || strings.Contains(reference, "/") {
		failure(w, 404, "NAME_UNKNOWN", "repository not found")
		return
	}
	if p.InstanceID != r.instance {
		failService(w, artifact.ErrNotFound)
		return
	}
	rep, err := r.route(req.Context(), p.InstanceID, name, req.URL.Query().Get("ns"))
	if err != nil {
		failService(w, err)
		return
	}
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		rep = r.canonicalOfficial(req.Context(), p.InstanceID, rep)
	}
	if rep.mirror && req.Method != http.MethodGet && req.Method != http.MethodHead {
		failure(w, 405, "UNSUPPORTED", "mirror feeds are read-only")
		return
	}
	if p.Anonymous && req.Method != http.MethodGet && req.Method != http.MethodHead {
		failure(w, 401, "UNAUTHORIZED", "authentication required")
		return
	}
	switch action {
	case "/blobs/uploads/":
		// Multi-platform pushes upload several blobs at once. Queue briefly for a
		// slot instead of failing so the client does not have to retry-storm.
		wait := time.NewTimer(15 * time.Second)
		select {
		case r.transfers <- struct{}{}:
			wait.Stop()
			defer func() { <-r.transfers }()
		case <-req.Context().Done():
			wait.Stop()
			return
		case <-wait.C:
			w.Header().Set("Retry-After", "1")
			failure(w, 503, "UNKNOWN", "upload capacity exhausted")
			return
		}
		r.upload(w, req, p, rep, reference)
	case "/blobs/":
		if req.Method == "GET" || req.Method == "HEAD" {
			r.read(w, req, p, rep, reference, "blob")
		} else {
			failure(w, 405, "UNSUPPORTED", "operation unavailable")
		}
	case "/manifests/":
		if req.Method == "PUT" {
			r.manifest(w, req, p, rep, reference)
		} else if req.Method == "GET" || req.Method == "HEAD" {
			r.read(w, req, p, rep, reference, "manifest")
		} else if req.Method == "DELETE" {
			r.deleteManifest(w, req, p, rep, reference)
		} else {
			failure(w, 405, "UNSUPPORTED", "operation unavailable")
		}
	case "/referrers/":
		if req.Method == "GET" || req.Method == "HEAD" {
			r.referrers(w, req, p, rep, reference)
		} else {
			failure(w, 405, "UNSUPPORTED", "operation unavailable")
		}
	case "/tags/list":
		if reference == "" && req.Method == "GET" {
			r.tags(w, req, p, rep)
		} else {
			failure(w, 405, "UNSUPPORTED", "operation unavailable")
		}
	}
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// deleteManifest implements the distribution-spec manifest delete. A digest
// reference removes the manifest (and the tags that point at it); a tag
// reference removes only that tag. Both go through the policy-gated artifact
// core (delete authorization, feed policy, audit, outbox, replication
// tombstones). Layer and config blobs are left for garbage collection.
func (r *Registry) deleteManifest(w http.ResponseWriter, req *http.Request, p identity.Principal, rep repository, reference string) {
	t := artifact.Target{FeedID: rep.feed, Namespace: rep.namespace, Coordinate: rep.name}
	if digest, ok := parseDigest(reference); ok {
		t.Version = string(digest)
	} else if tagPattern.MatchString(reference) {
		t.Tag = reference
	} else {
		failure(w, 400, "TAG_INVALID", "invalid manifest reference")
		return
	}
	if _, err := r.service.Delete(req.Context(), p, t); err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			failure(w, 404, "MANIFEST_UNKNOWN", "manifest not found")
			return
		}
		if errors.Is(err, artifact.ErrPolicy) || errors.Is(err, artifact.ErrUnlistOnly) || errors.Is(err, artifact.ErrImmutable) {
			failure(w, 405, "UNSUPPORTED", "deletion is disabled by repository policy")
			return
		}
		failService(w, err)
		return
	}
	w.WriteHeader(202)
}

// canonicalOfficial maps a single-segment name ("alpine") to Docker Hub's
// canonical "library/alpine" for reads on feeds that pull through from Docker
// Hub, so both forms address one component instead of creating duplicates. A
// component already stored under the short name keeps it, so nothing existing
// stops resolving. Writes are never rewritten.
func (r *Registry) canonicalOfficial(ctx context.Context, instance string, rep repository) repository {
	if rep.name == "" || strings.Contains(rep.name, "/") {
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := r.db.Query(ctx, `SELECT rc.config->>'origin' FROM feed_sources fs JOIN remote_source_configs rc ON rc.instance_id=fs.instance_id AND rc.source_id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2`, instance, rep.feed)
	if err != nil {
		return rep
	}
	hub := false
	for rows.Next() {
		var origin string
		if rows.Scan(&origin) == nil && remoteconfig.IsDockerHub(origin) {
			hub = true
		}
	}
	rows.Close()
	if !hub {
		return rep
	}
	var exists bool
	if err = r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN components c ON c.instance_id=fs.instance_id AND c.source_id=fs.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3)`, instance, rep.feed, rep.name).Scan(&exists); err != nil || exists {
		return rep
	}
	rep.name = "library/" + rep.name
	rep.namespace = "library"
	return rep
}
