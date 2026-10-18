package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func boundedText(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}

// validItem applies the same bounds as local publication. Peer data is never
// trusted to be well formed merely because the peer authenticated.
func validItem(it Item) error {
	bad := func(what string) error { return fmt.Errorf("%w: invalid %s in peer manifest", ErrPeer, what) }
	if !boundedText(it.Coordinate, 256) || !boundedText(it.Version, 256) || !boundedText(it.AssetKey, 2048) || !boundedText(it.MediaType, 256) {
		return bad("identity")
	}
	if it.Namespace != nil && (len(*it.Namespace) > 256 || !utf8.ValidString(*it.Namespace)) {
		return bad("namespace")
	}
	if !digestPattern.MatchString(it.Digest) || it.Size < 0 {
		return bad("digest")
	}
	if n, err := routing.NormalizePath("/" + it.AssetKey); err != nil || n != "/"+it.AssetKey {
		return bad("asset key")
	}
	if len(it.ComponentMetadata) > 1<<20 {
		return bad("metadata")
	}
	if s := it.Subject; s != nil && (!digestPattern.MatchString(s.Digest) || s.Size < 0 || len(s.MediaType) > 256 || len(s.ArtifactType) > 256 || len(s.Annotations) > 1024) {
		return bad("subject")
	}
	return nil
}
func validTag(t TagItem) error {
	if !boundedText(t.Coordinate, 256) || !boundedText(t.Version, 256) || !boundedText(t.Tag, 128) || strings.Contains(t.Tag, "/") || (t.Namespace != nil && (len(*t.Namespace) > 256 || !utf8.ValidString(*t.Namespace))) {
		return fmt.Errorf("%w: invalid tag in peer response", ErrPeer)
	}
	return nil
}

func metadataOrEmpty(m json.RawMessage) []byte {
	if len(m) == 0 || string(m) == "null" {
		return []byte("{}")
	}
	return m
}

