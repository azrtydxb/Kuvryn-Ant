package samllogin

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"time"

	"github.com/crewjam/saml"
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
