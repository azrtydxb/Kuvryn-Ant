package artifact

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ErrNotRemote means the source is not a remote source, so it has no cache.
var ErrNotRemote = errors.New("only remote sources have a cache")

// PurgeResult counts what a remote cache purge removed.
type PurgeResult struct {
	Components, Versions, Assets, Bytes int64
}

// PurgeRemoteCacheTx removes the cached components of a remote source, all of
// them or (coordinate non-empty) one coordinate, in the caller's transaction.
// Blob bytes that become unreferenced are reclaimed by the two-phase garbage
// collector, never inline, so a purge cannot race a concurrent reader of shared
// content. The caller authorizes the request and records the audit event.
func PurgeRemoteCacheTx(ctx context.Context, tx pgx.Tx, instance, source, coordinate string) (PurgeResult, error) {
	var kind string
	err := tx.QueryRow(ctx, `SELECT kind FROM sources WHERE instance_id=$1 AND id=$2 FOR SHARE`, instance, source).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return PurgeResult{}, ErrNotFound
	}
	if err != nil {
		return PurgeResult{}, err
	}
	if kind != "remote" {
		return PurgeResult{}, ErrNotRemote
	}
	var out PurgeResult
	comps := `SELECT id FROM components WHERE instance_id=$1 AND source_id=$2 AND ($3='' OR coordinate=$3)`
	if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(b.size_bytes),0)::bigint FROM assets a JOIN blobs b ON b.id=a.blob_id JOIN versions v ON v.id=a.version_id WHERE v.component_id IN (`+comps+`)`, instance, source, coordinate).Scan(&out.Bytes); err != nil {
		return PurgeResult{}, err
	}
	for _, step := range []struct {
		dst *int64
		sql string
	}{
		{new(int64), `DELETE FROM tags WHERE component_id IN (` + comps + `)`},
		{&out.Assets, `DELETE FROM assets WHERE version_id IN (SELECT id FROM versions WHERE component_id IN (` + comps + `))`},
		{&out.Versions, `DELETE FROM versions WHERE component_id IN (` + comps + `)`},
		{&out.Components, `DELETE FROM components WHERE id IN (` + comps + `)`},
	} {
		tag, e := tx.Exec(ctx, step.sql, instance, source, coordinate)
		if e != nil {
			return PurgeResult{}, e
		}
		*step.dst = tag.RowsAffected()
	}
	return out, nil
}
