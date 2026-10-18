package oidclogin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func secret() string { return base64.RawURLEncoding.EncodeToString(randomBytes()) }
func randomBytes() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure randomness unavailable")
	}
	return b
}
func hash(s string) []byte            { h := sha256.Sum256([]byte(s)); return h[:] }
func matches(s string, h []byte) bool { return subtle.ConstantTimeCompare(hash(s), h) == 1 }
func validSecret(s string) bool {
	b, e := base64.RawURLEncoding.DecodeString(s)
	return len(s) == 43 && e == nil && len(b) == 32
}

type Sessions struct {
	DB                  *pgxpool.Pool
	InstanceID          string
	MaxAge, IdleTimeout time.Duration
}
type Session struct {
	PrincipalID, InstanceID string
	ExpiresAt               time.Time
}

func (s Sessions) limits() (time.Duration, time.Duration, error) {
	max, idle := s.MaxAge, s.IdleTimeout
	if max == 0 {
		max = 8 * time.Hour
	}
	if idle == 0 {
		idle = 30 * time.Minute
	}
	if max < time.Second || max > 8*time.Hour || idle < time.Second || idle > 30*time.Minute {
		return 0, 0, errors.New("invalid browser session limits")
	}
	return max, idle, nil
}
func (s Sessions) begin(ctx context.Context, p *Provider, state, browser, nonce string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, e := s.DB.Exec(ctx, `INSERT INTO oidc_login_states(state_hash,instance_id,provider_key,browser_hash,nonce_hash) VALUES($1,$2,$3,$4,$5)`, hash(state), s.InstanceID, p.key, hash(browser), hash(nonce))
	if e != nil {
		return ErrUnavailable
	}
	return nil
}
func (s Sessions) consume(ctx context.Context, p *Provider, state, browser string) ([]byte, error) {
	if !validSecret(state) || !validSecret(browser) {
		return nil, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var nonce []byte
	e := s.DB.QueryRow(ctx, `DELETE FROM oidc_login_states WHERE state_hash=$1 AND instance_id=$2 AND provider_key=$3 AND browser_hash=$4 AND expires_at>clock_timestamp() RETURNING nonce_hash`, hash(state), s.InstanceID, p.key, hash(browser)).Scan(&nonce)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, ErrAuthentication
	}
	if e != nil {
		return nil, ErrUnavailable
	}
	return nonce, nil
}

