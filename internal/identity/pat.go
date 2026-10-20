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
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	patPrefix      = "antp_"
	patLength      = 48
	maxActivePATs  = 50
	maxPATLifetime = 365 * 24 * time.Hour
)

type patError string

func (e patError) Error() string { return string(e) }

const (
	ErrPATLimit    = patError("too many active personal access tokens")
	ErrPATNotFound = patError("personal access token not found")
	ErrPATInvalid  = patError("invalid personal access token request")
	ErrPATNotUser  = patError("personal access tokens are only available to user principals")
)

// PATs manages self-service personal access tokens. A PAT carries the owner's
// own grants, narrowed by its scopes; it can never exceed what the user holds.
type PATs struct{ DB *pgxpool.Pool }

type PATRecord struct {
	ID         string
	Name       string
	Scopes     []Scope
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// State is the computed lifecycle state at now: revoked wins over expired.
func (r PATRecord) State(now time.Time) string {
	switch {
	case r.RevokedAt != nil:
		return "revoked"
	case !r.ExpiresAt.After(now):
		return "expired"
	}
	return "active"
}

// PATCounts is the number of a user's tokens per state.
type PATCounts struct{ Active, Expired, Revoked int64 }

// ValidPATScopes limits PAT scopes to package-protocol actions.
func ValidPATScopes(scopes []Scope) bool {
	if !validScopes(scopes) {
		return false
	}
	for _, s := range scopes {
		for _, a := range s.Actions {
			if a != Read && a != Publish && a != Delete {
				return false
			}
		}
	}
	return true
}

// Create returns the secret exactly once. The caller must have authenticated
// principal as itself (never on behalf of another user).
func (t PATs) Create(ctx context.Context, instance, principal, name string, scopes []Scope, expires time.Time) (PATRecord, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := t.DB.Begin(ctx)
	if err != nil {
		return PATRecord{}, "", ErrUnavailable
	}
	defer rollback(tx)
	rec, raw, err := CreatePATTx(ctx, tx, instance, principal, name, scopes, expires)
	if err != nil {
		return PATRecord{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return PATRecord{}, "", ErrUnavailable
	}
	return rec, raw, nil
}

// CreatePATTx inserts a token and its audit event inside the caller's
// transaction, so an idempotent mutation can commit them together. The
// returned secret must be shown to the caller once and never stored.
func CreatePATTx(ctx context.Context, tx pgx.Tx, instance, principal, name string, scopes []Scope, expires time.Time) (PATRecord, string, error) {
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || strings.ContainsFunc(name, func(r rune) bool { return r < 32 || r == 127 }) || !ValidPATScopes(scopes) {
		return PATRecord{}, "", ErrPATInvalid
	}
	if !expires.After(time.Now()) || expires.After(time.Now().Add(maxPATLifetime)) {
		return PATRecord{}, "", ErrPATInvalid
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return PATRecord{}, "", ErrPATInvalid
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return PATRecord{}, "", ErrUnavailable
	}
	raw := patPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	clear(secret[:])
	verifier := sha256.Sum256([]byte(raw))
	encoded, _ := json.Marshal(scopes)
	var user bool
	if err = tx.QueryRow(ctx, "SELECT kind='user' AND NOT disabled FROM principals WHERE instance_id=$1 AND id=$2 FOR SHARE", instance, principal).Scan(&user); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PATRecord{}, "", ErrPATNotUser
		}
		return PATRecord{}, "", ErrUnavailable
	}
	if !user {
		return PATRecord{}, "", ErrPATNotUser
	}
	for _, s := range scopes {
		if s.FeedID == "" {
			continue
		}
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM feeds WHERE instance_id=$1 AND id=$2)", instance, s.FeedID).Scan(&exists); err != nil {
			return PATRecord{}, "", ErrUnavailable
		}
		if !exists {
			return PATRecord{}, "", ErrPATInvalid
		}
	}
	// Serialize per user so the cap cannot be raced past.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "pat:"+principal); err != nil {
		return PATRecord{}, "", ErrUnavailable
	}
	var active int
	if err = tx.QueryRow(ctx, "SELECT count(*) FROM personal_access_tokens WHERE instance_id=$1 AND principal_id=$2 AND revoked_at IS NULL AND expires_at>clock_timestamp()", instance, principal).Scan(&active); err != nil {
		return PATRecord{}, "", ErrUnavailable
	}
	if active >= maxActivePATs {
		return PATRecord{}, "", ErrPATLimit
	}
	rec := PATRecord{Name: name, Scopes: scopes}
	if err = tx.QueryRow(ctx, "INSERT INTO personal_access_tokens(instance_id,principal_id,name,verifier,scopes,expires_at) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,created_at,expires_at", instance, principal, name, verifier[:], encoded, expires).Scan(&rec.ID, &rec.CreatedAt, &rec.ExpiresAt); err != nil {
		return PATRecord{}, "", ErrUnavailable
	}
	if err = tokenEvent(ctx, tx, instance, principal, rec.ID, "pat.create"); err != nil {
		return PATRecord{}, "", err
	}
	return rec, raw, nil
}

