package oidclogin

import (
	"context"
	"errors"
	"testing"
)

func TestExternalSessionAndRevokeByIdP(t *testing.T) {
	s := dbFixture(t)
	ctx := context.Background()
	const issuer = "https://idp.example/saml"
	var principal string
	if e := s.DB.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','u') RETURNING id::text`, s.InstanceID).Scan(&principal); e != nil {
		t.Fatal(e)
	}
	for _, subject := range []string{"alice", "bob"} {
		if e := s.BindIdentity(ctx, issuer, subject, principal); e != nil && subject == "alice" {
			t.Fatal(e)
		}
	}
	// Two sessions with different IdP indexes, one without an index, one for another subject.
	_, one, _, e := s.IssueExternalSAML(ctx, issuer, "alice", nil, "idx-1", "urn:format")
	if e != nil {
		t.Fatal(e)
	}
	_, two, _, _ := s.IssueExternalSAML(ctx, issuer, "alice", nil, "idx-2", "")
	_, bare, _, _ := s.IssueExternalGroups(ctx, issuer, "alice", nil)
	ext, e := s.ExternalSession(ctx, one)
	if e != nil || ext.Issuer != issuer || ext.Subject != "alice" || ext.SessionIndex != "idx-1" || ext.NameIDFormat != "urn:format" {
		t.Fatalf("%+v %v", ext, e)
	}
	if ext, e = s.ExternalSession(ctx, bare); e != nil || ext.SessionIndex != "" {
		t.Fatalf("%+v %v", ext, e)
	}
	if _, e = s.ExternalSession(ctx, "not-a-session"); !errors.Is(e, ErrAuthentication) {
		t.Fatal(e)
	}
	// An IdP request for idx-1 ends that session and the one with no recorded
	// index, but not the session of a different IdP session.
	n, e := s.RevokeExternal(ctx, issuer, "alice", "idx-1")
	if e != nil || n != 2 {
		t.Fatalf("revoked %d %v", n, e)
	}
	if _, e = s.ExternalSession(ctx, two); e != nil {
		t.Fatal("unrelated IdP session ended", e)
	}
	for _, raw := range []string{one, bare} {
		if _, e = s.ExternalSession(ctx, raw); !errors.Is(e, ErrAuthentication) {
			t.Fatal("session survived", e)
		}
	}
	// Another issuer or subject never matches; no index ends everything.
	if n, e = s.RevokeExternal(ctx, "https://other.example", "alice", ""); e != nil || n != 0 {
		t.Fatalf("%d %v", n, e)
	}
	if n, e = s.RevokeExternal(ctx, issuer, "alice", ""); e != nil || n != 1 {
		t.Fatalf("%d %v", n, e)
	}
	if _, e = s.RevokeExternal(ctx, "", "alice", ""); !errors.Is(e, ErrAuthentication) {
		t.Fatal(e)
	}
}
