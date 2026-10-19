package management

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Feed purposes. "artifacts" is ordinary published content; "cache" is a build
// cache (buildx registry layer cache, sccache over WebDAV): evictable, never
// replicated, hidden from package search by default (see internal/retention).
const (
	PurposeArtifacts = "artifacts"
	PurposeCache     = "cache"

	// DefaultCacheMaxIdleSeconds is the idle time after which cache content is evicted.
	DefaultCacheMaxIdleSeconds = int64(7 * 24 * time.Hour / time.Second)
	maxCacheMaxIdleSeconds     = int64(10 * 365 * 24 * time.Hour / time.Second)
	minCacheMaxIdleSeconds     = int64(60)
)

// CachePolicy is the per-feed cache policy of a cache feed. MaxBytes is omitted
// when the feed has no size limit.
type CachePolicy struct {
	MaxIdleSeconds int64  `json:"max_idle_seconds"`
	MaxBytes       *int64 `json:"max_bytes,omitempty"`
}

// CachePolicyInput is the writable form. A zero MaxBytes removes the size limit.
type CachePolicyInput struct {
	MaxIdleSeconds *int64 `json:"max_idle_seconds,omitempty"`
	MaxBytes       *int64 `json:"max_bytes,omitempty"`
}

// cacheFormat reports whether a feed of this format may be a cache feed.
func cacheFormat(format string) bool { return format == "oci" || format == "raw" }

func validPurpose(purpose string) bool { return purpose == PurposeArtifacts || purpose == PurposeCache }

// validateCacheInput checks purpose, format and policy values for a create or
// a patch. purpose is the resulting purpose; format the feed's format.
func validateCacheInput(purpose, format string, in *CachePolicyInput) string {
	if !validPurpose(purpose) {
		return "purpose must be artifacts or cache"
	}
	if purpose == PurposeCache && !cacheFormat(format) {
		return "cache feeds support only the oci and raw formats"
	}
	if in == nil {
		return ""
	}
	if purpose != PurposeCache {
		return "cache_policy applies only to feeds with purpose cache"
	}
	if in.MaxIdleSeconds != nil && (*in.MaxIdleSeconds < minCacheMaxIdleSeconds || *in.MaxIdleSeconds > maxCacheMaxIdleSeconds) {
		return fmt.Sprintf("cache_policy.max_idle_seconds must be between %d and %d", minCacheMaxIdleSeconds, maxCacheMaxIdleSeconds)
	}
	if in.MaxBytes != nil && *in.MaxBytes < 0 {
		return "cache_policy.max_bytes must not be negative (0 removes the limit)"
	}
	return ""
}

const feedColumns = `id,slug,format,revision,anonymous_read,purpose,cache_max_idle_seconds,cache_max_bytes`

// scanFeed reads the columns of feedColumns.
func scanFeed(row rowScanner, item *Feed) error {
	var idle int64
	var maxBytes *int64
	if err := row.Scan(&item.ID, &item.Slug, &item.Format, &item.Revision, &item.AnonymousRead, &item.Purpose, &idle, &maxBytes); err != nil {
		return err
	}
	item.CachePolicy = nil
	if item.Purpose == PurposeCache {
		item.CachePolicy = &CachePolicy{MaxIdleSeconds: idle, MaxBytes: maxBytes}
	}
	return nil
}

// cachePolicyValues resolves the stored columns for a feed being written.
func cachePolicyValues(in *CachePolicyInput, idle int64, maxBytes *int64) (int64, *int64) {
	if in == nil {
		return idle, maxBytes
	}
	if in.MaxIdleSeconds != nil {
		idle = *in.MaxIdleSeconds
	}
	if in.MaxBytes != nil {
		if *in.MaxBytes == 0 {
			maxBytes = nil
		} else {
			v := *in.MaxBytes
			maxBytes = &v
		}
	}
	return idle, maxBytes
}

