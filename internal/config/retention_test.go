package config

import (
	"testing"
	"time"
)

func TestRetentionSettings(t *testing.T) {
	r, err := loadRetention(func(string) string { return "" })
	if err != nil || r.Interval != 6*time.Hour || r.Grace != 24*time.Hour || r.DryRun {
		t.Fatalf("defaults %+v %v", r, err)
	}
	env := map[string]string{"ANT_GC_INTERVAL": "0", "ANT_GC_GRACE": "48h", "ANT_GC_DRY_RUN": "true", "ANT_GC_BATCH_SIZE": "10", "ANT_INTEGRITY_BATCH": "5", "ANT_INTEGRITY_MAX_BYTES": "2097152"}
	r, err = loadRetention(func(k string) string { return env[k] })
	if err != nil || r.Interval != 0 || r.Grace != 48*time.Hour || !r.DryRun || r.BatchSize != 10 || r.IntegrityBatch != 5 || r.IntegrityMaxBytes != 2<<20 {
		t.Fatalf("parsed %+v %v", r, err)
	}
	for k, v := range map[string]string{"ANT_GC_INTERVAL": "5s", "ANT_GC_GRACE": "1s", "ANT_GC_DRY_RUN": "maybe", "ANT_GC_BATCH_SIZE": "0", "ANT_INTEGRITY_BATCH": "x", "ANT_INTEGRITY_MAX_BYTES": "1"} {
		if _, err = loadRetention(func(n string) string {
			if n == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}
