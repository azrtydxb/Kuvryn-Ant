package cacherelease

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxDepth = 16

// Result is what a release removed. Bytes are the sizes of blobs left without
// any reference, queued for the short sweep (not yet deleted).
type Result struct {
	Versions, Components int64
	Bytes                int64
}

// Unreferenced deletes the candidate versions of one component that have no
// tag and are not named by another version's manifest (version_refs), then
// does the same, transitively, for the versions the deleted manifests named (an
// index's children, an image's config and layers). Candidates that are still
// tagged or referenced are left alone; an emptied component is removed. Freed
// blobs are queued for the short sweep with ExpediteBlobs; no byte is deleted
// here. The caller holds the coordinate's advisory lock, which serializes this
// with publication of the same repository, and removes the tags it wants gone
// before calling.
func Unreferenced(ctx context.Context, tx pgx.Tx, instance, component string, candidates []string, sweepGrace, gcGrace time.Duration) (Result, error) {
	var res Result
	var blobIDs []string
	for depth := 0; depth < maxDepth && len(candidates) > 0; depth++ {
		eligible, err := ids(ctx, tx, `SELECT v.id::text FROM versions v WHERE v.component_id=$1 AND v.id=ANY($2::uuid[])
 AND NOT EXISTS(SELECT 1 FROM tags t WHERE t.version_id=v.id)
 AND NOT EXISTS(SELECT 1 FROM version_refs r WHERE r.component_id=v.component_id AND r.ref_version=v.version)`, component, candidates)
		if err != nil {
			return res, err
		}
		if len(eligible) == 0 {
			break
		}
		// Read what the doomed manifests name before the rows disappear.
		next, err := ids(ctx, tx, `SELECT DISTINCT v2.id::text FROM version_refs r JOIN versions v2 ON v2.component_id=r.component_id AND v2.version=r.ref_version WHERE r.parent_version_id=ANY($1::uuid[])`, eligible)
		if err != nil {
			return res, err
		}
		freed, err := ids(ctx, tx, `DELETE FROM assets WHERE instance_id=$1 AND version_id=ANY($2::uuid[]) RETURNING blob_id::text`, instance, eligible)
		if err != nil {
			return res, err
		}
		blobIDs = append(blobIDs, freed...)
		if _, err = tx.Exec(ctx, `DELETE FROM tags WHERE component_id=$1 AND version_id=ANY($2::uuid[])`, component, eligible); err != nil {
			return res, err
		}
		ct, err := tx.Exec(ctx, `DELETE FROM versions WHERE instance_id=$1 AND component_id=$2 AND id=ANY($3::uuid[])`, instance, component, eligible)
		if err != nil {
			return res, err
		}
		res.Versions += ct.RowsAffected()
		candidates = next
	}
	if res.Versions == 0 {
		return res, nil
	}
	ct, err := tx.Exec(ctx, `DELETE FROM components c WHERE c.id=$1 AND NOT EXISTS(SELECT 1 FROM versions WHERE component_id=c.id)`, component)
	if err != nil {
		return res, err
	}
	res.Components = ct.RowsAffected()
	res.Bytes, err = ExpediteBlobs(ctx, tx, blobIDs, sweepGrace, gcGrace)
	return res, err
}

func ids(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
