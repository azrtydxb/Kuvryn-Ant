package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/config"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/azrtydxb/Kuvryn-Ant/internal/oidclogin"
)

func TestBrowserMountedConfiguration(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	os.WriteFile(secret, []byte("client-secret\n"), 0600)
	cfg := browserConfig{Origin: "https://ant.example", Key: "main", Issuer: "https://id.example", ClientID: "ant", ClientSecretFile: secret, RedirectURI: "https://ant.example/auth/oidc/callback", AllowedEndpoints: []string{"https://id.example/token"}}
	data, _ := json.Marshal(cfg)
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"valid", data, true}, {"oversized", []byte(strings.Repeat(" ", 65537)), false},
		{"duplicate", []byte(strings.Replace(string(data), `"key":"main"`, `"key":"main","key":"other"`, 1)), false},
		{"case alias", []byte(strings.Replace(string(data), `"key":`, `"Key":`, 1)), false},
		{"cross origin", []byte(strings.Replace(string(data), "https://ant.example/auth/oidc/callback", "https://evil.example/auth/oidc/callback", 1)), false},
		{"http", []byte(strings.ReplaceAll(string(data), "https://ant.example", "http://ant.example")), false},
		{"trailing", append(append([]byte{}, data...), []byte(" {}")...), false},
		{"inline secret", []byte(strings.Replace(string(data), `"key":"main"`, `"key":"main","client_secret":"sensitive"`, 1)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "config")
			os.WriteFile(path, tc.data, 0600)
			_, got, err := readBrowserConfig(path)
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
			if tc.valid && got != "client-secret" {
				t.Fatal("secret newline not removed")
			}
			if err != nil && strings.Contains(err.Error(), "sensitive") {
				t.Fatal("secret disclosure")
			}
		})
	}
	t.Setenv("ANT_OIDC_CONFIG_FILE", "")
	if b, err := openBrowser(context.Background(), nil, config.Config{}, ""); err != nil || b != nil {
		t.Fatal("optional browser configuration")
	}
}

type rejectedBrowserToken struct{}

func (rejectedBrowserToken) Authenticate(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, errors.New("invalid")
}
func TestBrowserBridgeSessionAndCSRF(t *testing.T) {
	pool, _ := runtimeFixture(t)
	ctx := context.Background()
	owner, err := identity.Bootstrap(ctx, pool, "Browser")
	if err != nil {
		t.Fatal(err)
	}
	// Browser identities bind to an explicitly provisioned user, not the bootstrap service.
	var principal string
	if err = pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','browser-user') RETURNING id::text`, owner.InstanceID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	sessions := oidclogin.Sessions{DB: pool, InstanceID: owner.InstanceID}
	if err = sessions.BindIdentity(ctx, "https://id.example", "subject", principal); err != nil {
		t.Fatal(err)
	}
	raw := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	csrf := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
	verifier := sha256.Sum256([]byte(raw))
	csrfHash := sha256.Sum256([]byte(csrf))
	_, err = pool.Exec(ctx, `INSERT INTO browser_sessions(verifier,csrf_hash,instance_id,issuer,subject,principal_id,expires_at,idle_seconds) VALUES($1,$2,$3,'https://id.example','subject',$4,clock_timestamp()+interval '1 hour',1800)`, verifier[:], csrfHash[:], owner.InstanceID, principal)
	if err != nil {
		t.Fatal(err)
	}
	b := &browserRuntime{client: &oidclogin.Client{Sessions: sessions, Origin: "https://ant.example"}}
	protected := identity.NewBearer(rejectedBrowserToken{}).Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := identity.PrincipalFromContext(r.Context())
		if !ok || p.PrincipalID != principal || p.TokenID != "" || p.Scopes != nil {
			t.Error("invalid user principal")
		}
		w.WriteHeader(204)
	}))
	h := b.Middleware(protected)
	for _, tc := range []struct {
		name, method, path, origin, csrf, auth, site string
		want                                         int
	}{
		{name: "read", method: "GET", path: "/api/v1/feeds", want: 204},
		{name: "write", method: "POST", path: "/api/v1/feeds", origin: b.client.Origin, csrf: csrf, want: 204},
		{name: "missing CSRF", method: "POST", path: "/api/v1/feeds", origin: b.client.Origin, want: 401},
		{name: "wrong origin", method: "POST", path: "/api/v1/feeds", origin: "https://evil.example", csrf: csrf, want: 401},
		{name: "cross site", method: "GET", path: "/api/v1/feeds", site: "cross-site", want: 401},
		{name: "package path", method: "GET", path: "/v2/", want: 401},
		{name: "explicit bearer", method: "GET", path: "/api/v1/feeds", auth: "Bearer invalid", want: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.AddCookie(&http.Cookie{Name: oidclogin.SessionCookie, Value: raw})
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CSRF-Token", tc.csrf)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
	if err = sessions.Revoke(ctx, raw); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v1/feeds", nil)
	r.AddCookie(&http.Cookie{Name: oidclogin.SessionCookie, Value: raw})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("revoked session accepted")
	}
}

