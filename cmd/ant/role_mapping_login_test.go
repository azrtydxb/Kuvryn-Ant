package main

import (
	"context"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"net/http/httptest"
)

func localRole(t *testing.T, e *enterpriseLogin, name string) string {
	t.Helper()
	var id string
	if err := e.client.Sessions.DB.QueryRow(context.Background(), `INSERT INTO roles(instance_id,name) VALUES($1,$2) RETURNING id::text`, e.client.Sessions.InstanceID, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func claimRolesOf(t *testing.T, e *enterpriseLogin, issuer, subject string) (claim []string, managed, found bool) {
	t.Helper()
	ctx := context.Background()
	var principal string
	err := e.client.Sessions.DB.QueryRow(ctx, `SELECT principal_id::text,claim_managed FROM oidc_identity_bindings WHERE instance_id=$1 AND issuer=$2 AND subject=$3`, e.client.Sessions.InstanceID, issuer, subject).Scan(&principal, &managed)
	if err != nil {
		return nil, false, false
	}
	rows, err := e.client.Sessions.DB.Query(ctx, `SELECT r.name FROM principal_roles pr JOIN roles r ON r.id=pr.role_id WHERE pr.instance_id=$1 AND pr.principal_id=$2 AND pr.origin='claim' ORDER BY r.name`, e.client.Sessions.InstanceID, principal)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		claim = append(claim, n)
	}
	return claim, managed, true
}

func ldapSignIn(t *testing.T, e *enterpriseLogin) int {
	t.Helper()
	cookie, csrf := ldapPrelogin(t, e)
	return ldapPost(e, cookie, csrf, e.client.Origin, `{"username":"alice","password":"private-password"}`).Code
}

func TestLDAPGroupRolesAppliedAtLogin(t *testing.T) {
	e, _, ldap, _ := enterpriseFixture(t)
	reader, writer := localRole(t, e, "ldap-reader"), localRole(t, e, "ldap-writer")
	const issuer = "https://id.example/ldap"

	// An unbound directory user with mapped groups is provisioned just in time.
	ldap.subject, ldap.mapped, ldap.roles = "jit-user", true, []string{reader, writer}
	if code := ldapSignIn(t, e); code != 200 {
		t.Fatalf("jit login: %d", code)
	}
	if roles, managed, found := claimRolesOf(t, e, issuer, "jit-user"); !found || !managed || strings.Join(roles, ",") != "ldap-reader,ldap-writer" {
		t.Fatalf("jit roles: %v %v %v", roles, managed, found)
	}
	// Dropping a group at the directory drops the role at the next login.
	ldap.roles = []string{reader}
	if code := ldapSignIn(t, e); code != 200 {
		t.Fatalf("relogin: %d", code)
	}
	if roles, _, _ := claimRolesOf(t, e, issuer, "jit-user"); strings.Join(roles, ",") != "ldap-reader" {
		t.Fatalf("roles after group removal: %v", roles)
	}
	// Losing every mapped group keeps the account but leaves it with no claim roles.
	ldap.roles = nil
	if code := ldapSignIn(t, e); code != 200 {
		t.Fatalf("relogin without groups: %d", code)
	}
	if roles, _, _ := claimRolesOf(t, e, issuer, "jit-user"); len(roles) != 0 {
		t.Fatalf("roles after losing all groups: %v", roles)
	}

	// Mapped to nothing and unbound: no account, no session.
	ldap.subject = "nobody"
	if code := ldapSignIn(t, e); code != 401 {
		t.Fatalf("unmapped unbound user signed in: %d", code)
	}
	if _, _, found := claimRolesOf(t, e, issuer, "nobody"); found {
		t.Fatal("principal created without a mapped role")
	}

	// An explicit binding wins: the mapping never touches it.
	ldap.subject, ldap.roles = "immutable-subject", []string{reader, writer}
	if code := ldapSignIn(t, e); code != 200 {
		t.Fatalf("bound login: %d", code)
	}
	if roles, managed, found := claimRolesOf(t, e, issuer, "immutable-subject"); !found || managed || len(roles) != 0 {
		t.Fatalf("explicit binding was modified: %v %v %v", roles, managed, found)
	}

	// Without a configured mapping, role proposals are never granted.
	ldap.subject, ldap.mapped = "unmapped-provider-user", false
	if code := ldapSignIn(t, e); code != 401 {
		t.Fatalf("provider without mapping provisioned a user: %d", code)
	}
	var applied int
	if err := e.client.Sessions.DB.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action IN ('browser.claim_user_provisioned','browser.claim_roles_applied')`).Scan(&applied); err != nil || applied < 4 {
		t.Fatal("mapping not audited", applied, err)
	}
}

func TestSAMLAttributeRolesAppliedAtLogin(t *testing.T) {
	e, saml, _, _ := enterpriseFixture(t)
	admin := localRole(t, e, "saml-admin")
	const issuer = "https://id.example/saml"
	saml.subject, saml.rolesMapped, saml.roles = "saml-jit", true, []string{admin}
	samlSignIn(t, e)
	if roles, managed, found := claimRolesOf(t, e, issuer, "saml-jit"); !found || !managed || strings.Join(roles, ",") != "saml-admin" {
		t.Fatalf("saml jit roles: %v %v %v", roles, managed, found)
	}
	// The attribute disappears at the IdP: the role goes at the next login.
	saml.roles = nil
	samlSignIn(t, e)
	if roles, _, _ := claimRolesOf(t, e, issuer, "saml-jit"); len(roles) != 0 {
		t.Fatalf("roles after attribute removal: %v", roles)
	}
	// Explicitly bound identities keep their own memberships.
	saml.subject, saml.roles = "immutable-subject", []string{admin}
	samlSignIn(t, e)
	if roles, managed, _ := claimRolesOf(t, e, issuer, "immutable-subject"); managed || len(roles) != 0 {
		t.Fatalf("explicit binding was modified: %v %v", roles, managed)
	}
	// An unmapped, unbound SAML user stays refused.
	saml.subject, saml.roles, saml.rolesMapped = "stranger", nil, true
	start := httptest.NewRecorder()
	e.samlLogin(start, httptest.NewRequest("GET", "https://ant.example/auth/saml/login", nil))
	location, _ := url.Parse(start.Header().Get("Location"))
	body := url.Values{"RelayState": {location.Query().Get("RelayState")}, "SAMLResponse": {base64.StdEncoding.EncodeToString([]byte("verified-assertion"))}}
	r := httptest.NewRequest("POST", "https://ant.example/auth/saml/callback", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(findCookie(t, start, "__Host-ant-saml-login"))
	w := httptest.NewRecorder()
	e.samlCallback(w, r)
	if w.Code != 401 {
		t.Fatalf("unmapped unbound SAML user signed in: %d", w.Code)
	}
}
