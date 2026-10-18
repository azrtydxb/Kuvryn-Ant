package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/database"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/policytemplate"
	"github.com/azrtydxb/Kuvryn-Ant/internal/routing"
	"github.com/jackc/pgx/v5"
)

var templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// PolicyTemplate is a reusable read or publish policy fragment.
type PolicyTemplate struct {
	ID          string                   `json:"id"`
	Revision    int64                    `json:"revision"`
	BoundFeeds  int64                    `json:"bound_feeds"`
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Kind        string                   `json:"kind"`
	ExtendsID   string                   `json:"extends_id,omitempty"`
	Publish     *PublishPolicyInput      `json:"publish,omitempty"`
	Read        *policytemplate.ReadBody `json:"read,omitempty"`
}

// FeedPolicyBinding lists the templates a feed references.
type FeedPolicyBinding struct {
	FeedID            string `json:"feed_id"`
	Revision          int64  `json:"revision"`
	PublishTemplateID string `json:"publish_template_id,omitempty"`
	ReadTemplateID    string `json:"read_template_id,omitempty"`
}

type templateInput struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Kind        string                   `json:"kind"`
	ExtendsID   string                   `json:"extends_id"`
	Publish     json.RawMessage          `json:"publish"`
	Read        *policytemplate.ReadBody `json:"read"`
}

type parsedTemplate struct {
	in      templateInput
	body    json.RawMessage
	targets []string
}

func ruleDTOs(rules []routing.Rule) []PublishRule {
	out := make([]PublishRule, 0, len(rules))
	for _, rule := range rules {
		out = append(out, PublishRule{ID: rule.ID, Priority: int64(rule.Priority), TargetSourceID: rule.Target, Deny: rule.Deny, Selector: PublishSelector{Namespace: rule.Selector.Namespace, Package: rule.Selector.Package, PathPrefix: rule.Selector.PathPrefix, PrincipalID: rule.Selector.Principal}})
	}
	return out
}

// describeConfigErr turns a routing/template failure into a client-safe sentence.
func describeConfigErr(err error) string {
	var config *routing.ConfigError
	if errors.As(err, &config) {
		s := config.Code
		if config.RuleID != "" {
			s += " (rule " + config.RuleID
			if config.OtherRuleID != "" {
				s += " and rule " + config.OtherRuleID
			}
			s += ")"
		}
		return s
	}
	return "invalid"
}

func (h opsHandlers) templateAdmin(w http.ResponseWriter, r *http.Request) (identity.Principal, identity.Policy, bool) {
	p, policy, ok := h.policy(w, r)
	if !ok {
		return p, policy, false
	}
	if !feedAllowed(policy, p, "", identity.Configure) {
		Error(w, r, 403, api.ErrorCodeForbidden, "policy template configuration denied")
		return p, policy, false
	}
	return p, policy, true
}

func (h opsHandlers) parseTemplate(w http.ResponseWriter, r *http.Request, policy identity.Policy, p identity.Principal) (parsedTemplate, bool) {
	var in templateInput
	if !sourceJSON(w, r, &in, "name", "description", "kind", "extends_id", "publish", "read") {
		return parsedTemplate{}, false
	}
	bad := func(msg string) (parsedTemplate, bool) {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, msg)
		return parsedTemplate{}, false
	}
	if !templateName.MatchString(in.Name) {
		return bad("name must match [A-Za-z0-9][A-Za-z0-9_.-]{0,62}")
	}
	if len(in.Description) > 1024 || !utf8.ValidString(in.Description) {
		return bad("description too long")
	}
	if in.Kind != policytemplate.Publish && in.Kind != policytemplate.Read {
		return bad("kind must be publish or read")
	}
	if in.ExtendsID != "" {
		id, valid := feedID(in.ExtendsID)
		if !valid {
			return bad("extends_id must be a UUID")
		}
		in.ExtendsID = id
	}
	out := parsedTemplate{in: in}
	switch in.Kind {
	case policytemplate.Publish:
		if len(in.Publish) == 0 || in.Read != nil {
			return bad("a publish template needs publish and no read settings")
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(in.Publish, &raw) != nil {
			return bad("invalid publish body")
		}
		parsed, rules, err := parsePublishPolicy(raw)
		if err != nil {
			return bad("invalid publish body: check rule ids, selectors, targets and ambiguity")
		}
		if rules == nil {
			rules = []routing.Rule{}
		}
		out.body, _ = json.Marshal(policytemplate.PublishBody{DefaultSource: parsed.DefaultSourceID, Rules: rules})
		if parsed.DefaultSourceID != "" {
			out.targets = append(out.targets, parsed.DefaultSourceID)
		}
		for _, rule := range rules {
			if rule.Target != "" {
				out.targets = append(out.targets, rule.Target)
			}
		}
	default:
		if len(in.Publish) != 0 || in.Read == nil {
			return bad("a read template needs read settings and no publish body")
		}
		out.body, _ = json.Marshal(in.Read)
	}
	for _, id := range out.targets {
		if !sourceAllowed(policy, p, id, identity.Configure) {
			Error(w, r, 403, api.ErrorCodeForbidden, "configuration of a referenced source is denied")
			return parsedTemplate{}, false
		}
	}
	return out, true
}

