package config

import (
	"errors"
	"strconv"
	"time"
)

// Retention configures the background retention, garbage-collection and
// integrity worker. A zero Interval disables the worker (the `ant gc` command
// remains available).
type Retention struct {
	Interval          time.Duration
	Grace             time.Duration
	DryRun            bool
	BatchSize         int
	IntegrityBatch    int
	IntegrityMaxBytes int64
}

func loadRetention(getenv func(string) string) (Retention, error) {
	r := Retention{Interval: 6 * time.Hour, Grace: 24 * time.Hour, BatchSize: 500, IntegrityBatch: 200, IntegrityMaxBytes: 1 << 30}
	var err error
	if v := getenv("ANT_GC_INTERVAL"); v != "" {
		if r.Interval, err = time.ParseDuration(v); err != nil || r.Interval != 0 && (r.Interval < time.Minute || r.Interval > 30*24*time.Hour) {
			return Retention{}, errors.New("ANT_GC_INTERVAL must be 0 or between 1m and 720h")
		}
	}
	if v := getenv("ANT_GC_GRACE"); v != "" {
		if r.Grace, err = time.ParseDuration(v); err != nil || r.Grace < time.Minute || r.Grace > 90*24*time.Hour {
			return Retention{}, errors.New("ANT_GC_GRACE must be between 1m and 2160h")
		}
	}
	if v := getenv("ANT_GC_DRY_RUN"); v != "" {
		if r.DryRun, err = strconv.ParseBool(v); err != nil {
			return Retention{}, errors.New("ANT_GC_DRY_RUN is invalid")
		}
	}
	if v := getenv("ANT_GC_BATCH_SIZE"); v != "" {
		if r.BatchSize, err = strconv.Atoi(v); err != nil || r.BatchSize < 1 || r.BatchSize > 100000 {
			return Retention{}, errors.New("ANT_GC_BATCH_SIZE must be between 1 and 100000")
		}
	}
	if v := getenv("ANT_INTEGRITY_BATCH"); v != "" {
		if r.IntegrityBatch, err = strconv.Atoi(v); err != nil || r.IntegrityBatch < 1 || r.IntegrityBatch > 100000 {
			return Retention{}, errors.New("ANT_INTEGRITY_BATCH must be between 1 and 100000")
		}
	}
	if v := getenv("ANT_INTEGRITY_MAX_BYTES"); v != "" {
		if r.IntegrityMaxBytes, err = strconv.ParseInt(v, 10, 64); err != nil || r.IntegrityMaxBytes < 1<<20 {
			return Retention{}, errors.New("ANT_INTEGRITY_MAX_BYTES must be at least 1048576")
		}
	}
	return r, nil
}
