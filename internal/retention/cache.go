package retention

// Cache feeds (feed purpose "cache": buildx registry layer cache, sccache).
//
// Semantics, in contrast with release content:
//
//   - everything published to a cache feed's local source is evictable. It never
//     needs allow_release_delete, tagged versions are not exempt and min_keep
//     does not apply; ordinary retention policies are ignored for cache sources;
//   - the per-feed cache policy has max_idle (idle is the latest of created,
//     last accessed and last downloaded; default 7 days) and optionally
//     max_bytes (evict least recently used until the feed is under it);
//   - under storage pressure cache feeds are evicted first, least recently used
//     first, before any remote-cached content;
//   - blobs freed here get the short sweep wait (ANT_PRESSURE_SWEEP_GRACE)
//     instead of the full grace. The sweep itself is unchanged: it locks the
//     blobs row, re-checks references and only then deletes.
//
// Eviction works on "roots": versions no manifest names (an OCI image or index
// that is not a layer or child of another manifest; every file of a raw cache).
// A root's layers go with it, when nothing else references them
// (cacherelease.Unreferenced), and a layer is never evicted on its own while a
// live manifest still names it, so a manifest that is still in use never loses a
// layer to idleness.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/cacherelease"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/jackc/pgx/v5"
)

const (
	cacheScanLimit   = 2000 // roots examined per stage and run
	cacheRoundLimit  = 40   // size-limit rounds per feed and run
	cacheRoundSize   = 100  // roots evicted per size-limit round
	cacheReasonIdle  = "cache_idle"
	cacheReasonSize  = "cache_max_bytes"
	cacheReasonPress = "pressure_lru"
)

// CacheFeed is one cache feed with its policy.
type CacheFeed struct {
	ID       string
	Slug     string
	MaxIdle  time.Duration
	MaxBytes int64 // 0 = no size limit
	Sources  []string
}

