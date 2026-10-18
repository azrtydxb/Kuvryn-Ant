// Package identity provides scoped machine credentials and shared authorization.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnauthenticated = errors.New("invalid or inactive credential")
var ErrInvalidScope = errors.New("invalid token scope")
var ErrUnavailable = errors.New("identity storage unavailable")

type Action string

const (
	Read      Action = "read"
	Publish   Action = "publish"
	Delete    Action = "delete"
	Configure Action = "configure"
	Replicate Action = "replicate"
	Admin     Action = "admin"
)

type Scope struct {
	FeedID    string   `json:"feed_id,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Actions   []Action `json:"actions"`
}
type Principal struct {
	InstanceID  string
	PrincipalID string
	TokenID     string
	Scopes      []Scope
	// Anonymous marks the instance's internal anonymous-reader principal. It is
	// only ever set by server code for an unauthenticated read of a feed with
	// anonymous_read enabled, never derived from a request or token, and
	// Roles.Resolve grants it Read on those feeds only.
	Anonymous bool
	// ticket marks a principal minted by the client-authentication middleware
	// after it verified a credential against the feed policy. Only that
	// middleware can set it (via a single-use in-process ticket).
	ticket bool
}
type IssuedToken struct {
	ID, Secret string
	ExpiresAt  time.Time
}
type Tokens struct{ DB *pgxpool.Pool }

func validScopes(scopes []Scope) bool {
	if len(scopes) == 0 || len(scopes) > 64 {
		return false
	}
	for _, s := range scopes {
		if len(s.Namespace) > 200 || len(s.Actions) == 0 || len(s.Actions) > 6 {
			return false
		}
		if s.FeedID != "" {
			var id pgtype.UUID
			if id.Scan(s.FeedID) != nil || !id.Valid {
				return false
			}
		}
		seen := map[Action]bool{}
		for _, a := range s.Actions {
			if (a != Read && a != Publish && a != Delete && a != Configure && a != Replicate && a != Admin) || seen[a] {
				return false
			}
			seen[a] = true
		}
	}
	return true
}

// Issue requires prior authorization of actor and requested scopes by the caller.
// It returns secret material once; never route this through response caching.
func (t Tokens) Issue(ctx context.Context, instance, actor, principal string, scopes []Scope, expires time.Time) (IssuedToken, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !validScopes(scopes) {
		return IssuedToken{}, ErrInvalidScope
	}
	if !expires.After(time.Now()) || expires.After(time.Now().Add(365*24*time.Hour)) {
		return IssuedToken{}, errors.New("token expiry must be within one year")
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return IssuedToken{}, ErrInvalidScope
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return IssuedToken{}, errors.New("cannot generate credential")
	}
	raw := "ant_" + base64.RawURLEncoding.EncodeToString(secret[:])
	clear(secret[:])
	verifier := sha256.Sum256([]byte(raw))
	encoded, _ := json.Marshal(scopes)
	tx, err := t.DB.Begin(ctx)
	if err != nil {
		return IssuedToken{}, errors.New("cannot start token issuance")
	}
	defer rollback(tx)
	var active bool
	if err = tx.QueryRow(ctx, "SELECT NOT disabled AND kind='service' FROM principals WHERE instance_id=$1 AND id=$2 FOR SHARE", instance, principal).Scan(&active); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return IssuedToken{}, ErrUnavailable
	}
	if err != nil || !active {
		return IssuedToken{}, ErrUnauthenticated
	}
	for _, scope := range scopes {
		if scope.FeedID != "" {
			var exists bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)", instance, scope.FeedID).Scan(&exists); err != nil {
				return IssuedToken{}, ErrUnavailable
			}
			if !exists {
				return IssuedToken{}, ErrInvalidScope
			}
		}
	}
	var result IssuedToken
	if err = tx.QueryRow(ctx, "INSERT INTO service_tokens(instance_id,principal_id,verifier,scopes,expires_at) VALUES($1,$2,$3,$4,$5) RETURNING id,expires_at", instance, principal, verifier[:], encoded, expires).Scan(&result.ID, &result.ExpiresAt); err != nil {
		return IssuedToken{}, errors.New("cannot issue token")
	}
	if err = tokenEvent(ctx, tx, instance, actor, result.ID, "token.issue"); err != nil {
		return IssuedToken{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return IssuedToken{}, errors.New("cannot commit token issuance")
	}
	result.Secret = raw
	return result, nil
}

func (t Tokens) Authenticate(ctx context.Context, raw string) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if !strings.HasPrefix(raw, "ant_") || len(raw) != 47 {
		return Principal{}, ErrUnauthenticated
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw[4:])
	if err != nil || len(decoded) != 32 {
		return Principal{}, ErrUnauthenticated
	}
	clear(decoded)
	if p, ok := takeTicket(raw); ok {
		return p, nil
	}
	verifier := sha256.Sum256([]byte(raw))
	var p Principal
	var scopes []byte
	err = t.DB.QueryRow(ctx, `SELECT t.instance_id,t.principal_id,t.id,t.scopes
 FROM service_tokens t JOIN principals p ON (p.instance_id,p.id)=(t.instance_id,t.principal_id)
 WHERE t.verifier=$1 AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp() AND NOT p.disabled AND p.kind='service'`, verifier[:]).Scan(&p.InstanceID, &p.PrincipalID, &p.TokenID, &scopes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnavailable
	}
	if err != nil || json.Unmarshal(scopes, &p.Scopes) != nil || !validScopes(p.Scopes) {
		return Principal{}, ErrUnauthenticated
	}
	t.touch(ctx, p.TokenID)
	return p, nil
}

// touch records token use for the console's token list. It is best effort and
// throttled to one write per token per minute; failures never affect authentication.
func (t Tokens) touch(ctx context.Context, id string) {
	_, _ = t.DB.Exec(ctx, "UPDATE service_tokens SET last_used_at=clock_timestamp() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<clock_timestamp()-interval '1 minute')", id)
}

// Revoke scopes the lookup to one instance and records the mutation atomically.
// The caller must authorize actor before invoking this storage operation.
func (t Tokens) Revoke(ctx context.Context, instance, actor, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := t.DB.Begin(ctx)
	if err != nil {
		return errors.New("cannot start token revocation")
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, "UPDATE service_tokens SET revoked_at=clock_timestamp() WHERE instance_id=$1 AND id=$2 AND revoked_at IS NULL", instance, id)
	if err != nil {
		return errors.New("cannot revoke token")
	}
	if tag.RowsAffected() > 0 {
		if err = tokenEvent(ctx, tx, instance, actor, id, "token.revoke"); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("cannot commit token revocation")
	}
	return nil
}
func tokenEvent(ctx context.Context, tx pgx.Tx, instance, actor, id, action string) error {
	var audit int64
	if err := tx.QueryRow(ctx, "INSERT INTO audit_events(instance_id,principal_id,action,resource_id) VALUES($1,$2,$3,$4) RETURNING id", instance, actor, action, id).Scan(&audit); err != nil {
		return errors.New("cannot record token audit")
	}
	if _, err := tx.Exec(ctx, "INSERT INTO outbox_events(audit_id,topic,payload) VALUES($1,$2,jsonb_build_object('token_id',$3::text))", audit, action, id); err != nil {
		return errors.New("cannot record token event")
	}
	return nil
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// normalizeScopes canonicalizes UUID spelling before scopes are retained for
// authorization comparisons.
func normalizeScopes(in []Scope) ([]Scope, error) {
	scopes := append([]Scope(nil), in...)
	for i := range scopes {
		if scopes[i].FeedID != "" {
			var id pgtype.UUID
			if id.Scan(scopes[i].FeedID) != nil || !id.Valid {
				return nil, ErrInvalidScope
			}
			value, _ := id.Value()
			scopes[i].FeedID = value.(string)
		}
	}
	return scopes, nil
}