const templateColumns = `t.id::text,t.name,t.description,t.kind,COALESCE(t.extends_id::text,''),t.revision,t.body,
 (SELECT count(*) FROM feed_policy_bindings b WHERE b.publish_template_id=t.id OR b.read_template_id=t.id)`

type rowScanner interface{ Scan(...any) error }

func scanTemplate(row rowScanner) (PolicyTemplate, error) {
	var t PolicyTemplate
	var body []byte
	if err := row.Scan(&t.ID, &t.Name, &t.Description, &t.Kind, &t.ExtendsID, &t.Revision, &body, &t.BoundFeeds); err != nil {
		return t, err
	}
	if t.Kind == policytemplate.Publish {
		var b policytemplate.PublishBody
		if err := json.Unmarshal(body, &b); err != nil {
			return t, err
		}
		t.Publish = &PublishPolicyInput{DefaultSourceID: b.DefaultSource, Rules: ruleDTOs(b.Rules)}
	} else {
		var b policytemplate.ReadBody
		if err := json.Unmarshal(body, &b); err != nil {
			return t, err
		}
		t.Read = &b
	}
	return t, nil
}

// extendsProblem maps loader failures to a specific rejection.
func extendsProblem(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidConfig("extends_id does not name an existing template")
	}
	return err
}

func (h opsHandlers) listTemplates(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.templateAdmin(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	rows, err := h.db.Query(ctx, `SELECT `+templateColumns+` FROM policy_templates t WHERE t.instance_id=$1 ORDER BY t.name LIMIT 1000`, p.InstanceID)
	if err != nil {
		identityError(w, r, err)
		return
	}
	defer rows.Close()
	out := struct {
		Items []PolicyTemplate `json:"items"`
	}{Items: []PolicyTemplate{}}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			identityError(w, r, err)
			return
		}
		out.Items = append(out.Items, t)
	}
	if rows.Err() != nil {
		identityError(w, r, rows.Err())
		return
	}
	JSON(w, 200, out)
}

func (h opsHandlers) getTemplate(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.templateAdmin(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid template ID")
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	t, err := scanTemplate(h.db.QueryRow(ctx, `SELECT `+templateColumns+` FROM policy_templates t WHERE t.instance_id=$1 AND t.id=$2`, p.InstanceID, id))
	if err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(t.Revision, 10)))
	JSON(w, 200, t)
}

