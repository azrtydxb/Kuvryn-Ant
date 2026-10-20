package identity

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type presentedKey struct{}

// presented records what the client sent before ClientAuth replaced it with a
// single-use in-process ticket.
type presented struct {
	method Method
	secret string // only retained for personal access tokens
}

func withPresented(ctx context.Context, method Method, secret string) context.Context {
	if method != MethodPAT {
		secret = ""
	}
	return context.WithValue(ctx, presentedKey{}, presented{method, secret})
}

// LoginToken backs protocols whose login handshake hands the client an opaque
// token to reuse as a bearer (Conan's users/authenticate). The Authorization
// header cannot be echoed for credentials that ClientAuth verified, because it
// then carries a single-use ticket. For a personal access token the PAT itself
// is the reusable credential; for a directory (Basic) login a short-lived,
// user-owned PAT is issued, replacing any earlier one with the same label.
// ok is false when the caller should echo its bearer instead (service tokens
// and GitHub OIDC exchanges, which already are durable bearer credentials).
func LoginToken(ctx context.Context, db *pgxpool.Pool, p Principal, label string, ttl time.Duration) (secret string, ok bool, err error) {
	pr, _ := ctx.Value(presentedKey{}).(presented)
	switch pr.method {
	case MethodPAT:
		return pr.secret, pr.secret != "", nil
	case MethodBasicDirectory:
		return PATs{DB: db}.issueLogin(ctx, p, label, ttl)
	}
	return "", false, nil
}

func (t PATs) issueLogin(ctx context.Context, p Principal, label string, ttl time.Duration) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := t.DB.Begin(ctx)
	if err != nil {
		return "", false, ErrUnavailable
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "pat:"+p.PrincipalID); err != nil {
		return "", false, ErrUnavailable
	}
	rows, err := tx.Query(ctx, "SELECT id::text FROM personal_access_tokens WHERE instance_id=$1 AND principal_id=$2 AND name=$3 AND revoked_at IS NULL AND expires_at>clock_timestamp()", p.InstanceID, p.PrincipalID, label)
	if err != nil {
		return "", false, ErrUnavailable
	}
	var stale []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return "", false, ErrUnavailable
		}
		stale = append(stale, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return "", false, ErrUnavailable
	}
	for _, id := range stale {
		if err = RevokePATTx(ctx, tx, p.InstanceID, p.PrincipalID, id); err != nil {
			return "", false, err
		}
	}
	_, raw, err := CreatePATTx(ctx, tx, p.InstanceID, p.PrincipalID, label, []Scope{{Actions: []Action{Read, Publish, Delete}}}, time.Now().Add(ttl))
	if err != nil {
		return "", false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", false, ErrUnavailable
	}
	return raw, true, nil
}
