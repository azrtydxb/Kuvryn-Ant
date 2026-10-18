package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/azrtydxb/Kuvryn-Ant/api"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

// EffectiveAction is the outcome for one action. ViaRoles allow the action on
// their own; DeniedByRoles contain a matching deny, which overrides any allow.
type EffectiveAction struct {
	Action        identity.Action `json:"action"`
	Allowed       bool            `json:"allowed"`
	ViaRoles      []string        `json:"via_roles"`
	DeniedByRoles []string        `json:"denied_by_roles"`
}
type EffectiveResource struct {
	Kind      string `json:"kind"`
	FeedID    string `json:"feed_id,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}
type EffectivePermissions struct {
	PrincipalID string            `json:"principal_id"`
	Disabled    bool              `json:"disabled"`
	Resource    EffectiveResource `json:"resource"`
	Actions     []EffectiveAction `json:"actions"`
}

var allActions = []identity.Action{identity.Read, identity.Publish, identity.Delete, identity.Configure, identity.Replicate, identity.Admin}

// parseResource accepts instance, feed:<uuid>, source:<uuid> or
// artifact:<feed uuid>[:<namespace>] with an optional source query parameter.
func parseResource(raw, source string) (identity.Resource, bool) {
	kind, rest, _ := strings.Cut(raw, ":")
	res := identity.Resource{}
	switch kind {
	case "instance":
		if rest != "" || source != "" {
			return res, false
		}
		res.Kind = identity.InstanceResource
	case "feed":
		id, ok := feedID(rest)
		if !ok || source != "" {
			return res, false
		}
		res.Kind, res.FeedID = identity.FeedResource, id
	case "source":
		id, ok := feedID(rest)
		if !ok || source != "" {
			return res, false
		}
		res.Kind, res.SourceID = identity.SourceResource, id
	case "artifact":
		feed, ns, _ := strings.Cut(rest, ":")
		id, ok := feedID(feed)
		if !ok || len(ns) > 200 {
			return res, false
		}
		res.Kind, res.FeedID, res.Namespace = identity.ArtifactResource, id, ns
		if source != "" {
			if res.SourceID, ok = feedID(source); !ok {
				return res, false
			}
		}
	default:
		return res, false
	}
	return res, true
}

// matchesDeny mirrors identity grant matching for explaining denials.
func matchesDeny(g identity.Grant, res identity.Resource, a identity.Action) bool {
	if g.Effect != identity.Deny || g.Kind != res.Kind || g.InstanceID != res.InstanceID {
		return false
	}
	if (g.SourceID != "" && g.SourceID != res.SourceID) || (g.FeedID != "" && g.FeedID != res.FeedID) || (g.Namespace != "" && g.Namespace != res.Namespace) {
		return false
	}
	for _, c := range g.Actions {
		if c == a {
			return true
		}
	}
	return false
}

// effective serves GET /api/v1/permissions/effective. The result reflects
// role grants of the target principal; machine-token scopes narrow a specific
// token further and are not applied. Instance admins may inspect anyone;
// other callers may inspect themselves.
func (h opsHandlers) effective(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for k, v := range q {
		if (k != "principal" && k != "resource" && k != "source") || len(v) != 1 {
			Error(w, r, 400, api.ErrorCodeInvalidRequest, "unknown or repeated query parameter")
			return
		}
	}
	target, okTarget := feedID(q.Get("principal"))
	res, okRes := parseResource(q.Get("resource"), q.Get("source"))
	if !okTarget || !okRes {
		Error(w, r, 400, api.ErrorCodeInvalidRequest, "principal (uuid) and resource (instance, feed:<id>, source:<id> or artifact:<feed>[:<namespace>]) required")
		return
	}
	p, policy, ok := h.policy(w, r)
	if !ok {
		return
	}
	admin := policy.Authorize(p, identity.Resource{InstanceID: p.InstanceID, Kind: identity.InstanceResource}, identity.Admin)
	if !admin && target != p.PrincipalID {
		Error(w, r, 403, api.ErrorCodeForbidden, "instance administration denied")
		return
	}
	res.InstanceID = p.InstanceID
	ctx, cancel := h.ctx(r)
	defer cancel()
	var disabled bool
	err := h.db.QueryRow(ctx, `SELECT disabled FROM principals WHERE instance_id=$1 AND id=$2`, p.InstanceID, target).Scan(&disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		Error(w, r, 404, api.ErrorCodeNotFound, "principal not found")
		return
	}
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "authorization unavailable")
		return
	}
	subject := identity.Principal{InstanceID: p.InstanceID, PrincipalID: target}
	targetPolicy, err := (identity.Roles{DB: h.db}).Resolve(ctx, subject)
	if err != nil {
		Error(w, r, 503, api.ErrorCodeUnavailable, "authorization unavailable")
		return
	}
	out := EffectivePermissions{PrincipalID: target, Disabled: disabled, Actions: []EffectiveAction{}, Resource: EffectiveResource{
		Kind: string(res.Kind), FeedID: res.FeedID, SourceID: res.SourceID, Namespace: res.Namespace,
	}}
	for _, action := range allActions {
		item := EffectiveAction{Action: action, Allowed: !disabled && targetPolicy.Authorize(subject, res, action), ViaRoles: []string{}, DeniedByRoles: []string{}}
		for _, role := range targetPolicy.Roles {
			single := identity.Policy{Roles: []identity.Role{role}}
			if !disabled && single.Authorize(subject, res, action) {
				item.ViaRoles = append(item.ViaRoles, role.Name)
			}
			for _, g := range role.Grants {
				if matchesDeny(g, res, action) {
					item.DeniedByRoles = append(item.DeniedByRoles, role.Name)
					break
				}
			}
		}
		out.Actions = append(out.Actions, item)
	}
	JSON(w, 200, out)
}
