package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadToken inserts a token directly: expiredAgo/revokedAgo are how long ago
// the token expired and was revoked (negative expiredAgo: still valid).
func (f *authFixture) deadToken(kind, principal string, expiredAgo time.Duration, revokedAgo *time.Duration) string {
	f.t.Helper()
	secs := func(d time.Duration) string { return fmt.Sprintf("%d seconds", int(d.Seconds())) }
	table, name, val := "service_tokens", "", ""
	if kind == "personal" {
		table, name, val = "personal_access_tokens", ",name", ",'t'"
	}
	revoked := "NULL::timestamptz"
	args := []any{f.instance, principal, secs(expiredAgo)}
	if revokedAgo != nil {
		revoked = "now()-$4::interval"
		args = append(args, secs(*revokedAgo))
	}
	var id string
	err := f.db.QueryRow(context.Background(), `INSERT INTO `+table+`(instance_id,principal_id`+name+`,verifier,scopes,created_at,expires_at,revoked_at)
 VALUES($1,$2`+val+`,decode(md5(random()::text)||md5(random()::text),'hex'),'[]',now()-interval '2000 days',now()-$3::interval,`+revoked+`) RETURNING id::text`, args...).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func ago(d time.Duration) *time.Duration { return &d }

const hrs = time.Hour
const days = 24 * time.Hour

func TestTokenListStateFilters(t *testing.T) {
	f := newAuthFixture(t)
	svc := createIdentityPrincipal(f.sourceHTTPFixture, "robot", "service")
	user := createIdentityPrincipal(f.sourceHTTPFixture, "alice", "user")
	active := f.deadToken("service", svc.ID, -5*days, nil)
	expired := f.deadToken("service", svc.ID, 3*days, nil)
	revoked := f.deadToken("service", svc.ID, -5*days, ago(days))
	both := f.deadToken("service", svc.ID, 9*days, ago(days)) // revoked wins over expired
	ids := func(path string) map[string]TokenRecord {
		page := decodeJSON[TokenPage](t, mustStatus(t, f.admin("GET", path, ""), 200))
		out := map[string]TokenRecord{}
		for _, it := range page.Items {
			if it.State != it.Status {
				t.Fatalf("state %q != status %q", it.State, it.Status)
			}
			out[it.ID] = it
		}
		return out
	}
	base := "/api/v1/tokens?principal=" + svc.ID
	// The API default is unchanged: every state. The console asks for active.
	if got := ids(base); len(got) != 4 {
		t.Fatalf("default lists all, got %d", len(got))
	}
	if got := ids(base + "&state=all"); len(got) != 4 {
		t.Fatalf("all: %d", len(got))
	}
	if got := ids(base + "&state=active"); len(got) != 1 || got[active].State != "active" {
		t.Fatalf("active: %v", got)
	}
	if got := ids(base + "&state=expired"); len(got) != 1 || got[expired].State != "expired" || got[expired].ExpiresAt.IsZero() {
		t.Fatalf("expired: %v", got)
	}
	if got := ids(base + "&state=revoked"); len(got) != 2 || got[revoked].RevokedAt == nil || got[both].State != "revoked" {
		t.Fatalf("revoked: %v", got)
	}
	if got := ids(base + "&status=expired"); len(got) != 1 {
		t.Fatal("legacy status alias")
	}
	page := decodeJSON[TokenPage](t, mustStatus(t, f.admin("GET", base+"&state=active", ""), 200))
	if page.Counts != (TokenCounts{Active: 1, Expired: 1, Revoked: 2}) {
		t.Fatalf("counts ignore the state filter: %+v", page.Counts)
	}
	mustStatus(t, f.admin("GET", base+"&state=bogus", ""), 400)
	mustStatus(t, f.admin("GET", base+"&state=active&status=expired", ""), 400)
	mustStatus(t, f.admin("GET", base+"&state=active&state=expired", ""), 400)

	// Personal access tokens: same filters, scoped to the signed-in user.
	pActive := f.deadToken("personal", user.ID, -5*days, nil)
	f.deadToken("personal", user.ID, 2*days, nil)
	f.deadToken("personal", user.ID, -2*days, ago(hrs))
	pl := func(q string) PersonalAccessTokenList {
		return decodeJSON[PersonalAccessTokenList](t, mustStatus(t, f.as(user.ID, "GET", "/api/v1/auth/personal-access-tokens"+q, "", ""), 200))
	}
	if all := pl(""); len(all.Items) != 3 {
		t.Fatalf("default lists all: %d", len(all.Items))
	}
	act := pl("?state=active")
	if len(act.Items) != 1 || act.Items[0].ID != pActive || act.Items[0].State != "active" || act.Counts != (TokenCounts{Active: 1, Expired: 1, Revoked: 1}) {
		t.Fatalf("pat active: %+v", act)
	}
	if exp := pl("?state=expired"); len(exp.Items) != 1 || exp.Items[0].State != "expired" {
		t.Fatalf("pat expired: %+v", exp)
	}
	if rev := pl("?state=revoked"); len(rev.Items) != 1 || rev.Items[0].State != "revoked" || rev.Items[0].RevokedAt == nil {
		t.Fatalf("pat revoked: %+v", rev)
	}
	mustStatus(t, f.as(user.ID, "GET", "/api/v1/auth/personal-access-tokens?state=nope", "", ""), 400)
}

func TestTokenCleanupSettingAndAdminCleanup(t *testing.T) {
	f := newAuthFixture(t)
	svc := createIdentityPrincipal(f.sourceHTTPFixture, "robot", "service")
	user := createIdentityPrincipal(f.sourceHTTPFixture, "alice", "user")

	// Setting: default 30, enforced; validation; partial updates merge.
	item := func() SettingItem {
		for _, it := range decodeJSON[InstanceSettings](t, mustStatus(t, f.admin("GET", "/api/v1/settings", ""), 200)).Items {
			if it.Key == "token_cleanup_after_days" {
				return it
			}
		}
		t.Fatal("token_cleanup_after_days missing")
		return SettingItem{}
	}
	if it := item(); it.Value != "30" || it.Source != "default" || it.ReadOnly || !it.Enforced {
		t.Fatalf("default item %+v", it)
	}
	revision := func() string {
		w := mustStatus(t, f.admin("GET", "/api/v1/settings", ""), 200)
		return w.Header().Get("ETag")
	}
	putAt := func(body string, want int) {
		t.Helper()
		w := f.putSettings(body, revision())
		mustStatus(t, w, want)
	}
	putAt(`{"token_cleanup_after_days":-1}`, 400)
	putAt(`{"token_cleanup_after_days":3651}`, 400)
	putAt(`{"session_idle_timeout_seconds":600}`, 200)
	putAt(`{"token_cleanup_after_days":14}`, 200)
	settings := decodeJSON[InstanceSettings](t, mustStatus(t, f.admin("GET", "/api/v1/settings", ""), 200))
	vals := map[string]string{}
	for _, it := range settings.Items {
		vals[it.Key] = it.Value
	}
	if vals["token_cleanup_after_days"] != "14" || vals["session_idle_timeout_seconds"] != "600" {
		t.Fatalf("a partial update must not reset other fields: %v", vals)
	}

	// Seed: eligible at 14 days vs not.
	f.deadToken("service", svc.ID, 20*days, nil)
	f.deadToken("service", svc.ID, -3*days, ago(15*days)) // revoked 15d ago
	keepExpired := f.deadToken("service", svc.ID, 13*days, nil)
	keepRevoked := f.deadToken("service", svc.ID, -3*days, ago(2*days))
	keepActive := f.deadToken("service", svc.ID, -3*days, nil)
	f.deadToken("personal", user.ID, 40*days, nil)
	keepPAT := f.deadToken("personal", user.ID, -1*days, nil)

	body := `{"dry_run":true}`
	before := f.count(`SELECT count(*) FROM service_tokens`)
	// Admin only.
	mustStatus(t, f.as(user.ID, "POST", "/api/v1/tokens/cleanup", body, ""), 403)
	mustStatus(t, f.as("", "POST", "/api/v1/tokens/cleanup", body, ""), 401)
	dry := decodeJSON[TokenCleanupResult](t, mustStatus(t, f.admin("POST", "/api/v1/tokens/cleanup", body), 200))
	if !dry.DryRun || dry.AfterDays != 14 || dry.ServiceTokens != 2 || dry.PersonalAccessTokens != 1 || dry.Total != 3 {
		t.Fatalf("dry run %+v", dry)
	}
	if f.count(`SELECT count(*) FROM service_tokens`) != before || f.count(`SELECT count(*) FROM audit_events WHERE action='token.cleanup'`) != 0 {
		t.Fatal("dry run changed data")
	}
	run := decodeJSON[TokenCleanupResult](t, mustStatus(t, f.asKeyed("", "cleanup-1", "POST", "/api/v1/tokens/cleanup", `{"dry_run":false}`, f.credential), 200))
	if run.DryRun || run.Total != 3 || run.ServiceTokens != 2 || run.PersonalAccessTokens != 1 || run.HasMore {
		t.Fatalf("run %+v", run)
	}
	for table, ids := range map[string][]string{"service_tokens": {keepExpired, keepRevoked, keepActive}, "personal_access_tokens": {keepPAT}} {
		for _, id := range ids {
			if f.count(`SELECT count(*) FROM `+table+` WHERE id=$1`, id) != 1 {
				t.Fatalf("%s %s must survive", table, id)
			}
		}
	}
	// A replay of the same key does not run (or audit) twice.
	mustStatus(t, f.asKeyed("", "cleanup-1", "POST", "/api/v1/tokens/cleanup", `{"dry_run":false}`, f.credential), 200)
	if f.count(`SELECT count(*) FROM audit_events WHERE action='token.cleanup' AND principal_id IS NOT NULL AND (details->>'service_tokens')::int=2 AND (details->>'personal_access_tokens')::int=1 AND details->>'trigger'='admin'`) != 1 {
		t.Fatal("expected exactly one summary audit event with counts")
	}
	// Audit rows for tokens that no longer exist stay readable.
	gone := f.deadToken("service", svc.ID, 100*days, nil)
	f.db.Exec(context.Background(), `INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,'token.issue',$3)`, f.instance, svc.ID, gone)
	mustStatus(t, f.asKeyed("", "cleanup-2", "POST", "/api/v1/tokens/cleanup", `{"dry_run":false}`, f.credential), 200)
	audit := mustStatus(t, f.admin("GET", "/api/v1/audit?action=token.issue&limit=200", ""), 200)
	if !strings.Contains(audit.Body.String(), gone) {
		t.Fatalf("audit lost the reference to the deleted token: %s", audit.Body.String())
	}

	// 0 means never: cleanup is refused, nothing is deleted.
	putAt(`{"token_cleanup_after_days":0}`, 200)
	f.deadToken("service", svc.ID, 400*days, nil)
	mustStatus(t, f.admin("POST", "/api/v1/tokens/cleanup", body), 409)
	mustStatus(t, f.admin("POST", "/api/v1/tokens/cleanup", `{"dry_run":false}`), 409)
	if it := item(); it.Value != "0" || it.Source != "stored" {
		t.Fatalf("item %+v", it)
	}
	mustStatus(t, f.admin("POST", "/api/v1/tokens/cleanup", `{"dry_run":true,"after_days":1}`), 400)
}

func TestTokenCleanupSettingFromEnvironmentIsReadOnly(t *testing.T) {
	f := newAuthFixture(t)
	env := 7
	deps := f.deps
	deps.TokenCleanupDays = &env
	mux := http.NewServeMux()
	Register(mux)
	RegisterIdentities(mux, f.db)
	RegisterOperations(mux, deps)
	f.handler = WithRequestID(mux)
	var found SettingItem
	for _, it := range decodeJSON[InstanceSettings](t, mustStatus(t, f.admin("GET", "/api/v1/settings", ""), 200)).Items {
		if it.Key == "token_cleanup_after_days" {
			found = it
		}
	}
	if found.Value != "7" || found.Source != "env" || !found.ReadOnly || !found.Enforced {
		t.Fatalf("env item %+v", found)
	}
	mustStatus(t, f.putSettings(`{"token_cleanup_after_days":10}`, `"1"`), 409)
	// Other settings stay editable, and the env value drives the cleanup.
	mustStatus(t, f.putSettings(`{"session_idle_timeout_seconds":300}`, `"1"`), 200)
	svc := createIdentityPrincipal(f.sourceHTTPFixture, "robot", "service")
	f.deadToken("service", svc.ID, 8*days, nil)
	f.deadToken("service", svc.ID, 6*days, nil)
	res := decodeJSON[TokenCleanupResult](t, mustStatus(t, f.admin("POST", "/api/v1/tokens/cleanup", `{"dry_run":true}`), 200))
	if res.AfterDays != 7 || res.Total != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestPersonalTokenSelfCleanup(t *testing.T) {
	f := newAuthFixture(t)
	alice := createIdentityPrincipal(f.sourceHTTPFixture, "alice", "user")
	bob := createIdentityPrincipal(f.sourceHTTPFixture, "bob", "user")
	svc := createIdentityPrincipal(f.sourceHTTPFixture, "robot", "service")
	f.deadToken("personal", alice.ID, 2*days, nil)          // expired recently: still deletable by its owner
	f.deadToken("personal", alice.ID, -2*days, ago(hrs))    // revoked
	keep := f.deadToken("personal", alice.ID, -2*days, nil) // active
	bobExpired := f.deadToken("personal", bob.ID, 2*days, nil)
	svcExpired := f.deadToken("service", svc.ID, 2*days, nil)
	url := "/api/v1/auth/personal-access-tokens/cleanup"
	// Only a signed-in session can do this; machine tokens cannot.
	mustStatus(t, f.as("", "POST", url, "", f.credential), 403)
	mustStatus(t, f.as("", "POST", url, "", ""), 401)
	res := decodeJSON[PersonalTokenCleanupResult](t, mustStatus(t, f.asKeyed(alice.ID, "self-1", "POST", url, "", ""), 200))
	if res.Deleted != 2 {
		t.Fatalf("deleted %d", res.Deleted)
	}
	for table, id := range map[string]string{"personal_access_tokens": keep} {
		if f.count(`SELECT count(*) FROM `+table+` WHERE id=$1`, id) != 1 {
			t.Fatal("active token deleted")
		}
	}
	if f.count(`SELECT count(*) FROM personal_access_tokens WHERE id=$1`, bobExpired) != 1 || f.count(`SELECT count(*) FROM service_tokens WHERE id=$1`, svcExpired) != 1 {
		t.Fatal("cleanup touched somebody else's tokens")
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action='token.cleanup' AND principal_id=$1 AND details->>'trigger'='self' AND (details->>'personal_access_tokens')::int=2`, alice.ID) != 1 {
		t.Fatal("missing summary audit event")
	}
	// Replay is idempotent; a second run with nothing to delete audits nothing.
	mustStatus(t, f.asKeyed(alice.ID, "self-1", "POST", url, "", ""), 200)
	if again := decodeJSON[PersonalTokenCleanupResult](t, mustStatus(t, f.asKeyed(alice.ID, "self-2", "POST", url, "", ""), 200)); again.Deleted != 0 {
		t.Fatal("second cleanup deleted rows")
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action='token.cleanup'`) != 1 {
		t.Fatal("empty cleanup wrote an audit event")
	}
}

func (f *authFixture) putSettings(body, etag string) *httptest.ResponseRecorder {
	return f.requestAs("PUT", "/api/v1/settings", body, fmt.Sprintf("settings-test-%d", authKeySeq.Add(1)), etag, f.credential)
}
