// Package peer implements the federation transport: the dedicated mTLS
// listener (TLS 1.3, client certificates chained to the federation CA,
// revocation and membership checks), member request signatures with replay
// and clock-skew protection, and the peer HTTP client (pinned trust, no
// redirects). It knows nothing about documents; callers supply a Trust.
package peer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Request-signature headers and limits.
const (
	HeaderMember    = "X-Ant-Fed-Member"
	HeaderTimestamp = "X-Ant-Fed-Timestamp"
	HeaderNonce     = "X-Ant-Fed-Nonce"
	HeaderSignature = "X-Ant-Fed-Signature"
	MaxSkew         = 60 * time.Second
	NonceTTL        = 5 * time.Minute
	MaxBody         = 8 << 20
)

// Member is the peer identity resolved from the presented client certificate.
type Member struct {
	InstanceID string
	Name       string
	Status     string
	SigningKey ed25519.PublicKey
}

// Trust is the current federation trust state, refreshed by the caller after
// every membership apply. All methods must be safe for concurrent use.
type Trust interface {
	// ServerCertificate is this member's certificate, or nil before found/join.
	ServerCertificate() *tls.Certificate
	// CAPool holds the federation CA certificates of the applied membership.
	CAPool() *x509.CertPool
	// Revoked reports a revoked certificate serial (hex).
	Revoked(serial string) bool
	// Lookup returns a member by instance ID.
	Lookup(instanceID string) (Member, bool)
	// Now is the clock used for skew checks.
	Now() time.Time
}

type ctxKey struct{}

// Caller returns the authenticated peer of a request served by Handler.
func Caller(r *http.Request) (Member, bool) {
	m, ok := r.Context().Value(ctxKey{}).(Member)
	return m, ok
}

// ServerTLSConfig requires TLS 1.3 and verifies any presented client
// certificate against the current federation CA. Requests without a
// certificate reach only routes registered as public (the join endpoint).
func ServerTLSConfig(t Trust) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			cert := t.ServerCertificate()
			if cert == nil {
				return nil, errors.New("federation not configured")
			}
			return &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{*cert},
				ClientAuth:   tls.VerifyClientCertIfGiven,
				ClientCAs:    t.CAPool(),
			}, nil
		},
	}
}

// Handler authenticates federation requests. Paths in public skip client
// authentication (they authenticate in-band, e.g. with an invitation). Every
// other request must present a verified, unrevoked certificate whose CN is an
// active member and carry a valid member request signature.
type Handler struct {
	Trust  Trust
	Mux    http.Handler
	Public map[string]bool
	Logger *slog.Logger
	// Reject is called for every refused request (for audit and metrics).
	Reject func(r *http.Request, member, reason string)
	nonces nonceCache
}