func (h opsHandlers) createTemplate(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.templateAdmin(w, r)
	if !ok {
		return
	}
	in, ok := h.parseTemplate(w, r, policy, p)
	if !ok {
		return
	}
	h.mutateCfg(w, r, p, "policy_template.create", "templates", in.in, 201, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM policy_templates WHERE instance_id=$1 AND name=$2)`, p.InstanceID, in.in.Name).Scan(&taken); err != nil {
			return nil, err
		}
		if taken {
			return nil, conflictConfig("a template with this name already exists")
		}
		ancestors, err := policytemplate.CheckExtends(ctx, tx, p.InstanceID, "", in.in.ExtendsID, in.in.Kind)
		if err != nil {
			return nil, extendsProblem(err)
		}
		if ancestors+1 > policytemplate.MaxDepth {
			return nil, policytemplate.ErrDepth
		}
		var id string
		if err = tx.QueryRow(ctx, `INSERT INTO policy_templates(instance_id,name,description,kind,extends_id,body) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6) RETURNING id::text`, p.InstanceID, in.in.Name, in.in.Description, in.in.Kind, in.in.ExtendsID, in.body).Scan(&id); err != nil {
			return nil, err
		}
		t, err := scanTemplate(tx.QueryRow(ctx, `SELECT `+templateColumns+` FROM policy_templates t WHERE t.id=$1`, id))
		if err != nil {
			return nil, err
		}
		return t, audited(ctx, tx, p, id, "policy_template.create", t.Revision, map[string]string{"name": t.Name, "kind": t.Kind, "extends_id": t.ExtendsID})
	})
}

// revalidateFeeds rechecks every feed that resolves through a template (bound
// to it or to a template that extends it) and bumps their revisions, so a
// template edit cannot leave any feed with an invalid effective policy.
func (h opsHandlers) revalidateFeeds(ctx context.Context, tx pgx.Tx, instance, template string) ([]string, error) {
	rows, err := tx.Query(ctx, `WITH RECURSIVE d(id) AS (SELECT $2::uuid UNION SELECT t.id FROM policy_templates t JOIN d ON t.extends_id=d.id WHERE t.instance_id=$1)
 SELECT DISTINCT b.feed_id::text FROM feed_policy_bindings b WHERE b.instance_id=$1 AND (b.publish_template_id IN (SELECT id FROM d) OR b.read_template_id IN (SELECT id FROM d)) ORDER BY 1`, instance, template)
	if err != nil {
		return nil, err
	}
	var feeds []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		feeds = append(feeds, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	policies := artifact.Policies{DB: h.db, InstanceID: instance}
	for _, feed := range feeds {
		// Same lock order as every other feed policy write.
		var slug string
		if err = tx.QueryRow(ctx, `SELECT slug FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, instance, feed).Scan(&slug); err != nil {
			return nil, err
		}
		if err = policies.ValidateTx(ctx, tx, feed); err != nil {
			var config *routing.ConfigError
			if errors.As(err, &config) || errors.Is(err, artifact.ErrInput) {
				return nil, invalidConfig(fmt.Sprintf("feed %q would have an invalid effective publish policy: %s", slug, describeConfigErr(err)))
			}
			return nil, err
		}
		if _, err = tx.Exec(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, instance, feed); err != nil {
			return nil, err
		}
	}
	return feeds, nil
}

func (h opsHandlers) putTemplate(w http.ResponseWriter, r *http.Request) {
	p, policy, ok := h.templateAdmin(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid template ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	in, ok := h.parseTemplate(w, r, policy, p)
	if !ok {
		return
	}
	h.mutateCfg(w, r, p, "policy_template.put", id, struct {
		In       templateInput
		Revision int64
	}{in.in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var kind string
		var current int64
		if err := tx.QueryRow(ctx, `SELECT kind,revision FROM policy_templates WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id).Scan(&kind, &current); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		if kind != in.in.Kind {
			return nil, invalidConfig("the kind of a template cannot change")
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM policy_templates WHERE instance_id=$1 AND name=$2 AND id<>$3)`, p.InstanceID, in.in.Name, id).Scan(&taken); err != nil {
			return nil, err
		}
		if taken {
			return nil, conflictConfig("a template with this name already exists")
		}
		ancestors, err := policytemplate.CheckExtends(ctx, tx, p.InstanceID, id, in.in.ExtendsID, kind)
		if err != nil {
			return nil, extendsProblem(err)
		}
		below, err := policytemplate.Depth(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		if ancestors+below > policytemplate.MaxDepth {
			return nil, policytemplate.ErrDepth
		}
		if _, err = tx.Exec(ctx, `UPDATE policy_templates SET name=$3,description=$4,extends_id=NULLIF($5,'')::uuid,body=$6,revision=revision+1,updated_at=now() WHERE instance_id=$1 AND id=$2`, p.InstanceID, id, in.in.Name, in.in.Description, in.in.ExtendsID, in.body); err != nil {
			return nil, err
		}
		feeds, err := h.revalidateFeeds(ctx, tx, p.InstanceID, id)
		if err != nil {
			return nil, err
		}
		t, err := scanTemplate(tx.QueryRow(ctx, `SELECT `+templateColumns+` FROM policy_templates t WHERE t.id=$1`, id))
		if err != nil {
			return nil, err
		}
		sort.Strings(feeds)
		return t, audited(ctx, tx, p, id, "policy_template.put", t.Revision, map[string]any{"name": t.Name, "kind": t.Kind, "extends_id": t.ExtendsID, "revalidated_feeds": feeds})
	})
}

func (h opsHandlers) deleteTemplate(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.templateAdmin(w, r)
	if !ok {
		return
	}
	id, valid := feedID(r.PathValue("id"))
	if !valid {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "invalid template ID")
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match revision required")
		return
	}
	h.mutateCfg(w, r, p, "policy_template.delete", id, revision, http.StatusNoContent, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var current int64
		var name string
		if err := tx.QueryRow(ctx, `SELECT revision,name FROM policy_templates WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, id).Scan(&current, &name); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		var feeds, children int64
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM feed_policy_bindings WHERE instance_id=$1 AND (publish_template_id=$2 OR read_template_id=$2)),(SELECT count(*) FROM policy_templates WHERE instance_id=$1 AND extends_id=$2)`, p.InstanceID, id).Scan(&feeds, &children); err != nil {
			return nil, err
		}
		if feeds+children > 0 {
			return nil, conflictConfig(fmt.Sprintf("template is still referenced by %d feed(s) and %d template(s)", feeds, children))
		}
		if _, err := tx.Exec(ctx, `DELETE FROM policy_templates WHERE instance_id=$1 AND id=$2`, p.InstanceID, id); err != nil {
			return nil, err
		}
		return map[string]bool{"deleted": true}, audited(ctx, tx, p, id, "policy_template.delete", current, map[string]string{"name": name})
	})
}

