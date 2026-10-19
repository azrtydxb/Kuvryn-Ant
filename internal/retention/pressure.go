package retention

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/cacherelease"
	"github.com/azrtydxb/Kuvryn-Ant/internal/diskusage"
	"github.com/azrtydxb/Kuvryn-Ant/internal/events"
	"github.com/jackc/pgx/v5"
)

// Pressure states, in increasing order of concern.
const (
	StateDisabled    = "disabled"    // pressure eviction is not enabled
	StateNoCapacity  = "no_capacity" // enabled, but capacity is unknown (object storage without a quota)
	StateInvalid     = "invalid"     // the watermarks are inconsistent
	StateOK          = "ok"          // below the high watermark
	StatePressure    = "pressure"    // at or above the high watermark and being relieved
	StateUnevictable = "unevictable" // at or above the high watermark with nothing (more) evictable
)

const (
	// DefaultPressureInterval is how often the cheap capacity check runs.
	DefaultPressureInterval = 5 * time.Minute
	// DefaultPressureSweepGrace is how long a blob freed by eviction waits before
	// it can be swept, instead of the full GC grace. It only has to outlast
	// readers that resolved the asset just before the eviction.
	DefaultPressureSweepGrace = 15 * time.Minute

	pressureCandidateScan = 5000 // versions examined per run, least recently used first
	pressureEvictLimit    = 2000 // versions evicted per run
)

// Capacity is the space a pressure check measures usage against.
type Capacity struct {
	// Source is "filesystem" (disk blob root), "quota" (operator-configured
	// bytes) or "none" (object storage without a quota).
	Source     string `json:"source"`
	TotalBytes int64  `json:"total_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
}

// Ratio is used/total in [0, +inf), or 0 when capacity is unknown.
func (c Capacity) Ratio() float64 {
	if c.TotalBytes <= 0 {
		return 0
	}
	return float64(c.UsedBytes) / float64(c.TotalBytes)
}

// percentOf returns total*pct/100 without overflow.
func percentOf(total int64, pct int) int64 {
	return total/100*int64(pct) + total%100*int64(pct)/100
}

func (s *Service) sweepGrace() time.Duration {
	if s.Options.PressureSweepGrace > 0 {
		return s.Options.PressureSweepGrace
	}
	return DefaultPressureSweepGrace
}

// Capacity measures capacity and usage for the active backend. An operator
// quota applies to every backend and measures usage as committed blob bytes.
// Without a quota only disk has a capacity (its filesystem); object storage
// reports Source "none".
func (s *Service) Capacity(ctx context.Context, settings Settings) (Capacity, error) {
	backend := "disk"
	if s.Backend != nil {
		b, err := s.Backend(ctx)
		if err != nil {
			return Capacity{}, err
		}
		if b != "" {
			backend = b
		}
	}
	if q := settings.Pressure.CapacityBytes; q > 0 {
		var used int64
		if err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(size_bytes),0)::bigint FROM blobs WHERE instance_id=$1`, s.InstanceID).Scan(&used); err != nil {
			return Capacity{}, err
		}
		return Capacity{Source: "quota", TotalBytes: q, UsedBytes: used}, nil
	}
	if backend != "disk" || s.DataDir == "" {
		return Capacity{Source: "none"}, nil
	}
	fs := s.FSUsage
	if fs == nil {
		fs = func(path string) (uint64, uint64, error) {
			u, err := diskusage.Of(path)
			return u.Total, u.Free, err
		}
	}
	total, free, err := fs(s.DataDir)
	if err != nil {
		return Capacity{}, fmt.Errorf("filesystem usage: %w", err)
	}
	if total == 0 || free > total {
		return Capacity{Source: "none"}, nil
	}
	return Capacity{Source: "filesystem", TotalBytes: int64(total), UsedBytes: int64(total - free)}, nil
}

