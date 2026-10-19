package samllogin

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

// BeginLogout produces an XML-signed HTTP-POST form. Pass the Identity returned
// from Consume, preserved in the authenticated server-side session. Invalidate
// that local session regardless of the provider logout outcome.
func (p *Provider) BeginLogout(identity Identity, relay string) ([]byte, Pending, error) {
	endpoint := p.sp.GetSLOBindingLocation(saml.HTTPPostBinding)
	if endpoint == "" || p.cfg.LogoutURL == "" {
		return nil, Pending{}, ErrUnsupported
	}
	if !p.usable() || identity.Issuer != p.cfg.IDPEntityID || identity.Subject == "" || len(relay) > 80 {
		return nil, Pending{}, ErrInvalid
	}
	r, e := p.sp.MakeLogoutRequest(endpoint, identity.Subject)
	if e != nil {
		return nil, Pending{}, e
	}
	r.NameID.Format = identity.NameIDFormat
	if identity.SessionIndex != "" {
		r.SessionIndex = &saml.SessionIndex{Value: identity.SessionIndex}
	}
	// Re-sign after setting the exact NameID format and session index.
	r.Signature = nil
	if e = p.sp.SignLogoutRequest(r); e != nil {
		return nil, Pending{}, e
	}
	return r.Post(relay), Pending{ID: r.ID, Expires: time.Now().Add(requestLifetime), Logout: true}, nil
}

// ConsumeLogout accepts only signed POST LogoutResponses correlated to a
// browser-bound SP request. Unsolicited and Redirect-binding logout are excluded.
func (p *Provider) ConsumeLogout(ctx context.Context, raw []byte, pending Pending) error {
	if !p.usable() || !validPending(pending, true) || safeXML(raw, "LogoutResponse", protocolNS) != nil {
		return ErrInvalid
	}
	var r saml.LogoutResponse
	if xml.Unmarshal(raw, &r) != nil || r.ID == "" || len(r.ID) > 1024 || r.Version != "2.0" || r.Issuer == nil || r.Signature == nil || r.InResponseTo != pending.ID || r.IssueInstant.After(time.Now()) {
		return ErrInvalid
	}
	if p.sp.ValidateLogoutResponseForm(base64.StdEncoding.EncodeToString(raw)) != nil {
		return ErrInvalid
	}
	ok, e := p.cfg.Replay.Claim(ctx, []string{p.key("logout-request", pending.ID), p.key("logout-response", r.ID)}, pending.Expires)
	if e != nil {
		return e
	}
	if !ok {
		return ErrReplay
	}
	return nil
}

// LogoutPost is an HTTP-POST binding payload for the browser to submit to the
// identity provider: SAMLRequest or SAMLResponse (exactly one is set).
type LogoutPost struct {
	Destination  string
	SAMLRequest  string
	SAMLResponse string
	RelayState   string
}

// SupportsLogout reports whether single logout is usable: the IdP publishes an
// HTTP-POST single logout endpoint and this SP has a logout endpoint.
func (p *Provider) SupportsLogout() bool {
	return p.cfg.LogoutURL != "" && p.sp.GetSLOBindingLocation(saml.HTTPPostBinding) != ""
}