// List returns the principal's tokens in every state, newest first. Secrets are never stored.
func (t PATs) List(ctx context.Context, instance, principal string) ([]PATRecord, error) {
	recs, _, err := t.ListState(ctx, instance, principal, "")
	return recs, err
}

const patStateSQL = `CASE WHEN revoked_at IS NOT NULL THEN 'revoked' WHEN expires_at<=clock_timestamp() THEN 'expired' ELSE 'active' END`

// ListState lists the principal's tokens, newest first, optionally limited to
// one state ("active", "expired" or "revoked"; "" lists all), together with the
// per-state counts of all the principal's tokens.
func (t PATs) ListState(ctx context.Context, instance, principal, state string) ([]PATRecord, PATCounts, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var counts PATCounts
	if err := t.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE `+patStateSQL+`='active'),count(*) FILTER (WHERE `+patStateSQL+`='expired'),count(*) FILTER (WHERE `+patStateSQL+`='revoked') FROM personal_access_tokens WHERE instance_id=$1 AND principal_id=$2`, instance, principal).Scan(&counts.Active, &counts.Expired, &counts.Revoked); err != nil {
		return nil, counts, ErrUnavailable
	}
	rows, err := t.DB.Query(ctx, "SELECT id,name,scopes,created_at,expires_at,last_used_at,revoked_at FROM personal_access_tokens WHERE instance_id=$1 AND principal_id=$2 AND ($3='' OR $3="+patStateSQL+") ORDER BY created_at DESC,id LIMIT 200", instance, principal, state)
	if err != nil {
		return nil, counts, ErrUnavailable
	}
	defer rows.Close()
	out := []PATRecord{}
	for rows.Next() {
		var r PATRecord
		var scopes []byte
		if rows.Scan(&r.ID, &r.Name, &scopes, &r.CreatedAt, &r.ExpiresAt, &r.LastUsedAt, &r.RevokedAt) != nil || json.Unmarshal(scopes, &r.Scopes) != nil {
			return nil, counts, ErrUnavailable
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return nil, counts, ErrUnavailable
	}
	return out, counts, nil
}

// Revoke only affects tokens owned by principal; others report not found.
func (t PATs) Revoke(ctx context.Context, instance, principal, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := t.DB.Begin(ctx)
	if err != nil {
		return ErrUnavailable
	}
	defer rollback(tx)
	if err = RevokePATTx(ctx, tx, instance, principal, id); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return ErrUnavailable
	}
	return nil
}

// RevokePATTx revokes inside the caller's transaction (idempotency store,
// audit). Revoking an already revoked token is a no-op without a second audit
// row; tokens owned by others report not found.
func RevokePATTx(ctx context.Context, tx pgx.Tx, instance, principal, id string) error {
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM personal_access_tokens WHERE instance_id=$1 AND id::text=$2 AND principal_id=$3)", instance, id, principal).Scan(&exists); err != nil {
		return ErrUnavailable
	}
	if !exists {
		return ErrPATNotFound
	}
	tag, err := tx.Exec(ctx, "UPDATE personal_access_tokens SET revoked_at=clock_timestamp() WHERE instance_id=$1 AND id::text=$2 AND principal_id=$3 AND revoked_at IS NULL", instance, id, principal)
	if err != nil {
		return ErrUnavailable
	}
	if tag.RowsAffected() > 0 {
		return tokenEvent(ctx, tx, instance, principal, id, "pat.revoke")
	}
	return nil
}

// Authenticate verifies a PAT for package protocols. The owner must still be
// an enabled user. The returned principal carries the PAT's scopes.
func (t PATs) Authenticate(ctx context.Context, raw string) (Principal, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if len(raw) != patLength || !strings.HasPrefix(raw, patPrefix) {
		return Principal{}, ErrUnauthenticated
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw[len(patPrefix):])
	if err != nil || len(decoded) != 32 {
		return Principal{}, ErrUnauthenticated
	}
	verifier := sha256.Sum256([]byte(raw))
	var p Principal
	var scopes []byte
	err = t.DB.QueryRow(ctx, `SELECT t.instance_id,t.principal_id,t.id,t.scopes
 FROM personal_access_tokens t JOIN principals p ON (p.instance_id,p.id)=(t.instance_id,t.principal_id)
 WHERE t.verifier=$1 AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp() AND NOT p.disabled AND p.kind='user'`, verifier[:]).Scan(&p.InstanceID, &p.PrincipalID, &p.TokenID, &scopes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnavailable
	}
	if err != nil || json.Unmarshal(scopes, &p.Scopes) != nil || !ValidPATScopes(p.Scopes) {
		return Principal{}, ErrUnauthenticated
	}
	_, _ = t.DB.Exec(ctx, "UPDATE personal_access_tokens SET last_used_at=clock_timestamp() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at<clock_timestamp()-interval '1 minute')", p.TokenID)
	return p, nil
}
