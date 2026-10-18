package directory

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"
)

func TestDiagnoseReportsEachStage(t *testing.T) {
	ok := &fixture{search: func(*ldap.SearchRequest) (*ldap.SearchResult, error) {
		return result(entry("ou=x,dc=example", "1")), nil
	}}
	d := provider(t, config(), ok).Diagnose(context.Background())
	if !d.OK || d.TLSMode != "ldaps" || len(d.Steps) != 4 {
		t.Fatalf("%+v", d)
	}
	badBind := &fixture{bind: func(string, string) error { return errors.New("invalid creds") }, search: ok.search}
	d = provider(t, config(), badBind).Diagnose(context.Background())
	if d.OK || d.Steps[len(d.Steps)-1].Name != "service_bind" || d.Steps[len(d.Steps)-1].OK {
		t.Fatalf("%+v", d)
	}
	missingGroups := &fixture{search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		if r.BaseDN == "ou=groups,dc=example" {
			return result(), nil
		}
		return result(entry(r.BaseDN, "1")), nil
	}}
	d = provider(t, config(), missingGroups).Diagnose(context.Background())
	if d.OK || d.Steps[len(d.Steps)-1].Name != "group_base" {
		t.Fatalf("%+v", d)
	}
	for _, s := range d.Steps {
		if s.Detail == "" {
			t.Fatal("empty detail")
		}
	}
	c := config()
	c.URL = "ldap://directory.example"
	p, err := New(c, func(context.Context) ([]byte, time.Time, error) { return []byte("s"), time.Now().Add(time.Minute), nil },
		func(context.Context, string, *tls.Config) (Connection, error) { return nil, ErrUnavailable })
	if err != nil {
		t.Fatal(err)
	}
	d = p.Diagnose(context.Background())
	if d.TLSMode != "starttls" || d.OK || d.Steps[0].Name != "tls" || d.Steps[0].OK {
		t.Fatalf("%+v", d)
	}
}

func TestLookupMapsRolesWithoutPassword(t *testing.T) {
	c := config()
	c.GroupRoles = map[string][]string{base64.RawURLEncoding.EncodeToString([]byte("dev")): {"reader"}}
	calls := 0
	binds := 0
	f := &fixture{bind: func(dn, pw string) error { binds++; return nil }, search: func(r *ldap.SearchRequest) (*ldap.SearchResult, error) {
		calls++
		switch calls {
		case 1:
			return result(entry(userDN, "immutable-user")), nil
		case 2:
			return result(group("dev")), nil
		}
		return result(), nil
	}}
	got, err := provider(t, c, f).Lookup(context.Background(), "alice")
	if err != nil || len(got.RoleIDs) != 1 || got.RoleIDs[0] != "reader" || binds != 1 {
		t.Fatalf("%+v %v binds=%d", got, err, binds)
	}
	f.search = func(*ldap.SearchRequest) (*ldap.SearchResult, error) { return result(), nil }
	if _, err = provider(t, c, f).Lookup(context.Background(), "ghost"); !errors.Is(err, ErrAuthentication) {
		t.Fatal(err)
	}
}
