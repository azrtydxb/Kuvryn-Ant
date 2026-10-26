package management

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
	"github.com/azrtydxb/Kuvryn-Ant/internal/retention"
	"github.com/jackc/pgx/v5"
)

// FeedStats are derived on demand from metadata; Pulls comes from the batched
// traffic counters (it lags live traffic by one flush interval).
type FeedStats struct {
	FeedID   string `json:"feed_id"`
	Slug     string `json:"slug"`
	Format   string `json:"format"`
	Packages int64  `json:"packages"`
	Versions int64  `json:"versions"`
	Assets   int64  `json:"assets"`
	// Bytes counts each distinct blob once, however many assets reference it.
	Bytes int64 `json:"bytes"`
	Pulls int64 `json:"pulls"`
	// SourceIDs lists the attached sources the caller may read, in read order, so
	// list views can show a feed's upstreams without one request per feed.
	SourceIDs []string `json:"source_ids"`
}
type FeedStatsPage struct {
	Items []FeedStats  `json:"items"`
	Page  api.PageInfo `json:"page"`
}
type SourceStats struct {
	SourceID      string `json:"source_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Format        string `json:"format"`
	Packages      int64  `json:"packages"`
	Versions      int64  `json:"versions"`
	Assets        int64  `json:"assets"`
	Bytes         int64  `json:"bytes"`
	AttachedFeeds int64  `json:"attached_feeds"`
	CacheHits     int64  `json:"cache_hits"`
	CacheMisses   int64  `json:"cache_misses"`
	// CacheRevalidations are upstream 304s served from the cached copy.
	CacheRevalidations int64 `json:"cache_revalidations"`
}
type SourceStatsPage struct {
	Items []SourceStats `json:"items"`
	Page  api.PageInfo  `json:"page"`
}

func (h opsHandlers) feedStats(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	pg, ok := h.page(w, r, p, "feed-stats")
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	ids, more, err := h.permittedIDs(ctx, p, pg, `SELECT id::text FROM feeds WHERE instance_id=$1 AND ($2='' OR id>$2::uuid) ORDER BY id`, func(id string) bool { return feedAllowed(policy, p, id, identity.Read) })
	items := []FeedStats{}
	if err == nil {
		var rows pgx.Rows
		rows, err = h.db.Query(ctx, `SELECT f.id::text,f.slug,f.format,
 (SELECT count(*) FROM components c JOIN feed_sources fs ON fs.source_id=c.source_id WHERE fs.feed_id=f.id),
 (SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE NOT v.blob_only AND fs.feed_id=f.id),
 (SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE fs.feed_id=f.id),
 COALESCE((SELECT sum(size_bytes) FROM blobs b WHERE b.id IN (SELECT a.blob_id FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE fs.feed_id=f.id)),0)::bigint,
 COALESCE((SELECT sum(value) FROM traffic_buckets t WHERE t.instance_id=f.instance_id AND t.scope='feed' AND t.ref=f.id::text AND t.metric='pulls'),0)::bigint
 FROM feeds f WHERE f.instance_id=$1 AND f.id=ANY($2::uuid[]) ORDER BY f.id`, p.InstanceID, ids)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var it FeedStats
				if err = rows.Scan(&it.FeedID, &it.Slug, &it.Format, &it.Packages, &it.Versions, &it.Assets, &it.Bytes, &it.Pulls); err != nil {
					break
				}
				it.SourceIDs = []string{}
				items = append(items, it)
			}
			if err == nil {
				err = rows.Err()
			}
			if err == nil {
				rows.Close()
				err = h.attachFeedSourceIDs(ctx, p, policy, items)
			}
		}
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "feed statistics unavailable")
		return
	}
	info, err := h.pageInfo(pg, ids, more)
	if err != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "cannot encode page cursor")
		return
	}
	JSON(w, 200, FeedStatsPage{Items: items, Page: info})
}

func (h opsHandlers) sourceStats(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	pg, ok := h.page(w, r, p, "source-stats")
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	ids, more, err := h.permittedIDs(ctx, p, pg, `SELECT id::text FROM sources WHERE instance_id=$1 AND ($2='' OR id>$2::uuid) ORDER BY id`, func(id string) bool { return sourceAllowed(policy, p, id, identity.Read) })
	items := []SourceStats{}
	if err == nil {
		rows, qerr := h.db.Query(ctx, `SELECT s.id::text,s.name,s.kind,s.format,
 (SELECT count(*) FROM components c WHERE c.source_id=s.id),
 (SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE NOT v.blob_only AND c.source_id=s.id),
 (SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=s.id),
 COALESCE((SELECT sum(size_bytes) FROM blobs b WHERE b.id IN (SELECT a.blob_id FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=s.id)),0)::bigint,
 (SELECT count(*) FROM feed_sources fs WHERE fs.source_id=s.id),
 COALESCE((SELECT sum(value) FROM traffic_buckets t WHERE t.instance_id=s.instance_id AND t.scope='source' AND t.ref=s.id::text AND t.metric='cache_hits'),0)::bigint,
 COALESCE((SELECT sum(value) FROM traffic_buckets t WHERE t.instance_id=s.instance_id AND t.scope='source' AND t.ref=s.id::text AND t.metric='cache_misses'),0)::bigint,
 COALESCE((SELECT sum(value) FROM traffic_buckets t WHERE t.instance_id=s.instance_id AND t.scope='source' AND t.ref=s.id::text AND t.metric='cache_revalidations'),0)::bigint
 FROM sources s WHERE s.instance_id=$1 AND s.id=ANY($2::uuid[]) ORDER BY s.id`, p.InstanceID, ids)
		err = qerr
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var it SourceStats
				if err = rows.Scan(&it.SourceID, &it.Name, &it.Kind, &it.Format, &it.Packages, &it.Versions, &it.Assets, &it.Bytes, &it.AttachedFeeds, &it.CacheHits, &it.CacheMisses, &it.CacheRevalidations); err != nil {
					break
				}
				items = append(items, it)
			}
			if err == nil {
				err = rows.Err()
			}
		}
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "source statistics unavailable")
		return
	}
	info, err := h.pageInfo(pg, ids, more)
	if err != nil {
		Error(w, r, 500, api.ErrorCodeInternalError, "cannot encode page cursor")
		return
	}
	JSON(w, 200, SourceStatsPage{Items: items, Page: info})
}

// DomainUsage is blob usage inside one deduplication domain.
type DomainUsage struct {
	DomainID  string `json:"domain_id"`
	Name      string `json:"name"`
	BlobCount int64  `json:"blob_count"`
	BlobBytes int64  `json:"blob_bytes"`
}

// StorageUsage is derived from blob metadata, so it reflects committed blobs
// and costs no object-store listing. StagingBytes is present only for backends
// with local staging.
type StorageUsage struct {
	Backend string `json:"backend"`
	// BlobBytes is the physical size of all committed blobs; LogicalBytes is the
	// size as seen through assets (a blob shared by N assets counts N times).
	BlobCount    int64 `json:"blob_count"`
	BlobBytes    int64 `json:"blob_bytes"`
	LogicalBytes int64 `json:"logical_bytes"`
	// DedupSavedBytes is LogicalBytes minus the physical bytes of referenced blobs.
	DedupSavedBytes int64 `json:"dedup_saved_bytes"`
	// UnreferencedBytes belong to blobs with no asset reference (GC candidates after the grace period).
	UnreferencedBytes int64 `json:"unreferenced_bytes"`
	GCCandidates      int64 `json:"gc_candidates"`
	// ReclaimableBytes are unreferenced bytes garbage collection will free
	// (already marked, or old enough to be marked); InGraceBytes are unreferenced
	// bytes still inside the grace period, which is why unreferenced_bytes can be
	// large while gc_candidates is 0.
	ReclaimableBytes *int64                    `json:"reclaimable_bytes,omitempty"`
	InGraceBytes     *int64                    `json:"in_grace_bytes,omitempty"`
	Pressure         *retention.PressureStatus `json:"pressure,omitempty"`
	// CacheFeeds lists the cache feeds with the bytes each holds.
	CacheFeeds   []CacheFeedUsage `json:"cache_feeds"`
	StagingBytes *int64           `json:"staging_bytes,omitempty"`
	Domains      []DomainUsage    `json:"domains"`
	MeasuredAt   time.Time        `json:"measured_at"`
}

func (h opsHandlers) storageUsage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	out := StorageUsage{Backend: h.d.StorageBackend, Domains: []DomainUsage{}, CacheFeeds: []CacheFeedUsage{}, MeasuredAt: time.Now().UTC()}
	err := h.db.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size_bytes),0)::bigint,
 COALESCE((SELECT sum(b.size_bytes) FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.instance_id=$1),0)::bigint,
 COALESCE(sum(size_bytes) FILTER (WHERE NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=blobs.id)),0)::bigint,
 (SELECT count(*) FROM gc_candidates g WHERE g.instance_id=$1)
 FROM blobs WHERE instance_id=$1`, p.InstanceID).Scan(&out.BlobCount, &out.BlobBytes, &out.LogicalBytes, &out.UnreferencedBytes, &out.GCCandidates)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
		return
	}
	if h.d.StorageMigrator != nil {
		if m := h.d.StorageMigrator(p.InstanceID); m != nil {
			if rt, rerr := m.Route(ctx); rerr == nil {
				out.Backend = rt.Active
			}
		}
	}
	out.DedupSavedBytes = max(0, out.LogicalBytes-(out.BlobBytes-out.UnreferencedBytes))
	rows, err := h.db.Query(ctx, `SELECT d.id::text,d.name,count(b.id),COALESCE(sum(b.size_bytes),0)::bigint FROM dedup_domains d LEFT JOIN blobs b ON b.dedup_domain_id=d.id WHERE d.instance_id=$1 GROUP BY d.id,d.name ORDER BY 4 DESC,d.id LIMIT 100`, p.InstanceID)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var d DomainUsage
		if err = rows.Scan(&d.DomainID, &d.Name, &d.BlobCount, &d.BlobBytes); err != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
			return
		}
		out.Domains = append(out.Domains, d)
	}
	if rows.Err() != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
		return
	}
	if svc := h.retentionService(p.InstanceID); svc != nil {
		if st, perr := svc.PressureStatus(ctx); perr == nil {
			grace := max(0, out.UnreferencedBytes-st.ReclaimableBytes)
			out.ReclaimableBytes, out.InGraceBytes, out.Pressure = &st.ReclaimableBytes, &grace, &st
		}
	}
	crows, cerr := h.db.Query(ctx, `SELECT f.id::text,f.slug,f.format,f.cache_max_idle_seconds,f.cache_max_bytes,
 COALESCE((SELECT sum(b.size_bytes) FROM blobs b WHERE b.id IN (SELECT a.blob_id FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE fs.feed_id=f.id)),0)::bigint AS bytes,
 (SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE NOT v.blob_only AND fs.feed_id=f.id) AS versions
 FROM feeds f WHERE f.instance_id=$1 AND f.purpose='cache' ORDER BY bytes DESC,f.slug LIMIT 100`, p.InstanceID)
	if cerr != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
		return
	}
	defer crows.Close()
	for crows.Next() {
		var u CacheFeedUsage
		if cerr = crows.Scan(&u.FeedID, &u.Slug, &u.Format, &u.MaxIdleSeconds, &u.MaxBytes, &u.Bytes, &u.Versions); cerr != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
			return
		}
		out.CacheFeeds = append(out.CacheFeeds, u)
	}
	if crows.Err() != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
		return
	}
	if h.d.StagingBytes != nil {
		if n, ok := h.d.StagingBytes(ctx); ok {
			out.StagingBytes = &n
		}
	}
	JSON(w, 200, out)
}