// upsertComponent writes package-level state under the job's replica source
// only. withMetadata=false (tags) never overwrites existing metadata.
func (s *Service) upsertComponent(ctx context.Context, tx pgx.Tx, rt *run, ns *string, coordinate string, meta json.RawMessage, withMetadata bool) (string, error) {
	var nsArg any
	if ns != nil {
		nsArg = *ns
	}
	var id string
	if withMetadata {
		err := tx.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace,metadata) VALUES($1,$2,$3,$4,$5)
 ON CONFLICT(source_id,coordinate) DO UPDATE SET namespace=EXCLUDED.namespace,metadata=EXCLUDED.metadata RETURNING id::text`, s.cfg.InstanceID, rt.job.ReplicaSourceID, coordinate, nsArg, metadataOrEmpty(meta)).Scan(&id)
		return id, err
	}
	err := tx.QueryRow(ctx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4)
 ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=EXCLUDED.coordinate RETURNING id::text`, s.cfg.InstanceID, rt.job.ReplicaSourceID, coordinate, nsArg).Scan(&id)
	return id, err
}
func (s *Service) upsertVersion(ctx context.Context, tx pgx.Tx, component, version string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) ON CONFLICT(component_id,version) DO UPDATE SET version=EXCLUDED.version RETURNING id::text`, s.cfg.InstanceID, component, version).Scan(&id)
	return id, err
}
func (s *Service) clearHeld(ctx context.Context, tx pgx.Tx, rt *run, kind, coordinate, version, key string) error {
	_, err := tx.Exec(ctx, `DELETE FROM replication_held_deletions WHERE job_id=$1 AND kind=$2 AND coordinate=$3 AND version=$4 AND item_key=$5`, rt.job.ID, kind, coordinate, version, key)
	return err
}

// applyAsset idempotently upserts a replicated asset. The authority wins over
// its own earlier replicas, but an asset in the replica source that this job did
// not create, or one from a higher-precedence origin source, is never overwritten.
func (s *Service) applyAsset(ctx context.Context, tx pgx.Tx, rt *run, it Item, d *delta) error {
	if err := validItem(it); err != nil {
		return err
	}
	comp, err := s.upsertComponent(ctx, tx, rt, it.Namespace, it.Coordinate, it.ComponentMetadata, true)
	if err != nil {
		return err
	}
	ver, err := s.upsertVersion(ctx, tx, comp, it.Version)
	if err != nil {
		return err
	}
	var blobID string
	var blobSize int64
	if err = tx.QueryRow(ctx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3)
 ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=EXCLUDED.digest RETURNING id::text,size_bytes`, s.cfg.InstanceID, rt.domain, it.Digest, it.Size).Scan(&blobID, &blobSize); err != nil {
		return err
	}
	if blobSize != it.Size {
		return fmt.Errorf("%w: size conflict for %s", ErrCorrupt, it.Digest)
	}
	provenance, _ := json.Marshal(map[string]any{
		"replication_job": rt.job.ID, "peer_id": rt.job.PeerID, "origin_source_id": it.Origin.SourceID,
		"origin_kind": it.Origin.Kind, "origin_priority": it.Origin.Priority, "cached": it.Origin.Kind == "remote",
	})
	var assetID, owner, oldDigest string
	var oldPriority int
	var oldPriorityText *string
	var jobOwner *string
	err = tx.QueryRow(ctx, `SELECT a.id::text,a.provenance->>'replication_job',a.provenance->>'origin_priority',b.digest FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.version_id=$1 AND a.asset_key=$2`, ver, it.AssetKey).Scan(&assetID, &jobOwner, &oldPriorityText, &oldDigest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err = tx.QueryRow(ctx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,media_type,provenance) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, s.cfg.InstanceID, ver, it.AssetKey, blobID, it.MediaType, provenance).Scan(&assetID); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if jobOwner != nil {
			owner = *jobOwner
		}
		if owner != rt.job.ID {
			d.conflicts++
			return nil
		}
		if oldPriorityText != nil {
			oldPriority, _ = strconv.Atoi(*oldPriorityText)
		}
		if oldDigest != it.Digest && oldPriority < it.Origin.Priority {
			d.conflicts++
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE assets SET blob_id=$2,media_type=$3,provenance=$4 WHERE id=$1 AND (blob_id<>$2 OR media_type<>$3 OR provenance<>$4)`, assetID, blobID, it.MediaType, provenance); err != nil {
			return err
		}
	}
	if it.Subject != nil {
		ann, _ := json.Marshal(it.Subject.Annotations)
		if it.Subject.Annotations == nil {
			ann = []byte("{}")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO artifact_subjects(asset_id,instance_id,component_id,subject_digest,subject_size,subject_media_type,artifact_type,annotations) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
 ON CONFLICT(asset_id) DO UPDATE SET subject_digest=EXCLUDED.subject_digest,subject_size=EXCLUDED.subject_size,subject_media_type=EXCLUDED.subject_media_type,artifact_type=EXCLUDED.artifact_type,annotations=EXCLUDED.annotations`,
			assetID, s.cfg.InstanceID, comp, it.Subject.Digest, it.Subject.Size, it.Subject.MediaType, it.Subject.ArtifactType, ann); err != nil {
			return err
		}
	} else if _, err = tx.Exec(ctx, `DELETE FROM artifact_subjects WHERE asset_id=$1`, assetID); err != nil {
		return err
	}
	if err = s.clearHeld(ctx, tx, rt, "asset", it.Coordinate, it.Version, it.AssetKey); err != nil {
		return err
	}
	d.assets++
	return nil
}

