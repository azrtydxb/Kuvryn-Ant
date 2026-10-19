package replication

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/management"
	"github.com/jackc/pgx/v5/pgxpool"
)

const exportBase = "/api/v1/replication/export/feeds/{feed}"

// RegisterExport mounts the peer-facing export API. Every route requires a
// bearer token whose scopes and role grants permit "replicate" on the feed.
//
//	GET {base}/head
//	GET {base}/manifest?kind=asset|tag&after=&limit=&include_cached=&namespace=&package=&version_prefix=&source=
//	GET {base}/changes?after=&limit=&include_cached=&namespace=&package=&version_prefix=   (410 below the low-water mark)
//	GET {base}/blobs/{digest}   (single Range supported)
//	GET {base}/template         (non-secret feed configuration template)
func RegisterExport(mux *http.ServeMux, pool *pgxpool.Pool, e Exporter) {
	bearer := identity.NewBearer(identity.Tokens{DB: pool})
	roles := identity.Roles{DB: pool}
	h := exportHandlers{e: e, authz: func(w http.ResponseWriter, r *http.Request) (string, string, bool) {
		return bearerAuthorize(roles, w, r)
	}}
	mux.Handle("GET "+exportBase+"/head", bearer.Wrap(http.HandlerFunc(h.head)))
	mux.Handle("GET "+exportBase+"/manifest", bearer.Wrap(http.HandlerFunc(h.manifest)))
	mux.Handle("GET "+exportBase+"/changes", bearer.Wrap(http.HandlerFunc(h.changes)))
	mux.Handle("GET "+exportBase+"/blobs/{digest}", bearer.Wrap(http.HandlerFunc(h.blob)))
	mux.Handle("GET "+exportBase+"/template", bearer.Wrap(http.HandlerFunc(h.template)))
}

type exportHandlers struct {
	e Exporter
	// authz authenticates and authorizes "replicate" on the feed and returns
	// the exporting instance and the feed.
	authz func(w http.ResponseWriter, r *http.Request) (instance, feed string, ok bool)
}

func (h exportHandlers) authorize(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	return h.authz(w, r)
}

func bearerAuthorize(roles identity.Roles, w http.ResponseWriter, r *http.Request) (string, string, bool) {
	p, ok := identity.PrincipalFromContext(r.Context())
	feed := r.PathValue("feed")
	if !ok || !uuidPattern.MatchString(feed) {
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "feed not found")
		return "", "", false
	}
	policy, err := roles.Resolve(r.Context(), p)
	if err != nil {
		management.Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "authorization unavailable")
		return "", "", false
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Replicate) {
		management.Error(w, r, http.StatusForbidden, api.ErrorCodeForbidden, "replication access denied")
		return "", "", false
	}
	return p.InstanceID, feed, true
}

// FederationExportPrefix is the export API path on the federation listener.
const FederationExportPrefix = "/federation/v1/replication/export/feeds/"

// FederationExport serves the export API on the federation mTLS listener for
// federation-mode peers (same handlers and wire format as the bearer API).
// The caller is already an authenticated, active member (peer.Handler);
// authorize decides from the applied, authority-signed topology.
func FederationExport(e Exporter, instance string, authorize func(ctx context.Context, member, feed string) bool) http.Handler {
	h := exportHandlers{e: e, authz: func(w http.ResponseWriter, r *http.Request) (string, string, bool) {
		caller, ok := peer.Caller(r)
		feed := r.PathValue("feed")
		if !ok || !uuidPattern.MatchString(feed) {
			management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "feed not found")
			return "", "", false
		}
		if !authorize(r.Context(), caller.InstanceID, feed) {
			management.Error(w, r, http.StatusForbidden, api.ErrorCodeForbidden, "replication access denied")
			return "", "", false
		}
		return instance, feed, true
	}}
	mux := http.NewServeMux()
	base := FederationExportPrefix + "{feed}"
	mux.HandleFunc("GET "+base+"/head", h.head)
	mux.HandleFunc("GET "+base+"/manifest", h.manifest)
	mux.HandleFunc("GET "+base+"/changes", h.changes)
	mux.HandleFunc("GET "+base+"/blobs/{digest}", h.blob)
	mux.HandleFunc("GET "+base+"/template", h.template)
	return mux
}
func exportError(w http.ResponseWriter, r *http.Request, err error) {
	var purged *HistoryPurgedError
	switch {
	case errors.As(err, &purged):
		// 410 tells a consumer its cursor can no longer be served: it must
		// reseed from the manifest. The mark is in a header for the client.
		w.Header().Set("X-Replication-Low-Water", strconv.FormatInt(purged.LowWater, 10))
		management.Error(w, r, http.StatusGone, api.ErrorCodeConflict, "change history purged below cursor; reseed required")
	case errors.Is(err, ErrInput):
		management.Error(w, r, http.StatusBadRequest, api.ErrorCodeInvalidRequest, "invalid replication request")
	case errors.Is(err, ErrNotFound):
		management.Error(w, r, http.StatusNotFound, api.ErrorCodeNotFound, "not found")
	case errors.Is(err, blob.ErrIntegrity):
		management.Error(w, r, http.StatusInternalServerError, api.ErrorCodeInternalError, "source blob failed integrity verification")
	default:
		management.Error(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "replication export unavailable")
	}
}
func queryInt(r *http.Request, name string, def int64) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= 0
}
func includeCached(r *http.Request) bool { return r.URL.Query().Get("include_cached") != "false" }

