package identity

type ResourceKind string

const (
	ArtifactResource ResourceKind = "artifact"
	FeedResource     ResourceKind = "feed"
	SourceResource   ResourceKind = "source"
	InstanceResource ResourceKind = "instance"
)

type Resource struct {
	InstanceID, FeedID, SourceID, Namespace string
	Kind                                    ResourceKind
}
type Effect string

const (
	Allow Effect = "allow"
	Deny  Effect = "deny"
)

// Grant selectors are exact, except an empty feed/namespace selects all values.
// Namespaces do not imply descendants: "team" never matches "team/child".
// Kind and instance are always required and exact. Admin is a distinct action,
// not a wildcard; feed administration does not confer source administration.
type Grant struct {
	PrincipalID                             string
	Anonymous                               bool
	InstanceID, FeedID, SourceID, Namespace string
	Kind                                    ResourceKind
	Actions                                 []Action
	Effect                                  Effect
}

// Role memberships come from trusted identity/group resolution, never tokens.
// A role grant's PrincipalID and Anonymous selectors must be empty/false.
type Role struct {
	Name         string
	PrincipalIDs []string
	Grants       []Grant
}
type Policy struct {
	Grants []Grant
	Roles  []Role
}

// Authorize intersects effective grants with machine-token scopes. Every matching
// deny wins, including one from another role. A zero principal is anonymous and
// may only read resources explicitly granted Anonymous read access.
func (p Policy) Authorize(principal Principal, resource Resource, action Action) bool {
	if resource.InstanceID == "" || !validAction(action) || !validKind(resource.Kind) {
		return false
	}
	anonymous := principal.PrincipalID == ""
	if anonymous {
		if principal.InstanceID != "" || principal.TokenID != "" || len(principal.Scopes) != 0 || action != Read {
			return false
		}
	} else if principal.InstanceID != resource.InstanceID {
		return false
	}
	allowed := false
	consider := func(g Grant) bool {
		if !matchesGrant(g, resource, action) {
			return true
		}
		if g.Effect == Deny {
			return false
		}
		if g.Effect == Allow {
			allowed = true
		}
		return true
	}
	for _, g := range p.Grants {
		applies := g.Anonymous && anonymous && g.PrincipalID == ""
		if !anonymous {
			applies = !g.Anonymous && g.PrincipalID == principal.PrincipalID
		}
		if applies && !consider(g) {
			return false
		}
	}
	if !anonymous {
		for _, role := range p.Roles {
			if !contains(role.PrincipalIDs, principal.PrincipalID) {
				continue
			}
			for _, g := range role.Grants {
				if g.PrincipalID == "" && !g.Anonymous && !consider(g) {
					return false
				}
			}
		}
	}
	if !allowed {
		return false
	}
	if principal.TokenID == "" {
		return len(principal.Scopes) == 0
	}
	for _, scope := range principal.Scopes {
		if (scope.FeedID == "" || scope.FeedID == resource.FeedID) && (scope.Namespace == "" || scope.Namespace == resource.Namespace) && hasAction(scope.Actions, action) {
			return true
		}
	}
	return false
}
func matchesGrant(g Grant, r Resource, a Action) bool {
	return g.InstanceID == r.InstanceID && g.Kind == r.Kind && (g.SourceID == "" || g.SourceID == r.SourceID) && (g.FeedID == "" || g.FeedID == r.FeedID) && (g.Namespace == "" || g.Namespace == r.Namespace) && hasAction(g.Actions, a)
}
func validAction(a Action) bool {
	return a == Read || a == Publish || a == Delete || a == Configure || a == Replicate || a == Admin
}
func validKind(k ResourceKind) bool {
	return k == ArtifactResource || k == FeedResource || k == SourceResource || k == InstanceResource
}
func hasAction(actions []Action, a Action) bool {
	for _, candidate := range actions {
		if candidate == a {
			return true
		}
	}
	return false
}
func contains(values []string, v string) bool {
	for _, candidate := range values {
		if candidate == v {
			return true
		}
	}
	return false
}
