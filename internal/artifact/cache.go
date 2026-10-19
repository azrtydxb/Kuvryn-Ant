package artifact

// Cache feeds (feed purpose "cache": buildx registry layer cache, sccache over
// WebDAV). The core owns the three behaviours that differ from ordinary feeds:
//
//   - an OCI tag may move, and the manifest it leaves is released together with
//     the layers nothing else references (see afterCachePublish);
//   - content is deleted by prefix through CleanCache, the one operation CI
//     identities need (drop a branch's cache);
//   - released blobs get the short sweep wait, never an inline byte deletion:
//     the garbage collector's row-lock re-check protocol is unchanged.
//
// Eviction by idle time, size and storage pressure lives in internal/retention.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/cacherelease"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

const (
	// PurposeCache is the feeds.purpose value of a build-cache feed.
	PurposeCache = "cache"

	maxCleanBatch = 500
)

// ErrNotCache means a cache operation was addressed to a feed whose purpose is
// not "cache".
var ErrNotCache = fmt.Errorf("feed is not a cache feed: %w", ErrConflict)

func (s *Service) sweepGrace() time.Duration {
	if s.CacheSweepGrace > 0 {
		return s.CacheSweepGrace
	}
	return cacherelease.DefaultSweepGrace
}

func (s *Service) gcGrace() time.Duration {
	if s.GCGrace > 0 {
		return s.GCGrace
	}
	return cacherelease.DefaultGCGrace
}

func feedIsCache(ctx context.Context, q rowQuerier, instance, feed string) (bool, error) {
	var purpose string
	if err := q.QueryRow(ctx, `SELECT purpose FROM feeds WHERE instance_id=$1 AND id=$2`, instance, feed).Scan(&purpose); err != nil {
		return false, err
	}
	return purpose == PurposeCache, nil
}

func coordinateLock(ctx context.Context, tx pgx.Tx, instance, source, coordinate string) error {
	sum := sha256.Sum256([]byte(instance + "\x00" + source + "\x00" + coordinate))
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8])))
	return err
}

