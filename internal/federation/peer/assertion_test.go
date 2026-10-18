package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestAssertionVerification(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	caller := Member{InstanceID: "aaaaaaaa-0000-4000-8000-000000000001", SigningKey: pub}
	self := "bbbbbbbb-0000-4000-8000-000000000002"
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"kind":"feed"}`)
	base := Assertion{Issuer: caller.InstanceID, Federation: "f", Domain: "idp", Subject: "alice", Audience: self,
		Request: RequestHash("POST", "/federation/v1/access-check", body), IssuedAt: now.Unix(), Expires: now.Add(AssertionLifetime).Unix()}
	sign := func(a Assertion) string {
		h, err := SignAssertion(a, key)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	verify := func(h string, at time.Time, method, uri string, b []byte) error {
		_, err := VerifyAssertion(h, caller, self, at, method, uri, b)
		return err
	}
	if err := verify(sign(base), now, "POST", "/federation/v1/access-check", body); err != nil {
		t.Fatalf("valid assertion: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := SignAssertion(base, other)
	long := base
	long.Expires = now.Add(10 * time.Minute).Unix()
	wrongIss := base
	wrongIss.Issuer = self
	wrongAud := base
	wrongAud.Audience = caller.InstanceID
	h := sign(base)
	payload, sig, _ := strings.Cut(h, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(raw), "alice", "admin", 1))) + "." + sig
	cases := map[string]struct {
		header string
		at     time.Time
		uri    string
		body   []byte
		want   string
	}{
		"forged":            {forged, now, "/federation/v1/access-check", body, "assertion_signature_invalid"},
		"tampered":          {tamperedPayload, now, "/federation/v1/access-check", body, "assertion_signature_invalid"},
		"lifetime too long": {sign(long), now, "/federation/v1/access-check", body, "assertion_malformed"},
		"issuer":            {sign(wrongIss), now, "/federation/v1/access-check", body, "assertion_issuer_mismatch"},
		"audience":          {sign(wrongAud), now, "/federation/v1/access-check", body, "assertion_audience_mismatch"},
		"expired":           {sign(base), now.Add(AssertionLifetime + MaxSkew + time.Second), "/federation/v1/access-check", body, "assertion_clock_skew"},
		"future":            {sign(base), now.Add(-2 * MaxSkew), "/federation/v1/access-check", body, "assertion_clock_skew"},
		"other path":        {sign(base), now, "/federation/v1/search", body, "assertion_request_mismatch"},
		"other body":        {sign(base), now, "/federation/v1/access-check", []byte(`{}`), "assertion_request_mismatch"},
		"garbage":           {"not-an-assertion", now, "/federation/v1/access-check", body, "assertion_malformed"},
	}
	for name, c := range cases {
		if got := AssertionReason(verify(c.header, c.at, "POST", c.uri, c.body)); got != c.want {
			t.Errorf("%s: got %s, want %s", name, got, c.want)
		}
	}
	// Unknown claims are refused (strict decoding).
	extra := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"x","admin":true}`))
	if _, _, _, err := ParseAssertion(extra + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))); err == nil {
		t.Fatal("unknown claim accepted")
	}
}
