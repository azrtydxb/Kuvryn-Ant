package management

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/localadmin"
)

func TestLocalUserAdministration(t *testing.T) {
	f := newIDPFixture(t)
	const pw = "a-long-enough-passphrase"
	body := `{"username":"alice","password":"` + pw + `"}`

	// Admin only; unauthenticated callers are refused.
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/local-accounts", "", "", "", f.limited), 403)
	expectSourceStatus(t, f.requestAs("POST", "/api/v1/local-accounts", body, "", "", f.limited), 403)
	expectSourceStatus(t, f.requestAs("GET", "/api/v1/local-accounts", "", "", "", ""), 401)

	// Validation: weak password, bad username, unknown fields, missing key.
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"alice","password":"short"}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"Al Ice","password":"`+pw+`"}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"alice","password":"`+pw+`","admin":true}`, "", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", body, "-", ""), 400)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"break-glass","password":"`+pw+`"}`, "", ""), 409)

	// Create is idempotent, never echoes or persists the password.
	a := f.request("POST", "/api/v1/local-accounts", body, "lu-create", "")
	b := f.request("POST", "/api/v1/local-accounts", body, "lu-create", "")
	expectSourceStatus(t, a, 201)
	expectSourceStatus(t, b, 201)
	if strings.Contains(a.Body.String()+b.Body.String(), pw) {
		t.Fatal("password echoed")
	}
	u := decodeJSON[LocalUser](t, a)
	if u.Username != "alice" || u.PrincipalID == "" || u.BreakGlass {
		t.Fatalf("%+v", u)
	}
	if decodeJSON[LocalUser](t, b).PrincipalID != u.PrincipalID {
		t.Fatal("replay created a second principal")
	}
	var n int
	if err := f.db.QueryRow(t.Context(), `SELECT count(*) FROM mutation_results WHERE response::text LIKE '%`+pw+`%'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("password persisted in replay store", err, n)
	}
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts", body, "", ""), 409)

	list := decodeJSON[LocalUserList](t, f.get("/api/v1/local-accounts"))
	if len(list.Items) != 1 || list.Items[0].Username != "alice" {
		t.Fatalf("%+v", list)
	}

	// Password change and removal.
	expectSourceStatus(t, f.request("PUT", "/api/v1/local-accounts/alice/password", `{"password":"short"}`, "", ""), 400)
	expectSourceStatus(t, f.request("PUT", "/api/v1/local-accounts/nobody/password", `{"password":"`+pw+`-2"}`, "", ""), 404)
	expectSourceStatus(t, f.request("PUT", "/api/v1/local-accounts/alice/password", `{"password":"`+pw+`-2"}`, "", ""), 204)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-accounts/alice", "", "", ""), 204)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-accounts/alice", "", "", ""), 404)
	if got := decodeJSON[LocalUserList](t, f.get("/api/v1/local-accounts")); len(got.Items) != 0 {
		t.Fatalf("%+v", got)
	}
}

// session issues a request as a signed-in browser session of principal, the way
// the console does, rather than with a bearer token.
func (f *idpFixture) session(principal, method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		r.Header.Set("Idempotency-Key", fmt.Sprintf("session-%d", idpKeySeq.Add(1)))
	}
	r = r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), identity.Principal{InstanceID: f.instance, PrincipalID: principal}))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func testTOTPCode(secret []byte, at time.Time) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:off+4])&0x7fffffff)%1000000)
}

// Ordinary local users get the break-glass account's second-factor and unlock
// tooling: an admin can unlock them or reset a lost factor, and they can enrol
// their own TOTP once the instance setting allows it.
func TestLocalUserTOTPAndUnlock(t *testing.T) {
	f := newIDPFixture(t)
	ctx := t.Context()
	const pw = "a-long-enough-passphrase"
	alice := decodeJSON[LocalUser](t, mustStatus(t, f.request("POST", "/api/v1/local-accounts", `{"username":"alice","password":"`+pw+`"}`, "", ""), 201))
	svc := &localadmin.Service{DB: f.db, Keys: f.deps.Keys, InstanceID: f.instance}

	// Who am I: alice has an account, the service admin does not.
	if me := decodeJSON[LocalUser](t, mustStatus(t, f.session(alice.PrincipalID, "GET", "/api/v1/local-account/me", ""), 200)); me.Username != "alice" || me.TOTPEnabled {
		t.Fatalf("me: %+v", me)
	}
	expectSourceStatus(t, f.request("GET", "/api/v1/local-account/me", "", "", ""), 404)

	// Enrolment is off by default.
	expectSourceStatus(t, f.session(alice.PrincipalID, "POST", "/api/v1/local-account/totp", ""), 403)
	pol := decodeJSON[IdentityPolicy](t, mustStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":false,"local_user_totp":true}`, "", ""), 200))
	if pol.LocalUserTOTP == nil || !*pol.LocalUserTOTP {
		t.Fatalf("policy: %+v", pol)
	}
	if again := decodeJSON[IdentityPolicy](t, f.get("/api/v1/identity-policy")); again.LocalUserTOTP == nil || !*again.LocalUserTOTP {
		t.Fatalf("policy read: %+v", again)
	}
	// A bearer token (even the admin's) and a principal without a local account cannot enrol.
	expectSourceStatus(t, f.request("POST", "/api/v1/local-account/totp", "", "", ""), 403)

	enrol := decodeJSON[TOTPEnrollment](t, mustStatus(t, f.session(alice.PrincipalID, "POST", "/api/v1/local-account/totp", ""), 200))
	uri, err := url.Parse(enrol.OtpauthURI)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(uri.Query().Get("secret"))
	if err != nil || len(secret) == 0 {
		t.Fatal("no secret in", enrol.OtpauthURI, err)
	}
	expectSourceStatus(t, f.session(alice.PrincipalID, "POST", "/api/v1/local-account/totp/confirm", `{"code":"000000"}`), 400)
	expectSourceStatus(t, f.session(alice.PrincipalID, "POST", "/api/v1/local-account/totp/confirm", `{"code":"`+testTOTPCode(secret, time.Now().Add(-30*time.Second))+`"}`), 204)
	if me := decodeJSON[LocalUser](t, f.session(alice.PrincipalID, "GET", "/api/v1/local-account/me", "")); !me.TOTPEnabled {
		t.Fatal("totp not enabled after confirm")
	}

	// Verification at sign-in: the password alone is no longer enough.
	if _, err = svc.Login(ctx, "alice", pw, "", "203.0.113.9"); !errors.Is(err, localadmin.ErrMFARequired) {
		t.Fatalf("expected MFA required, got %v", err)
	}
	if _, err = svc.Login(ctx, "alice", pw, testTOTPCode(secret, time.Now()), "203.0.113.9"); err != nil {
		t.Fatalf("login with code: %v", err)
	}

	// Lock the account out, then an admin unlocks it without touching the password.
	for i := 0; i < localadmin.LockoutThreshold; i++ {
		_, _ = svc.Login(ctx, "alice", "wrong-password-value", "", "203.0.113.9")
	}
	if _, err = svc.Login(ctx, "alice", pw, "", "203.0.113.9"); !errors.Is(err, localadmin.ErrLocked) {
		t.Fatalf("expected lockout, got %v", err)
	}
	var locked bool
	for _, u := range decodeJSON[LocalUserList](t, f.get("/api/v1/local-accounts")).Items {
		if u.Username == "alice" {
			locked = u.LockedUntil != nil
		}
	}
	if !locked {
		t.Fatal("list does not report the lockout")
	}
	expectSourceStatus(t, f.session(alice.PrincipalID, "POST", "/api/v1/local-accounts/alice/unlock", ""), 403) // not an admin
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts/nobody/unlock", "", "", ""), 404)
	expectSourceStatus(t, f.request("POST", "/api/v1/local-accounts/alice/unlock", "", "", ""), 204)
	if _, err = svc.Login(ctx, "alice", "wrong-password-value", "", "203.0.113.9"); !errors.Is(err, localadmin.ErrInvalidCredentials) {
		t.Fatalf("still locked after unlock: %v", err)
	}

	// Lost device: an admin resets the factor; the user can sign in with the password again.
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-accounts/break-glass/totp", "", "", ""), 404) // not created here
	expectSourceStatus(t, f.session(alice.PrincipalID, "DELETE", "/api/v1/local-accounts/alice/totp", ""), 403)
	expectSourceStatus(t, f.request("DELETE", "/api/v1/local-accounts/alice/totp", "", "", ""), 204)
	if _, err = svc.Login(ctx, "alice", pw, "", "203.0.113.9"); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
	for _, action := range []string{"auth.local.unlocked", "auth.local.totp_reset", "auth.local.totp_enabled", "identity_policy.local_user_totp_enabled"} {
		var n int
		if err := f.db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action=$1`, action).Scan(&n); err != nil || n < 1 {
			t.Fatalf("%s not audited (%d, %v)", action, n, err)
		}
	}

	// Disabling the setting stops new enrolment.
	expectSourceStatus(t, f.request("PUT", "/api/v1/identity-policy", `{"sso_mandatory":false,"local_user_totp":false}`, "", ""), 200)
	expectSourceStatus(t, f.session(alice.PrincipalID, "POST", "/api/v1/local-account/totp", ""), 403)
}
