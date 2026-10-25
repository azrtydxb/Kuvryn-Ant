// Package upstream performs safe reads under a configured source trust policy.
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/azrtydxb/Kuvryn-Ant/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrPolicy = errors.New("upstream policy denied request")
var ErrUnavailable = errors.New("upstream unavailable")
var ErrCircuitOpen = errors.New("upstream circuit open")

type Config struct {
	Origin                  string
	RedirectOrigins         []string
	Credentials             http.Header // configured secret headers, sent only to Origin
	Roots                   *x509.CertPool
	AllowPrivate, AllowHTTP bool
	Resolver                func(context.Context, string) ([]netip.Addr, error)
	Now                     func() time.Time
	Sleep                   func(context.Context, time.Duration) error
	FailureThreshold        int
	OpenFor                 time.Duration
}
type Fetcher struct {
	cfg      Config
	origin   *url.URL
	allowed  map[string]bool
	client   *http.Client
	mu       sync.Mutex
	failures int
	until    time.Time
	probe    bool
	tokens   tokenState
}
type CircuitState struct {
	Failures  int
	OpenUntil time.Time
	Probe     bool
}

func (f *Fetcher) State() CircuitState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return CircuitState{f.failures, f.until, f.probe}
}
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}
func parse(raw string, allowHTTP bool) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 8192 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) {
		return nil, ErrPolicy
	}
	if u.Scheme == "http" {
		ip, e := netip.ParseAddr(u.Hostname())
		if e != nil || !ip.IsLoopback() {
			return nil, ErrPolicy
		}
	}
	return u, nil
}
func New(c Config) (*Fetcher, error) {
	u, e := parse(c.Origin, c.AllowHTTP)
	if e != nil || u.RawQuery != "" {
		return nil, ErrPolicy
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = 3
	}
	if c.OpenFor == 0 {
		c.OpenFor = 30 * time.Second
	}
	if c.FailureThreshold < 1 || c.OpenFor < 0 {
		return nil, ErrPolicy
	}
	c.Credentials = c.Credentials.Clone()
	for k, v := range c.Credentials {
		if !strings.EqualFold(k, "Authorization") && !strings.EqualFold(k, "X-Api-Key") {
			return nil, ErrPolicy
		}
		for _, s := range v {
			if len(s) > 8192 || strings.ContainsAny(s, "\r\n") {
				return nil, ErrPolicy
			}
		}
	}
	f := &Fetcher{cfg: c, origin: u, allowed: map[string]bool{origin(u): true}}
	for _, raw := range c.RedirectOrigins {
		v, e := parse(raw, c.AllowHTTP)
		if e != nil || v.RawQuery != "" || (v.Path != "" && v.Path != "/") {
			return nil, ErrPolicy
		}
		f.allowed[origin(v)] = true
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialTLSContext = nil
	tr.DialTLS = nil
	roots := c.Roots
	if roots != nil {
		roots = roots.Clone()
	}
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	tr.DialContext = f.dial
	tr.ResponseHeaderTimeout = 10 * time.Second
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.MaxResponseHeaderBytes = 64 << 10
	tr.MaxConnsPerHost = 8
	tr.MaxIdleConns = 16
	tr.MaxIdleConnsPerHost = 4
	f.client = &http.Client{Transport: userAgentTransport{tr}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return f, nil
}

// UserAgent identifies Ant to upstream registries. Go's default
// "Go-http-client/x" agent is throttled with 429 by Maven Central's CDN.
const UserAgent = "kuvryn-ant (+https://github.com/azrtydxb/Kuvryn-Ant)"

// userAgentTransport sets UserAgent on every upstream request.
type userAgentTransport struct{ base *http.Transport }

func (t userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", UserAgent)
	return t.base.RoundTrip(r)
}
func (t userAgentTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }
func (f *Fetcher) Close()                          { f.client.CloseIdleConnections() }
func (f *Fetcher) permitted(ctx context.Context, u *url.URL) error {
	if !f.allowed[origin(u)] {
		return ErrPolicy
	}
	_, e := f.addresses(ctx, u.Hostname())
	return e
}
func (f *Fetcher) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	var ips []netip.Addr
	var e error
	if f.cfg.Resolver != nil {
		ips, e = f.cfg.Resolver(ctx, host)
	} else {
		ips, e = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if e != nil || len(ips) == 0 {
		return nil, &FetchError{Class: "dns", Host: host}
	}
	for _, ip := range ips {
		if ip.Zone() != "" || !ip.IsValid() || (!f.cfg.AllowPrivate && !publicIP(ip)) {
			return nil, ErrPolicy
		}
	}
	return ips, nil
}
func (f *Fetcher) dial(ctx context.Context, _, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, ErrPolicy
	}
	ips, e := f.addresses(ctx, host)
	if e != nil {
		return nil, e
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	for _, ip := range ips {
		conn, e := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if e == nil {
			return &idleConn{Conn: conn}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, &FetchError{Class: "dial", Host: host}
}

type idleConn struct{ net.Conn }

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	return c.Conn.Read(p)
}
func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(p).Contains(ip) {
			return false
		}
	}
	return true
}
func (f *Fetcher) admit() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.until.IsZero() {
		return true
	}
	if f.cfg.Now().Before(f.until) || f.probe {
		return false
	}
	f.probe = true
	return true
}
func (f *Fetcher) finish(outcome int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probe = false
	if outcome == 0 {
		return
	}
	if outcome < 0 {
		f.failures = 0
		f.until = time.Time{}
		return
	}
	f.failures++
	if f.failures >= f.cfg.FailureThreshold {
		f.until = f.cfg.Now().Add(f.cfg.OpenFor)
	}
}
func retryable(status int) bool {
	return status == 429 || status == 502 || status == 503 || status == 504
}

