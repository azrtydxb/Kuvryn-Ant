package oidclogin

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// IssueExternalSAML is IssueExternalGroups for a SAML login. It also records the
// IdP session index and NameID format so single logout can address the IdP
// session later. Neither value grants anything.
func (s Sessions) IssueExternalSAML(ctx context.Context, issuer, subject string, groups []string, sessionIndex, nameIDFormat string) (Session, string, string, error) {
	if len(sessionIndex) > 1024 || len(nameIDFormat) > 256 {
		return Session{}, "", "", ErrAuthentication
	}
	return s.issueIdP(ctx, issuer, subject, groups, sessionIndex, nameIDFormat)
}

// ExternalSession is the IdP-side identity a browser session was issued from.
type ExternalSession struct {
	Issuer, Subject, SessionIndex, NameIDFormat string
}

// ExternalSession returns the IdP identity of a live browser session, for
// building a SAML LogoutRequest. Call after Authenticate succeeded.
func (s Sessions) ExternalSession(ctx context.Context, raw string) (ExternalSession, error) {
	if !validSecret(raw) {
		return ExternalSession{}, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out ExternalSession
	e := s.DB.QueryRow(ctx, `SELECT issuer,subject,COALESCE(idp_session_index,''),COALESCE(idp_name_id_format,'') FROM browser_sessions WHERE verifier=$1 AND instance_id=$2 AND expires_at>clock_timestamp()`, hash(raw), s.InstanceID).Scan(&out.Issuer, &out.Subject, &out.SessionIndex, &out.NameIDFormat)
	if errors.Is(e, pgx.ErrNoRows) {
		return ExternalSession{}, ErrAuthentication
	}
	if e != nil {
		return ExternalSession{}, ErrUnavailable
	}
	return out, nil
}

// RevokeExternal ends the browser sessions of an IdP subject in response to an
// IdP-initiated logout. With a session index, sessions recorded under a
// different index survive; sessions with no recorded index are ended too, since
// they cannot be told apart. Returns the number of sessions ended (zero is not
// an error: SAML treats logging out an absent session as success).
func (s Sessions) RevokeExternal(ctx context.Context, issuer, subject, sessionIndex string) (int, error) {
	if issuer == "" || subject == "" || len(issuer) > 2048 || len(subject) > 2048 || len(sessionIndex) > 1024 {
		return 0, ErrAuthentication
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return 0, ErrUnavailable
	}
	defer rollback(tx)
	rows, e := tx.Query(ctx, `DELETE FROM browser_sessions WHERE instance_id=$1 AND issuer=$2 AND subject=$3 AND ($4='' OR idp_session_index IS NULL OR idp_session_index=$4) RETURNING principal_id::text`, s.InstanceID, issuer, subject, sessionIndex)
	if e != nil {
		return 0, ErrUnavailable
	}
	var principals []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) != nil {
			rows.Close()
			return 0, ErrUnavailable
		}
		principals = append(principals, p)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, ErrUnavailable
	}
	for _, p := range principals {
		if e = s.audit(ctx, tx, p, p, "browser.session_revoked_by_idp"); e != nil {
			return 0, e
		}
	}
	if tx.Commit(ctx) != nil {
		return 0, ErrUnavailable
	}
	return len(principals), nil
}
