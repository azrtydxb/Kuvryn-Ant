package rawrepo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func TestWebDAVSubset(t *testing.T) {
	f := newPublishFixture(t)
	if w := f.request("OPTIONS", "", "", nil); w.Code != 204 || !strings.Contains(w.Header().Get("Allow"), "PROPFIND") || w.Header().Get("DAV") == "" {
		t.Fatalf("options %d %v", w.Code, w.Header())
	}
	if w := f.request("PROPFIND", "", "", map[string]string{"Depth": "1"}); w.Code != 207 || !strings.Contains(w.Body.String(), "<D:collection/>") {
		t.Fatalf("root %d %s", w.Code, w.Body)
	}
	if w := f.request("PROPFIND", "a/", "", map[string]string{"Depth": "0"}); w.Code != 404 {
		t.Fatalf("missing collection %d", w.Code)
	}
	if w := f.request("MKCOL", "a/", "", nil); w.Code != 201 {
		t.Fatalf("mkcol %d %s", w.Code, w.Body)
	}
	if w := f.request("MKCOL", "a/", "", nil); w.Code != 201 {
		t.Fatalf("mkcol repeat %d", w.Code)
	}
	if w := f.request("PUT", "a/b/key", "payload", nil); w.Code != 201 {
		t.Fatalf("put %d", w.Code)
	}
	for _, dir := range []string{"a/", "a/b/", "a"} {
		if w := f.request("PROPFIND", dir, "", nil); w.Code != 207 || !strings.Contains(w.Body.String(), "<D:collection/>") {
			t.Fatalf("collection %q %d %s", dir, w.Code, w.Body)
		}
	}
	if w := f.request("PROPFIND", "a/b/key", "", nil); w.Code != 207 || !strings.Contains(w.Body.String(), "<D:getcontentlength>7</D:getcontentlength>") || strings.Contains(w.Body.String(), "collection") {
		t.Fatalf("file %d %s", w.Code, w.Body)
	}
	if w := f.request("PROPFIND", "a/b/nope", "", nil); w.Code != 404 {
		t.Fatalf("missing file %d", w.Code)
	}
	if w := f.request("PROPFIND", "a/bb/", "", nil); w.Code != 404 {
		t.Fatalf("prefix is not a collection: %d", w.Code)
	}
	if w := f.request("DELETE", "a/b/key", "", nil); w.Code != 403 { // token lacks the delete action
		t.Fatalf("delete %d", w.Code)
	}
	// Basic authentication carries the token as password (WebDAV clients).
	if w := f.request("GET", "a/b/key", "", map[string]string{"Authorization": "Basic " + basic("u", f.secret)}); w.Code != 200 || w.Body.String() != "payload" {
		t.Fatalf("basic %d", w.Code)
	}
	// MKCOL needs publish rights; PROPFIND needs read rights.
	for _, tc := range []struct {
		action       identity.Action
		method, path string
		status       int
	}{{identity.Read, "MKCOL", "z/", 403}, {identity.Publish, "PROPFIND", "a/", 404}} {
		token, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.principal.InstanceID, f.principal.PrincipalID, f.principal.PrincipalID, []identity.Scope{{Actions: []identity.Action{tc.action}}}, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if w := f.request(tc.method, tc.path, "", map[string]string{"Authorization": "Bearer " + token.Secret}); w.Code != tc.status {
			t.Fatalf("%s as %v: %d", tc.method, tc.action, w.Code)
		}
	}
	if w := f.request("PROPFIND", "a/", "", map[string]string{"Authorization": ""}); w.Code != 401 {
		t.Fatalf("anonymous %d", w.Code)
	}
}

func basic(user, password string) string {
	return base64Std(user + ":" + password)
}

// TestSccacheWebDAV drives the real sccache client (WebDAV backend) at a raw
// feed: a cold compile writes cache entries, a second one hits them.
func TestSccacheWebDAV(t *testing.T) {
	sccache, err := exec.LookPath("sccache")
	if err != nil {
		t.Skip("sccache not installed")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler")
	}
	f := newPublishFixture(t)
	srv := httptest.NewServer(f.repo)
	defer srv.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "x.c"), []byte("int answer(void){return 42;}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"SCCACHE_WEBDAV_ENDPOINT="+srv.URL+"/feeds/artifacts/raw",
		"SCCACHE_WEBDAV_KEY_PREFIX=/ci",
		"SCCACHE_WEBDAV_TOKEN="+f.secret,
		"SCCACHE_SERVER_PORT="+strconv.Itoa(port),
		"SCCACHE_DIR="+filepath.Join(dir, "local"),
		"SCCACHE_CONF="+filepath.Join(dir, "none.toml"),
		"SCCACHE_ERROR_LOG="+filepath.Join(dir, "sccache.log"),
		"SCCACHE_LOG=debug",
	)
	run := func(args ...string) string {
		cmd := exec.Command(sccache, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			log, _ := os.ReadFile(filepath.Join(dir, "sccache.log"))
			t.Fatalf("sccache %v: %v\n%s\n%s", args, err, out, tail(log))
		}
		return string(out)
	}
	_ = exec.Command(sccache, "--stop-server").Run()
	run("--start-server")
	defer func() {
		cmd := exec.Command(sccache, "--stop-server")
		cmd.Env = env
		_ = cmd.Run()
	}()
	run(cc, "-c", "x.c", "-o", "x.o")
	_ = os.Remove(filepath.Join(dir, "x.o"))
	run(cc, "-c", "x.c", "-o", "x.o")
	var stats struct {
		Stats struct {
			CacheHits struct {
				Counts map[string]int `json:"counts"`
			} `json:"cache_hits"`
			CacheWrites int `json:"cache_writes"`
			WriteErrors int `json:"cache_write_errors"`
			ReadErrors  int `json:"cache_read_errors"`
		} `json:"stats"`
	}
	raw := run("--show-stats", "--stats-format", "json")
	if err = json.Unmarshal([]byte(raw), &stats); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("DEBUG_SCCACHE") != "" {
		log, _ := os.ReadFile(filepath.Join(dir, "sccache.log"))
		t.Log(tail(log))
	}
	hits := 0
	for _, n := range stats.Stats.CacheHits.Counts {
		hits += n
	}
	if hits < 1 || stats.Stats.CacheWrites < 1 || stats.Stats.WriteErrors != 0 || stats.Stats.ReadErrors != 0 {
		log, _ := os.ReadFile(filepath.Join(dir, "sccache.log"))
		t.Fatalf("hits=%d writes=%d write_errors=%d read_errors=%d\n%s", hits, stats.Stats.CacheWrites, stats.Stats.WriteErrors, stats.Stats.ReadErrors, tail(log))
	}
}

