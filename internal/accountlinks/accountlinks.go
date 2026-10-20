// Package accountlinks decides how a signed-in user changes their password.
//
// Local accounts change it in Ant. Everyone else owns their credential at an
// identity provider, so the console forwards them there. The destination is
// never taken from a request: it is an operator-configured URL, or a fixed
// derivation from the operator-configured issuer of a recognised product.
package accountlinks

import (
	"net/url"
	"strings"
)

// Mode says where a password is changed.
type Mode string

const (
	// ModeLocal: Ant owns the password; use POST /api/v1/me/password.
	ModeLocal Mode = "local"
	// ModeExternal: the identity provider owns it; open URL.
	ModeExternal Mode = "external"
	// ModeUnavailable: nothing to open; Reason explains who manages it.
	ModeUnavailable Mode = "unavailable"
)

// Change is the password-change capability of one session.
type Change struct {
	Mode   Mode   `json:"mode"`
	URL    string `json:"url,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Method is how the session was authenticated.
type Method string

const (
	MethodLocal Method = "local"
	MethodOIDC  Method = "oidc"
	MethodSAML  Method = "saml"
	MethodLDAP  Method = "ldap"
	MethodToken Method = "token"
)

// Provider is the operator configuration of the identity provider behind a
// session. Configured is its explicit password-change URL; Name is the
// administrator-chosen display name. Endpoints are the operator-approved OIDC
// endpoints, used only to recognise the product behind an issuer.
type Provider struct {
	Name       string
	Issuer     string
	Configured string
	Endpoints  []string
}

// ValidURL accepts only absolute https URLs without credentials. A fragment is
// allowed (Keycloak routes its account console by fragment).
func ValidURL(raw string) bool {
	if raw == "" || len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Hostname() == "" {
		return false
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f || r == '\\' {
			return false
		}
	}
	return true
}

func managedBy(name string) string {
	if name == "" {
		return "Your password is managed by your identity provider."
	}
	return "Your password is managed by " + name + "."
}

// For returns the password-change capability for a session.
func For(method Method, p Provider) Change {
	switch method {
	case MethodLocal:
		return Change{Mode: ModeLocal}
	case MethodToken:
		return Change{Mode: ModeUnavailable, Reason: "API token sessions have no password."}
	case MethodLDAP:
		if ValidURL(p.Configured) {
			return Change{Mode: ModeExternal, URL: p.Configured}
		}
		return Change{Mode: ModeUnavailable, Reason: "Your password is managed by your directory."}
	case MethodSAML:
		if ValidURL(p.Configured) {
			return Change{Mode: ModeExternal, URL: p.Configured}
		}
		return Change{Mode: ModeUnavailable, Reason: managedBy(p.Name)}
	case MethodOIDC:
		if ValidURL(p.Configured) {
			return Change{Mode: ModeExternal, URL: p.Configured}
		}
		if u := Derive(p.Issuer, p.Endpoints); u != "" {
			return Change{Mode: ModeExternal, URL: u}
		}
		return Change{Mode: ModeUnavailable, Reason: managedBy(p.Name)}
	}
	return Change{Mode: ModeUnavailable, Reason: managedBy(p.Name)}
}

// Derive returns the self-service password page of a recognised OIDC product,
// or "" when the product is not known. Recognition uses only the operator's
// issuer and approved endpoints.
//
//   - Zitadel: the Console "change password" page of the signed-in user,
//     <issuer>/ui/console/users/me/password (Console route "me/password", which
//     asks for the current password). Zitadel Cloud issuers are *.zitadel.cloud;
//     a self-hosted instance is recognised by its /oauth/v2/ endpoints.
//   - Keycloak: <issuer>/account/#/security/signingin (issuer is the realm URL).
//   - Microsoft Entra: https://mysignins.microsoft.com/security-info.
//   - Google: https://myaccount.google.com/security.
//   - Okta: <org>/enduser/settings (org is the scheme and host of the issuer).
func Derive(issuer string, endpoints []string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	base := strings.TrimSuffix(issuer, "/")
	has := func(suffix string) bool {
		for _, e := range endpoints {
			if strings.TrimSuffix(e, "/") == base+suffix {
				return true
			}
		}
		return false
	}
	switch {
	case host == "accounts.google.com":
		return "https://myaccount.google.com/security"
	case host == "login.microsoftonline.com" || host == "sts.windows.net":
		return "https://mysignins.microsoft.com/security-info"
	case host == "okta.com" || strings.HasSuffix(host, ".okta.com") || strings.HasSuffix(host, ".oktapreview.com") || strings.HasSuffix(host, ".okta-emea.com"):
		return "https://" + u.Host + "/enduser/settings"
	case strings.HasSuffix(host, ".zitadel.cloud") || has("/oauth/v2/token") || has("/oauth/v2/authorize"):
		return base + "/ui/console/users/me/password"
	case strings.Contains(u.Path, "/realms/") || has("/protocol/openid-connect/token") || has("/protocol/openid-connect/auth"):
		return base + "/account/#/security/signingin"
	}
	return ""
}
