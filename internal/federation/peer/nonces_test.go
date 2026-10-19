package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestNonceCacheCapAndExpiry(t *testing.T) {
	var c nonceCache
	now := time.Unix(1_800_000_000, 0)
	for i := range MaxNoncesPerMember {
		if ok, _ := c.add("m", strconv.Itoa(i), now); !ok {
			t.Fatalf("nonce %d refused", i)
		}
	}
	if ok, full := c.add("m", "0", now); ok || full {
		t.Fatal("replay not detected")
	}
	if ok, full := c.add("m", "overflow", now); ok || !full {
		t.Fatal("cache grew past its per-member cap")
	}
	if ok, _ := c.add("other", "overflow", now); !ok {
		t.Fatal("another member was limited")
	}
	// Expired nonces leave from the front; the member can continue.
	later := now.Add(NonceTTL + time.Second)
	if ok, _ := c.add("m", "fresh", later); !ok {
		t.Fatal("expired nonces not released")
	}
	if n := len(c.members["m"].seen); n != 1 {
		t.Fatalf("expired nonces kept: %d", n)
	}
}

func TestMemberRequestRateLimit(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	cert := leaf(t, memberID, 7)
	trust := &fakeTrust{members: map[string]Member{memberID: {InstanceID: memberID, Status: "active", SigningKey: pub, CertSHA256: []string{certSHA(cert)}}},
		revoked: map[string]bool{}, now: time.Unix(1_800_000_000, 0)}
	h := &Handler{Trust: trust, MemberRate: 1, MemberBurst: 3, Mux: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	codes := []int{}
	for i := range 5 {
		r := httptest.NewRequest("GET", "/x", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		ts, nonce := strconv.FormatInt(trust.now.Unix(), 10), fmt.Sprintf("nonce-%020d", i)
		r.Header.Set(HeaderMember, memberID)
		r.Header.Set(HeaderTimestamp, ts)
		r.Header.Set(HeaderNonce, nonce)
		r.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, signingString("GET", "/x", memberID, ts, nonce, nil))))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		codes = append(codes, w.Code)
	}
	if fmt.Sprint(codes) != "[200 200 200 429 429]" {
		t.Fatalf("per-member limit: %v", codes)
	}
}
