package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

// Entry-member cache of proxied immutable reads (routing cache: true). Only
// a member with a local feed bound to the key caches, and a hit is served
// only to callers this member's own policy lets read that whole feed
// (feed-wide artifact read): the holder's authorization is not reused for
// anyone else.
//
// Only digest-addressed reads (OCI blobs and manifests by digest, verified
// against the digest while they stream) are served from the cache as they
// are: their bytes cannot change behind the path. Any other cached read
// (package archives under a stable name, which a holder may delete or a
// mutable format may overwrite) is stored only with a strong ETag and is
// revalidated with a holder on every hit (If-None-Match): a 304 serves the
// cached bytes, a 200 replaces them, a 404 or 410 drops the entry, and no
// reachable holder means no answer from the cache either. Deletes replicated
// from the home drop every entry of the feed, and package metadata changes
// (yanks, deprecations) drop every entry that is not digest-addressed
// (InvalidateRouteCacheTx, called by replication in the applying
// transaction). Entries expire after cache_ttl_seconds; expired rows are
// purged by the federation worker and their blobs collected by the normal
// two-phase GC (federation_route_cache rows are GC roots while they exist).

const (
	routeCacheDomain   = "federation-route-cache"
	routeCacheMaxBytes = 8 << 30
	routeCacheMaxKey   = 4096
)

// routeCacheAddressedSQL matches the request keys contentAddressed accepts.
const routeCacheAddressedSQL = `request_key ~ '^oci/.+/(blobs|manifests)/sha256:[0-9a-f]{64}$'`

// InvalidateRouteCacheTx drops the entry-cache rows of every feed that
// attaches source: all of them (a delete replicated from the home), or only
// those that are not digest-addressed (package metadata changed, e.g. a
// yank). It runs in the caller's transaction.
func InvalidateRouteCacheTx(ctx context.Context, tx pgx.Tx, instanceID, sourceID string, all bool) error {
	q := `DELETE FROM federation_route_cache WHERE instance_id=$1 AND feed_id IN (SELECT feed_id FROM feed_sources WHERE instance_id=$1 AND source_id=$2)`
	if !all {
		q += ` AND NOT (` + routeCacheAddressedSQL + `)`
	}
	_, err := tx.Exec(ctx, q, instanceID, sourceID)
	return err
}

func (s *Service) cacheDomain(ctx context.Context) (string, error) {
	var id string
	err := s.cfg.DB.QueryRow(ctx, `INSERT INTO dedup_domains(instance_id,name) VALUES($1,$2) ON CONFLICT(instance_id,name) DO UPDATE SET name=excluded.name RETURNING id::text`,
		s.cfg.InstanceID, routeCacheDomain).Scan(&id)
	return id, err
}

// cacheWriter stages a proxied answer while it streams to the client. Only
// a complete, length-matching (and, for digest paths, digest-matching)
// transfer is recorded.
type cacheWriter struct {
	s         *Service
	ctx       context.Context
	feed      string
	key       string
	member    string
	mediaType string
	etag      string
	ttl       time.Duration
	domain    string
	upload    blob.Upload
	hash      hash.Hash
	expected  int64
	want      string
	failed    bool
}

// strongETag reports an ETag usable for revalidation (strong, bounded).
func strongETag(etag string) bool {
	return len(etag) >= 2 && len(etag) <= 256 && etag[0] == '"' && etag[len(etag)-1] == '"'
}

func (s *Service) newCacheWriter(ctx context.Context, target routeTarget, rest string, m Member, resp *http.Response) *cacheWriter {
	rt := s.routeRT.Load()
	key := target.format + "/" + rest
	if rt == nil || rt.Storage == nil || rt.Lease == nil || len(key) > routeCacheMaxKey || resp.ContentLength > routeCacheMaxBytes {
		return nil
	}
	want := contentAddressed(target.format, rest)
	etag := resp.Header.Get("ETag")
	if !strongETag(etag) {
		if want == "" {
			return nil // not digest-addressed and cannot be revalidated: never cached
		}
		etag = ""
	}
	domain, err := s.cacheDomain(ctx)
	if err != nil {
		return nil
	}
	store, err := rt.Storage(ctx, domain)
	if err != nil || store == nil {
		return nil
	}
	// The staged upload outlives a client that stops reading at
	// Content-Length; abort and the length check bound it.
	upload, err := store.Stage(context.WithoutCancel(ctx))
	if err != nil {
		return nil
	}
	ttl := DefaultRouteCacheTTL
	if r := target.feed.EffectiveRouting(); r.CacheTTLSeconds > 0 {
		ttl = time.Duration(r.CacheTTLSeconds) * time.Second
	}
	mt := resp.Header.Get("Content-Type")
	if len(mt) > 256 {
		mt = "application/octet-stream"
	}
	return &cacheWriter{s: s, ctx: context.WithoutCancel(ctx), feed: target.localFeed, key: key, member: m.InstanceID, mediaType: mt, etag: etag, ttl: ttl, domain: domain,
		upload: upload, hash: sha256.New(), expected: resp.ContentLength, want: want}
}

