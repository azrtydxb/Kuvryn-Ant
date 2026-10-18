// Package terraform implements a local Terraform provider and module registry;
// see README.md for the protocol surface, client setup and gates.
package terraform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	moduleNamespace   = "modules"
	providerNamespace = "providers"
	maxVersions       = 512
	maxMetadata       = 256 << 10
)

var (
	errInvalid     = errors.New("invalid Terraform artifact")
	slugPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	namePattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,62}[a-z0-9])?$`)
	typePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	systemPattern  = regexp.MustCompile(`^[a-z0-9]{1,64}$`)
	platformPart   = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-[0-9A-Za-z]+(\.[0-9A-Za-z-]+)*)?$`)
)

func validVersion(v string) bool { return len(v) <= 64 && versionPattern.MatchString(v) }

// Repository serves Terraform registry protocols for feeds of format terraform.
type Repository struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	base      atomic.Value
	transfers chan struct{}
}

// New returns a registry bound to the service's writer-lease instance.
func New(db *pgxpool.Pool, s *artifact.Service) (*Repository, error) {
	if db == nil || s == nil || s.Lease == nil || s.Lease().InstanceID == "" {
		return nil, errors.New("invalid Terraform configuration")
	}
	return &Repository{db: db, service: s, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: s.Lease().InstanceID, transfers: make(chan struct{}, 2)}, nil
}

func (s *Repository) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	a := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(a) < 7 || a[0] != "feeds" || a[2] != "terraform" || a[3] != "v1" || p.InstanceID != s.instance || strings.ContainsAny(r.URL.Path, "\\\x00") || strings.Contains(r.URL.EscapedPath(), "%") {
		failure(w, artifact.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	e := s.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='terraform'`, p.InstanceID, a[1]).Scan(&feed)
	cancel()
	if errors.Is(e, pgx.ErrNoRows) {
		e = artifact.ErrNotFound
	}
	if e != nil {
		failure(w, e)
		return
	}
	switch r.Method {
	case "GET", "HEAD", "PUT":
	case "DELETE":
		// The shared artifact core exposes no version deletion primitive and
		// published versions are immutable; deletion stays an operator/retention
		// action until the core grows a policy-gated primitive.
		w.Header().Set("Allow", "GET, HEAD, PUT")
		failureStatus(w, 405)
		return
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		failureStatus(w, 405)
		return
	}
	rest := a[5:]
	switch a[4] {
	case "modules":
		s.modules(w, r, p, a[1], feed, rest)
	case "providers":
		s.providers(w, r, p, a[1], feed, rest)
	default:
		failure(w, artifact.ErrNotFound)
	}
}

func failureStatus(w http.ResponseWriter, code int) { http.Error(w, http.StatusText(code), code) }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	return r.r.Read(b)
}

func info(b []byte) blob.Info {
	h := sha256.Sum256(b)
	return blob.Info{Size: int64(len(b)), Digest: blob.Digest(hex.EncodeToString(h[:]))}
}

func (s *Repository) acquire() bool {
	select {
	case s.transfers <- struct{}{}:
		return true
	default:
		return false
	}
}
func (s *Repository) release() { <-s.transfers }

// authorizeAdmin requires feed-level Admin (admin ingestion) and then resolves
// the pinned local source through the shared publish path before bytes arrive.
func (s *Repository) prepare(ctx context.Context, p identity.Principal, feed, namespace, coordinate, key string) (string, error) {
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		return "", artifact.ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Admin) {
		return "", artifact.ErrDenied
	}
	return s.service.Prepare(ctx, p, feed, namespace, coordinate, key)
}

// listVersions returns the versions of the package-owning source that carry the
// marker asset (the asset sealed last, so only complete versions appear).
func (s *Repository) listVersions(ctx context.Context, p identity.Principal, feed, namespace, coordinate, marker string) ([]string, error) {
	rows, e := s.db.Query(ctx, `WITH owner AS (SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,o.namespace,v.version FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id WHERE a.asset_key=$4 ORDER BY v.version LIMIT $5`, p.InstanceID, feed, coordinate, marker, maxVersions+1)
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	var versions []string
	var source string
	for rows.Next() {
		var ns, v string
		if e = rows.Scan(&source, &ns, &v); e != nil {
			break
		}
		if ns != namespace {
			rows.Close()
			return nil, artifact.ErrUnavailable
		}
		versions = append(versions, v)
	}
	if e == nil {
		e = rows.Err()
	}
	rows.Close()
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	if len(versions) == 0 || len(versions) > maxVersions {
		if len(versions) > maxVersions {
			return nil, artifact.ErrUnavailable
		}
		return nil, artifact.ErrNotFound
	}
	policy, e := (identity.Roles{DB: s.db}).Resolve(ctx, p)
	if e != nil {
		return nil, artifact.ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, SourceID: source, Namespace: namespace, Kind: identity.ArtifactResource}, identity.Read) {
		return nil, artifact.ErrNotFound
	}
	sort.Strings(versions)
	return versions, nil
}

// serveAsset streams one verified immutable asset.
func (s *Repository) serveAsset(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication) {
	record, body, e := s.service.Open(r.Context(), p, pub, 0, -1)
	if e != nil {
		failure(w, e)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", itoa(record.Info.Size))
	w.Header().Set("ETag", `"`+string(record.Info.Digest)+`"`)
	if r.Method != "HEAD" {
		_, _ = io.CopyBuffer(w, struct{ io.Reader }{body}, make([]byte, 64<<10))
	}
}

// readAsset loads a small verified asset into memory.
func (s *Repository) readAsset(ctx context.Context, p identity.Principal, pub artifact.Publication, limit int64) (artifact.Record, []byte, error) {
	record, body, e := s.service.Open(ctx, p, pub, 0, -1)
	if e != nil {
		return record, nil, e
	}
	defer body.Close()
	if record.Info.Size > limit {
		return record, nil, artifact.ErrUnavailable
	}
	data, e := io.ReadAll(io.LimitReader(contextReader{ctx, body}, limit+1))
	if e != nil || int64(len(data)) != record.Info.Size {
		return record, nil, artifact.ErrUnavailable
	}
	return record, data, nil
}
