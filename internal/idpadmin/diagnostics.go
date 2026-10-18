package idpadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/crewjam/saml"
)

// SkewWarning is the clock difference at which ID token and assertion
// validation (30 second leeway) starts to fail.
const SkewWarning = 30 * time.Second

const expiringSoon = 30 * 24 * time.Hour

// Diagnostics performs administrator-initiated connectivity tests. Transport
// nil selects a transport that refuses link-local and unspecified addresses
// (cloud metadata endpoints), since the target URL is operator supplied.
type Diagnostics struct {
	Transport http.RoundTripper
	Now       func() time.Time
}

func (d Diagnostics) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func safeTransport() http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errors.New("blocked address")
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("blocked address")
		}
		return nil
	}}
	return &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 64 << 10}
}

func (d Diagnostics) get(ctx context.Context, raw string) ([]byte, http.Header, error) {
	if !httpsURL(raw) {
		return nil, nil, errors.New("URL must be https")
	}
	tr := d.Transport
	if tr == nil {
		tr = safeTransport()
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects are not followed") }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, errors.New("request failed (network, TLS or redirect)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return nil, nil, errors.New("response unreadable or larger than 1 MiB")
	}
	return b, resp.Header, nil
}

type OIDCEndpoints struct {
	Authorization string `json:"authorization"`
	Token         string `json:"token"`
	JWKS          string `json:"jwks"`
}

type OIDCReport struct {
	OK               bool          `json:"ok"`
	Issuer           string        `json:"issuer"`
	IssuerMatches    bool          `json:"issuer_matches"`
	Endpoints        OIDCEndpoints `json:"endpoints"`
	JWKSKeys         int           `json:"jwks_keys"`
	SigningAlgs      []string      `json:"signing_algs"`
	SupportsRS256    bool          `json:"supports_rs256"`
	SupportsPKCE     bool          `json:"supports_pkce_s256"`
	ClockSkewSeconds *int64        `json:"clock_skew_seconds,omitempty"`
	SkewWarning      bool          `json:"skew_warning"`
	// SuggestedEndpoints is what to place in allowed_endpoints.
	SuggestedEndpoints []string `json:"suggested_allowed_endpoints"`
	MissingFromAllowed []string `json:"missing_from_allowed,omitempty"`
	Errors             []string `json:"errors"`
	Warnings           []string `json:"warnings"`
}

func skew(h http.Header, now time.Time) *int64 {
	date, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return nil
	}
	// Server minus local: positive means the provider clock is ahead.
	v := int64(date.Sub(now).Round(time.Second) / time.Second)
	return &v
}

