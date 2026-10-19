package localadmin

import (
	"context"
	"errors"
	"testing"
)

func TestLocalUserLifecycle(t *testing.T) {
	x := newFixture(t)
	ctx := context.Background()
	const pw = "another long enough password"

	if _, err := x.svc.CreateUser(ctx, "", "Bad Name", pw, false); !errors.Is(err, ErrUsername) {
		t.Fatal("bad username accepted", err)
	}
	if _, err := x.svc.CreateUser(ctx, "", BreakGlassUser, pw, false); !errors.Is(err, ErrReserved) {
		t.Fatal("break-glass name reusable", err)
	}
	if _, err := x.svc.CreateUser(ctx, "", "alice", "short", false); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatal("weak password accepted", err)
	}
	u, err := x.svc.CreateUser(ctx, "", "alice", pw, true)
	if err != nil || u.PrincipalID == "" {
		t.Fatal(err)
	}
	if _, err = x.svc.CreateUser(ctx, "", "alice", pw, false); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate accepted", err)
	}
	// Stored as argon2id, never plaintext.
	var hash string
	if err = x.f.Pool.QueryRow(ctx, `SELECT password_hash FROM local_accounts WHERE instance_id=$1 AND username='alice'`, x.f.InstanceID).Scan(&hash); err != nil || hash == pw || len(hash) < 40 || hash[:10] != "$argon2id$" {
		t.Fatal("password not hashed", err)
	}
	// Administrator role attached with instance admin grant.
	if n := x.f.Count(t, `SELECT count(*) FROM principal_roles pr JOIN roles r ON r.id=pr.role_id JOIN role_grants g ON g.role_id=r.id WHERE pr.instance_id=$1 AND pr.principal_id=$2 AND r.name=$3 AND g.kind='instance' AND 'admin'=ANY(g.actions)`, x.f.InstanceID, u.PrincipalID, AdministratorRole); n != 1 {
		t.Fatal("administrator role not granted", n)
	}

	res, err := x.svc.Login(ctx, "alice", pw, "", "203.0.113.5")
	if err != nil || res.BreakGlass || res.PrincipalID != u.PrincipalID || res.Subject != "alice" {
		t.Fatal("local user login", err, res)
	}
	if _, err = x.svc.Login(ctx, "alice", pw+"x", "", "203.0.113.5"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("wrong password accepted", err)
	}

	// Lockout applies to ordinary accounts too.
	for i := 0; i < LockoutThreshold; i++ {
		_, _ = x.svc.Login(ctx, "alice", "wrong password value", "", "203.0.113.5")
	}
	if _, err = x.svc.Login(ctx, "alice", pw, "", "203.0.113.5"); !errors.Is(err, ErrLocked) {
		t.Fatal("account not locked", err)
	}

	// Password reset clears the lockout and revokes sessions; the old password stops working.
	if err = x.svc.SetUserPassword(ctx, "", "alice", "a brand new password!"); err != nil {
		t.Fatal(err)
	}
	if _, err = x.svc.Login(ctx, "alice", pw, "", "203.0.113.5"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password still valid", err)
	}
	if _, err = x.svc.Login(ctx, "alice", "a brand new password!", "", "203.0.113.5"); err != nil {
		t.Fatal(err)
	}
	if err = x.svc.SetUserPassword(ctx, "", BreakGlassUser, "a brand new password!"); !errors.Is(err, ErrReserved) {
		t.Fatal("break-glass reset through user API", err)
	}
	if err = x.svc.SetUserPassword(ctx, "", "nobody", "a brand new password!"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	users, err := x.svc.ListUsers(ctx)
	if err != nil || len(users) != 2 || !users[0].BreakGlass || users[1].Username != "alice" {
		t.Fatal("list", err, users)
	}

	// SSO mandatory blocks ordinary local users but not break-glass.
	if _, err = x.f.Pool.Exec(ctx, `INSERT INTO identity_policy(instance_id,sso_mandatory) VALUES($1,true)`, x.f.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err = x.svc.Login(ctx, "alice", "a brand new password!", "", "203.0.113.5"); !errors.Is(err, ErrSSORequired) {
		t.Fatal("sso mandatory not enforced", err)
	}
	if _, err = x.svc.Login(ctx, "break-glass", goodPassword, "", "203.0.113.6"); err != nil {
		t.Fatal(err)
	}
	if _, err = x.f.Pool.Exec(ctx, `DELETE FROM identity_policy WHERE instance_id=$1`, x.f.InstanceID); err != nil {
		t.Fatal(err)
	}

	if err = x.svc.DeleteUser(ctx, "", BreakGlassUser); !errors.Is(err, ErrReserved) {
		t.Fatal("break-glass deletable", err)
	}
	if err = x.svc.DeleteUser(ctx, "", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = x.svc.Login(ctx, "alice", "a brand new password!", "", "203.0.113.5"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("deleted account still signs in", err)
	}
	if _, err = x.svc.CreateUser(ctx, "", "alice", pw, false); !errors.Is(err, ErrExists) {
		t.Fatal("deleted username reusable (would inherit old grants)", err)
	}
	if n := x.f.Count(t, `SELECT count(*) FROM oidc_identity_bindings WHERE instance_id=$1 AND issuer=$2 AND subject='alice'`, x.f.InstanceID, Issuer); n != 0 {
		t.Fatal("binding left behind")
	}
	for _, a := range []string{"auth.local.user_created", "auth.local.password_set", "auth.local.user_deleted"} {
		if x.audits(t, a, "success") < 1 {
			t.Fatal("not audited", a)
		}
	}
}