// reclaimableBytes is the size of unreferenced blobs that garbage collection
// will free without further waiting for a reference to disappear: already
// marked, or old enough to be marked on the next run.
func (s *Service) reclaimableBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint FROM blobs b WHERE b.instance_id=$1 AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id) AND NOT EXISTS(SELECT 1 FROM federation_route_cache rc WHERE rc.blob_id=b.id) AND (b.created_at<clock_timestamp()-$2::interval OR EXISTS(SELECT 1 FROM gc_candidates c WHERE c.dedup_domain_id=b.dedup_domain_id AND c.digest=b.digest))`,
		s.InstanceID, s.graceInterval()).Scan(&n)
	return n, err
}

// PressureReport describes one pressure check. Evicted lists (up to the
// per-run limit) the versions evicted or, in dry-run, that would be.
type PressureReport struct {
	DryRun           bool       `json:"dry_run"`
	State            string     `json:"state"`
	Detail           string     `json:"detail,omitempty"`
	CapacitySource   string     `json:"capacity_source"`
	UsedBytes        int64      `json:"used_bytes"`
	CapacityBytes    int64      `json:"capacity_bytes"`
	UsageRatio       float64    `json:"usage_ratio"`
	HighPercent      int        `json:"high_percent"`
	LowPercent       int        `json:"low_percent"`
	ReclaimableBytes int64      `json:"reclaimable_bytes"`
	TargetBytes      int64      `json:"target_bytes"`
	EvictedVersions  int        `json:"evicted_versions"`
	EvictedBytes     int64      `json:"evicted_bytes"`
	ShortfallBytes   int64      `json:"shortfall_bytes"`
	Expedited        int        `json:"expedited"`
	Swept            int        `json:"swept"`
	Evicted          []Deletion `json:"evicted"`
}

// RunPressure checks capacity and, at or above the high watermark, evicts
// remote-cached content least recently used first until the projected usage
// (current usage minus bytes garbage collection will free anyway) is at or
// below the low watermark. Blobs freed by the eviction are made sweepable after
// the short pressure grace and swept by the same call once due. The check is
// idempotent: between the watermarks it does nothing, which is what keeps it
// from flapping. With dryRun nothing is modified.
func (s *Service) RunPressure(ctx context.Context, dryRun bool) (PressureReport, error) {
	rep := PressureReport{DryRun: dryRun, Evicted: []Deletion{}, State: StateDisabled}
	if err := s.validate(); err != nil {
		return rep, err
	}
	s.pressureMu.Lock()
	defer s.pressureMu.Unlock()
	settings, err := s.Settings(ctx)
	if err != nil {
		return rep, err
	}
	rep.HighPercent, rep.LowPercent = settings.Pressure.HighPercent, settings.Pressure.LowPercent
	finish := func(err error) (PressureReport, error) {
		if !dryRun {
			s.recordPressure(ctx, rep, settings.Pressure.Enabled)
		}
		return rep, err
	}
	switch {
	case !settings.Pressure.Enabled:
		rep.Detail = "storage pressure eviction is not enabled"
		return finish(nil)
	case settings.Validate() != nil:
		rep.State, rep.Detail = StateInvalid, "the watermarks are inconsistent"
		return finish(nil)
	}
	capacity, err := s.Capacity(ctx, settings)
	if err != nil {
		return rep, err
	}
	rep.CapacitySource = capacity.Source
	if capacity.Source == "none" {
		rep.State = StateNoCapacity
		rep.Detail = "capacity is unknown: object storage needs a capacity quota before pressure eviction can run"
		return finish(nil)
	}
	rep.UsedBytes, rep.CapacityBytes, rep.UsageRatio = capacity.UsedBytes, capacity.TotalBytes, capacity.Ratio()
	if rep.ReclaimableBytes, err = s.reclaimableBytes(ctx); err != nil {
		return rep, err
	}
	high := percentOf(capacity.TotalBytes, settings.Pressure.HighPercent)
	low := percentOf(capacity.TotalBytes, settings.Pressure.LowPercent)
	rep.State = StateOK
	if capacity.UsedBytes < high {
		// Below the high watermark: nothing to evict, but blobs freed by an
		// earlier eviction may have become due.
		if !dryRun {
			if err = s.sweepExpedited(ctx, &rep); err != nil {
				return finish(err)
			}
		}
		return finish(nil)
	}
	rep.State = StatePressure
	projected := capacity.UsedBytes - min(rep.ReclaimableBytes, capacity.UsedBytes)
	rep.TargetBytes = max(0, projected-low)
	var errs []error
	if !dryRun {
		if err = s.requireLease(ctx); err != nil {
			return rep, err
		}
	}
	exhausted := true
	if rep.TargetBytes > 0 {
		// Cache feeds first, least recently used first, then remote-cached content.
		cacheFreed, cacheErr := s.evictCachePressure(ctx, rep.TargetBytes, dryRun, &rep)
		if cacheErr != nil {
			errs = append(errs, cacheErr)
		}
		if remaining := rep.TargetBytes - cacheFreed; remaining > 0 {
			var evictErr error
			exhausted, evictErr = s.evict(ctx, settings, dryRun, remaining, &rep)
			if evictErr != nil {
				errs = append(errs, evictErr)
			}
		}
		rep.ShortfallBytes = max(0, rep.TargetBytes-rep.EvictedBytes)
	}
	if !dryRun {
		if err = s.expedite(ctx, &rep); err != nil {
			errs = append(errs, err)
		}
		if err = s.sweepExpedited(ctx, &rep); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case rep.TargetBytes == 0:
		rep.Detail = "usage is above the high watermark but garbage collection will free enough"
	case rep.ShortfallBytes > 0 && exhausted:
		rep.State = StateUnevictable
		rep.Detail = fmt.Sprintf("usage is above the high watermark but only %d bytes of the %d needed are evictable; local and replica content is never evicted unless its policy allows release deletion", rep.EvictedBytes, rep.TargetBytes)
	case rep.ShortfallBytes > 0:
		rep.Detail = "eviction limit reached for this run; it continues on the next check"
	}
	return finish(errors.Join(errs...))
}

// pressureSource is one source whose content may be evicted, with the
// protections that apply to its versions.
type pressureSource struct {
	ID            string
	MinKeep       int
	IncludeTagged bool
}

// pressureSources lists the evictable sources. Remote (cache) sources are
// always evictable; their retention policies, or the instance default, only
// contribute protections (min_keep, tagged). Local and replica sources are
// release content: they are evictable only when every effective policy
// explicitly allows release deletion.
func (s *Service) pressureSources(ctx context.Context, settings Settings) ([]pressureSource, error) {
	withPolicy, err := s.loadSources(ctx)
	if err != nil {
		return nil, err
	}
	cacheSources, err := s.cacheSourceSet(ctx)
	if err != nil {
		return nil, err
	}
	policies := map[string]sourceInfo{}
	for _, si := range withPolicy {
		policies[si.ID] = si
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,kind FROM sources WHERE instance_id=$1 AND kind IN ('remote','local','replica') ORDER BY id`, s.InstanceID)
	if err != nil {
		return nil, err
	}
	type srcKind struct{ id, kind string }
	var all []srcKind
	for rows.Next() {
		var sk srcKind
		if err = rows.Scan(&sk.id, &sk.kind); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, sk)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	def := settings.Default
	var out []pressureSource
	for _, sk := range all {
		if cacheSources[sk.id] {
			continue // cache feeds are evicted first, by evictCachePressure
		}
		ps := pressureSource{ID: sk.id, MinKeep: 1}
		si, has := policies[sk.id]
		if sk.kind != "remote" {
			if !has || !slices.ContainsFunc(si.Policies, func(p Policy) bool { return p.AllowReleaseDelete }) || slices.ContainsFunc(si.Policies, func(p Policy) bool { return !p.AllowReleaseDelete }) {
				continue
			}
		}
		switch {
		case has:
			ps.MinKeep, ps.IncludeTagged = 0, true
			for _, p := range si.Policies {
				ps.MinKeep = max(ps.MinKeep, p.MinKeep)
				ps.IncludeTagged = ps.IncludeTagged && p.IncludeTagged
			}
		case def.Enabled:
			ps.MinKeep, ps.IncludeTagged = def.MinKeep, def.IncludeTagged
		}
		out = append(out, ps)
	}
	return out, nil
}

