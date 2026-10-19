// Package maven implements atomic local Maven release bundles and Maven2 reads.
package maven

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrConfiguration = errors.New("invalid Maven repository configuration")
var errSnapshot = errors.New("timestamped snapshots are not supported")
var segment = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

type Repository struct {
	db       *pgxpool.Pool
	service  *artifact.Service
	instance string
	auth     http.Handler
	uploads  chan struct{}
}

func New(db *pgxpool.Pool, service *artifact.Service) (*Repository, error) {
	if db == nil || service == nil || service.DB != db || service.Lease == nil || service.Lease().InstanceID == "" {
		return nil, ErrConfiguration
	}
	r := &Repository{db: db, service: service, instance: service.Lease().InstanceID, uploads: artifact.NewUploadSlots()}
	r.auth = identity.NewBearer(identity.Tokens{DB: db}).Wrap(http.HandlerFunc(r.serve))
	return r, nil
}
func (r *Repository) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	req = req.Clone(req.Context())
	// Require explicit machine credentials, never the browser-session bypass in Bearer.
	if len(req.Header.Values("Authorization")) == 0 {
		w.Header().Set("WWW-Authenticate", `Basic realm="kuvryn-ant"`)
		http.Error(w, "service credentials required", 401)
		return
	}
	if len(req.Header.Values("Authorization")) == 1 {
		if _, password, ok := req.BasicAuth(); ok {
			req.Header.Set("Authorization", "Bearer "+password)
		}
	}
	req.Body = idleBody{ReadCloser: req.Body, ctx: req.Context(), controller: controller}
	r.auth.ServeHTTP(&idleWriter{ResponseWriter: w, controller: controller}, req)
}

type location struct {
	slug, group, name, version, file, key string
	metadata                              bool
}

func parsePath(req *http.Request) (location, error) {
	var l location
	raw := req.URL.EscapedPath()
	if len(raw) > 2048 || strings.ContainsAny(raw, "%\\") {
		return l, artifact.ErrInput
	}
	parts := strings.Split(raw, "/")
	if len(parts) < 7 || parts[0] != "" || parts[1] != "feeds" || parts[3] != "maven" {
		return l, artifact.ErrInput
	}
	for _, s := range parts[2:] {
		if !segment.MatchString(s) || strings.Contains(s, "..") {
			return l, artifact.ErrInput
		}
	}
	l.slug = parts[2]
	l.file = parts[len(parts)-1]
	l.key = strings.Join(parts[4:], "/")
	l.metadata = l.file == "maven-metadata.xml" || strings.HasPrefix(l.file, "maven-metadata.xml.")
	end := len(parts) - 1
	if !l.metadata {
		end--
		l.version = parts[end]
		if strings.Contains(strings.ToUpper(l.version), "SNAPSHOT") {
			return l, errSnapshot
		}
	}
	if end < 6 {
		return l, artifact.ErrInput
	}
	for _, groupPart := range parts[4 : end-1] {
		if strings.Contains(groupPart, ".") {
			return l, artifact.ErrInput
		}
	}
	l.name = parts[end-1]
	l.group = strings.Join(parts[4:end-1], ".")
	if len(l.group)+len(l.name)+1 > 256 {
		return l, artifact.ErrInput
	}
	if l.metadata {
		if strings.Contains(strings.ToUpper(l.name), "SNAPSHOT") {
			return l, errSnapshot
		}
		if l.file != "maven-metadata.xml" && checksumSuffix(l.file) == "" {
			return l, artifact.ErrInput
		}
	} else if l.file != "_bundle" && !validAsset(l, l.file, true) {
		return l, artifact.ErrInput
	}
	return l, nil
}
func checksumSuffix(name string) string {
	for _, s := range []string{".sha256", ".sha1", ".md5"} {
		if strings.HasSuffix(name, s) {
			return s
		}
	}
	return ""
}
func validAsset(l location, name string, checksums bool) bool {
	if checksums {
		name = strings.TrimSuffix(name, checksumSuffix(name))
	}
	base := l.name + "-" + l.version
	if name == base+".pom" || name == base+".jar" {
		return true
	}
	if !strings.HasPrefix(name, base+"-") || !strings.HasSuffix(name, ".jar") {
		return false
	}
	classifier := strings.TrimSuffix(strings.TrimPrefix(name, base+"-"), ".jar")
	return segment.MatchString(classifier) && !strings.Contains(classifier, "..")
}
func (l location) publication(feed string) artifact.Publication {
	return artifact.Publication{FeedID: feed, Namespace: l.group, Coordinate: l.group + ":" + l.name, Version: l.version, AssetKey: l.key}
}
func (r *Repository) serve(w http.ResponseWriter, req *http.Request) {
	p, ok := identity.PrincipalFromContext(req.Context())
	if !ok || p.InstanceID != r.instance || p.TokenID == "" {
		fail(w, artifact.ErrNotFound)
		return
	}
	parseReq := req
	versionDir := req.Method == "DELETE" && strings.HasSuffix(req.URL.Path, "/")
	if versionDir {
		// DELETE of a version directory (trailing slash) removes the whole
		// version; validate it with the same rules as the _bundle endpoint.
		parseReq = req.Clone(req.Context())
		parseReq.URL.Path += "_bundle"
		parseReq.URL.RawPath = ""
	}
	l, err := parsePath(parseReq)
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
	var feed string
	err = r.db.QueryRow(ctx, `SELECT id::text FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='maven'`, p.InstanceID, l.slug).Scan(&feed)
	cancel()
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case req.Method == "DELETE" && !l.metadata:
		// A version directory removes the version; a file removes that asset
		// (and the version with its last asset). Policy-gated by the shared core.
		target := artifact.Target{FeedID: feed, Namespace: l.group, Coordinate: l.group + ":" + l.name, Version: l.version}
		if !versionDir {
			target.AssetKey = l.key
		}
		if _, err := r.service.Delete(req.Context(), p, target); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(204)
	case req.Method == "POST" && l.file == "_bundle":
		r.publish(w, req, p, l, feed)
	case (req.Method == "GET" || req.Method == "HEAD") && l.metadata:
		r.metadata(w, req, p, l, feed)
	case (req.Method == "GET" || req.Method == "HEAD") && l.file != "_bundle":
		r.get(w, req, p, l.publication(feed))
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, DELETE")
		http.Error(w, "native multi-PUT deployment unsupported; publish a complete release via POST to the version/_bundle endpoint", 405)
	}
}
func etag(rec artifact.Record) string { return `"sha256:` + string(rec.Info.Digest) + `"` }
func fail(w http.ResponseWriter, err error) {
	code := 503
	switch {
	case errors.Is(err, errSnapshot):
		http.Error(w, "timestamped snapshots unsupported; release bundles only", 501)
		return
	case errors.Is(err, artifact.ErrInput), errors.Is(err, blob.ErrIntegrity):
		code = 400
	case errors.Is(err, artifact.ErrDenied):
		code = 403
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		code = 404
	case errors.Is(err, artifact.ErrConflict):
		code = 409
	}
	http.Error(w, http.StatusText(code), code)
}
