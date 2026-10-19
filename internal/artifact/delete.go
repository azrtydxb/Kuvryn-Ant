package artifact

// Policy-gated deletion and unlisting (spec 6.1). Every adapter, the management
// API and MCP route removals through here so authorization, delete policy,
// audit, outbox and replication tombstones are identical for every format.
//
// Bytes are never removed inline: deleting assets only drops metadata rows, and
// internal/retention garbage collection later reclaims blobs that no asset
// references (after its grace period). Replication tombstones are recorded by
// the database change-feed triggers (migration 020) in the same transaction.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/ownership"
	"github.com/jackc/pgx/v5"
)

// The policy errors wrap the generic sentinels so every adapter's existing
// error mapping already yields a sensible status (403 for policy, 409 for
// unlist-only and immutable), while policy-aware callers can distinguish them.
var (
	// ErrPolicy means the feed or namespace delete policy forbids the operation.
	ErrPolicy = fmt.Errorf("deletion denied by policy: %w", ErrDenied)
	// ErrUnlistOnly means the policy only allows hiding a version, not deleting it.
	ErrUnlistOnly = fmt.Errorf("policy permits unlisting only: %w", ErrConflict)
	// ErrImmutable means the policy protects release versions from deletion.
	ErrImmutable = fmt.Errorf("release version is immutable: %w", ErrConflict)
)

// DeleteMode is the per-feed or per-namespace deletion mode.
type DeleteMode string

const (
	DeleteAllowed    DeleteMode = "allowed"
	DeleteDenied     DeleteMode = "denied"
	DeleteUnlistOnly DeleteMode = "unlist_only"
)

// Valid reports whether the mode is one of the defined values.
func (m DeleteMode) Valid() bool {
	return m == DeleteAllowed || m == DeleteDenied || m == DeleteUnlistOnly
}

// DeletePolicy is the effective policy for one feed namespace.
type DeletePolicy struct {
	Namespace         string     `json:"namespace"`
	Mode              DeleteMode `json:"mode"`
	ImmutableReleases bool       `json:"immutable_releases"`
	Revision          int64      `json:"revision"`
}

// Action is the kind of listing or removal being applied.
type Action string

const (
	ActionDelete Action = "delete"
	ActionUnlist Action = "unlist"
	ActionRelist Action = "relist"
)

// Target addresses a version (AssetKey empty) or one asset of a version of a
// package in a feed. SourceID optionally pins the owning source; otherwise the
// highest-priority source containing the coordinate owns it, as for reads.
//
// A non-empty Tag addresses only that mutable tag pointer (OCI tag delete): the
// version it points to and its bytes are untouched and Version must be empty.
type Target struct {
	FeedID, SourceID, Namespace, Coordinate, Version, AssetKey, Tag string
}

// Outcome reports what a mutation changed. Unchanged is set when an unlist or
// relist found the version already in the requested state (no audit is written).
type Outcome struct {
	Action                           Action
	SourceID, ComponentID, VersionID string
	AssetsRemoved, TagsRemoved       int64
	VersionRemoved, ComponentRemoved bool
	Unchanged                        bool
	AuditID                          int64
	Policy                           DeletePolicy
}

// Change is passed to Service.Changed after a mutation commits, so adapters and
// caches can invalidate derived metadata. It never carries credentials.
type Change struct {
	Action                                                     Action
	FeedID, SourceID, Namespace, Coordinate, Version, AssetKey string
}

// IsSnapshot reports whether a version is a mutable development build exempt
// from release immutability (Maven-style "-SNAPSHOT").
func IsSnapshot(version string) bool {
	return strings.HasSuffix(strings.ToUpper(version), "-SNAPSHOT")
}

// Unlisted reports whether the component metadata marks the version unlisted.
func Unlisted(metadata map[string]any, version string) bool {
	list, _ := metadata["unlisted"].([]any)
	for _, v := range list {
		if s, ok := v.(string); ok && s == version {
			return true
		}
	}
	return false
}

