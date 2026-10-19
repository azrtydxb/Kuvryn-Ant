package federation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// Numbers beyond 2^53 - 1 cannot be canonicalized for signing: distinct
// integers there share one double, so two generations would sign identically.
// Plain Canonicalize stays exactly RFC 8785.
func TestCanonicalNumbersBounded(t *testing.T) {
	if _, err := CanonicalJSON(json.RawMessage(`{"generation":9007199254740991}`)); err != nil {
		t.Fatalf("largest safe integer refused: %v", err)
	}
	for _, in := range []string{`{"generation":9007199254740993}`, `{"generation":9007199254740992}`, `{"t":-1e300}`} {
		if _, err := CanonicalJSON(json.RawMessage(in)); !errors.Is(err, ErrCanonical) {
			t.Errorf("%s canonicalized", in)
		}
	}
	f := newTestFed(t)
	g2 := f.next(t, addB)
	g2.Generation = 1 << 53
	if err := Sign(g2, f.authority); err == nil {
		t.Fatal("document with an unsafe generation signed")
	}
}

// New federation CAs are name-constrained and limited to TLS use; member
// certificates still verify under them.
func TestCANameConstraints(t *testing.T) {
	ca, cert, err := newCA("lab", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !cert.PermittedDNSDomainsCritical || len(cert.ExcludedEmailAddresses) != 1 || len(cert.ExcludedURIDomains) != 1 || len(cert.ExtKeyUsage) != 2 {
		t.Fatalf("CA constraints missing: %+v %+v %+v", cert.ExcludedEmailAddresses, cert.ExcludedURIDomains, cert.ExtKeyUsage)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr, _ := newCSR(key, tfB, "https://fed.example:8444")
	certPEM, _, _, _, err := issueMemberCert(ca, csr, tfB, "https://fed.example:8444", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	member, _ := parseCertPEM(certPEM)
	if _, err := member.Verify(x509.VerifyOptions{Roots: roots, DNSName: "fed.example", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("member certificate under the constrained CA: %v", err)
	}
	// A certificate with a URI name is refused by the constraints.
	caKey, _, _ := ca.parse()
	u, _ := url.Parse("spiffe://evil/x")
	tpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: tfB}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{u}, DNSNames: []string{"fed.example"}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, cert, &key.PublicKey, caKey)
	bad, _ := x509.ParseCertificate(der)
	if _, err := bad.Verify(x509.VerifyOptions{Roots: roots, DNSName: "fed.example", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("certificate with a URI name accepted under the constrained CA")
	}
}
