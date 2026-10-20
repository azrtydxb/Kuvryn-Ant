package events

import (
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// ValidSlug reports whether name is a legal webhook slug: lowercase letters,
// digits and hyphens, 1 to 63 characters.
func ValidSlug(name string) bool { return slugPattern.MatchString(name) }

// ValidateEndpointFor is ValidateEndpoint with the instance scheme policy: http
// destinations are accepted only when allowHTTP is true. Host rules (no
// localhost, no private or special-use address literals) are unchanged and DNS
// answers are still checked on every connection by the delivery transport.
func ValidateEndpointFor(raw string, allowHTTP bool) (string, error) {
	raw = canonicalEndpoint(raw)
	if !endpointAllowed(raw, Options{AllowHTTP: allowHTTP}) {
		return "", ErrConfiguration
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ErrConfiguration
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", ErrConfiguration
	}
	if ip, err := netip.ParseAddr(host); err == nil && (ip.Zone() != "" || !publicIP(ip)) {
		return "", ErrConfiguration
	}
	return raw, nil
}
