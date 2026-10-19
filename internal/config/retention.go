package config

import (
	"errors"
	"strconv"
	"strings"
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

	// PressureInterval is how often the cheap storage-pressure check runs; 0
	// disables the worker (on-demand and scheduled runs still evaluate it).
	PressureInterval time.Duration
	// PressureSweepGrace is how long a blob freed by pressure eviction waits
	// before it is swept (the normal GC grace does not apply to it).
	PressureSweepGrace time.Duration

	// Deployment-provided overrides of the stored instance settings. A nil
	// pointer leaves the stored value in control.
	DefaultEnabled  *bool
	DefaultKeepLast *int
	DefaultMaxAge   *time.Duration
	DefaultMaxIdle  *time.Duration
	PressureEnabled *bool
	HighPercent     *int
	LowPercent      *int
	CapacityBytes   *int64
}

func loadRetention(getenv func(string) string) (Retention, error) {
	r := Retention{Interval: 6 * time.Hour, Grace: 24 * time.Hour, BatchSize: 500, IntegrityBatch: 200, IntegrityMaxBytes: 1 << 30, PressureInterval: 5 * time.Minute, PressureSweepGrace: 15 * time.Minute}
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
	if err = loadPressure(getenv, &r); err != nil {
		return Retention{}, err
	}
	return r, nil
}

func loadPressure(getenv func(string) string, r *Retention) error {
	var err error
	if v := getenv("ANT_PRESSURE_INTERVAL"); v != "" {
		if r.PressureInterval, err = time.ParseDuration(v); err != nil || r.PressureInterval != 0 && (r.PressureInterval < time.Minute || r.PressureInterval > 24*time.Hour) {
			return errors.New("ANT_PRESSURE_INTERVAL must be 0 or between 1m and 24h")
		}
	}
	if v := getenv("ANT_PRESSURE_SWEEP_GRACE"); v != "" {
		if r.PressureSweepGrace, err = time.ParseDuration(v); err != nil || r.PressureSweepGrace < time.Minute || r.PressureSweepGrace > 24*time.Hour {
			return errors.New("ANT_PRESSURE_SWEEP_GRACE must be between 1m and 24h")
		}
	}
	boolean := func(name string) (*bool, error) {
		v := getenv(name)
		if v == "" {
			return nil, nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, errors.New(name + " is invalid")
		}
		return &b, nil
	}
	if r.PressureEnabled, err = boolean("ANT_PRESSURE_ENABLED"); err != nil {
		return err
	}
	if r.DefaultEnabled, err = boolean("ANT_RETENTION_DEFAULT_ENABLED"); err != nil {
		return err
	}
	percent := func(name string) (*int, error) {
		v := getenv(name)
		if v == "" {
			return nil, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 99 {
			return nil, errors.New(name + " must be between 1 and 99")
		}
		return &n, nil
	}
	if r.HighPercent, err = percent("ANT_PRESSURE_HIGH_PERCENT"); err != nil {
		return err
	}
	if r.LowPercent, err = percent("ANT_PRESSURE_LOW_PERCENT"); err != nil {
		return err
	}
	if r.HighPercent != nil && r.LowPercent != nil && *r.LowPercent >= *r.HighPercent {
		return errors.New("ANT_PRESSURE_LOW_PERCENT must be below ANT_PRESSURE_HIGH_PERCENT")
	}
	if v := getenv("ANT_RETENTION_DEFAULT_KEEP_LAST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000000 {
			return errors.New("ANT_RETENTION_DEFAULT_KEEP_LAST must be between 1 and 1000000")
		}
		r.DefaultKeepLast = &n
	}
	for name, dst := range map[string]**time.Duration{"ANT_RETENTION_DEFAULT_MAX_AGE": &r.DefaultMaxAge, "ANT_RETENTION_DEFAULT_MAX_IDLE": &r.DefaultMaxIdle} {
		if v := getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < time.Hour || d > 100*365*24*time.Hour {
				return errors.New(name + " must be a duration between 1h and 876000h")
			}
			*dst = &d
		}
	}
	if v := getenv("ANT_STORAGE_CAPACITY_BYTES"); v != "" {
		n, err := ParseSize(v)
		if err != nil || n < 1<<20 {
			return errors.New("ANT_STORAGE_CAPACITY_BYTES must be a size of at least 1MiB (bytes, or with a KiB/MiB/GiB/TiB or KB/MB/GB/TB suffix)")
		}
		r.CapacityBytes = &n
	}
	return nil
}

// ParseSize parses a byte count: a plain integer, or an integer or decimal with
// a binary (KiB, MiB, GiB, TiB) or decimal (KB, MB, GB, TB) suffix.
func ParseSize(v string) (int64, error) {
	s := strings.TrimSpace(v)
	units := []struct {
		suffix string
		scale  float64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1}}
	scale := 1.0
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, scale = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.scale
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f*scale > 9e18 {
		return 0, errors.New("invalid size")
	}
	return int64(f * scale), nil
}
