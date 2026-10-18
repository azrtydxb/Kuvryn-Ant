package observability

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BucketSize is the granularity of persisted traffic counters.
const BucketSize = 5 * time.Minute

const (
	maxTrafficKeys = 20000
	maxDownloads   = 50000
	retainBuckets  = 400 * 24 * time.Hour
)

// Traffic metric names (see migration 022 traffic_buckets.metric).
const (
	MetricPulls       = "pulls"
	MetricPublishes   = "publishes"
	MetricBytesOut    = "bytes_out"
	MetricBytesIn     = "bytes_in"
	MetricCacheHits   = "cache_hits"
	MetricCacheMisses = "cache_misses"
	MetricErrors      = "errors"
)

type trafficKey struct {
	bucket           int64 // unix seconds, truncated to BucketSize
	scope, ref, name string
}

// Traffic accumulates package-protocol traffic counters in memory and flushes
// them to PostgreSQL in batches. The serving hot path performs one short
// mutex-guarded map update per request and never touches the database.
type Traffic struct {
	mu        sync.Mutex
	counters  map[trafficKey]int64
	downloads map[string]int64
	lastPrune time.Time
	now       func() time.Time
}

func NewTraffic() *Traffic {
	return &Traffic{counters: map[trafficKey]int64{}, downloads: map[string]int64{}, now: time.Now}
}

func (t *Traffic) add(scope, ref, name string, n int64) {
	if n <= 0 {
		return
	}
	t.addKey(trafficKey{t.now().Truncate(BucketSize).Unix(), scope, ref, name}, n)
}

func (t *Traffic) addKey(k trafficKey, n int64) {
	if _, ok := t.counters[k]; !ok && len(t.counters) >= maxTrafficKeys {
		return
	}
	t.counters[k] += n
}

// Download counts one successful read of an asset.
func (t *Traffic) Download(assetID string) {
	if t == nil || assetID == "" {
		return
	}
	t.mu.Lock()
	if _, ok := t.downloads[assetID]; ok || len(t.downloads) < maxDownloads {
		t.downloads[assetID]++
	}
	t.mu.Unlock()
}

// Cache counts a remote-source cache outcome for a source.
func (t *Traffic) Cache(source string, hit bool) {
	if t == nil || source == "" {
		return
	}
	name := MetricCacheMisses
	if hit {
		name = MetricCacheHits
	}
	t.mu.Lock()
	t.add("source", source, name, 1)
	t.add("instance", "", name, 1)
	t.mu.Unlock()
}

// feedSlug extracts the feed slug from a package-protocol request path. Feed
// slugs are recorded as "feed" refs prefixed with "slug:" until Flush resolves
// them to feed identifiers.
func feedSlug(path string) (string, bool) {
	var rest string
	switch {
	case strings.HasPrefix(path, "/feeds/"):
		rest = path[len("/feeds/"):]
	case strings.HasPrefix(path, "/v2/"):
		rest = path[len("/v2/"):]
	default:
		return "", false
	}
	slug, _, _ := strings.Cut(rest, "/")
	if slug == "" || len(slug) > 63 {
		return "", false
	}
	return slug, true
}

type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *countingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *countingWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Middleware classifies completed package-protocol requests (paths under
// /feeds/{slug}/ and /v2/) into pulls, publishes, bytes and errors. Management
// (/api/), console and probe routes are ignored. It does not read bodies,
// headers other than Content-Length, query strings or principals.
func (t *Traffic) Middleware(next http.Handler) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slug, ok := feedSlug(r.URL.Path)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		cw := &countingWriter{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		status := cw.status
		if status == 0 {
			status = http.StatusOK
		}
		t.record(slug, r.Method, r.URL.Path, status, cw.bytes, r.ContentLength)
	})
}

