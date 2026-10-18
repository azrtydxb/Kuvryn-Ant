package peer

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Principal assertions (docs/planning/phase-2-federation.md §6.5). When member
// A acts for a caller on member B it sends, on the mTLS channel,
//
//	X-Ant-Fed-Assertion: base64url(claims JSON) "." base64url(Ed25519 signature by A)
//
// The signature covers the exact transmitted claim bytes. B checks the
// signature against A's member key from membership, that iss is the mTLS
// caller, aud is B, the 60-second lifetime and clock skew, the request-hash
// binding and jti replay, then evaluates its own policy for the asserted
// federated principal intersected with the asserted token scopes. B never
// trusts A's authorization decision.

// HeaderAssertion carries a principal assertion.
const HeaderAssertion = "X-Ant-Fed-Assertion"

// AssertionLifetime is the maximum validity of an assertion.
const AssertionLifetime = 60 * time.Second

// AssertedScope is a token scope expressed with a portable feed key ("" = all
// feeds); scopes on feeds without a federation key cannot be asserted.
type AssertedScope struct {
	FeedKey   string   `json:"feed_key"`
	Namespace string   `json:"namespace"`
	Actions   []string `json:"actions"`
}

// Assertion is the claim set.
type Assertion struct {
	Issuer      string `json:"iss"` // asserting member instance ID
	Federation  string `json:"fid"` // federation ID
	FederatedID string `json:"fed_id"`
	Domain      string `json:"dom"` // identity domain ID, or "federation" for service accounts
	Subject     string `json:"sub"`
	// Scopes is nil for a browser session and the (possibly empty) list of
	// translated token scopes otherwise. An empty list grants nothing.
	Scopes   []AssertedScope `json:"scopes"`
	Audience string          `json:"aud"` // receiving member instance ID
	Request  string          `json:"req"` // RequestHash of the bound request
	IssuedAt int64           `json:"iat"`
	Expires  int64           `json:"exp"`
	ID       string          `json:"jti"`
}

// Assertion verification failures; Reason maps them to stable codes.
var (
	ErrAssertionMalformed = errors.New("federation: assertion malformed")
	ErrAssertionSignature = errors.New("federation: assertion signature invalid")
	ErrAssertionIssuer    = errors.New("federation: assertion issuer is not the calling member")
	ErrAssertionAudience  = errors.New("federation: assertion audience mismatch")
	ErrAssertionExpired   = errors.New("federation: assertion expired")
	ErrAssertionSkew      = errors.New("federation: assertion clock skew")
	ErrAssertionRequest   = errors.New("federation: assertion bound to another request")
)

// AssertionReason is the stable rejection code of an assertion error.
func AssertionReason(err error) string {
	switch {
	case errors.Is(err, ErrAssertionSignature):
		return "assertion_signature_invalid"
	case errors.Is(err, ErrAssertionIssuer):
		return "assertion_issuer_mismatch"
	case errors.Is(err, ErrAssertionAudience):
		return "assertion_audience_mismatch"
	case errors.Is(err, ErrAssertionExpired):
		return "assertion_expired"
	case errors.Is(err, ErrAssertionSkew):
		return "assertion_clock_skew"
	case errors.Is(err, ErrAssertionRequest):
		return "assertion_request_mismatch"
	default:
		return "assertion_malformed"
	}
}

var jtiPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// RequestHash binds an assertion to one request: method, path with query and
// body digest.
func RequestHash(method, requestURI string, body []byte) string {
	b := sha256.Sum256(body)
	sum := sha256.Sum256([]byte(method + "\n" + requestURI + "\n" + hex.EncodeToString(b[:])))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SignAssertion returns the header value for a claim set.
func SignAssertion(a Assertion, key ed25519.PrivateKey) (string, error) {
	if a.ID == "" {
		a.ID = randomNonce()
	}
	payload, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)), nil
}

// ParseAssertion decodes a header value without verifying it.
func ParseAssertion(header string) (Assertion, []byte, []byte, error) {
	var a Assertion
	if len(header) > 16<<10 {
		return a, nil, nil, ErrAssertionMalformed
	}
	p, s, ok := strings.Cut(header, ".")
	if !ok {
		return a, nil, nil, ErrAssertionMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return a, nil, nil, ErrAssertionMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return a, nil, nil, ErrAssertionMalformed
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || dec.More() {
		return a, nil, nil, ErrAssertionMalformed
	}
	return a, payload, sig, nil
}

// VerifyAssertion checks everything except replay (the caller records jti in
// durable storage) and the principal mapping.
func VerifyAssertion(header string, caller Member, self string, now time.Time, method, requestURI string, body []byte) (Assertion, error) {
	a, payload, sig, err := ParseAssertion(header)
	if err != nil {
		return a, err
	}
	if !jtiPattern.MatchString(a.ID) || a.Subject == "" || len(a.Subject) > 512 || a.Domain == "" || len(a.Domain) > 64 || len(a.Scopes) > 64 ||
		a.Expires <= a.IssuedAt || time.Duration(a.Expires-a.IssuedAt)*time.Second > AssertionLifetime {
		return a, ErrAssertionMalformed
	}
	if a.Issuer != caller.InstanceID {
		return a, ErrAssertionIssuer
	}
	if !ed25519.Verify(caller.SigningKey, payload, sig) {
		return a, ErrAssertionSignature
	}
	if a.Audience != self {
		return a, ErrAssertionAudience
	}
	iat := time.Unix(a.IssuedAt, 0)
	if d := now.Sub(iat); d > MaxSkew+AssertionLifetime || d < -MaxSkew {
		return a, ErrAssertionSkew
	}
	if now.After(time.Unix(a.Expires, 0).Add(MaxSkew)) {
		return a, ErrAssertionExpired
	}
	if a.Request != RequestHash(method, requestURI, body) {
		return a, ErrAssertionRequest
	}
	return a, nil
}