// BindIdentity is privileged INTERNAL bootstrap/setup only; never expose it as an API.
// Administrative APIs must use BindIdentityAs with an authenticated caller. It binds immutable
// provider issuer+subject to an existing user; it never creates users or roles.
// Rebinding an existing identity is rejected; revoke its binding explicitly first.
func (s Sessions) BindIdentity(ctx context.Context, issuer, subject, principal string) error {
	return s.bind(ctx, "", issuer, subject, principal)
}
func (s Sessions) bind(ctx context.Context, actor, issuer, subject, principal string) error {
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
	if actor != "" {
		var active bool
		if e = tx.QueryRow(ctx, "SELECT NOT disabled FROM principals WHERE instance_id=$1 AND id=$2 FOR SHARE", s.InstanceID, actor).Scan(&active); e != nil || !active {
			return ErrAuthentication
		}
	}
	tag, e := tx.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) SELECT instance_id,$2,$3,id FROM principals WHERE instance_id=$1 AND id=$4 AND kind='user' AND NOT disabled`, s.InstanceID, issuer, subject, principal)
	if e != nil {
		return ErrUnavailable
	}
	if tag.RowsAffected() != 1 {
		return ErrAuthentication
	}
	if e = s.audit(ctx, tx, actor, principal, "browser.binding_created"); e != nil {
		return e
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}

// IssueExternal is trusted server-side only. Invoke after complete SAML/LDAP
// verification using immutable issuer+subject (never an email or browser input).
// It cannot provision accounts, memberships or roles.
func (s Sessions) IssueExternal(ctx context.Context, issuer, subject string) (Session, string, string, error) {
	return s.issueGroups(ctx, issuer, subject, nil)
}

// IssueExternalGroups is IssueExternal with the verified group IDs of the
// identity (SAML Groups, LDAP immutable group IDs). Groups only feed the
// federation sign-in hook (federated group bindings); they never grant local
// memberships.
func (s Sessions) IssueExternalGroups(ctx context.Context, issuer, subject string, groups []string) (Session, string, string, error) {
	return s.issueGroups(ctx, issuer, subject, groups)
}

// LoginHook runs before a session is issued for a verified external identity.
// The federation service uses it to materialize federated principals and
// evaluate group bindings (P2-M2). Its errors never block local sign-in.
type LoginHook interface {
	ExternalLogin(ctx context.Context, issuer, subject string, groups []string) error
}

type hookBox struct{ h LoginHook }

var loginHook atomic.Pointer[hookBox]

// SetLoginHook installs (or, with nil, removes) the process-wide sign-in hook.
func SetLoginHook(h LoginHook) {
	if h == nil {
		loginHook.Store(nil)
		return
	}
	loginHook.Store(&hookBox{h})
}

func (s Sessions) issue(ctx context.Context, issuer, subject string) (Session, string, string, error) {
	return s.issueGroups(ctx, issuer, subject, nil)
}
func (s Sessions) issueGroups(ctx context.Context, issuer, subject string, groups []string) (Session, string, string, error) {
	if b := loginHook.Load(); b != nil {
		hookCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = b.h.ExternalLogin(hookCtx, issuer, subject, groups)
		cancel()
	}
	max, idle, e := s.limits()
	if e != nil {
		return Session{}, "", "", e
	}
	raw, csrf := secret(), secret()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return Session{}, "", "", ErrUnavailable
	}
	defer rollback(tx)
	var result Session
	e = tx.QueryRow(ctx, `INSERT INTO browser_sessions(verifier,csrf_hash,instance_id,issuer,subject,principal_id,created_at,last_seen_at,expires_at,idle_seconds)
 SELECT $1,$2,b.instance_id,b.issuer,b.subject,b.principal_id,statement_timestamp(),statement_timestamp(),statement_timestamp()+$6*interval '1 second',$7
 FROM oidc_identity_bindings b JOIN principals p ON (p.instance_id,p.id)=(b.instance_id,b.principal_id)
 WHERE b.instance_id=$3 AND b.issuer=$4 AND b.subject=$5 AND NOT p.disabled AND p.kind IN ('user','emergency')
 RETURNING instance_id,principal_id,expires_at`, hash(raw), hash(csrf), s.InstanceID, issuer, subject, int64(max/time.Second), int64(idle/time.Second)).Scan(&result.InstanceID, &result.PrincipalID, &result.ExpiresAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return Session{}, "", "", ErrAuthentication
	}
	if e != nil {
		return Session{}, "", "", ErrUnavailable
	}
	if e = s.audit(ctx, tx, result.PrincipalID, result.PrincipalID, "browser.session_issued"); e != nil {
		return Session{}, "", "", e
	}
	if tx.Commit(ctx) != nil {
		return Session{}, "", "", ErrUnavailable
	}
	return result, raw, csrf, nil
}

// Authenticate atomically checks expiry, current binding, disabled status and
// (for mutations) CSRF before renewing inactivity. It grants no permissions.
func (s Sessions) Authenticate(ctx context.Context, raw, csrf string, mutation bool) (Session, error) {
	if !validSecret(raw) || mutation && !validSecret(csrf) {
		return Session{}, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result Session
	e := s.DB.QueryRow(ctx, `UPDATE browser_sessions s SET last_seen_at=clock_timestamp()
 FROM principals p,oidc_identity_bindings b WHERE s.verifier=$1 AND s.instance_id=$2
 AND (NOT $3 OR s.csrf_hash=$4) AND s.expires_at>clock_timestamp()
 AND s.last_seen_at+ s.idle_seconds*interval '1 second'>clock_timestamp()
 AND (p.instance_id,p.id)=(s.instance_id,s.principal_id) AND NOT p.disabled AND p.kind IN ('user','emergency')
 AND (b.instance_id,b.issuer,b.subject,b.principal_id)=(s.instance_id,s.issuer,s.subject,s.principal_id)
 RETURNING s.instance_id,s.principal_id,s.expires_at`, hash(raw), s.InstanceID, mutation, hash(csrf)).Scan(&result.InstanceID, &result.PrincipalID, &result.ExpiresAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return Session{}, ErrAuthentication
	}
	if e != nil {
		return Session{}, ErrUnavailable
	}
	return result, nil
}
func (s Sessions) Revoke(ctx context.Context, raw string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	var principal string
	e = tx.QueryRow(ctx, `DELETE FROM browser_sessions WHERE verifier=$1 AND instance_id=$2 RETURNING principal_id`, hash(raw), s.InstanceID).Scan(&principal)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil
	}
	if e != nil {
		return ErrUnavailable
	}
	if e = s.audit(ctx, tx, principal, principal, "browser.session_revoked"); e != nil {
		return e
	}
	if tx.Commit(ctx) != nil {
		return ErrUnavailable
	}
	return nil
}

// Cleanup removes at most 500 expired rows per table per invocation. Multiple
// workers skip locked rows; callers schedule another bounded pass later.
func (s Sessions) Cleanup(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, e := s.DB.Exec(ctx, `DELETE FROM oidc_login_states WHERE ctid IN (SELECT ctid FROM oidc_login_states WHERE instance_id=$1 AND expires_at<=clock_timestamp() LIMIT 500 FOR UPDATE SKIP LOCKED)`, s.InstanceID); e != nil {
		return ErrUnavailable
	}
	_, e := s.DB.Exec(ctx, `DELETE FROM browser_sessions WHERE ctid IN (SELECT ctid FROM browser_sessions WHERE instance_id=$1 AND (expires_at<=clock_timestamp() OR last_seen_at+idle_seconds*interval '1 second'<=clock_timestamp()) LIMIT 500 FOR UPDATE SKIP LOCKED)`, s.InstanceID)
	if e != nil {
		return ErrUnavailable
	}
	return nil
}
