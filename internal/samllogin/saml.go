// Package samllogin implements a deliberately narrow SP-initiated SAML profile.
// It does not create application sessions or grant roles.
package samllogin

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"github.com/beevik/etree"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

var ErrInvalid = errors.New("invalid SAML message")
var ErrReplay = errors.New("SAML message already consumed")
var ErrUnsupported = errors.New("SAML binding unsupported")

const maxXML = 1 << 20
const requestLifetime = 5 * time.Minute
const assertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"
const protocolNS = "urn:oasis:names:tc:SAML:2.0:protocol"

// ReplayStore must atomically claim ALL keys or none, returning false if ANY
// key was previously claimed. Claims survive restarts until expires. Fail closed
// on storage errors. Keys include provider identity and operation namespaces.
type ReplayStore interface {
	Claim(context.Context, []string, time.Time) (bool, error)
}

// Config is trusted administrator input. Metadata is imported bytes, never a
// remotely fetched URL. Rebuild Provider to atomically publish certificate rollover.
type Config struct {
	EntityID, IDPEntityID                                 string
	ACSURL, MetadataURL, LogoutURL                        string
	Metadata                                              []byte
	Key                                                   crypto.Signer
	Certificate                                           *x509.Certificate
	NameIDFormat                                          saml.NameIDFormat
	EmailAttribute, DisplayNameAttribute, GroupsAttribute string
	Replay                                                ReplayStore
}
type Provider struct {
	sp    saml.ServiceProvider
	cfg   Config
	certs []*x509.Certificate
}

// Pending must be kept in server-side, browser-bound, single-use state. Never
// reconstruct it from callback parameters. RelayState and cookie checks belong
// to the HTTP/session adapter, which must consume state on successful callback.
type Pending struct {
	ID      string
	Expires time.Time
	Logout  bool
}

// Identity is provider-neutral evidence, not authorization. Subject is NameID;
// email and groups must never implicitly provision privileged memberships.
type Identity struct {
	Issuer, Subject, NameIDFormat, Email, DisplayName, SessionIndex string
	Groups                                                          []string
	Attributes                                                      map[string][]string
	Expires                                                         time.Time
}