func validTarget(t Target) bool {
	if t.Tag != "" {
		return bounded(t.FeedID, 36) && bounded(t.Coordinate, 256) && bounded(t.Tag, 128) && t.Version == "" && t.AssetKey == "" &&
			len(t.Namespace) <= 256 && (t.SourceID == "" || bounded(t.SourceID, 36))
	}
	return bounded(t.FeedID, 36) && bounded(t.Coordinate, 256) && bounded(t.Version, 256) &&
		(t.AssetKey == "" || bounded(t.AssetKey, 2048)) && len(t.Namespace) <= 256 && (t.SourceID == "" || bounded(t.SourceID, 36))
}

// Delete removes a version (or one asset) subject to authorization and policy.
func (s *Service) Delete(ctx context.Context, p identity.Principal, t Target) (Outcome, error) {
	return s.mutate(ctx, p, ActionDelete, t)
}

// Unlist hides a version from client listings without removing anything.
func (s *Service) Unlist(ctx context.Context, p identity.Principal, t Target) (Outcome, error) {
	return s.mutate(ctx, p, ActionUnlist, t)
}

// Relist reverses Unlist (cargo unyank, nuget relist).
func (s *Service) Relist(ctx context.Context, p identity.Principal, t Target) (Outcome, error) {
	return s.mutate(ctx, p, ActionRelist, t)
}

func (s *Service) mutate(ctx context.Context, p identity.Principal, action Action, t Target) (Outcome, error) {
	if s == nil || s.DB == nil || s.Lease == nil || p.PrincipalID == "" || !validTarget(t) {
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
		out, err = s.mutateOnce(ctx, p, policy, action, t)
		if err == nil || attempt >= 4 || !transientDB(err) || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
	}
	if err == nil && !out.Unchanged && s.Changed != nil {
		s.Changed(Change{Action: action, FeedID: t.FeedID, SourceID: out.SourceID, Namespace: t.Namespace, Coordinate: t.Coordinate, Version: t.Version, AssetKey: t.AssetKey})
	}
	return out, err
}

