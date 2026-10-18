// Package policytemplate resolves reusable read/publish policy templates into
// the effective configuration of a feed and rejects template cycles. Merging is
// pure; loading reads templates through a narrow query interface so callers can
// use their own transaction.
package policytemplate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

// MaxDepth bounds an extends chain.
const MaxDepth = 8

// Template kinds.
const (
	Publish = "publish"
	Read    = "read"
)

// ErrCycle reports a template extends cycle; ErrDepth a chain over MaxDepth.
var (
	ErrCycle = errors.New("policy template cycle")
	ErrDepth = errors.New("policy template chain too deep")
	ErrKind  = errors.New("policy template kind mismatch")
)

// PublishBody is the body of a publish template.
type PublishBody struct {
	DefaultSource string         `json:"default_source_id"`
	Rules         []routing.Rule `json:"rules"`
}

// ReadBody is the body of a read template; nil means inherit/unset.
type ReadBody struct {
	LocalFirst              *bool `json:"local_first,omitempty"`
	FallbackOnNotFound      *bool `json:"fallback_on_not_found,omitempty"`
	FailoverOnUpstreamError *bool `json:"failover_on_upstream_error,omitempty"`
}

// Template is a stored template. Body is decoded according to Kind.
type Template struct {
	ID, Name, Kind, ExtendsID string
	Revision                  int64
	Body                      json.RawMessage
}

// OriginRule is an effective publish rule and where it came from: the name of
// the template, or "feed" for the feed's own policy.
type OriginRule struct {
	Rule   routing.Rule
	Origin string
}

// EffectivePublish is a feed's merged publish policy.
type EffectivePublish struct {
	DefaultSource string
	DefaultOrigin string
	Rules         []OriginRule
}

// MergePublish applies chain (root first) and then the feed's own policy.
// Rules are keyed by ID: a later layer replaces an earlier rule with the same
// ID in place; a non-empty default replaces the inherited default.
func MergePublish(chain []Template, ownDefault string, ownRules []routing.Rule) (EffectivePublish, error) {
	var out EffectivePublish
	index := map[string]int{}
	apply := func(origin, def string, rules []routing.Rule) {
		if def != "" {
			out.DefaultSource, out.DefaultOrigin = def, origin
		}
		for _, r := range rules {
			if i, ok := index[r.ID]; ok {
				out.Rules[i] = OriginRule{r, origin}
				continue
			}
			index[r.ID] = len(out.Rules)
			out.Rules = append(out.Rules, OriginRule{r, origin})
		}
	}
	for _, t := range chain {
		var b PublishBody
		if len(t.Body) > 0 {
			if err := json.Unmarshal(t.Body, &b); err != nil {
				return out, fmt.Errorf("template %s: %w", t.Name, err)
			}
		}
		apply("template:"+t.Name, b.DefaultSource, b.Rules)
	}
	apply("feed", ownDefault, ownRules)
	return out, nil
}

// Config converts the effective policy to a routing configuration.
func (e EffectivePublish) Config(sources []routing.Source) routing.Config {
	cfg := routing.Config{Sources: sources, DefaultTarget: e.DefaultSource}
	for _, r := range e.Rules {
		cfg.Rules = append(cfg.Rules, r.Rule)
	}
	return cfg
}

// EffectiveRead is a feed's merged read policy with the origin of each value.
type EffectiveRead struct {
	ReadBody
	Origins map[string]string
}

// MergeRead merges read templates root first; later layers override set fields.
func MergeRead(chain []Template) (EffectiveRead, error) {
	out := EffectiveRead{Origins: map[string]string{}}
	for _, t := range chain {
		var b ReadBody
		if len(t.Body) > 0 {
			if err := json.Unmarshal(t.Body, &b); err != nil {
				return out, fmt.Errorf("template %s: %w", t.Name, err)
			}
		}
		origin := "template:" + t.Name
		if b.LocalFirst != nil {
			out.LocalFirst, out.Origins["local_first"] = b.LocalFirst, origin
		}
		if b.FallbackOnNotFound != nil {
			out.FallbackOnNotFound, out.Origins["fallback_on_not_found"] = b.FallbackOnNotFound, origin
		}
		if b.FailoverOnUpstreamError != nil {
			out.FailoverOnUpstreamError, out.Origins["failover_on_upstream_error"] = b.FailoverOnUpstreamError, origin
		}
	}
	return out, nil
}

// Queryer is satisfied by pgx.Tx and *pgxpool.Pool.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadOne(ctx context.Context, q Queryer, instance, id string) (Template, error) {
	var t Template
	var ext *string
	err := q.QueryRow(ctx, `SELECT id::text,name,kind,extends_id::text,revision,body FROM policy_templates WHERE instance_id=$1 AND id=$2`, instance, id).Scan(&t.ID, &t.Name, &t.Kind, &ext, &t.Revision, &t.Body)
	if ext != nil {
		t.ExtendsID = *ext
	}
	return t, err
}

// LoadChain returns the template and its ancestors, root first. A cycle or a
// chain longer than MaxDepth is an error, so a corrupted store cannot hang
// publishing.
func LoadChain(ctx context.Context, q Queryer, instance, id string) ([]Template, error) {
	var chain []Template
	seen := map[string]bool{}
	for cur := id; cur != ""; {
		if seen[cur] {
			return nil, ErrCycle
		}
		if len(chain) >= MaxDepth {
			return nil, ErrDepth
		}
		seen[cur] = true
		t, err := loadOne(ctx, q, instance, cur)
		if err != nil {
			return nil, err
		}
		chain = append([]Template{t}, chain...)
		cur = t.ExtendsID
	}
	return chain, nil
}

// CheckExtends verifies that making id extend parent keeps the hierarchy
// acyclic and of one kind, and returns the number of ancestors parent brings
// (parent included). id may be empty for a template that does not exist yet (it
// cannot be anyone's ancestor). Callers add Depth(id) to bound the total chain.
func CheckExtends(ctx context.Context, q Queryer, instance, id, parent, kind string) (int, error) {
	if parent == "" {
		return 0, nil
	}
	if parent == id {
		return 0, fmt.Errorf("%w: a template cannot extend itself", ErrCycle)
	}
	seen := map[string]bool{}
	count := 0
	for cur := parent; cur != ""; {
		if cur == id {
			return 0, fmt.Errorf("%w: %s is already an ancestor of the new parent", ErrCycle, id)
		}
		if seen[cur] {
			return 0, fmt.Errorf("%w: existing loop at %s", ErrCycle, cur)
		}
		if count >= MaxDepth {
			return 0, ErrDepth
		}
		seen[cur] = true
		t, err := loadOne(ctx, q, instance, cur)
		if err != nil {
			return 0, err
		}
		if t.Kind != kind {
			return 0, ErrKind
		}
		count++
		cur = t.ExtendsID
	}
	return count, nil
}

// Depth returns the length of the longest chain starting at id and going down
// through the templates that extend it (id itself counts as 1).
func Depth(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, instance, id string) (int, error) {
	var depth int
	err := q.QueryRow(ctx, `WITH RECURSIVE d(id,n) AS (SELECT $2::uuid,1 UNION SELECT t.id,d.n+1 FROM policy_templates t JOIN d ON t.extends_id=d.id AND t.instance_id=$1 WHERE d.n<=$3) SELECT max(n) FROM d`, instance, id, MaxDepth+1).Scan(&depth)
	return depth, err
}
