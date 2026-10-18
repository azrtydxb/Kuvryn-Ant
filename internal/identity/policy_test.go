package identity

import "testing"

func TestPolicyIntersectionAndDenyWins(t *testing.T) {
	r := Resource{InstanceID: "instance", FeedID: "feed", Namespace: "team", Kind: ArtifactResource}
	principal := Principal{InstanceID: "instance", PrincipalID: "user", TokenID: "token", Scopes: []Scope{{FeedID: "feed", Namespace: "team", Actions: []Action{Read}}}}
	grant := Grant{InstanceID: "instance", Kind: ArtifactResource, Actions: []Action{Read, Publish}, Effect: Allow}
	policy := Policy{Roles: []Role{{Name: "reader", PrincipalIDs: []string{"user"}, Grants: []Grant{grant}}}}
	if !policy.Authorize(principal, r, Read) {
		t.Fatal("matching role and scope denied")
	}
	if policy.Authorize(principal, r, Publish) {
		t.Fatal("role bypassed token scope")
	}
	if (Policy{}).Authorize(principal, r, Read) {
		t.Fatal("token scope became a grant")
	}
	other := principal
	other.InstanceID = "other"
	if policy.Authorize(other, r, Read) {
		t.Fatal("cross-instance access")
	}
	child := r
	child.Namespace = "team/child"
	if policy.Authorize(principal, child, Read) {
		t.Fatal("exact namespace matched descendant")
	}
	deny := grant
	deny.Effect = Deny
	policy.Roles = append(policy.Roles, Role{Name: "denied", PrincipalIDs: []string{"user"}, Grants: []Grant{deny}})
	if policy.Authorize(principal, r, Read) {
		t.Fatal("role allow bypassed deny")
	}
}
func TestAdministrationAndAnonymousAreExplicit(t *testing.T) {
	r := Resource{InstanceID: "instance", FeedID: "feed", Kind: FeedResource}
	p := Principal{InstanceID: "instance", PrincipalID: "user"}
	grant := Grant{PrincipalID: "user", InstanceID: "instance", Kind: FeedResource, Actions: []Action{Configure}, Effect: Allow}
	policy := Policy{Grants: []Grant{grant}}
	if !policy.Authorize(p, r, Configure) {
		t.Fatal("explicit administration denied")
	}
	r.Kind = SourceResource
	if policy.Authorize(p, r, Configure) {
		t.Fatal("feed grant conferred source administration")
	}
	if policy.Authorize(Principal{}, r, Read) {
		t.Fatal("implicit anonymous access")
	}
	grant.PrincipalID = ""
	grant.Anonymous = true
	grant.Kind = SourceResource
	grant.Actions = []Action{Read, Publish}
	policy.Grants = append(policy.Grants, grant)
	if !policy.Authorize(Principal{}, r, Read) {
		t.Fatal("explicit anonymous read denied")
	}
	if policy.Authorize(Principal{}, r, Publish) {
		t.Fatal("anonymous write allowed")
	}
	grant.Effect = Deny
	policy.Grants = append(policy.Grants, grant)
	if policy.Authorize(Principal{}, r, Read) {
		t.Fatal("anonymous deny ignored")
	}
}

func TestSourceAdministrationSelectorsAndTokenIntersection(t *testing.T) {
	p := Principal{InstanceID: "instance", PrincipalID: "user", TokenID: "token", Scopes: []Scope{{Actions: []Action{Configure}}}}
	policy := Policy{Grants: []Grant{{PrincipalID: "user", InstanceID: "instance", Kind: SourceResource, SourceID: "source-a", Actions: []Action{Configure}, Effect: Allow}}}
	r := Resource{InstanceID: "instance", Kind: SourceResource, SourceID: "source-a"}
	if !policy.Authorize(p, r, Configure) {
		t.Fatal("scoped source administration denied")
	}
	r.SourceID = "source-b"
	if policy.Authorize(p, r, Configure) {
		t.Fatal("source grant escaped exact selector")
	}
	r.SourceID = "source-a"
	p.Scopes[0].FeedID = "feed"
	if policy.Authorize(p, r, Configure) {
		t.Fatal("feed-scoped token authorized independent source")
	}
}