func (s *Service) mutateOnce(ctx context.Context, p identity.Principal, policy identity.Policy, action Action, t Target) (Outcome, error) {
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
	out, err := MutateTx(ctx, tx, p, policy, action, t)
	if err != nil {
		return Outcome{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Outcome{}, dbErr(err)
	}
	return out, nil
}

// EffectiveDeletePolicy loads the namespace policy, falling back to the feed
// policy and finally to the permissive default.
func EffectiveDeletePolicy(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, instance, feed, namespace string) (DeletePolicy, error) {
	d := DeletePolicy{Namespace: "", Mode: DeleteAllowed}
	err := q.QueryRow(ctx, `SELECT namespace,mode,immutable_releases,revision FROM delete_policies WHERE instance_id=$1 AND feed_id=$2 AND namespace IN ($3,'') ORDER BY (namespace<>'') DESC LIMIT 1`, instance, feed, namespace).Scan(&d.Namespace, &d.Mode, &d.ImmutableReleases, &d.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return d, ErrUnavailable
	}
	return d, nil
}

// MutateTx performs a delete, unlist or relist inside the caller's transaction:
// authorization, policy check, row changes, audit and outbox. It never commits;
// rolling the caller back removes every effect. The policy must come from
// trusted authorization code (identity.Roles.Resolve). The caller is
// responsible for the instance writer lease where it applies.
func MutateTx(ctx context.Context, tx pgx.Tx, p identity.Principal, policy identity.Policy, action Action, t Target) (Outcome, error) {
	if p.PrincipalID == "" || !validTarget(t) || (action != ActionDelete && action != ActionUnlist && action != ActionRelist) {
		return Outcome{}, ErrInput
	}
	if err := frozenFeed(ctx, tx, p.InstanceID, t.FeedID); err != nil {
		return Outcome{}, err
	}
	out := Outcome{Action: action}
	var kind, nsActual string
	find := func(lock bool) error {
		q := `SELECT c.id::text,c.source_id::text,c.namespace,s.kind FROM feed_sources fs JOIN sources s ON s.id=fs.source_id AND s.instance_id=fs.instance_id JOIN components c ON c.source_id=fs.source_id AND c.instance_id=fs.instance_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND c.coordinate=$3 AND ($4='' OR c.source_id::text=$4) ORDER BY fs.priority LIMIT 1`
		if lock {
			q += ` FOR UPDATE OF c`
		}
		return tx.QueryRow(ctx, q, p.InstanceID, t.FeedID, t.Coordinate, t.SourceID).Scan(&out.ComponentID, &out.SourceID, &nsActual, &kind)
	}
	if err := find(false); errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, ErrNotFound
	} else if err != nil {
		return Outcome{}, dbErr(err)
	}
	// Serialize with publication and other mutations of this package coordinate.
	digest := sha256.Sum256([]byte(p.InstanceID + "\x00" + out.SourceID + "\x00" + t.Coordinate))
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		return Outcome{}, dbErr(err)
	}
	if err := find(true); errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, ErrNotFound
	} else if err != nil {
		return Outcome{}, dbErr(err)
	}
	if nsActual != t.Namespace {
		return Outcome{}, ErrNotFound
	}
	// Existence is not disclosed to principals that cannot read the package.
	res := identity.Resource{InstanceID: p.InstanceID, FeedID: t.FeedID, SourceID: out.SourceID, Namespace: t.Namespace, Kind: identity.ArtifactResource}
	if !policy.Authorize(p, res, identity.Read) {
		return Outcome{}, ErrNotFound
	}
	if !policy.Authorize(p, res, identity.Delete) {
		return Outcome{}, ErrDenied
	}
	if kind != "local" {
		return Outcome{}, fmt.Errorf("%w: package is owned by a %s source; only local sources can be modified", ErrConflict, kind)
	}
	dp, err := EffectiveDeletePolicy(ctx, tx, p.InstanceID, t.FeedID, t.Namespace)
	if err != nil {
		return Outcome{}, err
	}
	out.Policy = dp
	switch {
	case dp.Mode == DeleteDenied:
		return Outcome{}, ErrPolicy
	case action == ActionDelete && dp.Mode == DeleteUnlistOnly:
		return Outcome{}, ErrUnlistOnly
	case action == ActionDelete && t.Tag == "" && dp.ImmutableReleases && !IsSnapshot(t.Version):
		return Outcome{}, ErrImmutable
	}
	resource := ""
	if t.Tag != "" {
		if action != ActionDelete {
			return Outcome{}, ErrInput
		}
		// Tag scope: drop the pointer only. The version and its bytes stay.
		if err = tx.QueryRow(ctx, `DELETE FROM tags WHERE component_id=$1 AND name=$2 RETURNING version_id::text`, out.ComponentID, t.Tag).Scan(&out.VersionID); errors.Is(err, pgx.ErrNoRows) {
			return Outcome{}, ErrNotFound
		} else if err != nil {
			return Outcome{}, dbErr(err)
		}
		out.TagsRemoved = 1
		resource = out.VersionID
		return finishMutation(ctx, tx, p, t, dp, action, resource, out)
	}
	if err = tx.QueryRow(ctx, `SELECT id::text FROM versions WHERE instance_id=$1 AND component_id=$2 AND version=$3 FOR UPDATE`, p.InstanceID, out.ComponentID, t.Version).Scan(&out.VersionID); errors.Is(err, pgx.ErrNoRows) {
		return Outcome{}, ErrNotFound
	} else if err != nil {
		return Outcome{}, dbErr(err)
	}
	resource = out.VersionID
	switch action {
	case ActionUnlist, ActionRelist:
		var tag int64
		if action == ActionUnlist {
			ct, e := tx.Exec(ctx, `UPDATE components SET metadata=jsonb_set(metadata,'{unlisted}',COALESCE(metadata->'unlisted','[]'::jsonb)||to_jsonb($2::text)) WHERE id=$1 AND NOT COALESCE(metadata->'unlisted' @> to_jsonb($2::text),false)`, out.ComponentID, t.Version)
			if e != nil {
				return Outcome{}, dbErr(e)
			}
			tag = ct.RowsAffected()
		} else {
			ct, e := tx.Exec(ctx, `UPDATE components SET metadata=jsonb_set(metadata,'{unlisted}',COALESCE((SELECT jsonb_agg(x) FROM jsonb_array_elements(metadata->'unlisted') x WHERE x<>to_jsonb($2::text)),'[]'::jsonb)) WHERE id=$1 AND COALESCE(metadata->'unlisted' @> to_jsonb($2::text),false)`, out.ComponentID, t.Version)
			if e != nil {
				return Outcome{}, dbErr(e)
			}
			tag = ct.RowsAffected()
		}
		if tag == 0 {
			out.Unchanged = true
			return out, nil
		}
	case ActionDelete:
		if t.AssetKey != "" {
			if err = tx.QueryRow(ctx, `DELETE FROM assets WHERE instance_id=$1 AND version_id=$2 AND asset_key=$3 RETURNING id::text`, p.InstanceID, out.VersionID, t.AssetKey).Scan(&resource); errors.Is(err, pgx.ErrNoRows) {
				return Outcome{}, ErrNotFound
			} else if err != nil {
				return Outcome{}, dbErr(err)
			}
			out.AssetsRemoved = 1
			var remaining bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE version_id=$1)`, out.VersionID).Scan(&remaining); err != nil {
				return Outcome{}, dbErr(err)
			}
			if remaining {
				break
			}
		}
		if err = removeVersion(ctx, tx, p, t, &out); err != nil {
			return Outcome{}, err
		}
	}
	return finishMutation(ctx, tx, p, t, dp, action, resource, out)
}

// finishMutation records the audit event and outbox event in the caller's
// transaction.
func finishMutation(ctx context.Context, tx pgx.Tx, p identity.Principal, t Target, dp DeletePolicy, action Action, resource string, out Outcome) (Outcome, error) {
	detailJSON, err := json.Marshal(map[string]any{"feed_id": t.FeedID, "source_id": out.SourceID, "namespace": t.Namespace, "coordinate": t.Coordinate, "version": t.Version, "asset_key": t.AssetKey, "tag": t.Tag, "policy_mode": dp.Mode, "immutable_releases": dp.ImmutableReleases})
	if err != nil {
		return Outcome{}, ErrInput
	}
	details := string(detailJSON)
	topic := "artifact." + string(action)
	if err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id,details) VALUES($1,$2,$3,$4,$5::jsonb) RETURNING id`, p.InstanceID, p.PrincipalID, topic, resource, details).Scan(&out.AuditID); err != nil {
		return Outcome{}, dbErr(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,$3::jsonb||jsonb_build_object('version_id',$4::text,'assets_removed',$5::bigint,'tags_removed',$6::bigint,'version_removed',$7::boolean))`, out.AuditID, topic, details, out.VersionID, out.AssetsRemoved, out.TagsRemoved, out.VersionRemoved); err != nil {
		return Outcome{}, dbErr(err)
	}
	return out, nil
}

// removeVersion drops the remaining assets, tags and the version row, then the
// component when it has no versions left. Blobs stay for garbage collection.
func removeVersion(ctx context.Context, tx pgx.Tx, p identity.Principal, t Target, out *Outcome) error {
	ct, err := tx.Exec(ctx, `DELETE FROM tags WHERE component_id=$1 AND version_id=$2`, out.ComponentID, out.VersionID)
	if err != nil {
		return dbErr(err)
	}
	out.TagsRemoved = ct.RowsAffected()
	ct, err = tx.Exec(ctx, `DELETE FROM assets WHERE instance_id=$1 AND version_id=$2`, p.InstanceID, out.VersionID)
	if err != nil {
		return dbErr(err)
	}
	out.AssetsRemoved += ct.RowsAffected()
	if _, err = tx.Exec(ctx, `DELETE FROM versions WHERE instance_id=$1 AND id=$2`, p.InstanceID, out.VersionID); err != nil {
		return dbErr(err)
	}
	out.VersionRemoved = true
	// Forget any unlisted marker so a republished version starts listed.
	if _, err = tx.Exec(ctx, `UPDATE components SET metadata=jsonb_set(metadata,'{unlisted}',COALESCE((SELECT jsonb_agg(x) FROM jsonb_array_elements(metadata->'unlisted') x WHERE x<>to_jsonb($2::text)),'[]'::jsonb)) WHERE id=$1 AND COALESCE(metadata->'unlisted' @> to_jsonb($2::text),false)`, out.ComponentID, t.Version); err != nil {
		return dbErr(err)
	}
	ct, err = tx.Exec(ctx, `DELETE FROM components c WHERE c.id=$1 AND NOT EXISTS(SELECT 1 FROM versions WHERE component_id=c.id)`, out.ComponentID)
	if err != nil {
		return dbErr(err)
	}
	out.ComponentRemoved = ct.RowsAffected() == 1
	return nil
}
