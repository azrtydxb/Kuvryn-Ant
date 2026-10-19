// Package goproxy serves local Go modules using the GOPROXY protocol.
// Ingestion is a complete immutable bundle; it is not a Go publish command.
package goproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/mod/module"
)

var ErrConfiguration = errors.New("invalid Go proxy configuration")

type Proxy struct {
	db        *pgxpool.Pool
	service   *artifact.Service
	auth      *identity.Bearer
	instance  string
	transfers chan struct{}
	// sumdbOrigin overrides the checksum database origin (tests only).
	sumdbOrigin string
}

// New constructs a handler for /feeds/{slug}/go/. Basic passwords and Bearer
// credentials are shared scoped service tokens. Anonymous access is disabled.
func New(db *pgxpool.Pool, service *artifact.Service) (*Proxy, error) {
	if db == nil || service == nil || service.Lease == nil || service.Lease().InstanceID == "" {
		return nil, ErrConfiguration
	}
	return &Proxy{db: db, service: service, auth: identity.NewBearer(identity.Tokens{DB: db}), instance: service.Lease().InstanceID, transfers: artifact.NewUploadSlots()}, nil
}
func (s *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	req := r.Clone(r.Context())
	if len(req.Header.Values("Authorization")) == 1 {
		if _, password, ok := req.BasicAuth(); ok {
			req.Header.Set("Authorization", "Bearer "+password)
		}
	}
	s.auth.Wrap(http.HandlerFunc(s.serve)).ServeHTTP(w, req)
}

type route struct{ slug, module, version, kind string }

func parseRoute(raw string) (route, error) {
	var out route
	if len(raw) > 4096 || !strings.HasPrefix(raw, "/feeds/") || strings.ContainsAny(raw, "\\\x00") {
		return out, artifact.ErrInput
	}
	out.slug, raw, _ = strings.Cut(strings.TrimPrefix(raw, "/feeds/"), "/go/")
	if out.slug == "" || strings.Contains(out.slug, "/") || raw == "" {
		return out, artifact.ErrInput
	}
	var escaped string
	if strings.HasSuffix(raw, "/@latest") {
		escaped = strings.TrimSuffix(raw, "/@latest")
		out.kind = "latest"
	} else {
		var suffix string
		escaped, suffix, _ = strings.Cut(raw, "/@v/")
		if suffix == "list" {
			out.kind = "list"
		} else {
			version := suffix
			out.kind = "bundle"
			for _, kind := range []string{"info", "mod", "zip"} {
				if strings.HasSuffix(suffix, "."+kind) {
					version = strings.TrimSuffix(suffix, "."+kind)
					out.kind = kind
					break
				}
			}
			var err error
			out.version, err = module.UnescapeVersion(version)
			if err != nil || module.CanonicalVersion(out.version) != out.version || out.version == "" {
				return out, artifact.ErrInput
			}
			canonical, _ := module.EscapeVersion(out.version)
			if canonical != version {
				return out, artifact.ErrInput
			}
		}
	}
	var err error
	out.module, err = module.UnescapePath(escaped)
	if err != nil || len(out.module) > 256 || module.CheckPath(out.module) != nil {
		return out, artifact.ErrInput
	}
	canonical, _ := module.EscapePath(out.module)
	if canonical != escaped {
		return out, artifact.ErrInput
	}
	if out.version != "" && module.Check(out.module, out.version) != nil {
		return out, artifact.ErrInput
	}
	return out, nil
}
func (r route) publication(feed, kind string) artifact.Publication {
	ns := path.Dir(r.module)
	return artifact.Publication{FeedID: feed, Namespace: ns, Coordinate: r.module, Version: r.version, AssetKey: "module." + kind}
}

// upstreamFile maps a stored asset identity back to its GOPROXY request path.
func upstreamFile(pub artifact.Publication) (artifact.Origin, bool) {
	kind := strings.TrimPrefix(pub.AssetKey, "module.")
	if kind == pub.AssetKey || (kind != "info" && kind != "mod" && kind != "zip") {
		return artifact.Origin{}, false
	}
	module, err := moduleEscape(pub.Coordinate, pub.Version)
	if err != nil {
		return artifact.Origin{}, false
	}
	return artifact.Origin{Path: module.path + "/@v/" + module.version + "." + kind}, true
}

// withMediaType gives a cached asset the same media type local ingestion uses.
func withMediaType(pub artifact.Publication) artifact.Publication {
	switch pub.AssetKey {
	case "module.info":
		pub.MediaType = "application/json"
	case "module.mod":
		pub.MediaType = "text/plain; charset=utf-8"
	case "module.zip":
		pub.MediaType = "application/zip"
	}
	return pub
}

type escapedModule struct{ path, version string }