func (h exportHandlers) head(w http.ResponseWriter, r *http.Request) {
	instance, feed, ok := h.authorize(w, r)
	if !ok {
		return
	}
	head, err := h.e.Head(r.Context(), instance, feed)
	if err != nil {
		exportError(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, head)
}
func (h exportHandlers) manifest(w http.ResponseWriter, r *http.Request) {
	instance, feed, ok := h.authorize(w, r)
	if !ok {
		return
	}
	limit, valid := queryInt(r, "limit", 0)
	q := r.URL.Query()
	if !valid || limit > maxPage || len(q.Get("after")) > 2048 {
		exportError(w, r, ErrInput)
		return
	}
	page, err := h.e.Manifest(r.Context(), instance, feed, ManifestQuery{
		Kind: q.Get("kind"), After: q.Get("after"), Limit: int(limit), IncludeCached: includeCached(r), SourceID: q.Get("source"),
		Filters: Filters{Namespaces: q["namespace"], Packages: q["package"], VersionPrefixes: q["version_prefix"]},
	})
	if err != nil {
		exportError(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, page)
}
func (h exportHandlers) changes(w http.ResponseWriter, r *http.Request) {
	instance, feed, ok := h.authorize(w, r)
	if !ok {
		return
	}
	after, v1 := queryInt(r, "after", 0)
	limit, v2 := queryInt(r, "limit", 0)
	if !v1 || !v2 || limit > maxPage {
		exportError(w, r, ErrInput)
		return
	}
	q := r.URL.Query()
	page, err := h.e.Changes(r.Context(), instance, feed, ChangeQuery{
		After: after, Limit: int(limit), IncludeCached: includeCached(r),
		Filters: Filters{Namespaces: q["namespace"], Packages: q["package"], VersionPrefixes: q["version_prefix"]},
	})
	if err != nil {
		exportError(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, page)
}
func (h exportHandlers) blob(w http.ResponseWriter, r *http.Request) {
	instance, feed, ok := h.authorize(w, r)
	if !ok {
		return
	}
	offset, length, ranged, valid := parseRange(r.Header.Get("Range"))
	if !valid {
		w.Header().Set("Content-Range", "bytes */*")
		management.Error(w, r, http.StatusRequestedRangeNotSatisfiable, api.ErrorCodeInvalidRequest, "unsupported range")
		return
	}
	info, body, err := h.e.OpenBlob(r.Context(), instance, feed, r.PathValue("digest"), includeCached(r), offset, length)
	if err != nil {
		exportError(w, r, err)
		return
	}
	defer body.Close()
	// Both listeners have short write timeouts (10 s on the main server). A blob
	// stream may run far longer over a slow peer link, so the deadline moves
	// forward with every write instead: a transfer that keeps making progress
	// completes, one that stalls for blobWriteIdle is cut off.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Digest", "sha256:"+string(info.Digest))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "no-store")
	if offset > info.Size {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	n := info.Size - offset
	if length >= 0 && length < n {
		n = length
	}
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	if ranged {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+n-1, 10)+"/"+strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusPartialContent)
	}
	streamBlob(w, body)
}

// blobWriteIdle is how long a blob response may go without a successful write.
var blobWriteIdle = 60 * time.Second

// streamBlob copies body to w, moving the write deadline forward on every write.
func streamBlob(w http.ResponseWriter, body io.Reader) {
	c := http.NewResponseController(w)
	_ = c.SetWriteDeadline(time.Now().Add(blobWriteIdle))
	_, _ = io.CopyBuffer(&progressWriter{w: w, c: c}, body, make([]byte, 64<<10))
}

// progressWriter extends the write deadline before every write.
type progressWriter struct {
	w io.Writer
	c *http.ResponseController
}

func (p *progressWriter) Write(data []byte) (int, error) {
	_ = p.c.SetWriteDeadline(time.Now().Add(blobWriteIdle))
	return p.w.Write(data)
}

// parseRange accepts a single "bytes=start-" or "bytes=start-end" range.
func parseRange(v string) (offset, length int64, ranged, valid bool) {
	if v == "" {
		return 0, -1, false, true
	}
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok || a == "" {
		return 0, 0, false, false
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	if b == "" {
		return start, -1, true, true
	}
	end, err := strconv.ParseInt(b, 10, 64)
	if err != nil || end < start {
		return 0, 0, false, false
	}
	return start, end - start + 1, true, true
}

func (h exportHandlers) template(w http.ResponseWriter, r *http.Request) {
	instance, feed, ok := h.authorize(w, r)
	if !ok {
		return
	}
	t, err := h.e.Template(r.Context(), instance, feed)
	if err != nil {
		exportError(w, r, err)
		return
	}
	management.JSON(w, http.StatusOK, t)
}