func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, status int, member, reason string) {
	if h.Reject != nil {
		h.Reject(r, member, reason)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"code":%q}`+"\n", reason)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.TLS == nil {
		h.refuse(w, r, http.StatusForbidden, "", "tls_required")
		return
	}
	if h.Public[r.Method+" "+r.URL.Path] {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		h.Mux.ServeHTTP(w, r)
		return
	}
	if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		h.refuse(w, r, http.StatusUnauthorized, "", "client_certificate_required")
		return
	}
	leaf := r.TLS.PeerCertificates[0]
	id := leaf.Subject.CommonName
	if h.Trust.Revoked(leaf.SerialNumber.Text(16)) {
		h.refuse(w, r, http.StatusForbidden, id, "certificate_revoked")
		return
	}
	m, ok := h.Trust.Lookup(id)
	if !ok {
		h.refuse(w, r, http.StatusForbidden, id, "not_a_member")
		return
	}
	if m.Status != "active" {
		h.refuse(w, r, http.StatusForbidden, id, "member_"+m.Status)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		h.refuse(w, r, http.StatusRequestEntityTooLarge, id, "body_too_large")
		return
	}
	if reason := h.verifySignature(r, m, body); reason != "" {
		h.refuse(w, r, http.StatusUnauthorized, id, reason)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	h.Mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, m)))
}

// signingString binds method, path and query, body digest, member, time and nonce.
func signingString(method, target, member, timestamp, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{"kuvryn-ant-federation-v1", method, target, member, timestamp, nonce, hex.EncodeToString(sum[:])}, "\n"))
}

func (h *Handler) verifySignature(r *http.Request, m Member, body []byte) string {
	member, ts, nonce, sig := r.Header.Get(HeaderMember), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderNonce), r.Header.Get(HeaderSignature)
	if member != m.InstanceID {
		return "signature_member_mismatch"
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "signature_invalid"
	}
	if d := h.Trust.Now().Sub(time.Unix(secs, 0)); d > MaxSkew || d < -MaxSkew {
		return "clock_skew"
	}
	if len(nonce) < 16 || len(nonce) > 64 {
		return "signature_invalid"
	}
	raw, err := DecodeCanonical(sig)
	if err != nil || len(raw) != ed25519.SignatureSize || !ed25519.Verify(m.SigningKey, signingString(r.Method, r.URL.RequestURI(), member, ts, nonce, body), raw) {
		return "signature_invalid"
	}
	if !h.nonces.add(member+"/"+nonce, h.Trust.Now()) {
		return "replayed_request"
	}
	return ""
}

// nonceCache remembers request nonces for NonceTTL.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (c *nonceCache) add(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	if len(c.seen) > 4096 {
		for k, t := range c.seen {
			if now.Sub(t) > NonceTTL {
				delete(c.seen, k)
			}
		}
	}
	if t, ok := c.seen[key]; ok && now.Sub(t) <= NonceTTL {
		return false
	}
	c.seen[key] = now
	return true
}

// --- client ---

// Identity is the caller side of a signed request.
type Identity struct {
	InstanceID  string
	SigningKey  ed25519.PrivateKey
	Certificate *tls.Certificate
	Roots       *x509.CertPool
}

// Client performs federation requests. It never follows redirects and never
// uses the system trust store.
type Client struct {
	Timeout time.Duration
	Now     func() time.Time
}

func (c Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

func (c Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (c Client) httpClient(cfg *tls.Config) *http.Client {
	return &http.Client{
		Timeout:       c.timeout(),
		CheckRedirect: noRedirect,
		Transport: &http.Transport{
			TLSClientConfig:       cfg,
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: c.timeout(),
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// ErrPeer reports a non-2xx peer answer with the peer's stable code.
type ErrPeer struct {
	Status int
	Code   string
}

func (e *ErrPeer) Error() string {
	return fmt.Sprintf("federation peer answered %d %s", e.Status, e.Code)
}

func target(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("federation: invalid peer URL")
	}
	return "https://" + u.Host + path, nil
}

func readResponse(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, errors.New("federation: peer response too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, &ErrPeer{Status: resp.StatusCode, Code: e.Code}
	}
	return b, nil
}

// Do sends a signed request over mTLS. expectID, when set, must be the CN of
// the server certificate, so a member cannot answer in another's name.
func (c Client) Do(ctx context.Context, id Identity, baseURL, method, path string, body []byte, expectID string) ([]byte, error) {
	return c.DoHeader(ctx, id, baseURL, method, path, body, expectID, nil)
}

// DoHeader is Do with extra request headers (for example a principal
// assertion). Headers are not covered by the member request signature; an
// assertion carries its own signature and request binding.
func (c Client) DoHeader(ctx context.Context, id Identity, baseURL, method, path string, body []byte, expectID string, hdr map[string]string) ([]byte, error) {
	if id.Certificate == nil || id.Roots == nil || id.SigningKey == nil {
		return nil, errors.New("federation: identity incomplete")
	}
	full, err := target(baseURL, path)
	if err != nil {
		return nil, err
	}
	cert := *id.Certificate
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      id.Roots,
		Certificates: []tls.Certificate{cert},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if expectID != "" && (len(cs.PeerCertificates) == 0 || cs.PeerCertificates[0].Subject.CommonName != expectID) {
				return errors.New("federation: peer certificate does not match the expected member")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	ts := strconv.FormatInt(c.now().Unix(), 10)
	nonce := randomNonce()
	req.Header.Set(HeaderMember, id.InstanceID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(ed25519.Sign(id.SigningKey, signingString(method, req.URL.RequestURI(), id.InstanceID, ts, nonce, body))))
	hc := c.httpClient(cfg)
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	return readResponse(resp)
}

// DoPinned sends an unauthenticated request to an authority whose CA is pinned
// by SHA-256 (the join bootstrap). The server chain must contain the pinned CA
// certificate and the leaf must verify against it for the URL host.
func (c Client) DoPinned(ctx context.Context, baseURL, caSHA256, method, path string, body []byte) ([]byte, error) {
	full, err := target(baseURL, path)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(full)
	host := u.Hostname()
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Trust comes only from the pinned CA fingerprint, verified below;
		// the system trust store is deliberately not consulted.
		InsecureSkipVerify: true, //nolint:gosec // pinned verification in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyPinned(cs.PeerCertificates, caSHA256, host)
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, full, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.httpClient(cfg)
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	return readResponse(resp)
}

func verifyPinned(chain []*x509.Certificate, caSHA256, host string) error {
	if len(chain) == 0 {
		return errors.New("federation: no server certificate")
	}
	var ca *x509.Certificate
	for _, c := range chain[1:] {
		sum := sha256.Sum256(c.Raw)
		if hex.EncodeToString(sum[:]) == caSHA256 && c.IsCA {
			ca = c
		}
	}
	if ca == nil {
		return errors.New("federation: server chain does not contain the pinned CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, DNSName: host, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func randomNonce() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ClientTLS is the mTLS client configuration of a member identity. expectID,
// when set, must be the CN of the server certificate.
func ClientTLS(id Identity, expectID string) *tls.Config {
	var certs []tls.Certificate
	if id.Certificate != nil {
		certs = []tls.Certificate{*id.Certificate}
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      id.Roots,
		Certificates: certs,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if expectID != "" && (len(cs.PeerCertificates) == 0 || cs.PeerCertificates[0].Subject.CommonName != expectID) {
				return errors.New("federation: peer certificate does not match the expected member")
			}
			return nil
		},
	}
}

// SignRequest adds the member request signature headers to req for body
// (nil for GET). It is the same signature Do uses, for callers that need
// their own transport (for example streaming replication transfers).
func SignRequest(req *http.Request, id Identity, body []byte, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := randomNonce()
	req.Header.Set(HeaderMember, id.InstanceID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, base64.RawURLEncoding.EncodeToString(ed25519.Sign(id.SigningKey, signingString(req.Method, req.URL.RequestURI(), id.InstanceID, ts, nonce, body))))
}

// TargetURL joins a member federation base URL and a path (https only).
func TargetURL(base, path string) (string, error) { return target(base, path) }
