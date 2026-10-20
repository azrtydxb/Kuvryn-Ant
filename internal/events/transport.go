package events

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrConfiguration = errors.New("invalid event delivery configuration")
var ErrUnavailable = errors.New("event delivery storage unavailable")
var ErrLeaseLost = errors.New("event delivery lease lost")

func endpointAllowed(raw string, options Options) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return false
	}
	if u.Scheme != "https" && !(options.AllowHTTP && u.Scheme == "http") {
		return false
	}
	return true
}

// publicIP rejects special-use destinations, including cloud metadata, IPv4
// mapped IPv6 and transition ranges. Validation happens on resolved addresses at
// each new connection; DNS rebinding cannot substitute an unvalidated address.
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
func safeClient(in *http.Client, options Options) (*http.Client, error) {
	var transport *http.Transport
	if in != nil && in.Transport != nil {
		var ok bool
		transport, ok = in.Transport.(*http.Transport)
		if !ok {
			return nil, ErrConfiguration
		}
		transport = transport.Clone()
	} else {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DialTLS = nil
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		if transport.TLSClientConfig.InsecureSkipVerify {
			return nil, ErrConfiguration
		}
		if transport.TLSClientConfig.MinVersion < tls.VersionTLS12 {
			transport.TLSClientConfig.MinVersion = tls.VersionTLS12
		}
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrConfiguration
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("event destination unavailable")
		}
		if len(ips) == 0 {
			return nil, errors.New("event destination unavailable")
		}
		for _, ip := range ips {
			if ip.Zone() != "" || (!options.AllowPrivate && !publicIP(ip)) {
				return nil, errors.New("event destination denied")
			}
		}
		dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.New("event destination unavailable")
	}
	transport.MaxConnsPerHost = 4
	transport.MaxIdleConns = 16
	transport.MaxIdleConnsPerHost = 2
	transport.ResponseHeaderTimeout = 5 * time.Second
	transport.MaxResponseHeaderBytes = 16 << 10
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func canonicalEndpoint(raw string) string { return strings.TrimSpace(raw) }

// ValidateEndpoint validates configuration without network I/O. Resolved DNS
// addresses are checked again by the delivery transport on every connection.
func ValidateEndpoint(raw string) (string, error) { return ValidateEndpointFor(raw, false) }