type evictCandidate struct {
	VersionID, ComponentID, SourceID, Coordinate, Version string
	LastUsed                                              time.Time
}

type blobHold struct {
	ID   string
	Size int64
}

// selectEvictions picks, in least-recently-used order, versions whose removal
// reduces usage. A version whose every blob is also held by something that
// cannot be evicted (a protected version, another feed) frees nothing and is
// skipped. Selection stops once the projected bytes freed reach target.
func (s *Service) selectEvictions(ctx context.Context, sources []pressureSource, target int64) (picked []evictCandidate, freed int64, exhausted bool, err error) {
	ids := make([]string, len(sources))
	minKeeps := make([]int32, len(sources))
	tagged := make([]bool, len(sources))
	for i, src := range sources {
		ids[i], minKeeps[i], tagged[i] = src.ID, int32(src.MinKeep), src.IncludeTagged
	}
	rows, err := s.DB.Query(ctx, `WITH cfg AS (SELECT * FROM unnest($2::uuid[],$3::int[],$4::boolean[]) AS t(source_id,min_keep,include_tagged)),
 vs AS (
  SELECT v.id AS vid,v.component_id AS cid,c.source_id,c.coordinate,v.version,
   COALESCE((SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM assets a LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE a.version_id=v.id),v.created_at) AS last_used,
   row_number() OVER (PARTITION BY v.component_id ORDER BY v.created_at DESC,v.id DESC) AS rnk,
   EXISTS(SELECT 1 FROM tags t WHERE t.component_id=v.component_id AND t.version_id=v.id) AS is_tagged
  FROM versions v JOIN components c ON c.id=v.component_id WHERE v.instance_id=$1 AND c.source_id=ANY($2::uuid[]))
 SELECT vs.vid::text,vs.cid::text,vs.source_id::text,vs.coordinate,vs.version,vs.last_used FROM vs JOIN cfg ON cfg.source_id=vs.source_id
 WHERE vs.rnk>cfg.min_keep AND (NOT vs.is_tagged OR cfg.include_tagged) ORDER BY vs.last_used,vs.vid LIMIT $5`,
		s.InstanceID, ids, minKeeps, tagged, pressureCandidateScan)
	if err != nil {
		return nil, 0, false, err
	}
	var cands []evictCandidate
	for rows.Next() {
		var c evictCandidate
		if err = rows.Scan(&c.VersionID, &c.ComponentID, &c.SourceID, &c.Coordinate, &c.Version, &c.LastUsed); err != nil {
			rows.Close()
			return nil, 0, false, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, false, err
	}
	exhausted = len(cands) < pressureCandidateScan
	if len(cands) == 0 {
		return nil, 0, true, nil
	}
	versionIDs := make([]string, len(cands))
	for i, c := range cands {
		versionIDs[i] = c.VersionID
	}
	hold := map[string][]blobHold{}
	byBlob := map[string]int{} // references from the candidate set
	size := map[string]int64{}
	rows, err = s.DB.Query(ctx, `SELECT a.version_id::text,b.id::text,b.size_bytes FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.version_id=ANY($1::uuid[])`, versionIDs)
	if err != nil {
		return nil, 0, false, err
	}
	var blobIDs []string
	for rows.Next() {
		var v, b string
		var n int64
		if err = rows.Scan(&v, &b, &n); err != nil {
			rows.Close()
			return nil, 0, false, err
		}
		hold[v] = append(hold[v], blobHold{b, n})
		if byBlob[b] == 0 {
			blobIDs = append(blobIDs, b)
		}
		byBlob[b]++
		size[b] = n
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, false, err
	}
	external := map[string]int{} // references from outside the candidate set
	if len(blobIDs) > 0 {
		rows, err = s.DB.Query(ctx, `SELECT b.id::text,(SELECT count(*) FROM assets a WHERE a.blob_id=b.id)+(SELECT count(*) FROM federation_route_cache rc WHERE rc.blob_id=b.id) FROM blobs b WHERE b.id=ANY($1::uuid[])`, blobIDs)
		if err != nil {
			return nil, 0, false, err
		}
		for rows.Next() {
			var b string
			var total int
			if err = rows.Scan(&b, &total); err != nil {
				rows.Close()
				return nil, 0, false, err
			}
			external[b] = max(0, total-byBlob[b])
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, 0, false, err
		}
	}
	remaining := byBlob
	for _, c := range cands {
		if len(picked) >= pressureEvictLimit {
			exhausted = false
			break
		}
		if freed >= target {
			break
		}
		useful := false
		for _, h := range hold[c.VersionID] {
			if external[h.ID] == 0 {
				useful = true
			}
		}
		if !useful {
			continue
		}
		picked = append(picked, c)
		for _, h := range hold[c.VersionID] {
			remaining[h.ID]--
			if remaining[h.ID] == 0 && external[h.ID] == 0 {
				freed += size[h.ID]
			}
		}
	}
	if freed >= target {
		exhausted = false
	}
	return picked, freed, exhausted, nil
}

// evict selects and, unless dryRun, deletes versions. exhausted is true when
// every evictable version was considered.
func (s *Service) evict(ctx context.Context, settings Settings, dryRun bool, target int64, rep *PressureReport) (exhausted bool, err error) {
	sources, err := s.pressureSources(ctx, settings)
	if err != nil || len(sources) == 0 {
		return true, err
	}
	picked, freed, exhausted, err := s.selectEvictions(ctx, sources, target)
	if err != nil {
		return false, err
	}
	if dryRun {
		for _, c := range picked {
			rep.Evicted = append(rep.Evicted, Deletion{SourceID: c.SourceID, Coordinate: c.Coordinate, Version: c.Version, VersionID: c.VersionID, Reason: "pressure_lru"})
		}
		rep.EvictedVersions += len(picked)
		rep.EvictedBytes += freed
		return exhausted, nil
	}
	byComponent := map[string][]evictCandidate{}
	var order []string
	for _, c := range picked {
		if _, ok := byComponent[c.ComponentID]; !ok {
			order = append(order, c.ComponentID)
		}
		byComponent[c.ComponentID] = append(byComponent[c.ComponentID], c)
	}
	minKeep := map[string]pressureSource{}
	for _, src := range sources {
		minKeep[src.ID] = src
	}
	var errs []error
	for i, component := range order {
		if i%20 == 0 {
			if err = s.requireLease(ctx); err != nil {
				return false, errors.Join(append(errs, err)...)
			}
		}
		if err = ctx.Err(); err != nil {
			return false, errors.Join(append(errs, err)...)
		}
		group := byComponent[component]
		done, bytes, err := s.evictComponent(ctx, minKeep[group[0].SourceID], group)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Evicted = append(rep.Evicted, done...)
		rep.EvictedVersions += len(done)
		rep.EvictedBytes += bytes
	}
	return exhausted, errors.Join(errs...)
}

// evictComponent deletes the picked versions of one component under the same
// per-coordinate advisory lock publication uses and re-checks every protection
// and the access time under that lock, so a concurrent read, tag or newer
// publication is respected. Blobs freed by it become sweepable after the short
// pressure grace; the sweep itself still re-checks references under the blobs
// row lock.
func (s *Service) evictComponent(ctx context.Context, src pressureSource, group []evictCandidate) ([]Deletion, int64, error) {
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
	// Still present, still beyond min_keep, still unprotected and not read since selection.
	rows, err := tx.Query(ctx, `SELECT vid::text,rnk,is_tagged,last_used FROM (
  SELECT v.id AS vid,
   row_number() OVER (ORDER BY v.created_at DESC,v.id DESC) AS rnk,
   EXISTS(SELECT 1 FROM tags t WHERE t.component_id=v.component_id AND t.version_id=v.id) AS is_tagged,
   COALESCE((SELECT max(GREATEST(a.created_at,a.last_accessed_at,ad.last_downloaded_at)) FROM assets a LEFT JOIN asset_downloads ad ON ad.asset_id=a.id WHERE a.version_id=v.id),v.created_at) AS last_used
  FROM versions v WHERE v.component_id=$1) x
 WHERE vid=ANY($2::uuid[])`, group[0].ComponentID, ids)
	if err != nil {
		return nil, 0, err
	}
	current := map[string]evictCandidate{}
	still := map[string]bool{}
	for rows.Next() {
		var id string
		var rank int
		var isTagged bool
		var used time.Time
		if err = rows.Scan(&id, &rank, &isTagged, &used); err != nil {
			rows.Close()
			return nil, 0, err
		}
		current[id] = evictCandidate{LastUsed: used}
		still[id] = rank > src.MinKeep && (!isTagged || src.IncludeTagged)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	var batch []evictCandidate
	var batchIDs []string
	for _, c := range group {
		if still[c.VersionID] && !current[c.VersionID].LastUsed.After(c.LastUsed) {
			batch = append(batch, c)
			batchIDs = append(batchIDs, c.VersionID)
		}
	}
	if len(batch) == 0 {
		return nil, 0, nil
	}
	if _, err = tx.Exec(ctx, `DELETE FROM tags WHERE component_id=$1 AND version_id=ANY($2::uuid[])`, group[0].ComponentID, batchIDs); err != nil {
		return nil, 0, err
	}
	var blobIDs []string
	rows, err = tx.Query(ctx, `DELETE FROM assets WHERE instance_id=$1 AND version_id=ANY($2::uuid[]) RETURNING blob_id::text`, s.InstanceID, batchIDs)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var b string
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, 0, err
		}
		blobIDs = append(blobIDs, b)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM versions WHERE instance_id=$1 AND component_id=$2 AND id=ANY($3::uuid[])`, s.InstanceID, group[0].ComponentID, batchIDs); err != nil {
		return nil, 0, err
	}
	freed, err := cacherelease.ExpediteBlobs(ctx, tx, blobIDs, s.sweepGrace(), s.opts().Grace)
	if err != nil {
		return nil, 0, err
	}
	done := make([]Deletion, len(batch))
	for i, c := range batch {
		done[i] = Deletion{SourceID: c.SourceID, Coordinate: c.Coordinate, Version: c.Version, VersionID: c.VersionID, Reason: "pressure_lru"}
		payload := map[string]string{"source_id": c.SourceID, "coordinate": c.Coordinate, "version": c.Version, "reason": "pressure_lru"}
		if err = events.Emit(ctx, tx, s.InstanceID, "", "retention.pressure.evict", c.VersionID, payload); err != nil {
			return nil, 0, err
		}
	}
	if err = s.requireLeaseTx(ctx, tx); err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return done, freed, nil
}

func microseconds(d time.Duration) string { return fmt.Sprintf("%d microseconds", d.Microseconds()) }

// expedite shortens the wait of blobs garbage collection already has to remove:
// marked candidates and unreferenced rows older than the grace period. They
// would be freed anyway; under pressure they should not wait for the next run.
func (s *Service) expedite(ctx context.Context, rep *PressureReport) error {
	tag, err := s.DB.Exec(ctx, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind,sweep_after)
 SELECT b.instance_id,b.dedup_domain_id,b.digest,'row',clock_timestamp()+$3::interval FROM blobs b
  WHERE b.instance_id=$1 AND b.created_at<clock_timestamp()-$2::interval AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id) AND NOT EXISTS(SELECT 1 FROM federation_route_cache rc WHERE rc.blob_id=b.id)
  ORDER BY b.created_at LIMIT $4
 ON CONFLICT(dedup_domain_id,digest) DO UPDATE SET sweep_after=excluded.sweep_after
  WHERE gc_candidates.kind='row' AND COALESCE(gc_candidates.sweep_after,gc_candidates.first_seen_at+$2::interval)>excluded.sweep_after`,
		s.InstanceID, s.graceInterval(), microseconds(s.sweepGrace()), s.opts().BatchSize)
	rep.Expedited = int(tag.RowsAffected())
	return err
}