func moduleEscape(path, version string) (escapedModule, error) {
	p, err := module.EscapePath(path)
	if err != nil {
		return escapedModule{}, err
	}
	v := ""
	if version != "" {
		if v, err = module.EscapeVersion(version); err != nil {
			return escapedModule{}, err
		}
	}
	return escapedModule{p, v}, nil
}
func (s *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	p, _ := identity.PrincipalFromContext(r.Context())
	if p.InstanceID != s.instance {
		fail(w, artifact.ErrNotFound)
		return
	}
	if slug, rest, ok := sumdbRoute(r.URL.Path); ok {
		s.sumdb(w, r, p, slug, rest)
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	target, err := parseRoute(r.URL.Path)
	if err == nil && (strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") || strings.Contains(strings.ToLower(r.URL.RawPath), "%5c")) {
		err = artifact.ErrInput
	}
	if err != nil {
		if read {
			// The go command probes path prefixes and branch names; GOPROXY
			// requires 404 or 410 for anything that is not a module version so
			// it moves on instead of aborting on 400.
			failure404(w)
			return
		}
		fail(w, err)
		return
	}
	if (target.kind == "bundle" && r.Method != "PUT" && r.Method != "DELETE") || (target.kind != "bundle" && r.Method != "GET" && r.Method != "HEAD") {
		http.Error(w, "method not allowed", 405)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	var feed string
	err = s.db.QueryRow(ctx, `SELECT id FROM feeds WHERE instance_id=$1 AND slug=$2 AND format='go'`, p.InstanceID, target.slug).Scan(&feed)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		err = artifact.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	switch target.kind {
	case "bundle":
		if r.Method == "DELETE" {
			// GOPROXY has no delete verb (module authors retract via go.mod);
			// DELETE on the same /@v/{version} resource the bundle is PUT to is a
			// Kuvryn Ant management extension, policy-gated by the artifact core.
			if _, err := s.service.Delete(r.Context(), p, artifact.Target{FeedID: feed, Namespace: path.Dir(target.module), Coordinate: target.module, Version: target.version}); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(204)
			return
		}
		s.ingest(w, r, p, target, feed)
	case "list", "latest":
		s.index(w, r, p, target, feed)
	default:
		s.download(w, r, p, target.publication(feed, target.kind), "")
	}
}
func (s *Proxy) download(w http.ResponseWriter, r *http.Request, p identity.Principal, pub artifact.Publication, source string) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	record, err := s.service.Lookup(r.Context(), p, pub)
	if errors.Is(err, artifact.ErrNotFound) {
		// Pull-through of an immutable module file. The GOPROXY layout is the
		// same upstream; the version is already canonical (see parseRoute).
		origin, ok := upstreamFile(pub)
		if ok {
			if source != "" {
				origin.SourceID = source
			}
			stream, retry, through := s.service.PullThrough(r.Context(), p, withMediaType(pub), origin)
			switch {
			case through != nil:
				err = through
			case retry:
				record, err = s.service.Lookup(r.Context(), p, pub)
			default:
				stream.ServeStream(w, r.Method)
				return
			}
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	if source != "" && source != record.SourceID {
		fail(w, artifact.ErrConflict)
		return
	}
	var reader io.ReadCloser
	if r.Method != "HEAD" {
		var current artifact.Record
		current, reader, err = s.service.Open(r.Context(), p, pub, 0, -1)
		if err != nil {
			fail(w, err)
			return
		}
		defer reader.Close()
		if current.ID != record.ID || current.SourceID != record.SourceID || current.Info != record.Info {
			fail(w, artifact.ErrConflict)
			return
		}
	}
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(record.Info.Size, 10))
	w.Header().Set("ETag", strconv.Quote(string(record.Info.Digest)))
	w.WriteHeader(200)
	if reader != nil {
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_, _ = io.CopyBuffer(&progressWriter{w, controller}, reader, make([]byte, 64<<10))
	}
}
func fail(w http.ResponseWriter, err error) {
	status, message := 503, "Go proxy unavailable"
	switch {
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, blob.ErrNotFound):
		status, message = 404, "module not found"
	case errors.Is(err, artifact.ErrDenied):
		status, message = 403, "module access denied"
	case errors.Is(err, artifact.ErrConflict):
		status, message = 409, "immutable module conflicts"
	case errors.Is(err, artifact.ErrInput), errors.Is(err, blob.ErrIntegrity):
		status, message = 400, "invalid module bundle or request"
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Error(w, message, status)
}

// Progress deadlines replace the control-plane server's fixed timeout while
// preserving a bounded idle interval for module transfers.
type progressWriter struct {
	writer     io.Writer
	controller *http.ResponseController
}

func (w *progressWriter) Write(data []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return w.writer.Write(data)
}

type progressReader struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (r *progressReader) Read(data []byte) (int, error) {
	_ = r.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	return r.ReadCloser.Read(data)
}

func failure404(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	http.Error(w, "not found: not a module path or version", 404)
}
