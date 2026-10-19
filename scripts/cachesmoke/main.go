// Command cachesmoke is a local real-client smoke test for cache feeds. It
// starts a disposable PostgreSQL cluster and a real Ant server, creates an OCI
// cache feed "buildcache" and a raw cache feed "sccache", and then drives:
//
//   - real sccache (WebDAV) through cargo: a cold build writes the cache, a
//     second build after `cargo clean` hits it;
//   - real docker buildx with --cache-to/--cache-from type=registry: the second
//     build is served from the cache, and re-pushing the same cache tag releases
//     the superseded layers through the short sweep;
//   - the branch cleanup API with a scoped CI token that holds delete on the
//     cache feeds only.
//
// Needs PostgreSQL binaries, Go, sccache, cargo, docker with buildx. Every
// secret stays in private temporary files or child environments; only results
// are printed.
//
// Usage: go run ./scripts/cachesmoke [-sccache=false] [-buildx=false]
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/localfixture"
)

func main() {
	doSccache := flag.Bool("sccache", true, "run the sccache scenario")
	doBuildx := flag.Bool("buildx", true, "run the buildx scenario")
	flag.Parse()
	if err := run(*doSccache, *doBuildx); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("cachesmoke: ok")
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type env struct {
	f      *localfixture.Fixture
	base   string
	admin  string
	ci     string
	client *http.Client
}

func (e *env) do(method, path, token string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(method, e.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (e *env) api(method, path, token string, body any, revision string) (map[string]any, int, error) {
	var payload []byte
	h := map[string]string{"Idempotency-Key": sum([]byte(method + path + fmt.Sprint(body)))}
	if body != nil {
		payload, _ = json.Marshal(body)
		h["Content-Type"] = "application/json"
		h["Idempotency-Key"] = sum([]byte(method + path + string(payload)))
	}
	if revision != "" {
		h["If-Match"] = `"` + revision + `"`
	}
	if method == "POST" && strings.HasSuffix(path, "/tokens") {
		delete(h, "Idempotency-Key") // credentials are one-shot: the server refuses replay keys
	}
	status, data, err := e.do(method, path, token, payload, h)
	if err != nil {
		return nil, 0, err
	}
	var out map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return out, status, nil
}

func (e *env) must(method, path string, body any, revision string) (map[string]any, error) {
	out, status, err := e.api(method, path, e.admin, body, revision)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d %v", method, path, status, out["message"])
	}
	return out, nil
}

func rev(m map[string]any) string {
	if v, ok := m["revision"].(float64); ok {
		return fmt.Sprintf("%d", int64(v))
	}
	return ""
}

func run(doSccache, doBuildx bool) (err error) {
	out, e := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if e != nil {
		return fmt.Errorf("run from inside the repository: %w", e)
	}
	repo := strings.TrimSpace(string(out))
	f, err := localfixture.New(repo, "ant-cache-", "ant_test")
	if err != nil {
		return err
	}
	defer f.Cleanup()
	// The shortest intervals the configuration accepts, so the cache tick and the
	// short sweep show up within a few minutes.
	f.SetEnv("ANT_PRESSURE_INTERVAL", "1m")
	f.SetEnv("ANT_PRESSURE_SWEEP_GRACE", "1m")
	if err = f.StartPostgres(); err != nil {
		return err
	}
	if err = f.Build(); err != nil {
		return err
	}
	raw, err := f.Bootstrap("cache-smoke")
	if err != nil {
		return err
	}
	var cred struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(raw, &cred); err != nil || cred.Token == "" {
		return fmt.Errorf("bootstrap returned no token")
	}
	port, err := localfixture.FreePort()
	if err != nil {
		return err
	}
	server, err := f.StartServer(port, f.Path("service.log"), false)
	if err != nil {
		if log, e := os.ReadFile(f.Path("service.log")); e == nil {
			fmt.Fprintln(os.Stderr, tail(string(log)))
		}
		return err
	}
	defer server.Terminate(context.Background())
	e2 := &env{f: f, base: server.Base, admin: cred.Token, client: &http.Client{Timeout: 30 * time.Second}}

	feeds := map[string]string{}
	for _, c := range []struct{ slug, format string }{{"buildcache", "oci"}, {"sccache", "raw"}} {
		src, err := e2.must("POST", "/api/v1/sources", map[string]any{"name": c.slug + "-local", "kind": "local", "format": c.format}, "")
		if err != nil {
			return err
		}
		feed, err := e2.must("POST", "/api/v1/feeds/batch", map[string]any{
			"slug": c.slug, "format": c.format, "purpose": "cache",
			"cache_policy":   map[string]any{"max_idle_seconds": 7 * 86400},
			"source_ids":     []string{fmt.Sprint(src["id"])},
			"publish_policy": map[string]any{"default_source_id": src["id"], "rules": []any{}},
		}, "")
		if err != nil {
			return err
		}
		fd := feed["feed"].(map[string]any)
		feeds[c.slug] = fmt.Sprint(fd["id"])
		fmt.Printf("created %s cache feed %q purpose=%v\n", c.format, c.slug, fd["purpose"])
	}
	releases, err := e2.must("POST", "/api/v1/feeds", map[string]any{"slug": "releases", "format": "raw"}, "")
	if err != nil {
		return err
	}
	// A cache feed cannot be created over another format.
	if _, status, _ := e2.api("POST", "/api/v1/feeds", e2.admin, map[string]any{"slug": "npmcache", "format": "npm", "purpose": "cache"}, ""); status != 400 {
		return fmt.Errorf("a npm cache feed was not rejected (HTTP %d)", status)
	}
	fmt.Println("npm cache feed rejected with 400")

	// The CI identity: a service principal whose role grants read, publish and
	// delete on the two cache feeds only, and a token scoped to the same.
	principal, err := e2.must("POST", "/api/v1/principals", map[string]any{"subject": "ci-smoke", "kind": "service"}, "")
	if err != nil {
		return err
	}
	role, err := e2.must("POST", "/api/v1/roles", map[string]any{"name": "ci-cache"}, "")
	if err != nil {
		return err
	}
	var grants []any
	var scopes []any
	for _, id := range feeds {
		grants = append(grants, map[string]any{"kind": "artifact", "effect": "allow", "feed_id": id, "actions": []string{"read", "publish", "delete"}})
		scopes = append(scopes, map[string]any{"feed_id": id, "actions": []string{"read", "publish", "delete"}})
	}
	if _, err = e2.must("PUT", "/api/v1/roles/"+fmt.Sprint(role["id"])+"/grants", map[string]any{"grants": grants}, rev(role)); err != nil {
		return err
	}
	if _, err = e2.must("PUT", "/api/v1/principals/"+fmt.Sprint(principal["id"])+"/roles", map[string]any{"role_ids": []string{fmt.Sprint(role["id"])}}, rev(principal)); err != nil {
		return err
	}
	tok, err := e2.must("POST", "/api/v1/principals/"+fmt.Sprint(principal["id"])+"/tokens", map[string]any{"scopes": scopes, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "")
	if err != nil {
		return err
	}
	e2.ci = fmt.Sprint(tok["secret"])
	fmt.Println("issued a CI token scoped to the cache feeds (read, publish, delete)")

	if doSccache {
		if err = sccacheScenario(e2, port); err != nil {
			return fmt.Errorf("sccache: %w", err)
		}
	}
	if doBuildx {
		// The buildkit container reaches the host, not its loopback: forward a
		// public port to the loopback-only server.
		forward, ferr := forwardTo(port)
		if ferr != nil {
			return ferr
		}
		defer forward.Close()
		if err = buildxScenario(e2, forward.Addr().(*net.TCPAddr).Port, feeds["buildcache"]); err != nil {
			return fmt.Errorf("buildx: %w", err)
		}
	}
	// Authorization of the cleanup API.
	if _, status, _ := e2.api("DELETE", "/api/v1/feeds/"+fmt.Sprint(releases["id"])+"/cache?prefix=x", e2.ci, nil, ""); status != 404 && status != 403 {
		return fmt.Errorf("CI token could reach a feed outside its grants (HTTP %d)", status)
	}
	if _, status, _ := e2.api("DELETE", "/api/v1/feeds/"+fmt.Sprint(releases["id"])+"/cache?prefix=x", e2.admin, nil, ""); status != 409 {
		return fmt.Errorf("cleanup of a non-cache feed was not refused with 409 (HTTP %d)", status)
	}
	fmt.Println("cleanup refused on non-cache feeds and outside the CI grants")
	return nil
}

func psql(f *localfixture.Fixture, query string) (string, error) {
	cmd := tool("psql", "-h", f.Root, "-U", "ant", "-d", "ant_test", "-At", "-c", query)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

var statLine = regexp.MustCompile(`(?m)^(Compile requests|Cache hits|Cache misses|Cache hits rate|Cache writes|Cache write errors|Cache errors|Non-cacheable calls)\s+.*$`)

func sccacheScenario(e *env, port int) error {
	for _, auth := range []string{"token", "basic"} {
		if err := sccacheRun(e, port, auth); err != nil {
			return fmt.Errorf("%s auth: %w", auth, err)
		}
	}
	return nil
}

func sccacheRun(e *env, port int, auth string) error {
	fmt.Printf("=== sccache with %s authentication\n", auth)
	work, err := os.MkdirTemp("", "cachesmoke-sccache-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	crate := filepath.Join(work, "demo")
	if err = os.MkdirAll(filepath.Join(crate, "src"), 0o755); err != nil {
		return err
	}
	// A small workspace: three library crates and a binary, so several rustc
	// invocations are cacheable.
	files := map[string]string{
		"Cargo.toml":  "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n[dependencies]\nlib-a = { path = \"lib-a\" }\nlib-b = { path = \"lib-b\" }\nlib-c = { path = \"lib-c\" }\n",
		"src/main.rs": "fn main() { println!(\"{}\", lib_a::f() + lib_b::f() + lib_c::f()); }\n",
	}
	for _, n := range []string{"a", "b", "c"} {
		files["lib-"+n+"/Cargo.toml"] = "[package]\nname = \"lib-" + n + "\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"
		files["lib-"+n+"/src/lib.rs"] = "pub fn f() -> u64 { (1..=20u64).map(|x| x * " + n + "_k()).sum() }\nfn " + n + "_k() -> u64 { 3 }\n"
	}
	for name, body := range files {
		if err = os.MkdirAll(filepath.Dir(filepath.Join(crate, name)), 0o755); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(crate, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	sport, err := localfixture.FreePort()
	if err != nil {
		return err
	}
	cmdEnv := append(os.Environ(),
		"RUSTC_WRAPPER=sccache", "CARGO_INCREMENTAL=0",
		fmt.Sprintf("SCCACHE_SERVER_PORT=%d", sport),
		fmt.Sprintf("SCCACHE_WEBDAV_ENDPOINT=http://127.0.0.1:%d/feeds/sccache/raw", port),
		"SCCACHE_WEBDAV_KEY_PREFIX=smoke/"+auth,
		"CARGO_TARGET_DIR="+filepath.Join(work, "target"))
	if auth == "token" {
		cmdEnv = append(cmdEnv, "SCCACHE_WEBDAV_TOKEN="+e.ci)
	} else {
		cmdEnv = append(cmdEnv, "SCCACHE_WEBDAV_USERNAME=ci", "SCCACHE_WEBDAV_PASSWORD="+e.ci)
	}
	sc := func(args ...string) (string, error) {
		cmd := tool(args[0], args[1:]...)
		cmd.Dir, cmd.Env = crate, cmdEnv
		o, err := cmd.CombinedOutput()
		return string(o), err
	}
	defer func() { _, _ = sc("sccache", "--stop-server") }()
	_, _ = sc("sccache", "--stop-server")
	if o, err := sc("sccache", "--start-server"); err != nil {
		return fmt.Errorf("start sccache: %w\n%s", err, redact(o, e))
	}
	if o, err := sc("cargo", "build", "--quiet"); err != nil {
		return fmt.Errorf("cold build: %w\n%s", err, redact(o, e))
	}
	cold, _ := sc("sccache", "--show-stats")
	fmt.Println("--- sccache after the cold build")
	fmt.Println(strings.Join(statLine.FindAllString(cold, -1), "\n"))
	if _, err = sc("cargo", "clean"); err != nil {
		return err
	}
	if _, err = sc("sccache", "--zero-stats"); err != nil {
		return err
	}
	if o, err := sc("cargo", "build", "--quiet"); err != nil {
		return fmt.Errorf("warm build: %w\n%s", err, redact(o, e))
	}
	warm, _ := sc("sccache", "--show-stats")
	fmt.Println("--- sccache after cargo clean and a second build")
	fmt.Println(strings.Join(statLine.FindAllString(warm, -1), "\n"))
	if !regexp.MustCompile(`(?m)^Cache hits\s+[1-9]`).MatchString(warm) {
		return fmt.Errorf("the second build had no cache hits")
	}
	if !regexp.MustCompile(`(?m)^Cache write errors\s+0\s*$`).MatchString(warm + cold) {
		return fmt.Errorf("cache write errors reported")
	}
	n, _ := psql(e.f, "SELECT count(*) FROM components c JOIN feed_sources fs ON fs.source_id=c.source_id JOIN feeds f ON f.id=fs.feed_id WHERE f.slug='sccache'")
	fmt.Println("objects in the sccache feed:", n)
	// Branch cleanup with the CI token.
	res, status, err := e.api("DELETE", "/api/v1/feeds/"+feedID(e, "sccache")+"/cache?prefix=smoke/"+auth+"/", e.ci, nil, "")
	if err != nil || status != 200 {
		return fmt.Errorf("sccache cleanup: HTTP %d", status)
	}
	fmt.Printf("cleanup smoke/%s/: matched=%v versions_removed=%v bytes_released=%v\n", auth, res["matched"], res["versions_removed"], res["bytes_released"])
	again, _, _ := e.api("DELETE", "/api/v1/feeds/"+feedID(e, "sccache")+"/cache?prefix=smoke/"+auth+"/", e.ci, nil, "")
	if fmt.Sprint(again["matched"]) != "0" {
		return fmt.Errorf("repeating the cleanup removed more content")
	}
	fmt.Println("repeating the cleanup removes nothing (idempotent)")
	return nil
}

func feedID(e *env, slug string) string {
	out, _, _ := e.api("GET", "/api/v1/feeds?prefix="+slug, e.admin, nil, "")
	items, _ := out["items"].([]any)
	for _, it := range items {
		m := it.(map[string]any)
		if m["slug"] == slug {
			return fmt.Sprint(m["id"])
		}
	}
	return ""
}

func redact(s string, e *env) string {
	for _, secret := range []string{e.ci, e.admin} {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "<redacted>")
		}
	}
	return s
}

func buildxScenario(e *env, port int, feed string) error {
	host := fmt.Sprintf("host.docker.internal:%d", port)
	work, err := os.MkdirTemp("", "cachesmoke-buildx-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	dockerCfg := filepath.Join(work, "docker")
	if err = os.MkdirAll(dockerCfg, 0o700); err != nil {
		return err
	}
	auth := base64.StdEncoding.EncodeToString([]byte("ci:" + e.ci))
	// The private docker config keeps the user's CLI plugins and contexts (via
	// symlinks) but none of their registry credentials.
	home, _ := os.UserHomeDir()
	for _, name := range []string{"cli-plugins", "contexts", "buildx"} {
		_ = os.Symlink(filepath.Join(home, ".docker", name), filepath.Join(dockerCfg, name))
	}
	context := "default"
	if user, err := os.ReadFile(filepath.Join(home, ".docker", "config.json")); err == nil {
		var c struct {
			CurrentContext string `json:"currentContext"`
		}
		if json.Unmarshal(user, &c) == nil && c.CurrentContext != "" {
			context = c.CurrentContext
		}
	}
	cfg, _ := json.Marshal(map[string]any{"currentContext": context, "auths": map[string]any{host: map[string]string{"auth": auth}}})
	if err = os.WriteFile(filepath.Join(dockerCfg, "config.json"), cfg, 0o600); err != nil {
		return err
	}
	toml := fmt.Sprintf("[registry.%q]\n  http = true\n  insecure = true\n", host)
	tomlPath := filepath.Join(work, "buildkitd.toml")
	if err = os.WriteFile(tomlPath, []byte(toml), 0o600); err != nil {
		return err
	}
	ctxDir := filepath.Join(work, "ctx")
	if err = os.MkdirAll(ctxDir, 0o755); err != nil {
		return err
	}
	dockerfile := func(tail string) error {
		body := "FROM alpine:3.20\n" +
			"RUN echo one > /one && head -c 3000000 /dev/urandom > /blob1\n" +
			"RUN echo two > /two && head -c 3000000 /dev/urandom > /blob2\n" +
			"COPY app.txt /app.txt\n" + tail
		return os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(body), 0o644)
	}
	if err = os.WriteFile(filepath.Join(ctxDir, "app.txt"), []byte("app v1\n"), 0o644); err != nil {
		return err
	}
	if err = dockerfile(""); err != nil {
		return err
	}
	builder := fmt.Sprintf("cachesmoke-%d", port)
	denv := append(os.Environ(), "DOCKER_CONFIG="+dockerCfg, "BUILDX_NO_DEFAULT_ATTESTATIONS=1")
	d := func(args ...string) (string, error) {
		cmd := tool("docker", args...)
		cmd.Env, cmd.Dir = denv, ctxDir
		o, err := cmd.CombinedOutput()
		return redact(string(o), e), err
	}
	// buildx needs its own config dir for the builder instance, but it must not
	// inherit the user's docker contexts: create the builder against the default context.
	if o, err := d("buildx", "create", "--name", builder, "--driver", "docker-container", "--config", tomlPath); err != nil {
		return fmt.Errorf("buildx create: %w\n%s", err, o)
	}
	defer func() { _, _ = d("buildx", "rm", "--force", builder) }()
	arch := runtime.GOARCH
	ref := host + "/buildcache/smoke:main-" + arch
	build := func(label, platform string) (time.Duration, string, error) {
		start := time.Now()
		o, err := d("buildx", "build", "--builder", builder, "--progress=plain", "--platform", platform,
			"--cache-to", "type=registry,ref="+ref+",mode=max,image-manifest=true,ignore-error=true",
			"--cache-from", "type=registry,ref="+ref,
			"--output", "type=cacheonly", ".")
		return time.Since(start), o, err
	}
	cachedSteps := func(out string) int { return strings.Count(out, "CACHED") }
	fmt.Println("--- buildx build 1 (cold, exports the cache)")
	t1, o1, err := build("cold", "linux/"+arch)
	if err != nil {
		return fmt.Errorf("cold build: %w\n%s", err, tail(o1))
	}
	fmt.Printf("build 1: %s, cached steps: %d\n", t1.Round(time.Millisecond), cachedSteps(o1))
	if !strings.Contains(o1, "preparing build cache for export") && !strings.Contains(o1, "writing layer") {
		fmt.Println(tail(o1))
	}
	// Drop the builder's local cache so the next build can only be served by the registry.
	if o, err := d("buildx", "prune", "--builder", builder, "--all", "--force"); err != nil {
		return fmt.Errorf("prune: %w\n%s", err, o)
	}
	fmt.Println("--- buildx build 2 (local cache pruned; cache-from the registry)")
	t2, o2, err := build("warm", "linux/"+arch)
	if err != nil {
		return fmt.Errorf("warm build: %w\n%s", err, tail(o2))
	}
	hits := cachedSteps(o2)
	fmt.Println(grepLines(o2, "DONE", "CACHED", "importing cache", "resolve ", "sha256:"))
	fmt.Printf("build 2: %s, cached steps: %d (importing cache manifest: %v)\n", t2.Round(time.Millisecond), hits, strings.Contains(o2, "importing cache manifest"))
	if hits < 2 {
		return fmt.Errorf("the second build did not hit the registry cache\n%s", tail(o2))
	}
	repoState := func() string {
		s, _ := psql(e.f, "SELECT count(*)||' versions, '||(SELECT count(*) FROM tags t JOIN components c ON c.id=t.component_id WHERE c.coordinate='smoke')||' tag(s), '||(SELECT count(*) FROM blobs)||' blobs ('||(SELECT COALESCE(sum(size_bytes),0) FROM blobs)||' bytes), '||(SELECT count(*) FROM gc_candidates)||' gc candidates' FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='smoke'")
		return s
	}
	fmt.Println("registry state after build 2:", repoState())

	// Overwrite the tag with a different cache: change the last layer and the
	// middle one, so some layers become unreferenced.
	if err = os.WriteFile(filepath.Join(ctxDir, "app.txt"), []byte("app v2\n"), 0o644); err != nil {
		return err
	}
	if err = dockerfile("RUN head -c 3000000 /dev/urandom > /blob3\n"); err != nil {
		return err
	}
	if o, err := d("buildx", "prune", "--builder", builder, "--all", "--force"); err != nil {
		return fmt.Errorf("prune: %w\n%s", err, o)
	}
	// Export only the new build's cache (no cache-from) so the new manifest shares nothing but the base.
	o3, err := d("buildx", "build", "--builder", builder, "--progress=plain", "--platform", "linux/"+arch,
		"--cache-to", "type=registry,ref="+ref+",mode=max,image-manifest=true,ignore-error=true",
		"--no-cache", "--output", "type=cacheonly", ".")
	if err != nil {
		return fmt.Errorf("overwrite build: %w\n%s", err, tail(o3))
	}
	fmt.Println("registry state right after overwriting the tag:", repoState())
	deadline := time.Now().Add(200 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := psql(e.f, "SELECT count(*) FROM gc_candidates")
		if n == "0" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("registry state after the short sweep:", repoState())
	if n, _ := psql(e.f, "SELECT count(*) FROM gc_candidates"); n != "0" {
		return fmt.Errorf("superseded layers were not swept (%s candidates left)", n)
	}

	// Branch cleanup through the API with the CI token.
	res, status, err := e.api("DELETE", "/api/v1/feeds/"+feed+"/cache?prefix=smoke:main-", e.ci, nil, "")
	if err != nil || status != 200 {
		return fmt.Errorf("buildcache cleanup: HTTP %d", status)
	}
	fmt.Printf("cleanup smoke:main-: tags_removed=%v versions_removed=%v bytes_released=%v\n", res["tags_removed"], res["versions_removed"], res["bytes_released"])
	deadline = time.Now().Add(200 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := psql(e.f, "SELECT count(*) FROM blobs")
		if n == "0" {
			break
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println("registry state after cleanup and the short sweep:", repoState())
	if n, _ := psql(e.f, "SELECT count(*) FROM blobs"); n != "0" {
		return fmt.Errorf("blobs left after cleanup: %s", n)
	}
	return nil
}

// grepLines keeps the progress lines that mention any of the words.
func grepLines(s string, words ...string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		for _, w := range words {
			if strings.Contains(l, w) {
				keep = append(keep, l)
				break
			}
		}
	}
	return strings.Join(keep, "\n")
}

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	return strings.Join(lines, "\n")
}

// tool builds the command for one of the external tools the smoke test drives.
// The program name is always a literal.
func tool(name string, args ...string) *exec.Cmd {
	switch name {
	case "sccache":
		return exec.Command("sccache", args...)
	case "cargo":
		return exec.Command("cargo", args...)
	case "docker":
		return exec.Command("docker", args...)
	case "psql":
		return exec.Command("psql", args...)
	}
	panic("unknown tool " + name)
}

// forwardTo listens on all interfaces and relays every connection to the
// loopback port, so a container can reach the server through the host.
func forwardTo(port int) (net.Listener, error) {
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			in, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer in.Close()
				out, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					return
				}
				defer out.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(out, in); done <- struct{}{} }()
				go func() { _, _ = io.Copy(in, out); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return l, nil
}
