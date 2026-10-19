package terraform

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Terraform sends credentials to the registry host's API only. The URLs of the
// provider zip, SHA256SUMS and signature (download_url, shasums_url and
// shasums_signature_url) are fetched without them, which an authenticated feed
// would answer with 401. The download document therefore advertises those three
// file URLs with a short-lived signature minted for the principal that was just
// authorized to read the provider. The signature binds the exact path and an
// expiry; on use the principal is re-resolved against the live token and role
// state, so a revoked token or removed grant stops working immediately and the
// URL can never read anything the principal could not.

const (
	signedQuery = "ant-sig"
	signedTTL   = 15 * time.Minute
)

type signedPayload struct {
	Instance  string           `json:"i"`
	Principal string           `json:"p"`
	Token     string           `json:"t,omitempty"`
	Scopes    []identity.Scope `json:"s,omitempty"`
	Anonymous bool             `json:"a,omitempty"`
	Expires   int64            `json:"e"`
	Path      string           `json:"u"`
}

// SetSigningKey enables signed file URLs. root is the 32 byte instance root key;
// the signing key is derived from it for this purpose only.
func (s *Repository) SetSigningKey(root []byte) {
	if len(root) != 32 {
		return
	}
	m := hmac.New(sha256.New, root)
	m.Write([]byte("kuvryn-ant/terraform/file-url"))
	s.sigKey = m.Sum(nil)
}

func (s *Repository) mac(body []byte) []byte {
	m := hmac.New(sha256.New, s.sigKey)
	m.Write(body)
	return m.Sum(nil)
}

// signFileURL returns raw with a signature for principal p appended, or raw
// unchanged when no signing key is configured.
func (s *Repository) signFileURL(p identity.Principal, raw string) string {
	u, err := url.Parse(raw)
	if len(s.sigKey) != 32 || err != nil || p.PrincipalID == "" {
		return raw
	}
	body, _ := json.Marshal(signedPayload{Instance: p.InstanceID, Principal: p.PrincipalID, Token: p.TokenID, Scopes: p.Scopes, Anonymous: p.Anonymous, Expires: time.Now().Add(signedTTL).Unix(), Path: u.Path})
	q := u.Query()
	q.Set(signedQuery, base64.RawURLEncoding.EncodeToString(body)+"."+base64.RawURLEncoding.EncodeToString(s.mac(body)))
	u.RawQuery = q.Encode()
	return u.String()
}

// verifySigned returns the principal a valid, unexpired signature was minted for.
func (s *Repository) verifySigned(r *http.Request) (identity.Principal, bool) {
	raw := r.URL.Query().Get(signedQuery)
	if raw == "" || len(s.sigKey) != 32 || len(raw) > 8192 || (r.Method != "GET" && r.Method != "HEAD") {
		return identity.Principal{}, false
	}
	b, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return identity.Principal{}, false
	}
	body, e1 := base64.RawURLEncoding.DecodeString(b)
	want, e2 := base64.RawURLEncoding.DecodeString(sig)
	if e1 != nil || e2 != nil || !hmac.Equal(want, s.mac(body)) {
		return identity.Principal{}, false
	}
	var v signedPayload
	if json.Unmarshal(body, &v) != nil || time.Now().Unix() > v.Expires || v.Path != r.URL.Path || v.Instance == "" || v.Principal == "" {
		return identity.Principal{}, false
	}
	return identity.Principal{InstanceID: v.Instance, PrincipalID: v.Principal, TokenID: v.Token, Scopes: v.Scopes, Anonymous: v.Anonymous}, true
}

// serveSigned answers a request carrying a signature. It reports whether the
// request was handled.
func (s *Repository) serveSigned(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get(signedQuery) == "" {
		return false
	}
	p, ok := s.verifySigned(r)
	if !ok {
		failure(w, artifact.ErrDenied)
		return true
	}
	s.serve(w, r.WithContext(identity.WithAuthenticatedPrincipal(r.Context(), p)))
	return true
}
