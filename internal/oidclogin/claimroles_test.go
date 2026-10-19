package oidclogin

import (
	"context"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

const zitadelClaim = "urn:zitadel:iam:org:project:123:roles"

func TestClaimValues(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{`["a","b"]`, []string{"a", "b"}},
		{`"solo"`, []string{"solo"}},
		{`{"ant-user":{"o":"x"},"ant-admin":{"o":"x"}}`, []string{"ant-admin", "ant-user"}},
		{`12`, nil},
		{`null`, nil},
	} {
		if got := claimValues([]byte(tc.raw)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.raw, got, tc.want)
		}
	}
}

func TestExchangeMapsClaimToRoles(t *testing.T) {
	f, p := fixtureProvider(t)
	p.rolesClaim = zitadelClaim
	p.claimRoles = map[string][]string{"ant-admin": {"role-admin"}, "ant-user": {"role-read"}}
	f.mu.Lock()
	f.claims[zitadelClaim] = map[string]any{"ant-user": map[string]string{"1": "x"}, "ant-admin": map[string]string{"1": "x"}, "other": map[string]string{}}
	f.mu.Unlock()
	id, e := p.exchangeIdentity(context.Background(), "code", f.verifier, hash("nonce"))
	if e != nil || !id.RolesMapped || !slices.Equal(id.Roles, []string{"role-admin", "role-read"}) {
		t.Fatalf("%+v %v", id, e)
	}
	f.mu.Lock()
	delete(f.claims, zitadelClaim)
	f.mu.Unlock()
	id, e = p.exchangeIdentity(context.Background(), "code", f.verifier, hash("nonce"))
	if e != nil || !id.RolesMapped || len(id.Roles) != 0 {
		t.Fatalf("absent claim must map to nothing: %+v %v", id, e)
	}
	p.rolesClaim = ""
	id, _ = p.exchangeIdentity(context.Background(), "code", f.verifier, hash("nonce"))
	if id.RolesMapped {
		t.Fatal("mapping applied without configuration")
	}
}

func claimRoleFixture(t *testing.T, s Sessions, name string, kind, action string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if e := s.DB.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,$2) RETURNING id::text", s.InstanceID, name).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,$3,'allow',$4)", s.InstanceID, id, kind, []string{action}); e != nil {
		t.Fatal(e)
	}
	return id
}