// cacheFeeds lists the cache feeds with their local sources. A source also
// attached to a feed that is not a cache feed is never treated as cache
// content (the management API refuses such an attachment; this is the guard if
// it ever happens).
func (s *Service) cacheFeeds(ctx context.Context) ([]CacheFeed, error) {
	rows, err := s.DB.Query(ctx, `SELECT f.id::text,f.slug,f.cache_max_idle_seconds,COALESCE(f.cache_max_bytes,0),fs.source_id::text
 FROM feeds f JOIN feed_sources fs ON fs.instance_id=f.instance_id AND fs.feed_id=f.id
 JOIN sources so ON so.instance_id=fs.instance_id AND so.id=fs.source_id AND so.kind='local'
 WHERE f.instance_id=$1 AND f.purpose='cache'
  AND NOT EXISTS(SELECT 1 FROM feed_sources fs2 JOIN feeds f2 ON f2.instance_id=fs2.instance_id AND f2.id=fs2.feed_id WHERE fs2.source_id=fs.source_id AND f2.purpose<>'cache')
 ORDER BY f.id,fs.priority`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheFeed
	for rows.Next() {
		var id, slug, src string
		var idle, maxBytes int64
		if err = rows.Scan(&id, &slug, &idle, &maxBytes, &src); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != id {
			out = append(out, CacheFeed{ID: id, Slug: slug, MaxIdle: time.Duration(idle) * time.Second, MaxBytes: maxBytes})
		}
		out[len(out)-1].Sources = append(out[len(out)-1].Sources, src)
	}
	return out, rows.Err()
}

// cacheSourceIdle maps each cache source to the strictest max_idle of the cache
// feeds it is attached to.
func cacheSourceIdle(feeds []CacheFeed) map[string]time.Duration {
	out := map[string]time.Duration{}
	for _, f := range feeds {
		for _, src := range f.Sources {
			if cur, ok := out[src]; !ok || f.MaxIdle < cur {
				out[src] = f.MaxIdle
			}
		}
	}
	return out
}

// cacheSourceSet returns the ids of all cache sources.
func (s *Service) cacheSourceSet(ctx context.Context) (map[string]bool, error) {
	feeds, err := s.cacheFeeds(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for src := range cacheSourceIdle(feeds) {
		out[src] = true
	}
	return out, nil
}

// cachePick is a root version chosen for eviction.
type cachePick struct {
	VersionID, ComponentID, SourceID, Coordinate, Version string
	LastUsed                                              time.Time
	Bytes                                                 int64 // estimated bytes of the root and what it names
}

// pickCacheRoots returns root versions of the given cache sources, least
// recently used first. With idleOnly only roots idle for longer than their
// source's max_idle are returned.
func (s *Service) pickCacheRoots(ctx context.Context, sources map[string]time.Duration, idleOnly bool, limit int) ([]cachePick, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	// A blob-only root a remaining manifest still names is never a candidate, so
	// page past such rows instead of letting them crowd out real candidates.
	var picks []cachePick
	var after *time.Time
	var afterID *string
	for page := 0; page < 20 && len(picks) < limit; page++ {
		rows, blobOnly, err := s.pickCacheRootsPage(ctx, sources, idleOnly, limit, after, afterID)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		last := rows[len(rows)-1]
		after, afterID = &last.LastUsed, &last.VersionID
		protected := map[string]bool{}
		byComponent := map[string]map[string]bool{}
		for _, c := range rows {
			if blobOnly[c.VersionID] {
				if byComponent[c.ComponentID] == nil {
					byComponent[c.ComponentID] = map[string]bool{}
				}
				byComponent[c.ComponentID][c.VersionID] = true
			}
		}
		for component, doomed := range byComponent {
			keep, err := s.protectedBlobs(ctx, s.DB, component, doomed)
			if err != nil {
				return nil, err
			}
			for id := range keep {
				protected[id] = true
			}
		}
		for _, c := range rows {
			if !protected[c.VersionID] && len(picks) < limit {
				picks = append(picks, c)
			}
		}
		if len(rows) < limit {
			break
		}
	}
	if len(picks) == 0 {
		return nil, nil
	}
	versionIDs := make([]string, len(picks))
	for i, c := range picks {
		versionIDs[i] = c.VersionID
	}
	closure, err := s.closureBytes(ctx, versionIDs)
	if err != nil {
		return nil, err
	}
	for i := range picks {
		picks[i].Bytes = closure[picks[i].VersionID]
	}
	return picks, nil
}

// pickCacheRootsPage reads one keyset page of candidate roots, least recently
// used first, and which of them are blob-only.
func (s *Service) pickCacheRootsPage(ctx context.Context, sources map[string]time.Duration, idleOnly bool, limit int, after *time.Time, afterID *string) ([]cachePick, map[string]bool, error) {
	ids := make([]string, 0, len(sources))
	secs := make([]int64, 0, len(sources))
	for id, idle := range sources {
		ids = append(ids, id)
		secs = append(secs, int64(idle/time.Second))
	}
	rows, err := s.DB.Query(ctx, `WITH cfg AS (SELECT * FROM unnest($2::uuid[],$3::bigint[]) AS t(source_id,idle_seconds)),
 vs AS (
  SELECT v.id AS vid,v.component_id AS cid,c.source_id,c.coordinate,v.version,v.blob_only,
   COALESCE((SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM assets a LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE a.version_id=v.id),v.created_at) AS last_used
  FROM versions v JOIN components c ON c.id=v.component_id
  WHERE v.instance_id=$1 AND c.source_id=ANY($2::uuid[])
   AND NOT EXISTS(SELECT 1 FROM version_refs r WHERE r.component_id=v.component_id AND r.ref_version=v.version))
 SELECT vs.vid::text,vs.cid::text,vs.source_id::text,vs.coordinate,vs.version,vs.last_used,vs.blob_only FROM vs JOIN cfg ON cfg.source_id=vs.source_id
 WHERE (NOT $4::boolean OR vs.last_used<clock_timestamp()-make_interval(secs=>cfg.idle_seconds::double precision))
  AND ($6::timestamptz IS NULL OR (vs.last_used,vs.vid)>($6::timestamptz,$7::uuid))
 ORDER BY vs.last_used,vs.vid LIMIT $5`, s.InstanceID, ids, secs, idleOnly, limit, after, afterID)
	if err != nil {
		return nil, nil, err
	}
	picks := []cachePick{}
	blobOnly := map[string]bool{}
	for rows.Next() {
		var c cachePick
		var only bool
		if err = rows.Scan(&c.VersionID, &c.ComponentID, &c.SourceID, &c.Coordinate, &c.Version, &c.LastUsed, &only); err != nil {
			rows.Close()
			return nil, nil, err
		}
		picks = append(picks, c)
		if only {
			blobOnly[c.VersionID] = true
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	return picks, blobOnly, nil
}

// closureBytes estimates, per root, the distinct blob bytes of the root and of
// everything it names (an index's manifests, an image's config and layers).
// Bytes shared between roots count once for each, so it can overestimate what
// evicting several roots frees; callers re-measure.
func (s *Service) closureBytes(ctx context.Context, roots []string) (map[string]int64, error) {
	rows, err := s.DB.Query(ctx, `WITH RECURSIVE cl(root,vid) AS (
  SELECT id,id FROM versions WHERE id=ANY($1::uuid[])
  UNION
  SELECT cl.root,v2.id FROM cl JOIN version_refs r ON r.parent_version_id=cl.vid JOIN versions v2 ON v2.component_id=r.component_id AND v2.version=r.ref_version)
 SELECT x.root::text,COALESCE(sum(b.size_bytes),0)::bigint FROM (SELECT DISTINCT cl.root,a.blob_id FROM cl JOIN assets a ON a.version_id=cl.vid) x JOIN blobs b ON b.id=x.blob_id GROUP BY x.root`, roots)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var root string
		var n int64
		if err = rows.Scan(&root, &n); err != nil {
			return nil, err
		}
		out[root] = n
	}
	return out, rows.Err()
}

// cacheFeedBytes is the distinct blob bytes referenced through the feed's sources.
func (s *Service) cacheFeedBytes(ctx context.Context, feed string) (int64, error) {
	var n int64
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint FROM blobs b WHERE b.id IN (
  SELECT a.blob_id FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE fs.instance_id=$1 AND fs.feed_id=$2)`, s.InstanceID, feed).Scan(&n)
	return n, err
}

// CacheUsage is the bytes a cache feed holds and its limits.
type CacheUsage struct {
	FeedID, Slug string
	Bytes        int64
	MaxBytes     int64
	MaxIdle      time.Duration
}

// CacheUsage measures every cache feed and refreshes the per-feed metrics.
func (s *Service) CacheUsage(ctx context.Context) ([]CacheUsage, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	feeds, err := s.cacheFeeds(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CacheUsage, 0, len(feeds))
	for _, f := range feeds {
		n, err := s.cacheFeedBytes(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, CacheUsage{FeedID: f.ID, Slug: f.Slug, Bytes: n, MaxBytes: f.MaxBytes, MaxIdle: f.MaxIdle})
	}
	cacheMetricsState.observeUsage(out)
	return out, nil
}

// CacheReport describes one cache maintenance pass.
type CacheReport struct {
	DryRun  bool       `json:"dry_run"`
	Evicted []Deletion `json:"evicted"`
	Bytes   int64      `json:"bytes"`
	Swept   int        `json:"swept"`
}

// ApplyCache evicts idle content and enforces max_bytes of every cache feed.
// It is part of ApplyRetention and of the periodic pressure loop. With dryRun
// nothing is modified; the size-limit stage then reports an estimate from the
// current state (it cannot re-measure between evictions).
func (s *Service) ApplyCache(ctx context.Context, dryRun bool) (CacheReport, error) {
	rep := CacheReport{DryRun: dryRun, Evicted: []Deletion{}}
	if err := s.validate(); err != nil {
		return rep, err
	}
	feeds, err := s.cacheFeeds(ctx)
	if err != nil || len(feeds) == 0 {
		return rep, err
	}
	var errs []error
	// 1. Idle content.
	picks, err := s.pickCacheRoots(ctx, cacheSourceIdle(feeds), true, cacheScanLimit)
	if err != nil {
		return rep, err
	}
	if err = s.evictCachePicks(ctx, picks, cacheReasonIdle, dryRun, &rep); err != nil {
		errs = append(errs, err)
	}
	// 2. Size limits, per feed.
	for _, f := range feeds {
		if f.MaxBytes <= 0 {
			continue
		}
		if err = s.enforceCacheBytes(ctx, f, dryRun, &rep); err != nil {
			errs = append(errs, err)
		}
	}
	return rep, errors.Join(errs...)
}

func (s *Service) enforceCacheBytes(ctx context.Context, f CacheFeed, dryRun bool, rep *CacheReport) error {
	sources := map[string]time.Duration{}
	for _, src := range f.Sources {
		sources[src] = f.MaxIdle
	}
	for round := 0; round < cacheRoundLimit; round++ {
		used, err := s.cacheFeedBytes(ctx, f.ID)
		if err != nil {
			return err
		}
		need := used - f.MaxBytes
		if need <= 0 {
			return nil
		}
		picks, err := s.pickCacheRoots(ctx, sources, false, cacheRoundSize)
		if err != nil {
			return err
		}
		var chosen []cachePick
		var estimate int64
		for _, c := range picks {
			chosen = append(chosen, c)
			if estimate += c.Bytes; estimate >= need {
				break
			}
		}
		if len(chosen) == 0 {
			return nil
		}
		if dryRun {
			// Without evicting nothing changes between rounds: report one round.
			return s.evictCachePicks(ctx, chosen, cacheReasonSize, true, rep)
		}
		before := len(rep.Evicted)
		if err = s.evictCachePicks(ctx, chosen, cacheReasonSize, false, rep); err != nil {
			return err
		}
		if len(rep.Evicted) == before {
			return nil // nothing could be evicted (concurrent reads); retry next pass
		}
	}
	return nil
}

// evictCachePicks evicts the picked roots grouped by component.
func (s *Service) evictCachePicks(ctx context.Context, picks []cachePick, reason string, dryRun bool, rep *CacheReport) error {
	if len(picks) == 0 {
		return nil
	}
	if dryRun {
		for _, c := range picks {
			rep.Evicted = append(rep.Evicted, Deletion{SourceID: c.SourceID, Coordinate: c.Coordinate, Version: c.Version, VersionID: c.VersionID, Reason: reason})
			rep.Bytes += c.Bytes
		}
		return nil
	}
	byComponent := map[string][]cachePick{}
	var order []string
	for _, c := range picks {
		if _, ok := byComponent[c.ComponentID]; !ok {
			order = append(order, c.ComponentID)
		}
		byComponent[c.ComponentID] = append(byComponent[c.ComponentID], c)
	}
	var errs []error
	for i, component := range order {
		if i%20 == 0 {
			if err := s.requireLease(ctx); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		done, bytes, err := s.evictCacheComponent(ctx, byComponent[component], reason)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Evicted = append(rep.Evicted, done...)
		rep.Bytes += bytes
	}
	return errors.Join(errs...)
}

// evictCacheComponent removes the picked roots of one component under the
// per-coordinate advisory lock publication uses. Under that lock it re-checks
// that each root still has no referrer and was not used since it was selected,
// deletes its tags, and releases it with whatever only it referenced. Freed
// blobs get the short sweep wait; the sweep re-checks references itself.
func (s *Service) evictCacheComponent(ctx context.Context, group []cachePick, reason string) ([]Deletion, int64, error) {
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	sum := sha256.Sum256([]byte(s.InstanceID + "\x00" + group[0].SourceID + "\x00" + group[0].Coordinate))
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(sum[:8]))); err != nil {
		return nil, 0, err
	}
	ids := make([]string, len(group))
	for i, c := range group {
		ids[i] = c.VersionID
	}
	rows, err := tx.Query(ctx, `SELECT v.id::text,
  COALESCE((SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM assets a LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE a.version_id=v.id),v.created_at),
  EXISTS(SELECT 1 FROM version_refs r WHERE r.component_id=v.component_id AND r.ref_version=v.version)
 FROM versions v WHERE v.component_id=$1 AND v.id=ANY($2::uuid[])`, group[0].ComponentID, ids)
	if err != nil {
		return nil, 0, err
	}
	type state struct {
		used       time.Time
		referenced bool
	}
	current := map[string]state{}
	for rows.Next() {
		var id string
		var st state
		if err = rows.Scan(&id, &st.used, &st.referenced); err != nil {
			rows.Close()
			return nil, 0, err
		}
		current[id] = st
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	var batch []cachePick
	var batchIDs []string
	for _, c := range group {
		if st, ok := current[c.VersionID]; ok && !st.referenced && !st.used.After(c.LastUsed) {
			batch = append(batch, c)
			batchIDs = append(batchIDs, c.VersionID)
		}
	}
	// A blob-only root is released only when no remaining manifest names it, however
	// long it has been idle.
	doomed := map[string]bool{}
	for _, id := range batchIDs {
		doomed[id] = true
	}
	keep, err := s.protectedBlobs(ctx, tx, group[0].ComponentID, doomed)
	if err != nil {
		return nil, 0, err
	}
	if len(keep) > 0 {
		kept, keptIDs := batch[:0:0], batchIDs[:0:0]
		for _, c := range batch {
			if !keep[c.VersionID] {
				kept, keptIDs = append(kept, c), append(keptIDs, c.VersionID)
			}
		}
		batch, batchIDs = kept, keptIDs
	}
	if len(batch) == 0 {
		return nil, 0, nil
	}
	if _, err = tx.Exec(ctx, `DELETE FROM tags WHERE component_id=$1 AND version_id=ANY($2::uuid[])`, group[0].ComponentID, batchIDs); err != nil {
		return nil, 0, err
	}
	res, err := cacherelease.Unreferenced(ctx, tx, s.InstanceID, group[0].ComponentID, batchIDs, s.sweepGrace(), s.opts().Grace)
	if err != nil {
		return nil, 0, err
	}
	done := make([]Deletion, len(batch))
	for i, c := range batch {
		done[i] = Deletion{SourceID: c.SourceID, Coordinate: c.Coordinate, Version: c.Version, VersionID: c.VersionID, Reason: reason}
		payload := map[string]string{"source_id": c.SourceID, "coordinate": c.Coordinate, "version": c.Version, "reason": reason}
		if err = events.Emit(ctx, tx, s.InstanceID, "", "retention.cache.evict", c.VersionID, payload); err != nil {
			return nil, 0, err
		}
	}
	if err = s.requireLeaseTx(ctx, tx); err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	cacheMetricsState.observeEviction(reason, len(batch), res.Bytes)
	return done, res.Bytes, nil
}

// evictCachePressure evicts cache content, least recently used first, until
// target bytes are projected freed. It runs before any other pressure
// eviction. It returns the bytes it freed.
func (s *Service) evictCachePressure(ctx context.Context, target int64, dryRun bool, rep *PressureReport) (int64, error) {
	feeds, err := s.cacheFeeds(ctx)
	if err != nil || len(feeds) == 0 || target <= 0 {
		return 0, err
	}
	sources := cacheSourceIdle(feeds)
	var freed int64
	var errs []error
	for round := 0; round < cacheRoundLimit && freed < target; round++ {
		picks, err := s.pickCacheRoots(ctx, sources, false, cacheRoundSize)
		if err != nil {
			return freed, err
		}
		var chosen []cachePick
		var estimate int64
		for _, c := range picks {
			chosen = append(chosen, c)
			if estimate += c.Bytes; freed+estimate >= target {
				break
			}
		}
		if len(chosen) == 0 {
			break
		}
		var sub CacheReport
		if err = s.evictCachePicks(ctx, chosen, cacheReasonPress, dryRun, &sub); err != nil {
			errs = append(errs, err)
		}
		if len(sub.Evicted) == 0 {
			break
		}
		rep.Evicted = append(rep.Evicted, sub.Evicted...)
		rep.EvictedVersions += len(sub.Evicted)
		rep.EvictedBytes += sub.Bytes
		freed += sub.Bytes
		if dryRun {
			break // nothing changes between rounds without evicting
		}
	}
	return freed, errors.Join(errs...)
}

// CacheTick is the cheap periodic pass: cache eviction plus the short sweep of
// everything it (or a tag overwrite or cleanup) queued. It runs from the
// pressure loop, so it works whether or not pressure eviction is enabled.
func (s *Service) CacheTick(ctx context.Context) (CacheReport, error) {
	rep, err := s.ApplyCache(ctx, false)
	if _, uerr := s.CacheUsage(ctx); uerr != nil && err == nil {
		err = fmt.Errorf("cache usage: %w", uerr)
	}
	var pr PressureReport
	if serr := s.sweepExpedited(ctx, &pr); serr != nil {
		err = errors.Join(err, serr)
	}
	rep.Swept = pr.Swept
	return rep, err
}