// sweepExpedited sweeps candidates whose shortened wait is over. It is the
// ordinary sweep (row lock, reference re-check, byte deletion) restricted to
// expedited candidates, so pressure checks never pull forward the normal
// schedule of unrelated candidates.
func (s *Service) sweepExpedited(ctx context.Context, rep *PressureReport) error {
	var runStart time.Time
	if err := s.DB.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&runStart); err != nil {
		return err
	}
	if err := s.requireLease(ctx); err != nil {
		return err
	}
	var gc GCReport
	err := s.sweep(ctx, runStart, false, true, &gc)
	rep.Swept = len(gc.Swept)
	return err
}

// PressureStatus is the live view for the console and storage usage.
type PressureStatus struct {
	Enabled              bool       `json:"enabled"`
	State                string     `json:"state"`
	Detail               string     `json:"detail,omitempty"`
	CapacitySource       string     `json:"capacity_source"`
	UsedBytes            int64      `json:"used_bytes"`
	CapacityBytes        int64      `json:"capacity_bytes"`
	UsageRatio           float64    `json:"usage_ratio"`
	HighPercent          int        `json:"high_percent"`
	LowPercent           int        `json:"low_percent"`
	ReclaimableBytes     int64      `json:"reclaimable_bytes"`
	CheckedAt            *time.Time `json:"checked_at,omitempty"`
	LastEvictedAt        *time.Time `json:"last_evicted_at,omitempty"`
	LastEvictedVersions  int        `json:"last_evicted_versions"`
	LastEvictedBytes     int64      `json:"last_evicted_bytes"`
	TotalEvictedVersions int64      `json:"total_evicted_versions"`
	TotalEvictedBytes    int64      `json:"total_evicted_bytes"`
}

