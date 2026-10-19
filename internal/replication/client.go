package replication

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/federation/peer"
)

// ValidatePeerURL requires HTTPS. Plain HTTP is accepted only for a loopback
// host (development and tests). Credentials, queries and fragments are rejected;
// a base path prefix is allowed so peers can sit behind a reverse proxy.
func ValidatePeerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(raw) > 2048 {
		return "", ErrInput
	}
	switch u.Scheme {
	case "https":
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", ErrInput
		}
	default:
		return "", ErrInput
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

// peerClient talks to one peer with a per-peer token and optional private CA.
// It never follows redirects, so the bearer token cannot be forwarded to a
// different origin.
type peerClient struct {
	base   string
	token  string
	prefix string // export feed path prefix
	// sign, when set, authenticates the request with the federation member
	// signature (mTLS transport) instead of the bearer token.
	sign func(*http.Request)
	http *http.Client
}

func newPeerClient(base, token, caPath string) (*peerClient, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	if caPath != "" {
		pem, err := readBounded(caPath, 1<<20)
		if err != nil {
			return nil, ErrInput
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, ErrInput
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return &peerClient{base: base, token: token, prefix: "/api/v1/replication/export/feeds/", http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrInput
	}
	return b, nil
}
func (c *peerClient) do(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	return c.doRange(ctx, path, q, -1)
}

// doRange issues a GET, with a "bytes=offset-" Range header when offset > 0.
// 200 and 206 are accepted; 410 maps to *HistoryPurgedError.
func (c *peerClient) doRange(ctx context.Context, path string, q url.Values, offset int64) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, ErrInput
	}
	if c.sign != nil {
		c.sign(req)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s", ErrPeer, "request failed")
	}
	if resp.StatusCode == http.StatusOK || (offset > 0 && resp.StatusCode == http.StatusPartialContent) {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		low, _ := strconv.ParseInt(resp.Header.Get("X-Replication-Low-Water"), 10, 64)
		return nil, &HistoryPurgedError{LowWater: low}
	}
	return nil, fmt.Errorf("%w: status %d", ErrPeer, resp.StatusCode)
}
func (c *peerClient) json(ctx context.Context, path string, q url.Values, dst any) error {
	resp, err := c.do(ctx, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(dst) != nil {
		return fmt.Errorf("%w: malformed response", ErrPeer)
	}
	return nil
}
func (c *peerClient) feedPath(feed string) string { return c.prefix + feed }

// newFederationClient talks to a member's federation listener: mTLS with
// the member certificate (server CN pinned to the member), every request
// signed with the member key, no redirects and no system trust store.
func newFederationClient(base, member string, id peer.Identity, now func() time.Time) *peerClient {
	transport := &http.Transport{
		TLSClientConfig:       peer.ClientTLS(id, member),
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &peerClient{base: strings.TrimRight(base, "/"), prefix: FederationExportPrefix, sign: func(r *http.Request) { peer.SignRequest(r, id, nil, now()) },
		http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *peerClient) Head(ctx context.Context, feed string) (Head, error) {
	var h Head
	err := c.json(ctx, c.feedPath(feed)+"/head", nil, &h)
	return h, err
}
func addFilters(q url.Values, f Filters) {
	for _, v := range f.Namespaces {
		q.Add("namespace", v)
	}
	for _, v := range f.Packages {
		q.Add("package", v)
	}
	for _, v := range f.VersionPrefixes {
		q.Add("version_prefix", v)
	}
}

// Manifest pages the peer's manifest. source, when set, restricts it to one
// attached source (back-fill).
func (c *peerClient) Manifest(ctx context.Context, feed, kind, after string, limit int, includeCached bool, f Filters, source string) (ManifestPage, error) {
	q := url.Values{"kind": {kind}, "limit": {strconv.Itoa(limit)}, "include_cached": {strconv.FormatBool(includeCached)}}
	if after != "" {
		q.Set("after", after)
	}
	if source != "" {
		q.Set("source", source)
	}
	addFilters(q, f)
	var p ManifestPage
	err := c.json(ctx, c.feedPath(feed)+"/manifest", q, &p)
	return p, err
}

// Changes reads changes after a cursor with server-side filtering. A cursor
// below the peer's low-water mark yields *HistoryPurgedError.
func (c *peerClient) Changes(ctx context.Context, feed string, after int64, limit int, includeCached bool, f Filters) (ChangePage, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}, "include_cached": {strconv.FormatBool(includeCached)}}
	addFilters(q, f)
	var p ChangePage
	err := c.json(ctx, c.feedPath(feed)+"/changes", q, &p)
	return p, err
}

// Template fetches the peer's non-secret feed configuration template.
func (c *peerClient) Template(ctx context.Context, feed string) (Template, error) {
	var t Template
	err := c.json(ctx, c.feedPath(feed)+"/template", nil, &t)
	return t, err
}

// Blob opens the blob stream from offset. start is the offset the returned
// stream actually begins at: offset when the peer honored the Range request
// (206 with a matching Content-Range), otherwise 0 (the peer sent the whole
// blob). length is the number of bytes the stream will deliver, or -1.
func (c *peerClient) Blob(ctx context.Context, feed, digest string, includeCached bool, offset int64) (body io.ReadCloser, start, length int64, err error) {
	resp, err := c.doRange(ctx, c.feedPath(feed)+"/blobs/"+digest, url.Values{"include_cached": {strconv.FormatBool(includeCached)}}, offset)
	if err != nil {
		return nil, 0, 0, err
	}
	if resp.StatusCode == http.StatusPartialContent {
		var from, to, total int64
		if _, scanErr := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &from, &to, &total); scanErr != nil || from != offset {
			resp.Body.Close()
			return nil, 0, 0, fmt.Errorf("%w: unexpected Content-Range", ErrPeer)
		}
		return resp.Body, from, resp.ContentLength, nil
	}
	return resp.Body, 0, resp.ContentLength, nil
}
