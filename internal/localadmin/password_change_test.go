package localadmin

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestChangeOwnPassword(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	const pw, next = "another long enough password", "a brand new long password"
	u, err := x.svc.CreateUser(ctx, "", "carol", pw, false)
	if err != nil {
		t.Fatal(err)
	}
	// Three sessions; the one making the change must survive.
	for _, v := range []string{"keep", "other1", "other2"} {
		if _, err = x.f.Pool.Exec(ctx, `INSERT INTO browser_sessions(verifier,csrf_hash,instance_id,issuer,subject,principal_id,expires_at,idle_seconds) VALUES(sha256($1::bytea),sha256('c'::bytea),$2,$3,'carol',$4,clock_timestamp()+interval '1 hour',1800)`, v, x.f.InstanceID, Issuer, u.PrincipalID); err != nil {
			t.Fatal(err)
		}
	}
	keep := sha(t, x, "keep")

	if _, err = x.svc.ChangeOwnPassword(ctx, u.PrincipalID, pw+"x", next, keep); !errors.Is(err, ErrWrongPassword) {
		t.Fatal("wrong current password accepted", err)
	}
	if x.audits(t, "auth.local.password_change_failed", "failure") != 1 {
		t.Fatal("failure not audited")
	}
	if _, err = x.svc.ChangeOwnPassword(ctx, u.PrincipalID, pw, "short", keep); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatal("policy not enforced", err)
	}
	if _, err = x.svc.ChangeOwnPassword(ctx, u.PrincipalID, pw, pw, keep); !errors.Is(err, ErrSamePassword) {
		t.Fatal("same password accepted", err)
	}
	if n := x.f.Count(t, `SELECT count(*) FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`, x.f.InstanceID, u.PrincipalID); n != 3 {
		t.Fatal("failed attempts must not end sessions", n)
	}
	n, err := x.svc.ChangeOwnPassword(ctx, u.PrincipalID, pw, next, keep)
	if err != nil || n != 2 {
		t.Fatal("change failed", n, err)
	}
	if x.f.Count(t, `SELECT count(*) FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2 AND verifier=$3`, x.f.InstanceID, u.PrincipalID, keep) != 1 ||
		x.f.Count(t, `SELECT count(*) FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`, x.f.InstanceID, u.PrincipalID) != 1 {
		t.Fatal("only the current session must remain")
	}
	if x.audits(t, "auth.local.password_changed", "success") != 1 {
		t.Fatal("success not audited")
	}
	if _, err = x.svc.Login(ctx, "carol", pw, "", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password still works")
	}
	if _, err = x.svc.Login(ctx, "carol", next, "", ""); err != nil {
		t.Fatal("new password does not work", err)
	}
}

func TestChangeOwnPasswordRateLimit(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	const pw = "another long enough password"
	u, err := x.svc.CreateUser(ctx, "", "dave", pw, false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < LockoutThreshold; i++ {
		if _, err = x.svc.ChangeOwnPassword(ctx, u.PrincipalID, "wrong password value", "a brand new long password", nil); !errors.Is(err, ErrWrongPassword) {
			t.Fatal(err)
		}
	}
	// Locked: even the right current password is refused until the lock expires.
	if _, err = x.svc.ChangeOwnPassword(ctx, u.PrincipalID, pw, "a brand new long password", nil); !errors.Is(err, ErrLocked) {
		t.Fatal("lockout not enforced", err)
	}
	x.now = x.now.Add(16 * time.Minute)
	if _, err = x.svc.ChangeOwnPassword(ctx, u.PrincipalID, pw, "a brand new long password", nil); err != nil {
		t.Fatal("lock should have expired", err)
	}
	if _, err = x.svc.ChangeOwnPassword(ctx, "00000000-0000-0000-0000-000000000000", pw, "a brand new long password", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown principal", err)
	}
}

func sha(t *testing.T, x *fixture, v string) []byte {
	t.Helper()
	var b []byte
	if err := x.f.Pool.QueryRow(context.Background(), `SELECT sha256($1::bytea)`, v).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}
