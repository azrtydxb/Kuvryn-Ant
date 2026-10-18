package replication

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/observability"
)

// transferBytes counts every blob byte received from peers by this process,
// including bytes of attempts that later failed or were resumed.
var transferBytes atomic.Int64

func recordTransferBytes(n int64) {
	if n > 0 {
		transferBytes.Add(n)
	}
}

// RegisterMetrics exposes replication lag, bytes transferred and failures on
// the Prometheus endpoint. Per-job values come from the durable job rows at
// scrape time, so counters stay monotonic across restarts and match the status
// API exactly. Job names are operator-chosen identifiers; no peer URLs, tokens
// or feed identifiers are emitted. The returned function removes the collector.
func (s *Service) RegisterMetrics() func() {
	return observability.RegisterCollector("replication", func(w io.Writer) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		jobs, err := s.ListJobs(ctx, s.cfg.InstanceID)
		fmt.Fprintln(w, "# HELP ant_replication_transfer_bytes_total Blob bytes received from replication peers by this process.\n# TYPE ant_replication_transfer_bytes_total counter")
		fmt.Fprintf(w, "ant_replication_transfer_bytes_total %d\n", transferBytes.Load())
		if err != nil {
			fmt.Fprintln(w, "# HELP ant_replication_scrape_errors_total Replication job state could not be read for this scrape.\n# TYPE ant_replication_scrape_errors_total counter\nant_replication_scrape_errors_total 1")
			return
		}
		type series struct{ name, kind, help string }
		def := []series{
			{"ant_replication_lag_seconds", "gauge", "Age of the oldest change the job has not applied yet (0 when caught up)."},
			{"ant_replication_changes_behind", "gauge", "Peer changes not yet applied."},
			{"ant_replication_seconds_since_last_success", "gauge", "Seconds since the last successful run (-1 if never)."},
			{"ant_replication_running", "gauge", "1 while a worker holds the job."},
			{"ant_replication_consecutive_failures", "gauge", "Failed runs since the last success."},
			{"ant_replication_failures_total", "counter", "Failed runs."},
			{"ant_replication_bytes_transferred_total", "counter", "Blob bytes received for the job."},
			{"ant_replication_blobs_transferred_total", "counter", "Blobs transferred for the job."},
			{"ant_replication_blobs_deduplicated_total", "counter", "Blobs skipped because they were already present."},
			{"ant_replication_resumed_bytes_total", "counter", "Staged bytes that resumed transfers did not fetch again."},
			{"ant_replication_reseeds_total", "counter", "Automatic reseeds after the peer purged needed history."},
			{"ant_replication_backfills_pending", "gauge", "Attached sources queued for back-fill."},
			{"ant_replication_conflicts_total", "counter", "Items left untouched because destination content took precedence."},
		}
		value := func(j Job, name string) float64 {
			st := j.Status
			switch name {
			case "ant_replication_lag_seconds":
				if st.LagSeconds != nil {
					return *st.LagSeconds
				}
			case "ant_replication_changes_behind":
				if st.ChangesBehind != nil {
					return float64(*st.ChangesBehind)
				}
			case "ant_replication_seconds_since_last_success":
				if st.LastSuccessAt == nil {
					return -1
				}
				return time.Since(*st.LastSuccessAt).Seconds()
			case "ant_replication_running":
				if st.Running {
					return 1
				}
			case "ant_replication_consecutive_failures":
				return float64(st.ConsecutiveFailures)
			case "ant_replication_failures_total":
				return float64(st.FailuresTotal)
			case "ant_replication_bytes_transferred_total":
				return float64(st.BytesTransferred)
			case "ant_replication_blobs_transferred_total":
				return float64(st.BlobsTransferred)
			case "ant_replication_blobs_deduplicated_total":
				return float64(st.BlobsDeduplicated)
			case "ant_replication_resumed_bytes_total":
				return float64(st.ResumedBytes)
			case "ant_replication_reseeds_total":
				return float64(st.Reseeds)
			case "ant_replication_backfills_pending":
				return float64(st.BackfillsPending)
			case "ant_replication_conflicts_total":
				return float64(st.Conflicts)
			}
			return 0
		}
		for _, d := range def {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.name, d.help, d.name, d.kind)
			for _, j := range jobs {
				fmt.Fprintf(w, "%s{job=%s} %g\n", d.name, observability.Label(j.Name), value(j, d.name))
			}
		}
	})
}
