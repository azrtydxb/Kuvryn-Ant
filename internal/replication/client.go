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
	base  string
	token string
	http  *http.Client
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
	return &peerClient{base: base, token: token, http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
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
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, ErrInput
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrPeer, "request failed")
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("%w: status %d", ErrPeer, resp.StatusCode)
	}
	return resp, nil
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
func feedPath(feed string) string { return "/api/v1/replication/export/feeds/" + feed }

func (c *peerClient) Head(ctx context.Context, feed string) (Head, error) {
	var h Head
	err := c.json(ctx, feedPath(feed)+"/head", nil, &h)
	return h, err
}
func (c *peerClient) Manifest(ctx context.Context, feed, kind, after string, limit int, includeCached bool, f Filters) (ManifestPage, error) {
	q := url.Values{"kind": {kind}, "limit": {strconv.Itoa(limit)}, "include_cached": {strconv.FormatBool(includeCached)}}
	if after != "" {
		q.Set("after", after)
	}
	for _, v := range f.Namespaces {
		q.Add("namespace", v)
	}
	for _, v := range f.Packages {
		q.Add("package", v)
	}
	for _, v := range f.VersionPrefixes {
		q.Add("version_prefix", v)
	}
	var p ManifestPage
	err := c.json(ctx, feedPath(feed)+"/manifest", q, &p)
	return p, err
}
func (c *peerClient) Changes(ctx context.Context, feed string, after int64, limit int, includeCached bool) (ChangePage, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}, "include_cached": {strconv.FormatBool(includeCached)}}
	var p ChangePage
	err := c.json(ctx, feedPath(feed)+"/changes", q, &p)
	return p, err
}

// Blob opens the full blob stream. The caller verifies size and digest.
func (c *peerClient) Blob(ctx context.Context, feed, digest string, includeCached bool) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, feedPath(feed)+"/blobs/"+digest, url.Values{"include_cached": {strconv.FormatBool(includeCached)}})
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}