func (h opsHandlers) loadBinding(ctx context.Context, instance, feed string) (FeedPolicyBinding, error) {
	b := FeedPolicyBinding{FeedID: feed}
	var pub, rd *string
	if err := h.db.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2`, instance, feed).Scan(&b.Revision); err != nil {
		return b, err
	}
	err := h.db.QueryRow(ctx, `SELECT publish_template_id::text,read_template_id::text FROM feed_policy_bindings WHERE instance_id=$1 AND feed_id=$2`, instance, feed).Scan(&pub, &rd)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return b, err
	}
	if pub != nil {
		b.PublishTemplateID = *pub
	}
	if rd != nil {
		b.ReadTemplateID = *rd
	}
	return b, nil
}

func (h opsHandlers) getBinding(w http.ResponseWriter, r *http.Request) {
	p, feed, ok := h.feedConfigure(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	b, err := h.loadBinding(ctx, p.InstanceID, feed)
	if err != nil {
		identityError(w, r, identityDBError(err))
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(b.Revision, 10)))
	JSON(w, 200, b)
}

// feedConfigure authorizes configuration access to the feed in the path; a
// caller without it sees 404 so feed existence is not disclosed.
func (h opsHandlers) feedConfigure(w http.ResponseWriter, r *http.Request) (identity.Principal, string, bool) {
	return h.feedRead(w, r, identity.Configure)
}

func (h opsHandlers) putBinding(w http.ResponseWriter, r *http.Request) {
	p, feed, ok := h.feedConfigure(w, r)
	if !ok {
		return
	}
	revision, ok := revisionHeader(r)
	if !ok {
		Error(w, r, 428, api.ErrorCodePreconditionFailed, "If-Match feed revision required")
		return
	}
	var in struct {
		PublishTemplateID string `json:"publish_template_id"`
		ReadTemplateID    string `json:"read_template_id"`
	}
	if !sourceJSON(w, r, &in, "publish_template_id", "read_template_id") {
		return
	}
	for _, v := range []*string{&in.PublishTemplateID, &in.ReadTemplateID} {
		if *v == "" {
			continue
		}
		id, valid := feedID(*v)
		if !valid {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "template IDs must be UUIDs")
			return
		}
		*v = id
	}
	_, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	h.mutateCfg(w, r, p, "feed.policy_templates", feed, struct {
		In       any
		Revision int64
	}{in, revision}, 200, func(ctx context.Context, tx pgx.Tx) (any, error) {
		var current int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM feeds WHERE instance_id=$1 AND id=$2 FOR UPDATE`, p.InstanceID, feed).Scan(&current); err != nil {
			return nil, err
		}
		if current != revision {
			return nil, database.ErrRevisionConflict
		}
		for _, want := range []struct{ id, kind, field string }{{in.PublishTemplateID, policytemplate.Publish, "publish_template_id"}, {in.ReadTemplateID, policytemplate.Read, "read_template_id"}} {
			if want.id == "" {
				continue
			}
			var kind string
			err := tx.QueryRow(ctx, `SELECT kind FROM policy_templates WHERE instance_id=$1 AND id=$2`, p.InstanceID, want.id).Scan(&kind)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, invalidConfig(want.field + " does not name an existing template")
			}
			if err != nil {
				return nil, err
			}
			if kind != want.kind {
				return nil, invalidConfig(want.field + " must name a " + want.kind + " template")
			}
		}
		if in.PublishTemplateID != "" {
			// Binding must not let a feed administrator route to sources they cannot configure.
			chain, err := policytemplate.LoadChain(ctx, tx, p.InstanceID, in.PublishTemplateID)
			if err != nil {
				return nil, err
			}
			eff, err := policytemplate.MergePublish(chain, "", nil)
			if err != nil {
				return nil, err
			}
			if eff.DefaultSource != "" && !sourceAllowed(policy, p, eff.DefaultSource, identity.Configure) {
				return nil, &cfgError{http.StatusForbidden, api.ErrorCodeForbidden, "configuration of a source referenced by the template is denied"}
			}
			for _, rule := range eff.Rules {
				if rule.Rule.Target != "" && !sourceAllowed(policy, p, rule.Rule.Target, identity.Configure) {
					return nil, &cfgError{http.StatusForbidden, api.ErrorCodeForbidden, "configuration of a source referenced by the template is denied"}
				}
			}
		}
		if in.PublishTemplateID == "" && in.ReadTemplateID == "" {
			if _, err := tx.Exec(ctx, `DELETE FROM feed_policy_bindings WHERE instance_id=$1 AND feed_id=$2`, p.InstanceID, feed); err != nil {
				return nil, err
			}
		} else if _, err := tx.Exec(ctx, `INSERT INTO feed_policy_bindings(instance_id,feed_id,publish_template_id,read_template_id) VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid) ON CONFLICT(feed_id) DO UPDATE SET publish_template_id=excluded.publish_template_id,read_template_id=excluded.read_template_id`, p.InstanceID, feed, in.PublishTemplateID, in.ReadTemplateID); err != nil {
			return nil, err
		}
		if err := (artifact.Policies{DB: h.db, InstanceID: p.InstanceID}).ValidateTx(ctx, tx, feed); err != nil {
			var config *routing.ConfigError
			if errors.As(err, &config) || errors.Is(err, artifact.ErrInput) {
				return nil, invalidConfig("the feed's effective publish policy would be invalid: " + describeConfigErr(err) + "; check that every source the template targets is attached to this feed")
			}
			return nil, err
		}
		out := FeedPolicyBinding{FeedID: feed, PublishTemplateID: in.PublishTemplateID, ReadTemplateID: in.ReadTemplateID}
		if err := tx.QueryRow(ctx, `UPDATE feeds SET revision=revision+1 WHERE instance_id=$1 AND id=$2 RETURNING revision`, p.InstanceID, feed).Scan(&out.Revision); err != nil {
			return nil, err
		}
		return out, audited(ctx, tx, p, feed, "feed.policy_templates", out.Revision, map[string]string{"publish_template_id": in.PublishTemplateID, "read_template_id": in.ReadTemplateID})
	})
}