func TestBrowserOriginNormalization(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://ANT.example:443/", "https://ant.example"},
		{"https://[::1]:8443/", "https://[::1]:8443"},
		{"https://ant.example:8443", "https://ant.example:8443"},
		{"http://ant.example", ""}, {"https://user@ant.example", ""}, {"https://ant.example/path", ""}, {"https://ant.example?", ""}, {"https://ant.example/#x", ""}, {"https://ant.example:99999", ""},
	} {
		got, ok := browserOrigin(tc.raw)
		if got != tc.want || ok != (tc.want != "") {
			t.Fatalf("%s => %s %v", tc.raw, got, ok)
		}
	}
}
func TestBrowserCleanupBoundedAndPreservesLiveState(t *testing.T) {
	e, _, _, principal := enterpriseFixture(t)
	pool := e.client.Sessions.DB
	ctx := context.Background()
	instance := e.client.Sessions.InstanceID
	queries := []string{
		`INSERT INTO oidc_login_states(state_hash,instance_id,provider_key,browser_hash,nonce_hash,expires_at) SELECT sha256(('oidc'||n)::bytea),$1,'provider',sha256('browser'::bytea),sha256('nonce'::bytea),clock_timestamp()-interval '1 minute' FROM generate_series(1,650) n`,
		`INSERT INTO enterprise_login_states(state_hash,browser_hash,instance_id,provider,kind,request_id,expires_at) SELECT sha256(('enterprise'||n)::bytea),sha256('browser'::bytea),$1,'provider','ldap','',clock_timestamp()-interval '1 minute' FROM generate_series(1,650) n`,
		`INSERT INTO saml_replay_claims(instance_id,provider_hash,key_hash,created_at,expires_at) SELECT $1,sha256('provider'::bytea),sha256(('saml'||n)::bytea),clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour' FROM generate_series(1,650) n`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query, instance); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO browser_sessions(verifier,csrf_hash,instance_id,issuer,subject,principal_id,created_at,last_seen_at,expires_at,idle_seconds) SELECT sha256(('session'||n)::bytea),sha256('csrf'::bytea),$1,'https://id.example/ldap','immutable-subject',$2,statement_timestamp()-interval '2 hours',statement_timestamp()-interval '2 hours',statement_timestamp()-interval '1 hour',1800 FROM generate_series(1,650) n`, instance, principal); err != nil {
		t.Fatal(err)
	}
	_, live, _, err := e.client.Sessions.IssueExternal(ctx, e.ldapIssuer, "immutable-subject")
	if err != nil {
		t.Fatal(err)
	}
	b := &browserRuntime{client: e.client, enterprise: e}
	if err = b.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"oidc_login_states", "enterprise_login_states", "saml_replay_claims", "browser_sessions"} {
		var n int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE expires_at<clock_timestamp()").Scan(&n); err != nil || n != 150 {
			t.Fatal(table, n, err)
		}
	}
	if _, err = e.client.Sessions.Authenticate(ctx, live, "", false); err != nil {
		t.Fatal("live session deleted", err)
	}
	if err = b.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"oidc_login_states", "enterprise_login_states", "saml_replay_claims", "browser_sessions"} {
		var n int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE expires_at<clock_timestamp()").Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func TestOpenBrowserRequiresProviderPublicOriginAgreement(t *testing.T) {
	pool, _ := runtimeFixture(t)
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	if err := os.WriteFile(password, []byte("service-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := ldapBrowserConfig{Origin: "https://ant.example", ProviderID: "https://identity.example/directory/main", URL: "ldaps://directory.example:636", BindDN: "cn=service,dc=example", BindPasswordFile: password, UserBaseDN: "ou=users,dc=example", GroupBaseDN: "ou=groups,dc=example", LoginAttribute: "uid", UserIDAttribute: "entryUUID", GroupIDAttribute: "entryUUID", UserClass: "inetOrgPerson", GroupClass: "groupOfNames", MemberAttribute: "member"}
	raw, _ := json.Marshal(provider)
	path := filepath.Join(dir, "ldap.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANT_OIDC_CONFIG_FILE", "")
	t.Setenv("ANT_SAML_CONFIG_FILE", "")
	t.Setenv("ANT_LDAP_CONFIG_FILE", path)
	for _, tc := range []struct {
		url   string
		valid bool
	}{{"https://ANT.example:443/", true}, {"https://different.example", false}, {"http://ant.example", false}, {"https://ant.example/other", false}} {
		b, err := openBrowser(context.Background(), pool, config.Config{PublicURL: tc.url}, "00000000-0000-0000-0000-000000000001")
		if (err == nil) != tc.valid || (b != nil) != tc.valid {
			t.Fatalf("origin %s: %v", tc.url, err)
		}
	}
}
