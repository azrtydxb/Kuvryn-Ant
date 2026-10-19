package artifact

// Mutable tag management (npm dist-tag add/rm). Tags are pointers; moving or
// removing one never touches versions or bytes, so the authorization is the
// publish action (as for the publish that created the tag), not delete.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

const (
	ActionTag   Action = "tag"
	ActionUntag Action = "untag"
)

// SetTag points tag at an existing version of the package (t.Version), creating
// or moving it. RemoveTag drops the pointer only.
func (s *Service) SetTag(ctx context.Context, p identity.Principal, t Target, tag string) (Outcome, error) {
	return s.tagMutate(ctx, p, ActionTag, t, tag)
}

// RemoveTag deletes tag from the package; t.Version is ignored.
func (s *Service) RemoveTag(ctx context.Context, p identity.Principal, t Target, tag string) (Outcome, error) {
	return s.tagMutate(ctx, p, ActionUntag, t, tag)
}

func (s *Service) tagMutate(ctx context.Context, p identity.Principal, action Action, t Target, tag string) (Outcome, error) {
	if s == nil || s.DB == nil || s.Lease == nil || p.PrincipalID == "" || !bounded(tag, 128) || !bounded(t.FeedID, 36) || !bounded(t.Coordinate, 256) || len(t.Namespace) > 256 || (action == ActionTag && !bounded(t.Version, 256)) {
		return Outcome{}, ErrInput
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if errors.Is(err, identity.ErrUnauthenticated) {
		return Outcome{}, ErrDenied
	}
	if err != nil {
		return Outcome{}, ErrUnavailable
	}
	var out Outcome
	for attempt := 0; ; attempt++ {
		out, err = s.tagOnce(ctx, p, policy, action, t, tag)
		if err == nil || attempt >= 4 || !transientDB(err) || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	if err == nil && !out.Unchanged && s.Changed != nil {
		s.Changed(Change{Action: action, FeedID: t.FeedID, SourceID: out.SourceID, Namespace: t.Namespace, Coordinate: t.Coordinate, Version: t.Version})
	}
	return out, err
}

func (s *Service) tagOnce(ctx context.Context, p identity.Principal, policy identity.Policy, action Action, t Target, tag string) (Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Outcome{}, dbErr(err)
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return Outcome{}, ErrDenied
	}
	if err = ownership.Require(ctx, tx, lease); err != nil {
		return Outcome{}, err
	}
	if err = frozenFeed(ctx, tx, p.InstanceID, t.FeedID); err != nil {
		return Outcome{}, err
	}
	out := Outcome{Action: action}
	var kind, nsActual string
	find := func(lock bool) error {
		q := `SELECT c.id::text,c.source_id::text,c.namespace,s.kind FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1`
		if lock {
			q += ` FOR UPDATE OF c`
		}
		return tx.QueryRow(ctx, q, p.InstanceID, t.FeedID, t.Coordinate).Scan(&out.ComponentID, &out.SourceID, &nsActual, &kind)
	}
	if err = find(false); errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, ErrNotFound
	} else if err != nil {
		return Outcome{}, dbErr(err)
	}
	digest := sha256.Sum256([]byte(p.InstanceID + "\x00" + out.SourceID + "\x00" + t.Coordinate))
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		return Outcome{}, dbErr(err)
	}
	if err = find(true); errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, ErrNotFound
	} else if err != nil {
		return Outcome{}, dbErr(err)
	}
	if nsActual != t.Namespace {
		return Outcome{}, ErrNotFound
	}
	res := identity.Resource{InstanceID: p.InstanceID, FeedID: t.FeedID, SourceID: out.SourceID, Namespace: t.Namespace, Kind: identity.ArtifactResource}
	if !policy.Authorize(p, res, identity.Read) {
		return Outcome{}, ErrNotFound
	}
	if !policy.Authorize(p, res, identity.Publish) {
		return Outcome{}, ErrDenied
	}
	if kind != "local" {
		return Outcome{}, ErrConflict
	}
	if action == ActionTag {
		if err = tx.QueryRow(ctx, `SELECT id::text FROM versions WHERE instance_id=$1 AND component_id=$2 AND version=$3`, p.InstanceID, out.ComponentID, t.Version).Scan(&out.VersionID); errors.Is(err, pgx.ErrNoRows) {
			return Outcome{}, ErrNotFound
		} else if err != nil {
			return Outcome{}, dbErr(err)
		}
		ct, e := tx.Exec(ctx, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3) ON CONFLICT(component_id,name) DO UPDATE SET version_id=EXCLUDED.version_id WHERE tags.version_id<>EXCLUDED.version_id`, out.ComponentID, tag, out.VersionID)
		if e != nil {
			return Outcome{}, dbErr(e)
		}
		if ct.RowsAffected() == 0 {
			out.Unchanged = true
			return out, nil
		}
	} else {
		if err = tx.QueryRow(ctx, `DELETE FROM tags WHERE component_id=$1 AND name=$2 RETURNING version_id::text`, out.ComponentID, tag).Scan(&out.VersionID); errors.Is(err, pgx.ErrNoRows) {
			return Outcome{}, ErrNotFound
		} else if err != nil {
			return Outcome{}, dbErr(err)
		}
		out.TagsRemoved = 1
	}
	tt := t
	tt.Tag = tag
	if out, err = finishMutation(ctx, tx, p, tt, DeletePolicy{}, action, out.VersionID, out); err != nil {
		return Outcome{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Outcome{}, dbErr(err)
	}
	return out, nil
}
