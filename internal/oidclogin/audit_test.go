package oidclogin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func userPrincipal(t *testing.T, s Sessions, subject string) string {
	t.Helper()
	var id string
	if e := s.DB.QueryRow(context.Background(), "INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user',$2) RETURNING id", s.InstanceID, subject).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return id
}
func TestAuditedExternalSessionsAndRollback(t *testing.T) {
	s := dbFixture(t)
	ctx := context.Background()
	principal := userPrincipal(t, s, "internal-user")
	issuer, subject := "trusted:saml", "sensitive-immutable-subject"
	if _, _, _, e := s.IssueExternal(ctx, issuer, subject); e == nil {
		t.Fatal("external implicit provisioning")
	}
	if e := s.BindIdentity(ctx, issuer, subject, principal); e != nil {
		t.Fatal(e)
	}
	_, raw, csrf, e := s.IssueExternal(ctx, issuer, subject)
	if e != nil {
		t.Fatal(e)
	}
	var roles int
	if e = s.DB.QueryRow(ctx, "SELECT count(*) FROM principal_roles WHERE principal_id=$1", principal).Scan(&roles); e != nil || roles != 0 {
		t.Fatal("external issuance changed roles", e)
	}
	if _, e = s.DB.Exec(ctx, "UPDATE principals SET disabled=true WHERE id=$1", principal); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = s.IssueExternal(ctx, issuer, subject); e == nil {
		t.Fatal("disabled principal issued session")
	}
	if e = s.BindIdentity(ctx, issuer, "another-subject", principal); e == nil {
		t.Fatal("disabled principal bound")
	}
	if _, e = s.DB.Exec(ctx, "UPDATE principals SET disabled=false WHERE id=$1", principal); e != nil {
		t.Fatal(e)
	}
	if e = s.RecordLoginFailure(ctx); e != nil {
		t.Fatal(e)
	}
	var auditBefore int
	if e = s.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events").Scan(&auditBefore); e != nil {
		t.Fatal(e)
	}
	// Reject the second half of the event transaction, proving audit/outbox failure
	// cannot leave successful security mutations or a misleading partial event.
	if _, e = s.DB.Exec(ctx, "ALTER TABLE outbox_events ADD CONSTRAINT reject_audit CHECK(false) NOT VALID"); e != nil {
		t.Fatal(e)
	}
	if e = s.BindIdentity(ctx, issuer, "rollback-subject", principal); e == nil {
		t.Fatal("binding committed without outbox")
	}
	if session, token, csrf, e := s.IssueExternal(ctx, issuer, subject); e == nil || token != "" || csrf != "" || session.PrincipalID != "" {
		t.Fatal("issuance leaked uncommitted credential")
	}
	if e = s.Revoke(ctx, raw); e == nil {
		t.Fatal("revoke committed without outbox")
	}
	if e = s.RecordLoginFailure(ctx); e == nil {
		t.Fatal("audit failure hidden")
	}
	var audits, sessions, bindings int
	s.DB.QueryRow(ctx, "SELECT count(*) FROM audit_events").Scan(&audits)
	s.DB.QueryRow(ctx, "SELECT count(*) FROM browser_sessions").Scan(&sessions)
	s.DB.QueryRow(ctx, "SELECT count(*) FROM oidc_identity_bindings").Scan(&bindings)
	if audits != auditBefore || sessions != 1 || bindings != 1 {
		t.Fatalf("partial transaction: audit=%d session=%d binding=%d", audits, sessions, bindings)
	}
	if _, e = s.Authenticate(ctx, raw, csrf, true); e != nil {
		t.Fatal("failed revoke changed session")
	}
	if _, e = s.DB.Exec(ctx, "ALTER TABLE outbox_events DROP CONSTRAINT reject_audit"); e != nil {
		t.Fatal(e)
	}
	if e = s.Revoke(ctx, raw); e != nil {
		t.Fatal(e)
	}
	rows, e := s.DB.Query(ctx, `SELECT row_to_json(a)::text FROM audit_events a UNION ALL SELECT row_to_json(o)::text FROM outbox_events o`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var record string
		if e = rows.Scan(&record); e != nil {
			t.Fatal(e)
		}
		for _, sensitive := range []string{raw, csrf, subject, issuer, "rollback-subject"} {
			if strings.Contains(record, sensitive) {
				t.Fatal("sensitive input retained in audit")
			}
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
}
func TestBindIdentityAsEnforcesSharedPolicy(t *testing.T) {
	s := dbFixture(t)
	ctx := context.Background()
	actor := identity.Principal{InstanceID: s.InstanceID, PrincipalID: userPrincipal(t, s, "actor")}
	target := userPrincipal(t, s, "target")
	if e := s.BindIdentityAs(ctx, actor, "trusted:ldap", "uid=user", target); e == nil {
		t.Fatal("unprivileged actor accepted")
	}
	var role string
	if e := s.DB.QueryRow(ctx, "INSERT INTO roles(instance_id,name) VALUES($1,'test-admin') RETURNING id", s.InstanceID).Scan(&role); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)", s.InstanceID, actor.PrincipalID, role); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,'instance','allow',ARRAY['admin'])", s.InstanceID, role); e != nil {
		t.Fatal(e)
	}
	if e := s.BindIdentityAs(ctx, actor, "trusted:ldap", "uid=user", target); e != nil {
		t.Fatal(e)
	}
	var recorded string
	if e := s.DB.QueryRow(ctx, "SELECT principal_id FROM audit_events WHERE action='browser.binding_created'").Scan(&recorded); e != nil || recorded != actor.PrincipalID {
		t.Fatal("missing actor provenance")
	}
	if _, e := s.DB.Exec(ctx, "INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,'instance','deny',ARRAY['admin'])", s.InstanceID, role); e != nil {
		t.Fatal(e)
	}
	if e := s.BindIdentityAs(ctx, actor, "trusted:ldap", "uid=denied", target); e == nil {
		t.Fatal("explicit deny ignored")
	}
	if _, e := s.DB.Exec(ctx, "DELETE FROM role_grants WHERE effect='deny'; UPDATE principals SET disabled=true WHERE subject='actor'"); e != nil {
		t.Fatal(e)
	}
	if e := s.BindIdentityAs(ctx, actor, "trusted:ldap", "uid=disabled", target); e == nil {
		t.Fatal("disabled admin accepted")
	}
}
func TestLoginFailureThrottleAuditAndHeaders(t *testing.T) {
	s := dbFixture(t)
	c := Client{Provider: &Provider{redirect: "https://console.example/callback"}, Sessions: s}
	for i := 0; i < 7; i++ {
		r := httptest.NewRequest("GET", "https://console.example/callback?code=raw-secret-code&state=raw-secret-state", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Forwarded-For", time.Now().String())
		w := httptest.NewRecorder()
		c.Callback(w, r)
		want := 401
		if i >= 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d status %d want %d", i, w.Code, want)
		}
		if strings.Contains(w.Body.String(), "raw-secret") {
			t.Fatal("credential reflected")
		}
	}
	var count int
	if e := s.DB.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE action='browser.login_failed'").Scan(&count); e != nil || count != 5 {
		t.Fatal("missing or unbounded failure events", count, e)
	}
	var records string
	if e := s.DB.QueryRow(context.Background(), "SELECT coalesce(string_agg(row_to_json(a)::text, ''),'') FROM audit_events a").Scan(&records); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(records, "raw-secret") || strings.Contains(records, "192.0.2.1") {
		t.Fatal("login failure audit retained request data")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if e := s.RecordLoginFailure(canceled); e != nil {
		t.Fatal("request cancellation lost audit")
	}
}
func TestPeerLimiterBounded(t *testing.T) {
	var l peerLimiter
	now := time.Now()
	for range 20 {
		if !l.allow("same", now) {
			t.Fatal("premature limit")
		}
	}
	if l.allow("same", now) {
		t.Fatal("attempt limit bypass")
	}
	if !l.allow("same", now.Add(time.Minute)) {
		t.Fatal("window not renewed")
	}
	l = peerLimiter{peers: make(map[string]peerBudget)}
	for i := 0; i < 4096; i++ {
		l.peers[string(rune(i))] = peerBudget{until: now.Add(time.Minute)}
	}
	if l.allow("new-key", now) {
		t.Fatal("capacity bypass")
	}
	if len(l.peers) != 4096 {
		t.Fatal("unbounded peer map")
	}
	r := httptest.NewRequest("GET", "https://example.test", nil)
	r.RemoteAddr = "invalid-attacker-string"
	if peer(r) != "unknown" {
		t.Fatal("unbounded peer key")
	}
}
