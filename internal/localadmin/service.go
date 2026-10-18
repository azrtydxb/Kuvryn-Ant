// Package localadmin implements the emergency (break-glass) local administrator:
// an argon2id-hashed password, per-account lockout, optional TOTP and the
// SSO-mandatory policy. It issues no sessions itself; callers pass the result
// to the browser session layer.
package localadmin

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/secrets"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Issuer and Subject identify the break-glass principal in the browser
	// session layer (oidc_identity_bindings).
	Issuer           = "ant-local"
	BreakGlassUser   = "break-glass"
	breakGlassRole   = "break-glass-administrator"
	LockoutThreshold = 5
	lockoutBase      = 15 * time.Minute
	lockoutMax       = 24 * time.Hour
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrLocked             = errors.New("account locked")
	ErrMFARequired        = errors.New("second factor required")
	ErrSSORequired        = errors.New("local sign-in disabled by policy")
	ErrUnavailable        = errors.New("local administrator unavailable")
	ErrNotFound           = errors.New("local account not found")
	ErrKeyring            = errors.New("root keyring required")
)

type Service struct {
	DB         *pgxpool.Pool
	Keys       *secrets.Keyring
	InstanceID string
	Now        func() time.Time
	// dummyHash equalizes timing for unknown usernames.
	dummyHash string
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Result is a successful authentication; Subject is the session-layer subject.
type Result struct {
	PrincipalID string
	// Subject is the session-layer subject (the username).
	Subject    string
	BreakGlass bool
}

type account struct {
	principal, hash string
	breakGlass      bool
	totpEnabled     bool
	totpSecret      *string
	lastStep        int64
	failed          int
	lockedUntil     *time.Time
}

func audit(ctx context.Context, tx pgx.Tx, instance, principal, action, result string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_events(instance_id,principal_id,action,result,details) VALUES($1,NULLIF($2,'')::uuid,$3,$4,$5)`, instance, principal, action, result, details)
	return err
}

// SSOMandatory reads the policy; absent row means local sign-in is allowed.
func SSOMandatory(ctx context.Context, db *pgxpool.Pool, instance string) (bool, error) {
	var v bool
	err := db.QueryRow(ctx, `SELECT sso_mandatory FROM identity_policy WHERE instance_id=$1`, instance).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return v, err
}

func lockoutFor(failed int) time.Duration {
	d := lockoutBase
	for i := LockoutThreshold + 1; i <= failed && d < lockoutMax; i++ {
		d *= 2
	}
	if d > lockoutMax {
		d = lockoutMax
	}
	return d
}

// Login authenticates a local account. Every outcome is audited. Failures and
// lockouts are persisted before returning. The same ErrInvalidCredentials is
// returned for unknown users and wrong passwords.
func (s *Service) Login(ctx context.Context, username, password, totp, peer string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	username = strings.ToLower(strings.TrimSpace(username))
	if s.DB == nil {
		return Result{}, ErrUnavailable
	}
	if s.dummyHash == "" {
		s.dummyHash, _ = HashPassword("not-a-real-password-for-timing")
	}
	if len(password) > MaxPassword || len(username) > 64 {
		return Result{}, ErrInvalidCredentials
	}
	mandatory, err := SSOMandatory(ctx, s.DB, s.InstanceID)
	if err != nil {
		return Result{}, ErrUnavailable
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Result{}, ErrUnavailable
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	var a account
	err = tx.QueryRow(ctx, `SELECT principal_id::text,password_hash,break_glass,totp_enabled,totp_secret_id::text,totp_last_step,failed_attempts,locked_until FROM local_accounts WHERE instance_id=$1 AND username=$2 FOR UPDATE`, s.InstanceID, username).
		Scan(&a.principal, &a.hash, &a.breakGlass, &a.totpEnabled, &a.totpSecret, &a.lastStep, &a.failed, &a.lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		VerifyPassword(s.dummyHash, password)
		_ = audit(ctx, tx, s.InstanceID, "", "auth.local.failure", "failure", map[string]any{"reason": "unknown_account"})
		_ = tx.Commit(ctx)
		return Result{}, ErrInvalidCredentials
	}
	if err != nil {
		return Result{}, ErrUnavailable
	}
	now := s.now()
	finish := func(action, result string, d map[string]any, e error) (Result, error) {
		if err := audit(ctx, tx, s.InstanceID, a.principal, action, result, d); err != nil {
			return Result{}, ErrUnavailable
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, ErrUnavailable
		}
		return Result{}, e
	}
	if mandatory && !a.breakGlass {
		return finish("auth.local.denied", "denied", map[string]any{"reason": "sso_mandatory"}, ErrSSORequired)
	}
	if a.lockedUntil != nil && a.lockedUntil.After(now) {
		return finish("auth.local.denied", "denied", map[string]any{"reason": "locked"}, ErrLocked)
	}
	fail := func(reason string) (Result, error) {
		a.failed++
		var locked *time.Time
		if a.failed >= LockoutThreshold {
			t := now.Add(lockoutFor(a.failed))
			locked = &t
		}
		if _, err := tx.Exec(ctx, `UPDATE local_accounts SET failed_attempts=$3,locked_until=$4 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, a.principal, a.failed, locked); err != nil {
			return Result{}, ErrUnavailable
		}
		d := map[string]any{"reason": reason, "failed_attempts": a.failed}
		if locked != nil {
			d["locked_until"] = locked.UTC().Format(time.RFC3339)
		}
		return finish("auth.local.failure", "failure", d, ErrInvalidCredentials)
	}
	if !VerifyPassword(a.hash, password) {
		return fail("bad_password")
	}
	if a.totpEnabled {
		if totp == "" {
			// Password was correct, so revealing the MFA requirement leaks nothing
			// an attacker holding the password could not learn. No attempt counted.
			return finish("auth.local.mfa_required", "denied", nil, ErrMFARequired)
		}
		secret, err := s.openTOTP(ctx, a)
		if err != nil {
			return Result{}, ErrUnavailable
		}
		step, ok := matchTOTP(secret, totp, now, a.lastStep)
		clear(secret)
		if !ok {
			return fail("bad_totp")
		}
		a.lastStep = step
	}
	if _, err := tx.Exec(ctx, `UPDATE local_accounts SET failed_attempts=0,locked_until=NULL,last_login_at=$3,totp_last_step=$4 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, a.principal, now, a.lastStep); err != nil {
		return Result{}, ErrUnavailable
	}
	action := "auth.local.success"
	if a.breakGlass {
		action = "auth.breakglass.login"
	}
	if err := audit(ctx, tx, s.InstanceID, a.principal, action, "success", map[string]any{"mfa": a.totpEnabled}); err != nil {
		return Result{}, ErrUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, ErrUnavailable
	}
	return Result{PrincipalID: a.principal, Subject: username, BreakGlass: a.breakGlass}, nil
}

func (s *Service) openTOTP(ctx context.Context, a account) ([]byte, error) {
	if a.totpSecret == nil || s.Keys == nil {
		return nil, ErrKeyring
	}
	_, secret, err := s.openSecret(ctx, *a.totpSecret, a.principal)
	return secret, err
}

func (s *Service) openSecret(ctx context.Context, id, principal string) (secrets.Record, []byte, error) {
	return (secrets.Store{DB: s.DB, Keys: s.Keys}).Open(ctx, s.InstanceID, id, "local-totp:"+principal)
}

// ResetBreakGlass creates or resets the break-glass account: new password,
// lockout cleared, TOTP removed, existing browser sessions revoked. It is the
// only way to recover the account and is invoked by the local CLI, which loads
// the root keyring from the host configuration before calling it.
func (s *Service) ResetBreakGlass(ctx context.Context, password string) (created bool, err error) {
	if s.Keys == nil {
		return false, ErrKeyring
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, ErrUnavailable
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	var principal string
	err = tx.QueryRow(ctx, `SELECT principal_id::text FROM local_accounts WHERE instance_id=$1 AND break_glass FOR UPDATE`, s.InstanceID).Scan(&principal)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		created = true
		if err = tx.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'emergency',$2,$3) ON CONFLICT(instance_id,issuer,subject) DO UPDATE SET disabled=false RETURNING id::text`, s.InstanceID, BreakGlassUser, Issuer).Scan(&principal); err != nil {
			return false, ErrUnavailable
		}
		var role string
		if err = tx.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,$2) ON CONFLICT(instance_id,name) DO UPDATE SET name=excluded.name RETURNING id::text`, s.InstanceID, breakGlassRole).Scan(&role); err != nil {
			return false, ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.InstanceID, principal, role); err != nil {
			return false, ErrUnavailable
		}
		var grants int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM role_grants WHERE instance_id=$1 AND role_id=$2`, s.InstanceID, role).Scan(&grants); err != nil {
			return false, ErrUnavailable
		}
		if grants == 0 {
			actions := []string{"read", "publish", "delete", "configure", "replicate", "admin"}
			for _, kind := range []string{"artifact", "feed", "source", "instance"} {
				if _, err = tx.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,$3,'allow',$4)`, s.InstanceID, role, kind, actions); err != nil {
					return false, ErrUnavailable
				}
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.InstanceID, Issuer, BreakGlassUser, principal); err != nil {
			return false, ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `INSERT INTO local_accounts(instance_id,principal_id,username,password_hash,break_glass) VALUES($1,$2,$3,$4,true)`, s.InstanceID, principal, BreakGlassUser, hash); err != nil {
			return false, ErrUnavailable
		}
	case err != nil:
		return false, ErrUnavailable
	default:
		if _, err = tx.Exec(ctx, `UPDATE local_accounts SET password_hash=$3,password_changed_at=now(),failed_attempts=0,locked_until=NULL,totp_enabled=false,totp_secret_id=NULL,totp_last_step=0 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal, hash); err != nil {
			return false, ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2 AND NOT EXISTS(SELECT 1 FROM local_accounts WHERE instance_id=$1 AND totp_secret_id=encrypted_secrets.id)`, s.InstanceID, "local-totp:"+principal); err != nil {
			return false, ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `UPDATE principals SET disabled=false WHERE instance_id=$1 AND id=$2`, s.InstanceID, principal); err != nil {
			return false, ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal); err != nil {
			return false, ErrUnavailable
		}
	}
	action := "auth.breakglass.reset"
	if created {
		action = "auth.breakglass.created"
	}
	if err = audit(ctx, tx, s.InstanceID, principal, action, "success", map[string]any{"channel": "cli"}); err != nil {
		return false, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return false, ErrUnavailable
	}
	return created, nil
}

// BreakGlassPrincipal returns the account's principal ID, or ErrNotFound.
func (s *Service) BreakGlassPrincipal(ctx context.Context) (string, error) {
	var id string
	err := s.DB.QueryRow(ctx, `SELECT principal_id::text FROM local_accounts WHERE instance_id=$1 AND break_glass`, s.InstanceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	return id, nil
}

// BeginTOTP generates and stores (encrypted) a pending secret for the account,
// replacing any previous unconfirmed enrollment. The code is not enforced until
// ConfirmTOTP succeeds. Returns the otpauth URI for authenticator apps.
func (s *Service) BeginTOTP(ctx context.Context, principal string) (string, error) {
	if s.Keys == nil {
		return "", ErrKeyring
	}
	secret, err := newTOTPSecret()
	if err != nil {
		return "", ErrUnavailable
	}
	defer clear(secret)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", ErrUnavailable
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	var username string
	var enabled bool
	if err = tx.QueryRow(ctx, `SELECT username,totp_enabled FROM local_accounts WHERE instance_id=$1 AND principal_id=$2 FOR UPDATE`, s.InstanceID, principal).Scan(&username, &enabled); err != nil {
		return "", ErrNotFound
	}
	if enabled {
		return "", errors.New("totp already enabled; disable it first")
	}
	// The generated purpose column derives from the principal, so the record
	// must be created with that purpose and linked afterwards.
	if _, err = tx.Exec(ctx, `UPDATE local_accounts SET totp_secret_id=NULL WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal); err != nil {
		return "", ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2`, s.InstanceID, "local-totp:"+principal); err != nil {
		return "", ErrUnavailable
	}
	rec, err := (secrets.Store{DB: s.DB, Keys: s.Keys}).CreateTx(ctx, tx, s.InstanceID, principal, "local-totp:"+principal, secret)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE local_accounts SET totp_secret_id=$3,totp_last_step=0 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal, rec.ID); err != nil {
		return "", ErrUnavailable
	}
	if err = audit(ctx, tx, s.InstanceID, principal, "auth.local.totp_enroll_started", "success", nil); err != nil {
		return "", ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return "", ErrUnavailable
	}
	return totpURI("Kuvryn Ant", username, secret), nil
}

// ConfirmTOTP enables TOTP once the caller proves possession with a valid code.
func (s *Service) ConfirmTOTP(ctx context.Context, principal, code string) error {
	var a account
	a.principal = principal
	err := s.DB.QueryRow(ctx, `SELECT totp_secret_id::text,totp_last_step,totp_enabled FROM local_accounts WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal).Scan(&a.totpSecret, &a.lastStep, &a.totpEnabled)
	if err != nil || a.totpSecret == nil || a.totpEnabled {
		return ErrNotFound
	}
	secret, err := s.openTOTP(ctx, a)
	if err != nil {
		return ErrUnavailable
	}
	defer clear(secret)
	step, ok := matchTOTP(secret, code, s.now(), a.lastStep)
	if !ok {
		return ErrInvalidCredentials
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if _, err = tx.Exec(ctx, `UPDATE local_accounts SET totp_enabled=true,totp_last_step=$3 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal, step); err != nil {
		return ErrUnavailable
	}
	if err = audit(ctx, tx, s.InstanceID, principal, "auth.local.totp_enabled", "success", nil); err != nil {
		return ErrUnavailable
	}
	return tx.Commit(ctx)
}

// DisableTOTP removes the second factor; callers must authorize this.
func (s *Service) DisableTOTP(ctx context.Context, principal string) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if _, err = tx.Exec(ctx, `UPDATE local_accounts SET totp_enabled=false,totp_secret_id=NULL,totp_last_step=0 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal); err != nil {
		return ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2`, s.InstanceID, "local-totp:"+principal); err != nil {
		return ErrUnavailable
	}
	if err = audit(ctx, tx, s.InstanceID, principal, "auth.local.totp_disabled", "success", nil); err != nil {
		return ErrUnavailable
	}
	return tx.Commit(ctx)
}

// AccountStatus is the console view of the break-glass account.
type AccountStatus struct {
	Exists      bool       `json:"exists"`
	Username    string     `json:"username,omitempty"`
	TOTPEnabled bool       `json:"totp_enabled"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
	PrincipalID string     `json:"principal_id,omitempty"`
}

func (s *Service) Status(ctx context.Context) (AccountStatus, error) {
	var st AccountStatus
	err := s.DB.QueryRow(ctx, `SELECT username,totp_enabled,CASE WHEN locked_until>now() THEN locked_until END,last_login_at,principal_id::text FROM local_accounts WHERE instance_id=$1 AND break_glass`, s.InstanceID).
		Scan(&st.Username, &st.TOTPEnabled, &st.LockedUntil, &st.LastLoginAt, &st.PrincipalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountStatus{}, nil
	}
	if err != nil {
		return AccountStatus{}, ErrUnavailable
	}
	st.Exists = true
	return st, nil
}

// Unlock clears the lockout counter (administrator action, audited by caller).
func (s *Service) Unlock(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `UPDATE local_accounts SET failed_attempts=0,locked_until=NULL WHERE instance_id=$1 AND break_glass`, s.InstanceID)
	return err
}
