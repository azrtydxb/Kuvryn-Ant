package artifact

// Package discovery records on the artifact core: the tag a client resolved
// through a remote source, and the grouping of index children under their index.
// Both are metadata only; neither changes what a read resolves to.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

// observedRefresh bounds how often an unchanged observed tag is rewritten, so a
// busy tag does not turn every pull into a database write.
const observedRefresh = 10 * time.Minute

// recordChildren stores the grouping of an index version. It is idempotent and
// runs inside the caller's transaction. Children are validated and bounded; an
// invalid entry is dropped rather than failing the publication.
func recordChildren(ctx context.Context, tx pgx.Tx, component, version string, children []Child) error {
	if len(children) == 0 {
		return nil
	}
	if len(children) > 4096 {
		children = children[:4096]
	}
	for _, c := range children {
		if !bounded(c.Version, 256) || len(c.Platform) > 128 || !utf8.ValidString(c.Platform) || c.Version == version {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO version_children(component_id,parent_version_id,child_version,platform) SELECT $1,v.id,$3,$4 FROM versions v WHERE v.component_id=$1 AND v.version=$2 ON CONFLICT(parent_version_id,child_version) DO UPDATE SET platform=excluded.platform WHERE version_children.platform<>excluded.platform`, component, version, c.Version, c.Platform); err != nil {
			return dbErr(err)
		}
	}
	return nil
}

// ObserveTag records that a remote source answered tag with the cached version
// r.Version (a digest) of r.Coordinate. The tag is marked observed: it never
// replaces a tag without that mark, is never consulted by ResolveTag, and is
// refreshed or moved whenever the tag is resolved again. Children, when given,
// are grouped under the version in the same transaction (this also backfills
// index links for manifests cached before the grouping existed). The version
// must already be cached; otherwise ErrNotFound and nothing is recorded.
func (s *Service) ObserveTag(ctx context.Context, p identity.Principal, r Publication, source, tag string, children []Child) error {
	if s == nil || s.DB == nil || s.Lease == nil || p.PrincipalID == "" || !bounded(tag, 128) || !bounded(r.FeedID, 36) || !bounded(r.Coordinate, 256) || !bounded(r.Version, 256) || !bounded(source, 36) || len(r.Namespace) > 256 {
		return ErrInput
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = s.observeTagOnce(ctx, p, r, source, tag, children)
		if err == nil || attempt >= 3 || !transientDB(err) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
}

func (s *Service) observeTagOnce(ctx context.Context, p identity.Principal, r Publication, source, tag string, children []Child) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return dbErr(err)
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return ErrDenied
	}
	if err = ownership.Require(ctx, tx, lease); err != nil {
		return err
	}
	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id JOIN feeds f ON f.id=fs.feed_id AND f.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.id=$3 AND s.kind='remote' AND s.format=f.format FOR SHARE OF fs,s`, p.InstanceID, r.FeedID, source).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return dbErr(err)
	}
	digest := sha256.Sum256([]byte(p.InstanceID + "\x00" + source + "\x00" + r.Coordinate))
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		return dbErr(err)
	}
	var component, version string
	err = tx.QueryRow(ctx, `SELECT c.id::text,v.id::text FROM components c JOIN versions v ON v.component_id=c.id WHERE c.source_id=$1 AND c.instance_id=$2 AND c.coordinate=$3 AND COALESCE(c.namespace,'')=$4 AND v.version=$5`, source, p.InstanceID, r.Coordinate, r.Namespace, r.Version).Scan(&component, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return dbErr(err)
	}
	if err = recordChildren(ctx, tx, component, r.Version, children); err != nil {
		return err
	}
	var current string
	var observed *time.Time
	err = tx.QueryRow(ctx, `SELECT version_id::text,observed_at FROM tags WHERE component_id=$1 AND name=$2 FOR UPDATE`, component, tag).Scan(&current, &observed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `INSERT INTO tags(component_id,name,version_id,observed_at) VALUES($1,$2,$3,now())`, component, tag, version)
	case err != nil:
		return dbErr(err)
	case observed == nil:
		// An authoritative tag is never overwritten by what a remote answered.
		return tx.Commit(ctx)
	case current == version:
		if time.Since(*observed) < observedRefresh {
			return tx.Commit(ctx)
		}
		if _, err = tx.Exec(ctx, `UPDATE tags SET observed_at=now() WHERE component_id=$1 AND name=$2`, component, tag); err != nil {
			return dbErr(err)
		}
		return tx.Commit(ctx)
	default:
		_, err = tx.Exec(ctx, `UPDATE tags SET version_id=$3,observed_at=now(),revision=revision+1 WHERE component_id=$1 AND name=$2`, component, tag, version)
	}
	if err != nil {
		return dbErr(err)
	}
	var audit int64
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'artifact.observe_tag',$3) RETURNING id`, p.InstanceID, p.PrincipalID, version).Scan(&audit); err != nil {
		return dbErr(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'artifact.observe_tag',jsonb_build_object('component_id',$2::text,'version_id',$3::text,'tag',$4::text,'source_id',$5::text))`, audit, component, version, tag, source); err != nil {
		return dbErr(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return dbErr(err)
	}
	return nil
}

// RecordChildren groups children under an existing index version in its own
// transaction. It is the maintenance entry point used to backfill indexes that
// were stored before grouping was recorded at publish time. Idempotent.
func RecordChildren(ctx context.Context, db interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}, component, version string, children []Child) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return dbErr(err)
	}
	defer rollback(tx)
	if err = recordChildren(ctx, tx, component, version, children); err != nil {
		return err
	}
	return dbErr0(tx.Commit(ctx))
}

func dbErr0(err error) error {
	if err == nil {
		return nil
	}
	return dbErr(err)
}
