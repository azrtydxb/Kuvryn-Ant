package accountlinks

import "testing"

func TestDerive(t *testing.T) {
	cases := []struct {
		name, issuer string
		endpoints    []string
		want         string
	}{
		{"zitadel cloud", "https://acme-x1.zitadel.cloud", nil, "https://acme-x1.zitadel.cloud/ui/console/users/me/password"},
		{"zitadel self-hosted by endpoints", "https://auth.example.com", []string{"https://auth.example.com/oauth/v2/authorize", "https://auth.example.com/oauth/v2/token", "https://auth.example.com/oauth/v2/keys"}, "https://auth.example.com/ui/console/users/me/password"},
		{"zitadel trailing slash", "https://auth.example.com/", []string{"https://auth.example.com/oauth/v2/token"}, "https://auth.example.com/ui/console/users/me/password"},
		{"keycloak by realm path", "https://sso.example.com/realms/ant", nil, "https://sso.example.com/realms/ant/account/#/security/signingin"},
		{"keycloak legacy auth path", "https://sso.example.com/auth/realms/ant", nil, "https://sso.example.com/auth/realms/ant/account/#/security/signingin"},
		{"entra v2", "https://login.microsoftonline.com/0000/v2.0", nil, "https://mysignins.microsoft.com/security-info"},
		{"google", "https://accounts.google.com", nil, "https://myaccount.google.com/security"},
		{"okta custom auth server", "https://dev-1.okta.com/oauth2/default", nil, "https://dev-1.okta.com/enduser/settings"},
		{"okta org", "https://acme.oktapreview.com", nil, "https://acme.oktapreview.com/enduser/settings"},
		{"unknown", "https://idp.example.com", nil, ""},
		{"http rejected", "http://acme.zitadel.cloud", nil, ""},
		{"userinfo rejected", "https://u:p@acme.zitadel.cloud", nil, ""},
		{"lookalike host", "https://accounts.google.com.evil.example", nil, ""},
		{"lookalike okta", "https://evilokta.com", nil, ""},
		{"empty", "", nil, ""},
	}
	for _, c := range cases {
		if got := Derive(c.issuer, c.endpoints); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestFor(t *testing.T) {
	zit := Provider{Name: "Zitadel", Issuer: "https://auth.example.com", Endpoints: []string{"https://auth.example.com/oauth/v2/token"}}
	if c := For(MethodLocal, Provider{}); c.Mode != ModeLocal || c.URL != "" {
		t.Fatalf("local: %+v", c)
	}
	if c := For(MethodOIDC, zit); c.Mode != ModeExternal || c.URL != "https://auth.example.com/ui/console/users/me/password" {
		t.Fatalf("oidc derived: %+v", c)
	}
	zit.Configured = "https://account.example.com/password"
	if c := For(MethodOIDC, zit); c.URL != "https://account.example.com/password" {
		t.Fatalf("explicit override must win: %+v", c)
	}
	zit.Configured = "http://account.example.com/password"
	if c := For(MethodOIDC, zit); c.URL != "https://auth.example.com/ui/console/users/me/password" {
		t.Fatalf("non-https override must be ignored: %+v", c)
	}
	zit.Configured = "javascript:alert(1)"
	if c := For(MethodOIDC, zit); c.Mode != ModeExternal || c.URL == "javascript:alert(1)" {
		t.Fatalf("scheme: %+v", c)
	}
	unknown := Provider{Name: "Corp SSO", Issuer: "https://idp.example.com"}
	if c := For(MethodOIDC, unknown); c.Mode != ModeUnavailable || c.Reason != "Your password is managed by Corp SSO." || c.URL != "" {
		t.Fatalf("unknown oidc: %+v", c)
	}
	if c := For(MethodSAML, unknown); c.Mode != ModeUnavailable {
		t.Fatalf("saml: %+v", c)
	}
	unknown.Configured = "https://sso.example.com/pw"
	if c := For(MethodSAML, unknown); c.Mode != ModeExternal || c.URL != unknown.Configured {
		t.Fatalf("saml configured: %+v", c)
	}
	if c := For(MethodLDAP, Provider{}); c.Mode != ModeUnavailable || c.Reason != "Your password is managed by your directory." {
		t.Fatalf("ldap: %+v", c)
	}
	if c := For(MethodLDAP, Provider{Configured: "https://pw.example.com/"}); c.Mode != ModeExternal {
		t.Fatalf("ldap configured: %+v", c)
	}
	if c := For(MethodToken, zit); c.Mode != ModeUnavailable || c.URL != "" {
		t.Fatalf("token: %+v", c)
	}
}

func TestValidURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://a.example.com/x": true, "https://a.example.com/#/s": true,
		"http://a.example.com": false, "//a.example.com": false, "javascript:alert(1)": false,
		"https://u@a.example.com": false, "https:///x": false, "": false,
		"https://a.example.com/ x": false, "https://a.example.com/\\x": false,
	} {
		if ValidURL(raw) != want {
			t.Errorf("%q want %v", raw, want)
		}
	}
}