// OIDC fetches discovery and the JWKS exactly as the login runtime would and
// reports problems that would prevent sign-in. allowed may be empty.
func (d Diagnostics) OIDC(ctx context.Context, issuer string, allowed []string) OIDCReport {
	r := OIDCReport{Issuer: issuer, Errors: []string{}, Warnings: []string{}, SigningAlgs: []string{}}
	if !httpsURL(issuer) || strings.Contains(issuer, "?") {
		r.Errors = append(r.Errors, "issuer must be an https URL without a query")
		return r
	}
	before := d.now()
	body, header, err := d.get(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration")
	if err != nil {
		r.Errors = append(r.Errors, "discovery fetch failed: "+err.Error())
		return r
	}
	r.ClockSkewSeconds = skew(header, before)
	if r.ClockSkewSeconds != nil {
		abs := *r.ClockSkewSeconds
		if abs < 0 {
			abs = -abs
		}
		if time.Duration(abs)*time.Second > SkewWarning {
			r.SkewWarning = true
			r.Warnings = append(r.Warnings, fmt.Sprintf("clock differs from the provider by %ds; ID token validation tolerates 30s, so sign-ins will fail until NTP is fixed", *r.ClockSkewSeconds))
		}
	}
	var doc struct {
		Issuer        string   `json:"issuer"`
		Authorization string   `json:"authorization_endpoint"`
		Token         string   `json:"token_endpoint"`
		JWKS          string   `json:"jwks_uri"`
		Algs          []string `json:"id_token_signing_alg_values_supported"`
		PKCE          []string `json:"code_challenge_methods_supported"`
	}
	if json.Unmarshal(body, &doc) != nil {
		r.Errors = append(r.Errors, "discovery document is not valid JSON")
		return r
	}
	r.IssuerMatches = doc.Issuer == issuer
	if !r.IssuerMatches {
		r.Errors = append(r.Errors, "discovery issuer does not exactly match the configured issuer (including any trailing slash)")
	}
	r.Endpoints = OIDCEndpoints{doc.Authorization, doc.Token, doc.JWKS}
	for name, e := range map[string]string{"authorization_endpoint": doc.Authorization, "token_endpoint": doc.Token, "jwks_uri": doc.JWKS} {
		if !httpsURL(e) {
			r.Errors = append(r.Errors, name+" is missing or not https")
		}
	}
	r.SuggestedEndpoints = []string{doc.Authorization, doc.Token, doc.JWKS}
	have := map[string]bool{}
	for _, a := range allowed {
		have[a] = true
	}
	if len(allowed) > 0 {
		for _, e := range r.SuggestedEndpoints {
			if !have[e] {
				r.MissingFromAllowed = append(r.MissingFromAllowed, e)
			}
		}
		if len(r.MissingFromAllowed) > 0 {
			r.Errors = append(r.Errors, "allowed_endpoints is missing discovered endpoints; sign-in would be refused")
		}
	}
	for _, p := range doc.PKCE {
		if p == "S256" {
			r.SupportsPKCE = true
		}
	}
	if !r.SupportsPKCE {
		r.Warnings = append(r.Warnings, "provider does not advertise PKCE S256")
	}
	for _, a := range doc.Algs {
		if a == "RS256" {
			r.SupportsRS256 = true
		}
	}
	if !r.SupportsRS256 {
		r.Errors = append(r.Errors, "provider does not advertise RS256; only RS256 ID tokens are accepted")
	}
	if httpsURL(doc.JWKS) {
		jb, _, err := d.get(ctx, doc.JWKS)
		if err != nil {
			r.Errors = append(r.Errors, "JWKS fetch failed: "+err.Error())
		} else {
			var set struct {
				Keys []struct {
					Kty string `json:"kty"`
					Alg string `json:"alg"`
					Use string `json:"use"`
				} `json:"keys"`
			}
			if json.Unmarshal(jb, &set) != nil || len(set.Keys) == 0 {
				r.Errors = append(r.Errors, "JWKS has no keys")
			}
			seen := map[string]bool{}
			for _, k := range set.Keys {
				if k.Use == "enc" {
					continue
				}
				r.JWKSKeys++
				alg := k.Alg
				if alg == "" && k.Kty == "RSA" {
					alg = "RS256"
				}
				if alg != "" && !seen[alg] {
					seen[alg] = true
					r.SigningAlgs = append(r.SigningAlgs, alg)
				}
			}
			if r.JWKSKeys > 0 && !seen["RS256"] {
				r.Errors = append(r.Errors, "JWKS contains no RS256 signing key")
			}
		}
	}
	r.OK = len(r.Errors) == 0
	return r
}

// CertStatus describes a certificate's validity window.
type CertStatus struct {
	Subject   string    `json:"subject"`
	Use       string    `json:"use,omitempty"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	DaysLeft  int       `json:"days_left"`
	// Status is ok, expiring (under 30 days), expired or not_yet_valid.
	Status string `json:"status"`
	SHA256 string `json:"sha256"`
}

func certStatus(c *x509.Certificate, use string, now time.Time) CertStatus {
	sum := sha256.Sum256(c.Raw)
	s := CertStatus{Subject: c.Subject.String(), Use: use, NotBefore: c.NotBefore, NotAfter: c.NotAfter, SHA256: hex.EncodeToString(sum[:]), DaysLeft: int(c.NotAfter.Sub(now).Hours() / 24)}
	switch {
	case now.Before(c.NotBefore):
		s.Status = "not_yet_valid"
	case !now.Before(c.NotAfter):
		s.Status = "expired"
	case c.NotAfter.Sub(now) < expiringSoon:
		s.Status = "expiring"
	default:
		s.Status = "ok"
	}
	return s
}

// SPCertificateStatus reports on the stored service provider certificate.
func (d Diagnostics) SPCertificateStatus(cfg SAMLConfig) []CertStatus {
	out := []CertStatus{}
	for _, c := range pemCertificates([]byte(cfg.SPCertificatePEM)) {
		out = append(out, certStatus(c, "sp-signing", d.now()))
	}
	return out
}

type Endpoint struct {
	Binding  string `json:"binding"`
	Location string `json:"location"`
}

type SAMLMetadataReport struct {
	EntityID     string       `json:"entity_id"`
	SSO          []Endpoint   `json:"sso_endpoints"`
	SLO          []Endpoint   `json:"slo_endpoints"`
	Certificates []CertStatus `json:"certificates"`
	ValidUntil   *time.Time   `json:"valid_until,omitempty"`
	Errors       []string     `json:"errors"`
	Warnings     []string     `json:"warnings"`
	OK           bool         `json:"ok"`
}

// ImportSAMLMetadata parses pasted IdP metadata (never fetched remotely) and
// reports endpoints and signing-certificate expiry. DOCTYPE is refused.
func (d Diagnostics) ImportSAMLMetadata(raw []byte) SAMLMetadataReport {
	r := SAMLMetadataReport{SSO: []Endpoint{}, SLO: []Endpoint{}, Certificates: []CertStatus{}, Errors: []string{}, Warnings: []string{}}
	if len(raw) == 0 || len(raw) > 256<<10 {
		r.Errors = append(r.Errors, "metadata must be between 1 byte and 256 KiB")
		return r
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("<!doctype")) || bytes.Contains(bytes.ToLower(raw), []byte("<!entity")) {
		r.Errors = append(r.Errors, "DOCTYPE and entity declarations are not allowed")
		return r
	}
	var md saml.EntityDescriptor
	if err := xml.Unmarshal(raw, &md); err != nil || md.EntityID == "" {
		r.Errors = append(r.Errors, "not a valid SAML EntityDescriptor (federation EntitiesDescriptor aggregates are not supported)")
		return r
	}
	r.EntityID = md.EntityID
	now := d.now()
	if !md.ValidUntil.IsZero() {
		v := md.ValidUntil
		r.ValidUntil = &v
		if !now.Before(v) {
			r.Errors = append(r.Errors, "metadata validUntil has passed")
		}
	}
	if len(md.IDPSSODescriptors) != 1 {
		r.Errors = append(r.Errors, "metadata must contain exactly one IDPSSODescriptor")
		return r
	}
	idp := md.IDPSSODescriptors[0]
	for _, e := range idp.SingleSignOnServices {
		r.SSO = append(r.SSO, Endpoint{e.Binding, e.Location})
		if !httpsURL(e.Location) {
			r.Errors = append(r.Errors, "SSO endpoint is not https")
		}
	}
	if len(r.SSO) == 0 {
		r.Errors = append(r.Errors, "no SingleSignOnService endpoint")
	}
	for _, e := range idp.SingleLogoutServices {
		r.SLO = append(r.SLO, Endpoint{e.Binding, e.Location})
	}
	signing := 0
	for _, kd := range idp.KeyDescriptors {
		for _, xc := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(xc.Data), ""))
			if err != nil {
				r.Errors = append(r.Errors, "unreadable certificate encoding")
				continue
			}
			c, err := x509.ParseCertificate(der)
			if err != nil {
				r.Errors = append(r.Errors, "unparseable X.509 certificate")
				continue
			}
			st := certStatus(c, kd.Use, now)
			r.Certificates = append(r.Certificates, st)
			if kd.Use == "" || kd.Use == "signing" {
				signing++
				switch st.Status {
				case "expired", "not_yet_valid":
					r.Errors = append(r.Errors, "signing certificate "+st.SHA256[:12]+" is "+st.Status)
				case "expiring":
					r.Warnings = append(r.Warnings, fmt.Sprintf("signing certificate %s expires in %d days; ask the IdP admin for rollover metadata", st.SHA256[:12], st.DaysLeft))
				}
			}
		}
	}
	if signing == 0 {
		r.Errors = append(r.Errors, "no signing certificate in metadata")
	}
	r.OK = len(r.Errors) == 0
	return r
}

// ExportableOrigin sanity-checks an origin used for SP metadata.
func ExportableOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Path == ""
}
