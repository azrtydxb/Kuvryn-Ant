package oidclogin

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

type providerFixture struct {
	mu         sync.Mutex
	server     *httptest.Server
	key        *rsa.PrivateKey
	signingKey *rsa.PrivateKey
	kid        string
	claims     map[string]any
	verifier   string
	jwks       int
	badPKCE    bool
}

func fixtureProvider(t *testing.T) (*providerFixture, *Provider) {
	t.Helper()
	f := &providerFixture{kid: "first", verifier: secret()}
	f.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "jwks_uri": f.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			f.jwks++
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			r.ParseForm()
			if r.Form.Get("code_verifier") != f.verifier || r.Form.Get("redirect_uri") != "https://console.example/callback" || r.Form.Get("grant_type") != "authorization_code" {
				f.badPKCE = true
				http.Error(w, "invalid request", 400)
				return
			}
			signingKey := f.key
			if f.signingKey != nil {
				signingKey = f.signingKey
			}
			signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signingKey}, (&jose.SignerOptions{}).WithHeader("kid", f.kid))
			if e != nil {
				panic(e)
			}
			payload, _ := json.Marshal(f.claims)
			signed, _ := signer.Sign(payload)
			raw, _ := signed.CompactSerialize()
			json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque", "token_type": "Bearer", "id_token": raw})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	f.claims = map[string]any{"iss": f.server.URL, "sub": "immutable-user", "aud": "client", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"}
	p, e := newProvider(context.Background(), ProviderConfig{Key: "fixture", Issuer: f.server.URL, ClientID: "client", RedirectURI: "https://console.example/callback", AllowedEndpoints: []string{f.server.URL + "/authorize", f.server.URL + "/token", f.server.URL + "/keys"}}, http.DefaultTransport, true)
	if e != nil {
		t.Fatal(e)
	}
	return f, p
}
func TestProviderPKCEValidationAndKeyRotation(t *testing.T) {
	f, p := fixtureProvider(t)
	ctx := context.Background()
	u, _ := url.Parse(p.authorization("state", "nonce", f.verifier))
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(f.verifier) || q.Get("nonce") != "nonce" || q.Get("state") != "state" {
		t.Fatal("invalid authorization request")
	}
	if sub, e := p.exchange(ctx, "code", f.verifier, hash("nonce")); e != nil || sub != "immutable-user" {
		t.Fatalf("valid exchange: %s %v", sub, e)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.mu.Lock()
	f.key = key
	f.kid = "rotated"
	f.mu.Unlock()
	if _, e := p.exchange(ctx, "code", f.verifier, hash("nonce")); e != nil {
		t.Fatal("rotation", e)
	}
	for _, tc := range []struct {
		key   string
		value any
	}{{"iss", "https://other.example"}, {"aud", "other-client"}, {"exp", time.Now().Add(-time.Minute).Unix()}, {"iat", time.Now().Add(time.Hour).Unix()}, {"nonce", "wrong"}, {"nbf", time.Now().Add(time.Hour).Unix()}, {"azp", "wrong"}, {"sub", ""}} {
		t.Run(tc.key, func(t *testing.T) {
			f.mu.Lock()
			old, exists := f.claims[tc.key]
			f.claims[tc.key] = tc.value
			f.mu.Unlock()
			if _, e := p.exchange(ctx, "code", f.verifier, hash("nonce")); e == nil {
				t.Fatal("accepted invalid claim")
			}
			f.mu.Lock()
			if exists {
				f.claims[tc.key] = old
			} else {
				delete(f.claims, tc.key)
			}
			f.mu.Unlock()
		})
	}
	wrongKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.mu.Lock()
	f.signingKey = wrongKey
	f.mu.Unlock()
	if _, e := p.exchange(ctx, "code", f.verifier, hash("nonce")); e == nil {
		t.Fatal("invalid signature accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jwks < 2 || f.badPKCE {
		t.Fatal("rotation/PKCE not exercised")
	}
}
func TestDiscoveryOutboundBounds(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", (1<<20)+1))) }))
	defer s.Close()
	client := &http.Client{Transport: boundedTransport{base: http.DefaultTransport, allowed: map[string]bool{s.URL: true}}}
	if _, e := client.Get(s.URL); e == nil {
		t.Fatal("unbounded body accepted")
	}
	if _, e := client.Get(s.URL + "/unapproved"); e == nil {
		t.Fatal("unapproved target accepted")
	}
	if _, e := NewProvider(context.Background(), ProviderConfig{Key: "x", Issuer: s.URL, ClientID: "x", RedirectURI: "https://console.example/callback"}); e == nil {
		t.Fatal("production accepted HTTP")
	}
}
func TestCookieAndCSRFBoundary(t *testing.T) {
	w := httptest.NewRecorder()
	cookie(w, SessionCookie, secret(), time.Now().Add(time.Hour), 3600)
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatal("unsafe cookie")
	}
	client := Client{Origin: "https://console.example"}
	r := httptest.NewRequest("POST", "https://console.example/api", nil)
	r.Header.Set("Origin", "https://evil.example")
	if _, e := client.AuthenticateRequest(r); e == nil {
		t.Fatal("cross-origin mutation")
	}
}

func TestUnapprovedAuthorizationAndRedirectDenied(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, server.URL+"/unexpected", 302)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"issuer": server.URL, "authorization_endpoint": "https://unapproved.example/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/keys"})
	}))
	defer server.Close()
	_, err := newProvider(context.Background(), ProviderConfig{Key: "x", Issuer: server.URL, ClientID: "client", RedirectURI: "https://console.example/callback", AllowedEndpoints: []string{server.URL + "/token", server.URL + "/keys"}}, http.DefaultTransport, true)
	if err == nil {
		t.Fatal("unapproved browser authorization destination accepted")
	}
	_, p := fixtureProvider(t)
	// Every redirect, even to an approved origin, must fail closed.
	p.client.Transport = boundedTransport{base: http.DefaultTransport, allowed: map[string]bool{server.URL + "/redirect": true, server.URL + "/unexpected": true}}
	if _, err = p.client.Get(server.URL + "/redirect"); err == nil {
		t.Fatal("provider redirect followed")
	}
}

func TestSessionLimits(t *testing.T) {
	for _, s := range []Sessions{{MaxAge: 9 * time.Hour}, {IdleTimeout: 31 * time.Minute}, {MaxAge: -time.Second}, {IdleTimeout: -time.Second}} {
		if _, _, e := s.limits(); e == nil {
			t.Fatal("unsafe session policy accepted")
		}
	}
	max, idle, e := (Sessions{}).limits()
	if e != nil || max != 8*time.Hour || idle != 30*time.Minute {
		t.Fatal("invalid defaults")
	}
}
