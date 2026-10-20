package oidclogin

import (
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestEndSessionURL(t *testing.T) {
	p := &Provider{endSession: "https://auth.example.com/oidc/v1/end_session", oauth: oauth2.Config{ClientID: "client-1"}}
	raw := p.EndSessionURL("hint.jwt.value", "https://ant.example/console/")
	u, err := url.Parse(raw)
	if err != nil || u.Host != "auth.example.com" || u.Path != "/oidc/v1/end_session" {
		t.Fatal(raw, err)
	}
	q := u.Query()
	if q.Get("id_token_hint") != "hint.jwt.value" || q.Get("client_id") != "client-1" || q.Get("post_logout_redirect_uri") != "https://ant.example/console/" {
		t.Fatal(q)
	}
	if got := (&Provider{}).EndSessionURL("x", "y"); got != "" {
		t.Fatal("no endpoint must yield no URL", got)
	}
	if q := p.EndSessionURL("", ""); q == "" || mustQuery(t, q).Has("id_token_hint") {
		t.Fatal("hint must be omitted when unknown", q)
	}
}

func TestEndSessionEndpointMustBeApproved(t *testing.T) {
	allowed := map[string]bool{"https://logout.partner.example/end": true}
	for endpoint, want := range map[string]bool{
		"https://auth.example.com/oidc/v1/end_session": true,
		"https://logout.partner.example/end":           true,
		"https://evil.example/end":                     false,
		"http://auth.example.com/end":                  false,
	} {
		if sameOrigin(endpoint, "https://auth.example.com", allowed) != want {
			t.Errorf("%s: want %v", endpoint, want)
		}
	}
}

func TestProfileSanitize(t *testing.T) {
	p := Profile{Name: "Ad\na", Email: "a@example.com", Picture: "http://x.example/p.png", IDToken: "t"}.sanitize()
	if p.Name != "" || p.Picture != "" || p.Email != "a@example.com" || p.IDToken != "t" {
		t.Fatalf("%+v", p)
	}
	if got := (Profile{Picture: "https://x.example/p.png"}).sanitize().Picture; got != "https://x.example/p.png" {
		t.Fatal(got)
	}
	if got := (Profile{Picture: "javascript:alert(1)"}).sanitize().Picture; got != "" {
		t.Fatal(got)
	}
}

func TestLoginScopes(t *testing.T) {
	got := loginScopes([]string{"groups", "email", "offline_access"})
	want := []string{"openid", "profile", "email", "groups", "offline_access"}
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatal(got)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}