func tail(b []byte) string {
	if len(b) > 4000 {
		b = b[len(b)-4000:]
	}
	return string(b)
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// An sccache/OpenDAL client creates every parent collection (MKCOL, one level
// at a time, nested paths) before a PUT, then reads with GET, HEAD and PROPFIND.
// The same sequence must work on a cache feed, where the entries are also
// evictable: after eviction the key can be written again.
func TestWebDAVNestedMKCOLOnCacheFeed(t *testing.T) {
	f := newPublishFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE feeds SET purpose='cache'`); err != nil {
		t.Fatal(err)
	}
	// sccache key layout: <prefix>/<h0>/<h1>/<h2>/<hash>, prefix itself nested.
	const key = "ci/repo/main/a/b/c/abcdef0123456789"
	dirs := []string{"ci/", "ci/repo/", "ci/repo/main/", "ci/repo/main/a/", "ci/repo/main/a/b/", "ci/repo/main/a/b/c/"}
	for _, dir := range dirs {
		if w := f.request("MKCOL", dir, "", nil); w.Code != 201 {
			t.Fatalf("MKCOL %s: %d %s", dir, w.Code, w.Body)
		}
	}
	// MKCOL of a nested collection whose parents were never created is accepted
	// as well: directories are virtual and appear with the first file.
	if w := f.request("MKCOL", "ci/repo/other/x/y/", "", nil); w.Code != 201 {
		t.Fatalf("MKCOL of a deep new path: %d", w.Code)
	}
	if w := f.request("PUT", key, "object bytes", nil); w.Code != 201 {
		t.Fatalf("PUT %d %s", w.Code, w.Body)
	}
	if w := f.request("GET", key, "", nil); w.Code != 200 || w.Body.String() != "object bytes" {
		t.Fatalf("GET %d %q", w.Code, w.Body)
	}
	if w := f.request("HEAD", key, "", nil); w.Code != 200 || w.Header().Get("Content-Length") != "12" {
		t.Fatalf("HEAD %d %v", w.Code, w.Header())
	}
	for _, dir := range dirs {
		if w := f.request("PROPFIND", dir, "", map[string]string{"Depth": "0"}); w.Code != 207 || !strings.Contains(w.Body.String(), "<D:collection/>") {
			t.Fatalf("PROPFIND %s: %d %s", dir, w.Code, w.Body)
		}
	}
	if w := f.request("PROPFIND", key, "", map[string]string{"Depth": "0"}); w.Code != 207 || !strings.Contains(w.Body.String(), "<D:getlastmodified>") {
		t.Fatalf("PROPFIND file: %d %s", w.Code, w.Body)
	}
	// Writing the same bytes again is accepted, different bytes are a conflict
	// (sccache logs it and carries on).
	if w := f.request("PUT", key, "object bytes", nil); w.Code != 201 && w.Code != 200 && w.Code != 204 {
		t.Fatalf("identical re-PUT: %d", w.Code)
	}
	if w := f.request("PUT", key, "different", nil); w.Code != 409 {
		t.Fatalf("different re-PUT: %d, want 409", w.Code)
	}
	// Evicted (deleted by retention or cleanup): the key is gone and writable again.
	for _, q := range []string{
		`DELETE FROM assets WHERE version_id IN (SELECT v.id FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate='` + key + `')`,
		`DELETE FROM versions WHERE component_id IN (SELECT id FROM components WHERE coordinate='` + key + `')`,
		`DELETE FROM components WHERE coordinate='` + key + `'`,
	} {
		if _, err := f.pool.Exec(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.request("GET", key, "", nil); w.Code != 404 {
		t.Fatalf("GET after eviction: %d", w.Code)
	}
	if w := f.request("PUT", key, "different", nil); w.Code != 201 {
		t.Fatalf("PUT after eviction: %d", w.Code)
	}
}
