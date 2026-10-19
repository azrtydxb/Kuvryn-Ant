package oidclogin

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// ApplyClaimRoles applies a provider's claim-derived local role IDs for a
// verified OIDC identity (issuer+subject taken from the verified ID token).
//
//   - A subject with an explicit identity binding is never touched: explicit
//     bindings and memberships take precedence over claim mapping.
//   - A subject with no binding and at least one valid mapped role gets a user
//     principal and a claim-managed binding (just-in-time provisioning). An
//     existing principal with that issuer+subject is never taken over.
//   - A claim-managed subject has its claim-origin memberships reconciled to
//     exactly the mapped roles, so a role removed at the IdP is dropped at the
//     next login. Sessions already issued keep their roles until they expire
//     (at most the session lifetime).
//
// Only existing local roles are assigned. The audit trail records principal IDs
// only, never the issuer, subject or claim values.
func (s Sessions) ApplyClaimRoles(ctx context.Context, issuer, subject string, roleIDs []string) error {
	if issuer == "" || subject == "" || len(issuer) > 2048 || len(subject) > 2048 {
		return ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	var valid []string
	if len(roleIDs) > 0 {
		rows, e := tx.Query(ctx, `SELECT id::text FROM roles WHERE instance_id=$1 AND origin='local' AND id::text=ANY($2) ORDER BY id`, s.InstanceID, roleIDs)
		if e != nil {
			return ErrUnavailable
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) != nil {
				rows.Close()
				return ErrUnavailable
			}
			valid = append(valid, id)
		}
		rows.Close()
		if rows.Err() != nil {
			return ErrUnavailable
		}
	}
	var principal string
	var managed, created bool
	e = tx.QueryRow(ctx, `SELECT b.principal_id::text,b.claim_managed FROM oidc_identity_bindings b WHERE b.instance_id=$1 AND b.issuer=$2 AND b.subject=$3 FOR UPDATE`, s.InstanceID, issuer, subject).Scan(&principal, &managed)
	switch {
	case errors.Is(e, pgx.ErrNoRows):
		if len(valid) == 0 {
			return nil // unmapped and unbound: no provisioning, login is refused downstream
		}
		e = tx.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'user',$2,$3) ON CONFLICT DO NOTHING RETURNING id::text`, s.InstanceID, subject, issuer).Scan(&principal)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil // identity already owned by another principal: never taken over
		}
		if e != nil {
			return ErrUnavailable
		}
		if _, e = tx.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id,claim_managed) VALUES($1,$2,$3,$4,true)`, s.InstanceID, issuer, subject, principal); e != nil {
			return ErrUnavailable
		}
		created, managed = true, true
		if e = s.audit(ctx, tx, principal, principal, "browser.claim_user_provisioned"); e != nil {
			return e
		}
	case e != nil:
		return ErrUnavailable
	}
	if !managed {
		return nil
	}
	var disabled bool
	if e = tx.QueryRow(ctx, `SELECT disabled FROM principals WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.InstanceID, principal).Scan(&disabled); e != nil {
		return ErrUnavailable
	}
	if disabled {
		return nil
	}
	rows, e := tx.Query(ctx, `SELECT role_id::text FROM principal_roles WHERE instance_id=$1 AND principal_id=$2 AND origin='claim'`, s.InstanceID, principal)
	if e != nil {
		return ErrUnavailable
	}
	var current []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return ErrUnavailable
		}
		current = append(current, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return ErrUnavailable
	}
	changed := created
	for _, id := range current {
		if !slices.Contains(valid, id) {
			if _, e = tx.Exec(ctx, `DELETE FROM principal_roles WHERE instance_id=$1 AND principal_id=$2 AND role_id=$3 AND origin='claim'`, s.InstanceID, principal, id); e != nil {
				return ErrUnavailable
			}
			changed = true
		}
	}
	for _, id := range valid {
		if slices.Contains(current, id) {
			continue
		}
		tag, e := tx.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id,origin) VALUES($1,$2,$3,'claim') ON CONFLICT DO NOTHING`, s.InstanceID, principal, id)
		if e != nil {
			return ErrUnavailable
		}
		if tag.RowsAffected() > 0 {
			changed = true
		}
	}
	if changed {
		if _, e = tx.Exec(ctx, `UPDATE principals SET revision=revision+1 WHERE instance_id=$1 AND id=$2`, s.InstanceID, principal); e != nil {
			return ErrUnavailable
		}
		if e = s.audit(ctx, tx, principal, principal, "browser.claim_roles_applied"); e != nil {
			return e
		}
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}