func encodeElement(r interface{ Element() *etree.Element }) (string, error) {
	doc := etree.NewDocument()
	doc.SetRoot(r.Element())
	b, e := doc.WriteToBytes()
	if e != nil {
		return "", e
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// BeginLogoutPost is BeginLogout returning the form fields instead of HTML, so
// the HTTP adapter can serve its own page under a strict content policy.
func (p *Provider) BeginLogoutPost(identity Identity, relay string) (LogoutPost, Pending, error) {
	endpoint := p.sp.GetSLOBindingLocation(saml.HTTPPostBinding)
	if endpoint == "" || p.cfg.LogoutURL == "" {
		return LogoutPost{}, Pending{}, ErrUnsupported
	}
	if !p.usable() || identity.Issuer != p.cfg.IDPEntityID || identity.Subject == "" || len(relay) > 80 {
		return LogoutPost{}, Pending{}, ErrInvalid
	}
	r, e := p.sp.MakeLogoutRequest(endpoint, identity.Subject)
	if e != nil {
		return LogoutPost{}, Pending{}, e
	}
	if identity.NameIDFormat != "" {
		r.NameID.Format = identity.NameIDFormat
	}
	if identity.SessionIndex != "" {
		r.SessionIndex = &saml.SessionIndex{Value: identity.SessionIndex}
	}
	r.Signature = nil
	if e = p.sp.SignLogoutRequest(r); e != nil {
		return LogoutPost{}, Pending{}, e
	}
	enc, e := encodeElement(r)
	if e != nil {
		return LogoutPost{}, Pending{}, e
	}
	return LogoutPost{Destination: r.Destination, SAMLRequest: enc, RelayState: relay}, Pending{ID: r.ID, Expires: time.Now().Add(requestLifetime), Logout: true}, nil
}

// LogoutRequest is a validated, IdP-initiated logout request. NameID and
// SessionIndex identify the sessions to end; SessionIndex is empty when the IdP
// wants every session of the subject ended.
type LogoutRequest struct {
	ID, Issuer, NameID, SessionIndex string
}

// ConsumeLogoutRequest accepts only signed, unexpired, single-use POST
// LogoutRequests from the configured IdP addressed to this SP's logout endpoint.
// It changes nothing: the caller ends the matching sessions.
func (p *Provider) ConsumeLogoutRequest(ctx context.Context, raw []byte) (LogoutRequest, error) {
	if !p.usable() || !p.SupportsLogout() || safeXML(raw, "LogoutRequest", protocolNS) != nil {
		return LogoutRequest{}, ErrInvalid
	}
	var r saml.LogoutRequest
	now := time.Now()
	if xml.Unmarshal(raw, &r) != nil || r.ID == "" || len(r.ID) > 1024 || r.Version != "2.0" || r.Issuer == nil || r.Issuer.Value != p.cfg.IDPEntityID || r.Signature == nil ||
		r.Destination != p.cfg.LogoutURL || r.NameID == nil || r.NameID.Value == "" || len(r.NameID.Value) > 2048 ||
		r.IssueInstant.After(now.Add(time.Minute)) || r.IssueInstant.Add(requestLifetime).Before(now) || (r.NotOnOrAfter != nil && !now.Before(*r.NotOnOrAfter)) {
		return LogoutRequest{}, ErrInvalid
	}
	if r.SessionIndex != nil && len(r.SessionIndex.Value) > 1024 {
		return LogoutRequest{}, ErrInvalid
	}
	if !p.rootSignature(raw, "LogoutRequest", r.ID) {
		return LogoutRequest{}, ErrInvalid
	}
	ok, e := p.cfg.Replay.Claim(ctx, []string{p.key("idp-logout-request", r.ID)}, r.IssueInstant.Add(requestLifetime+time.Minute))
	if e != nil {
		return LogoutRequest{}, e
	}
	if !ok {
		return LogoutRequest{}, ErrReplay
	}
	out := LogoutRequest{ID: r.ID, Issuer: r.Issuer.Value, NameID: r.NameID.Value}
	if r.SessionIndex != nil {
		out.SessionIndex = r.SessionIndex.Value
	}
	return out, nil
}

// LogoutResponsePost builds the signed Success LogoutResponse answering an
// IdP-initiated request, for the browser to POST back to the IdP.
func (p *Provider) LogoutResponsePost(requestID, relay string) (LogoutPost, error) {
	endpoint := p.sp.GetSLOBindingLocation(saml.HTTPPostBinding)
	if endpoint == "" || p.cfg.LogoutURL == "" {
		return LogoutPost{}, ErrUnsupported
	}
	if !p.usable() || requestID == "" || len(relay) > 80 {
		return LogoutPost{}, ErrInvalid
	}
	r, e := p.sp.MakeLogoutResponse(endpoint, requestID)
	if e != nil {
		return LogoutPost{}, e
	}
	enc, e := encodeElement(r)
	if e != nil {
		return LogoutPost{}, e
	}
	return LogoutPost{Destination: endpoint, SAMLResponse: enc, RelayState: relay}, nil
}

// rootSignature verifies the enveloped signature of the document root, which
// must be the expected element carrying the given ID.
func (p *Provider) rootSignature(raw []byte, tag, id string) bool {
	doc := etree.NewDocument()
	if doc.ReadFromBytes(raw) != nil || doc.Root() == nil || doc.Root().Tag != tag || doc.Root().SelectAttrValue("ID", "") != id {
		return false
	}
	ctx := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: p.certs})
	ctx.IdAttribute = "ID"
	validated, err := ctx.Validate(doc.Root())
	return err == nil && validated.SelectAttrValue("ID", "") == id
}
