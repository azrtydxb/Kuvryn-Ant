package oci

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func basic(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// TestCraneIndexCopyAndMount drives the real crane client (when installed)
// through a ServeMux-mounted registry: image pushes, an index referencing
// several child manifests, a cross-repository copy (blob mount attempts) and a
// pull back. It is the regression for multi-platform index pushes.
func TestCraneIndexCopyAndMount(t *testing.T) {
	crane, err := exec.LookPath("crane")
	if err != nil {
		t.Skip("crane not installed")
	}
	f := setup(t)
	mux := http.NewServeMux()
	mux.Handle("/v2/", f.registry)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(crane, append(args, "--insecure")...)
		cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+dir)
		cmd.Dir = dir
		done := make(chan struct{})
		var out []byte
		var cerr error
		go func() { out, cerr = cmd.CombinedOutput(); close(done) }()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("crane %v hung", args)
		}
		if cerr != nil {
			t.Fatalf("crane %v: %v\n%s", args, cerr, out)
		}
		return string(out)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"`+host+`":{"auth":"`+basic("service", f.token)+`"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var children []string
	for i, platform := range []string{"linux/amd64", "linux/arm64", "linux/s390x"} {
		layer := filepath.Join(dir, "layer"+string(rune('a'+i))+".tar")
		if err := os.WriteFile(layer, []byte(strings.Repeat(platform, 100)), 0600); err != nil {
			t.Fatal(err)
		}
		ref := host + "/images/team/app:" + string(rune('a'+i))
		run("append", "-t", ref, "-f", layer, "--platform", platform)
		children = append(children, ref)
	}
	args := []string{"index", "append", "-t", host + "/images/team/app:multi"}
	for _, c := range children {
		args = append(args, "-m", c)
	}
	run(args...)
	// Same-registry copy of the index into another repository mounts blobs.
	run("copy", host+"/images/team/app:multi", host+"/images/other/nested/deep/app:multi")
	if out := run("manifest", host+"/images/other/nested/deep/app:multi"); strings.Count(out, "sha256:") < 3 {
		t.Fatalf("index not copied: %s", out)
	}
}