func (c *cacheWriter) Write(p []byte) (int, error) {
	if c.failed {
		return len(p), nil
	}
	c.hash.Write(p)
	if _, err := c.upload.Write(p); err != nil {
		c.abort()
	}
	return len(p), nil // caching never fails the client transfer
}

func (c *cacheWriter) abort() {
	if !c.failed {
		c.failed = true
		_ = c.upload.Abort()
	}
}

// finish records the entry after a complete transfer (in the background:
// the client already has every byte).
func (c *cacheWriter) finish(n int64) {
	if c.failed || n != c.expected {
		c.abort()
		return
	}
	digest := hex.EncodeToString(c.hash.Sum(nil))
	if c.want != "" && digest != c.want {
		c.abort()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 60*time.Second)
		defer cancel()
		info := blob.Info{Digest: blob.Digest(digest), Size: n}
		if _, err := c.upload.Finalize(ctx, info); err != nil {
			_ = c.upload.Abort()
			return
		}
		if c.s.commitCache(ctx, c, info) == nil {
			c.s.route.cacheStores.Add(1)
		}
	}()
}

func (s *Service) commitCache(ctx context.Context, c *cacheWriter, info blob.Info) error {
	rt := s.routeRT.Load()
	if rt == nil || rt.Lease == nil {
		return ErrNotConfigured
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		lease := rt.Lease()
		if lease.InstanceID != s.cfg.InstanceID {
			return ownership.ErrInvalid
		}
		if err := ownership.Require(ctx, tx, lease); err != nil {
			return err
		}
		var blobID string
		var size int64
		// Upserting the blob row takes its lock, so a concurrent GC sweep of
		// the same digest waits and then sees the cache reference.
		if err := tx.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text,size_bytes`,
			s.cfg.InstanceID, c.domain, string(info.Digest), info.Size).Scan(&blobID, &size); err != nil {
			return err
		}
		if size != info.Size {
			return blob.ErrIntegrity
		}
		_, err := tx.Exec(ctx, `INSERT INTO federation_route_cache(instance_id,feed_id,request_key,blob_id,media_type,etag,member_instance_id,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT(instance_id,feed_id,request_key) DO UPDATE SET blob_id=excluded.blob_id,media_type=excluded.media_type,etag=excluded.etag,member_instance_id=excluded.member_instance_id,created_at=now(),expires_at=excluded.expires_at`,
			s.cfg.InstanceID, c.feed, c.key, blobID, c.mediaType, c.etag, c.member, s.now().Add(c.ttl))
		return err
	})
}

// cacheEntry is an entry-cache row usable for a read.
type cacheEntry struct {
	key       string
	digest    string // blob digest (hex)
	size      int64
	domain    string
	mediaType string
	etag      string
	addressed bool // digest-addressed: served without revalidation
}

// lookupCached returns the cache entry for a read. It reports false when the
// read must be routed (no entry, caching off, a Range read, or the caller
// may not read the whole feed here).
func (s *Service) lookupCached(r *http.Request, target routeTarget, rest string, c caller) (cacheEntry, bool) {
	rt := s.routeRT.Load()
	if rt == nil || rt.Storage == nil || target.localFeed == "" || !target.feed.EffectiveRouting().Cache || !immutableRead(target.format, rest) ||
		c.principal.PrincipalID == "" || r.Header.Get("Range") != "" {
		return cacheEntry{}, false
	}
	e := cacheEntry{key: target.format + "/" + rest}
	if r.URL.RawQuery != "" {
		e.key += "?" + r.URL.RawQuery
	}
	if len(e.key) > routeCacheMaxKey {
		return cacheEntry{}, false
	}
	ctx := r.Context()
	policy, err := (identity.Roles{DB: s.cfg.DB}).Resolve(ctx, c.principal)
	if err != nil || !policy.Authorize(c.principal, identity.Resource{InstanceID: s.cfg.InstanceID, FeedID: target.localFeed, Kind: identity.ArtifactResource}, identity.Read) {
		return cacheEntry{}, false
	}
	err = s.cfg.DB.QueryRow(ctx, `SELECT b.digest,b.size_bytes,b.dedup_domain_id::text,c.media_type,c.etag FROM federation_route_cache c JOIN blobs b ON b.id=c.blob_id
WHERE c.instance_id=$1 AND c.feed_id=$2 AND c.request_key=$3 AND c.expires_at>$4`, s.cfg.InstanceID, target.localFeed, e.key, s.now()).Scan(&e.digest, &e.size, &e.domain, &e.mediaType, &e.etag)
	if err != nil {
		return cacheEntry{}, false
	}
	e.addressed = contentAddressed(target.format, strings.TrimPrefix(e.key, target.format+"/")) == e.digest && e.digest != ""
	if !e.addressed && !strongETag(e.etag) {
		return cacheEntry{}, false // cannot be revalidated: route the read
	}
	store, err := rt.Storage(ctx, e.domain)
	if err != nil || store == nil {
		return cacheEntry{}, false
	}
	info := blob.Info{Digest: blob.Digest(e.digest), Size: e.size}
	if got, err := store.Stat(ctx, info.Digest); err != nil || got != info {
		return cacheEntry{}, false
	}
	return e, true
}

// dropCached deletes an entry a holder no longer serves.
func (s *Service) dropCached(target routeTarget, e cacheEntry) {
	_, _ = s.cfg.DB.Exec(context.Background(), `DELETE FROM federation_route_cache WHERE instance_id=$1 AND feed_id=$2 AND request_key=$3`, s.cfg.InstanceID, target.localFeed, e.key)
	s.route.cacheDrops.Add(1)
}

// writeCached answers a read from a cache entry (route names the answer in
// X-Kuvryn-Route). The body transfer runs under the routing transfer guard.
func (s *Service) writeCached(w http.ResponseWriter, r *http.Request, target routeTarget, e cacheEntry, route string) {
	ctx := r.Context()
	h := w.Header()
	for _, k := range []string{"Content-Length", "Content-Range", "Content-Encoding", "Etag", "Last-Modified", "Content-Type", "Docker-Content-Digest", "Location", "Link"} {
		h.Del(k)
	}
	if e.mediaType != "" {
		h.Set("Content-Type", e.mediaType)
	}
	if e.etag != "" {
		h.Set("ETag", e.etag)
	}
	if target.format == "oci" {
		h.Set("Docker-Content-Digest", "sha256:"+e.digest)
	}
	hardenProxied(h)
	h.Set("X-Kuvryn-Cache", "hit")
	h.Set(headerRoute, route)
	s.route.cacheHits.Add(1)
	_, _ = s.cfg.DB.Exec(context.WithoutCancel(ctx), `UPDATE federation_route_cache SET last_hit_at=$4 WHERE instance_id=$1 AND feed_id=$2 AND request_key=$3`, s.cfg.InstanceID, target.localFeed, e.key, s.now())
	if e.etag != "" && r.Header.Get("If-None-Match") == e.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(e.size, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	var body io.ReadCloser
	if rt := s.routeRT.Load(); rt != nil && rt.Storage != nil {
		if store, err := rt.Storage(ctx, e.domain); err == nil && store != nil {
			body, _ = store.OpenRange(ctx, blob.Digest(e.digest), 0, -1)
		}
	}
	if body == nil {
		h.Del("Content-Length")
		routeError(w, http.StatusServiceUnavailable, "route_unavailable", "the cached copy is unavailable; retry later")
		return
	}
	defer func() { _ = body.Close() }()
	guard := s.newTransferGuard()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(guard.deadline(0))
	w.WriteHeader(http.StatusOK)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := body.Read(buf)
		if k > 0 {
			_ = rc.SetWriteDeadline(guard.deadline(n + int64(k)))
			if _, werr := w.Write(buf[:k]); werr != nil {
				return
			}
			n += int64(k)
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// purgeRouteCache drops expired cache entries (bounded per run); their blobs
// are collected by GC.
func (s *Service) purgeRouteCache(ctx context.Context) {
	_, _ = s.cfg.DB.Exec(ctx, `DELETE FROM federation_route_cache WHERE ctid IN (SELECT ctid FROM federation_route_cache WHERE instance_id=$1 AND expires_at<$2 LIMIT 5000)`, s.cfg.InstanceID, s.now())
}
