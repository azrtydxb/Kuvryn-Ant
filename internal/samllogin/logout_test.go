package samllogin

import (
	"context"
	"encoding/base64"
	"errors"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

func logoutResponse(t *testing.T, p *Provider, signer *saml.ServiceProvider, s Pending, mutate func(*saml.LogoutResponse)) []byte {
	t.Helper()
	r := &saml.LogoutResponse{ID: "logout-response-one", Version: "2.0", InResponseTo: s.ID, Destination: p.cfg.LogoutURL, IssueInstant: time.Now().Add(-time.Second), Issuer: &saml.Issuer{Value: p.cfg.IDPEntityID}, Status: saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}}}
	if mutate != nil {
		mutate(r)
	}
	el := r.Element()
	if signer != nil {
		el = signed(t, signer, el)
	}
	return document(t, el)
}
func TestSignedLogout(t *testing.T) {
	p, signer, _ := fixture(t)
	identity := Identity{Issuer: p.cfg.IDPEntityID, Subject: "user", NameIDFormat: string(saml.PersistentNameIDFormat), SessionIndex: "session-one"}
	form, s, e := p.BeginLogout(identity, "state")
	if e != nil {
		t.Fatal(e)
	}
	// Decode and independently verify the generated request, including the session index.
	match := regexp.MustCompile(`name="SAMLRequest" value="([^"]+)"`).FindSubmatch(form)
	if len(match) != 2 {
		t.Fatalf("missing form request: %s", form)
	}
	raw, e := base64.StdEncoding.DecodeString(html.UnescapeString(string(match[1])))
	if e != nil {
		t.Fatal(e)
	}
	doc := etree.NewDocument()
	if e = doc.ReadFromBytes(raw); e != nil {
		t.Fatal(e)
	}
	ctx := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: p.certs})
	ctx.IdAttribute = "ID"
	verified, e := ctx.Validate(doc.Root())
	if e != nil {
		t.Fatal(e)
	}
	if verified.FindElement("./SessionIndex").Text() != "session-one" {
		t.Fatal("session index missing")
	}
	response := logoutResponse(t, p, signer, s, nil)
	if e = p.ConsumeLogout(context.Background(), response, s); e != nil {
		t.Fatal(e)
	}
	if e = p.ConsumeLogout(context.Background(), response, s); !errors.Is(e, ErrReplay) {
		t.Fatalf("logout replay: %v", e)
	}
}
func TestLogoutRejections(t *testing.T) {
	p, signer, _ := fixture(t)
	_, s, e := p.BeginLogout(Identity{Issuer: p.cfg.IDPEntityID, Subject: "user"}, "state")
	if e != nil {
		t.Fatal(e)
	}
	cases := map[string][]byte{"unsigned": logoutResponse(t, p, nil, s, nil), "uncorrelated": logoutResponse(t, p, signer, s, func(r *saml.LogoutResponse) { r.InResponseTo = "other" }), "wrong destination": logoutResponse(t, p, signer, s, func(r *saml.LogoutResponse) { r.Destination += "/" }), "future": logoutResponse(t, p, signer, s, func(r *saml.LogoutResponse) { r.IssueInstant = time.Now().Add(time.Minute) }), "malformed": []byte(`<samlp:LogoutResponse xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>`)}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if e := p.ConsumeLogout(context.Background(), raw, s); e == nil {
				t.Fatal("accepted invalid logout")
			}
		})
	}
	p.cfg.LogoutURL = ""
	if _, _, e = p.BeginLogout(Identity{}, ""); !errors.Is(e, ErrUnsupported) {
		t.Fatal(e)
	}
}

func logoutRequest(t *testing.T, p *Provider, signer *saml.ServiceProvider, mutate func(*saml.LogoutRequest)) []byte {
	t.Helper()
	r := &saml.LogoutRequest{ID: "idp-logout-one", Version: "2.0", Destination: p.cfg.LogoutURL, IssueInstant: time.Now().Add(-time.Second), Issuer: &saml.Issuer{Value: p.cfg.IDPEntityID}, NameID: &saml.NameID{Value: "immutable-user", Format: string(saml.PersistentNameIDFormat)}, SessionIndex: &saml.SessionIndex{Value: "session-one"}}
	if mutate != nil {
		mutate(r)
	}
	el := r.Element()
	if signer != nil {
		el = signed(t, signer, el)
	}
	return document(t, el)
}

func TestIdPInitiatedLogoutRequest(t *testing.T) {
	p, signer, _ := fixture(t)
	raw := logoutRequest(t, p, signer, nil)
	got, e := p.ConsumeLogoutRequest(context.Background(), raw)
	if e != nil {
		t.Fatal(e)
	}
	if got.ID != "idp-logout-one" || got.NameID != "immutable-user" || got.SessionIndex != "session-one" || got.Issuer != p.cfg.IDPEntityID {
		t.Fatalf("unexpected request: %+v", got)
	}
	if _, e = p.ConsumeLogoutRequest(context.Background(), raw); !errors.Is(e, ErrReplay) {
		t.Fatalf("replayed logout request: %v", e)
	}
	// No SessionIndex means every session of the subject.
	all, e := p.ConsumeLogoutRequest(context.Background(), logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.ID = "idp-logout-two"; r.SessionIndex = nil }))
	if e != nil || all.SessionIndex != "" {
		t.Fatalf("all-session request: %+v %v", all, e)
	}
}

