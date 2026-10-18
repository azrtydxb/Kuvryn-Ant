package idpadmin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/directory"
	"github.com/azrtydxb/Kuvryn-Ant/internal/testdb"
	"github.com/go-ldap/ldap/v3"
)

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// ---- fake OIDC provider (httptest TLS) ----

type fakeOIDC struct {
	srv      *httptest.Server
	issuer   string
	noRS256  bool
	badIssue bool
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	f := &fakeOIDC{}
	mux := http.NewServeMux()
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	f.issuer = f.srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		algs := []string{"RS256"}
		if f.noRS256 {
			algs = []string{"ES256"}
		}
		iss := f.issuer
		if f.badIssue {
			iss += "/"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "authorization_endpoint": f.issuer + "/authorize", "token_endpoint": f.issuer + "/token", "jwks_uri": f.issuer + "/jwks",
			"id_token_signing_alg_values_supported": algs, "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		kty, alg := "RSA", "RS256"
		if f.noRS256 {
			kty, alg = "EC", "ES256"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": kty, "alg": alg, "use": "sig", "kid": "k1", "n": "AQAB", "e": "AQAB"}}})
	})
	return f
}

func TestOIDCDiagnostics(t *testing.T) {
	f := newFakeOIDC(t)
	d := Diagnostics{Transport: f.srv.Client().Transport}
	allowed := []string{f.issuer + "/authorize", f.issuer + "/token", f.issuer + "/jwks"}
	r := d.OIDC(context.Background(), f.issuer, allowed)
	if !r.OK || r.JWKSKeys != 1 || !r.SupportsRS256 || !r.SupportsPKCE || !r.IssuerMatches || r.ClockSkewSeconds == nil {
		t.Fatalf("%+v", r)
	}
	if len(r.SuggestedEndpoints) != 3 {
		t.Fatal("no suggested endpoints")
	}
	// Missing approved endpoints.
	r = d.OIDC(context.Background(), f.issuer, allowed[:1])
	if r.OK || len(r.MissingFromAllowed) != 2 {
		t.Fatalf("%+v", r)
	}
	// Clock skew detection: local clock two minutes behind the provider's Date header.
	skewed := Diagnostics{Transport: d.Transport, Now: func() time.Time { return time.Now().Add(-2 * time.Minute) }}
	r = skewed.OIDC(context.Background(), f.issuer, allowed)
	if !r.SkewWarning || r.ClockSkewSeconds == nil || *r.ClockSkewSeconds < 100 {
		t.Fatalf("skew not detected: %+v", r)
	}
	f.badIssue = true
	if r = d.OIDC(context.Background(), f.issuer, allowed); r.OK || r.IssuerMatches {
		t.Fatal("issuer mismatch not detected")
	}
	f.badIssue, f.noRS256 = false, true
	if r = d.OIDC(context.Background(), f.issuer, allowed); r.OK || r.SupportsRS256 {
		t.Fatal("missing RS256 not detected")
	}
	// Plain http and unreachable issuers are refused cleanly.
	if r = d.OIDC(context.Background(), "http://idp.example", nil); r.OK || len(r.Errors) == 0 {
		t.Fatal("http issuer accepted")
	}
	f.srv.Close()
	if r = d.OIDC(context.Background(), f.issuer, allowed); r.OK {
		t.Fatal("unreachable provider reported OK")
	}
}

