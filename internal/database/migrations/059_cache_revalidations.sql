-- Cache efficiency distinguishes three outcomes per remote lookup: served from
-- cache (cache_hits, now including artifacts read from storage), served from
-- cache after an upstream 304 (cache_revalidations) and transferred from
-- upstream (cache_misses).
ALTER TABLE traffic_buckets DROP CONSTRAINT traffic_buckets_metric_check;
ALTER TABLE traffic_buckets ADD CONSTRAINT traffic_buckets_metric_check CHECK(metric IN ('pulls','publishes','bytes_out','bytes_in','cache_hits','cache_misses','cache_revalidations','errors'));
