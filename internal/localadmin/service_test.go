package localadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/testdb"
)

const goodPassword = "correct horse battery staple"

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword(goodPassword)
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatal(h, err)
	}
	if !VerifyPassword(h, goodPassword) || VerifyPassword(h, goodPassword+"x") {
		t.Fatal("verify")
	}
	if _, err = HashPassword("short"); err == nil {
		t.Fatal("policy not enforced")
	}
	for _, bad := range []string{"", "$argon2id$v=19$m=999999999,t=3,p=2$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA", "plain", "$argon2i$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA"} {
		if VerifyPassword(bad, goodPassword) {
			t.Fatal("accepted malformed hash", bad)
		}
	}
}

func TestTOTPReplayAndWindow(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(59, 0) // RFC 6238 vector: step 1, code 287082 for the 8-digit 94287082
	code := totpCode(secret, 1)
	if code != "287082" {
		t.Fatal("RFC 6238 vector mismatch", code)
	}
	step, ok := matchTOTP(secret, code, now, 0)
	if !ok || step != 1 {
		t.Fatal("valid code rejected")
	}
	if _, ok = matchTOTP(secret, code, now, step); ok {
		t.Fatal("replay accepted")
	}
	if _, ok = matchTOTP(secret, code, now.Add(5*time.Minute), 0); ok {
		t.Fatal("stale code accepted")
	}
}

type fixture struct {
	f   testdb.Fixture
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	f := testdb.New(t)
	x := &fixture{f: f, now: time.Now()}
	x.svc = &Service{DB: f.Pool, Keys: f.Keys, InstanceID: f.InstanceID, Now: func() time.Time { return x.now }}
	if _, err := x.svc.ResetBreakGlass(context.Background(), goodPassword); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *fixture) audits(t *testing.T, action, result string) int {
	return x.f.Count(t, `SELECT count(*) FROM audit_events WHERE instance_id=$1 AND action=$2 AND result=$3`, x.f.InstanceID, action, result)
}

func TestLoginLockoutAndAudit(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	res, err := x.svc.Login(ctx, "break-glass", goodPassword, "", "203.0.113.1")
	if err != nil || !res.BreakGlass || res.PrincipalID == "" {
		t.Fatal(err)
	}
	if x.audits(t, "auth.breakglass.login", "success") != 1 {
		t.Fatal("break-glass login not audited")
	}
	for i := 0; i < LockoutThreshold; i++ {
		if _, err = x.svc.Login(ctx, "break-glass", "wrong password here", "", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	// Even the right password is refused while locked.
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, "", ""); !errors.Is(err, ErrLocked) {
		t.Fatal("lockout not enforced", err)
	}
	if x.audits(t, "auth.local.failure", "failure") != LockoutThreshold || x.audits(t, "auth.local.denied", "denied") != 1 {
		t.Fatal("failures/lockout not audited")
	}
	// Lock expires; one more failure escalates the lockout (counter persists).
	x.now = x.now.Add(16 * time.Minute)
	_, _ = x.svc.Login(ctx, "break-glass", "wrong password here", "", "")
	st, _ := x.svc.Status(ctx)
	if st.LockedUntil == nil || st.LockedUntil.Before(x.now.Add(29*time.Minute)) {
		t.Fatal("lockout did not escalate", st.LockedUntil)
	}
	x.now = x.now.Add(31 * time.Minute)
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := x.f.Count(t, `SELECT failed_attempts FROM local_accounts WHERE instance_id=$1`, x.f.InstanceID); n != 0 {
		t.Fatal("counter not reset")
	}
	if _, err = x.svc.Login(ctx, "nobody", goodPassword, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("unknown user must look like bad password")
	}
}

func TestTOTPEnrollmentAndEnforcement(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	principal, err := x.svc.BreakGlassPrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := x.svc.BeginTOTP(ctx, principal)
	if err != nil || !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatal(uri, err)
	}
	// Not enforced until confirmed.
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, "", ""); err != nil {
		t.Fatal("unconfirmed TOTP must not be enforced", err)
	}
	secret := mustSecret(t, x, principal)
	step := x.now.Unix() / totpPeriod
	if err = x.svc.ConfirmTOTP(ctx, principal, "000000"); err == nil {
		t.Fatal("wrong confirm code accepted")
	}
	if err = x.svc.ConfirmTOTP(ctx, principal, totpCode(secret, step)); err != nil {
		t.Fatal(err)
	}
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, "", ""); !errors.Is(err, ErrMFARequired) {
		t.Fatal("MFA not required", err)
	}
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, totpCode(secret, step), ""); err == nil {
		t.Fatal("confirmation code replayed at login")
	}
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, totpCode(secret, step+1), ""); err != nil {
		t.Fatal(err)
	}
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, totpCode(secret, step+1), ""); err == nil {
		t.Fatal("TOTP replay accepted")
	}
	// Reset removes MFA (lost authenticator recovery).
	if _, err = x.svc.ResetBreakGlass(ctx, "another long passphrase!"); err != nil {
		t.Fatal(err)
	}
	if _, err = x.svc.Login(ctx, "break-glass", "another long passphrase!", "", ""); err != nil {
		t.Fatal("reset must clear MFA", err)
	}
}