// afterCachePublish runs at the end of a publication inside its transaction.
// For a cache feed it records which versions a manifest names (so a later
// release knows what is still referenced), then releases the version a moved
// tag left behind. It does nothing for other feeds.
func (s *Service) afterCachePublish(ctx context.Context, tx pgx.Tx, p identity.Principal, r Publication, component, version, supersede string) error {
	if len(r.References) == 0 && supersede == "" {
		return nil
	}
	cache, err := feedIsCache(ctx, tx, p.InstanceID, r.FeedID)
	if err != nil {
		return ErrUnavailable
	}
	if !cache {
		return nil
	}
	for _, ref := range r.References {
		if ref.Version == "" || ref.Version == r.Version {
			continue
		}
		if _, err = tx.Exec(ctx, `INSERT INTO version_refs(component_id,parent_version_id,ref_version) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, component, version, ref.Version); err != nil {
			return dbErr(err)
		}
	}
	if supersede == "" {
		return nil
	}
	res, err := cacherelease.Unreferenced(ctx, tx, p.InstanceID, component, []string{supersede}, s.sweepGrace(), s.gcGrace())
	if err != nil {
		return dbErr(err)
	}
	if err = cacheAudit(ctx, tx, p, "artifact.cache.supersede", component, map[string]any{"feed_id": r.FeedID, "coordinate": r.Coordinate, "tag": r.Tag, "versions_released": res.Versions, "bytes_released": res.Bytes}); err != nil {
		return err
	}
	return nil
}

func cacheAudit(ctx context.Context, tx pgx.Tx, p identity.Principal, action, resource string, details map[string]any) error {
	encoded, err := json.Marshal(details)
	if err != nil {
		return ErrInput
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,$3,$4,$5::jsonb) RETURNING id`, p.InstanceID, p.PrincipalID, action, resource, string(encoded)).Scan(&audit); err != nil {
		return dbErr(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,$3::jsonb)`, audit, action, string(encoded)); err != nil {
		return dbErr(err)
	}
	return nil
}

// CacheClean selects what CleanCache removes. Prefix is required.
//
// For an OCI feed Prefix is "<repository>[:<tag prefix>]": the tags of that
// repository that start with the tag prefix, or every tag of the repository
// when there is no colon. For a raw feed it is a path prefix: every file whose
// path starts with it.
type CacheClean struct {
	FeedID, Prefix string
}

// CacheCleanResult reports a cleanup. Truncated means the batch limit stopped
// it; repeating the call continues (and a finished cleanup is a no-op).
type CacheCleanResult struct {
	Matched    int64 // repositories (OCI) or files (raw) that matched
	Tags       int64 // tags removed
	Versions   int64 // versions released (manifests, layers, files)
	Components int64 // repositories or files left empty and removed
	Bytes      int64 // bytes queued for the short sweep
	Truncated  bool
}

// CleanCache removes cache content by prefix from a cache feed. The principal
// needs delete on the feed's artifacts (a scoped CI token can hold exactly
// that), the feed must be a cache feed, and the call is idempotent. Removal is
// metadata only: freed blobs are deleted by the garbage collector after the
// short sweep wait, under its usual row-lock re-check.
func (s *Service) CleanCache(ctx context.Context, p identity.Principal, c CacheClean) (CacheCleanResult, error) {
	if s == nil || s.DB == nil || s.Lease == nil || p.PrincipalID == "" || !bounded(c.FeedID, 36) || !bounded(c.Prefix, 512) {
		return CacheCleanResult{}, ErrInput
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if errors.Is(err, identity.ErrUnauthenticated) {
		return CacheCleanResult{}, ErrDenied
	}
	if err != nil {
		return CacheCleanResult{}, ErrUnavailable
	}
	feedRes := identity.Resource{InstanceID: p.InstanceID, FeedID: c.FeedID, Kind: identity.ArtifactResource}
	var purpose, format string
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = s.DB.QueryRow(qctx, `SELECT purpose,format FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, c.FeedID).Scan(&purpose, &format)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		return CacheCleanResult{}, ErrNotFound
	}
	if err != nil {
		return CacheCleanResult{}, ErrUnavailable
	}
	// A principal that cannot read the feed learns nothing about it.
	if !policy.Authorize(p, feedRes, identity.Read) {
		return CacheCleanResult{}, ErrNotFound
	}
	if !policy.Authorize(p, feedRes, identity.Delete) {
		return CacheCleanResult{}, ErrDenied
	}
	if purpose != PurposeCache {
		return CacheCleanResult{}, ErrNotCache
	}
	dp, err := EffectiveDeletePolicy(ctx, s.DB, p.InstanceID, c.FeedID, "")
	if err != nil {
		return CacheCleanResult{}, err
	}
	if dp.Mode == DeleteDenied {
		return CacheCleanResult{}, ErrPolicy
	}
	if err = frozenFeed(ctx, s.DB, p.InstanceID, c.FeedID); err != nil {
		return CacheCleanResult{}, err
	}

	oci := format == "oci" || format == "oci-artifacts"
	repo, tagPrefix := c.Prefix, ""
	if oci {
		repo, tagPrefix, _ = strings.Cut(c.Prefix, ":")
		if repo == "" {
			return CacheCleanResult{}, ErrInput
		}
	}
	type target struct{ component, source, coordinate, namespace string }
	var targets []target
	var rows pgx.Rows
	if oci {
		rows, err = s.DB.Query(ctx, `SELECT c.id::text,c.source_id::text,c.coordinate,c.namespace FROM feed_sources fs JOIN sources so ON so.id=fs.source_id AND so.instance_id=fs.instance_id AND so.kind='local' JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY c.id LIMIT $4`, p.InstanceID, c.FeedID, repo, maxCleanBatch+1)
	} else {
		rows, err = s.DB.Query(ctx, `SELECT c.id::text,c.source_id::text,c.coordinate,c.namespace FROM feed_sources fs JOIN sources so ON so.id=fs.source_id AND so.instance_id=fs.instance_id AND so.kind='local' JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND starts_with(c.coordinate,$3) ORDER BY c.id LIMIT $4`, p.InstanceID, c.FeedID, repo, maxCleanBatch+1)
	}
	if err != nil {
		return CacheCleanResult{}, ErrUnavailable
	}
	for rows.Next() {
		var t target
		if err = rows.Scan(&t.component, &t.source, &t.coordinate, &t.namespace); err != nil {
			rows.Close()
			return CacheCleanResult{}, ErrUnavailable
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return CacheCleanResult{}, ErrUnavailable
	}
	var out CacheCleanResult
	if len(targets) > maxCleanBatch {
		targets, out.Truncated = targets[:maxCleanBatch], true
	}
	for _, t := range targets {
		res := identity.Resource{InstanceID: p.InstanceID, FeedID: c.FeedID, SourceID: t.source, Namespace: t.namespace, Kind: identity.ArtifactResource}
		if !policy.Authorize(p, res, identity.Delete) {
			continue
		}
		var one componentClean
		for attempt := 0; ; attempt++ {
			one, err = s.cleanComponent(ctx, p, c, t.component, t.source, t.coordinate, tagPrefix, oci)
			if err == nil || attempt >= 4 || !transientDB(err) || ctx.Err() != nil {
				break
			}
			time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
		}
		if err != nil {
			return out, err
		}
		if one.gone || (one.tags == 0 && one.release.Versions == 0) {
			continue
		}
		out.Matched++
		out.Tags += one.tags
		out.Versions += one.release.Versions
		out.Components += one.release.Components
		out.Bytes += one.release.Bytes
		if s.Changed != nil && (one.tags > 0 || one.release.Versions > 0) {
			s.Changed(Change{Action: ActionDelete, FeedID: c.FeedID, SourceID: t.source, Namespace: t.namespace, Coordinate: t.coordinate})
		}
	}
	// One audit event per call, including a call that found nothing, so a
	// cleanup is traceable to the principal that asked for it.
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, dbErr(err)
	}
	defer rollback(tx)
	if err = cacheAudit(ctx, tx, p, "cache.clean", c.FeedID, map[string]any{"feed_id": c.FeedID, "prefix": c.Prefix, "matched": out.Matched, "tags_removed": out.Tags, "versions_removed": out.Versions, "components_removed": out.Components, "bytes_released": out.Bytes, "truncated": out.Truncated}); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, dbErr(err)
	}
	return out, nil
}

