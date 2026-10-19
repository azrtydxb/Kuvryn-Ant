// Package harness drives real, pinned ecosystem clients (in containers) against a Kuvryn Ant instance
// and records machine-readable evidence. Tokens reach containers by environment variable name only and
// are redacted from every recorded output.
package harness

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed clients.json
var clientsJSON []byte

// ClientSpec is one pinned client container image.
type ClientSpec struct {
	Image    string `json:"image"`
	Shell    string `json:"shell"`
	Ref      string `json:"ref"`
	Platform string `json:"platform"`
	Pnpm     string `json:"pnpm"`
}

// Clients is the pinned client table (clients.json).
var Clients = func() map[string]ClientSpec {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(clientsJSON, &raw); err != nil {
		panic(err)
	}
	out := map[string]ClientSpec{}
	for k, v := range raw {
		var s ClientSpec
		if json.Unmarshal(v, &s) == nil && s.Image != "" {
			out[k] = s
		}
	}
	return out
}()

const outTail = 6000

var authRE = regexp.MustCompile(`(?i)401|403|unauthori[sz]ed|authenticat|denied|forbidden|credential|login|permission`)

// Env is the target instance plus credentials.
type Env struct {
	Target, Base, Token, CA, RunID string
	HTTP                           *http.Client
	Loopback                       bool
	ClientBase                     string
	sidecar                        string
	bridges                        []*http.Server
	mu                             sync.Mutex
}

// NewEnv builds an Env; ca is an optional PEM bundle path for a private CA.
func NewEnv(target, base, token, ca, runID string) (*Env, error) {
	base = strings.TrimRight(base, "/")
	tr := &http.Transport{}
	if ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, err
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(pem)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	h := u.Hostname()
	return &Env{Target: target, Base: base, Token: token, CA: ca, RunID: runID,
		HTTP:     &http.Client{Transport: tr, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Loopback: h == "127.0.0.1" || h == "localhost" || h == "::1", ClientBase: base}, nil
}

// Start launches the loopback forwarder so containers can use 127.0.0.1:<port>.
func (e *Env) Start() error {
	if !e.Loopback {
		return nil
	}
	u, _ := url.Parse(e.Base)
	port := u.Port()
	name := "conf-fwd-" + e.RunID
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, Clients["socat"].Image,
		"TCP-LISTEN:"+port+",fork,reuseaddr", "TCP:host.docker.internal:"+port).CombinedOutput()
	if err != nil {
		return fmt.Errorf("socat sidecar: %v: %s", err, out)
	}
	e.sidecar = name
	time.Sleep(time.Second)
	return nil
}

// Stop removes sidecars and bridges.
func (e *Env) Stop() {
	for _, b := range e.bridges {
		_ = b.Close()
	}
	if e.sidecar != "" {
		_ = exec.Command("docker", "rm", "-f", e.sidecar).Run()
	}
}

func (e *Env) netArgs() []string {
	if e.sidecar != "" {
		return []string{"--network", "container:" + e.sidecar}
	}
	return []string{"--add-host", "host.docker.internal:host-gateway"}
}

// Redact removes the token (and its Basic encodings) from s.
func (e *Env) Redact(s string) string {
	for _, t := range []string{e.Token,
		base64.StdEncoding.EncodeToString([]byte("service:" + e.Token)),
		base64.StdEncoding.EncodeToString([]byte(":" + e.Token))} {
		if t != "" {
			s = strings.ReplaceAll(s, t, "***REDACTED***")
		}
	}
	return s
}

// Bridge returns a client-reachable base URL that adds the bearer token (for clients like Go that refuse
// credentials over plain HTTP). HTTPS targets get userinfo instead.
func (e *Env) Bridge(token string) string {
	if strings.HasPrefix(e.Base, "https://") {
		return strings.Replace(e.Base, "://", "://service:"+token+"@", 1)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequest(r.Method, e.Base+r.URL.RequestURI(), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := e.HTTP.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(resp.StatusCode)
		if r.Method != "HEAD" {
			_, _ = w.Write(body)
		}
	})}
	ln, err := newLoopbackListener()
	if err != nil {
		panic(err)
	}
	go func() { _ = srv.Serve(ln) }()
	e.bridges = append(e.bridges, srv)
	return "http://host.docker.internal:" + fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