// Fetch permits only GET/HEAD. Callers own and must close a successful response
// body. Retries cover connection/header failures, never a partially read body.
func (f *Fetcher) Fetch(ctx context.Context, method, target string, headers http.Header) (*http.Response, error) {
	ctx, span := tracing.Start(ctx, "upstream fetch", attribute.String("http.request.method", method), attribute.String("server.address", f.origin.Hostname()))
	response, err := f.fetch(ctx, method, target, headers)
	if response != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", response.StatusCode))
	}
	tracing.End(span, err)
	return response, err
}

func (f *Fetcher) fetch(ctx context.Context, method, target string, headers http.Header) (*http.Response, error) {
	if method != "GET" && method != "HEAD" {
		return nil, ErrPolicy
	}
	v, e := url.Parse(target)
	if e != nil {
		return nil, ErrPolicy
	}
	u := f.origin.ResolveReference(v)
	u, e = parse(u.String(), f.cfg.AllowHTTP)
	if e != nil {
		return nil, e
	}
	if origin(u) == origin(f.origin) {
		u.Path = libraryPath(u.Hostname(), u.Path)
		u.RawPath = ""
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	e = f.permitted(bounded, u)
	cancel()
	if e != nil {
		return nil, e
	}
	if !f.admit() {
		return nil, ErrCircuitOpen
	}
	outcome := 0
	defer func() { f.finish(outcome) }()
	sameOrigin := origin(u) == origin(f.origin)
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		auth, usedKey := "", ""
		if sameOrigin {
			if t, key := f.cachedToken(u); key != "" {
				auth, usedKey = "Bearer "+t.value, key
			}
		}
		response, e := f.once(ctx, method, u, headers, auth)
		if e == nil && response.StatusCode == http.StatusUnauthorized && sameOrigin {
			response, e = f.bearerRetry(ctx, method, u, headers, response, usedKey)
			if e != nil {
				var rl *RateLimitError
				if errors.As(e, &rl) {
					return nil, e
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
		}
		if e == nil && response.StatusCode == http.StatusTooManyRequests {
			// Retrying a rate limit only burns more of the quota: surface it.
			rl := rateLimit(response)
			response.Body.Close()
			return nil, rl
		}
		if e == nil && !retryable(response.StatusCode) {
			outcome = -1
			return response, nil
		}
		if response != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var redirect *RedirectError
		if errors.As(e, &redirect) {
			return nil, redirect
		}
		if errors.Is(e, ErrPolicy) {
			return nil, ErrPolicy
		}
		if e != nil {
			last = e
		} else if response != nil {
			last = &FetchError{Class: "status", Host: f.origin.Hostname(), Status: response.StatusCode}
		}
		if attempt < 2 {
			jitter, _ := rand.Int(rand.Reader, big.NewInt(100))
			delay := time.Duration(100*(1<<attempt)) * time.Millisecond
			if jitter != nil {
				delay += time.Duration(jitter.Int64()) * time.Millisecond
			}
			if e = f.cfg.Sleep(ctx, delay); e != nil {
				return nil, e
			}
		}
	}
	outcome = 1
	if last != nil {
		return nil, last
	}
	return nil, ErrUnavailable
}
func (f *Fetcher) once(ctx context.Context, method string, start *url.URL, headers http.Header, bearer string) (*http.Response, error) {
	u := start
	for redirects := 0; redirects <= 5; redirects++ {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		e := f.permitted(check, u)
		cancel()
		if e != nil {
			return nil, e
		}
		requestCtx, cancelRequest := context.WithCancel(ctx)
		req, e := http.NewRequestWithContext(requestCtx, method, u.String(), nil)
		if e != nil {
			cancelRequest()
			return nil, ErrPolicy
		}
		for _, k := range []string{"Accept", "Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
			for _, v := range headers.Values(k) {
				if len(v) > 8192 || strings.ContainsAny(v, "\r\n") {
					cancelRequest()
					return nil, ErrPolicy
				}
				req.Header.Add(k, v)
			}
		}
		if origin(u) == origin(f.origin) {
			for k, v := range f.cfg.Credentials {
				req.Header[k] = append([]string(nil), v...)
			}
			if bearer != "" {
				req.Header.Set("Authorization", bearer)
			}
		}
		headerTimer := time.AfterFunc(30*time.Second, cancelRequest)
		resp, e := f.client.Do(req)
		headerTimer.Stop()
		if e != nil {
			cancelRequest()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, classify(e, u.Hostname())
		}
		resp.Body = &responseBody{ReadCloser: resp.Body, ctx: requestCtx, cancel: cancelRequest, timer: time.AfterFunc(30*time.Second, cancelRequest)}
		if resp.StatusCode != 301 && resp.StatusCode != 302 && resp.StatusCode != 303 && resp.StatusCode != 307 && resp.StatusCode != 308 {
			return resp, nil
		}
		location, e := resp.Location()
		resp.Body.Close()
		if e != nil {
			return nil, ErrPolicy
		}
		u, e = parse(location.String(), f.cfg.AllowHTTP)
		if e != nil {
			return nil, e
		}
		if !f.allowed[origin(u)] {
			return nil, &RedirectError{Status: resp.StatusCode}
		}
	}
	return nil, ErrPolicy
}

// Response body progress is bounded independently of connection reuse/HTTP2.
type responseBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
}

func (b *responseBody) Read(p []byte) (int, error) {
	b.timer.Reset(30 * time.Second)
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		if b.ctx.Err() != nil {
			return n, b.ctx.Err()
		}
		return n, ErrUnavailable
	}
	return n, err
}
func (b *responseBody) Close() error {
	b.timer.Stop()
	b.cancel()
	if b.ReadCloser.Close() != nil {
		return ErrUnavailable
	}
	return nil
}

// libraryPath adds Docker Hub's implicit "library/" namespace to a
// single-segment repository path (/v2/alpine/... -> /v2/library/alpine/...).
func libraryPath(host, path string) string {
	switch strings.ToLower(host) {
	case "registry-1.docker.io", "index.docker.io", "registry.hub.docker.com":
	default:
		return path
	}
	rest, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return path
	}
	name, tail, ok := strings.Cut(rest, "/")
	if !ok || name == "" {
		return path
	}
	switch name {
	case "library", "_catalog":
		return path
	}
	for _, kind := range []string{"manifests/", "blobs/", "tags/", "referrers/"} {
		if strings.HasPrefix(tail, kind) {
			return "/v2/library/" + rest
		}
	}
	return path
}
