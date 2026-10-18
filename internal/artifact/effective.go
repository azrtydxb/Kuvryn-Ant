package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/policytemplate"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

// bindings returns the template IDs a feed references; empty means unbound.
func bindings(ctx context.Context, q queryer, instance, feed string) (publish, read string, err error) {
	var pub, rd *string
	err = q.QueryRow(ctx, `SELECT publish_template_id::text,read_template_id::text FROM feed_policy_bindings WHERE instance_id=$1 AND feed_id=$2`, instance, feed).Scan(&pub, &rd)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", ErrUnavailable
	}
	if pub != nil {
		publish = *pub
	}
	if rd != nil {
		read = *rd
	}
	return publish, read, nil
}

// effectivePublish merges the feed's bound publish template chain with its own
// policy. A corrupt or looping chain is ErrInput, never a hang.
func effectivePublish(ctx context.Context, q queryer, instance, feed, ownDefault string, own []routing.Rule) (policytemplate.EffectivePublish, []policytemplate.Template, error) {
	pub, _, err := bindings(ctx, q, instance, feed)
	if err != nil {
		return policytemplate.EffectivePublish{}, nil, err
	}
	var chain []policytemplate.Template
	if pub != "" {
		if chain, err = policytemplate.LoadChain(ctx, q, instance, pub); err != nil {
			return policytemplate.EffectivePublish{}, nil, ErrInput
		}
	}
	eff, err := policytemplate.MergePublish(chain, ownDefault, own)
	if err != nil {
		return eff, chain, ErrInput
	}
	return eff, chain, nil
}

// Effective is the merged, inspectable configuration of a feed.
type Effective struct {
	FeedID       string
	Revision     int64
	PublishChain []string
	ReadChain    []string
	Publish      policytemplate.EffectivePublish
	Read         policytemplate.EffectiveRead
}

// Effective resolves the templates bound to a feed and the feed's own publish
// policy in one consistent snapshot. Callers authorize configuration access.
func (p Policies) Effective(ctx context.Context, feed string) (Effective, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := p.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Effective{}, ErrUnavailable
	}
	defer rollback(tx)
	out := Effective{FeedID: feed}
	err = tx.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2`, p.InstanceID, feed).Scan(&out.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Effective{}, ErrNotFound
	}
	if err != nil {
		return Effective{}, ErrUnavailable
	}
	var ownDefault string
	var encoded []byte
	var own []routing.Rule
	err = tx.QueryRow(ctx, `SELECT COALESCE(default_source_id::text,''),rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2`, p.InstanceID, feed).Scan(&ownDefault, &encoded)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Effective{}, ErrUnavailable
	}
	if err == nil && (len(encoded) > 256*4096 || json.Unmarshal(encoded, &own) != nil) {
		return Effective{}, ErrInput
	}
	var chain []policytemplate.Template
	out.Publish, chain, err = effectivePublish(ctx, tx, p.InstanceID, feed, ownDefault, own)
	if err != nil {
		return Effective{}, err
	}
	for _, t := range chain {
		out.PublishChain = append(out.PublishChain, t.Name)
	}
	_, read, err := bindings(ctx, tx, p.InstanceID, feed)
	if err != nil {
		return Effective{}, err
	}
	var readChain []policytemplate.Template
	if read != "" {
		if readChain, err = policytemplate.LoadChain(ctx, tx, p.InstanceID, read); err != nil {
			return Effective{}, ErrInput
		}
	}
	for _, t := range readChain {
		out.ReadChain = append(out.ReadChain, t.Name)
	}
	if out.Read, err = policytemplate.MergeRead(readChain); err != nil {
		return Effective{}, ErrInput
	}
	return out, nil
}

// ValidateTx compiles the feed's effective publish policy inside the caller's
// transaction (which must already include the proposed binding or template
// change). It rejects configuration whose effective rules target unattached or
// non-local sources, or are ambiguous.
func (p Policies) ValidateTx(ctx context.Context, tx pgx.Tx, feed string) error {
	sources, err := loadSources(ctx, tx, p.InstanceID, feed)
	if err != nil {
		return err
	}
	var ownDefault string
	var encoded []byte
	var own []routing.Rule
	err = tx.QueryRow(ctx, `SELECT COALESCE(default_source_id::text,''),rules FROM publish_policies WHERE instance_id=$1 AND feed_id=$2`, p.InstanceID, feed).Scan(&ownDefault, &encoded)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ErrUnavailable
	}
	if err == nil && json.Unmarshal(encoded, &own) != nil {
		return ErrInput
	}
	eff, _, err := effectivePublish(ctx, tx, p.InstanceID, feed, ownDefault, own)
	if err != nil {
		return err
	}
	_, err = routing.Compile(eff.Config(sources))
	return err
}
