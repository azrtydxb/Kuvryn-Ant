package identity

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

// trustedProxies holds the process-wide set of reverse proxies (ingress
// controllers, gateways) whose forwarding headers are believed. It is empty by
// default: forwarding headers are then ignored and the TCP peer is the client.
var trustedProxies atomic.Pointer[[]netip.Prefix]

// ParseTrustedProxies parses a comma separated list of CIDRs or bare addresses.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, errors.New("trusted proxy " + item + " is not a CIDR or address")
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// SetTrustedProxies configures the trusted proxy set. Call once at startup.
func SetTrustedProxies(p []netip.Prefix) { trustedProxies.Store(&p) }

func trustedSet() []netip.Prefix {
	if p := trustedProxies.Load(); p != nil {
		return *p
	}
	return nil
}

func inPrefixes(a netip.Addr, set []netip.Prefix) bool {
	for _, p := range set {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr returns the client address of a request. X-Forwarded-For and
// X-Real-IP are honored only when the TCP peer is a trusted proxy; the
// forwarded chain is walked from the right and trusted hops are skipped so a
// client-supplied prefix can never choose its own identity.
func ClientAddr(r *http.Request) netip.Addr { return clientAddr(r, trustedSet()) }

func clientAddr(r *http.Request, trusted []netip.Prefix) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	peer = peer.Unmap().WithZone("")
	if len(trusted) == 0 || !inPrefixes(peer, trusted) {
		return peer
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return peer // malformed chain: do not guess
		}
		a = a.Unmap().WithZone("")
		if !inPrefixes(a, trusted) {
			return a
		}
	}
	if len(hops) == 0 {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			return a.Unmap().WithZone("")
		}
	}
	return peer
}

// ClientIP resolves the client address against an explicit trusted proxy set.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr { return clientAddr(r, trusted) }

// TrustedProxies returns the process-wide trusted proxy set.
func TrustedProxies() []netip.Prefix { return trustedSet() }

// clientKey is the throttle key for a request using this authenticator's
// trusted proxies, falling back to the process-wide set.
func (c *ClientAuth) trustedSet() []netip.Prefix {
	if len(c.TrustedProxies) > 0 {
		return c.TrustedProxies
	}
	return trustedSet()
}

func (c *ClientAuth) clientKey(r *http.Request) string {
	if a := clientAddr(r, c.trustedSet()); a.IsValid() {
		return a.String()
	}
	return "unknown"
}

// ClientKey is ClientAddr as a string key ("unknown" when unparseable).
func ClientKey(r *http.Request) string {
	if a := ClientAddr(r); a.IsValid() {
		return a.String()
	}
	return "unknown"
}
