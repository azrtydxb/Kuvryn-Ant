package federation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
)

// Protocol versions this build speaks on the federation listener.
const (
	ProtocolMin = 1
	ProtocolMax = 1
)

// Document verification failures. Each is a distinct, audited rejection reason.
var (
	ErrSignature      = errors.New("federation: document signature invalid")
	ErrChain          = errors.New("federation: document does not extend the applied chain")
	ErrStaleGen       = errors.New("federation: document generation is not newer than the applied generation")
	ErrStaleEpoch     = errors.New("federation: document authority epoch is older than the applied epoch")
	ErrGap            = errors.New("federation: document generation skips missing generations")
	ErrAuthorityCert  = errors.New("federation: authority key certification invalid")
	ErrRecoveryChange = errors.New("federation: recovery key changed without a valid rotation statement")
	ErrMalformed      = errors.New("federation: document malformed")
	ErrWrongFed       = errors.New("federation: document belongs to another federation")
)

// RejectReason maps a verification error to the stable code recorded in audit
// and returned by the API.
func RejectReason(err error) string {
	for _, c := range []struct {
		err  error
		code string
	}{{ErrSignature, "bad_signature"}, {ErrChain, "chain_mismatch"}, {ErrStaleGen, "stale_generation"}, {ErrStaleEpoch, "stale_epoch"},
		{ErrGap, "generation_gap"}, {ErrAuthorityCert, "bad_authority_certificate"}, {ErrRecoveryChange, "bad_recovery_rotation"},
		{ErrMalformed, "malformed"}, {ErrWrongFed, "wrong_federation"}} {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return "invalid"
}

// Member is one entry of a membership document.
type Member struct {
	InstanceID      string            `json:"instance_id"`
	Name            string            `json:"name"`
	Status          string            `json:"status"` // active | suspended | removed
	APIURL          string            `json:"api_url"`
	FederationURL   string            `json:"federation_url"`
	CertSHA256      string            `json:"cert_sha256"`
	CertSerial      string            `json:"cert_serial"`
	SigningKey      string            `json:"signing_key"` // ed25519:<base64url>
	Labels          map[string]string `json:"labels"`
	SoftwareVersion string            `json:"software_version"`
	Protocol        ProtocolRange     `json:"protocol"`
	JoinedAt        string            `json:"joined_at"`
}

// ProtocolRange is an inclusive range of peer protocol versions.
type ProtocolRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// AuthorityCert certifies the authority signing key of one epoch. Epoch keys
// are certified by the offline recovery key (founding, forced recovery,
// recovery-key rotation) or by the previous epoch's key (planned handover).
type AuthorityCert struct {
	Epoch       int64  `json:"epoch"`
	InstanceID  string `json:"instance_id"`
	PublicKey   string `json:"public_key"`
	CertifiedBy string `json:"certified_by"` // recovery | previous_epoch
	Signature   string `json:"signature"`
}

// RecoveryRotation is the statement, signed by the previous recovery key,
// that replaces the federation recovery key.
type RecoveryRotation struct {
	PreviousKey string `json:"previous_key"`
	NewKey      string `json:"new_key"`
	Generation  int64  `json:"generation"`
	Signature   string `json:"signature"`
}

// Signature is the detached document signature.
type Signature struct {
	KeyID string `json:"key_id"`
	Alg   string `json:"alg"`
	Value string `json:"value"`
}

// Membership is the signed, versioned membership document (§4.1).
type Membership struct {
	Kind               string            `json:"kind"`
	FederationID       string            `json:"federation_id"`
	FederationName     string            `json:"federation_name"`
	Generation         int64             `json:"generation"`
	AuthorityEpoch     int64             `json:"authority_epoch"`
	PrevHash           string            `json:"prev_hash"`
	Protocol           ProtocolRange     `json:"protocol"`
	Authority          AuthorityCert     `json:"authority"`
	RecoveryPublicKey  string            `json:"recovery_public_key"`
	RecoveryRotation   *RecoveryRotation `json:"recovery_rotation,omitempty"`
	CACertificates     []string          `json:"ca_certificates"`
	Members            []Member          `json:"members"`
	RevokedCertSerials []string          `json:"revoked_cert_serials"`
	IssuedAt           string            `json:"issued_at"`
	IssuedBy           string            `json:"issued_by"` // principal or peer that caused this generation
	Signature          *Signature        `json:"signature,omitempty"`
}

// Member returns the entry for an instance ID.
func (m *Membership) Member(id string) (Member, bool) {
	for _, x := range m.Members {
		if x.InstanceID == id {
			return x, true
		}
	}
	return Member{}, false
}

// Revoked reports whether a certificate serial is revoked.
func (m *Membership) Revoked(serial string) bool {
	for _, s := range m.RevokedCertSerials {
		if s == serial {
			return true
		}
	}
	return false
}

// Clone returns a deep copy without the signature, ready for the next generation.
func (m *Membership) Clone() *Membership {
	raw, _ := json.Marshal(m)
	var c Membership
	_ = json.Unmarshal(raw, &c)
	c.Signature = nil
	c.RecoveryRotation = nil
	return &c
}

// --- key encoding ---

const keyPrefix = "ed25519:"

func encodeKey(pub ed25519.PublicKey) string {
	return keyPrefix + base64.RawURLEncoding.EncodeToString(pub)
}

func decodeKey(s string) (ed25519.PublicKey, error) {
	raw, ok := strings.CutPrefix(s, keyPrefix)
	if !ok {
		return nil, ErrMalformed
	}
	b, err := peer.DecodeCanonical(raw)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrMalformed
	}
	return ed25519.PublicKey(b), nil
}

