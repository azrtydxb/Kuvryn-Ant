package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

// PublishBundle verifies every asset before atomically exposing one complete
// package version. Up to 16 distinct asset keys share one feed/namespace/package/
// version and resolved local source. Input/result order is preserved. Existing
// versions must contain exactly this entire bundle with matching content/media;
// partial or expanded versions conflict. Identical retries create no events.
// Failed staging/metadata can leave unreferenced verified bytes for normal GC.
func (s *Service) PublishBundle(ctx context.Context, p identity.Principal, publications []Publication, bodies []io.Reader) ([]Record, error) {
	if s == nil || s.DB == nil || s.Lease == nil || s.Router == nil || s.Storage == nil || p.PrincipalID == "" || len(publications) < 1 || len(publications) > 16 || len(publications) != len(bodies) {
		return nil, ErrInput
	}
	base := publications[0]
	keys := map[string]Publication{}
	for i, item := range publications {
		if bodies[i] == nil || !valid(item) || item.FeedID != base.FeedID || item.Namespace != base.Namespace || item.Coordinate != base.Coordinate || item.Version != base.Version {
			return nil, ErrInput
		}
		if _, duplicate := keys[item.AssetKey]; duplicate {
			return nil, ErrInput
		}
		keys[item.AssetKey] = item
	}
	initialCtx, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	var initialRevision int64
	err := s.DB.QueryRow(initialCtx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, base.FeedID).Scan(&initialRevision)
	initialCancel()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	router, err := s.Router(ctx, base.FeedID)
	if err != nil || router == nil {
		return nil, ErrUnavailable
	}
	source := ""
	for i := range publications {
		item := publications[i]
		decision, err := router.Resolve(routing.Request{Namespace: item.Namespace, Package: item.Coordinate, Path: "/" + item.AssetKey, Principal: p.PrincipalID})
		if err != nil {
			return nil, ErrInput
		}
		if !decision.Allowed {
			return nil, ErrDenied
		}
		if item.ExpectedSource != "" && item.ExpectedSource != decision.TargetID {
			return nil, ErrConflict
		}
		if source != "" && source != decision.TargetID {
			return nil, ErrConflict
		}
		source = decision.TargetID
		publications[i].MatchedRule = decision.RuleID
	}
	prepared, err := s.Prepare(ctx, p, base.FeedID, base.Namespace, base.Coordinate, base.AssetKey)
	if err != nil {
		return nil, err
	}
	if prepared != source {
		return nil, ErrConflict
	}
	for _, item := range publications {
		if err = s.authorize(ctx, p, item, source); err != nil {
			return nil, err
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var domain string
	var revision int64
	err = s.DB.QueryRow(checkCtx, `SELECT d.id::text,f.revision FROM feeds f JOIN feed_sources fs ON fs.feed_id=f.id AND fs.instance_id=f.instance_id JOIN sources s ON s.id=fs.source_id AND s.instance_id=f.instance_id JOIN dedup_domains d ON d.instance_id=f.instance_id AND d.name='source:'||s.id::text WHERE f.instance_id=$1 AND f.id=$2 AND s.id=$3 AND s.kind='local' AND s.format=f.format`, p.InstanceID, base.FeedID, source).Scan(&domain, &revision)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	if revision != initialRevision {
		return nil, ErrConflict
	}
	store, err := s.Storage(ctx, domain)
	if err != nil || store == nil {
		return nil, ErrUnavailable
	}
	buffer := make([]byte, 64<<10)
	for i, item := range publications {
		if err = stageBundleAsset(ctx, store, item, bodies[i], buffer); err != nil {
			return nil, err
		}
	}
	for _, item := range publications {
		if err = s.authorize(ctx, p, item, source); err != nil {
			return nil, err
		}
	}
	commitCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	tx, err := s.DB.BeginTx(commitCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return nil, ErrDenied
	}
	if err = ownership.Require(commitCtx, tx, lease); err != nil {
		return nil, err
	}
	var current int64
	err = tx.QueryRow(commitCtx, `SELECT f.revision FROM feeds f JOIN feed_sources fs ON fs.feed_id=f.id JOIN sources s ON s.id=fs.source_id WHERE f.instance_id=$1 AND f.id=$2 AND s.id=$3 AND s.instance_id=f.instance_id AND s.kind='local' AND s.format=f.format FOR UPDATE OF f,s,fs`, p.InstanceID, base.FeedID, source).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	if current != revision {
		return nil, ErrConflict
	}
	lock := sha256.Sum256([]byte(p.InstanceID + "\x00" + source + "\x00" + base.Coordinate))
	if _, err = tx.Exec(commitCtx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(lock[:8]))); err != nil {
		return nil, ErrUnavailable
	}
	var component, version string
	err = tx.QueryRow(commitCtx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate WHERE components.namespace=excluded.namespace RETURNING id::text`, p.InstanceID, source, base.Coordinate, base.Namespace).Scan(&component)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConflict
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	err = tx.QueryRow(commitCtx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) ON CONFLICT(component_id,version) DO UPDATE SET version=excluded.version RETURNING id::text`, p.InstanceID, component, base.Version).Scan(&version)
	if err != nil {
		return nil, ErrUnavailable
	}
	var count int
	if err = tx.QueryRow(commitCtx, `SELECT count(*) FROM (SELECT 1 FROM assets WHERE version_id=$1 LIMIT 17) existing_assets`, version).Scan(&count); err != nil {
		return nil, ErrUnavailable
	}
	if count != 0 && count != len(publications) {
		return nil, ErrConflict
	}
	// Validate all dependencies before inserts, accepting references to other
	// verified members of this same atomic bundle independent of input order.
	for _, item := range publications {
		if count > 0 {
			var existing Record
			var digest string
			err = tx.QueryRow(commitCtx, `SELECT a.id::text,b.digest,b.size_bytes,a.media_type FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.version_id=$1 AND a.asset_key=$2`, version, item.AssetKey).Scan(&existing.ID, &digest, &existing.Info.Size, &existing.MediaType)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrConflict
			}
			if err != nil {
				return nil, ErrUnavailable
			}
			if digest != string(item.Info.Digest) || existing.Info.Size != item.Info.Size || existing.MediaType != item.MediaType {
				return nil, ErrConflict
			}
			if item.Tag != "" {
				var tagged string
				err = tx.QueryRow(commitCtx, `SELECT version_id::text FROM tags WHERE component_id=$1 AND name=$2`, component, item.Tag).Scan(&tagged)
				if errors.Is(err, pgx.ErrNoRows) || err == nil && tagged != version {
					return nil, ErrConflict
				}
				if err != nil {
					return nil, ErrUnavailable
				}
			}
		}
		for _, ref := range item.References {
			if member, ok := keys[ref.AssetKey]; ok && ref.Version == base.Version {
				if member.Info != ref.Info {
					return nil, blob.ErrIntegrity
				}
				continue
			}
			var digest string
			var size int64
			err = tx.QueryRow(commitCtx, `SELECT b.digest,b.size_bytes FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND v.version=$2 AND a.asset_key=$3`, component, ref.Version, ref.AssetKey).Scan(&digest, &size)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			if err != nil {
				return nil, ErrUnavailable
			}
			if digest != string(ref.Info.Digest) || size != ref.Info.Size {
				return nil, blob.ErrIntegrity
			}
		}
	}
	records := make([]Record, 0, len(publications))
	for _, item := range publications {
		record, err := commitAsset(commitCtx, tx, store, p, item, source, domain, component, version, false)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err = tx.Commit(commitCtx); err != nil {
		return nil, ErrUnavailable
	}
	return records, nil
}
func stageBundleAsset(ctx context.Context, store blob.Store, item Publication, body io.Reader, buffer []byte) (resultErr error) {
	upload, err := store.Stage(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, upload.Abort()) }()
	n, err := io.CopyBuffer(upload, io.LimitReader(body, item.Info.Size), buffer)
	if err != nil {
		return err
	}
	if n != item.Info.Size {
		return blob.ErrIntegrity
	}
	var extra [1]byte
	nExtra, err := io.ReadFull(body, extra[:])
	if nExtra != 0 {
		return blob.ErrIntegrity
	}
	if err != io.EOF {
		return err
	}
	_, err = upload.Finalize(ctx, item.Info)
	return err
}
