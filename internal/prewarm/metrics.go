package prewarm

import (
	"fmt"
	"io"
	"sync/atomic"
)

type counters struct {
	warmed, cached, failed, skipped atomic.Int64
	bytes                           atomic.Int64
	runs                            [6]atomic.Int64
	active                          atomic.Int64
}

var metrics counters

var runStates = [6]string{StateSucceeded, StatePartial, StateFailed, StateCanceled, StateQueued, StateRunning}

func (c *counters) item(state string, bytes int64) {
	switch state {
	case ItemWarmed:
		c.warmed.Add(1)
	case ItemCached:
		c.cached.Add(1)
	case ItemFailed:
		c.failed.Add(1)
	case ItemSkipped:
		c.skipped.Add(1)
	}
	c.bytes.Add(bytes)
}

func (c *counters) run(state string) {
	for i, s := range runStates {
		if s == state {
			c.runs[i].Add(1)
		}
	}
}

// WriteMetrics writes Prometheus text-format samples for this process. Labels
// are closed sets; feeds, jobs, packages and principals never appear.
func WriteMetrics(w io.Writer) {
	fmt.Fprintln(w, "# HELP ant_prewarm_items_total Prewarm items finished, by result (warmed: fetched from upstream; cached: already present).\n# TYPE ant_prewarm_items_total counter")
	for _, r := range []struct {
		name string
		v    *atomic.Int64
	}{{ItemWarmed, &metrics.warmed}, {ItemCached, &metrics.cached}, {ItemFailed, &metrics.failed}, {ItemSkipped, &metrics.skipped}} {
		fmt.Fprintf(w, "ant_prewarm_items_total{result=%q} %d\n", r.name, r.v.Load())
	}
	fmt.Fprintln(w, "# HELP ant_prewarm_bytes_total Bytes read from feeds while prewarming.\n# TYPE ant_prewarm_bytes_total counter")
	fmt.Fprintf(w, "ant_prewarm_bytes_total %d\n", metrics.bytes.Load())
	fmt.Fprintln(w, "# HELP ant_prewarm_runs_total Prewarm runs finished, by final state.\n# TYPE ant_prewarm_runs_total counter")
	for i, s := range runStates[:4] {
		fmt.Fprintf(w, "ant_prewarm_runs_total{state=%q} %d\n", s, metrics.runs[i].Load())
	}
	fmt.Fprintln(w, "# HELP ant_prewarm_active_runs Prewarm runs executing in this process.\n# TYPE ant_prewarm_active_runs gauge")
	fmt.Fprintf(w, "ant_prewarm_active_runs %d\n", metrics.active.Load())
}
