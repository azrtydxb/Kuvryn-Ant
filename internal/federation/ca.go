package federation

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"time"
)

// Certificate lifetimes. Member certificates renew at two thirds of their life.
const (
	caLifetime     = 10 * 365 * 24 * time.Hour
	memberLifetime = 30 * 24 * time.Hour
)

var errCSR = errors.New("federation: certificate signing request invalid")

// caMaterial is the federation CA private key and certificate, stored
// encrypted with purpose federation-ca on the authority.
type caMaterial struct {
	KeyDER  []byte `json:"key_der"`
	CertPEM string `json:"cert_pem"`
}

// memberMaterial is a member's TLS key and request-signing key, stored
// encrypted with purpose federation-member-key.
type memberMaterial struct {
	TLSKeyDER  []byte `json:"tls_key_der"`
	SigningKey []byte `json:"signing_key"` // ed25519 seed
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func newCA(name string, now time.Time) (caMaterial, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return caMaterial{}, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return caMaterial{}, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Kuvryn Ant federation CA " + name, Organization: []string{"Kuvryn Ant federation"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return caMaterial{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return caMaterial{}, nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	return caMaterial{KeyDER: keyDER, CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}, cert, nil
}

func (c caMaterial) parse() (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := x509.ParseECPrivateKey(c.KeyDER)
	if err != nil {
		return nil, nil, err
	}
	cert, err := parseCertPEM(c.CertPEM)
	return key, cert, err
}

func newMemberMaterial() (memberMaterial, *ecdsa.PrivateKey, ed25519.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return memberMaterial{}, nil, nil, err
	}
	_, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return memberMaterial{}, nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return memberMaterial{}, nil, nil, err
	}
	return memberMaterial{TLSKeyDER: der, SigningKey: sk.Seed()}, key, sk, nil
}

func (m memberMaterial) parse() (*ecdsa.PrivateKey, ed25519.PrivateKey, error) {
	key, err := x509.ParseECPrivateKey(m.TLSKeyDER)
	if err != nil {
		return nil, nil, err
	}
	if len(m.SigningKey) != ed25519.SeedSize {
		return nil, nil, errors.New("federation: member signing key invalid")
	}
	return key, ed25519.NewKeyFromSeed(m.SigningKey), nil
}

func (m memberMaterial) marshal() []byte { b, _ := json.Marshal(m); return b }
func (c caMaterial) marshal() []byte     { b, _ := json.Marshal(c); return b }

// newCSR builds a CSR whose CN is the instance ID and whose SAN is the host of
// the member's federation URL.
func newCSR(key *ecdsa.PrivateKey, instanceID, federationURL string) (string, error) {
	tpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: instanceID}}
	addSAN(&tpl.DNSNames, &tpl.IPAddresses, federationURL)
	der, err := x509.CreateCertificateRequest(rand.Reader, tpl, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

func addSAN(dns *[]string, ips *[]net.IP, federationURL string) {
	u, err := url.Parse(federationURL)
	if err != nil {
		return
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		*ips = append(*ips, ip)
	} else if u.Hostname() != "" {
		*dns = append(*dns, u.Hostname())
	}
}

// issueMemberCert signs a member certificate for a CSR. The CN must equal the
// expected instance ID; SANs are taken from the federation URL recorded by
// the authority, never from the CSR.
func issueMemberCert(ca caMaterial, csrPEM, instanceID, federationURL string, now time.Time) (certPEM string, serial string, sha string, notAfter time.Time, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(csrPEM) > 16384 {
		return "", "", "", time.Time{}, errCSR
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil || csr.Subject.CommonName != instanceID {
		return "", "", "", time.Time{}, errCSR
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return "", "", "", time.Time{}, errCSR
	}
	caKey, caCert, err := ca.parse()
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	sn, err := randomSerial()
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	notAfter = now.Add(memberLifetime).Truncate(time.Second)
	if notAfter.After(caCert.NotAfter) {
		notAfter = caCert.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: instanceID, Organization: []string{"Kuvryn Ant federation member"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	addSAN(&tpl.DNSNames, &tpl.IPAddresses, federationURL)
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), sn.Text(16), CertSHA256(der), notAfter, nil
}

// tlsCertificate assembles the listener/client certificate with its CA chain.
func tlsCertificate(certPEM, caPEM string, key *ecdsa.PrivateKey) (*tls.Certificate, error) {
	leaf, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	ca, err := parseCertPEM(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw, ca.Raw}, PrivateKey: key, Leaf: leaf}, nil
}