// PressureStatus measures capacity now and combines it with the last recorded
// check. It never modifies anything.
func (s *Service) PressureStatus(ctx context.Context) (PressureStatus, error) {
	st := PressureStatus{State: StateDisabled}
	if err := s.validate(); err != nil {
		return st, err
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		return st, err
	}
	st.Enabled = settings.Pressure.Enabled
	st.HighPercent, st.LowPercent = settings.Pressure.HighPercent, settings.Pressure.LowPercent
	var recorded string
	var checked time.Time
	err = s.DB.QueryRow(ctx, `SELECT state,checked_at,last_evicted_at,last_evicted_versions,last_evicted_bytes,total_evicted_versions,total_evicted_bytes FROM retention_pressure_state WHERE instance_id=$1`, s.InstanceID).
		Scan(&recorded, &checked, &st.LastEvictedAt, &st.LastEvictedVersions, &st.LastEvictedBytes, &st.TotalEvictedVersions, &st.TotalEvictedBytes)
	switch {
	case err == nil:
		st.CheckedAt = &checked
	case errors.Is(err, pgx.ErrNoRows):
		recorded = ""
	default:
		return st, err
	}
	capacity, err := s.Capacity(ctx, settings)
	if err != nil {
		return st, err
	}
	st.CapacitySource = capacity.Source
	st.UsedBytes, st.CapacityBytes, st.UsageRatio = capacity.UsedBytes, capacity.TotalBytes, capacity.Ratio()
	if st.ReclaimableBytes, err = s.reclaimableBytes(ctx); err != nil {
		return st, err
	}
	switch {
	case !st.Enabled:
		st.State, st.Detail = StateDisabled, "storage pressure eviction is not enabled"
	case settings.Validate() != nil:
		st.State, st.Detail = StateInvalid, "the watermarks are inconsistent"
	case capacity.Source == "none":
		st.State = StateNoCapacity
		st.Detail = "capacity is unknown: object storage needs a capacity quota before pressure eviction can run"
	case capacity.UsedBytes < percentOf(capacity.TotalBytes, st.HighPercent):
		st.State = StateOK
	case recorded == StateUnevictable:
		st.State = StateUnevictable
		st.Detail = "usage is above the high watermark and nothing more is evictable"
	default:
		st.State = StatePressure
	}
	return st, nil
}

