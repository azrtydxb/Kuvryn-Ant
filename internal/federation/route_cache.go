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
// anyone else. Entries expire after cache_ttl_seconds; expired rows are
// purged by the federation worker and their blobs collected by the normal
// two-phase GC (federation_route_cache rows are GC roots while they exist).

const (
	routeCacheDomain   = "federation-route-cache"
	routeCacheMaxBytes = 8 << 30
	routeCacheMaxKey   = 4096
)

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

func digestInPath(rest string) string {
	p, _, _ := strings.Cut(rest, "?")
	i := strings.LastIndex(p, "sha256:")
	if i < 0 {
		return ""
	}
	d := p[i+len("sha256:"):]
	if len(d) != 64 || strings.ToLower(d) != d || !isHex(d) {
		return ""
	}
	return d
}

func (s *Service) newCacheWriter(ctx context.Context, target routeTarget, rest string, m Member, resp *http.Response) *cacheWriter {
	rt := s.routeRT.Load()
	key := target.format + "/" + rest
	if rt == nil || rt.Storage == nil || rt.Lease == nil || len(key) > routeCacheMaxKey || resp.ContentLength > routeCacheMaxBytes {
		return nil
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
	etag := resp.Header.Get("ETag")
	if len(etag) > 256 {
		etag = ""
	}
	return &cacheWriter{s: s, ctx: context.WithoutCancel(ctx), feed: target.localFeed, key: key, member: m.InstanceID, mediaType: mt, etag: etag, ttl: ttl, domain: domain,
		upload: upload, hash: sha256.New(), expected: resp.ContentLength, want: digestInPath(rest)}
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

// serveCached answers a read from the entry cache. It returns false when
// the read must be routed (no entry, caching off, or the caller may not read
// the whole feed here).
func (s *Service) serveCached(w http.ResponseWriter, r *http.Request, target routeTarget, rest string, c caller) bool {
	rt := s.routeRT.Load()
	if rt == nil || rt.Storage == nil || target.localFeed == "" || !target.feed.EffectiveRouting().Cache || !immutableRead(target.format, rest) ||
		c.principal.PrincipalID == "" {
		return false
	}
	key := target.format + "/" + rest
	if r.URL.RawQuery != "" {
		key += "?" + r.URL.RawQuery
	}
	if len(key) > routeCacheMaxKey {
		return false
	}
	ctx := r.Context()
	policy, err := (identity.Roles{DB: s.cfg.DB}).Resolve(ctx, c.principal)
	if err != nil || !policy.Authorize(c.principal, identity.Resource{InstanceID: s.cfg.InstanceID, FeedID: target.localFeed, Kind: identity.ArtifactResource}, identity.Read) {
		return false
	}
	var digest, domain, mediaType, etag string
	var size int64
	err = s.cfg.DB.QueryRow(ctx, `SELECT b.digest,b.size_bytes,b.dedup_domain_id::text,c.media_type,c.etag FROM federation_route_cache c JOIN blobs b ON b.id=c.blob_id
WHERE c.instance_id=$1 AND c.feed_id=$2 AND c.request_key=$3 AND c.expires_at>$4`, s.cfg.InstanceID, target.localFeed, key, s.now()).Scan(&digest, &size, &domain, &mediaType, &etag)
	if err != nil {
		return false
	}
	store, err := rt.Storage(ctx, domain)
	if err != nil || store == nil {
		return false
	}
	info := blob.Info{Digest: blob.Digest(digest), Size: size}
	if got, err := store.Stat(ctx, info.Digest); err != nil || got != info {
		return false
	}
	h := w.Header()
	if mediaType != "" {
		h.Set("Content-Type", mediaType)
	}
	if etag != "" {
		h.Set("ETag", etag)
	}
	if target.format == "oci" {
		h.Set("Docker-Content-Digest", "sha256:"+digest)
	}
	h.Set("X-Kuvryn-Cache", "hit")
	h.Set(headerRoute, "cache")
	s.route.cacheHits.Add(1)
	_, _ = s.cfg.DB.Exec(context.WithoutCancel(ctx), `UPDATE federation_route_cache SET last_hit_at=$4 WHERE instance_id=$1 AND feed_id=$2 AND request_key=$3`, s.cfg.InstanceID, target.localFeed, key, s.now())
	if etag != "" && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}
	body, err := store.OpenRange(ctx, info.Digest, 0, -1)
	if err != nil {
		h.Del("Content-Length")
		return false
	}
	defer func() { _ = body.Close() }()
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true
			}
		}
		if errors.Is(err, io.EOF) {
			return true
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