func (s *Service) applyTag(ctx context.Context, tx pgx.Tx, rt *run, t TagItem) error {
	if err := validTag(t); err != nil {
		return err
	}
	comp, err := s.upsertComponent(ctx, tx, rt, t.Namespace, t.Coordinate, nil, false)
	if err != nil {
		return err
	}
	ver, err := s.upsertVersion(ctx, tx, comp, t.Version)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3) ON CONFLICT(component_id,name) DO UPDATE SET version_id=EXCLUDED.version_id,revision=tags.revision+1 WHERE tags.version_id<>EXCLUDED.version_id`, comp, t.Tag, ver); err != nil {
		return err
	}
	return s.clearHeld(ctx, tx, rt, "tag", t.Coordinate, "", t.Tag)
}

// applyChange applies one ordered change. Puts whose item has since vanished at
// the source are no-ops; a later tombstone follows. Replays are harmless.
func (s *Service) applyChange(ctx context.Context, tx pgx.Tx, rt *run, c Change, d *delta) error {
	switch {
	case c.Op == "put" && c.Kind == "asset":
		if c.Asset != nil && s.wanted(rt, c.Asset.Origin, c.Asset.Namespace, c.Asset.Coordinate, c.Asset.Version) {
			return s.applyAsset(ctx, tx, rt, *c.Asset, d)
		}
	case c.Op == "put" && c.Kind == "tag":
		if c.Tag != nil && s.wanted(rt, c.Tag.Origin, c.Tag.Namespace, c.Tag.Coordinate, c.Tag.Version) {
			return s.applyTag(ctx, tx, rt, *c.Tag)
		}
	case c.Op == "put" && c.Kind == "component":
		if c.Component != nil && s.wanted(rt, c.Component.Origin, c.Component.Namespace, c.Component.Coordinate, "") {
			// Package-level state is only updated for packages already present;
			// it never creates an empty package from a bare metadata change.
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM components WHERE source_id=$1 AND coordinate=$2)`, rt.job.ReplicaSourceID, c.Component.Coordinate).Scan(&exists); err != nil {
				return err
			}
			if exists {
				_, err := s.upsertComponent(ctx, tx, rt, c.Component.Namespace, c.Component.Coordinate, c.Component.Metadata, true)
				return err
			}
		}
	case c.Op == "delete":
		key := heldKey(c.Kind, c.Version)
		if rt.job.DeletionPolicy == "propagate" {
			res, err := s.performDelete(ctx, tx, rt, c.Kind, c.Coordinate, c.Version, c.Key)
			if err != nil {
				return err
			}
			if res == dispositionWouldDelete {
				d.deletions++
			}
			if res == dispositionProtected {
				d.conflicts++
			}
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO replication_held_deletions(job_id,kind,coordinate,version,item_key,seq) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(job_id,kind,coordinate,version,item_key) DO UPDATE SET seq=EXCLUDED.seq,held_at=now()`, rt.job.ID, c.Kind, c.Coordinate, key, c.Key, c.Seq)
		return err
	}
	return nil
}

// heldKey normalizes the held-tombstone version: only assets are version-scoped.
func heldKey(kind, version string) string {
	if kind == "asset" {
		return version
	}
	return ""
}

const (
	dispositionWouldDelete = "would_delete"
	dispositionProtected   = "protected"
	dispositionAbsent      = "absent"
)

// classify reports what a tombstone would do without changing anything.
func (s *Service) classify(ctx context.Context, q pgxQuerier, rt *run, kind, coordinate, version, key string) (string, error) {
	switch kind {
	case "asset":
		var owner *string
		err := q.QueryRow(ctx, `SELECT a.provenance->>'replication_job' FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id WHERE c.source_id=$1 AND c.coordinate=$2 AND v.version=$3 AND a.asset_key=$4`, rt.job.ReplicaSourceID, coordinate, version, key).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return dispositionAbsent, nil
		}
		if err != nil {
			return "", err
		}
		if owner != nil && *owner == rt.job.ID {
			return dispositionWouldDelete, nil
		}
		return dispositionProtected, nil
	case "tag":
		var exists bool
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tags t JOIN components c ON c.id=t.component_id WHERE c.source_id=$1 AND c.coordinate=$2 AND t.name=$3)`, rt.job.ReplicaSourceID, coordinate, key).Scan(&exists)
		if err != nil {
			return "", err
		}
		if exists {
			return dispositionWouldDelete, nil
		}
		return dispositionAbsent, nil
	default:
		var exists, empty bool
		err := q.QueryRow(ctx, `SELECT true,NOT EXISTS(SELECT 1 FROM versions v WHERE v.component_id=c.id) FROM components c WHERE c.source_id=$1 AND c.coordinate=$2`, rt.job.ReplicaSourceID, coordinate).Scan(&exists, &empty)
		if errors.Is(err, pgx.ErrNoRows) {
			return dispositionAbsent, nil
		}
		if err != nil {
			return "", err
		}
		if empty {
			return dispositionWouldDelete, nil
		}
		return dispositionProtected, nil
	}
}

type pgxQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// performDelete removes replicated content. Every statement is scoped to the
// job's replica source and, for assets, to rows this job provenance-stamped:
// destination-only content is classified protected and left untouched.
func (s *Service) performDelete(ctx context.Context, tx pgx.Tx, rt *run, kind, coordinate, version, key string) (string, error) {
	disposition, err := s.classify(ctx, tx, rt, kind, coordinate, version, key)
	if err != nil || disposition != dispositionWouldDelete {
		return disposition, err
	}
	var comp, ver string
	switch kind {
	case "asset":
		err = tx.QueryRow(ctx, `DELETE FROM assets a USING versions v,components c WHERE a.version_id=v.id AND v.component_id=c.id AND c.source_id=$1 AND c.coordinate=$2 AND v.version=$3 AND a.asset_key=$4 AND a.provenance->>'replication_job'=$5 RETURNING c.id::text,v.id::text`,
			rt.job.ReplicaSourceID, coordinate, version, key, rt.job.ID).Scan(&comp, &ver)
	case "tag":
		err = tx.QueryRow(ctx, `DELETE FROM tags t USING components c WHERE t.component_id=c.id AND c.source_id=$1 AND c.coordinate=$2 AND t.name=$3 RETURNING c.id::text,t.version_id::text`, rt.job.ReplicaSourceID, coordinate, key).Scan(&comp, &ver)
	default:
		_, err = tx.Exec(ctx, `DELETE FROM components c WHERE c.source_id=$1 AND c.coordinate=$2 AND NOT EXISTS(SELECT 1 FROM versions v WHERE v.component_id=c.id)`, rt.job.ReplicaSourceID, coordinate)
		return dispositionWouldDelete, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return dispositionAbsent, nil
	}
	if err != nil {
		return "", err
	}
	// Remove only now-empty scaffolding; blobs are left for retention/GC.
	if _, err = tx.Exec(ctx, `DELETE FROM versions v WHERE v.id=$1 AND NOT EXISTS(SELECT 1 FROM assets WHERE version_id=v.id) AND NOT EXISTS(SELECT 1 FROM tags WHERE version_id=v.id)`, ver); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM components c WHERE c.id=$1 AND NOT EXISTS(SELECT 1 FROM versions WHERE component_id=c.id)`, comp); err != nil {
		return "", err
	}
	return dispositionWouldDelete, nil
}

// applyHeld applies tombstones that were held while the policy was 'ignore'
// once the policy has been switched to 'propagate'.
func (s *Service) applyHeld(ctx context.Context, rt *run) error {
	if rt.job.DeletionPolicy != "propagate" || rt.job.held == 0 {
		return nil
	}
	return s.commit(ctx, rt, func(tx pgx.Tx, d *delta) error {
		rows, err := tx.Query(ctx, `SELECT kind,coordinate,version,item_key FROM replication_held_deletions WHERE job_id=$1 ORDER BY seq LIMIT $2`, rt.job.ID, s.cfg.PageSize)
		if err != nil {
			return err
		}
		type held struct{ kind, coordinate, version, key string }
		var list []held
		for rows.Next() {
			var h held
			if err = rows.Scan(&h.kind, &h.coordinate, &h.version, &h.key); err != nil {
				rows.Close()
				return err
			}
			list = append(list, h)
		}
		rows.Close()
		if rows.Err() != nil {
			return rows.Err()
		}
		for _, h := range list {
			res, err := s.performDelete(ctx, tx, rt, h.kind, h.coordinate, h.version, h.key)
			if err != nil {
				return err
			}
			if res == dispositionWouldDelete {
				d.deletions++
			}
			if res == dispositionProtected {
				d.conflicts++
			}
			if _, err = tx.Exec(ctx, `DELETE FROM replication_held_deletions WHERE job_id=$1 AND kind=$2 AND coordinate=$3 AND version=$4 AND item_key=$5`, rt.job.ID, h.kind, h.coordinate, h.version, h.key); err != nil {
				return err
			}
		}
		return nil
	})
}
