// Package maintenance holds operator-driven and scheduled metadata upkeep that
// does not belong to blob garbage collection: audit log retention and metadata
// consistency repair.
package maintenance

import (
	"context"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuditPurge deletes audit_events older than Retain for one instance.
//
// Events not yet exported are never purged: if the instance has an audit sink
// row (enabled or not), only events at or below its durable checkpoint are
// eligible, so a lagging or broken sink holds back deletion instead of losing
// evidence. Remove the sink to release the hold. Outbox rows that reference a
// purged event are removed with it (webhook delivery of an event older than
// the retention window is moot).
type AuditPurge struct {
	DB         *pgxpool.Pool
	InstanceID string
	Retain     time.Duration
	BatchSize  int
	// MaxBatches bounds one run; 0 means 200.
	MaxBatches int
	// Lease, when set, fences every deleting transaction at commit.
	Lease func() ownership.Lease
	Now   func() time.Time
}

// AuditPurgeReport describes one run.
type AuditPurgeReport struct {
	Cutoff  time.Time `json:"cutoff"`
	Deleted int64     `json:"deleted"`
	// HeldBySink counts events past the cutoff retained because the sink has
	// not exported them yet.
	HeldBySink int64 `json:"held_by_sink"`
	SinkLag    bool  `json:"sink_lagging"`
	Disabled   bool  `json:"disabled,omitempty"`
}

const exported = `(NOT EXISTS (SELECT 1 FROM audit_sinks s WHERE s.instance_id=$1)
  OR a.id<=(SELECT s.checkpoint FROM audit_sinks s WHERE s.instance_id=$1))`
const eligible = `a.instance_id=$1 AND a.created_at<$2 AND ` + exported

// Run purges in bounded batches and reports what happened.
func (p AuditPurge) Run(ctx context.Context) (AuditPurgeReport, error) {
	var report AuditPurgeReport
	if p.DB == nil || p.InstanceID == "" || p.BatchSize < 1 {
		return report, errors.New("audit purge is not configured")
	}
	if p.Retain <= 0 {
		report.Disabled = true
		return report, nil
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	report.Cutoff = now().Add(-p.Retain).UTC()
	batches := p.MaxBatches
	if batches < 1 {
		batches = 200
	}
	for i := 0; i < batches; i++ {
		n, err := p.batch(ctx, report.Cutoff)
		report.Deleted += n
		if err != nil {
			return report, err
		}
		if n < int64(p.BatchSize) {
			break
		}
	}
	err := p.DB.QueryRow(ctx, `SELECT count(*) FROM audit_events a WHERE a.instance_id=$1 AND a.created_at<$2 AND NOT `+exported, p.InstanceID, report.Cutoff).Scan(&report.HeldBySink)
	if err != nil {
		return report, err
	}
	report.SinkLag = report.HeldBySink > 0
	return report, nil
}

func (p AuditPurge) batch(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if p.Lease != nil {
		if err = ownership.Require(ctx, tx, p.Lease()); err != nil {
			return 0, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT a.id FROM audit_events a WHERE `+eligible+` ORDER BY a.id LIMIT $3 FOR UPDATE`, p.InstanceID, cutoff, p.BatchSize)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM outbox_events WHERE audit_id=ANY($1)`, ids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id=ANY($1)`, ids)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}
