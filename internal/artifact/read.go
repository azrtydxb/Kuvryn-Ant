package artifact

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// Lookup uses package-owner composition: the first source containing the
// coordinate owns every version. Missing assets never fall through to another
// source. Artifact authorization remains separate from source administration.
func (s *Service) Lookup(ctx context.Context, p identity.Principal, r Publication) (Record, error) {
	if s == nil || s.DB == nil || p.PrincipalID == "" || !bounded(r.FeedID, 36) || !bounded(r.Coordinate, 256) || !bounded(r.Version, 256) || !bounded(r.AssetKey, 2048) {
		return Record{}, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Read ownership and the asset in one statement's snapshot.
	result := Record{}
	err := s.DB.QueryRow(ctx, `WITH owner AS (
 SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1
 ) SELECT a.id::text,o.source_id::text,v.id::text,b.dedup_domain_id::text,a.media_type,b.digest,b.size_bytes FROM owner o JOIN versions v ON v.component_id=o.id JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.version=$4 AND a.asset_key=$5 AND o.namespace=$6`, p.InstanceID, r.FeedID, r.Coordinate, r.Version, r.AssetKey, r.Namespace).Scan(&result.ID, &result.SourceID, &result.VersionID, &result.DomainID, &result.MediaType, &result.Info.Digest, &result.Info.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrUnavailable
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: r.FeedID, SourceID: result.SourceID, Namespace: r.Namespace, Kind: identity.ArtifactResource}, identity.Read) {
		return Record{}, ErrNotFound
	}
	return result, nil
}

// Open resolves and authorizes again; accepting arbitrary caller-supplied blob
// IDs or a stale authorization result would bypass feed permissions.
func (s *Service) Open(ctx context.Context, p identity.Principal, r Publication, offset, length int64) (Record, io.ReadCloser, error) {
	record, err := s.Lookup(ctx, p, r)
	if err != nil {
		return Record{}, nil, err
	}
	if s.Storage == nil {
		return Record{}, nil, ErrUnavailable
	}
	store, err := s.Storage(ctx, record.DomainID)
	if err != nil || store == nil {
		return Record{}, nil, ErrUnavailable
	}
	info, err := store.Stat(ctx, record.Info.Digest)
	if err != nil {
		return Record{}, nil, err
	}
	if info != record.Info {
		return Record{}, nil, blob.ErrIntegrity
	}
	reader, err := store.OpenRange(ctx, record.Info.Digest, offset, length)
	if err != nil {
		return Record{}, nil, err
	}
	if s.Downloads != nil {
		s.Downloads.Download(record.ID)
	}
	return record, reader, nil
}

// ResolveTag binds a mutable pointer to the current package owner. The resulting
// version must still be passed through Lookup/Open so access is checked there too.
func (s *Service) ResolveTag(ctx context.Context, p identity.Principal, r Publication, tag string) (string, error) {
	if s == nil || s.DB == nil || p.PrincipalID == "" || !bounded(tag, 128) || !bounded(r.Coordinate, 256) || !bounded(r.FeedID, 36) {
		return "", ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var source, version string
	err := s.DB.QueryRow(ctx, `WITH owner AS(SELECT c.id,c.source_id,c.namespace FROM feed_sources fs JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 ORDER BY fs.priority LIMIT 1) SELECT o.source_id::text,v.version FROM owner o JOIN tags t ON t.component_id=o.id JOIN versions v ON v.id=t.version_id WHERE t.name=$4 AND t.observed_at IS NULL AND o.namespace=$5`, p.InstanceID, r.FeedID, r.Coordinate, tag, r.Namespace).Scan(&source, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if err != nil {
		return "", ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: r.FeedID, SourceID: source, Namespace: r.Namespace, Kind: identity.ArtifactResource}, identity.Read) {
		return "", ErrNotFound
	}
	return version, nil
}
