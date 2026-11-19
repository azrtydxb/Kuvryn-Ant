package main

import (
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"
)

// poolCollector exposes the PostgreSQL pool so that a request that waits for a
// connection can be told apart from one that waits for an upstream: a growing
// ant_db_pool_empty_acquire_total with acquired == max means the pool is the
// bottleneck, while idle connections and shed upstream slots mean it is not.
func poolCollector(pool *pgxpool.Pool) func(io.Writer) {
	return func(w io.Writer) {
		s := pool.Stat()
		fmt.Fprintln(w, "# HELP ant_db_pool_connections PostgreSQL pool connections by state.\n# TYPE ant_db_pool_connections gauge")
		fmt.Fprintf(w, "ant_db_pool_connections{state=\"acquired\"} %d\nant_db_pool_connections{state=\"idle\"} %d\nant_db_pool_connections{state=\"constructing\"} %d\nant_db_pool_connections{state=\"total\"} %d\nant_db_pool_connections{state=\"max\"} %d\n", s.AcquiredConns(), s.IdleConns(), s.ConstructingConns(), s.TotalConns(), s.MaxConns())
		fmt.Fprintln(w, "# HELP ant_db_pool_acquire_total Connection acquisitions.\n# TYPE ant_db_pool_acquire_total counter")
		fmt.Fprintf(w, "ant_db_pool_acquire_total %d\n", s.AcquireCount())
		fmt.Fprintln(w, "# HELP ant_db_pool_empty_acquire_total Acquisitions that had to wait because no connection was idle.\n# TYPE ant_db_pool_empty_acquire_total counter")
		fmt.Fprintf(w, "ant_db_pool_empty_acquire_total %d\n", s.EmptyAcquireCount())
		fmt.Fprintln(w, "# HELP ant_db_pool_wait_seconds_total Time spent waiting for a connection.\n# TYPE ant_db_pool_wait_seconds_total counter")
		fmt.Fprintf(w, "ant_db_pool_wait_seconds_total %g\n", s.EmptyAcquireWaitTime().Seconds())
		fmt.Fprintln(w, "# HELP ant_db_pool_canceled_acquire_total Acquisitions abandoned (context canceled or timed out) while waiting.\n# TYPE ant_db_pool_canceled_acquire_total counter")
		fmt.Fprintf(w, "ant_db_pool_canceled_acquire_total %d\n", s.CanceledAcquireCount())
	}
}