// TrafficPoint holds counters for one time bucket. CacheHitRatio is the share
// of remote lookups served without transferring from upstream (hits plus 304
// revalidations); it is absent when there were no cache lookups.
type TrafficPoint struct {
	Bucket      time.Time `json:"bucket"`
	Pulls       int64     `json:"pulls"`
	Publishes   int64     `json:"publishes"`
	BytesOut    int64     `json:"bytes_out"`
	BytesIn     int64     `json:"bytes_in"`
	CacheHits   int64     `json:"cache_hits"`
	CacheMisses int64     `json:"cache_misses"`
	// CacheRevalidations are upstream 304s served from the cached copy.
	CacheRevalidations int64    `json:"cache_revalidations"`
	Errors             int64    `json:"errors"`
	CacheHitRatio      *float64 `json:"cache_hit_ratio,omitempty"`
}
type TrafficSeries struct {
	Scope       string         `json:"scope"`
	Ref         string         `json:"ref,omitempty"`
	From        time.Time      `json:"from"`
	To          time.Time      `json:"to"`
	StepSeconds int64          `json:"step_seconds"`
	Points      []TrafficPoint `json:"points"`
	Totals      TrafficPoint   `json:"totals"`
}

func (t *TrafficPoint) add(metric string, v int64) {
	switch metric {
	case observability.MetricPulls:
		t.Pulls += v
	case observability.MetricPublishes:
		t.Publishes += v
	case observability.MetricBytesOut:
		t.BytesOut += v
	case observability.MetricBytesIn:
		t.BytesIn += v
	case observability.MetricCacheHits:
		t.CacheHits += v
	case observability.MetricCacheMisses:
		t.CacheMisses += v
	case observability.MetricCacheRevalidations:
		t.CacheRevalidations += v
	case observability.MetricErrors:
		t.Errors += v
	}
}
func (t *TrafficPoint) ratio() {
	t.CacheHitRatio = cacheRatio(t.CacheHits, t.CacheRevalidations, t.CacheMisses)
}