func TestSafeTransportBlocksMetadataAddress(t *testing.T) {
	d := Diagnostics{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := d.OIDC(ctx, "https://169.254.169.254", nil)
	if r.OK || len(r.Errors) == 0 {
		t.Fatal("link-local address must be blocked")
	}
}

// ---- SAML fixtures ----

func idpMetadata(t *testing.T, entityID string, notAfter time.Time) string {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "idp"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	if notAfter.Before(time.Now()) {
		tpl.NotBefore = notAfter.Add(-time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>%s</X509Certificate></X509Data></KeyInfo></KeyDescriptor><SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/></IDPSSODescriptor></EntityDescriptor>`,
		entityID, base64.StdEncoding.EncodeToString(der))
}

func TestSAMLMetadataImportCertificateStatus(t *testing.T) {
	d := Diagnostics{}
	ok := d.ImportSAMLMetadata([]byte(idpMetadata(t, "https://idp.example/", time.Now().AddDate(1, 0, 0))))
	if !ok.OK || ok.EntityID != "https://idp.example/" || len(ok.SSO) != 1 || ok.Certificates[0].Status != "ok" {
		t.Fatalf("%+v", ok)
	}
	soon := d.ImportSAMLMetadata([]byte(idpMetadata(t, "https://idp.example/", time.Now().AddDate(0, 0, 10))))
	if !soon.OK || soon.Certificates[0].Status != "expiring" || len(soon.Warnings) == 0 {
		t.Fatalf("%+v", soon)
	}
	gone := d.ImportSAMLMetadata([]byte(idpMetadata(t, "https://idp.example/", time.Now().Add(-time.Hour))))
	if gone.OK || gone.Certificates[0].Status != "expired" {
		t.Fatalf("%+v", gone)
	}
	for _, bad := range []string{"", "not xml", `<!DOCTYPE x [<!ENTITY a "b">]>` + idpMetadata(t, "x", time.Now().AddDate(1, 0, 0))} {
		if r := d.ImportSAMLMetadata([]byte(bad)); r.OK || len(r.Errors) == 0 {
			t.Fatal("accepted bad metadata", bad)
		}
	}
}

// ---- store, builders, preview (database) ----

type fakeConn struct {
	search func(*ldap.SearchRequest) *ldap.SearchResult
}

func (fakeConn) Bind(string, string) error { return nil }
func (c fakeConn) Search(_ context.Context, r *ldap.SearchRequest) (*ldap.SearchResult, error) {
	return c.search(r), nil
}
func (fakeConn) Close() error { return nil }

func ldapConfig() LDAPConfig {
	return LDAPConfig{ProviderID: "https://ldap.example", URL: "ldaps://directory.example", BindDN: "cn=reader,dc=example", UserBaseDN: "ou=users,dc=example", GroupBaseDN: "ou=groups,dc=example",
		LoginAttribute: "uid", UserIDAttribute: "entryUUID", GroupIDAttribute: "entryUUID", UserClass: "person", GroupClass: "groupOfNames", MemberAttribute: "member",
		GroupRoles: map[string][]string{base64.RawURLEncoding.EncodeToString([]byte("dev")): {"reader"}}}
}

func secretPtr(s string) *string { return &s }

func TestStoreLifecycleSecretsAndSSOGuard(t *testing.T) {
	f := testdb.New(t)
	ctx := context.Background()
	b := Builder{Origin: "https://ant.example", DB: f.Pool, Keys: f.Keys, InstanceID: f.InstanceID}
	s := Store{DB: f.Pool, Keys: f.Keys, InstanceID: f.InstanceID}
	var actor string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','admin') RETURNING id::text`, f.InstanceID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	// Validation: missing secret / bad config / bad name are refused.
	if _, err := s.Create(ctx, actor, Input{Kind: LDAP, Name: "corp", Config: raw(ldapConfig())}, b.Check); !errors.Is(err, ErrInvalid) {
		t.Fatal("LDAP without bind password accepted", err)
	}
	if _, err := s.Create(ctx, actor, Input{Kind: LDAP, Name: "", Config: raw(ldapConfig()), Secret: secretPtr("x")}, b.Check); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty name accepted")
	}
	if _, err := s.Create(ctx, actor, Input{Kind: OIDC, Name: "o", Config: json.RawMessage(`{"issuer":"http://x","client_id":"c","allowed_endpoints":["https://x"]}`), Enabled: true}, b.Check); !errors.Is(err, ErrInvalid) {
		t.Fatal("http issuer accepted")
	}
	// Create disabled, then enable; secret is encrypted at rest and never listed.
	p, err := s.Create(ctx, actor, Input{Kind: LDAP, Name: "corp", Config: raw(ldapConfig()), Secret: secretPtr("bind-password-123")}, b.Check)
	if err != nil || p.Enabled || !p.HasSecret || p.Revision != 1 {
		t.Fatal(err, p)
	}
	if f.Count(t, `SELECT count(*) FROM encrypted_secrets WHERE instance_id=$1 AND envelope::text LIKE '%bind-password%'`, f.InstanceID) != 0 {
		t.Fatal("secret stored in plaintext")
	}
	if f.Count(t, `SELECT count(*) FROM identity_providers WHERE config::text LIKE '%bind-password%'`) != 0 {
		t.Fatal("secret stored in config")
	}
	list, _ := s.List(ctx)
	enc, _ := json.Marshal(list)
	if strings.Contains(string(enc), "bind-password") {
		t.Fatal("secret returned by list")
	}
	if _, err = s.SetEnabled(ctx, actor, p.ID, 99, true, b.Check); !errors.Is(err, ErrRevision) {
		t.Fatal("stale revision accepted", err)
	}
	p, err = s.SetEnabled(ctx, actor, p.ID, p.Revision, true, b.Check)
	if err != nil || !p.Enabled {
		t.Fatal(err)
	}
	loaded, err := s.LoadEnabled(ctx)
	if err != nil || len(loaded) != 1 || string(loaded[0].Secret) != "bind-password-123" {
		t.Fatal("enabled provider not loadable with secret", err)
	}
	// A second enabled LDAP provider swaps the first one out atomically.
	cfg2 := ldapConfig()
	cfg2.ProviderID = "https://ldap2.example"
	p2, err := s.Create(ctx, actor, Input{Kind: LDAP, Name: "corp2", Enabled: true, Config: raw(cfg2), Secret: secretPtr("pw2-pw2-pw2")}, b.Check)
	if err != nil || !p2.Enabled {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, p.ID); got.Enabled {
		t.Fatal("first provider still enabled")
	}
	// Secret rotation keeps config; unchanged secret is retained on update.
	cur, _ := s.Get(ctx, p2.ID)
	p2, err = s.Update(ctx, actor, p2.ID, cur.Revision, Input{Name: "corp2", Enabled: true, Config: raw(cfg2)}, b.Check)
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Load(ctx, p2.ID); string(l.Secret) != "pw2-pw2-pw2" {
		t.Fatal("secret lost on update")
	}
	p2, err = s.Update(ctx, actor, p2.ID, p2.Revision, Input{Name: "corp2", Enabled: true, Config: raw(cfg2), Secret: secretPtr("rotated-secret-1")}, b.Check)
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Load(ctx, p2.ID); string(l.Secret) != "rotated-secret-1" {
		t.Fatal("secret not rotated")
	}
	// SSO mandatory safety rails.
	if err = (Store{DB: f.Pool, Keys: f.Keys, InstanceID: f.InstanceID}).SetSSOMandatory(ctx, actor, true); !errors.Is(err, ErrNoBreakGlass) {
		t.Fatal("SSO mandatory allowed without break-glass", err)
	}
	if _, err = f.Pool.Exec(ctx, `INSERT INTO principals(instance_id,kind,subject,issuer) VALUES($1,'emergency','break-glass','ant-local')`, f.InstanceID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Pool.Exec(ctx, `INSERT INTO local_accounts(instance_id,principal_id,username,password_hash,break_glass) SELECT $1,id,'break-glass',repeat('x',64),true FROM principals WHERE subject='break-glass'`, f.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSSOMandatory(ctx, actor, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetEnabled(ctx, actor, p2.ID, p2.Revision, false, b.Check); !errors.Is(err, ErrLastProvider) {
		t.Fatal("disabled last provider under mandatory SSO", err)
	}
	if err = s.Delete(ctx, actor, p2.ID, p2.Revision); !errors.Is(err, ErrLastProvider) {
		t.Fatal("deleted last provider under mandatory SSO", err)
	}
	if err = s.SetSSOMandatory(ctx, actor, false); err != nil {
		t.Fatal(err)
	}
	if err = s.Delete(ctx, actor, p2.ID, p2.Revision); err != nil {
		t.Fatal(err)
	}
	if f.Count(t, `SELECT count(*) FROM encrypted_secrets WHERE instance_id=$1 AND purpose=$2`, f.InstanceID, purposePrefix+p2.ID) != 0 {
		t.Fatal("secret survived delete")
	}
	if f.Count(t, `SELECT count(*) FROM audit_events WHERE instance_id=$1 AND action LIKE 'identity_provider.%'`, f.InstanceID) < 6 {
		t.Fatal("lifecycle not audited")
	}
}

func TestOIDCStoredBuildAndSAMLExport(t *testing.T) {
	f := testdb.New(t)
	ctx := context.Background()
	oidc := newFakeOIDC(t)
	b := Builder{Origin: "https://ant.example", Transport: oidc.srv.Client().Transport, DB: f.Pool, Keys: f.Keys, InstanceID: f.InstanceID}
	s := Store{DB: f.Pool, Keys: f.Keys, InstanceID: f.InstanceID}
	var actor string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user','admin') RETURNING id::text`, f.InstanceID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	cfg := OIDCConfig{Issuer: oidc.issuer, ClientID: "ant", AllowedEndpoints: []string{oidc.issuer + "/authorize", oidc.issuer + "/token", oidc.issuer + "/jwks"}}
	// A config whose endpoints are not approved is rejected before being stored.
	bad := cfg
	bad.AllowedEndpoints = bad.AllowedEndpoints[:1]
	if _, err := s.Create(ctx, actor, Input{Kind: OIDC, Name: "sso", Enabled: true, Config: raw(bad), Secret: secretPtr("shh")}, b.Check); err == nil {
		t.Fatal("broken enabled OIDC config was stored")
	}
	if f.Count(t, `SELECT count(*) FROM identity_providers`) != 0 || f.Count(t, `SELECT count(*) FROM encrypted_secrets`) != 0 {
		t.Fatal("failed create left rows behind")
	}
	p, err := s.Create(ctx, actor, Input{Kind: OIDC, Name: "sso", Enabled: true, Config: raw(cfg), Secret: secretPtr("shh")}, b.Check)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := s.Load(ctx, p.ID)
	if prov, err := b.OIDC(ctx, l); err != nil || prov == nil {
		t.Fatal("stored OIDC provider does not build", err)
	}

	// SAML: SP key generated server side; metadata exportable; rotation changes cert.
	idp := idpMetadata(t, "https://idp.example/", time.Now().AddDate(1, 0, 0))
	sc := SAMLConfig{EntityID: "https://ant.example/auth/saml/metadata", IDPEntityID: "https://idp.example/", IDPMetadataXML: idp, GroupsAttribute: "groups"}
	sp, err := s.Create(ctx, actor, Input{Kind: SAML, Name: "saml", Enabled: true, Config: raw(sc)}, b.Check)
	if err != nil {
		t.Fatal(err)
	}
	ls, _ := s.Load(ctx, sp.ID)
	meta, err := b.SPMetadata(ls)
	if err != nil || !strings.Contains(string(meta), "https://ant.example/auth/saml/callback") || strings.Contains(string(meta), "PRIVATE KEY") {
		t.Fatal("SP metadata export", err)
	}
	cfgOut, _ := SAMLConfigOf(sp.Config)
	st := Diagnostics{}.SPCertificateStatus(cfgOut)
	if len(st) != 1 || st[0].Status != "ok" || st[0].DaysLeft < 700 {
		t.Fatalf("%+v", st)
	}
	before := st[0].SHA256
	sp, err = s.Update(ctx, actor, sp.ID, sp.Revision, Input{Name: "saml", Enabled: true, Config: raw(sc), RotateSPCertificate: true}, b.Check)
	if err != nil {
		t.Fatal(err)
	}
	cfgOut, _ = SAMLConfigOf(sp.Config)
	if after := (Diagnostics{}).SPCertificateStatus(cfgOut)[0].SHA256; after == before {
		t.Fatal("certificate not rotated")
	}
	// Expired IdP metadata cannot be enabled.
	expired := sc
	expired.IDPMetadataXML = idpMetadata(t, "https://idp.example/", time.Now().Add(-time.Hour))
	if _, err = s.Update(ctx, actor, sp.ID, sp.Revision, Input{Name: "saml", Enabled: true, Config: raw(expired)}, b.Check); err == nil {
		t.Fatal("expired IdP certificate accepted")
	}
}

func fixtureBinding(t *testing.T, f testdb.Fixture, issuer, subject string) string {
	ctx := context.Background()
	var principal, role string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO principals(instance_id,kind,subject) VALUES($1,'user',$2) RETURNING id::text`, f.InstanceID, "user-"+subject).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(ctx, `INSERT INTO roles(instance_id,name) VALUES($1,'reader') ON CONFLICT(instance_id,name) DO UPDATE SET name='reader' RETURNING id::text`, f.InstanceID).Scan(&role); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO role_grants(instance_id,role_id,kind,effect,actions) SELECT $1,$2::uuid,'instance','allow',ARRAY['read'] WHERE NOT EXISTS(SELECT 1 FROM role_grants WHERE role_id=$2::uuid)`,
	} {
		if _, err := f.Pool.Exec(ctx, q, f.InstanceID, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Pool.Exec(ctx, `INSERT INTO principal_roles(instance_id,principal_id,role_id) VALUES($1,$2,$3)`, f.InstanceID, principal, role); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Pool.Exec(ctx, `INSERT INTO oidc_identity_bindings(instance_id,issuer,subject,principal_id) VALUES($1,$2,$3,$4)`, f.InstanceID, issuer, subject, principal); err != nil {
		t.Fatal(err)
	}
	return principal
}

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func TestMappingPreviewOIDCSAMLLDAP(t *testing.T) {
	f := testdb.New(t)
	ctx := context.Background()
	now := time.Now()
	pv := Preview{DB: f.Pool, InstanceID: f.InstanceID, Now: func() time.Time { return now }}
	principal := fixtureBinding(t, f, "https://idp.example", "alice")
	cfg := OIDCConfig{Issuer: "https://idp.example", ClientID: "ant", GroupsClaim: "roles"}
	good := map[string]any{"iss": cfg.Issuer, "sub": "alice", "aud": "ant", "exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "roles": []string{"eng", "ops"}, "email": "a@example.com"}
	r, err := pv.OIDCToken(ctx, cfg, jwt(good))
	if err != nil || !r.WouldSignIn || r.Verified || !r.Binding.Found || r.Binding.PrincipalID != principal || len(r.Binding.Roles) != 1 || r.Binding.Roles[0] != "reader" || len(r.Subject.Groups) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	// Unbound subject, wrong audience, expired and future-dated tokens are diagnosed.
	unbound := map[string]any{"iss": cfg.Issuer, "sub": "mallory", "aud": "other", "exp": now.Add(-time.Hour).Unix(), "iat": now.Add(5 * time.Minute).Unix()}
	r, _ = pv.OIDCToken(ctx, cfg, jwt(unbound))
	joined := strings.Join(r.Problems, "|")
	for _, want := range []string{"aud", "expired", "future", "no identity binding"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing problem", want, joined)
		}
	}
	if r.WouldSignIn || r.ClockSkewSeconds == nil || *r.ClockSkewSeconds < 200 {
		t.Fatalf("%+v", r)
	}
	if r, _ = pv.OIDCToken(ctx, cfg, "garbage"); len(r.Problems) == 0 {
		t.Fatal("garbage accepted")
	}

	// SAML assertion.
	fixtureBinding(t, f, "https://saml-idp.example/", "nameid-7")
	sc := SAMLConfig{EntityID: "https://ant.example/auth/saml/metadata", IDPEntityID: "https://saml-idp.example/", GroupsAttribute: "memberOf"}
	assertion := func(nameID string, notBefore, notAfter time.Time) string {
		return fmt.Sprintf(`<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion"><saml:Issuer>https://saml-idp.example/</saml:Issuer><saml:Assertion><saml:Issuer>https://saml-idp.example/</saml:Issuer><saml:Subject><saml:NameID>%s</saml:NameID></saml:Subject><saml:Conditions NotBefore="%s" NotOnOrAfter="%s"><saml:AudienceRestriction><saml:Audience>https://ant.example/auth/saml/metadata</saml:Audience></saml:AudienceRestriction></saml:Conditions><saml:AttributeStatement><saml:Attribute Name="memberOf"><saml:AttributeValue>cn=eng</saml:AttributeValue><saml:AttributeValue>cn=ops</saml:AttributeValue></saml:Attribute></saml:AttributeStatement></saml:Assertion></samlp:Response>`,
			nameID, notBefore.UTC().Format(time.RFC3339), notAfter.UTC().Format(time.RFC3339))
	}
	r, err = pv.SAMLAssertion(ctx, sc, base64.StdEncoding.EncodeToString([]byte(assertion("nameid-7", now.Add(-time.Minute), now.Add(time.Hour)))))
	if err != nil || !r.WouldSignIn || len(r.Subject.Groups) != 2 || r.Binding.Roles[0] != "reader" {
		t.Fatalf("%+v %v", r, err)
	}
	r, _ = pv.SAMLAssertion(ctx, sc, assertion("nameid-7", now.Add(10*time.Minute), now.Add(time.Hour)))
	if r.WouldSignIn || !strings.Contains(strings.Join(r.Problems, "|"), "ahead") {
		t.Fatalf("future NotBefore not flagged: %+v", r)
	}
	r, _ = pv.SAMLAssertion(ctx, sc, assertion("stranger", now.Add(-time.Minute), now.Add(-time.Second)))
	if r.WouldSignIn || !strings.Contains(strings.Join(r.Problems, "|"), "expired") {
		t.Fatalf("%+v", r)
	}
	if r, _ = pv.SAMLAssertion(ctx, sc, `<!DOCTYPE x [<!ENTITY a "b">]><Response/>`); len(r.Problems) == 0 {
		t.Fatal("DOCTYPE accepted")
	}

	// LDAP lookup.
	lc := ldapConfig()
	dev := base64.RawURLEncoding.EncodeToString([]byte("immutable-user"))
	fixtureBinding(t, f, lc.ProviderID, dev)
	calls := 0
	conn := fakeConn{search: func(r *ldap.SearchRequest) *ldap.SearchResult {
		calls++
		e := func(dn, id string) *ldap.Entry { return ldap.NewEntry(dn, map[string][]string{"entryUUID": {id}}) }
		switch calls {
		case 1:
			return &ldap.SearchResult{Entries: []*ldap.Entry{e("uid=alice,ou=users,dc=example", "immutable-user")}}
		case 2:
			return &ldap.SearchResult{Entries: []*ldap.Entry{e("cn=dev,ou=groups,dc=example", "dev")}}
		}
		return &ldap.SearchResult{}
	}}
	b := Builder{Dial: func(context.Context, string, *tls.Config) (directory.Connection, error) { return conn, nil }}
	dir, err := b.LDAP(Loaded{Provider: Provider{Kind: LDAP, Config: raw(lc)}, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	r, err = pv.LDAPUser(ctx, dir, lc.ProviderID, "alice")
	if err != nil || !r.WouldSignIn || len(r.ProposedRoles) != 1 || r.ProposedRoles[0] != "reader" || len(r.Subject.Groups) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	d := dir.Diagnose(ctx)
	_ = d
}
