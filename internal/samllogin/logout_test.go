package samllogin

import (
	"context"
	"encoding/base64"
	"errors"
	"html"
	"regexp"
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
