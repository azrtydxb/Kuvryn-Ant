package oidclogin

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Profile is the display information an identity provider vouched for at login.
// It is informational only and never grants anything.
type Profile struct {
	Name, Email, Picture string
	// IDToken is the OIDC ID token, kept solely as the id_token_hint of
	// RP-initiated logout.
	IDToken string
}

type profileKey struct{}

// WithProfile attaches the login profile to the context of a session issue.
func WithProfile(ctx context.Context, p Profile) context.Context {
	return context.WithValue(ctx, profileKey{}, p)
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, "\x00\r\n") {
		return ""
	}
	if len(s) > max {
		return ""
	}
	if !utf8.ValidString(s) {
		return ""
	}
	return s
}

// sanitize drops anything unfit to store or render; a picture must be an
// absolute https URL.
func (p Profile) sanitize() Profile {
	out := Profile{Name: clip(p.Name, 256), Email: clip(p.Email, 320), IDToken: clip(p.IDToken, 16384)}
	if pic := clip(p.Picture, 2048); pic != "" {
		if u, err := url.Parse(pic); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
			out.Picture = pic
		}
	}
	return out
}

func profileFrom(ctx context.Context) Profile {
	p, _ := ctx.Value(profileKey{}).(Profile)
	return p.sanitize()
}

// Details is what a live browser session knows about its identity.
type Details struct {
	PrincipalID, Issuer, Subject string
	Name, Email, Picture         string
	CreatedAt, ExpiresAt         time.Time
}

// Describe returns the identity facts of a live session. Call after Authenticate.
func (s Sessions) Describe(ctx context.Context, raw string) (Details, error) {
	if !validSecret(raw) {
		return Details{}, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var d Details
	e := s.DB.QueryRow(ctx, `SELECT principal_id::text,issuer,subject,COALESCE(display_name,''),COALESCE(email,''),COALESCE(picture,''),created_at,expires_at FROM browser_sessions WHERE verifier=$1 AND instance_id=$2 AND expires_at>clock_timestamp()`, hash(raw), s.InstanceID).
		Scan(&d.PrincipalID, &d.Issuer, &d.Subject, &d.Name, &d.Email, &d.Picture, &d.CreatedAt, &d.ExpiresAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return Details{}, ErrAuthentication
	}
	if e != nil {
		return Details{}, ErrUnavailable
	}
	return d, nil
}

// IDTokenHint returns the stored OIDC ID token of a session, or "".
func (s Sessions) IDTokenHint(ctx context.Context, raw string) string {
	if !validSecret(raw) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var t string
	if s.DB.QueryRow(ctx, `SELECT COALESCE(idp_id_token,'') FROM browser_sessions WHERE verifier=$1 AND instance_id=$2`, hash(raw), s.InstanceID).Scan(&t) != nil {
		return ""
	}
	return t
}

// RevokeOthers ends every browser session of the principal except the one
// presented, returning how many were ended.
func (s Sessions) RevokeOthers(ctx context.Context, principal, keepRaw string) (int, error) {
	if !validSecret(keepRaw) {
		return 0, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tag, e := s.DB.Exec(ctx, `DELETE FROM browser_sessions WHERE instance_id=$1 AND principal_id=$2 AND verifier<>$3`, s.InstanceID, principal, hash(keepRaw))
	if e != nil {
		return 0, ErrUnavailable
	}
	return int(tag.RowsAffected()), nil
}

// VerifierHash is the stored form of a session cookie value, for callers that
// must refer to a session row (for example to keep it when revoking others).
func VerifierHash(raw string) []byte {
	if !validSecret(raw) {
		return nil
	}
	return hash(raw)
}