// KeyFingerprint is a short, display-safe fingerprint of an encoded public key.
func KeyFingerprint(encoded string) string {
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:8])
}

func sign(priv ed25519.PrivateKey, payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload))
}

func verify(encodedKey, sig string, payload []byte) bool {
	pub, err := decodeKey(encodedKey)
	if err != nil {
		return false
	}
	raw, err := peer.DecodeCanonical(sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, payload, raw)
}

// --- statements ---

func authorityStatement(federationID string, a AuthorityCert) []byte {
	b, _ := CanonicalJSON(map[string]any{"type": "authority-certificate", "federation_id": federationID, "epoch": a.Epoch,
		"instance_id": a.InstanceID, "public_key": a.PublicKey, "certified_by": a.CertifiedBy})
	return b
}

func rotationStatement(federationID string, r RecoveryRotation) []byte {
	b, _ := CanonicalJSON(map[string]any{"type": "recovery-rotation", "federation_id": federationID,
		"previous_key": r.PreviousKey, "new_key": r.NewKey, "generation": r.Generation})
	return b
}

// signingPayload is the canonical document without its signature member.
func signingPayload(m *Membership) ([]byte, error) {
	c := *m
	c.Signature = nil
	return CanonicalJSON(&c)
}

// Hash is the chain hash of a signed document: SHA-256 over its canonical
// form including the signature.
func Hash(m *Membership) string {
	b, err := CanonicalJSON(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Sign sets the authority signature on a document.
func Sign(m *Membership, authority ed25519.PrivateKey) error {
	payload, err := signingPayload(m)
	if err != nil {
		return err
	}
	m.Signature = &Signature{KeyID: fmt.Sprintf("authority-epoch-%d", m.AuthorityEpoch), Alg: "Ed25519", Value: sign(authority, payload)}
	return nil
}

// --- structural validation ---

var (
	namePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	uuidPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	hexPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidName reports whether a federation or member name is acceptable.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// ValidLabels checks label keys and values (at most 16 labels).
func ValidLabels(l map[string]string) bool {
	if len(l) > 16 {
		return false
	}
	for k, v := range l {
		if !labelPattern.MatchString(k) || len(v) > 128 || !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\r\n") {
			return false
		}
	}
	return true
}

// ValidFederationURL accepts https origins (host and optional port, no path,
// credentials, query or fragment). URLs come only from signed documents or
// administrator input and are never followed through redirects.
func ValidFederationURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 2048
}

func validAPIURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 2048
}