func (t *Traffic) record(slug, method, path string, status int, out, in int64) {
	success := status >= 200 && status < 400
	var pulls, publishes int64
	switch {
	case !success:
	case method == http.MethodGet && status != http.StatusNotModified:
		pulls = 1
	case (method == http.MethodPut || method == http.MethodPost) && status < 300:
		// An OCI push is many uploads; only the manifest PUT completes an artifact.
		publishes = 1
		if strings.HasPrefix(path, "/v2/") && !(method == http.MethodPut && strings.Contains(path, "/manifests/")) {
			publishes = 0
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if status >= 500 {
		t.add("instance", "", MetricErrors, 1)
	}
	if pulls+publishes == 0 {
		return
	}
	ref := "slug:" + slug
	t.add("feed", ref, MetricPulls, pulls)
	t.add("instance", "", MetricPulls, pulls)
	t.add("feed", ref, MetricPublishes, publishes)
	t.add("instance", "", MetricPublishes, publishes)
	if pulls > 0 {
		t.add("feed", ref, MetricBytesOut, out)
		t.add("instance", "", MetricBytesOut, out)
	}
	if publishes > 0 {
		t.add("feed", ref, MetricBytesIn, in)
		t.add("instance", "", MetricBytesIn, in)
	}
}

// Flush persists and clears accumulated counters. On failure the counts are
// merged back so a later flush retries them.
func (t *Traffic) Flush(ctx context.Context, db *pgxpool.Pool, instance string) error {
	if t == nil || db == nil {
		return nil
	}
	t.mu.Lock()
	counters, downloads := t.counters, t.downloads
	t.counters, t.downloads = map[trafficKey]int64{}, map[string]int64{}
	prune := t.now().Sub(t.lastPrune) > time.Hour
	if prune {
		t.lastPrune = t.now()
	}
	t.mu.Unlock()
	err := t.persist(ctx, db, instance, counters, downloads, prune)
	if err != nil {
		t.mu.Lock()
		for k, v := range counters {
			t.addKey(k, v)
		}
		for id, n := range downloads {
			if _, ok := t.downloads[id]; ok || len(t.downloads) < maxDownloads {
				t.downloads[id] += n
			}
		}
		t.mu.Unlock()
	}
	return err
}

func (t *Traffic) persist(ctx context.Context, db *pgxpool.Pool, instance string, counters map[trafficKey]int64, downloads map[string]int64, prune bool) error {
	if len(counters) > 0 {
		slugs := []string{}
		for k := range counters {
			if strings.HasPrefix(k.ref, "slug:") {
				slugs = append(slugs, k.ref[len("slug:"):])
			}
		}
		ids := map[string]string{}
		if len(slugs) > 0 {
			rows, err := db.Query(ctx, `SELECT slug,id::text FROM feeds WHERE instance_id=$1 AND slug=ANY($2)`, instance, slugs)
			if err != nil {
				return err
			}
			for rows.Next() {
				var slug, id string
				if err = rows.Scan(&slug, &id); err != nil {
					rows.Close()
					return err
				}
				ids[slug] = id
			}
			rows.Close()
			if rows.Err() != nil {
				return rows.Err()
			}
		}
		merged := map[trafficKey]int64{}
		for k, v := range counters {
			if strings.HasPrefix(k.ref, "slug:") {
				id, ok := ids[k.ref[len("slug:"):]]
				if !ok {
					continue
				}
				k.ref = id
			}
			merged[k] += v
		}
		buckets := make([]time.Time, 0, len(merged))
		scopes, refs, names := make([]string, 0, len(merged)), make([]string, 0, len(merged)), make([]string, 0, len(merged))
		values := make([]int64, 0, len(merged))
		for k, v := range merged {
			buckets = append(buckets, time.Unix(k.bucket, 0).UTC())
			scopes, refs, names, values = append(scopes, k.scope), append(refs, k.ref), append(names, k.name), append(values, v)
		}
		if len(values) > 0 {
			_, err := db.Exec(ctx, `INSERT INTO traffic_buckets(instance_id,bucket,scope,ref,metric,value)
 SELECT $1,t.b,t.s,t.r,t.m,t.v FROM unnest($2::timestamptz[],$3::text[],$4::text[],$5::text[],$6::bigint[]) AS t(b,s,r,m,v)
 ON CONFLICT(instance_id,scope,ref,metric,bucket) DO UPDATE SET value=traffic_buckets.value+excluded.value`, instance, buckets, scopes, refs, names, values)
			if err != nil {
				return err
			}
		}
	}
	if len(downloads) > 0 {
		ids, counts := make([]string, 0, len(downloads)), make([]int64, 0, len(downloads))
		for id, n := range downloads {
			ids, counts = append(ids, id), append(counts, n)
		}
		_, err := db.Exec(ctx, `INSERT INTO asset_downloads(asset_id,instance_id,downloads,last_downloaded_at)
 SELECT a.id,$1,t.n,now() FROM unnest($2::uuid[],$3::bigint[]) AS t(id,n) JOIN assets a ON a.id=t.id AND a.instance_id=$1
 ON CONFLICT(asset_id) DO UPDATE SET downloads=asset_downloads.downloads+excluded.downloads,last_downloaded_at=excluded.last_downloaded_at`, instance, ids, counts)
		if err != nil {
			return err
		}
	}
	if prune {
		_, _ = db.Exec(ctx, `DELETE FROM traffic_buckets WHERE instance_id=$1 AND bucket<$2`, instance, time.Now().Add(-retainBuckets))
	}
	return nil
}

// Run flushes every interval until ctx ends, then once more.
func (t *Traffic) Run(ctx context.Context, db *pgxpool.Pool, instance string, interval time.Duration) {
	if t == nil || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = t.Flush(flush, db, instance)
			cancel()
			return
		case <-ticker.C:
			flush, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = t.Flush(flush, db, instance)
			cancel()
		}
	}
}
