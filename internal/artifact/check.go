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

// UnlistedVersions returns the unlisted versions of the package owner for a
// coordinate (highest-priority source containing it), for adapters that
// surface yank/unlist state in their indexes. It does not authorize; callers
// have already authorized the read of the package they are describing.
func (s *Service) UnlistedVersions(ctx context.Context, instance, feed, coordinate string) (map[string]bool, error) {
	if s == nil || s.DB == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var list []string
	err := s.DB.QueryRow(ctx, `WITH owner AS (SELECT c.metadata FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT COALESCE(ARRAY(SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(metadata->'unlisted')='array' THEN metadata->'unlisted' ELSE '[]'::jsonb END)),ARRAY[]::text[]) FROM owner`, instance, feed, coordinate).Scan(&list)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	out := make(map[string]bool, len(list))
	for _, v := range list {
		out[v] = true
	}
	return out, nil
}
