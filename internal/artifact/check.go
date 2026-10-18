package artifact

import (
	"context"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Check reports whether Delete/Unlist/Relist would be accepted for the target
// right now, without changing anything (the work is rolled back). Adapters that
// regenerate a signed index before removing a version call it first so a policy
// refusal never leaves the index and the stored versions out of step. The real
// call still re-checks authorization and policy.
func (s *Service) Check(ctx context.Context, p identity.Principal, action Action, t Target) error {
	if s == nil || s.DB == nil || p.PrincipalID == "" || !validTarget(t) {
		return ErrInput
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if errors.Is(err, identity.ErrUnauthenticated) {
		return ErrDenied
	}
	if err != nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return dbErr(err)
	}
	defer rollback(tx)
	_, err = MutateTx(ctx, tx, p, policy, action, t)
	return err
}

// DeletePolicy returns the effective delete policy for a feed namespace. Callers
// that tombstone metadata themselves (for example mutable refs) use it to honor
// a "denied" policy; version removal must go through Delete or Unlist.
func (s *Service) DeletePolicy(ctx context.Context, instance, feed, namespace string) (DeletePolicy, error) {
	if s == nil || s.DB == nil {
		return DeletePolicy{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return EffectiveDeletePolicy(ctx, s.DB, instance, feed, namespace)
}