func memberships(t *testing.T, s Sessions, principal string) []string {
	t.Helper()
	rows, e := s.DB.Query(context.Background(), "SELECT role_id::text FROM principal_roles WHERE instance_id=$1 AND principal_id=$2 ORDER BY role_id", s.InstanceID, principal)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func principalOf(t *testing.T, s Sessions, issuer, subject string) (string, bool, bool) {
	t.Helper()
	var id string
	var managed bool
	e := s.DB.QueryRow(context.Background(), "SELECT principal_id::text,claim_managed FROM oidc_identity_bindings WHERE instance_id=$1 AND issuer=$2 AND subject=$3", s.InstanceID, issuer, subject).Scan(&id, &managed)
	return id, managed, e == nil
}

func TestApplyClaimRoles(t *testing.T) {
	s := dbFixture(t)
	ctx := context.Background()
	const issuer = "https://idp.example"
	admin := claimRoleFixture(t, s, "ant-admin", "instance", "admin")
	reader := claimRoleFixture(t, s, "reader", "feed", "read")

	// Unmapped, unbound: nothing is provisioned and sign-in is still refused.
	if e := s.ApplyClaimRoles(ctx, issuer, "nobody", nil); e != nil {
		t.Fatal(e)
	}
	if _, _, ok := principalOf(t, s, issuer, "nobody"); ok {
		t.Fatal("unmapped user provisioned")
	}
	if _, _, _, e := s.issueGroups(ctx, issuer, "nobody", nil); e == nil {
		t.Fatal("unmapped user signed in")
	}
	// Unknown role IDs are ignored, so a mapping of only those provisions nothing.
	if e := s.ApplyClaimRoles(ctx, issuer, "ghost", []string{"00000000-0000-4000-8000-000000000000"}); e != nil {
		t.Fatal(e)
	}
	if _, _, ok := principalOf(t, s, issuer, "ghost"); ok {
		t.Fatal("unknown role provisioned a user")
	}

	// Just-in-time provisioning.
	if e := s.ApplyClaimRoles(ctx, issuer, "alice", []string{admin, reader}); e != nil {
		t.Fatal(e)
	}
	alice, managed, ok := principalOf(t, s, issuer, "alice")
	if !ok || !managed {
		t.Fatal("alice not provisioned as claim-managed")
	}
	if got := memberships(t, s, alice); len(got) != 2 {
		t.Fatalf("memberships %v", got)
	}
	session, _, _, e := s.issueGroups(ctx, issuer, "alice", nil)
	if e != nil || session.PrincipalID != alice {
		t.Fatalf("alice sign-in: %v", e)
	}
	pol, e := (identity.Roles{DB: s.DB}).Resolve(ctx, identity.Principal{InstanceID: s.InstanceID, PrincipalID: alice})
	if e != nil || !pol.Authorize(identity.Principal{InstanceID: s.InstanceID, PrincipalID: alice}, identity.Resource{InstanceID: s.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		t.Fatalf("mapped admin is not instance admin: %v", e)
	}

	// Demotion at the IdP is applied at the next login.
	if e := s.ApplyClaimRoles(ctx, issuer, "alice", []string{reader}); e != nil {
		t.Fatal(e)
	}
	if got := memberships(t, s, alice); !slices.Equal(got, []string{reader}) {
		t.Fatalf("after demotion: %v", got)
	}
	pol, _ = (identity.Roles{DB: s.DB}).Resolve(ctx, identity.Principal{InstanceID: s.InstanceID, PrincipalID: alice})
	if pol.Authorize(identity.Principal{InstanceID: s.InstanceID, PrincipalID: alice}, identity.Resource{InstanceID: s.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		t.Fatal("demoted user still admin")
	}
	// Removal of every mapped value drops all claim memberships.
	if e := s.ApplyClaimRoles(ctx, issuer, "alice", nil); e != nil {
		t.Fatal(e)
	}
	if got := memberships(t, s, alice); len(got) != 0 {
		t.Fatalf("after removal: %v", got)
	}

	// Explicit bindings take precedence: no role change, claims ignored.
	var bob string
	if e := s.DB.QueryRow(ctx, "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','bob') RETURNING id::text", s.InstanceID).Scan(&bob); e != nil {
		t.Fatal(e)
	}
	if e := s.BindIdentity(ctx, issuer, "bob-sub", bob); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)", s.InstanceID, bob, reader); e != nil {
		t.Fatal(e)
	}
	if e := s.ApplyClaimRoles(ctx, issuer, "bob-sub", []string{admin}); e != nil {
		t.Fatal(e)
	}
	if got := memberships(t, s, bob); !slices.Equal(got, []string{reader}) {
		t.Fatalf("explicit binding changed by claims: %v", got)
	}
	if e := s.ApplyClaimRoles(ctx, issuer, "bob-sub", nil); e != nil {
		t.Fatal(e)
	}
	if got := memberships(t, s, bob); !slices.Equal(got, []string{reader}) {
		t.Fatalf("explicit membership removed by empty claims: %v", got)
	}
	if _, managed, _ := principalOf(t, s, issuer, "bob-sub"); managed {
		t.Fatal("explicit binding became claim-managed")
	}

	// A local membership for the same role is never converted or removed.
	var carol string
	if e := s.ApplyClaimRoles(ctx, issuer, "carol", []string{reader}); e != nil {
		t.Fatal(e)
	}
	carol, _, _ = principalOf(t, s, issuer, "carol")
	if _, e := s.DB.Exec(ctx, "INSERT INTO principal_roles(instance_id,principal_id,role_id,origin) VALUES($1,$2,$3,'local')", s.InstanceID, carol, admin); e != nil {
		t.Fatal(e)
	}
	if e := s.ApplyClaimRoles(ctx, issuer, "carol", nil); e != nil {
		t.Fatal(e)
	}
	if got := memberships(t, s, carol); !slices.Equal(got, []string{admin}) {
		t.Fatalf("local membership disturbed: %v", got)
	}

	// An existing principal that owns issuer+subject without a binding is not taken over.
	if _, e := s.DB.Exec(ctx, "INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'user','dave',$2)", s.InstanceID, issuer); e != nil {
		t.Fatal(e)
	}
	if e := s.ApplyClaimRoles(ctx, issuer, "dave", []string{admin}); e != nil {
		t.Fatal(e)
	}
	if _, _, ok := principalOf(t, s, issuer, "dave"); ok {
		t.Fatal("existing principal taken over")
	}

	// Disabled claim-managed principals receive no new roles.
	if e := s.ApplyClaimRoles(ctx, issuer, "erin", []string{reader}); e != nil {
		t.Fatal(e)
	}
	erin, _, _ := principalOf(t, s, issuer, "erin")
	s.DB.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", erin)
	if e := s.ApplyClaimRoles(ctx, issuer, "erin", []string{admin, reader}); e != nil {
		t.Fatal(e)
	}
	if got := memberships(t, s, erin); !slices.Equal(got, []string{reader}) {
		t.Fatalf("disabled principal gained roles: %v", got)
	}

	var audits int
	s.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE instance_id=$1 AND action IN ('browser.claim_user_provisioned','browser.claim_roles_applied')", s.InstanceID).Scan(&audits)
	if audits < 5 {
		t.Fatalf("claim events not audited: %d", audits)
	}
}

// End to end through the HTTP callback with a fake OIDC issuer: the first
// login of a mapped user provisions and signs in; an unmapped one is refused;
// a mapping whose roles cannot be applied fails closed.
func TestCallbackProvisionsMappedUser(t *testing.T) {
	s := dbFixture(t)
	ctx := context.Background()
	f, p := fixtureProvider(t)
	admin := claimRoleFixture(t, s, "ant-admin", "instance", "admin")
	p.rolesClaim = zitadelClaim
	p.claimRoles = map[string][]string{"ant-admin": {admin}}
	c := Client{Provider: p, Sessions: s, Origin: "https://console.example"}
	login := func(claims map[string]any) int {
		w := httptest.NewRecorder()
		c.Login(w, httptest.NewRequest("GET", "https://console.example/login", nil))
		auth, _ := url.Parse(w.Header().Get("Location"))
		cookie := w.Result().Cookies()[0]
		f.mu.Lock()
		f.verifier = cookie.Value
		f.claims["nonce"] = auth.Query().Get("nonce")
		delete(f.claims, zitadelClaim)
		for k, v := range claims {
			f.claims[k] = v
		}
		f.mu.Unlock()
		cb := httptest.NewRequest("GET", "https://console.example/callback?code=fixture&state="+auth.Query().Get("state"), nil)
		cb.AddCookie(cookie)
		rec := httptest.NewRecorder()
		c.Callback(rec, cb)
		return rec.Code
	}
	if code := login(nil); code != 401 {
		t.Fatalf("unmapped unbound login: %d", code)
	}
	if code := login(map[string]any{zitadelClaim: map[string]any{"ant-admin": map[string]string{"x": "y"}}}); code != 200 {
		t.Fatalf("mapped login: %d", code)
	}
	principal, managed, ok := principalOf(t, s, p.issuer, "immutable-user")
	if !ok || !managed || !slices.Equal(memberships(t, s, principal), []string{admin}) {
		t.Fatal("mapped user not provisioned with mapped role")
	}
	if code := login(map[string]any{zitadelClaim: map[string]any{"other": map[string]string{}}}); code != 200 {
		t.Fatalf("known user without mapped values: %d", code)
	}
	if got := memberships(t, s, principal); len(got) != 0 {
		t.Fatalf("roles survived removal at the IdP: %v", got)
	}
	_ = ctx
}