type componentClean struct {
	tags    int64
	release cacherelease.Result
	gone    bool
}

// cleanComponent removes the matching tags (OCI) or every version (raw) of one
// component under the coordinate's advisory lock.
func (s *Service) cleanComponent(ctx context.Context, p identity.Principal, c CacheClean, component, source, coordinate, tagPrefix string, oci bool) (componentClean, error) {
	var out componentClean
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, dbErr(err)
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return out, ErrDenied
	}
	if err = ownership.Require(ctx, tx, lease); err != nil {
		return out, err
	}
	if err = coordinateLock(ctx, tx, p.InstanceID, source, coordinate); err != nil {
		return out, dbErr(err)
	}
	var still string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM components WHERE id=$1 FOR UPDATE`, component).Scan(&still); errors.Is(err, pgx.ErrNoRows) {
		out.gone = true
		return out, nil
	} else if err != nil {
		return out, dbErr(err)
	}
	var candidates []string
	if oci {
		rows, qerr := tx.Query(ctx, `DELETE FROM tags WHERE component_id=$1 AND starts_with(name,$2) RETURNING version_id::text`, component, tagPrefix)
		if qerr != nil {
			return out, dbErr(qerr)
		}
		seen := map[string]bool{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return out, dbErr(err)
			}
			out.tags++
			if !seen[id] {
				seen[id] = true
				candidates = append(candidates, id)
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return out, dbErr(err)
		}
	} else {
		rows, qerr := tx.Query(ctx, `SELECT id::text FROM versions WHERE component_id=$1`, component)
		if qerr != nil {
			return out, dbErr(qerr)
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return out, dbErr(err)
			}
			candidates = append(candidates, id)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return out, dbErr(err)
		}
	}
	if out.release, err = cacherelease.Unreferenced(ctx, tx, p.InstanceID, component, candidates, s.sweepGrace(), s.gcGrace()); err != nil {
		return out, dbErr(err)
	}
	if out.tags == 0 && out.release.Versions == 0 {
		// Nothing changed: this component only matched by name.
		return out, nil
	}
	if err = cacheAudit(ctx, tx, p, "cache.clean.component", component, map[string]any{"feed_id": c.FeedID, "coordinate": coordinate, "tag_prefix": tagPrefix, "tags_removed": out.tags, "versions_removed": out.release.Versions}); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, dbErr(err)
	}
	return out, nil
}
