package config

import (
	"errors"
	"strconv"
	"time"
)

// ReplicationHistory bounds the authority-side change history. Whichever bound
// trims more wins; a zero Interval disables purging (history then grows
// without bound).
type ReplicationHistory struct {
	MaxAge   time.Duration
	MaxRows  int64
	Interval time.Duration
}

func loadReplicationHistory(getenv func(string) string) (ReplicationHistory, error) {
	h := ReplicationHistory{MaxAge: 14 * 24 * time.Hour, MaxRows: 10_000_000, Interval: time.Hour}
	var err error
	if v := getenv("ANT_REPLICATION_HISTORY_MAX_AGE"); v != "" {
		if h.MaxAge, err = time.ParseDuration(v); err != nil || h.MaxAge < time.Minute || h.MaxAge > 3650*24*time.Hour {
			return ReplicationHistory{}, errors.New("ANT_REPLICATION_HISTORY_MAX_AGE must be between 1m and 87600h")
		}
	}
	if v := getenv("ANT_REPLICATION_HISTORY_MAX_ROWS"); v != "" {
		if h.MaxRows, err = strconv.ParseInt(v, 10, 64); err != nil || h.MaxRows < 1000 {
			return ReplicationHistory{}, errors.New("ANT_REPLICATION_HISTORY_MAX_ROWS must be at least 1000")
		}
	}
	if v := getenv("ANT_REPLICATION_PURGE_INTERVAL"); v != "" {
		if h.Interval, err = time.ParseDuration(v); err != nil || h.Interval != 0 && (h.Interval < time.Second || h.Interval > 30*24*time.Hour) {
			return ReplicationHistory{}, errors.New("ANT_REPLICATION_PURGE_INTERVAL must be 0 or between 1s and 720h")
		}
	}
	return h, nil
}
