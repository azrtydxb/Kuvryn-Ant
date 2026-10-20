package maintenance

import (
	"fmt"
	"io"
	"sync"
	"time"
)

var tokenMetrics struct {
	mu      sync.Mutex
	deleted map[[2]string]uint64 // {kind, trigger}
	runs    uint64
	last    time.Time
}

// RecordTokenPurge folds a cleanup into the process-wide counters. trigger is
// "scheduled" (worker), "admin" (console action) or "self" (a user deleting
// their own tokens).
func RecordTokenPurge(trigger string, r TokenPurgeReport) {
	m := &tokenMetrics
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleted == nil {
		m.deleted = map[[2]string]uint64{}
	}
	m.deleted[[2]string{"service", trigger}] += uint64(r.ServiceTokens)
	m.deleted[[2]string{"personal", trigger}] += uint64(r.PersonalAccessTokens)
	m.runs++
	m.last = time.Now()
}

// WriteTokenMetrics emits the token cleanup counters in Prometheus text format.
func WriteTokenMetrics(w io.Writer) {
	m := &tokenMetrics
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintln(w, "# HELP ant_token_cleanup_deleted_total Expired or revoked token rows removed by cleanup.")
	fmt.Fprintln(w, "# TYPE ant_token_cleanup_deleted_total counter")
	for _, kind := range []string{"service", "personal"} {
		for _, trigger := range []string{"scheduled", "admin", "self"} {
			fmt.Fprintf(w, "ant_token_cleanup_deleted_total{kind=%q,trigger=%q} %d\n", kind, trigger, m.deleted[[2]string{kind, trigger}])
		}
	}
	fmt.Fprintln(w, "# HELP ant_token_cleanup_runs_total Token cleanup runs completed.")
	fmt.Fprintln(w, "# TYPE ant_token_cleanup_runs_total counter")
	fmt.Fprintf(w, "ant_token_cleanup_runs_total %d\n", m.runs)
	fmt.Fprintln(w, "# HELP ant_token_cleanup_last_run_timestamp_seconds Unix time of the last cleanup run, 0 if none.")
	fmt.Fprintln(w, "# TYPE ant_token_cleanup_last_run_timestamp_seconds gauge")
	ts := int64(0)
	if !m.last.IsZero() {
		ts = m.last.Unix()
	}
	fmt.Fprintf(w, "ant_token_cleanup_last_run_timestamp_seconds %d\n", ts)
}
