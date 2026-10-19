-- Phase 2 P2-M5: placement-aware routing (docs/planning/phase-2-federation.md
-- §8, §11). Routing settings live in the signed topology document; the only
-- table is the optional entry-member cache of proxied immutable reads
-- (routing cache: true). Additive, unused until a feed key enables caching.
--
-- A row references a blob in the instance's federation-route-cache dedup
-- domain and is a GC root while it exists. Expired rows are purged by the
-- federation worker; the blob is then collected by the normal two-phase GC.
CREATE TABLE federation_route_cache (
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_id uuid NOT NULL,
 request_key text NOT NULL CHECK(octet_length(request_key) BETWEEN 1 AND 4096),
 blob_id uuid NOT NULL REFERENCES blobs(id) ON DELETE CASCADE,
 media_type text NOT NULL DEFAULT '' CHECK(octet_length(media_type)<=256),
 etag text NOT NULL DEFAULT '' CHECK(octet_length(etag)<=256),
 member_instance_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 last_hit_at timestamptz,
 PRIMARY KEY(instance_id,feed_id,request_key),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);
CREATE INDEX federation_route_cache_expiry_idx ON federation_route_cache(instance_id,expires_at);
CREATE INDEX federation_route_cache_blob_idx ON federation_route_cache(blob_id);
