package replication

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

// PurgePolicy bounds the authority-side change history. The history is trimmed
// to the more restrictive of the two bounds; zero disables a bound.
type PurgePolicy struct {
	MaxAge  time.Duration
	MaxRows int64
}

// PurgeResult reports one purge pass.
type PurgeResult struct {
	LowWater int64 // new low-water mark (highest purged sequence)
	Raised   bool  // the mark moved in this pass
	Deleted  int64 // change rows physically removed
}

const purgeBatch = 5000

// PurgeHistory trims replication_changes for the instance and raises the
// low-water mark. The mark is raised and committed before any row is removed,
// so a reader can never observe a hole it was not told about: a consumer whose
// cursor is below the mark gets ErrHistoryPurged and must reseed. The mark
// change is fenced by the instance writer lease; row removal is idempotent.
//
// Only rows strictly older than MaxAge are removed by the age bound (the
// boundary is derived from the oldest retained row, never from newest purged).
func (e Exporter) PurgeHistory(ctx context.Context, instance string, p PurgePolicy) (PurgeResult, error) {
	if e.DB == nil || e.Lease == nil {
		return PurgeResult{}, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var head, low int64
	err := e.DB.QueryRow(ctx, `SELECT seq,low_water FROM replication_heads WHERE instance_id=$1`, instance).Scan(&head, &low)
	if errors.Is(err, pgx.ErrNoRows) {
		return PurgeResult{}, nil
	}
	if err != nil {
		return PurgeResult{}, ErrUnavailable
	}
	target := low
	if p.MaxRows > 0 && head-p.MaxRows > target {
		target = head - p.MaxRows
	}
	if p.MaxAge > 0 {
		var firstKept *int64
		err = e.DB.QueryRow(ctx, `SELECT min(seq) FROM replication_changes WHERE instance_id=$1 AND seq IS NOT NULL AND seq<=$2 AND created_at>=clock_timestamp()-$3::bigint*interval '1 millisecond'`, instance, head, p.MaxAge.Milliseconds()).Scan(&firstKept)
		if err != nil {
			return PurgeResult{}, ErrUnavailable
		}
		age := head
		if firstKept != nil {
			age = *firstKept - 1
		}
		if age > target {
			target = age
		}
	}
	if target > head {
		target = head
	}
	res := PurgeResult{LowWater: low}
	if target > low {
		tx, err := e.DB.Begin(ctx)
		if err != nil {
			return res, ErrUnavailable
		}
		defer rollback(tx)
		if err = ownership.Require(ctx, tx, e.Lease()); err != nil {
			return res, err
		}
		var now int64
		err = tx.QueryRow(ctx, `UPDATE replication_heads SET low_water=GREATEST(low_water,$2),purged_at=clock_timestamp() WHERE instance_id=$1 RETURNING low_water`, instance, target).Scan(&now)
		if err != nil {
			return res, ErrUnavailable
		}
		if err = tx.Commit(ctx); err != nil {
			return res, ErrUnavailable
		}
		res.LowWater, res.Raised = now, now > low
	}
	for {
		tag, err := e.DB.Exec(ctx, `DELETE FROM replication_changes WHERE id IN (SELECT id FROM replication_changes WHERE instance_id=$1 AND seq IS NOT NULL AND seq<=$2 ORDER BY seq LIMIT $3)`, instance, res.LowWater, purgeBatch)
		if err != nil {
			return res, ErrUnavailable
		}
		n := tag.RowsAffected()
		res.Deleted += n
		if n < purgeBatch {
			break
		}
	}
	if res.Deleted > 0 {
		_, _ = e.DB.Exec(ctx, `UPDATE replication_heads SET purged_total=purged_total+$2 WHERE instance_id=$1`, instance, res.Deleted)
	}
	return res, nil
}

// RunPurge trims the change history every interval until ctx ends. Start it
// only in the process that holds the instance writer lease.
func (e Exporter) RunPurge(ctx context.Context, instance string, p PurgePolicy, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 || (p.MaxAge <= 0 && p.MaxRows <= 0) {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		res, err := e.PurgeHistory(ctx, instance, p)
		switch {
		case err != nil && ctx.Err() == nil && logger != nil:
			logger.Warn("replication history purge failed", "reason", err)
		case err == nil && res.Raised && logger != nil:
			logger.Info("replication history purged", "low_water", res.LowWater, "rows", res.Deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
