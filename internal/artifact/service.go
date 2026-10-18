// Package artifact implements protocol-neutral, immutable artifact publication.
// Adapters validate their format before calling Publish; this package owns storage,
// authorization, transactional visibility and provenance.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/blob"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInput       = errors.New("invalid artifact request")
	ErrDenied      = errors.New("artifact access denied")
	ErrNotFound    = errors.New("artifact not found")
	ErrConflict    = errors.New("immutable artifact conflicts")
	ErrUnavailable = errors.New("artifact service unavailable")
)

// Router and Storage are trusted configuration resolvers, not caller-provided
// destinations. Storage must isolate each deduplication domain's object keys.
type Service struct {
	DB      *pgxpool.Pool
	Lease   func() ownership.Lease
	Router  func(context.Context, string) (*routing.Router, error)
	Storage func(context.Context, string) (blob.Store, error)
	// Remote enables pull-through reads (see remote.go). Nil disables them.
	Remote RemoteBackend
	// Downloads, when set, counts successful reads per asset (cheap, in-memory).
	Downloads interface{ Download(assetID string) }
	// MaxMaterializeBytes bounds one cached upstream artifact; zero means 8 GiB.
	MaxMaterializeBytes int64

	flightMu sync.Mutex
	flights  map[string]*flight
}
type Reference struct {
	Version, AssetKey string
	Info              blob.Info
}
type Publication struct {
	Subject                                                     *Subject
	ArtifactType                                                string
	Annotations                                                 map[string]string
	ExpectedSource                                              string
	References                                                  []Reference
	Tag                                                         string
	FeedID, Namespace, Coordinate, Version, AssetKey, MediaType string
	// MatchedRule is the publish-policy rule that selected the target, recorded as provenance.
	MatchedRule string
	Info        blob.Info
}
type Record struct {
	ID, SourceID, VersionID, DomainID, MediaType string
	Info                                         blob.Info
	Replayed                                     bool
}

func bounded(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func valid(p Publication) bool {
	if !validSubject(p) || !bounded(p.FeedID, 36) || !bounded(p.Coordinate, 256) || !bounded(p.Version, 256) || !bounded(p.AssetKey, 2048) || !bounded(p.MediaType, 256) || len(p.Namespace) > 256 || !utf8.ValidString(p.Namespace) || !p.Info.Digest.Valid() || p.Info.Size < 0 {
		return false
	}
	if len(p.References) > 4096 || len(p.Tag) > 128 || strings.ContainsAny(p.Tag, "/\x00\r\n") {
		return false
	}
	for _, ref := range p.References {
		if !bounded(ref.Version, 256) || !bounded(ref.AssetKey, 2048) || !ref.Info.Digest.Valid() || ref.Info.Size < 0 {
			return false
		}
	}
	normalized, err := routing.NormalizePath("/" + p.AssetKey)
	return err == nil && normalized == "/"+p.AssetKey
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func (s *Service) authorize(ctx context.Context, p identity.Principal, r Publication, source string) error {
	policy, err := (identity.Roles{DB: s.DB}).Resolve(ctx, p)
	if errors.Is(err, identity.ErrUnauthenticated) {
		return ErrDenied
	}
	if err != nil {
		return ErrUnavailable
	}
	if !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: r.FeedID, SourceID: source, Namespace: r.Namespace, Kind: identity.ArtifactResource}, identity.Publish) {
		return ErrDenied
	}
	if r.Subject != nil && !policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, FeedID: r.FeedID, SourceID: source, Namespace: r.Namespace, Kind: identity.ArtifactResource}, identity.Read) {
		return ErrDenied
	}
	return nil
}