// checkCacheSourcePlacement enforces that cache content never shares a source
// with release content: a cache feed may attach only local sources that no
// non-cache feed uses, and a non-cache feed may not attach a source that a
// cache feed uses. Retention treats every source of a cache feed as evictable,
// so mixing would make release content evictable.
func checkCacheSourcePlacement(ctx context.Context, tx pgx.Tx, instance, feed, purpose, source string) (string, error) {
	var kind string
	if err := tx.QueryRow(ctx, `SELECT kind FROM sources WHERE instance_id=$1 AND id=$2`, instance, source).Scan(&kind); err != nil {
		return "", err
	}
	if purpose == PurposeCache {
		if kind != "local" {
			return "a cache feed accepts only local sources", nil
		}
		var other bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN feeds f ON f.instance_id=fs.instance_id AND f.id=fs.feed_id WHERE fs.instance_id=$1 AND fs.source_id=$2 AND fs.feed_id<>$3 AND f.purpose<>'cache')`, instance, source, feed).Scan(&other); err != nil {
			return "", err
		}
		if other {
			return "the source is also attached to a feed that is not a cache feed; cache content must not share a source with release content", nil
		}
		return "", nil
	}
	var cached bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN feeds f ON f.instance_id=fs.instance_id AND f.id=fs.feed_id WHERE fs.instance_id=$1 AND fs.source_id=$2 AND fs.feed_id<>$3 AND f.purpose='cache')`, instance, source, feed).Scan(&cached); err != nil {
		return "", err
	}
	if cached {
		return "the source belongs to a cache feed and cannot be attached to a feed that is not a cache feed", nil
	}
	return "", nil
}

// checkPurposeChange validates turning an artifacts feed into a cache feed.
// It returns a human-readable reason when the change is not allowed.
func checkPurposeChange(ctx context.Context, tx pgx.Tx, instance, feed string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT source_id::text FROM feed_sources WHERE instance_id=$1 AND feed_id=$2`, instance, feed)
	if err != nil {
		return "", err
	}
	var sources []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			rows.Close()
			return "", err
		}
		sources = append(sources, s)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return "", err
	}
	for _, s := range sources {
		if reason, e := checkCacheSourcePlacement(ctx, tx, instance, feed, PurposeCache, s); e != nil || reason != "" {
			return reason, e
		}
	}
	var replicated bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM replication_jobs WHERE instance_id=$1 AND dest_feed_id=$2)
 OR EXISTS(SELECT 1 FROM federated_feed_bindings WHERE instance_id=$1 AND feed_id=$2)
 OR EXISTS(SELECT 1 FROM federation_home_feeds WHERE instance_id=$1 AND feed_id=$2)`, instance, feed).Scan(&replicated)
	if err != nil {
		return "", err
	}
	if replicated {
		return "the feed takes part in replication or federation (replication job, federation binding or home feed); cache feeds are never replicated", nil
	}
	return "", nil
}

// applyCacheColumns writes purpose and the cache policy for a feed row already
// locked by the caller.
func applyCacheColumns(ctx context.Context, tx pgx.Tx, instance, feed, purpose string, idle int64, maxBytes *int64) error {
	_, err := tx.Exec(ctx, `UPDATE feeds SET purpose=$3,cache_max_idle_seconds=$4,cache_max_bytes=$5 WHERE instance_id=$1 AND id=$2`, instance, feed, purpose, idle, maxBytes)
	return err
}

// purposeChangeAllowed reports whether the principal may change a feed's purpose.
func purposeChangeAllowed(policy identity.Policy, p identity.Principal, feed string) bool {
	return feedAllowed(policy, p, feed, identity.Admin)
}

func rejectCacheInput(w http.ResponseWriter, r *http.Request, msg string) bool {
	if msg == "" {
		return false
	}
	Error(w, r, 400, api.ErrorCodeInvalidRequest, msg)
	return true
}

// feedPurposeAudit records a purpose change with its old and new values.
func feedPurposeAudit(ctx context.Context, tx pgx.Tx, p identity.Principal, item Feed, from string) error {
	details := map[string]any{"from": from, "to": item.Purpose, "feed_id": item.ID, "slug": item.Slug}
	if item.CachePolicy != nil {
		details["cache_policy"] = item.CachePolicy
	}
	return auditTx(ctx, tx, p, "feed.purpose.change", item.ID, details, map[string]any{"feed_id": item.ID, "from": from, "to": item.Purpose, "revision": item.Revision})
}
