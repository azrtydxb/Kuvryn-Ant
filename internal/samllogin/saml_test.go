package samllogin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

type memoryReplay struct {
	mu   sync.Mutex
	keys map[string]bool
	fail bool
}

func (m *memoryReplay) Claim(_ context.Context, keys []string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return false, errors.New("store unavailable")
	}
	for _, k := range keys {
		if m.keys[k] {
			return false, nil
		}
	}
	for _, k := range keys {
		m.keys[k] = true
	}
	return true, nil
}
func certificate(t *testing.T) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	k, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "local-fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	c, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	return k, c
}
func fixture(t *testing.T) (*Provider, *saml.ServiceProvider, Pending) {
	t.Helper()
	k, c := certificate(t)
	cfg := Config{EntityID: "https://sp.test/entity", IDPEntityID: "https://idp.test/entity", ACSURL: "https://sp.test/acs", MetadataURL: "https://sp.test/metadata", LogoutURL: "https://sp.test/logout", Key: k, Certificate: c, EmailAttribute: "email", GroupsAttribute: "groups", Replay: &memoryReplay{keys: map[string]bool{}}}
	cfg.Metadata = metadata(c)
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	_, pending, e := p.Begin("opaque-state")
	if e != nil {
		t.Fatal(e)
	}
	signer := &saml.ServiceProvider{Key: k, Certificate: c, SignatureMethod: dsig.RSASHA256SignatureMethod}
	return p, signer, pending
}
func metadata(certs ...*x509.Certificate) []byte {
	keys := ""
	for _, c := range certs {
		keys += fmt.Sprintf(`<KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo></KeyDescriptor>`, base64.StdEncoding.EncodeToString(c.Raw))
	}
	return []byte(`<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="https://idp.test/entity"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">` + keys + `<SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.test/sso"/><SingleLogoutService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="https://idp.test/logout"/></IDPSSODescriptor></EntityDescriptor>`)
}
func assertion(p *Provider, s Pending) *saml.Assertion {
	now := time.Now().UTC()
	return &saml.Assertion{ID: "assertion-one", Version: "2.0", IssueInstant: now.Add(-time.Second), Issuer: saml.Issuer{Value: p.cfg.IDPEntityID}, Subject: &saml.Subject{NameID: &saml.NameID{Value: "immutable-user", Format: string(saml.PersistentNameIDFormat)}, SubjectConfirmations: []saml.SubjectConfirmation{{Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer", SubjectConfirmationData: &saml.SubjectConfirmationData{Recipient: p.cfg.ACSURL, InResponseTo: s.ID, NotOnOrAfter: now.Add(time.Minute)}}}}, Conditions: &saml.Conditions{NotBefore: now.Add(-time.Minute), NotOnOrAfter: now.Add(time.Minute), AudienceRestrictions: []saml.AudienceRestriction{{Audience: saml.Audience{Value: p.cfg.EntityID}}}}, AuthnStatements: []saml.AuthnStatement{{AuthnInstant: now.Add(-time.Minute), SessionIndex: "session-one"}}, AttributeStatements: []saml.AttributeStatement{{Attributes: []saml.Attribute{{Name: "email", Values: []saml.AttributeValue{{Value: "person@example.test"}}}, {Name: "groups", Values: []saml.AttributeValue{{Value: "readers"}}}}}}}
}
func document(t *testing.T, el *etree.Element) []byte {
	t.Helper()
	d := etree.NewDocument()
	d.SetRoot(el)
	b, e := d.WriteToBytes()
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func signed(t *testing.T, signer *saml.ServiceProvider, el *etree.Element) *etree.Element {
	t.Helper()
	ctx, e := saml.GetSigningContext(signer)
	if e != nil {
		t.Fatal(e)
	}
	out, e := ctx.SignEnveloped(el)
	if e != nil {
		t.Fatal(e)
	}
	return out
}
func response(t *testing.T, p *Provider, signer *saml.ServiceProvider, pending Pending, a *saml.Assertion) []byte {
	t.Helper()
	r := &saml.Response{ID: "response-one", Version: "2.0", IssueInstant: time.Now().Add(-time.Second), Destination: p.cfg.ACSURL, InResponseTo: pending.ID, Issuer: &saml.Issuer{Value: p.cfg.IDPEntityID}, Status: saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}}}
	root := r.Element()
	el := a.Element()
	if signer != nil {
		el = signed(t, signer, el)
	}
	root.AddChild(el)
	return document(t, root)
}
func TestLoginAndReplay(t *testing.T) {
	p, signer, s := fixture(t)
	raw := response(t, p, signer, s, assertion(p, s))
	out, e := p.Consume(context.Background(), raw, s)
	if e != nil {
		t.Fatal(e)
	}
	if out.Subject != "immutable-user" || out.Email != "person@example.test" || out.SessionIndex != "session-one" || len(out.Groups) != 1 {
		t.Fatalf("unexpected identity: %+v", out)
	}
	if _, e = p.Consume(context.Background(), raw, s); !errors.Is(e, ErrReplay) {
		t.Fatalf("replay: %v", e)
	}
}
func TestRejectedAssertions(t *testing.T) {
	p, signer, s := fixture(t)
	cases := map[string]func(*saml.Assertion){"expired": func(a *saml.Assertion) { a.Conditions.NotOnOrAfter = time.Now().Add(-time.Second) }, "future": func(a *saml.Assertion) { a.Conditions.NotBefore = time.Now().Add(time.Minute) }, "audience": func(a *saml.Assertion) { a.Conditions.AudienceRestrictions[0].Audience.Value += "/" }, "no audience": func(a *saml.Assertion) { a.Conditions.AudienceRestrictions = nil }, "recipient": func(a *saml.Assertion) { a.Subject.SubjectConfirmations[0].SubjectConfirmationData.Recipient += "/" }, "request": func(a *saml.Assertion) {
		a.Subject.SubjectConfirmations[0].SubjectConfirmationData.InResponseTo = "other"
	}, "no subject": func(a *saml.Assertion) { a.Subject = nil }, "no conditions": func(a *saml.Assertion) { a.Conditions = nil }, "no confirmation": func(a *saml.Assertion) { a.Subject.SubjectConfirmations = nil }, "no data": func(a *saml.Assertion) { a.Subject.SubjectConfirmations[0].SubjectConfirmationData = nil }, "expired confirmation": func(a *saml.Assertion) {
		a.Subject.SubjectConfirmations[0].SubjectConfirmationData.NotOnOrAfter = time.Now().Add(-time.Second)
	}, "issuer": func(a *saml.Assertion) { a.Issuer.Value = "https://evil.test" }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := assertion(p, s)
			mutate(a)
			raw := response(t, p, signer, s, a)
			if _, e := p.Consume(context.Background(), raw, s); e == nil {
				t.Fatal("accepted invalid assertion")
			}
		})
	}
}
func TestSignatureAndWrappingRejections(t *testing.T) {
	p, signer, s := fixture(t)
	good := response(t, p, signer, s, assertion(p, s))
	k, c := certificate(t)
	untrusted := &saml.ServiceProvider{Key: k, Certificate: c, SignatureMethod: dsig.RSASHA256SignatureMethod}
	cases := map[string][]byte{"unsigned": response(t, p, nil, s, assertion(p, s)), "untrusted": response(t, p, untrusted, s, assertion(p, s)), "tampered": []byte(strings.Replace(string(good), "immutable-user", "attacker", 1)), "malformed": []byte(`<Response><broken></Response>`), "DTD": append([]byte(`<!DOCTYPE foo [<!ENTITY x SYSTEM "file:///etc/passwd">]>`), good...), "oversized": []byte(strings.Repeat("x", maxXML+1))}
	d := etree.NewDocument()
	if e := d.ReadFromBytes(good); e != nil {
		t.Fatal(e)
	}
	a := d.Root().FindElement("./Assertion")
	d.Root().AddChild(a.Copy())
	cases["duplicate assertion"] = document(t, d.Root())
	d = etree.NewDocument()
	_ = d.ReadFromBytes(good)
	a = d.Root().FindElement("./Assertion")
	d.Root().RemoveChild(a)
	d.Root().CreateElement("wrapper").AddChild(a)
	cases["wrapped assertion"] = document(t, d.Root())
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := p.Consume(context.Background(), raw, s); e == nil {
				t.Fatal("accepted hostile XML")
			}
		})
	}
	if _, e := p.Consume(context.Background(), good, Pending{}); e == nil {
		t.Fatal("unsolicited login accepted")
	}
	s.Expires = time.Now().Add(-time.Second)
	if _, e := p.Consume(context.Background(), good, s); e == nil {
		t.Fatal("expired request accepted")
	}
}
func TestCertificateRolloverAndMetadata(t *testing.T) {
	p, old, s := fixture(t)
	k, c := certificate(t)
	cfg := p.cfg
	cfg.Metadata = metadata(old.Certificate, c)
	rotated, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	next := &saml.ServiceProvider{Key: k, Certificate: c, SignatureMethod: dsig.RSASHA256SignatureMethod}
	for i, signer := range []*saml.ServiceProvider{old, next} {
		_, s, e = rotated.Begin("state")
		if e != nil {
			t.Fatal(e)
		}
		a := assertion(rotated, s)
		a.ID = fmt.Sprintf("assertion-%d", i)
		if _, e = rotated.Consume(context.Background(), response(t, rotated, signer, s, a), s); e != nil {
			t.Fatal(e)
		}
	}
	raw, e := rotated.Metadata()
	if e != nil {
		t.Fatal(e)
	}
	var md saml.EntityDescriptor
	if xml.Unmarshal(raw, &md) != nil || md.EntityID != cfg.EntityID {
		t.Fatal("bad exported metadata")
	}
	cfg.Metadata = metadata(c)
	cfg.IDPEntityID = "other"
	if _, e = New(cfg); e == nil {
		t.Fatal("untrusted issuer accepted")
	}
}
func TestStoreFailureAndConcurrentReplay(t *testing.T) {
	p, signer, s := fixture(t)
	raw := response(t, p, signer, s, assertion(p, s))
	store := p.cfg.Replay.(*memoryReplay)
	store.fail = true
	if _, e := p.Consume(context.Background(), raw, s); e == nil {
		t.Fatal("storage failure accepted")
	}
	store.fail = false
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := p.Consume(context.Background(), raw, s); results <- e }()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, ErrReplay) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("successes: %d", success)
	}
}