// cacheRatio is the share of lookups served from cache, or nil without lookups.
func cacheRatio(hits, revalidations, misses int64) *float64 {
	n := hits + revalidations + misses
	if n == 0 {
		return nil
	}
	v := float64(hits+revalidations) / float64(n)
	return &v
}

var trafficSteps = map[string]time.Duration{"5m": 5 * time.Minute, "1h": time.Hour, "1d": 24 * time.Hour}

func (h opsHandlers) traffic(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, v := range q {
		if (k != "scope" && k != "ref" && k != "from" && k != "to" && k != "step") || len(v) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "unknown or repeated query parameter")
			return
		}
	}
	scope := q.Get("scope")
	if scope == "" {
		scope = "instance"
	}
	ref := q.Get("ref")
	stepName := q.Get("step")
	if stepName == "" {
		stepName = "1h"
	}
	step, okStep := trafficSteps[stepName]
	now := time.Now().UTC()
	to, from := now, now.Add(-24*time.Hour)
	var err error
	if v := q.Get("to"); v != "" {
		to, err = time.Parse(time.RFC3339, v)
	}
	if v := q.Get("from"); v != "" && err == nil {
		from, err = time.Parse(time.RFC3339, v)
	} else if q.Get("to") != "" && q.Get("from") == "" {
		from = to.Add(-24 * time.Hour)
	}
	if err != nil || !okStep || !to.After(from) || to.Sub(from)/step > 2000 || (scope != "instance" && scope != "feed" && scope != "source") || (scope == "instance") != (ref == "") {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid traffic query")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	switch scope {
	case "instance":
		ok = policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin)
	case "feed":
		ref, ok = feedID(ref)
		ok = ok && feedAllowed(policy, p, ref, identity.Read)
	case "source":
		ref, ok = feedID(ref)
		ok = ok && sourceAllowed(policy, p, ref, identity.Read)
	}
	if !ok {
		if scope == "instance" {
			Error(w, r, 403, api.ErrorCodeForbidden, "instance administration denied")
		} else {
			Error(w, r, 404, api.ErrorCodeNotFound, "traffic scope not found")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT date_bin(make_interval(secs=>$6),bucket,TIMESTAMPTZ 'epoch'),metric,sum(value)::bigint FROM traffic_buckets
 WHERE instance_id=$1 AND scope=$2 AND ref=$3 AND bucket>=$4 AND bucket<$5 GROUP BY 1,2 ORDER BY 1`, p.InstanceID, scope, ref, from, to, step.Seconds())
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "traffic metrics unavailable")
		return
	}
	defer rows.Close()
	out := TrafficSeries{Scope: scope, Ref: ref, From: from, To: to, StepSeconds: int64(step.Seconds()), Points: []TrafficPoint{}}
	for rows.Next() {
		var bucket time.Time
		var metric string
		var value int64
		if err = rows.Scan(&bucket, &metric, &value); err != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "traffic metrics unavailable")
			return
		}
		if n := len(out.Points); n == 0 || !out.Points[n-1].Bucket.Equal(bucket) {
			out.Points = append(out.Points, TrafficPoint{Bucket: bucket.UTC()})
		}
		out.Points[len(out.Points)-1].add(metric, value)
		out.Totals.add(metric, value)
	}
	if rows.Err() != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "traffic metrics unavailable")
		return
	}
	for i := range out.Points {
		out.Points[i].ratio()
	}
	out.Totals.Bucket = from
	out.Totals.ratio()
	JSON(w, 200, out)
}

// attachFeedSourceIDs fills SourceIDs for a page of feeds with one query. Only
// sources the caller may read are listed, ordered by feed read priority.
func (h opsHandlers) attachFeedSourceIDs(ctx context.Context, p identity.Principal, policy identity.Policy, items []FeedStats) error {
	if len(items) == 0 {
		return nil
	}
	index := make(map[string]int, len(items))
	ids := make([]string, 0, len(items))
	for i, it := range items {
		index[it.FeedID] = i
		ids = append(ids, it.FeedID)
	}
	rows, err := h.db.Query(ctx, `SELECT fs.feed_id::text,fs.source_id::text FROM feed_sources fs WHERE fs.instance_id=$1 AND fs.feed_id=ANY($2::uuid[]) ORDER BY fs.feed_id,fs.priority`, p.InstanceID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var feed, source string
		if err = rows.Scan(&feed, &source); err != nil {
			return err
		}
		if i, ok := index[feed]; ok && sourceAllowed(policy, p, source, identity.Read) {
			items[i].SourceIDs = append(items[i].SourceIDs, source)
		}
	}
	return rows.Err()
}

// SourceCacheStats is one remote source's cache efficiency over a window.
type SourceCacheStats struct {
	SourceID           string   `json:"source_id"`
	Name               string   `json:"name"`
	Format             string   `json:"format"`
	CacheHits          int64    `json:"cache_hits"`
	CacheRevalidations int64    `json:"cache_revalidations"`
	CacheMisses        int64    `json:"cache_misses"`
	CacheHitRatio      *float64 `json:"cache_hit_ratio,omitempty"`
}

// CacheStats breaks the instance cache ratio down per remote source, worst
// ratio first, so poorly caching upstreams stand out.
type CacheStats struct {
	From   time.Time          `json:"from"`
	To     time.Time          `json:"to"`
	Items  []SourceCacheStats `json:"items"`
	Totals SourceCacheStats   `json:"totals"`
}

// maxCacheStatsSources bounds the per-source breakdown; instances have tens of
// remote sources, not thousands.
const maxCacheStatsSources = 500

// maxCacheStatsWindow matches traffic counter retention.
const maxCacheStatsWindow = 400 * 24 * time.Hour

func (h opsHandlers) cacheStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, v := range q {
		if (k != "from" && k != "to") || len(v) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "unknown or repeated query parameter")
			return
		}
	}
	to := time.Now().UTC()
	from := to.Add(-24 * time.Hour)
	var err error
	if v := q.Get("to"); v != "" {
		to, err = time.Parse(time.RFC3339, v)
		from = to.Add(-24 * time.Hour)
	}
	if v := q.Get("from"); v != "" && err == nil {
		from, err = time.Parse(time.RFC3339, v)
	}
	if err != nil || !to.After(from) || to.Sub(from) > maxCacheStatsWindow {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid cache statistics window")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT s.id::text,s.name,s.format,
 COALESCE(sum(t.value) FILTER (WHERE t.metric='cache_hits'),0)::bigint,
 COALESCE(sum(t.value) FILTER (WHERE t.metric='cache_revalidations'),0)::bigint,
 COALESCE(sum(t.value) FILTER (WHERE t.metric='cache_misses'),0)::bigint
 FROM sources s LEFT JOIN traffic_buckets t ON t.instance_id=s.instance_id AND t.scope='source' AND t.ref=s.id::text AND t.bucket>=$2 AND t.bucket<$3 AND t.metric IN ('cache_hits','cache_revalidations','cache_misses')
 WHERE s.instance_id=$1 AND s.kind='remote' GROUP BY s.id,s.name,s.format ORDER BY s.name,s.id LIMIT $4`, p.InstanceID, from, to, maxCacheStatsSources)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "cache statistics unavailable")
		return
	}
	defer rows.Close()
	out := CacheStats{From: from, To: to, Items: []SourceCacheStats{}}
	for rows.Next() {
		var it SourceCacheStats
		if err = rows.Scan(&it.SourceID, &it.Name, &it.Format, &it.CacheHits, &it.CacheRevalidations, &it.CacheMisses); err != nil {
			Error(w, r, 503, api.ErrorCodeUnavailable, "cache statistics unavailable")
			return
		}
		if !sourceAllowed(policy, p, it.SourceID, identity.Read) {
			continue
		}
		it.CacheHitRatio = cacheRatio(it.CacheHits, it.CacheRevalidations, it.CacheMisses)
		out.Totals.CacheHits += it.CacheHits
		out.Totals.CacheRevalidations += it.CacheRevalidations
		out.Totals.CacheMisses += it.CacheMisses
		out.Items = append(out.Items, it)
	}
	if rows.Err() != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "cache statistics unavailable")
		return
	}
	// Worst first; sources without lookups go last.
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i].CacheHitRatio, out.Items[j].CacheHitRatio
		if (a == nil) != (b == nil) {
			return b == nil
		}
		return a != nil && *a < *b
	})
	out.Totals.CacheHitRatio = cacheRatio(out.Totals.CacheHits, out.Totals.CacheRevalidations, out.Totals.CacheMisses)
	JSON(w, 200, out)
}