func validate(m *Membership) error {
	if m.Kind != "membership" || !uuidPattern.MatchString(m.FederationID) || !ValidName(m.FederationName) || m.Generation < 1 || m.AuthorityEpoch < 1 ||
		m.Protocol.Min < 1 || m.Protocol.Max < m.Protocol.Min || m.Authority.Epoch != m.AuthorityEpoch || !uuidPattern.MatchString(m.Authority.InstanceID) ||
		len(m.Members) == 0 || len(m.Members) > 256 || len(m.CACertificates) == 0 || len(m.CACertificates) > 2 || len(m.RevokedCertSerials) > 4096 ||
		m.Signature == nil || m.Signature.Alg != "Ed25519" || m.Signature.KeyID != fmt.Sprintf("authority-epoch-%d", m.AuthorityEpoch) {
		return ErrMalformed
	}
	if (m.Generation == 1) != (m.PrevHash == "") {
		return ErrMalformed
	}
	if _, err := decodeKey(m.RecoveryPublicKey); err != nil {
		return err
	}
	if _, err := decodeKey(m.Authority.PublicKey); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, m.IssuedAt); err != nil {
		return ErrMalformed
	}
	for _, c := range m.CACertificates {
		if _, err := parseCertPEM(c); err != nil {
			return ErrMalformed
		}
	}
	ids, names := map[string]bool{}, map[string]bool{}
	authorityActive := false
	for _, x := range m.Members {
		if !uuidPattern.MatchString(x.InstanceID) || ids[x.InstanceID] || !ValidName(x.Name) || !ValidFederationURL(x.FederationURL) || !validAPIURL(x.APIURL) ||
			!ValidLabels(x.Labels) || len(x.SoftwareVersion) > 128 || x.Protocol.Min < 1 || x.Protocol.Max < x.Protocol.Min || len(x.CertSerial) > 128 ||
			(x.CertSHA256 != "" && !hexPattern.MatchString(x.CertSHA256)) {
			return ErrMalformed
		}
		switch x.Status {
		case "active", "suspended":
			if names[x.Name] {
				return ErrMalformed
			}
			names[x.Name] = true
		case "removed":
		default:
			return ErrMalformed
		}
		if _, err := decodeKey(x.SigningKey); err != nil {
			return err
		}
		ids[x.InstanceID] = true
		if x.InstanceID == m.Authority.InstanceID && x.Status == "active" {
			authorityActive = true
		}
	}
	if !authorityActive {
		return ErrMalformed
	}
	return nil
}

// Verify checks a document against the currently applied document (nil when
// the verifier has none yet, which requires generation 1). It enforces the
// structure, the recovery-key pin and rotation chain, the authority key
// certification, the epoch and generation rules, the hash chain and the
// document signature. It never mutates its inputs.
func Verify(doc, applied *Membership) error {
	if err := validate(doc); err != nil {
		return err
	}
	if applied != nil && doc.FederationID != applied.FederationID {
		return ErrWrongFed
	}
	// Ordering first: an old document is stale whatever else it carries.
	if applied != nil {
		if doc.AuthorityEpoch < applied.AuthorityEpoch {
			return ErrStaleEpoch
		}
		if doc.Generation <= applied.Generation {
			return ErrStaleGen
		}
	}
	// Recovery key: pinned by generation 1, changed only by a rotation
	// statement signed with the previously trusted recovery key.
	switch {
	case applied == nil:
		if doc.Generation != 1 {
			return ErrGap
		}
	case doc.RecoveryPublicKey != applied.RecoveryPublicKey:
		r := doc.RecoveryRotation
		if r == nil || r.PreviousKey != applied.RecoveryPublicKey || r.NewKey != doc.RecoveryPublicKey || r.Generation != doc.Generation ||
			!verify(applied.RecoveryPublicKey, r.Signature, rotationStatement(doc.FederationID, *r)) {
			return ErrRecoveryChange
		}
	case doc.RecoveryRotation != nil:
		return ErrRecoveryChange
	}
	// Authority certification.
	switch doc.Authority.CertifiedBy {
	case "recovery":
		if !verify(doc.RecoveryPublicKey, doc.Authority.Signature, authorityStatement(doc.FederationID, doc.Authority)) {
			return ErrAuthorityCert
		}
	case "previous_epoch":
		if applied == nil || doc.AuthorityEpoch != applied.AuthorityEpoch+1 ||
			!verify(applied.Authority.PublicKey, doc.Authority.Signature, authorityStatement(doc.FederationID, doc.Authority)) {
			return ErrAuthorityCert
		}
	default:
		return ErrAuthorityCert
	}
	if applied != nil && doc.AuthorityEpoch == applied.AuthorityEpoch && doc.Authority.PublicKey != applied.Authority.PublicKey {
		return ErrAuthorityCert
	}
	// Signature.
	payload, err := signingPayload(doc)
	if err != nil {
		return ErrMalformed
	}
	if !verify(doc.Authority.PublicKey, doc.Signature.Value, payload) {
		return ErrSignature
	}
	// Chain.
	if applied != nil {
		if doc.Generation != applied.Generation+1 {
			return ErrGap
		}
		if doc.PrevHash != Hash(applied) {
			return ErrChain
		}
	}
	return nil
}

// --- certificates ---

func parseCertPEM(s string) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, ErrMalformed
	}
	return x509.ParseCertificate(block.Bytes)
}

// CertSHA256 is the hex SHA-256 of a certificate's DER encoding.
func CertSHA256(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}
