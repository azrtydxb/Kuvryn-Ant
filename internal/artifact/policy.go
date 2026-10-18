package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Policies loads only sources attached to the feed. All policy writes lock the
// feed row and increment its revision, invalidating in-flight publication plans.
type Policies struct {
	DB         *pgxpool.Pool
	InstanceID string
}
type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadSources(ctx context.Context, q queryer, instance, feed string) ([]routing.Source, error) {
	rows, err := q.Query(ctx, `SELECT s.id::text,s.kind FROM feed_sources fs JOIN sources s ON s.id=fs.source_id JOIN feeds f ON f.id=fs.feed_id WHERE fs.instance_id=$1 AND fs.feed_id=$2 AND s.format=f.format ORDER BY fs.priority`, instance, feed)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	var result []routing.Source
	for rows.Next() {
		var s routing.Source
		if rows.Scan(&s.ID, &s.Kind) != nil {
			return nil, ErrUnavailable
		}
		result = append(result, s)
		if len(result) > routing.MaxSources {
			return nil, ErrInput
		}
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}
func (p Policies) Load(ctx context.Context, feed string) (*routing.Router, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rollback(tx)
	config := routing.Config{}
	config.Sources, err = loadSources(ctx, tx, p.InstanceID, feed)
	if err != nil {
		return nil, err
	}
	var encoded []byte
	var ownDefault string
	var own []routing.Rule
	err = tx.QueryRow(ctx, "SELECT COALESCE(default_source_id::text,''),rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2", p.InstanceID, feed).Scan(&ownDefault, &encoded)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnavailable
	}
	if err == nil && (len(encoded) > 256*4096 || json.Unmarshal(encoded, &own) != nil) {
		return nil, ErrInput
	}
	// Bound templates are merged under the feed's own policy.
	eff, _, err := effectivePublish(ctx, tx, p.InstanceID, feed, ownDefault, own)
	if err != nil {
		return nil, err
	}
	config = eff.Config(config.Sources)
	router, err := routing.Compile(config)
	if err != nil {
		return nil, err
	}
	if tx.Commit(ctx) != nil {
		return nil, ErrUnavailable
	}
	return router, nil
}

// Set replaces an exact feed revision. Callers can wrap it in a management retry
// contract; an equal configuration is not implicitly treated as an idempotency key.
func (p Policies) Set(ctx context.Context, principal identity.Principal, feed string, expected int64, defaultSource string, rules []routing.Rule) (int64, error) {
	if expected < 1 || principal.InstanceID != p.InstanceID {
		return 0, ErrInput
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	policy, err := (identity.Roles{DB: p.DB}).Resolve(ctx, principal)
	if err != nil {
		return 0, ErrUnavailable
	}
	if !policy.Authorize(principal, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Configure) {
		return 0, ErrDenied
	}
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, ErrUnavailable
	}
	defer rollback(tx)
	revision, err := p.SetTx(ctx, tx, principal, policy, feed, expected, defaultSource, rules)
	if err != nil {
		return 0, err
	}
	if tx.Commit(ctx) != nil {
		return 0, ErrUnavailable
	}
	return revision, nil
}

// SetTx participates in the caller's transaction, including its idempotency
// record. The supplied policy must be resolved by trusted authorization code.
// It never commits; caller rollback also removes revision/audit/outbox changes.
func (p Policies) SetTx(ctx context.Context, tx pgx.Tx, principal identity.Principal, policy identity.Policy, feed string, expected int64, defaultSource string, rules []routing.Rule) (int64, error) {
	if expected < 1 || principal.InstanceID != p.InstanceID {
		return 0, ErrInput
	}
	if !policy.Authorize(principal, identity.Resource{InstanceID: p.InstanceID, FeedID: feed, Kind: identity.FeedResource}, identity.Configure) {
		return 0, ErrDenied
	}
	var revision int64
	err := tx.QueryRow(ctx, "SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE", p.InstanceID, feed).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, ErrUnavailable
	}
	if revision != expected {
		return 0, ErrConflict
	}
	sources, err := loadSources(ctx, tx, p.InstanceID, feed)
	if err != nil {
		return 0, err
	}
	attached := make(map[string]bool, len(sources))
	for _, source := range sources {
		attached[source.ID] = true
	}
	used := map[string]bool{}
	if defaultSource != "" {
		used[defaultSource] = true
	}
	for _, rule := range rules {
		if rule.Target != "" {
			used[rule.Target] = true
		}
	}
	for id := range used {
		if !policy.Authorize(principal, identity.Resource{InstanceID: p.InstanceID, SourceID: id, Kind: identity.SourceResource}, identity.Configure) {
			return 0, ErrDenied
		}
		if !attached[id] {
			return 0, ErrNotFound
		}
	}
	// The configuration that will apply is the bound templates plus these rules.
	eff, _, err := effectivePublish(ctx, tx, p.InstanceID, feed, defaultSource, rules)
	if err != nil {
		return 0, err
	}
	if _, err = routing.Compile(eff.Config(sources)); err != nil {
		return 0, err
	}
	if rules == nil {
		rules = []routing.Rule{}
	}
	encoded, err := json.Marshal(rules)
	if err != nil || len(encoded) > 256*4096 {
		return 0, ErrInput
	}
	_, err = tx.Exec(ctx, `INSERT INTO publish_policies(instance_id,feed_id,default_source_id,rules) VALUES($1,$2,NULLIF($3,'')::uuid,$4) ON CONFLICT(feed_id) DO UPDATE SET default_source_id=excluded.default_source_id,rules=excluded.rules`, p.InstanceID, feed, defaultSource, encoded)
	if err != nil {
		return 0, ErrUnavailable
	}
	err = tx.QueryRow(ctx, "UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING revision", p.InstanceID, feed).Scan(&revision)
	if err != nil {
		return 0, ErrUnavailable
	}
	var audit int64
	err = tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'feed.publish_policy',$3) RETURNING id`, p.InstanceID, principal.PrincipalID, feed).Scan(&audit)
	if err != nil {
		return 0, ErrUnavailable
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,'feed.publish_policy',jsonb_build_object('feed_id',$2::text,'revision',$3::bigint))`, audit, feed, revision)
	if err != nil {
		return 0, ErrUnavailable
	}
	return revision, nil
}

// PolicySnapshot binds configuration to the same feed revision used by uploads.
// Callers must authorize configuration access before returning these selectors.
type PolicySnapshot struct {
	FeedID        string
	Revision      int64
	DefaultSource string
	Rules         []routing.Rule
}

func (p Policies) Snapshot(ctx context.Context, feed string) (PolicySnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PolicySnapshot{}, ErrUnavailable
	}
	defer rollback(tx)
	snapshot := PolicySnapshot{FeedID: feed, Rules: []routing.Rule{}}
	err = tx.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, feed).Scan(&snapshot.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicySnapshot{}, ErrNotFound
	}
	if err != nil {
		return PolicySnapshot{}, ErrUnavailable
	}
	var encoded []byte
	err = tx.QueryRow(ctx, `SELECT COALESCE(default_source_id::text,''),rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2`, p.InstanceID, feed).Scan(&snapshot.DefaultSource, &encoded)
	if !errors.Is(err, pgx.ErrNoRows) {
		if err != nil {
			return PolicySnapshot{}, ErrUnavailable
		}
		if len(encoded) > 256*4096 || json.Unmarshal(encoded, &snapshot.Rules) != nil {
			return PolicySnapshot{}, ErrInput
		}
	}
	if tx.Commit(ctx) != nil {
		return PolicySnapshot{}, ErrUnavailable
	}
	return snapshot, nil
}
