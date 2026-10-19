// Package cacherelease holds the cache-feed release logic that both the
// artifact core (tag overwrite, prefix cleanup) and internal/retention
// (idle and size eviction) need: delete versions that nothing references any
// more together with the layers only they referenced, and queue the freed blobs
// for the short garbage-collection sweep. It is a leaf package so the two do
// not import each other.
package cacherelease

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// DefaultSweepGrace mirrors retention.DefaultPressureSweepGrace.
	DefaultSweepGrace = 15 * time.Minute
	// DefaultGCGrace mirrors retention.DefaultGrace.
	DefaultGCGrace = 24 * time.Hour
)

func interval(d time.Duration) string { return fmt.Sprintf("%d microseconds", d.Microseconds()) }

// ExpediteBlobs marks the blobs among blobIDs that no asset or federation route cache
// entry references as garbage-collection candidates that become sweepable
// after sweepGrace instead of the full grace period (gcGrace is that full
// grace, used only to keep an earlier existing wait). It returns the bytes of
// the blobs it marked. It runs in the caller's transaction, which has just
// removed the references, and changes no reference itself: the sweep still
// locks the blobs row and re-checks references before it deletes anything.
func ExpediteBlobs(ctx context.Context, tx pgx.Tx, blobIDs []string, sweepGrace, gcGrace time.Duration) (int64, error) {
	if len(blobIDs) == 0 {
		return 0, nil
	}
	if sweepGrace <= 0 {
		sweepGrace = DefaultSweepGrace
	}
	if gcGrace <= 0 {
		gcGrace = DefaultGCGrace
	}
	const unreferenced = `b.id=ANY($1::uuid[]) AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.blob_id=b.id) AND NOT EXISTS(SELECT 1 FROM federation_route_cache rc WHERE rc.blob_id=b.id)`
	var freed int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint FROM blobs b WHERE `+unreferenced, blobIDs).Scan(&freed); err != nil {
		return 0, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO gc_candidates(instance_id,dedup_domain_id,digest,kind,sweep_after) SELECT b.instance_id,b.dedup_domain_id,b.digest,'row',clock_timestamp()+$2::interval FROM blobs b WHERE `+unreferenced+`
 ON CONFLICT(dedup_domain_id,digest) DO UPDATE SET sweep_after=LEAST(COALESCE(gc_candidates.sweep_after,gc_candidates.first_seen_at+$3::interval),excluded.sweep_after) WHERE gc_candidates.kind='row'`,
		blobIDs, interval(sweepGrace), interval(gcGrace))
	return freed, err
}
