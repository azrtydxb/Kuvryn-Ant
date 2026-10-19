package management

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/jackc/pgx/v5"
)

// CacheCleanResponse is the result of DELETE /api/v1/feeds/{id}/cache.
type CacheCleanResponse struct {
	FeedID            string `json:"feed_id"`
	Prefix            string `json:"prefix"`
	Matched           int64  `json:"matched"`
	TagsRemoved       int64  `json:"tags_removed"`
	VersionsRemoved   int64  `json:"versions_removed"`
	ComponentsRemoved int64  `json:"components_removed"`
	BytesReleased     int64  `json:"bytes_released"`
	Truncated         bool   `json:"truncated"`
}

// cacheClean deletes cache content by prefix. Authorization, the cache-feed
// check, the delete policy and the audit event live in artifact.CleanCache, so
// a scoped service token (including one a GitHub OIDC trust rule issued) needs
// exactly the delete action on the feed.
func (h opsHandlers) cacheClean(w http.ResponseWriter, r *http.Request) {
	feed, ok := feedID(r.PathValue("id"))
	if !ok {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid feed ID")
		return
	}
	if !singleParams(w, r, "prefix") {
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix == "" || len(prefix) > 512 || !utf8.ValidString(prefix) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "prefix is required (1 to 512 bytes): <repository>[:<tag prefix>] for an OCI cache, a path prefix for a raw cache")
		return
	}
	p, _, ok := h.policy(w, r)
	if !ok {
		return
	}
	var svc *artifact.Service
	if h.d.Artifacts != nil {
		svc = h.d.Artifacts(p.InstanceID)
	}
	if svc == nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "this instance is not serving artifacts")
		return
	}
	res, err := svc.CleanCache(r.Context(), p, artifact.CacheClean{FeedID: feed, Prefix: prefix})
	switch {
	case err == nil:
	case errors.Is(err, artifact.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		Error(w, r, 404, api.ErrorCodeNotFound, "feed not found")
		return
	case errors.Is(err, artifact.ErrDenied):
		Error(w, r, 403, api.ErrorCodeForbidden, "deleting cache content requires the delete action on this feed")
		return
	case errors.Is(err, artifact.ErrNotCache):
		Error(w, r, 409, api.ErrorCodeConflict, "the feed is not a cache feed (purpose cache)")
		return
	case errors.Is(err, artifact.ErrInput):
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid cache cleanup request")
		return
	case errors.Is(err, artifact.ErrConflict):
		Error(w, r, 409, api.ErrorCodeConflict, err.Error())
		return
	default:
		var transferring *artifact.FeedTransferringError
		if errors.As(err, &transferring) {
			Error(w, r, 503, api.ErrorCodeUnavailable, "the feed is transferring")
			return
		}
		Error(w, r, 503, api.ErrorCodeUnavailable, "cache cleanup unavailable")
		return
	}
	JSON(w, 200, CacheCleanResponse{FeedID: feed, Prefix: prefix, Matched: res.Matched, TagsRemoved: res.Tags, VersionsRemoved: res.Versions,
		ComponentsRemoved: res.Components, BytesReleased: res.Bytes, Truncated: res.Truncated})
}

// CacheFeedUsage is the storage a cache feed holds, with its limits, for the
// retention overview.
type CacheFeedUsage struct {
	FeedID         string `json:"feed_id"`
	Slug           string `json:"slug"`
	Format         string `json:"format"`
	Bytes          int64  `json:"bytes"`
	Versions       int64  `json:"versions"`
	MaxIdleSeconds int64  `json:"max_idle_seconds"`
	MaxBytes       *int64 `json:"max_bytes,omitempty"`
}
