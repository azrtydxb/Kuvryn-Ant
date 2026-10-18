// Command ocismoke is a local real-client smoke test for the OCI registry.
// No container/cloud/provider environment is required, only installed
// PostgreSQL binaries, Go and crane. All secrets and fixtures are private
// temporary files; it prints only the client version and validation results.
//
// Usage: go run ./scripts/ocismoke
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/localfixture"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func repoRoot() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		return "", fmt.Errorf("run from inside the repository: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func run() (err error) {
	if _, e := exec.LookPath("crane"); e != nil {
		return errors.New("required tool missing: crane")
	}
	repo, err := repoRoot()
	if err != nil {
		return err
	}
	f, err := localfixture.New(repo, "ant-oci-", "ant_test")
	if err != nil {
		return err
	}
	defer f.Cleanup()
	f.SetEnv("DOCKER_CONFIG", f.Path("docker"))

	// crane runs with the fixture environment; output is never echoed on failure
	// because child tools may print credentials.
	crane := func(env []string, stdin []byte, args ...string) ([]byte, error) {
		cmd := exec.Command("crane", args...)
		cmd.Env = env
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		out, e := cmd.Output()
		if e != nil {
			return out, fmt.Errorf("crane failed: %w", e)
		}
		return out, nil
	}
	mustCrane := func(args ...string) []byte {
		out, e := crane(f.Env, nil, args...)
		if e != nil && err == nil {
			err = e
		}
		return out
	}

	if err = f.StartPostgres(); err != nil {
		return err
	}
	if err = f.Build(); err != nil {
		return err
	}
	raw, err := f.Bootstrap("oci-client-test")
	if err != nil {
		return err
	}
	var credential struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(raw, &credential); err != nil || credential.Token == "" {
		return errors.New("bootstrap returned no token")
	}
	port, err := localfixture.FreePort()
	if err != nil {
		return err
	}
	logPath := f.Path("service.log")
	server, err := f.StartServer(port, logPath, false)
	if err != nil {
		return err
	}
	defer func() {
		if server != nil {
			server.Terminate(context.Background())
		}
	}()
	base := server.Base
	registry := fmt.Sprintf("127.0.0.1:%d", port)
	client := &http.Client{Timeout: 10 * time.Second}

	do := func(method, path string, body []byte, headers map[string]string) (int, http.Header, []byte, error) {
		req, e := http.NewRequest(method, base+path, bytes.NewReader(body))
		if e != nil {
			return 0, nil, nil, e
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, e := client.Do(req)
		if e != nil {
			return 0, nil, nil, e
		}
		defer resp.Body.Close()
		data, e := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, data, e
	}
	api := func(method, path string, body any, revision string) (map[string]any, error) {
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		h := map[string]string{
			"Authorization":   "Bearer " + credential.Token,
			"Content-Type":    "application/json",
			"Idempotency-Key": sum([]byte(method + path + string(payload))),
		}
		if revision != "" {
			h["If-Match"] = `"` + revision + `"`
		}
		status, _, data, e := do(method, path, payload, h)
		if e != nil {
			return nil, e
		}
		if status >= 300 {
			return nil, fmt.Errorf("%s %s: HTTP %d", method, path, status)
		}
		if len(data) == 0 {
			return nil, nil
		}
		var out map[string]any
		return out, json.Unmarshal(data, &out)
	}
	revision := func(m map[string]any) string {
		switch v := m["revision"].(type) {
		case float64:
			return fmt.Sprintf("%d", int64(v))
		case string:
			return v
		}
		return ""
	}
	registryRequest := func(method, path string, data []byte, extra map[string]string) (int, http.Header, error) {
		h := map[string]string{"Authorization": "Bearer " + credential.Token, "Content-Type": "application/octet-stream"}
		for k, v := range extra {
			h[k] = v
		}
		status, hdr, _, e := do(method, path, data, h)
		return status, hdr, e
	}

	feed, err := api("POST", "/api/v1/feeds", map[string]any{"slug": "images", "format": "oci"}, "")
	if err != nil {
		return err
	}
	source, err := api("POST", "/api/v1/sources", map[string]any{"name": "local-images", "kind": "local", "format": "oci"}, "")
	if err != nil {
		return err
	}
	feedID, sourceID := fmt.Sprint(feed["id"]), fmt.Sprint(source["id"])
	if _, err = api("PUT", "/api/v1/feeds/"+feedID+"/sources/"+sourceID, map[string]any{"priority": 0}, revision(feed)); err != nil {
		return err
	}
	if feed, err = api("GET", "/api/v1/feeds/"+feedID, nil, ""); err != nil {
		return err
	}
	if _, err = api("PUT", "/api/v1/feeds/"+feedID+"/publish-policy", map[string]any{"default_source_id": sourceID, "rules": []any{}}, revision(feed)); err != nil {
		return err
	}
	if _, err = crane(f.Env, []byte(credential.Token), "auth", "login", registry, "-u", "service", "--password-stdin"); err != nil {
		return err
	}

	var layerBuf bytes.Buffer
	tw := tar.NewWriter(&layerBuf)
	content := []byte("Kuvryn Ant real-client fixture\n")
	if err = tw.WriteHeader(&tar.Header{Name: "fixture.txt", Size: int64(len(content)), Mode: 0o644}); err != nil {
		return err
	}
	if _, err = tw.Write(content); err != nil {
		return err
	}
	if err = tw.Close(); err != nil {
		return err
	}
	layer := f.Path("layer.tar")
	if err = os.WriteFile(layer, layerBuf.Bytes(), 0o600); err != nil {
		return err
	}
	reference := registry + "/images/demo:smoke"
	mustCrane("append", "--oci-empty-base", "-f", layer, "-t", reference, "--insecure")
	if err != nil {
		return err
	}
	digest := strings.TrimSpace(string(mustCrane("digest", reference, "--insecure")))
	manifest := mustCrane("manifest", reference, "--insecure")
	if err != nil {
		return err
	}
	// crane manifest may append a newline for terminal output.
	if digest != "sha256:"+sum(manifest) && digest != "sha256:"+sum(bytes.TrimRight(manifest, "\n")) {
		return errors.New("manifest digest mismatch")
	}
	mustCrane("pull", reference, f.Path("image.tar"), "--insecure")
	if err != nil {
		return err
	}
	if n, e := tarEntries(f.Path("image.tar")); e != nil || n == 0 {
		return errors.New("empty pulled image")
	}

	// Leave a partially committed upload, restart the complete process, then
	// complete the session and read it with the real registry client.
	resumable := []byte("persisted OCI upload across process restart")
	status, hdr, e := registryRequest("POST", "/v2/images/demo/blobs/uploads/", nil, nil)
	if e != nil || status != 202 {
		return errors.New("upload initialization failed")
	}
	location := hdr.Get("Location")
	if status, _, e = registryRequest("PATCH", location, resumable[:11], map[string]string{"Content-Range": "0-10"}); e != nil || status != 202 {
		return errors.New("upload checkpoint failed")
	}
	server.Terminate(context.Background())
	if server, err = f.StartServer(port, logPath, false); err != nil {
		server = nil
		return err
	}
	status, hdr, e = registryRequest("GET", location, nil, nil)
	if e != nil || status != 204 || hdr.Get("Range") != "0-10" {
		return errors.New("upload checkpoint did not survive restart")
	}
	resumedDigest := "sha256:" + sum(resumable)
	status, _, e = registryRequest("PUT", location+"?digest="+resumedDigest, resumable[11:],
		map[string]string{"Content-Range": fmt.Sprintf("11-%d", len(resumable)-1)})
	if e != nil || status != 201 {
		return errors.New("resumed upload did not finalize")
	}
	if blob := mustCrane("blob", registry+"/images/demo@"+resumedDigest, "--insecure"); err != nil || !bytes.Equal(blob, resumable) {
		return errors.New("resumed blob bytes changed")
	}
	if strings.TrimSpace(string(mustCrane("digest", reference, "--insecure"))) != digest {
		return errors.New("published manifest did not survive restart")
	}
	mustCrane("pull", reference, f.Path("after-restart.tar"), "--insecure")
	if err != nil {
		return err
	}

	amd64, arm64, multi := registry+"/images/demo:amd64", registry+"/images/demo:arm64", registry+"/images/demo:multi"
	mustCrane("mutate", reference, "--set-platform", "linux/amd64", "-t", amd64, "--insecure")
	mustCrane("mutate", reference, "--set-platform", "linux/arm64", "-t", arm64, "--insecure")
	mustCrane("index", "append", "-m", amd64, "-m", arm64, "-t", multi, "--insecure")
	indexRaw := mustCrane("manifest", multi, "--insecure")
	if err != nil {
		return err
	}
	var index struct {
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err = json.Unmarshal(indexRaw, &index); err != nil {
		return err
	}
	platforms := map[string]bool{}
	for _, m := range index.Manifests {
		platforms[m.Platform.OS+"/"+m.Platform.Architecture] = true
	}
	if len(platforms) != 2 || !platforms["linux/amd64"] || !platforms["linux/arm64"] {
		return errors.New("multi-platform index descriptors differ")
	}
	mustCrane("pull", multi, f.Path("arm64.tar"), "--platform", "linux/arm64", "--insecure")
	if err != nil {
		return err
	}
	if err = checkPlatform(f.Path("arm64.tar")); err != nil {
		return err
	}

	// Separate disposable client credentials prove a rejected token does not
	// fall back to the valid login. No tool output containing auth is printed.
	badConfig := f.Path("bad-docker")
	if err = os.Mkdir(badConfig, 0o700); err != nil {
		return err
	}
	cfg, _ := json.Marshal(map[string]any{"auths": map[string]any{registry: map[string]string{
		"auth": base64.StdEncoding.EncodeToString([]byte("service:invalid-token"))}}})
	if err = os.WriteFile(filepath.Join(badConfig, "config.json"), cfg, 0o600); err != nil {
		return err
	}
	badEnv := localfixture.WithEnv(f.Env, "DOCKER_CONFIG", badConfig)
	if _, e = crane(badEnv, nil, "digest", reference, "--insecure"); e == nil {
		return errors.New("crane unexpectedly succeeded with an invalid token")
	}
	for _, token := range []string{"", "invalid-token"} {
		h := map[string]string{}
		if token != "" {
			h["Authorization"] = "Bearer " + token
		}
		status, _, data, e := do("GET", "/v2/", nil, h)
		if e != nil {
			return e
		}
		if status != 401 {
			return errors.New("registry accepted missing/invalid credentials or gave unexpected rejection")
		}
		var body struct {
			Errors []struct {
				Code string `json:"code"`
			} `json:"errors"`
		}
		if json.Unmarshal(data, &body) != nil || len(body.Errors) == 0 || body.Errors[0].Code != "UNAUTHORIZED" {
			return errors.New("registry auth rejection had wrong envelope")
		}
	}

	version, e := crane(f.Env, nil, "version")
	if e != nil {
		return e
	}
	fmt.Println("Client:", strings.TrimSpace(string(version)))
	fmt.Println("PASS: API setup, authenticated push/pull, durable upload restart, multi-platform index and rejected credentials")
	return nil
}

func tarEntries(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	n, tr := 0, tar.NewReader(file)
	for {
		if _, err = tr.Next(); err == io.EOF {
			return n, nil
		} else if err != nil {
			return n, err
		}
		n++
	}
}

func tarFile(path, name string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	tr := tar.NewReader(file)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("%s not found in archive", name)
		}
		if h.Name == name {
			return io.ReadAll(tr)
		}
	}
}

func checkPlatform(path string) error {
	raw, err := tarFile(path, "manifest.json")
	if err != nil {
		return err
	}
	var catalog []struct{ Config string }
	if err = json.Unmarshal(raw, &catalog); err != nil || len(catalog) == 0 {
		return errors.New("pulled archive has no manifest catalog")
	}
	raw, err = tarFile(path, catalog[0].Config)
	if err != nil {
		return err
	}
	var config struct{ Architecture, OS string }
	if err = json.Unmarshal(raw, &config); err != nil || config.Architecture != "arm64" || config.OS != "linux" {
		return errors.New("platform-selected pull used wrong image")
	}
	return nil
}
