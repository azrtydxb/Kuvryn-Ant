package oci

import (
	"context"
	"io"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BackfillReport is the outcome of BackfillIndexChildren.
type BackfillReport struct {
	DryRun   bool  `json:"dry_run"`
	Indexes  int64 `json:"indexes_found"`
	Children int64 `json:"children_linked"`
	Skipped  int64 `json:"unreadable"`
}

// BackfillIndexChildren reads image indexes stored before grouping was recorded
// at publish time and links their per-platform manifests. It is idempotent and
// only reads manifest bytes already in blob storage (it never contacts an
// upstream). Pull-through indexes are also linked lazily the next time their tag
// is resolved; this covers hosted indexes and untouched cached ones.
func BackfillIndexChildren(ctx context.Context, db *pgxpool.Pool, resolve func(ctx context.Context, domain string) (blob.Store, error), dryRun bool) (BackfillReport, error) {
	report := BackfillReport{DryRun: dryRun}
	rows, err := db.Query(ctx, `SELECT v.component_id::text,v.version,a.media_type,b.digest,b.dedup_domain_id::text
 FROM assets a JOIN versions v ON v.id=a.version_id JOIN blobs b ON b.id=a.blob_id
 WHERE a.asset_key='manifest' AND a.media_type IN ($1,$2) AND b.size_bytes<=$3
 AND NOT EXISTS (SELECT 1 FROM version_children vc WHERE vc.parent_version_id=v.id)`, imageIndex, dockerIndex, int64(manifestLimit))
	if err != nil {
		return report, err
	}
	type item struct{ component, version, media, digest, domain string }
	var items []item
	for rows.Next() {
		var it item
		if err = rows.Scan(&it.component, &it.version, &it.media, &it.digest, &it.domain); err != nil {
			rows.Close()
			return report, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return report, err
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		store, err := resolve(ctx, it.domain)
		if err != nil {
			report.Skipped++
			continue
		}
		rc, err := store.OpenRange(ctx, blob.Digest(it.digest), 0, -1)
		if err != nil {
			report.Skipped++
			continue
		}
		body, err := io.ReadAll(io.LimitReader(rc, manifestLimit))
		_ = rc.Close()
		if err != nil {
			report.Skipped++
			continue
		}
		children := indexChildren(body, it.media)
		if len(children) == 0 {
			continue
		}
		report.Indexes++
		if dryRun {
			report.Children += int64(len(children))
			continue
		}
		if err = artifact.RecordChildren(ctx, db, it.component, it.version, children); err != nil {
			return report, err
		}
		report.Children += int64(len(children))
	}
	return report, nil
}
