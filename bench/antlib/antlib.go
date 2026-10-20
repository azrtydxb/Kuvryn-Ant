// Package antlib is a small management-API and kubectl helper shared by the benchmark runner and the
// evidence tools. Tokens are read from ~/.kuvryn-ant/<ctx>-automation-token.txt and never printed.
// TLS verification is disabled for the management client because the kw ingress uses a private CA.
package antlib

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Instances maps a short name to its public URL and kubectl context. The URLs come from
// ANT_OKE_URL and ANT_KW_URL (there is no default: they name deployments, not code), the
// contexts from ANT_OKE_CONTEXT and ANT_KW_CONTEXT (defaults "azrty-oke" and "kw").
var Instances = map[string][2]string{
	"oke": {os.Getenv("ANT_OKE_URL"), envOr("ANT_OKE_CONTEXT", "azrty-oke")},
	"kw":  {os.Getenv("ANT_KW_URL"), envOr("ANT_KW_CONTEXT", "kw")},
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// Configured reports whether the named instance has a URL (see Instances).
func Configured(name string) bool { return Instances[name][0] != "" }

// Obj is a decoded JSON object.
type Obj map[string]any

// Str returns a string field.
func (o Obj) Str(k string) string { s, _ := o[k].(string); return s }

// Int returns a numeric field.
func (o Obj) Int(k string) int64 { f, _ := o[k].(float64); return int64(f) }

// Bool returns a boolean field.
func (o Obj) Bool(k string) bool { b, _ := o[k].(bool); return b }

// Obj returns a nested object.
func (o Obj) Obj(k string) Obj { m, _ := o[k].(map[string]any); return Obj(m) }

// Items returns the objects of a list field (default "items").
func (o Obj) Items(k ...string) []Obj {
	key := "items"
	if len(k) > 0 {
		key = k[0]
	}
	var out []Obj
	if a, ok := o[key].([]any); ok {
		for _, v := range a {
			if m, ok := v.(map[string]any); ok {
				out = append(out, Obj(m))
			}
		}
	}
	return out
}

// TokenFor reads the secret of a credential file (JSON with "secret", or "token", or a bare token).
func TokenFor(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		h, _ := os.UserHomeDir()
		path = filepath.Join(h, path[2:])
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var j struct{ Secret, Token string }
	if json.Unmarshal(b, &j) == nil && (j.Secret != "" || j.Token != "") {
		if j.Secret != "" {
			return j.Secret, nil
		}
		return j.Token, nil
	}
	return strings.TrimSpace(string(b)), nil
}

// Api is a management/data-plane client for one instance.
type Api struct {
	Base, Token string
	C           *http.Client
}

// New builds a client.
func New(base, token string) *Api {
	return &Api{Base: strings.TrimRight(base, "/"), Token: token, C: &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // private-CA ingress
	}}
}

// Named builds a client for "oke" or "kw".
func Named(name string) *Api {
	in, ok := Instances[name]
	if !ok {
		panic("unknown instance " + name)
	}
	if in[0] == "" {
		panic("set ANT_" + strings.ToUpper(name) + "_URL to the base URL of the " + name + " instance")
	}
	t, err := TokenFor("~/.kuvryn-ant/" + in[1] + "-automation-token.txt")
	if err != nil {
		panic(err)
	}
	return New(in[0], t)
}

// Opt tunes a request.
type Opt struct {
	Rev     int64
	Raw     []byte
	Headers map[string]string
}

// Req performs a request; body (if non-nil) is sent as JSON.
func (a *Api) Req(method, path string, body any, o ...Opt) (int, http.Header, []byte, error) {
	var opt Opt
	if len(o) > 0 {
		opt = o[0]
	}
	var rd io.Reader
	hdr := map[string]string{}
	if a.Token != "" {
		hdr["Authorization"] = "Bearer " + a.Token
	}
	switch {
	case body != nil:
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
		hdr["Content-Type"] = "application/json"
	case opt.Raw != nil:
		rd = bytes.NewReader(opt.Raw)
	}
	if method != "GET" && !strings.HasSuffix(path, "/tokens") {
		hdr["Idempotency-Key"] = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano(), path))))[:32]
	}
	if opt.Rev != 0 {
		hdr["If-Match"] = fmt.Sprintf("\"%d\"", opt.Rev)
	}
	for k, v := range opt.Headers {
		hdr[k] = v
	}
	req, err := http.NewRequest(method, a.Base+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.C.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data, err
}

