package localadmin

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// AdministratorRole is the role the CLI attaches for `--admin` local accounts. It
// carries the same grants as the break-glass role but is a separate role so the
// emergency account's grants can be audited on their own.
const AdministratorRole = "local-administrator"

var (
	ErrUsername = errors.New("username must match ^[a-z0-9][a-z0-9._-]{0,62}$")
	ErrExists   = errors.New("a principal with that username already exists")
	ErrReserved = errors.New("the break-glass account is managed with `ant admin break-glass`")
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// User is the administrative view of a local account.
type User struct {
	Username    string     `json:"username"`
	PrincipalID string     `json:"principal_id"`
	BreakGlass  bool       `json:"break_glass"`
	TOTPEnabled bool       `json:"totp_enabled"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func validUsername(u string) bool { return usernamePattern.MatchString(u) }

// ensureAdminRole creates (idempotently) a role with full grants on every
// resource kind and returns its ID.
func ensureAdminRole(ctx context.Context, tx pgx.Tx, instance, name string) (string, error) {
	var role string
	if err := tx.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,$2) ON CONFLICT(instance_id,name) DO UPDATE SET name=excluded.name RETURNING id::text`, instance, name).Scan(&role); err != nil {
		return "", err
	}
	var grants int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM role_grants WHERE instance_id=$1 AND role_id=$2`, instance, role).Scan(&grants); err != nil {
		return "", err
	}
	if grants == 0 {
		actions := []string{"read", "publish", "delete", "configure", "replicate", "admin"}
		for _, kind := range []string{"artifact", "feed", "source", "instance"} {
			if _, err := tx.Exec(ctx, `INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) VALUES($1,$2,$3,'allow',$4)`, instance, role, kind, actions); err != nil {
				return "", err
			}
		}
	}
	return role, nil
}

// CreateUser creates a local sign-in account: a `user` principal, its
// ant-local identity binding (so the browser session layer can resolve it) and
// an argon2id-hashed password. With administrator it also attaches
// AdministratorRole. actor is the administering principal ("" for the host CLI).
// Roles are otherwise assigned through the principal roles API.
func (s *Service) CreateUser(ctx context.Context, actor, username, password string, administrator bool) (User, error) {
	if !validUsername(username) {
		return User{}, ErrUsername
	}
	if username == BreakGlassUser {
		return User{}, ErrReserved
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return User{}, ErrUnavailable
	}
	defer rollbackTx(ctx, tx)
	var principal string
	err = tx.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'user',$2,$3) ON CONFLICT DO NOTHING RETURNING id::text`, s.InstanceID, username, Issuer).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrExists
	}
	if err != nil {
		return User{}, ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,$2,$3,$4)`, s.InstanceID, Issuer, username, principal); err != nil {
		return User{}, ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `INSERT INTO local_accounts(instance_id,principal_id,username,password_hash,break_glass) VALUES($1,$2,$3,$4,false)`, s.InstanceID, principal, username, hash); err != nil {
		return User{}, ErrUnavailable
	}
	if administrator {
		role, err := ensureAdminRole(ctx, tx, s.InstanceID, AdministratorRole)
		if err != nil {
			return User{}, ErrUnavailable
		}
		if _, err = tx.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, s.InstanceID, principal, role); err != nil {
			return User{}, ErrUnavailable
		}
	}
	if err = audit(ctx, tx, s.InstanceID, actor, "auth.local.user_created", "success", map[string]any{"username": username, "principal_id": principal, "administrator": administrator}); err != nil {
		return User{}, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return User{}, ErrUnavailable
	}
	return User{Username: username, PrincipalID: principal}, nil
}

// SetUserPassword replaces a (non break-glass) account's password, clears its
// lockout and revokes its browser sessions. TOTP, if enrolled, stays.
func (s *Service) SetUserPassword(ctx context.Context, actor, username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer rollbackTx(ctx, tx)
	var principal string
	var breakGlass bool
	err = tx.QueryRow(ctx, `SELECT principal_id::text,break_glass FROM local_accounts WHERE instance_id=$1 AND username=$2 FOR UPDATE`, s.InstanceID, username).Scan(&principal, &breakGlass)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrUnavailable
	}
	if breakGlass {
		return ErrReserved
	}
	if _, err = tx.Exec(ctx, `UPDATE local_accounts SET password_hash=$3,password_changed_at=now(),failed_attempts=0,locked_until=NULL WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal, hash); err != nil {
		return ErrUnavailable
	}
	if _, err = tx.Exec(ctx, `DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal); err != nil {
		return ErrUnavailable
	}
	if err = audit(ctx, tx, s.InstanceID, actor, "auth.local.password_set", "success", map[string]any{"username": username, "principal_id": principal}); err != nil {
		return ErrUnavailable
	}
	return tx.Commit(ctx)
}

// DeleteUser removes a (non break-glass) account's ability to sign in: the
// password, the ant-local binding and browser sessions go, and the principal is
// disabled (kept for audit integrity and its username is not reusable).
func (s *Service) DeleteUser(ctx context.Context, actor, username string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer rollbackTx(ctx, tx)
	var principal string
	var breakGlass bool
	var totp *string
	err = tx.QueryRow(ctx, `SELECT principal_id::text,break_glass,totp_secret_id::text FROM local_accounts WHERE instance_id=$1 AND username=$2 FOR UPDATE`, s.InstanceID, username).Scan(&principal, &breakGlass, &totp)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrUnavailable
	}
	if breakGlass {
		return ErrReserved
	}
	for _, q := range []string{
		`DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`,
		`DELETE FROM local_accounts WHERE instance_id=$1 AND principal_id=$2`,
		`DELETE FROM oidc_identity_bindings WHERE instance_id=$1 AND issuer='` + Issuer + `' AND principal_id=$2`,
		`UPDATE principals SET disabled=true WHERE instance_id=$1 AND id=$2`,
	} {
		if _, err = tx.Exec(ctx, q, s.InstanceID, principal); err != nil {
			return ErrUnavailable
		}
	}
	if totp != nil {
		if _, err = tx.Exec(ctx, `DELETE FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2`, s.InstanceID, "local-totp:"+principal); err != nil {
			return ErrUnavailable
		}
	}
	if err = audit(ctx, tx, s.InstanceID, actor, "auth.local.user_deleted", "success", map[string]any{"username": username, "principal_id": principal}); err != nil {
		return ErrUnavailable
	}
	return tx.Commit(ctx)
}

// ListUsers returns every local account, break-glass first.
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.Query(ctx, `SELECT username,principal_id::text,break_glass,totp_enabled,CASE WHEN locked_until>now() THEN locked_until END,last_login_at FROM local_accounts WHERE instance_id=$1 ORDER BY break_glass DESC,username`, s.InstanceID)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err = rows.Scan(&u.Username, &u.PrincipalID, &u.BreakGlass, &u.TOTPEnabled, &u.LockedUntil, &u.LastLoginAt); err != nil {
			return nil, ErrUnavailable
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		return nil, ErrUnavailable
	}
	return out, nil
}

func rollbackTx(ctx context.Context, tx pgx.Tx) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(c)
}

// UnlockUser clears one account's failed-attempt counter and lockout without
// touching its password (administrator action). Unlike a password reset the
// user keeps their credentials, which is the right fix for a typo storm.
func (s *Service) UnlockUser(ctx context.Context, actor, username string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer rollbackTx(ctx, tx)
	var principal string
	err = tx.QueryRow(ctx, `UPDATE local_accounts SET failed_attempts=0,locked_until=NULL WHERE instance_id=$1 AND username=$2 RETURNING principal_id::text`, s.InstanceID, username).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrUnavailable
	}
	if err = audit(ctx, tx, s.InstanceID, actor, "auth.local.unlocked", "success", map[string]any{"username": username, "principal_id": principal}); err != nil {
		return ErrUnavailable
	}
	return tx.Commit(ctx)
}

// ResetUserTOTP removes a (non break-glass) account's second factor so a user
// who lost their device can sign in and enrol again. Their browser sessions
// are revoked. The break-glass account manages its own factor.
func (s *Service) ResetUserTOTP(ctx context.Context, actor, username string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer rollbackTx(ctx, tx)
	var principal string
	var breakGlass bool
	err = tx.QueryRow(ctx, `SELECT principal_id::text,break_glass FROM local_accounts WHERE instance_id=$1 AND username=$2 FOR UPDATE`, s.InstanceID, username).Scan(&principal, &breakGlass)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return ErrUnavailable
	}
	if breakGlass {
		return ErrReserved
	}
	for _, q := range []string{
		`UPDATE local_accounts SET totp_enabled=false,totp_secret_id=NULL,totp_last_step=0 WHERE instance_id=$1 AND principal_id=$2`,
		`DELETE FROM encrypted_secrets WHERE instance_id=$1 AND purpose='local-totp:'||$2::text`,
		`DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2`,
	} {
		if _, err = tx.Exec(ctx, q, s.InstanceID, principal); err != nil {
			return ErrUnavailable
		}
	}
	if err = audit(ctx, tx, s.InstanceID, actor, "auth.local.totp_reset", "success", map[string]any{"username": username, "principal_id": principal}); err != nil {
		return ErrUnavailable
	}
	return tx.Commit(ctx)
}

// Account returns the local account behind a principal, if it has one.
func (s *Service) Account(ctx context.Context, principal string) (User, bool, error) {
	var u User
	err := s.DB.QueryRow(ctx, `SELECT username,principal_id::text,break_glass,totp_enabled,CASE WHEN locked_until>now() THEN locked_until END,last_login_at FROM local_accounts WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal).
		Scan(&u.Username, &u.PrincipalID, &u.BreakGlass, &u.TOTPEnabled, &u.LockedUntil, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, ErrUnavailable
	}
	return u, true, nil
}
