package maintenance

import (
	"context"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTokenCleanupDays is the built-in retention of dead tokens.
const DefaultTokenCleanupDays = 30

// MaxTokenCleanupDays bounds the setting.
const MaxTokenCleanupDays = 3650

// TokenCleanupAction is the audit action written once per purge run.
const TokenCleanupAction = "token.cleanup"

// deadSince is the moment a token stopped working: the earlier of its expiry
// and its revocation. An active token is never "dead" before its expiry.
const deadSince = `LEAST(expires_at,COALESCE(revoked_at,expires_at))`

// TokenPurgeReport describes one purge run (or one dry-run count).
type TokenPurgeReport struct {
	Cutoff               time.Time `json:"cutoff"`
	ServiceTokens        int64     `json:"service_tokens"`
	PersonalAccessTokens int64     `json:"personal_access_tokens"`
	Disabled             bool      `json:"disabled,omitempty"`
}

// Total is the number of token rows affected.
func (r TokenPurgeReport) Total() int64 { return r.ServiceTokens + r.PersonalAccessTokens }

// TokenCleanupCutoff returns the instant before which dead tokens are eligible.
func TokenCleanupCutoff(now time.Time, days int) time.Time {
	return now.Add(-time.Duration(days) * 24 * time.Hour).UTC()
}

// CountDeadTokens counts tokens expired or revoked before cutoff.
func CountDeadTokens(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, instance string, cutoff time.Time) (TokenPurgeReport, error) {
	r := TokenPurgeReport{Cutoff: cutoff}
	err := q.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM service_tokens WHERE instance_id=$1 AND `+deadSince+`<$2),
 (SELECT count(*) FROM personal_access_tokens WHERE instance_id=$1 AND `+deadSince+`<$2)`, instance, cutoff).Scan(&r.ServiceTokens, &r.PersonalAccessTokens)
	return r, err
}

// DeleteDeadTokensTx removes up to limit dead tokens before cutoff inside the
// caller's transaction. When principal is set only that user's personal access
// tokens are considered. Audit rows are never touched.
func DeleteDeadTokensTx(ctx context.Context, tx pgx.Tx, instance, principal string, cutoff time.Time, limit int) (TokenPurgeReport, error) {
	r := TokenPurgeReport{Cutoff: cutoff}
	if principal == "" {
		tag, err := tx.Exec(ctx, `DELETE FROM service_tokens WHERE id IN (
 SELECT id FROM service_tokens WHERE instance_id=$1 AND `+deadSince+`<$2 ORDER BY expires_at LIMIT $3 FOR UPDATE SKIP LOCKED)`, instance, cutoff, limit)
		if err != nil {
			return r, err
		}
		r.ServiceTokens = tag.RowsAffected()
	}
	tag, err := tx.Exec(ctx, `DELETE FROM personal_access_tokens WHERE id IN (
 SELECT id FROM personal_access_tokens WHERE instance_id=$1 AND ($4='' OR principal_id=NULLIF($4,'')::uuid) AND `+deadSince+`<$2 ORDER BY expires_at LIMIT $3 FOR UPDATE SKIP LOCKED)`, instance, cutoff, limit, principal)
	if err != nil {
		return r, err
	}
	r.PersonalAccessTokens = tag.RowsAffected()
	return r, nil
}

// RecordTokenCleanupTx writes the single summary audit event of a purge run.
// actor is empty for the scheduled worker (system event).
func RecordTokenCleanupTx(ctx context.Context, tx pgx.Tx, instance, actor, trigger string, days int, r TokenPurgeReport) error {
	var audit int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,details) VALUES($1,NULLIF($2,'')::uuid,$3,
 jsonb_build_object('trigger',$4::text,'after_days',$5::int,'cutoff',$6::timestamptz,'service_tokens',$7::bigint,'personal_access_tokens',$8::bigint)) RETURNING id`,
		instance, actor, TokenCleanupAction, trigger, days, r.Cutoff, r.ServiceTokens, r.PersonalAccessTokens).Scan(&audit); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('trigger',$3::text,'service_tokens',$4::bigint,'personal_access_tokens',$5::bigint))`,
		audit, TokenCleanupAction, trigger, r.ServiceTokens, r.PersonalAccessTokens)
	return err
}

// EffectiveTokenCleanupDays resolves the active setting: the environment value
// wins over the stored one, which wins over the default. 0 means never.
func EffectiveTokenCleanupDays(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, instance string, env *int) (days int, source string, err error) {
	if env != nil {
		return *env, "env", nil
	}
	var stored *int32
	err = q.QueryRow(ctx, `SELECT token_cleanup_after_days FROM instance_settings WHERE instance_id=$1`, instance).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultTokenCleanupDays, "default", nil
	}
	if err != nil {
		return 0, "", err
	}
	if stored == nil {
		return DefaultTokenCleanupDays, "default", nil
	}
	return int(*stored), "stored", nil
}

// TokenPurge deletes tokens that have been expired or revoked for longer than
// the configured number of days, in small batches under the writer lease.
type TokenPurge struct {
	DB         *pgxpool.Pool
	InstanceID string
	// Days reads the effective setting on every run; 0 disables the purge.
	Days      func(context.Context) (int, error)
	BatchSize int
	// MaxBatches bounds one run; 0 means 100.
	MaxBatches int
	Lease      func() ownership.Lease
	Now        func() time.Time
}

// Run deletes in bounded batches (each its own transaction, fenced by the
// lease) and records one summary audit event if anything was removed.
func (p TokenPurge) Run(ctx context.Context) (TokenPurgeReport, error) {
	var report TokenPurgeReport
	if p.DB == nil || p.InstanceID == "" || p.Days == nil {
		return report, errors.New("token purge is not configured")
	}
	days, err := p.Days(ctx)
	if err != nil {
		return report, err
	}
	if days <= 0 {
		report.Disabled = true
		return report, nil
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	batch, batches := p.BatchSize, p.MaxBatches
	if batch < 1 {
		batch = 500
	}
	if batches < 1 {
		batches = 100
	}
	report.Cutoff = TokenCleanupCutoff(now(), days)
	var runErr error
	for i := 0; i < batches; i++ {
		var n TokenPurgeReport
		n, runErr = p.batch(ctx, report.Cutoff, batch)
		report.ServiceTokens += n.ServiceTokens
		report.PersonalAccessTokens += n.PersonalAccessTokens
		if runErr != nil || n.ServiceTokens < int64(batch) && n.PersonalAccessTokens < int64(batch) {
			break
		}
	}
	if report.Total() > 0 {
		// Recorded even when a later batch failed: the summary must match what
		// was actually deleted. It uses a fresh context so shutdown cannot drop it.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := p.summary(rctx, days, report); err != nil && runErr == nil {
			runErr = err
		}
	}
	RecordTokenPurge("scheduled", report)
	return report, runErr
}

func (p TokenPurge) batch(ctx context.Context, cutoff time.Time, limit int) (TokenPurgeReport, error) {
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return TokenPurgeReport{}, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if p.Lease != nil {
		if err = ownership.Require(ctx, tx, p.Lease()); err != nil {
			return TokenPurgeReport{}, err
		}
	}
	r, err := DeleteDeadTokensTx(ctx, tx, p.InstanceID, "", cutoff, limit)
	if err != nil {
		return TokenPurgeReport{}, err
	}
	return r, tx.Commit(ctx)
}

func (p TokenPurge) summary(ctx context.Context, days int, r TokenPurgeReport) error {
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if p.Lease != nil {
		if err = ownership.Require(ctx, tx, p.Lease()); err != nil {
			return err
		}
	}
	if err = RecordTokenCleanupTx(ctx, tx, p.InstanceID, "", "scheduled", days, r); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