// OK is Req that requires a 2xx status and decodes a JSON object (nil for an empty body).
func (a *Api) OK(method, path string, body any, rev ...int64) (Obj, error) {
	var o Opt
	if len(rev) > 0 {
		o.Rev = rev[0]
	}
	s, _, d, err := a.Req(method, path, body, o)
	if err != nil {
		return nil, err
	}
	if s >= 300 {
		return nil, fmt.Errorf("%s %s -> %d %s", method, path, s, trunc(string(d), 300))
	}
	var m Obj
	_ = json.Unmarshal(d, &m)
	return m, nil
}

// Must panics on error (tools are single-shot scripts).
func Must(o Obj, err error) Obj {
	if err != nil {
		panic(err)
	}
	return o
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Rev returns the current revision of /api/v1/<kind>/<id>.
func (a *Api) Rev(kind, id string) int64 {
	return Must(a.OK("GET", "/api/v1/"+kind+"/"+id, nil)).Int("revision")
}

// FeedBySlug finds a feed.
func (a *Api) FeedBySlug(slug string) Obj {
	for _, f := range Must(a.OK("GET", "/api/v1/feeds", nil)).Items() {
		if f.Str("slug") == slug {
			return f
		}
	}
	return nil
}

// MakeFeed creates a feed with one local source and a default publish policy.
func (a *Api) MakeFeed(slug, format string) (feed, src Obj) {
	feed = Must(a.OK("POST", "/api/v1/feeds", Obj{"slug": slug, "format": format, "anonymous_read": false}))
	src = Must(a.OK("POST", "/api/v1/sources", Obj{"name": slug, "kind": "local", "format": format}))
	Must(a.OK("PUT", "/api/v1/feeds/"+feed.Str("id")+"/sources/"+src.Str("id"), Obj{"priority": 0}, a.Rev("feeds", feed.Str("id"))))
	Must(a.OK("PUT", "/api/v1/feeds/"+feed.Str("id")+"/publish-policy", Obj{"default_source_id": src.Str("id"), "rules": []any{}}, a.Rev("feeds", feed.Str("id"))))
	return feed, src
}

// RawPut publishes bytes to a raw feed.
func (a *Api) RawPut(slug, path string, data []byte) int {
	s, _, _, err := a.Req("PUT", "/feeds/"+slug+"/raw/"+path, nil, Opt{Raw: data, Headers: map[string]string{"Content-Type": "application/octet-stream"}})
	if err != nil {
		return 0
	}
	return s
}

// RawGet reads from a raw feed, optionally with a Range header.
func (a *Api) RawGet(slug, path, rng string) (int, http.Header, []byte) {
	h := map[string]string{}
	if rng != "" {
		h["Range"] = rng
	}
	s, hd, d, _ := a.Req("GET", "/feeds/"+slug+"/raw/"+path, nil, Opt{Headers: h})
	return s, hd, d
}

// SHA returns the hex SHA-256.
func SHA(b []byte) string { h := sha256.Sum256(b); return fmt.Sprintf("%x", h) }

// Det returns deterministic bytes (SHA-256 counter mode) for (seed,n).
func Det(seed string, n int) []byte {
	out := make([]byte, 0, n+32)
	for i := 0; len(out) < n; i++ {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", seed, i)))
		out = append(out, h[:]...)
	}
	return out[:n]
}

// Kubectl runs kubectl against a context and returns stdout.
func Kubectl(ctx string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", ctx}, args...)...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		return so.String(), fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, trunc(se.String(), 300))
	}
	return so.String(), nil
}

// Now is an RFC3339 UTC timestamp.
func Now() string { return time.Now().UTC().Format(time.RFC3339) }