// ---- management API

// Api calls /api/v1 with the admin token.
type Api struct{ E *Env }

// Req performs a management call and returns status and decoded JSON (map or nil).
func (a Api) Req(method, path string, body any, rev int64) (int, map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, a.E.Base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", randHex(16))
	r.Header.Set("Authorization", "Bearer "+a.E.Token)
	if rev > 0 {
		r.Header.Set("If-Match", fmt.Sprintf(`"%d"`, rev))
	}
	resp, err := a.E.HTTP.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m == nil {
		m = map[string]any{"_raw": string(data)}
	}
	return resp.StatusCode, m, nil
}

// OK is Req that fails on status >= 300.
func (a Api) OK(method, path string, body any, rev int64) (map[string]any, error) {
	s, m, err := a.Req(method, path, body, rev)
	if err != nil {
		return nil, err
	}
	if s >= 300 {
		j, _ := json.Marshal(m)
		return nil, fmt.Errorf("%s %s -> %d %.400s", method, path, s, j)
	}
	return m, nil
}

// ---- evidence

// Step is one recorded scenario step.
type Step struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Duration int64  `json:"duration_ms"`
	Detail   string `json:"detail"`
	Output   string `json:"output_tail"`
}

// ClientInfo records the client image and the version it reported.
type ClientInfo struct {
	Image   string `json:"image"`
	Version string `json:"reported_version"`
	Pinned  string `json:"pinned_ref"`
}

// Evidence is the per-format results document.
type Evidence struct {
	Format  string                `json:"format"`
	Target  string                `json:"target"`
	RunID   string                `json:"run_id"`
	Started string                `json:"started"`
	Ant     map[string]any        `json:"ant"`
	Clients map[string]ClientInfo `json:"clients"`
	Steps   []*Step               `json:"steps"`
	Dur     int64                 `json:"duration_ms"`
	Summary map[string]int        `json:"summary"`
	Result  string                `json:"result"`
	Cleanup []string              `json:"feed_cleanup"`
}

// Ctx is the per-format scenario context.
type Ctx struct {
	Env     *Env
	Format  string
	RunID   string
	Work    string
	Ev      *Evidence
	API     Api
	feeds   []string
	feedIDs map[string]string
	// LastSource is the source created by the most recent Feed call.
	LastSource string
	curOut     string
	lastDenial time.Time
}

// NewCtx creates the context and its shared work directory (mounted at /work in clients).
func NewCtx(e *Env, format string, ev *Evidence) (*Ctx, error) {
	d, err := os.MkdirTemp("", "conf-"+format+"-")
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(d, 0o777)
	return &Ctx{Env: e, Format: format, RunID: e.RunID, Work: d, Ev: ev, API: Api{e}, feedIDs: map[string]string{}}, nil
}

// Remote describes an upstream for a remote source.
type Remote struct {
	Origin   string
	Redirect []string
}

func (c *Ctx) rev(kind, id string) (int64, error) {
	m, err := c.API.OK("GET", "/api/v1/"+kind+"/"+id, nil, 0)
	if err != nil {
		return 0, err
	}
	f, _ := m["revision"].(float64)
	return int64(f), nil
}

