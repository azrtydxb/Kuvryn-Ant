package oidclogin

import (
	"context"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/jackc/pgx/v5"
)

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// Events deliberately exclude issuer/subject, URLs, peer addresses, session
// verifiers, tokens and protocol input. Only internal principal IDs are retained.
func (s Sessions) audit(ctx context.Context, tx pgx.Tx, actor, target, action string) error {
	var id int64
	if e := tx.QueryRow(ctx, `INSERT INTO audit_events(instance_id,principal_id,resource_id,action) VALUES($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4) RETURNING id`, s.InstanceID, actor, target, action).Scan(&id); e != nil {
		return ErrUnavailable
	}
	if _, e := tx.Exec(ctx, `INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('principal_id',NULLIF($3,'')::text))`, id, action, target); e != nil {
		return ErrUnavailable
	}
	return nil
}

// RecordLoginFailure records a fixed, redacted event independently of request
// cancellation. Call only after a bounded attempt; never pass protocol data.
func (s Sessions) RecordLoginFailure(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	if e = s.audit(ctx, tx, "", "", "browser.login_failed"); e != nil {
		return e
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}

// BindIdentityAs accepts an authenticated caller, not an actor UUID. It resolves
// database-owned roles and enforces shared instance-admin policy. The caller
// must authenticate the Principal; possession of a UUID is never authentication.
func (s Sessions) BindIdentityAs(ctx context.Context, actor identity.Principal, issuer, subject, principal string) error {
	policy, e := (identity.Roles{DB: s.DB}).Resolve(ctx, actor)
	if e != nil {
		return ErrAuthentication
	}
	if !policy.Authorize(actor, identity.Resource{InstanceID: s.InstanceID, Kind: identity.InstanceResource}, identity.Admin) {
		return ErrAuthentication
	}
	return s.bind(ctx, actor.PrincipalID, issuer, subject, principal)
}
