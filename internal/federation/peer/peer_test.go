package peer

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeTrust struct {
	members map[string]Member
	revoked map[string]bool
	now     time.Time
}

func (f *fakeTrust) ServerCertificate() *tls.Certificate { return nil }
func (f *fakeTrust) CAPool() *x509.CertPool              { return x509.NewCertPool() }
func (f *fakeTrust) Revoked(serial string) bool          { return f.revoked[serial] }
func (f *fakeTrust) Lookup(id string) (Member, bool)     { m, ok := f.members[id]; return m, ok }
func (f *fakeTrust) Now() time.Time                      { return f.now }

const memberID = "11111111-2222-4333-8444-555555555555"

func leaf(t *testing.T, cn string, serial int64) *x509.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func TestHandlerAuthentication(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	trust := &fakeTrust{members: map[string]Member{memberID: {InstanceID: memberID, Name: "beta", Status: "active", SigningKey: pub}}, revoked: map[string]bool{}, now: time.Now()}
	served := 0
	h := &Handler{Trust: trust, Public: map[string]bool{"POST /join": true}, Mux: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m, ok := Caller(r); r.URL.Path != "/join" && (!ok || m.InstanceID != memberID) {
			t.Error("caller missing")
		}
		served++
	})}
	cert := leaf(t, memberID, 7)
	request := func(method, path, body string, certs bool, sign func(r *http.Request)) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.TLS = &tls.ConnectionState{}
		if certs {
			r.TLS.PeerCertificates = []*x509.Certificate{cert}
			r.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
		}
		if sign != nil {
			sign(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	signer := func(ts time.Time, nonce, member string, key ed25519.PrivateKey, body string) func(r *http.Request) {
		return func(r *http.Request) {
			s := strconv.FormatInt(ts.Unix(), 10)
			r.Header.Set(HeaderMember, member)
			r.Header.Set(HeaderTimestamp, s)
			r.Header.Set(HeaderNonce, nonce)
			r.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signingString(r.Method, r.URL.RequestURI(), member, s, nonce, []byte(body)))))
		}
	}
	nonce := strings.Repeat("n", 24)
	if c := request("GET", "/heads", "", true, signer(trust.now, nonce, memberID, priv, "")); c != 200 {
		t.Fatalf("valid request: %d", c)
	}
	if c := request("GET", "/heads", "", true, signer(trust.now, nonce, memberID, priv, "")); c != 401 {
		t.Fatalf("replayed nonce: %d", c)
	}
	if c := request("GET", "/heads", "", true, signer(trust.now.Add(-2*MaxSkew), strings.Repeat("s", 24), memberID, priv, "")); c != 401 {
		t.Fatalf("clock skew: %d", c)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if c := request("GET", "/heads", "", true, signer(trust.now, strings.Repeat("o", 24), memberID, other, "")); c != 401 {
		t.Fatalf("foreign signing key: %d", c)
	}
	// The signature binds the body.
	tampered := func(r *http.Request) {
		signer(trust.now, strings.Repeat("b", 24), memberID, priv, `{"a":1}`)(r)
	}
	if c := request("POST", "/leave", `{"a":2}`, true, tampered); c != 401 {
		t.Fatalf("body substitution: %d", c)
	}
	if c := request("GET", "/heads", "", true, signer(trust.now, strings.Repeat("m", 24), "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", priv, "")); c != 401 {
		t.Fatalf("member header mismatch: %d", c)
	}
	if c := request("GET", "/heads", "", false, nil); c != 401 {
		t.Fatalf("no client certificate: %d", c)
	}
	if c := request("POST", "/join", "{}", false, nil); c != 200 {
		t.Fatalf("public join path: %d", c)
	}
	trust.revoked["7"] = true
	if c := request("GET", "/heads", "", true, signer(trust.now, strings.Repeat("r", 24), memberID, priv, "")); c != 403 {
		t.Fatalf("revoked certificate: %d", c)
	}
	delete(trust.revoked, "7")
	m := trust.members[memberID]
	m.Status = "suspended"
	trust.members[memberID] = m
	if c := request("GET", "/heads", "", true, signer(trust.now, strings.Repeat("u", 24), memberID, priv, "")); c != 403 {
		t.Fatalf("suspended member: %d", c)
	}
	delete(trust.members, memberID)
	if c := request("GET", "/heads", "", true, signer(trust.now, strings.Repeat("x", 24), memberID, priv, "")); c != 403 {
		t.Fatalf("unknown member: %d", c)
	}
	if served != 2 {
		t.Fatalf("handler served %d requests, want 2", served)
	}
}

func TestVerifyPinnedRequiresPinnedCA(t *testing.T) {
	c := leaf(t, "x", 1)
	if err := verifyPinned([]*x509.Certificate{c}, strings.Repeat("0", 64), "127.0.0.1"); err == nil {
		t.Fatal("chain without the pinned CA accepted")
	}
	if err := verifyPinned(nil, strings.Repeat("0", 64), "127.0.0.1"); err == nil {
		t.Fatal("empty chain accepted")
	}
}