// Feed creates a feed with one source (local or remote) and returns its slug.
func (c *Ctx) Feed(format, tag string, remote *Remote) (string, error) {
	slug := fmt.Sprintf("cf-%s-%s-%s", c.Format, tag, c.RunID)
	if len(slug) > 60 {
		slug = slug[:60]
	}
	slug = strings.ReplaceAll(slug, "_", "-")
	feed, err := c.API.OK("POST", "/api/v1/feeds", map[string]any{"slug": slug, "format": format}, 0)
	if err != nil {
		return "", err
	}
	kind := "local"
	if remote != nil {
		kind = "remote"
	}
	src, err := c.API.OK("POST", "/api/v1/sources", map[string]any{"name": slug, "kind": kind, "format": format}, 0)
	if err != nil {
		return "", err
	}
	fid, sid := feed["id"].(string), src["id"].(string)
	c.feeds = append(c.feeds, fid)
	c.feedIDs[slug] = fid
	if remote != nil {
		red := remote.Redirect
		if red == nil {
			red = []string{}
		}
		cfg := map[string]any{"origin": remote.Origin, "redirect_origins": red, "allow_private": false, "timeout_seconds": 60,
			"cache": map[string]any{"metadata_ttl_seconds": 60, "negative_ttl_seconds": 5, "stale_if_error_seconds": 3600, "max_entries": 10000, "max_bytes": 268435456}}
		r, err := c.rev("sources", sid)
		if err != nil {
			return "", err
		}
		if _, err = c.API.OK("PUT", "/api/v1/sources/"+sid+"/remote-config", cfg, r); err != nil {
			return "", err
		}
	}
	r, err := c.rev("feeds", fid)
	if err != nil {
		return "", err
	}
	if _, err = c.API.OK("PUT", "/api/v1/feeds/"+fid+"/sources/"+sid, map[string]any{"priority": 0}, r); err != nil {
		return "", err
	}
	if remote == nil {
		r, err = c.rev("feeds", fid)
		if err != nil {
			return "", err
		}
		if _, err = c.API.OK("PUT", "/api/v1/feeds/"+fid+"/publish-policy", map[string]any{"default_source_id": sid, "rules": []any{}}, r); err != nil {
			return "", err
		}
	}
	c.LastSource = sid
	return slug, nil
}

// MustFeed is Feed that panics; scenarios recover it into a setup failure.
func (c *Ctx) MustFeed(format, tag string, remote *Remote) string {
	s, err := c.Feed(format, tag, remote)
	if err != nil {
		panic(err)
	}
	return s
}

// SetRemoteOrigin re-points a remote source (used to prove cache hits with the upstream unreachable).
func (c *Ctx) SetRemoteOrigin(sourceID, origin string) error {
	m, err := c.API.OK("GET", "/api/v1/sources/"+sourceID+"/remote-config", nil, 0)
	if err != nil {
		return err
	}
	if inner, ok := m["config"].(map[string]any); ok {
		m = inner
	}
	cfg := map[string]any{}
	for _, k := range []string{"credential_id", "ca_ref", "allow_private", "timeout_seconds", "cache"} {
		if v, ok := m[k]; ok {
			cfg[k] = v
		}
	}
	cfg["origin"], cfg["redirect_origins"] = origin, []any{}
	r, err := c.rev("sources", sourceID)
	if err != nil {
		return err
	}
	_, err = c.API.OK("PUT", "/api/v1/sources/"+sourceID+"/remote-config", cfg, r)
	return err
}

// Unreachable is an origin nothing listens on.
const Unreachable = "https://127.0.0.1:9"

// ---- containers

// RunOpt tunes a client invocation.
type RunOpt struct {
	Env        map[string]string
	ExpectFail bool
	ExpectRe   *regexp.Regexp
	NoAuth     bool
	User       string
}