func TestIdPInitiatedLogoutRequestRejections(t *testing.T) {
	p, signer, _ := fixture(t)
	k, c := certificate(t)
	untrusted := &saml.ServiceProvider{Key: k, Certificate: c, SignatureMethod: dsig.RSASHA256SignatureMethod}
	good := logoutRequest(t, p, signer, nil)
	cases := map[string][]byte{
		"unsigned":          logoutRequest(t, p, nil, nil),
		"untrusted signer":  logoutRequest(t, p, untrusted, nil),
		"tampered":          []byte(strings.Replace(string(good), "immutable-user", "someone-else", 1)),
		"wrong issuer":      logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.Issuer.Value = "https://evil.test" }),
		"wrong destination": logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.Destination += "/" }),
		"expired":           logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.IssueInstant = time.Now().Add(-time.Hour) }),
		"future":            logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.IssueInstant = time.Now().Add(time.Hour) }),
		"past NotOnOrAfter": logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { past := time.Now().Add(-time.Second); r.NotOnOrAfter = &past }),
		"no NameID":         logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.NameID = nil }),
		"empty NameID":      logoutRequest(t, p, signer, func(r *saml.LogoutRequest) { r.NameID.Value = "" }),
		"DTD":               append([]byte(`<!DOCTYPE foo [<!ENTITY x SYSTEM "file:///etc/passwd">]>`), good...),
		"response root":     []byte(`<samlp:LogoutResponse xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := p.ConsumeLogoutRequest(context.Background(), raw); e == nil {
				t.Fatal("accepted invalid logout request")
			}
		})
	}
	p.cfg.LogoutURL = ""
	if _, e := p.ConsumeLogoutRequest(context.Background(), good); e == nil {
		t.Fatal("logout request accepted without a logout endpoint")
	}
}

func TestLogoutResponsePostIsSignedSuccess(t *testing.T) {
	p, _, _ := fixture(t)
	post, e := p.LogoutResponsePost("idp-logout-one", "relay")
	if e != nil {
		t.Fatal(e)
	}
	if post.Destination != "https://idp.test/logout" || post.RelayState != "relay" || post.SAMLRequest != "" {
		t.Fatalf("unexpected post: %+v", post)
	}
	raw, e := base64.StdEncoding.DecodeString(post.SAMLResponse)
	if e != nil {
		t.Fatal(e)
	}
	doc := etree.NewDocument()
	if e = doc.ReadFromBytes(raw); e != nil {
		t.Fatal(e)
	}
	// The SP's own certificate verifies the signature (the fixture uses one key pair).
	ctx := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: p.certs})
	ctx.IdAttribute = "ID"
	verified, e := ctx.Validate(doc.Root())
	if e != nil {
		t.Fatal(e)
	}
	if verified.SelectAttrValue("InResponseTo", "") != "idp-logout-one" || !strings.Contains(string(raw), saml.StatusSuccess) {
		t.Fatalf("response is not a correlated success: %s", raw)
	}
	if _, e = p.LogoutResponsePost("", ""); e == nil {
		t.Fatal("empty request id accepted")
	}
}

func TestBeginLogoutPostMatchesForm(t *testing.T) {
	p, _, _ := fixture(t)
	post, pending, e := p.BeginLogoutPost(Identity{Issuer: p.cfg.IDPEntityID, Subject: "user", NameIDFormat: string(saml.PersistentNameIDFormat), SessionIndex: "s1"}, "relay")
	if e != nil {
		t.Fatal(e)
	}
	if post.Destination != "https://idp.test/logout" || post.RelayState != "relay" || post.SAMLRequest == "" || !pending.Logout || pending.ID == "" {
		t.Fatalf("unexpected: %+v %+v", post, pending)
	}
	if !p.SupportsLogout() {
		t.Fatal("logout should be supported")
	}
	if _, _, e = p.BeginLogoutPost(Identity{Issuer: "https://other.test", Subject: "user"}, ""); e == nil {
		t.Fatal("foreign issuer accepted")
	}
}

func TestRoleAttributeMapping(t *testing.T) {
	p, signer, s := fixture(t)
	p.cfg.RolesAttribute = "roles"
	p.cfg.ClaimRoles = map[string][]string{"admins": {"role-b", "role-a"}, "readers": {"role-a"}}
	a := assertion(p, s)
	a.AttributeStatements[0].Attributes = append(a.AttributeStatements[0].Attributes, saml.Attribute{Name: "roles", Values: []saml.AttributeValue{{Value: "admins"}, {Value: "readers"}, {Value: "unmapped"}}})
	out, e := p.Consume(context.Background(), response(t, p, signer, s, a), s)
	if e != nil {
		t.Fatal(e)
	}
	if !out.RolesMapped || strings.Join(out.Roles, ",") != "role-a,role-b" {
		t.Fatalf("roles: %+v", out)
	}
	// Without a mapping nothing is proposed and RolesMapped stays false.
	p2, signer2, s2 := fixture(t)
	out, e = p2.Consume(context.Background(), response(t, p2, signer2, s2, assertion(p2, s2)), s2)
	if e != nil || out.RolesMapped || len(out.Roles) != 0 {
		t.Fatalf("unmapped provider: %+v %v", out, e)
	}
}