// recordPressure persists the outcome and refreshes the process metrics. It is
// best effort: a failure to record never fails the check.
func (s *Service) recordPressure(ctx context.Context, rep PressureReport, enabled bool) {
	pressureMetricsState.observe(rep, enabled)
	rctx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	detail := strings.ToValidUTF8(rep.Detail, "?")
	_, _ = s.DB.Exec(rctx, `INSERT INTO retention_pressure_state(instance_id,state,detail,checked_at,used_bytes,capacity_bytes,last_evicted_at,last_evicted_versions,last_evicted_bytes,total_evicted_versions,total_evicted_bytes)
 VALUES($1,$2,$3,now(),$4,$5,CASE WHEN $6::int>0 THEN now() END,$6,$7,$6,$7)
 ON CONFLICT(instance_id) DO UPDATE SET state=excluded.state,detail=excluded.detail,checked_at=now(),used_bytes=excluded.used_bytes,capacity_bytes=excluded.capacity_bytes,
  last_evicted_at=CASE WHEN $6::int>0 THEN now() ELSE retention_pressure_state.last_evicted_at END,
  last_evicted_versions=CASE WHEN $6::int>0 THEN $6 ELSE retention_pressure_state.last_evicted_versions END,
  last_evicted_bytes=CASE WHEN $6::int>0 THEN $7 ELSE retention_pressure_state.last_evicted_bytes END,
  total_evicted_versions=retention_pressure_state.total_evicted_versions+$6,
  total_evicted_bytes=retention_pressure_state.total_evicted_bytes+$7`,
		s.InstanceID, rep.State, detail, rep.UsedBytes, rep.CapacityBytes, rep.EvictedVersions, rep.EvictedBytes)
}

// RunPressureLoop runs the cheap capacity check every interval until ctx ends.
// Each pass is fenced by the writer lease like every other mutating step.
func (s *Service) RunPressureLoop(ctx context.Context, interval time.Duration, handle func(PressureReport, error)) {
	if interval <= 0 {
		return
	}
	timer := time.NewTimer(min(30*time.Second, interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		rep, err := s.RunPressure(ctx, false)
		if handle != nil {
			handle(rep, err)
		}
		// Cache feeds: idle and size eviction, then the short sweep of everything
		// freed by it, a tag overwrite or a cleanup. Independent of pressure.
		crep, cerr := s.CacheTick(ctx)
		if handle != nil && ctx.Err() == nil && (cerr != nil || len(crep.Evicted) > 0 || crep.Swept > 0) {
			handle(PressureReport{State: "cache", Evicted: crep.Evicted, EvictedVersions: len(crep.Evicted), EvictedBytes: crep.Bytes, Swept: crep.Swept}, cerr)
		}
		timer.Reset(interval)
	}
}
