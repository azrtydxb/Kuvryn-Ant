package retention

import (
	"strings"
	"testing"
)

func TestPurposeMetrics(t *testing.T) {
	saved := cacheMetricsState
	cacheMetricsState = &cacheMetrics{evictions: map[string]*cacheEvictions{}, usage: map[string]CacheUsage{}}
	defer func() { cacheMetricsState = saved }()
	cacheMetricsState.observeEviction(cacheReasonIdle, 3, 300)
	cacheMetricsState.observeEviction(cacheReasonPress, 2, 200)
	cacheMetricsState.observeUsage([]CacheUsage{{Slug: "buildcache", Bytes: 1234, MaxBytes: 5000}, {Slug: "sccache", Bytes: 99}})
	var out strings.Builder
	writePurposeMetrics(&out, 10, 1000)
	text := out.String()
	for _, want := range []string{
		`ant_storage_evicted_versions_total{purpose="cache",reason="cache_idle"} 3`,
		`ant_storage_evicted_versions_total{purpose="cache",reason="pressure_lru"} 2`,
		// The pressure totals include cache evictions; artifacts gets the rest.
		`ant_storage_evicted_versions_total{purpose="artifacts",reason="pressure_lru"} 8`,
		`ant_storage_evicted_bytes_total{purpose="artifacts",reason="pressure_lru"} 800`,
		`ant_storage_evicted_bytes_total{purpose="cache",reason="cache_idle"} 300`,
		`ant_feed_used_bytes{purpose="cache",feed="buildcache"} 1234`,
		`ant_feed_max_bytes{purpose="cache",feed="buildcache"} 5000`,
		`ant_feed_max_bytes{purpose="cache",feed="sccache"} 0`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}