// Publish streams to private staging, then finalizes immutable verified bytes.
// A metadata failure can leave unreferenced bytes, never a visible partial asset.
// Byte-identical retries return the original asset without another success event.
func (s *Service) Publish(ctx context.Context, p identity.Principal, r Publication, body io.Reader) (Record, error) {
	if s == nil || s.DB == nil || s.Lease == nil || s.Router == nil || s.Storage == nil || body == nil || !valid(r) || p.PrincipalID == "" {
		return Record{}, ErrInput
	}
	var initialRevision int64
	initialCtx, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	initialErr := s.DB.QueryRow(initialCtx, "SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2", p.InstanceID, r.FeedID).Scan(&initialRevision)
	initialCancel()
	if errors.Is(initialErr, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if initialErr != nil {
		return Record{}, ErrUnavailable
	}
	router, err := s.Router(ctx, r.FeedID)
	if err != nil || router == nil {
		return Record{}, ErrUnavailable
	}
	decision, err := router.Resolve(routing.Request{Namespace: r.Namespace, Package: r.Coordinate, Path: "/" + r.AssetKey, Principal: p.PrincipalID})
	if err != nil {
		return Record{}, ErrInput
	}
	if !decision.Allowed {
		return Record{}, ErrDenied
	}
	source := decision.TargetID
	r.MatchedRule = decision.RuleID
	if r.ExpectedSource != "" && r.ExpectedSource != source {
		return Record{}, ErrConflict
	}
	if _, err = s.Prepare(ctx, p, r.FeedID, r.Namespace, r.Coordinate, r.AssetKey); err != nil {
		return Record{}, err
	}
	if err = s.authorize(ctx, p, r, source); err != nil {
		return Record{}, err
	}
	// Check the configured relationship before reading any body. Recheck while
	// holding the feed row at commit so concurrent detach cannot authorize a write.
	var domain string
	var feedRevision int64
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = s.DB.QueryRow(checkCtx, `SELECT d.id::text,f.revision FROM feeds f JOIN feed_sources fs ON fs.feed_id=f.id AND fs.instance_id=f.instance_id JOIN sources s ON s.id=fs.source_id AND s.instance_id=f.instance_id JOIN dedup_domains d ON d.instance_id=f.instance_id AND d.name='source:'||s.id::text WHERE f.instance_id=$1 AND f.id=$2 AND s.id=$3 AND s.kind='local' AND s.format=f.format`, p.InstanceID, r.FeedID, source).Scan(&domain, &feedRevision)
	cancel()
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if feedRevision != initialRevision {
		return Record{}, ErrConflict
	}
	store, err := s.Storage(ctx, domain)
	if err != nil || store == nil {
		return Record{}, ErrUnavailable
	}
	upload, err := store.Stage(ctx)
	if err != nil {
		return Record{}, err
	}
	defer upload.Abort()
	// Copy at most the declared content plus one byte, detecting overlong input
	// without accepting an unbounded stream. LimitReader cannot overflow at MaxInt64.
	remaining := r.Info.Size
	buffer := make([]byte, 64*1024)
	n, err := io.CopyBuffer(upload, io.LimitReader(body, remaining), buffer)
	if err != nil {
		return Record{}, err
	}
	if n != remaining {
		return Record{}, blob.ErrIntegrity
	}
	var extra [1]byte
	nExtra, extraErr := io.ReadFull(body, extra[:])
	if nExtra != 0 {
		return Record{}, blob.ErrIntegrity
	}
	if extraErr != io.EOF {
		return Record{}, extraErr
	}
	if _, err = upload.Finalize(ctx, r.Info); err != nil {
		return Record{}, err
	}
	if err = s.authorize(ctx, p, r, source); err != nil {
		return Record{}, err
	}
	// Concurrent publishes (a multi-platform image pushes several blobs at once)
	// contend on the feed/source rows and on foreign-key share locks taken by
	// upload sessions; PostgreSQL may pick one as a deadlock victim. The bytes
	// are already staged, so only the metadata transaction is retried.
	var result Record
	for attempt := 0; ; attempt++ {
		result, err = s.publishTx(ctx, p, r, source, domain, feedRevision, store)
		if err == nil || attempt >= 4 || !transientDB(err) || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	return result, err
}

// dbErr reports a database failure as ErrUnavailable while keeping the cause
// for retry decisions.
func dbErr(err error) error {
	if err == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// transientDB reports a deadlock or serialization failure, safe to retry.
func transientDB(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001")
}

func (s *Service) publishTx(ctx context.Context, p identity.Principal, r Publication, source, domain string, feedRevision int64, store blob.Store) (Record, error) {
	commitCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	tx, err := s.DB.BeginTx(commitCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Record{}, dbErr(err)
	}
	defer rollback(tx)
	lease := s.Lease()
	if lease.InstanceID != p.InstanceID {
		return Record{}, ErrDenied
	}
	if err = ownership.Require(commitCtx, tx, lease); err != nil {
		return Record{}, err
	}
	var feedID string
	var currentRevision int64
	err = tx.QueryRow(commitCtx, `SELECT f.id::text,f.revision FROM feeds f JOIN feed_sources fs ON fs.feed_id=f.id JOIN sources s ON s.id=fs.source_id WHERE f.instance_id=$1 AND f.id=$2 AND s.id=$3 AND s.instance_id=f.instance_id AND s.kind='local' AND s.format=f.format FOR UPDATE OF f,s,fs`, p.InstanceID, r.FeedID, source).Scan(&feedID, &currentRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, dbErr(err)
	}
	if currentRevision != feedRevision {
		return Record{}, ErrConflict
	}
	// Serialize every asset mutation for one package coordinate. Namespace is
	// adapter-derived metadata; Coordinate must be the full normalized identity.
	digest := sha256.Sum256([]byte(p.InstanceID + "\x00" + source + "\x00" + r.Coordinate))
	if _, err = tx.Exec(commitCtx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		return Record{}, dbErr(err)
	}
	var component, version string
	err = tx.QueryRow(commitCtx, `INSERT INTO components(instance_id,source_id,coordinate,namespace) VALUES($1,$2,$3,$4) ON CONFLICT(source_id,coordinate) DO UPDATE SET coordinate=excluded.coordinate WHERE components.namespace=excluded.namespace RETURNING id::text`, p.InstanceID, source, r.Coordinate, r.Namespace).Scan(&component)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrConflict
	}
	if err != nil {
		return Record{}, dbErr(err)
	}
	err = tx.QueryRow(commitCtx, `INSERT INTO versions(instance_id,component_id,version) VALUES($1,$2,$3) ON CONFLICT(component_id,version) DO UPDATE SET version=excluded.version RETURNING id::text`, p.InstanceID, component, r.Version).Scan(&version)
	if err != nil {
		return Record{}, dbErr(err)
	}
	result, err := commitAsset(commitCtx, tx, store, p, r, source, domain, component, version, true)
	if err != nil {
		return Record{}, err
	}
	if err = tx.Commit(commitCtx); err != nil {
		return Record{}, dbErr(err)
	}
	return result, nil
}

// commitAsset never commits: its caller owns atomic visibility and rollback.
func commitAsset(commitCtx context.Context, tx pgx.Tx, store blob.Store, p identity.Principal, r Publication, source, domain, component, version string, checkReferences bool) (Record, error) {
	var err error
	// Manifest/index closures are checked in the same transaction as publication.
	if checkReferences {
		for _, ref := range r.References {
			var refDigest string
			var refSize int64
			err = tx.QueryRow(commitCtx, `SELECT b.digest,b.size_bytes FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.component_id=$1 AND v.version=$2 AND a.asset_key=$3`, component, ref.Version, ref.AssetKey).Scan(&refDigest, &refSize)
			if errors.Is(err, pgx.ErrNoRows) {
				return Record{}, ErrNotFound
			}
			if err != nil {
				return Record{}, ErrUnavailable
			}
			if refDigest != string(ref.Info.Digest) || refSize != ref.Info.Size {
				return Record{}, blob.ErrIntegrity
			}
		}
	}
	// Tags are immutable until an explicit mutable-tag policy is implemented.
	if r.Tag != "" {
		var tagged string
		err = tx.QueryRow(commitCtx, "SELECT version_id::text FROM tags WHERE component_id=$1 AND name=$2", component, r.Tag).Scan(&tagged)
		if err == nil && tagged != version {
			return Record{}, ErrConflict
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Record{}, ErrUnavailable
		}
	}
	result := Record{SourceID: source, VersionID: version, DomainID: domain, Info: r.Info, MediaType: r.MediaType}
	var oldDigest, oldMedia string
	var oldSize int64
	err = tx.QueryRow(commitCtx, `SELECT a.id::text,b.digest,b.size_bytes,a.media_type FROM assets a JOIN blobs b ON b.id=a.blob_id WHERE a.instance_id=$1 AND a.version_id=$2 AND a.asset_key=$3`, p.InstanceID, version, r.AssetKey).Scan(&result.ID, &oldDigest, &oldSize, &oldMedia)
	if err == nil {
		if oldDigest != string(r.Info.Digest) || oldSize != r.Info.Size || oldMedia != r.MediaType {
			return Record{}, ErrConflict
		}
		if r.Tag != "" {
			tag, tagErr := tx.Exec(commitCtx, "INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3) ON CONFLICT(component_id,name) DO NOTHING", component, r.Tag, version)
			if tagErr != nil {
				return Record{}, ErrUnavailable
			}
			if tag.RowsAffected() == 1 {
				var audit int64
				if tx.QueryRow(commitCtx, "INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'artifact.tag',$3) RETURNING id", p.InstanceID, p.PrincipalID, result.ID).Scan(&audit) != nil {
					return Record{}, ErrUnavailable
				}
				if _, tagErr = tx.Exec(commitCtx, "INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'artifact.tag',jsonb_build_object('asset_id',$2::text,'tag',$3::text))", audit, result.ID, r.Tag); tagErr != nil {
					return Record{}, ErrUnavailable
				}
			}
		}

		if err = commitSubjectTx(commitCtx, tx, r, p.InstanceID, component, result.ID); err != nil {
			return Record{}, err
		}
		result.Replayed = true
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrUnavailable
	}
	var blobID string
	var size int64
	err = tx.QueryRow(commitCtx, `INSERT INTO blobs(instance_id,dedup_domain_id,digest,size_bytes,storage_key) VALUES($1,$2,$3,$4,$3) ON CONFLICT(dedup_domain_id,digest_algorithm,digest) DO UPDATE SET digest=excluded.digest RETURNING id::text,size_bytes`, p.InstanceID, domain, r.Info.Digest, r.Info.Size).Scan(&blobID, &size)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if size != r.Info.Size {
		return Record{}, blob.ErrIntegrity
	}
	if err = requireBlobBytes(commitCtx, store, r.Info); err != nil {
		return Record{}, err
	}
	err = tx.QueryRow(commitCtx, `INSERT INTO assets(instance_id,version_id,asset_key,blob_id,media_type,provenance) VALUES($1,$2,$3,$4,$5,jsonb_build_object('source_id',$6::text,'principal_id',$7::text,'feed_id',$8::text,'token_id',$9::text,'matched_rule',$10::text)) RETURNING id::text`, p.InstanceID, version, r.AssetKey, blobID, r.MediaType, source, p.PrincipalID, r.FeedID, p.TokenID, r.MatchedRule).Scan(&result.ID)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	if err = commitSubjectTx(commitCtx, tx, r, p.InstanceID, component, result.ID); err != nil {
		return Record{}, err
	}
	if r.Tag != "" {
		if _, err = tx.Exec(commitCtx, "INSERT INTO tags(component_id,name,version_id) VALUES($1,$2,$3) ON CONFLICT(component_id,name) DO NOTHING", component, r.Tag, version); err != nil {
			return Record{}, ErrUnavailable
		}
	}
	var audit int64
	err = tx.QueryRow(commitCtx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'artifact.publish',$3) RETURNING id`, p.InstanceID, p.PrincipalID, result.ID).Scan(&audit)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	_, err = tx.Exec(commitCtx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'artifact.publish',jsonb_build_object('asset_id',$2::text,'source_id',$3::text,'digest',$4::text))`, audit, result.ID, source, r.Info.Digest)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	return result, nil
}

// requireBlobBytes verifies, inside the commit transaction and after the blobs
// row has been inserted or locked, that the bytes still exist. The garbage
// collector deletes bytes only while holding that row's lock, so once this check
// passes the sweep cannot remove the bytes before the metadata commits.
func requireBlobBytes(ctx context.Context, store blob.Store, info blob.Info) error {
	if store == nil {
		return ErrUnavailable
	}
	got, err := store.Stat(ctx, info.Digest)
	if err != nil || got.Size != info.Size {
		return ErrUnavailable
	}
	return nil
}