func mustSecret(t *testing.T, x *fixture, principal string) []byte {
	var id string
	if err := x.f.Pool.QueryRow(context.Background(), `SELECT totp_secret_id::text FROM local_accounts WHERE instance_id=$1`, x.f.InstanceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	_, secret, err := (x.svc).openSecret(context.Background(), id, principal)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestSSOMandatoryOnlyBreakGlassMayUseLocalLogin(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	hash, _ := HashPassword("second account passphrase")
	var p string
	if err := x.f.Pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'user','ops','local') RETURNING id::text`, x.f.InstanceID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.Pool.Exec(ctx, `INSERT INTO local_accounts(instance_id,principal_id,username,password_hash) VALUES($1,$2,'ops',$3)`, x.f.InstanceID, p, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.Login(ctx, "ops", "second account passphrase", "", ""); err != nil {
		t.Fatal("ordinary local login should work before policy", err)
	}
	if _, err := x.f.Pool.Exec(ctx, `INSERT INTO identity_policy(instance_id,sso_mandatory) VALUES($1,true)`, x.f.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err := x.svc.Login(ctx, "ops", "second account passphrase", "", ""); !errors.Is(err, ErrSSORequired) {
		t.Fatal("SSO mandatory not enforced", err)
	}
	if x.audits(t, "auth.local.denied", "denied") != 1 {
		t.Fatal("denial not audited")
	}
	if _, err := x.svc.Login(ctx, "break-glass", goodPassword, "", ""); err != nil {
		t.Fatal("break-glass must keep working", err)
	}
}

func TestBreakGlassResetRecovers(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	for i := 0; i < LockoutThreshold; i++ {
		_, _ = x.svc.Login(ctx, "break-glass", "wrong password here", "", "")
	}
	if _, err := x.svc.Login(ctx, "break-glass", goodPassword, "", ""); !errors.Is(err, ErrLocked) {
		t.Fatal("expected locked")
	}
	created, err := x.svc.ResetBreakGlass(ctx, "brand new passphrase 123")
	if err != nil || created {
		t.Fatal("reset", created, err)
	}
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, "", ""); err == nil {
		t.Fatal("old password still valid")
	}
	if _, err = x.svc.Login(ctx, "break-glass", "brand new passphrase 123", "", ""); err != nil {
		t.Fatal("reset did not clear lockout", err)
	}
	if x.audits(t, "auth.breakglass.reset", "success") != 1 || x.audits(t, "auth.breakglass.created", "success") != 1 {
		t.Fatal("create/reset not audited")
	}
	noKeys := &Service{DB: x.f.Pool, InstanceID: x.f.InstanceID}
	if _, err = noKeys.ResetBreakGlass(ctx, "brand new passphrase 123"); !errors.Is(err, ErrKeyring) {
		t.Fatal("reset must require the root keyring", err)
	}
}
