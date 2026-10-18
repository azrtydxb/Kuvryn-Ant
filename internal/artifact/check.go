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

// ListVersions returns up to limit versions of the package owner for a
// coordinate whose version starts with prefix (empty matches all), ordered by
// version. It does not authorize; callers pass the result to Delete or Unlist,
// which authorize and apply policy per version.
func (s *Service) ListVersions(ctx context.Context, instance, feed, coordinate, prefix string, limit int) ([]string, error) {
	if s == nil || s.DB == nil || limit < 1 || limit > 100000 {
		return nil, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.DB.Query(ctx, `WITH owner AS (SELECT c.id FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT v.version FROM owner o JOIN versions v ON v.component_id=o.id WHERE starts_with(v.version,$4) ORDER BY v.version LIMIT $5`, instance, feed, coordinate, prefix, limit+1)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if rows.Scan(&v) != nil {
			return nil, ErrUnavailable
		}
		out = append(out, v)
	}
	if rows.Err() != nil || len(out) > limit {
		return nil, ErrUnavailable
	}
	return out, nil
}