func New(c Config) (*Provider, error) {
	if c.NameIDFormat == "" {
		c.NameIDFormat = saml.PersistentNameIDFormat
	}
	if c.EntityID == "" || len(c.EntityID) > 2048 || c.IDPEntityID == "" || len(c.IDPEntityID) > 2048 || c.Replay == nil || c.Certificate == nil {
		return nil, ErrInvalid
	}
	key, ok := c.Key.(*rsa.PrivateKey)
	if !ok || key.N.BitLen() < 2048 || !key.PublicKey.Equal(c.Certificate.PublicKey) {
		return nil, ErrInvalid
	}
	now := time.Now()
	if now.Before(c.Certificate.NotBefore) || !now.Before(c.Certificate.NotAfter) {
		return nil, ErrInvalid
	}
	acs, e := httpsURL(c.ACSURL)
	if e != nil {
		return nil, e
	}
	meta, e := httpsURL(c.MetadataURL)
	if e != nil {
		return nil, e
	}
	var slo *url.URL
	if c.LogoutURL != "" {
		slo, e = httpsURL(c.LogoutURL)
		if e != nil {
			return nil, e
		}
	}
	if err := safeXML(c.Metadata, "EntityDescriptor", "urn:oasis:names:tc:SAML:2.0:metadata"); err != nil {
		return nil, err
	}
	var md saml.EntityDescriptor
	if xml.Unmarshal(c.Metadata, &md) != nil || md.EntityID != c.IDPEntityID || len(md.IDPSSODescriptors) != 1 {
		return nil, ErrInvalid
	}
	if !md.ValidUntil.IsZero() && !now.Before(md.ValidUntil) {
		return nil, ErrInvalid
	}
	certCount := 0
	var trustedCerts []*x509.Certificate
	for _, kd := range md.IDPSSODescriptors[0].KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, xc := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(xc.Data), ""))
			if err != nil {
				return nil, ErrInvalid
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				return nil, ErrInvalid
			}
			certCount++
			trustedCerts = append(trustedCerts, cert)
		}
	}
	if certCount == 0 {
		return nil, ErrInvalid
	}
	for _, ep := range md.IDPSSODescriptors[0].SingleSignOnServices {
		if _, err := httpsURL(ep.Location); err != nil {
			return nil, err
		}
	}
	for _, ep := range md.IDPSSODescriptors[0].SingleLogoutServices {
		if _, err := httpsURL(ep.Location); err != nil {
			return nil, err
		}
	}
	p := &Provider{cfg: c, sp: saml.ServiceProvider{EntityID: c.EntityID, Key: c.Key, Certificate: c.Certificate, MetadataURL: *meta, AcsURL: *acs, IDPMetadata: &md, AuthnNameIDFormat: c.NameIDFormat, SignatureMethod: dsig.RSASHA256SignatureMethod, LogoutBindings: []string{saml.HTTPPostBinding}}}
	p.certs = trustedCerts
	if slo != nil {
		p.sp.SloURL = *slo
	}
	if p.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding) == "" {
		return nil, ErrUnsupported
	}
	return p, nil
}
func httpsURL(s string) (*url.URL, error) {
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, ErrInvalid
	}
	return u, nil
}
func (p *Provider) usable() bool {
	v := p.sp.IDPMetadata.ValidUntil
	now := time.Now()
	return (v.IsZero() || now.Before(v)) && !now.Before(p.sp.Certificate.NotBefore) && now.Before(p.sp.Certificate.NotAfter)
}
func (p *Provider) Metadata() ([]byte, error) { return xml.Marshal(p.sp.Metadata()) }
func (p *Provider) Begin(relay string) (*url.URL, Pending, error) {
	if !p.usable() || len(relay) > 80 {
		return nil, Pending{}, ErrInvalid
	}
	r, e := p.sp.MakeAuthenticationRequest(p.sp.GetSSOBindingLocation(saml.HTTPRedirectBinding), saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if e != nil {
		return nil, Pending{}, e
	}
	u, e := r.Redirect(relay, &p.sp)
	return u, Pending{ID: r.ID, Expires: time.Now().Add(requestLifetime)}, e
}
func validPending(s Pending, logout bool) bool {
	return s.ID != "" && len(s.ID) <= 1024 && s.Logout == logout && time.Now().Before(s.Expires) && !s.Expires.After(time.Now().Add(requestLifetime))
}
func (p *Provider) Consume(ctx context.Context, raw []byte, pending Pending) (Identity, error) {
	if !p.usable() || !validPending(pending, false) || safeXML(raw, "Response", protocolNS) != nil {
		return Identity{}, ErrInvalid
	}
	// Preflight prevents library nil dereferences and rejects ambiguous structures;
	// only the assertion returned by the signature library supplies identity data.
	var r saml.Response
	if xml.Unmarshal(raw, &r) != nil || r.Assertion == nil || r.ID == "" || r.Issuer == nil || r.Destination != p.sp.AcsURL.String() || r.InResponseTo != pending.ID || r.Version != "2.0" || r.IssueInstant.After(time.Now()) {
		return Identity{}, ErrInvalid
	}
	if !p.strict(r.Assertion, pending.ID) {
		return Identity{}, ErrInvalid
	}
	a, e := p.sp.ParseXMLResponse(raw, []string{pending.ID}, p.sp.AcsURL)
	if e != nil {
		return Identity{}, ErrInvalid
	}
	if !p.strict(a, pending.ID) || !p.assertionSignature(raw, a.ID) {
		return Identity{}, ErrInvalid
	}
	expires := a.Conditions.NotOnOrAfter
	if t := a.Subject.SubjectConfirmations[0].SubjectConfirmationData.NotOnOrAfter; t.Before(expires) {
		expires = t
	}
	if end := a.AuthnStatements[0].SessionNotOnOrAfter; end != nil && end.Before(expires) {
		expires = *end
	}
	// Retain request claims through the whole pending window, even for short assertions.
	retention := a.Conditions.NotOnOrAfter
	if pending.Expires.After(retention) {
		retention = pending.Expires
	}
	ok, e := p.cfg.Replay.Claim(ctx, []string{p.key("request", pending.ID), p.key("assertion", a.ID)}, retention)
	if e != nil {
		return Identity{}, e
	}
	if !ok {
		return Identity{}, ErrReplay
	}
	out := Identity{Issuer: a.Issuer.Value, Subject: a.Subject.NameID.Value, NameIDFormat: a.Subject.NameID.Format, Attributes: map[string][]string{}, Expires: expires}
	for _, st := range a.AttributeStatements {
		for _, attr := range st.Attributes {
			for _, v := range attr.Values {
				out.Attributes[attr.Name] = append(out.Attributes[attr.Name], v.Value)
			}
		}
	}
	first := func(k string) string {
		v := out.Attributes[k]
		if len(v) > 0 {
			return v[0]
		}
		return ""
	}
	out.Email = first(p.cfg.EmailAttribute)
	out.DisplayName = first(p.cfg.DisplayNameAttribute)
	out.Groups = append([]string(nil), out.Attributes[p.cfg.GroupsAttribute]...)
	if len(a.AuthnStatements) > 0 {
		out.SessionIndex = a.AuthnStatements[0].SessionIndex
	}
	return out, nil
}
func (p *Provider) key(kind, id string) string {
	return "saml:" + base64.RawURLEncoding.EncodeToString([]byte(p.cfg.IDPEntityID)) + ":" + kind + ":" + id
}
func (p *Provider) strict(a *saml.Assertion, id string) bool {
	now := time.Now()
	if a == nil || a.ID == "" || len(a.ID) > 1024 || a.Version != "2.0" || a.Signature == nil || a.Subject == nil || a.Subject.NameID == nil || a.Subject.NameID.Value == "" || a.Conditions == nil || len(a.Subject.SubjectConfirmations) != 1 || a.IssueInstant.After(now) || a.IssueInstant.Add(requestLifetime).Before(now) {
		return false
	}
	c := a.Conditions
	if c.NotBefore.IsZero() || c.NotOnOrAfter.After(now.Add(24*time.Hour)) || now.Before(c.NotBefore) || !now.Before(c.NotOnOrAfter) || len(c.AudienceRestrictions) != 1 || c.AudienceRestrictions[0].Audience.Value != p.cfg.EntityID {
		return false
	}
	if len(a.AuthnStatements) != 1 || a.AuthnStatements[0].AuthnInstant.After(now) {
		return false
	}
	if end := a.AuthnStatements[0].SessionNotOnOrAfter; end != nil && !now.Before(*end) {
		return false
	}
	if p.cfg.NameIDFormat != "" && a.Subject.NameID.Format != string(p.cfg.NameIDFormat) {
		return false
	}
	s := a.Subject.SubjectConfirmations[0]
	d := s.SubjectConfirmationData
	return s.Method == "urn:oasis:names:tc:SAML:2.0:cm:bearer" && d != nil && d.InResponseTo == id && d.Recipient == p.sp.AcsURL.String() && !now.Before(d.NotBefore) && now.Before(d.NotOnOrAfter)
}

// safeXML bounds parsing, excludes DTDs, duplicate IDs, wrapping and ambiguous
// singleton elements. It is structural validation, not XML signature verification.
func safeXML(raw []byte, root, ns string) error {
	if len(raw) == 0 || len(raw) > maxXML {
		return ErrInvalid
	}
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth := 0
	roots := 0
	assertions := 0
	ids := map[string]bool{}
	counts := map[string]int{}
	stack := []string{}
	// Each signature must contain exactly one direct SignedInfo and one direct
	// Reference. This narrow SAML profile excludes multi-reference XMLDSIG,
	// independently of the dependency fix for GO-2026-4753.
	signatureDepth, signedInfoCount, referenceCount := 0, 0, 0
	singleton := map[string]bool{"Subject": true, "NameID": true, "Conditions": true, "AudienceRestriction": true, "Audience": true, "SubjectConfirmation": true, "SubjectConfirmationData": true, "Signature": true, "Issuer": true, "Status": true, "StatusCode": true}
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return ErrInvalid
		}
		switch v := t.(type) {
		case xml.Directive:
			return ErrInvalid
		case xml.StartElement:
			depth++
			if depth > 40 {
				return ErrInvalid
			}
			switch v.Name.Local {
			case "Signature":
				if v.Name.Space != dsig.Namespace || signatureDepth != 0 {
					return ErrInvalid
				}
				signatureDepth, signedInfoCount, referenceCount = depth, 0, 0
			case "SignedInfo":
				if v.Name.Space != dsig.Namespace || signatureDepth == 0 || depth != signatureDepth+1 {
					return ErrInvalid
				}
				signedInfoCount++
				if signedInfoCount != 1 {
					return ErrInvalid
				}
			case "Reference":
				if v.Name.Space != dsig.Namespace || signatureDepth == 0 || depth != signatureDepth+2 || stack[len(stack)-1] != "SignedInfo" {
					return ErrInvalid
				}
				referenceCount++
				if referenceCount != 1 {
					return ErrInvalid
				}
			}
			if depth == 1 {
				roots++
				if v.Name.Local != root || v.Name.Space != ns {
					return ErrInvalid
				}
			}
			if v.Name.Local == "EncryptedAssertion" {
				return ErrUnsupported
			}
			if v.Name.Local == "Assertion" {
				assertions++
				if root != "Response" || depth != 2 || v.Name.Space != assertionNS || assertions > 1 {
					return ErrInvalid
				}
			}
			path := ""
			for _, s := range stack {
				path += "/" + s
			}
			path += "/" + v.Name.Local
			if root == "Response" || root == "LogoutResponse" {
				if singleton[v.Name.Local] {
					expected := assertionNS
					if v.Name.Local == "Signature" {
						expected = "http://www.w3.org/2000/09/xmldsig#"
					}
					if v.Name.Local == "Status" || v.Name.Local == "StatusCode" {
						expected = protocolNS
					}
					if v.Name.Space != expected {
						return ErrInvalid
					}
				}
			}
			if singleton[v.Name.Local] {
				counts[path]++
				if counts[path] > 1 {
					return ErrInvalid
				}
			}
			for _, a := range v.Attr {
				if a.Name.Local == "ID" || a.Name.Local == "Id" || a.Name.Local == "id" {
					if a.Value == "" || ids[a.Value] {
						return ErrInvalid
					}
					ids[a.Value] = true
				}
			}
			stack = append(stack, v.Name.Local)
		case xml.EndElement:
			if v.Name.Local == "Signature" {
				if depth != signatureDepth || signedInfoCount != 1 || referenceCount != 1 {
					return ErrInvalid
				}
				signatureDepth = 0
			}
			depth--
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(v)) != 0 {
				return ErrInvalid
			}
		}
	}
	if roots != 1 || depth != 0 || (root == "Response" && assertions != 1) {
		return ErrInvalid
	}
	return nil
}

// Independently validate the assertion signature even when the response is also
// signed (crewjam otherwise permits response signatures to cover assertions).
// XMLDSIG canonicalization and cryptography remain entirely in goxmldsig.
func (p *Provider) assertionSignature(raw []byte, id string) bool {
	doc := etree.NewDocument()
	if doc.ReadFromBytes(raw) != nil {
		return false
	}
	var el *etree.Element
	for _, child := range doc.Root().ChildElements() {
		if child.Tag == "Assertion" && child.NamespaceURI() == assertionNS {
			el = child
		}
	}
	if el == nil {
		return false
	}
	ctx := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: p.certs})
	ctx.IdAttribute = "ID"
	validated, err := ctx.Validate(el)
	return err == nil && validated.SelectAttrValue("ID", "") == id
}
