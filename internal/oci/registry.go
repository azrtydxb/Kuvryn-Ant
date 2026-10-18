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
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
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
}

func New(db *pgxpool.Pool, service *artifact.Service, uploadRoot string) (*Registry, error) {
	if db == nil || service == nil || uploadRoot == "" || db.Config().MaxConns < 4 || service.Lease == nil || service.Lease().InstanceID == "" {
		return nil, ErrConfiguration
	}
	if err := os.MkdirAll(uploadRoot, 0700); err != nil {
		return nil, ErrConfiguration
	}
	info, err := os.Lstat(uploadRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
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
	budget := int(db.Config().MaxConns / 4)
	if budget > 4 {
		budget = 4
	}
	return &Registry{db: db, service: service, root: root, auth: identity.NewBearer(identity.Tokens{DB: db}), transfers: make(chan struct{}, budget), instance: service.Lease().InstanceID, rootID: hex.EncodeToString(rootHash[:])}, nil
}
func (r *Registry) Close() error { return r.root.Close() }

// ServeHTTP accepts Basic(username,service-token) or a service-token Bearer
// header. Credential validation, revocation and failed-attempt limits are shared.
func (r *Registry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	// OCI clients discover authentication with an initial credential-free probe.
	// This cheap challenge does not represent a failed credential verification.
	if len(request.Header.Values("Authorization")) == 0 {
		w.Header().Set("WWW-Authenticate", `Basic realm="Kuvryn Ant"`)
		failure(w, http.StatusUnauthorized, "UNAUTHORIZED", "registry authentication required")
		return
	}
	req := request.Clone(request.Context())
	if len(req.Header.Values("Authorization")) == 1 {
		if _, password, ok := req.BasicAuth(); ok {
			req.Header.Set("Authorization", "Bearer "+password)
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
	switch {
	case errors.Is(err, artifact.ErrDenied):
		failure(w, 403, "DENIED", "repository access denied")
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, blob.ErrNotFound):
		failure(w, 404, "BLOB_UNKNOWN", "content not found")
	case errors.Is(err, artifact.ErrConflict):
		failure(w, 409, "DENIED", "immutable content conflicts")
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

type repository struct{ feed, name, namespace, full string }

func (r *Registry) repository(ctx context.Context, p identity.Principal, name string) (repository, error) {
	if p.InstanceID != r.instance {
		return repository{}, artifact.ErrNotFound
	}
	feed, repo, ok := strings.Cut(name, "/")
	if !ok || len(name) > 256 || !namePattern.MatchString(name) {
		return repository{}, artifact.ErrInput
	}
	result := repository{name: repo, full: name}
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		result.namespace = repo[:i]
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := r.db.QueryRow(ctx, `SELECT id FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='oci'`, p.InstanceID, feed).Scan(&result.feed)
	if errors.Is(err, pgx.ErrNoRows) {
		return repository{}, artifact.ErrNotFound
	}
	if err != nil {
		return repository{}, artifact.ErrUnavailable
	}
	return result, nil
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
	path := strings.TrimPrefix(req.URL.Path, "/v2/")
	var name, action, reference string
	for _, candidate := range []string{"/blobs/uploads/", "/manifests/", "/blobs/", "/tags/list"} {
		if index := strings.LastIndex(path, candidate); index > 0 {
			name = path[:index]
			action = candidate
			reference = path[index+len(candidate):]
			break
		}
	}
	if name == "" || strings.Contains(reference, "/") {
		failure(w, 404, "NAME_UNKNOWN", "repository not found")
		return
	}
	rep, err := r.repository(req.Context(), p, name)
	if err != nil {
		failService(w, err)
		return
	}
	switch action {
	case "/blobs/uploads/":
		select {
		case r.transfers <- struct{}{}:
			defer func() { <-r.transfers }()
		default:
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