// EffectivePolicy is the merged configuration of a feed with value origins.
type EffectivePolicy struct {
	FeedID               string                  `json:"feed_id"`
	Revision             int64                   `json:"revision"`
	PublishTemplateChain []string                `json:"publish_template_chain"`
	ReadTemplateChain    []string                `json:"read_template_chain"`
	DefaultSourceID      string                  `json:"default_source_id"`
	DefaultOrigin        string                  `json:"default_origin"`
	Rules                []EffectivePublishRule  `json:"rules"`
	Read                 policytemplate.ReadBody `json:"read"`
	ReadOrigins          map[string]string       `json:"read_origins"`
}

// EffectivePublishRule is a publish rule with the layer that defined it.
type EffectivePublishRule struct {
	PublishRule
	Origin string `json:"origin"`
}

func (h opsHandlers) effectivePolicy(w http.ResponseWriter, r *http.Request) {
	p, feed, ok := h.feedConfigure(w, r)
	if !ok {
		return
	}
	_, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	eff, err := (artifact.Policies{DB: h.db, InstanceID: p.InstanceID}).Effective(r.Context(), feed)
	if err != nil {
		policyError(w, r, err)
		return
	}
	var rules []routing.Rule
	for _, rule := range eff.Publish.Rules {
		rules = append(rules, rule.Rule)
	}
	if !policyAllowed(policy, p, feed, eff.Publish.DefaultSource, rules) {
		policyError(w, r, artifact.ErrDenied)
		return
	}
	out := EffectivePolicy{FeedID: feed, Revision: eff.Revision, PublishTemplateChain: nonNil(eff.PublishChain), ReadTemplateChain: nonNil(eff.ReadChain), DefaultSourceID: eff.Publish.DefaultSource, DefaultOrigin: eff.Publish.DefaultOrigin, Rules: []EffectivePublishRule{}, Read: eff.Read.ReadBody, ReadOrigins: eff.Read.Origins}
	for _, rule := range eff.Publish.Rules {
		out.Rules = append(out.Rules, EffectivePublishRule{PublishRule: ruleDTOs([]routing.Rule{rule.Rule})[0], Origin: rule.Origin})
	}
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(out.Revision, 10)))
	JSON(w, 200, out)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
