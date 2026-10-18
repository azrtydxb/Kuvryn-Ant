// Package ownership provides PostgreSQL-backed instance writer leases and
// transaction fencing. All metadata writers must call Require in their mutation
// transaction; owning a lease alone does not fence writes outside that protocol.
package ownership

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MinTTL = time.Second
const MaxTTL = 5 * time.Minute

var (
	ErrInvalid     = errors.New("invalid writer lease request")
	ErrHeld        = errors.New("instance writer lease is held")
	ErrLost        = errors.New("writer lease is missing, expired, or superseded")
	ErrUnavailable = errors.New("writer lease storage unavailable")
)

type Lease struct {
	InstanceID, OwnerID string
	Epoch               int64
	ExpiresAt           time.Time
}

func validTTL(ttl time.Duration) bool {
	return ttl >= MinTTL && ttl <= MaxTTL && ttl%time.Millisecond == 0
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func normalize(ctx context.Context, tx pgx.Tx, instance, owner string) (string, string, error) {
	var a, b string
	if err := tx.QueryRow(ctx, "SELECT $1::uuid::text,$2::uuid::text", instance, owner).Scan(&a, &b); err != nil {
		return "", "", ErrInvalid
	}
	return a, b, nil
}

// locked reads identity/epoch before obtaining a fresh database-clock timestamp.
// Evaluating expiry inside SELECT ... FOR UPDATE could evaluate the expression
// before waiting on the lock, allowing a lease that expired during that wait.
func locked(ctx context.Context, tx pgx.Tx, instance string) (Lease, bool, error) {
	var current Lease
	err := tx.QueryRow(ctx, "SELECT instance_id::text,owner_id::text,epoch,expires_at FROM instance_writer_leases WHERE instance_id=$1 FOR UPDATE", instance).Scan(&current.InstanceID, &current.OwnerID, &current.Epoch, &current.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, false, ErrLost
	}
	if err != nil {
		return Lease{}, false, ErrUnavailable
	}
	var alive bool
	if err = tx.QueryRow(ctx, "SELECT $1::timestamptz>clock_timestamp()", current.ExpiresAt).Scan(&alive); err != nil {
		return Lease{}, false, ErrUnavailable
	}
	return current, alive, nil
}

// Acquire creates or takes over an expired instance lease. Even reacquisition by
// the same owner increments the epoch; a live owner must use Renew instead.
// OwnerID must be a new process/session UUID, never a static deployment name.
func Acquire(ctx context.Context, pool *pgxpool.Pool, instance, owner string, ttl time.Duration) (Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if pool == nil || !validTTL(ttl) {
		return Lease{}, ErrInvalid
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Lease{}, ErrUnavailable
	}
	defer rollback(tx)
	instance, owner, err = normalize(ctx, tx, instance, owner)
	if err != nil {
		return Lease{}, err
	}
	// The first row starts expired so every successful acquisition follows the
	// same locked update path. Initial epoch becomes 2; only monotonicity matters.
	if _, err = tx.Exec(ctx, "INSERT INTO instance_writer_leases(instance_id,owner_id,epoch,expires_at) VALUES($1,$2,1,clock_timestamp()-interval '1 second') ON CONFLICT(instance_id) DO NOTHING", instance, owner); err != nil {
		return Lease{}, ErrUnavailable
	}
	current, alive, err := locked(ctx, tx, instance)
	if err != nil {
		return Lease{}, err
	}
	if alive {
		return Lease{}, ErrHeld
	}
	var result Lease
	err = tx.QueryRow(ctx, "UPDATE instance_writer_leases SET owner_id=$2,epoch=epoch+1,expires_at=clock_timestamp()+($3::bigint * interval '1 millisecond'),updated_at=clock_timestamp() WHERE instance_id=$1 AND epoch=$4 RETURNING instance_id::text,owner_id::text,epoch,expires_at", instance, owner, ttl.Milliseconds(), current.Epoch).Scan(&result.InstanceID, &result.OwnerID, &result.Epoch, &result.ExpiresAt)
	if err != nil {
		return Lease{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Lease{}, ErrUnavailable
	}
	return result, nil
}

// Require holds the lease-row lock until the caller commits or rolls back. A
// takeover cannot commit between this check and that transaction's metadata
// commit. ExpiresAt supplied by callers is never trusted. Use READ COMMITTED for
// the enclosing mutation transaction; serialization errors fail closed otherwise.
func Require(ctx context.Context, tx pgx.Tx, lease Lease) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if tx == nil || lease.Epoch < 1 {
		return ErrInvalid
	}
	instance, owner, err := normalize(ctx, tx, lease.InstanceID, lease.OwnerID)
	if err != nil {
		return err
	}
	current, alive, err := locked(ctx, tx, instance)
	if err != nil {
		return err
	}
	if !alive || current.OwnerID != owner || current.Epoch != lease.Epoch {
		return ErrLost
	}
	return nil
}
func Renew(ctx context.Context, pool *pgxpool.Pool, lease Lease, ttl time.Duration) (Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if pool == nil || !validTTL(ttl) {
		return Lease{}, ErrInvalid
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Lease{}, ErrUnavailable
	}
	defer rollback(tx)
	if err = Require(ctx, tx, lease); err != nil {
		return Lease{}, err
	}
	var result Lease
	err = tx.QueryRow(ctx, "UPDATE instance_writer_leases SET expires_at=clock_timestamp()+($4::bigint * interval '1 millisecond'),updated_at=clock_timestamp() WHERE instance_id=$1 AND owner_id=$2 AND epoch=$3 RETURNING instance_id::text,owner_id::text,epoch,expires_at", lease.InstanceID, lease.OwnerID, lease.Epoch, ttl.Milliseconds()).Scan(&result.InstanceID, &result.OwnerID, &result.Epoch, &result.ExpiresAt)
	if err != nil {
		return Lease{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return Lease{}, ErrUnavailable
	}
	return result, nil
}

// Release expires only the matching live lease, preserving its fencing epoch.
// It is intentionally not idempotent: an expired/replaced lease returns ErrLost.
func Release(ctx context.Context, pool *pgxpool.Pool, lease Lease) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if pool == nil {
		return ErrInvalid
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	if err = Require(ctx, tx, lease); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE instance_writer_leases SET expires_at=clock_timestamp(),updated_at=clock_timestamp() WHERE instance_id=$1 AND owner_id=$2 AND epoch=$3", lease.InstanceID, lease.OwnerID, lease.Epoch); err != nil {
		return ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}