// Sh runs script in the pinned client container; output is recorded in the step.
func (c *Ctx) Sh(client, script string, o ...RunOpt) error {
	var opt RunOpt
	if len(o) > 0 {
		opt = o[0]
	}
	spec, ok := Clients[client]
	if !ok {
		return fmt.Errorf("unknown client %q", client)
	}
	timeout := 900 * time.Second
	if v := os.Getenv("CONF_CLIENT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil {
			timeout = d
		}
	}
	name := fmt.Sprintf("conf-%s-%s", c.RunID, randHex(3))
	args := []string{"run", "--rm", "--name", name, "-v", c.Work + ":/work", "-w", "/work", "--entrypoint", spec.Shell}
	if opt.User != "" {
		args = append(args, "--user", opt.User)
	}
	if spec.Platform != "" {
		args = append(args, "--platform", spec.Platform)
	}
	args = append(args, c.Env.netArgs()...)
	u, _ := url.Parse(c.Env.ClientBase)
	e := map[string]string{"ANT": c.Env.ClientBase, "HOST": u.Host, "SCHEME": u.Scheme, "RUN": c.RunID, "HOME": "/work/home"}
	if !opt.NoAuth {
		e["TOKEN"] = c.Env.Token
	}
	for k, v := range opt.Env {
		e[k] = v
	}
	cmdEnv := os.Environ()
	for k, v := range e {
		args = append(args, "-e", k)
		cmdEnv = append(cmdEnv, k+"="+v)
	}
	prefix := "mkdir -p /work/home; "
	if c.Env.CA != "" {
		// Tools that replace (rather than extend) their trust store with the variable below must still reach public
		// upstreams, so the private CA is appended to the image's own bundle instead of used alone.
		args = append(args, "-v", c.Env.CA+":/ca/ca.pem:ro")
		prefix += `rm -f /tmp/ca-all.pem; for f in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem; do ` +
			`if [ -f $f ]; then cat $f /ca/ca.pem > /tmp/ca-all.pem; if [ -w $f ]; then cat /ca/ca.pem >> $f; fi; break; fi; done; ` +
			`[ -f /tmp/ca-all.pem ] || cp /ca/ca.pem /tmp/ca-all.pem; ` +
			`export CA_PEM=/tmp/ca-all.pem; for v in SSL_CERT_FILE CURL_CA_BUNDLE NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE GIT_SSL_CAINFO CARGO_HTTP_CAINFO PIP_CERT COMPOSER_CAFILE AWS_CA_BUNDLE; do export $v=/tmp/ca-all.pem; done; `
	}
	args = append(args, spec.Image, "-c", prefix+script)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = cmdEnv
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	runErr := cmd.Run()
	out := c.Env.Redact(buf.String())
	c.curOut = tail(out, outTail)
	if ctx.Err() != nil {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		return fmt.Errorf("client timed out after %s: %s", timeout, tail(out, 1200))
	}
	if opt.ExpectFail {
		if runErr == nil {
			return fmt.Errorf("expected failure but command succeeded: %s", tail(out, 800))
		}
		re := authRE
		if opt.ExpectRe != nil {
			re = opt.ExpectRe
		}
		if !re.MatchString(out) {
			return fmt.Errorf("command failed but not with an auth denial: %s", tail(out, 800))
		}
		return nil
	}
	if runErr != nil {
		return fmt.Errorf("%v: %s", runErr, tail(out, 1800))
	}
	return nil
}

// Out returns the recorded output tail of the last client/HTTP call.
func (c *Ctx) Out() string { return c.curOut }

// Version runs a version command and records the first output line for client.
func (c *Ctx) Version(client, cmd string) error {
	if err := c.Sh(client, cmd, RunOpt{NoAuth: true}); err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(c.curOut), "\n")
	v := ""
	if len(lines) > 0 {
		v = lines[0]
	}
	if len(v) > 200 {
		v = v[:200]
	}
	spec := Clients[client]
	c.Ev.Clients[client] = ClientInfo{Image: spec.Image, Version: v, Pinned: spec.Ref}
	return nil
}

// ---- HTTP from the host

// AuthMode selects the credential for Do.
type AuthMode int

// Credential modes for Do.
const (
	Bearer AuthMode = iota
	Basic
	NoAuth
	BadToken
)

// Req is one direct HTTP call to the instance.
type Req struct {
	Method, Path string
	Body         []byte
	Hdr          map[string]string
	Auth         AuthMode
	Want         []int
}

