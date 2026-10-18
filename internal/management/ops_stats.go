package management

import (
	"context"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
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
 (SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id JOIN feed_sources fs ON fs.source_id=c.source_id WHERE fs.feed_id=f.id),
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
				items = append(items, it)
			}
			if err == nil {
				err = rows.Err()
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
 (SELECT count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.source_id=s.id),
 (SELECT count(*) FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=s.id),
 COALESCE((SELECT sum(size_bytes) FROM blobs b WHERE b.id IN (SELECT a.blob_id FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=s.id)),0)::bigint,
 (SELECT count(*) FROM feed_sources fs WHERE fs.source_id=s.id),
 COALESCE((SELECT sum(value) FROM traffic_buckets t WHERE t.instance_id=s.instance_id AND t.scope='source' AND t.ref=s.id::text AND t.metric='cache_hits'),0)::bigint,
 COALESCE((SELECT sum(value) FROM traffic_buckets t WHERE t.instance_id=s.instance_id AND t.scope='source' AND t.ref=s.id::text AND t.metric='cache_misses'),0)::bigint
 FROM sources s WHERE s.instance_id=$1 AND s.id=ANY($2::uuid[]) ORDER BY s.id`, p.InstanceID, ids)
		err = qerr
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var it SourceStats
				if err = rows.Scan(&it.SourceID, &it.Name, &it.Kind, &it.Format, &it.Packages, &it.Versions, &it.Assets, &it.Bytes, &it.AttachedFeeds, &it.CacheHits, &it.CacheMisses); err != nil {
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
	UnreferencedBytes int64         `json:"unreferenced_bytes"`
	GCCandidates      int64         `json:"gc_candidates"`
	StagingBytes      *int64        `json:"staging_bytes,omitempty"`
	Domains           []DomainUsage `json:"domains"`
	MeasuredAt        time.Time     `json:"measured_at"`
}

func (h opsHandlers) storageUsage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.admin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	out := StorageUsage{Backend: h.d.StorageBackend, Domains: []DomainUsage{}, MeasuredAt: time.Now().UTC()}
	err := h.db.QueryRow(ctx, `SELECT count(*),COALESCE(sum(size_bytes),0)::bigint,
 COALESCE((SELECT sum(b.size_bytes) FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.instance_id=$1),0)::bigint,
 COALESCE(sum(size_bytes) FILTER (WHERE NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=blobs.id)),0)::bigint,
 (SELECT count(*) FROM gc_candidates g WHERE g.instance_id=$1)
 FROM blobs WHERE instance_id=$1`, p.InstanceID).Scan(&out.BlobCount, &out.BlobBytes, &out.LogicalBytes, &out.UnreferencedBytes, &out.GCCandidates)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "storage usage unavailable")
		return
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
	if h.d.StagingBytes != nil {
		if n, ok := h.d.StagingBytes(ctx); ok {
			out.StagingBytes = &n
		}
	}
	JSON(w, 200, out)
}

// TrafficPoint holds counters for one time bucket. CacheHitRatio is absent
// when there were no cache lookups.
type TrafficPoint struct {
	Bucket        time.Time `json:"bucket"`
	Pulls         int64     `json:"pulls"`
	Publishes     int64     `json:"publishes"`
	BytesOut      int64     `json:"bytes_out"`
	BytesIn       int64     `json:"bytes_in"`
	CacheHits     int64     `json:"cache_hits"`
	CacheMisses   int64     `json:"cache_misses"`
	Errors        int64     `json:"errors"`
	CacheHitRatio *float64  `json:"cache_hit_ratio,omitempty"`
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
	case observability.MetricErrors:
		t.Errors += v
	}
}
func (t *TrafficPoint) ratio() {
	if n := t.CacheHits + t.CacheMisses; n > 0 {
		v := float64(t.CacheHits) / float64(n)
		t.CacheHitRatio = &v
	}
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
