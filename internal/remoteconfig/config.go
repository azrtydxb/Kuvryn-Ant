// Package remoteconfig persists remote-source trust/cache policy without loading
// secret material or contacting an upstream. Runtime materialization is separate.
package remoteconfig

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrInput       = errors.New("invalid remote source configuration")
	ErrNotFound    = errors.New("remote source configuration not found")
	ErrDenied      = errors.New("remote source configuration denied")
	ErrConflict    = errors.New("source revision changed")
	ErrUnavailable = errors.New("remote source configuration unavailable")
)
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var refID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var hostName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
var safePath = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

type CachePolicy struct {
	MetadataTTLSeconds  int   `json:"metadata_ttl_seconds"`
	NegativeTTLSeconds  int   `json:"negative_ttl_seconds"`
	StaleIfErrorSeconds int   `json:"stale_if_error_seconds"`
	MaxEntries          int   `json:"max_entries"`
	MaxBytes            int64 `json:"max_bytes"`
}
type Config struct {
	Origin          string      `json:"origin"`
	RedirectOrigins []string    `json:"redirect_origins"`
	CredentialID    string      `json:"credential_id,omitempty"`
	CARef           string      `json:"ca_ref,omitempty"`
	AllowPrivate    bool        `json:"allow_private"`
	TimeoutSeconds  int         `json:"timeout_seconds"`
	Cache           CachePolicy `json:"cache"`
}
type Record struct {
	SourceID string `json:"source_id"`
	Revision int64  `json:"revision"`
	Config   Config `json:"config"`
}

// DefaultConfig selects explicit conservative bounds. Stale serving is disabled.
func DefaultConfig(origin string) Config {
	return Config{Origin: origin, RedirectOrigins: []string{}, TimeoutSeconds: 30, Cache: CachePolicy{MetadataTTLSeconds: 300, NegativeTTLSeconds: 30, MaxEntries: 1024, MaxBytes: 16 << 20}}
}

// CredentialPurpose binds encrypted credential AAD/reference authority to exactly
// one source. It returns empty for a noncanonical UUID, which callers must reject.
func CredentialPurpose(sourceID string) string {
	if !uuid.MatchString(sourceID) {
		return ""
	}
	return "remote-source:" + sourceID
}

// Normalize returns an independent canonical config. HTTPS is mandatory; only an
// unescaped safe base path is accepted. No query/userinfo/header credential fields
// exist. DNS/address policy is rechecked by upstream at connection time, not here.
func Normalize(c Config) (Config, error) {
	origin, err := canonicalURL(c.Origin, true)
	if err != nil {
		return Config{}, ErrInput
	}
	c.Origin = origin
	if len(c.RedirectOrigins) > 16 || c.CredentialID != "" && !uuid.MatchString(c.CredentialID) || c.CARef != "" && !refID.MatchString(c.CARef) {
		return Config{}, ErrInput
	}
	redirects := make([]string, 0, len(c.RedirectOrigins))
	seen := map[string]bool{}
	for _, raw := range c.RedirectOrigins {
		v, e := canonicalURL(raw, false)
		if e != nil || seen[v] {
			return Config{}, ErrInput
		}
		seen[v] = true
		redirects = append(redirects, v)
	}
	sort.Strings(redirects)
	c.RedirectOrigins = redirects
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 300 || c.Cache.MetadataTTLSeconds < 1 || c.Cache.MetadataTTLSeconds > 86400 || c.Cache.NegativeTTLSeconds < 1 || c.Cache.NegativeTTLSeconds > 300 || c.Cache.StaleIfErrorSeconds < 0 || c.Cache.StaleIfErrorSeconds > 86400 || c.Cache.MaxEntries < 1 || c.Cache.MaxEntries > 100000 || c.Cache.MaxBytes < 1024 || c.Cache.MaxBytes > 1<<30 {
		return Config{}, ErrInput
	}
	return c, nil
}
func canonicalURL(raw string, basePath bool) (string, error) {
	if len(raw) == 0 || len(raw) > 4096 || strings.ContainsAny(raw, "\\\r\n\t ") {
		return "", ErrInput
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.Contains(raw, "%") {
		return "", ErrInput
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || strings.HasSuffix(host, ".") {
		return "", ErrInput
	}
	if ip := net.ParseIP(host); ip == nil {
		if len(host) > 253 || !hostName.MatchString(host) {
			return "", ErrInput
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return "", ErrInput
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", ErrInput
	}
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", ErrInput
		}
		port = strconv.Itoa(n)
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	path := u.Path
	if path != "" && path != "/" {
		if !basePath || !safePath.MatchString(path) || strings.Contains(path, "//") {
			return "", ErrInput
		}
		for _, segment := range strings.Split(path, "/") {
			if segment == "." || segment == ".." {
				return "", ErrInput
			}
		}
	}
	if path == "/" {
		path = ""
	}
	return "https://" + host + path, nil
}