// Do performs the call, asserting Want (when set), and returns the response body.
func (c *Ctx) Do(r Req) ([]byte, int, http.Header, error) {
	if r.Method == "" {
		r.Method = "GET"
	}
	var rd io.Reader
	if r.Body != nil {
		rd = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequest(r.Method, c.Env.Base+r.Path, rd)
	if err != nil {
		return nil, 0, nil, err
	}
	for k, v := range r.Hdr {
		req.Header.Set(k, v)
	}
	tok := c.Env.Token
	if r.Auth == BadToken {
		tok = "invalid-token"
	}
	switch r.Auth {
	case Bearer, BadToken:
		req.Header.Set("Authorization", "Bearer "+tok)
	case Basic:
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("service:"+tok)))
	}
	resp, err := c.Env.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	c.curOut = c.Env.Redact(tail(string(data), 1500))
	if len(r.Want) > 0 {
		okc := false
		for _, w := range r.Want {
			okc = okc || w == resp.StatusCode
		}
		if !okc {
			return data, resp.StatusCode, resp.Header, fmt.Errorf("%s %s -> %d, want %v: %.300s", r.Method, strings.SplitN(r.Path, "?", 2)[0], resp.StatusCode, r.Want, c.Env.Redact(string(data)))
		}
	}
	return data, resp.StatusCode, resp.Header, nil
}

// Expect is Do that returns only the error.
func (c *Ctx) Expect(method, path string, auth AuthMode, want ...int) error {
	_, _, _, err := c.Do(Req{Method: method, Path: path, Auth: auth, Want: want})
	return err
}

// Get fetches path with the admin token, requiring 200.
func (c *Ctx) Get(path string) ([]byte, error) {
	b, _, _, err := c.Do(Req{Path: path, Want: []int{200}})
	return b, err
}

// ---- management lifecycle

