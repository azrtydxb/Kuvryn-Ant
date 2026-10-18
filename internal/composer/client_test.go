package composer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPostgresComposerClient(t *testing.T) {
	client, e := exec.LookPath("composer")
	if e != nil {
		t.Skip("Composer unavailable")
	}
	f := setup(t)
	assertCode(t, f.push(t, packageBytes(t, "acme/widget", "1.2.0", "")), 201)
	server := httptest.NewServer(f.registry)
	defer server.Close()
	if e = f.registry.SetPublicURL(server.URL); e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	config := map[string]any{"name": "fixture/root", "repositories": []any{map[string]any{"type": "composer", "url": server.URL + testBase}, map[string]any{"packagist.org": false}}, "require": map[string]string{"acme/widget": "1.2.0"}, "config": map[string]any{"secure-http": false, "allow-plugins": false, "audit": map[string]any{"block-insecure": false}}}
	data, _ := json.Marshal(config)
	if e = os.WriteFile(filepath.Join(dir, "composer.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	auth, _ := json.Marshal(map[string]any{"http-basic": map[string]any{strings.TrimPrefix(server.URL, "http://"): map[string]string{"username": "fixture", "password": f.token}}})
	// Credentials stay in a private temporary file, never command arguments/output.
	if e = os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0600); e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, client, args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "COMPOSER_HOME=" + filepath.Join(dir, "home"), "COMPOSER_CACHE_DIR=" + filepath.Join(dir, "cache"), "COMPOSER_NO_INTERACTION=1"}
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("Composer %s: %v %s", args[0], e, strings.ReplaceAll(string(out), f.token, "[redacted]"))
		}
	}
	version, _ := exec.Command(client, "--version").Output()
	binary, e := os.ReadFile(client)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(binary)
	t.Logf("client %s sha256=%s", strings.TrimSpace(string(version)), hex.EncodeToString(sum[:]))
	run("update", "--no-plugins", "--no-scripts", "--no-audit", "--prefer-dist")
	if _, e = os.Stat(filepath.Join(dir, "vendor", "acme", "widget", "src", "Fixture.php")); e != nil {
		t.Fatal(e)
	}
	// Force a second archive retrieval against the locked metadata, without cache.
	if e = os.RemoveAll(filepath.Join(dir, "vendor")); e != nil {
		t.Fatal(e)
	}
	if e = os.RemoveAll(filepath.Join(dir, "cache")); e != nil {
		t.Fatal(e)
	}
	run("install", "--no-plugins", "--no-scripts", "--prefer-dist")
}
