package localadmin

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrWrongPassword: the current password did not verify. Counts toward the
	// account lockout exactly like a failed sign-in.
	ErrWrongPassword = errors.New("current password is incorrect")
	// ErrSamePassword: the new password equals the current one.
	ErrSamePassword = errors.New("new password must differ from the current password")
)

// ChangeOwnPassword lets a signed-in local user replace their own password.
//
// It verifies the current password in constant time, applies the password
// policy to the new one, and in one transaction stores the new hash, resets the
// failure counter and revokes every browser session of the account except the
// one whose verifier hash is keep (the session making the change stays signed
// in; pass nil to revoke all). A wrong current password counts as a failed
// attempt: after LockoutThreshold failures the account is locked with the same
// escalating backoff as sign-in, which is the rate limit. Every outcome is
// audited. TOTP, if enrolled, is unaffected.
func (s *Service) ChangeOwnPassword(ctx context.Context, principal, current, next string, keep []byte) (revoked int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if s.DB == nil {
		return 0, ErrUnavailable
	}
	if len(current) == 0 || len(current) > MaxPassword {
		return 0, ErrWrongPassword
	}
	if err = ValidatePassword(next); err != nil {
		return 0, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, ErrUnavailable
	}
	defer rollbackTx(ctx, tx)
	var a account
	err = tx.QueryRow(ctx, `SELECT password_hash,failed_attempts,locked_until FROM local_accounts WHERE instance_id=$1 AND principal_id=$2 FOR UPDATE`, s.InstanceID, principal).Scan(&a.hash, &a.failed, &a.lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, ErrUnavailable
	}
	now := s.now()
	if a.lockedUntil != nil && a.lockedUntil.After(now) {
		_ = audit(ctx, tx, s.InstanceID, principal, "auth.local.password_change_denied", "denied", map[string]any{"reason": "locked"})
		_ = tx.Commit(ctx)
		return 0, ErrLocked
	}
	if !VerifyPassword(a.hash, current) {
		a.failed++
		var locked *time.Time
		if a.failed >= LockoutThreshold {
			t := now.Add(lockoutFor(a.failed))
			locked = &t
		}
		if _, err = tx.Exec(ctx, `UPDATE local_accounts SET failed_attempts=$3,locked_until=$4 WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal, a.failed, locked); err != nil {
			return 0, ErrUnavailable
		}
		d := map[string]any{"reason": "bad_password", "failed_attempts": a.failed}
		if locked != nil {
			d["locked_until"] = locked.UTC().Format(time.RFC3339)
		}
		if err = audit(ctx, tx, s.InstanceID, principal, "auth.local.password_change_failed", "failure", d); err != nil {
			return 0, ErrUnavailable
		}
		if err = tx.Commit(ctx); err != nil {
			return 0, ErrUnavailable
		}
		return 0, ErrWrongPassword
	}
	if current == next {
		return 0, ErrSamePassword
	}
	hash, err := HashPassword(next)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE local_accounts SET password_hash=$3,password_changed_at=now(),failed_attempts=0,locked_until=NULL WHERE instance_id=$1 AND principal_id=$2`, s.InstanceID, principal, hash); err != nil {
		return 0, ErrUnavailable
	}
	tag, err := tx.Exec(ctx, `DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2 AND ($3::bytea IS NULL OR verifier<>$3)`, s.InstanceID, principal, keep)
	if err != nil {
		return 0, ErrUnavailable
	}
	revoked = int(tag.RowsAffected())
	if err = audit(ctx, tx, s.InstanceID, principal, "auth.local.password_changed", "success", map[string]any{"sessions_revoked": revoked}); err != nil {
		return 0, ErrUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, ErrUnavailable
	}
	return revoked, nil
}
