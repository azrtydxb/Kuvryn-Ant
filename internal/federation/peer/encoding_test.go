package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Malleate returns another spelling of an unpadded base64url value that the
// lenient standard decoder maps to the same bytes: the unused trailing bits
// of the last character are set (64-byte signatures and 32-byte keys both
// end in a partial sextet).
func Malleate(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, s[len(s)-1])
	return s[:len(s)-1] + string(alphabet[i|1])
}

func TestDecodeCanonicalRejectsAlternateSpellings(t *testing.T) {
	sig := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if _, err := DecodeCanonical(sig); err != nil {
		t.Fatalf("canonical value rejected: %v", err)
	}
	for name, v := range map[string]string{"trailing bits": Malleate(sig), "newline": sig[:10] + "\n" + sig[10:], "carriage return": sig[:10] + "\r" + sig[10:]} {
		if b, err := base64.RawURLEncoding.DecodeString(v); err != nil || len(b) != ed25519.SignatureSize {
			t.Fatalf("%s: precondition, lenient decoder should accept it", name)
		}
		if _, err := DecodeCanonical(v); err == nil {
			t.Errorf("%s: alternate spelling accepted", name)
		}
	}
}

func TestMalleatedSignaturesRejected(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	caller := Member{InstanceID: "aaaaaaaa-0000-4000-8000-000000000001", SigningKey: pub}
	self := "bbbbbbbb-0000-4000-8000-000000000002"
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{}`)
	a := Assertion{Issuer: caller.InstanceID, Federation: "f", Domain: "idp", Subject: "alice", Audience: self,
		Request: RequestHash("POST", "/x", body), IssuedAt: now.Unix(), Expires: now.Add(AssertionLifetime).Unix()}
	h, _ := SignAssertion(a, key)
	if _, err := VerifyAssertion(h, caller, self, now, "POST", "/x", body); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAssertion(Malleate(h), caller, self, now, "POST", "/x", body); err == nil {
		t.Fatal("assertion with a malleated signature encoding accepted")
	}

	// Member request signature.
	trust := &fakeTrust{members: map[string]Member{memberID: {InstanceID: memberID, Status: "active", SigningKey: pub}}, revoked: map[string]bool{}, now: now}
	served := 0
	h2 := &Handler{Trust: trust, Mux: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served++ })}
	cert := leaf(t, memberID, 9)
	r := httptest.NewRequest("GET", "/federation/v1/status", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := "nonce-0123456789abcdef"
	r.Header.Set(HeaderMember, memberID)
	r.Header.Set(HeaderTimestamp, ts)
	r.Header.Set(HeaderNonce, nonce)
	r.Header.Set(HeaderSignature, Malleate(base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signingString("GET", "/federation/v1/status", memberID, ts, nonce, nil)))))
	w := httptest.NewRecorder()
	h2.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || served != 0 {
		t.Fatalf("request with a malleated signature encoding: %d", w.Code)
	}
}
