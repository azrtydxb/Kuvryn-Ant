package replication

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyManifestPage applies one manifest page (assets and tags) inside tx.
func (s *Service) applyManifestPage(ctx context.Context, tx pgx.Tx, rt *run, page ManifestPage, items []Item, applied *delta) error {
	for _, it := range items {
		if err := s.applyAsset(ctx, tx, rt, it, applied); err != nil {
			return err
		}
	}
	for _, t := range page.Tags {
		if !s.wanted(rt, t.Origin, t.Namespace, t.Coordinate, t.Version) {
			continue
		}
		if err := s.applyTag(ctx, tx, rt, t); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) wantedItems(rt *run, page ManifestPage) []Item {
	var items []Item
	for _, it := range page.Items {
		if s.wanted(rt, it.Origin, it.Namespace, it.Coordinate, it.Version) {
			items = append(items, it)
		}
	}
	return items
}

func (s *Service) seedPage(ctx context.Context, rt *run, kind string) error {
	page, err := rt.client.Manifest(ctx, rt.job.RemoteFeedID, kind, rt.job.seedCursor, s.cfg.PageSize, rt.job.IncludeCached, rt.job.Filters, "")
	if err != nil {
		return err
	}
	items := s.wantedItems(rt, page)
	d := delta{}
	if err = s.ensureBlobs(ctx, rt, items, &d); err != nil {
		return err
	}
	next := page.Next
	return s.commit(ctx, rt, func(tx pgx.Tx, applied *delta) error {
		*applied = d
		if err := s.applyManifestPage(ctx, tx, rt, page, items, applied); err != nil {
			return err
		}
		count := len(page.Items) + len(page.Tags)
		if _, err := tx.Exec(ctx, `UPDATE replication_jobs SET seed_cursor=$2,seed_done=seed_done+$3,seed_total=COALESCE($4,seed_total) WHERE id=$1`, rt.job.ID, next, count, page.Total); err != nil {
			return err
		}
		if next != "" {
			return nil
		}
		if kind == manifestAssets {
			_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='seed_tags',seed_cursor='' WHERE id=$1`, rt.job.ID)
			return err
		}
		if rt.job.reseeding {
			if err := s.holdVanished(ctx, tx, rt); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET phase='incremental',seed_cursor='',reseeding=false,checkpoint=COALESCE(seed_head,0),checkpoint_change_at=clock_timestamp() WHERE id=$1`, rt.job.ID)
		return err
	})
}

// holdVanished runs when a reseed completes. A replicated asset of this job
// that the finished manifest walk did not re-confirm (its stamped seed
// generation is older than the current one) was deleted at the peer during the
// purged window. Its tombstone is gone, so it is recorded as a held deletion
// exactly as if the tombstone had arrived under the "ignore" policy. Nothing is
// deleted here, only rows carrying this job's provenance are considered, and
// items outside the job's current filters are left alone.
func (s *Service) holdVanished(ctx context.Context, tx pgx.Tx, rt *run) error {
	rows, err := tx.Query(ctx, `SELECT c.namespace,c.coordinate,v.version,a.asset_key,COALESCE(a.provenance->>'origin_kind','local')
 FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id
 WHERE c.source_id=$1 AND a.provenance->>'replication_job'=$2 AND COALESCE((a.provenance->>'seed_generation')::bigint,0)<$3`, rt.job.ReplicaSourceID, rt.job.ID, rt.job.seedGeneration)
	if err != nil {
		return err
	}
	type gone struct{ coordinate, version, key string }
	var list []gone
	for rows.Next() {
		var ns *string
		var g gone
		var originKind string
		if err = rows.Scan(&ns, &g.coordinate, &g.version, &g.key, &originKind); err != nil {
			rows.Close()
			return err
		}
		if s.wanted(rt, Origin{Kind: originKind}, ns, g.coordinate, g.version) {
			list = append(list, g)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	seq := int64(0)
	if rt.job.seedHead != nil {
		seq = *rt.job.seedHead
	}
	for _, g := range list {
		if _, err = tx.Exec(ctx, `INSERT INTO replication_held_deletions(job_id,kind,coordinate,version,item_key,seq) VALUES($1,'asset',$2,$3,$4,$5) ON CONFLICT(job_id,kind,coordinate,version,item_key) DO NOTHING`, rt.job.ID, g.coordinate, g.version, g.key, seq); err != nil {
			return err
		}
	}
	return nil
}

// originWanted applies the include-cached option to a whole source.
func (s *Service) originWanted(rt *run, o Origin) bool {
	if o.Kind == "remote" {
		return rt.job.IncludeCached
	}
	return o.Kind == "local"
}

// wanted applies the job's include-cached option and selective filters.
func (s *Service) wanted(rt *run, o Origin, ns *string, coordinate, version string) bool {
	if !s.originWanted(rt, o) {
		return false
	}
	return rt.job.Filters.Match(ns, coordinate, version)
}

// backfillPage drains one page of the oldest queued back-fill: the current
// content of a source attached to the remote feed after this job's seed point.
// It reads the manifest restricted to that source, so it costs only that
// source's content, and commits cursor and content together.
func (s *Service) backfillPage(ctx context.Context, rt *run) error {
	var source, phase, cursor string
	err := s.cfg.DB.QueryRow(ctx, `SELECT source_id::text,phase,cursor FROM replication_backfills WHERE job_id=$1 ORDER BY seq,source_id LIMIT 1`, rt.job.ID).Scan(&source, &phase, &cursor)
	if err != nil {
		return dbError(err)
	}
	kind := manifestAssets
	if phase == "tags" {
		kind = manifestTags
	}
	page, err := rt.client.Manifest(ctx, rt.job.RemoteFeedID, kind, cursor, s.cfg.PageSize, rt.job.IncludeCached, rt.job.Filters, source)
	if err != nil {
		return err
	}
	items := s.wantedItems(rt, page)
	d := delta{}
	if err = s.ensureBlobs(ctx, rt, items, &d); err != nil {
		return err
	}
	next := page.Next
	return s.commit(ctx, rt, func(tx pgx.Tx, applied *delta) error {
		*applied = d
		if err := s.applyManifestPage(ctx, tx, rt, page, items, applied); err != nil {
			return err
		}
		count := len(page.Items) + len(page.Tags)
		if next != "" {
			_, err := tx.Exec(ctx, `UPDATE replication_backfills SET cursor=$3,done=done+$4 WHERE job_id=$1 AND source_id=$2`, rt.job.ID, source, next, count)
			return err
		}
		if phase == "assets" {
			_, err := tx.Exec(ctx, `UPDATE replication_backfills SET phase='tags',cursor='',done=done+$3 WHERE job_id=$1 AND source_id=$2`, rt.job.ID, source, count)
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM replication_backfills WHERE job_id=$1 AND source_id=$2`, rt.job.ID, source); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET backfills_completed=backfills_completed+1 WHERE id=$1`, rt.job.ID)
		return err
	})
}

func (s *Service) incrementalPage(ctx context.Context, rt *run) (bool, error) {
	page, err := rt.client.Changes(ctx, rt.job.RemoteFeedID, rt.job.Status.Checkpoint, s.cfg.PageSize, rt.job.IncludeCached, rt.job.Filters)
	if err != nil {
		return false, err
	}
	if page.Next < rt.job.Status.Checkpoint {
		return false, fmt.Errorf("%w: peer cursor moved backwards", ErrPeer)
	}
	var items []Item
	for _, c := range page.Changes {
		if c.Op == "put" && c.Kind == "asset" && c.Asset != nil && s.wanted(rt, c.Asset.Origin, c.Asset.Namespace, c.Asset.Coordinate, c.Asset.Version) {
			items = append(items, *c.Asset)
		}
	}
	d := delta{}
	if err = s.ensureBlobs(ctx, rt, items, &d); err != nil {
		return false, err
	}
	err = s.commit(ctx, rt, func(tx pgx.Tx, applied *delta) error {
		*applied = d
		for _, c := range page.Changes {
			if err := s.applyChange(ctx, tx, rt, c, applied); err != nil {
				return err
			}
		}
		var at any
		if n := len(page.Changes); n > 0 {
			at = page.Changes[n-1].At
		}
		_, err := tx.Exec(ctx, `UPDATE replication_jobs SET checkpoint=$2,checkpoint_change_at=COALESCE($3,checkpoint_change_at,clock_timestamp()),remote_head=$4,remote_head_at=COALESCE($5,remote_head_at),source_low_water=$6 WHERE id=$1`, rt.job.ID, page.Next, at, page.Head, page.HeadAt, page.LowWater)
		return err
	})
	if err != nil {
		return false, err
	}
	rt.job.Status.Checkpoint = page.Next
	return !page.More, nil
}