func TestSignedEnvelopeDoesNotReplaceAssertionSignature(t *testing.T) {
	p, signer, s := fixture(t)
	for _, tamper := range []bool{false, true} {
		raw := response(t, p, signer, s, assertion(p, s))
		d := etree.NewDocument()
		if e := d.ReadFromBytes(raw); e != nil {
			t.Fatal(e)
		}
		a := d.Root().FindElement("./Assertion")
		if tamper {
			a.FindElement("./Subject/NameID").SetText("attacker")
		} else {
			a.RemoveChild(a.FindElement("./Signature"))
		}
		raw = document(t, signed(t, signer, d.Root()))
		if _, e := p.Consume(context.Background(), raw, s); e == nil {
			t.Fatal("signed envelope replaced required valid assertion signature")
		}
	}
}

func TestRequestCorrelationAndCertificateRetirement(t *testing.T) {
	p, old, s := fixture(t)
	good := response(t, p, old, s, assertion(p, s))
	other := s
	other.ID = "other-request"
	if _, e := p.Consume(context.Background(), good, other); e == nil {
		t.Fatal("wrong browser request accepted")
	}
	_, next := certificate(t)
	cfg := p.cfg
	cfg.Metadata = metadata(next)
	retired, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = retired.Consume(context.Background(), good, s); e == nil {
		t.Fatal("retired signer accepted")
	}
	// Failed validations have not consumed the pending request.
	if _, e = p.Consume(context.Background(), good, s); e != nil {
		t.Fatal(e)
	}
}