// MgmtVersion applies action (delete|unlist|relist) through the management API to a version of the component
// whose coordinate contains needle. version "" picks the first listed version.
func (c *Ctx) MgmtVersion(slug, needle, version, action string) error {
	fid := c.feedIDs[slug]
	cursor := ""
	comp := ""
	var seen []string
	for i := 0; i < 20 && comp == ""; i++ {
		q := "?limit=100"
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		d, err := c.API.OK("GET", "/api/v1/feeds/"+fid+"/packages"+q, nil, 0)
		if err != nil {
			return err
		}
		items, _ := d["items"].([]any)
		for _, it := range items {
			m := it.(map[string]any)
			nm := fmt.Sprint(m["coordinate"], " ", m["name"], " ", m["namespace"])
			seen = append(seen, nm)
			if strings.Contains(nm, needle) {
				comp = m["id"].(string)
				break
			}
		}
		cursor, _ = d["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	if comp == "" {
		return fmt.Errorf("component %q not found in feed (saw %v)", needle, seen)
	}
	if version == "" {
		vs, err := c.API.OK("GET", "/api/v1/feeds/"+fid+"/packages/"+comp+"/versions?limit=50", nil, 0)
		if err != nil {
			return err
		}
		items, _ := vs["items"].([]any)
		if len(items) == 0 {
			return fmt.Errorf("component %s has no versions", needle)
		}
		version = items[0].(map[string]any)["version"].(string)
	}
	s, d, err := c.API.Req("DELETE", fmt.Sprintf("/api/v1/feeds/%s/packages/%s/versions/%s?action=%s", fid, comp, url.PathEscape(version), action), nil, 0)
	if err != nil {
		return err
	}
	if s != 200 {
		j, _ := json.Marshal(d)
		return fmt.Errorf("mgmt %s %s@%s -> %d %.300s", action, needle, version, s, j)
	}
	return nil
}

// Cleanup removes the workdir and deletes every feed created by the scenario.
func (c *Ctx) Cleanup() {
	_ = os.RemoveAll(c.Work)
	var res []string
	for _, fid := range c.feeds {
		r, err := c.rev("feeds", fid)
		if err == nil {
			var s int
			s, _, err = c.API.Req("DELETE", "/api/v1/feeds/"+fid+"?force=true", nil, r)
			if err == nil {
				res = append(res, fmt.Sprintf("%s:%d", fid[:8], s))
				continue
			}
		}
		res = append(res, fid[:8]+":"+err.Error())
	}
	c.Ev.Cleanup = res
}

// ---- steps

// Opt modifies how a step is recorded.
type Opt func(*stepCfg)

type stepCfg struct{ skip, unsupported string }

// Skip marks the step skipped with reason when reason is non-empty.
func Skip(reason string) Opt { return func(s *stepCfg) { s.skip = reason } }

// Limitation turns a failure into a documented limitation (status "unsupported").
func Limitation(doc string) Opt { return func(s *stepCfg) { s.unsupported = doc } }

// SkipUnless skips when ok is false because the prerequisite step failed.
func SkipUnless(ok bool) Opt {
	if ok {
		return func(*stepCfg) {}
	}
	return Skip("prerequisite step failed")
}

// Step records one step; returns true when it passed.
func (c *Ctx) Step(name string, fn func() error, opts ...Opt) bool {
	var cfg stepCfg
	for _, o := range opts {
		o(&cfg)
	}
	if strings.HasPrefix(name, "auth denial") && cfg.skip == "" {
		// Failed authentication attempts are limited per client address (10 per minute); let the window drain between
		// denial steps so each is judged on its own.
		if wait := 65*time.Second - time.Since(c.lastDenial); !c.lastDenial.IsZero() && wait > 0 {
			time.Sleep(wait)
		}
		defer func() { c.lastDenial = time.Now() }()
	}
	s := &Step{Name: name, Status: "pass"}
	c.curOut = ""
	t := time.Now()
	switch {
	case cfg.skip != "":
		s.Status, s.Detail = "skip", cfg.skip
	default:
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return fn()
		}()
		if err != nil {
			s.Status = "fail"
			s.Detail = tail(c.Env.Redact(err.Error()), 2000)
			if cfg.unsupported != "" {
				s.Status = "unsupported"
				s.Detail = fmt.Sprintf("documented limitation (%s): %s", cfg.unsupported, s.Detail)
			}
		}
		s.Output = c.curOut
	}
	s.Duration = time.Since(t).Milliseconds()
	c.Ev.Steps = append(c.Ev.Steps, s)
	mark := map[string]string{"pass": "PASS", "fail": "FAIL", "skip": "SKIP", "unsupported": "UNSUP"}[s.Status]
	d := ""
	if s.Status != "pass" {
		d = " :: " + strings.ReplaceAll(tail(s.Detail, 300), "\n", " | ")
	}
	fmt.Printf("  [%s] %s (%dms)%s\n", mark, name, s.Duration, d)
	return s.Status == "pass"
}

// ---- small helpers

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// F substitutes @key@ placeholders: F("echo @a@", "a", "1").
func F(tmpl string, kv ...string) string {
	return strings.NewReplacer(withAt(kv)...).Replace(tmpl)
}

func withAt(kv []string) []string {
	out := make([]string, len(kv))
	for i := range kv {
		if i%2 == 0 {
			out[i] = "@" + kv[i] + "@"
		} else {
			out[i] = kv[i]
		}
	}
	return out
}

// Seq runs fns in order and stops at the first error.
func Seq(fns ...func() error) func() error {
	return func() error {
		for _, f := range fns {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	}
}

// Contains is an assertion helper.
func Contains(hay, needle string) error {
	if !strings.Contains(hay, needle) {
		return fmt.Errorf("expected %q in %.300s", needle, hay)
	}
	return nil
}

// WriteFile writes data into the shared work directory.
func (c *Ctx) WriteFile(rel string, data []byte) error {
	p := filepath.Join(c.Work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o666)
}

// ReadFile reads from the shared work directory.
func (c *Ctx) ReadFile(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(c.Work, rel)) }
