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

func TestPressureAndDefaultPolicyEnvironment(t *testing.T) {
	r, err := loadRetention(func(string) string { return "" })
	if err != nil || r.PressureInterval != 5*time.Minute || r.PressureSweepGrace != 15*time.Minute || r.PressureEnabled != nil || r.CapacityBytes != nil || r.DefaultMaxIdle != nil {
		t.Fatalf("defaults %+v %v", r, err)
	}
	env := map[string]string{
		"ANT_PRESSURE_ENABLED": "true", "ANT_PRESSURE_HIGH_PERCENT": "85", "ANT_PRESSURE_LOW_PERCENT": "75",
		"ANT_STORAGE_CAPACITY_BYTES": "500GiB", "ANT_RETENTION_DEFAULT_MAX_IDLE": "720h", "ANT_RETENTION_DEFAULT_KEEP_LAST": "20",
		"ANT_PRESSURE_INTERVAL": "10m",
	}
	r, err = loadRetention(func(k string) string { return env[k] })
	if err != nil || r.PressureEnabled == nil || !*r.PressureEnabled || *r.HighPercent != 85 || *r.LowPercent != 75 || *r.CapacityBytes != 500<<30 || *r.DefaultMaxIdle != 720*time.Hour || *r.DefaultKeepLast != 20 || r.PressureInterval != 10*time.Minute {
		t.Fatalf("parsed %+v %v", r, err)
	}
	for k, v := range map[string]string{
		"ANT_PRESSURE_ENABLED": "maybe", "ANT_PRESSURE_HIGH_PERCENT": "100", "ANT_PRESSURE_LOW_PERCENT": "0", "ANT_STORAGE_CAPACITY_BYTES": "10",
		"ANT_RETENTION_DEFAULT_MAX_IDLE": "5m", "ANT_RETENTION_DEFAULT_KEEP_LAST": "0", "ANT_PRESSURE_INTERVAL": "1s", "ANT_PRESSURE_SWEEP_GRACE": "0s",
	} {
		if _, err = loadRetention(func(n string) string {
			if n == k {
				return v
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
	// Inverted watermarks are refused at startup.
	if _, err = loadRetention(func(k string) string {
		return map[string]string{"ANT_PRESSURE_HIGH_PERCENT": "70", "ANT_PRESSURE_LOW_PERCENT": "80"}[k]
	}); err == nil {
		t.Error("low >= high accepted")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"1048576": 1 << 20, "2GiB": 2 << 30, "1.5TB": 1500000000000, "10 MB": 10000000, "100B": 100} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "-1GiB", "0", "1ZB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
