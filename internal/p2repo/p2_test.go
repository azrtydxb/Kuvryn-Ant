package p2repo

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
	"github.com/ulikunitz/xz"
)

const testBase = "/feeds/packages/p2/"

// repoZip builds a Tycho-like repository zip. compress selects content.jar+artifacts.xml.xz.
func repoZip(t *testing.T, jars map[string][]byte, units []string, tamper, compress, missingRef bool) []byte {
	t.Helper()
	var ids []string
	for id := range jars {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var art, content strings.Builder
	art.WriteString("<?xml version='1.0'?><repository name='x'><artifacts size='1'>")
	for _, id := range ids {
		s, m := hashes(jars[id])
		if tamper {
			s = strings.Repeat("0", 64)
		}
		art.WriteString("<artifact classifier='osgi.bundle' id='" + id + "' version='1.0.0'><properties size='3'><property name='download.size' value='" + strconv.Itoa(len(jars[id])) + "'/><property name='download.md5' value='" + m + "'/><property name='download.checksum.sha-256' value='" + s + "'/></properties></artifact>")
	}
	art.WriteString("</artifacts></repository>")
	content.WriteString("<?xml version='1.0'?><repository name='x'><units size='1'>")
	for _, id := range ids {
		ref := id
		if missingRef {
			ref = "ghost"
		}
		content.WriteString("<unit id='" + id + "' version='1.0.0'><provides size='1'><provided namespace='osgi.bundle' name='" + id + "' version='1.0.0'/></provides><artifacts size='1'><artifact classifier='osgi.bundle' id='" + ref + "' version='1.0.0'/></artifacts></unit>")
	}
	for _, u := range units {
		content.WriteString(u)
	}
	content.WriteString("</units></repository>")
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	put := func(name string, b []byte) {
		w, _ := z.Create(name)
		_, _ = w.Write(b)
	}
	if compress {
		put("content.jar", jarOf("content.xml", []byte(content.String())))
		put("artifacts.xml.xz", xzOf([]byte(art.String())))
	} else {
		put("content.xml", []byte(content.String()))
		put("artifacts.xml", []byte(art.String()))
	}
	for _, id := range ids {
		put("plugins/"+id+"_1.0.0.jar", jars[id])
	}
	_ = z.Close()
	return out.Bytes()
}

func TestPostgres(t *testing.T) {
	f := setup(t)
	jars := map[string][]byte{"com.a": []byte("PK-signed-bytes-a"), "com.b": []byte("PK-signed-bytes-b")}
	up := testBase + "repository.zip"
	// Unauthenticated, checksum tamper, missing closure, garbage.
	w := httptest.NewRecorder()
	f.registry.ServeHTTP(w, httptest.NewRequest("GET", testBase+"p2.index", nil))
	assertCode(t, w, 401)
	assertCode(t, f.request("PUT", up, repoZip(t, jars, nil, true, false, false), nil), 400)
	assertCode(t, f.request("PUT", up, repoZip(t, jars, nil, false, false, true), nil), 400)
	assertCode(t, f.request("PUT", up, []byte("not a zip"), nil), 400)
	w = f.request("GET", testBase+"content.xml", nil, nil)
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), "com.a") {
		t.Fatal("failed upload visible")
	}
	assertCode(t, f.request("GET", testBase+"plugins/com.a_1.0.0.jar", nil, nil), 404)
	good := repoZip(t, jars, nil, false, false, false)
	assertCode(t, f.request("PUT", up, good, nil), 201)
	assertCode(t, f.request("PUT", up, good, nil), 201)
	for _, file := range []string{"content.xml", "content.jar", "content.xml.xz"} {
		w = f.request("GET", testBase+file, nil, nil)
		assertCode(t, w, 200)
		body := w.Body.Bytes()
		switch file {
		case "content.jar":
			zr, e := zip.NewReader(bytes.NewReader(body), int64(len(body)))
			if e != nil {
				t.Fatal(e)
			}
			rc, _ := zr.File[0].Open()
			body, _ = io.ReadAll(rc)
		case "content.xml.xz":
			xr, e := xz.NewReader(bytes.NewReader(body))
			if e != nil {
				t.Fatal(e)
			}
			body, _ = io.ReadAll(xr)
		}
		if !strings.Contains(string(body), "<unit id='com.a' version='1.0.0'>") || !strings.Contains(string(body), "<units size='2'>") {
			t.Fatal(file, string(body))
		}
	}
	w = f.request("GET", testBase+"artifacts.xml", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "download.checksum.sha-256") || !strings.Contains(w.Body.String(), "plugins/${id}_${version}.jar") {
		t.Fatal(w.Body.String())
	}
	a1 := f.request("GET", testBase+"artifacts.jar", nil, nil).Body.Bytes()
	a2 := f.request("GET", testBase+"artifacts.jar", nil, nil).Body.Bytes()
	if len(a1) == 0 || !bytes.Equal(a1, a2) {
		t.Fatal("non-deterministic")
	}
	assertCode(t, f.request("GET", testBase+"p2.index", nil, nil), 200)
	w = f.request("GET", testBase+"plugins/com.a_1.0.0.jar", nil, nil)
	assertCode(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), jars["com.a"]) {
		t.Fatal("jar changed")
	}
	assertCode(t, f.request("HEAD", testBase+"plugins/com.a_1.0.0.jar", nil, nil), 200)
	assertCode(t, f.request("GET", testBase+"plugins/com.zzz_1.0.0.jar", nil, nil), 404)
	// Unsigned substitution: same id/version, different (self-consistent) bytes conflicts.
	sub := map[string][]byte{"com.a": []byte("PK-substituted")}
	assertCode(t, f.request("PUT", up, repoZip(t, sub, nil, false, true, false), nil), 409)
	// Second upload merges; jar/xz metadata forms work; IU referencing an earlier artifact is closed.
	extra := "<unit id='com.feature.group' version='2.0.0'><artifacts size='1'><artifact classifier='osgi.bundle' id='com.a' version='1.0.0'/></artifacts></unit>"
	more := map[string][]byte{"com.c": []byte("PK-c")}
	assertCode(t, f.request("PUT", up, repoZip(t, more, []string{extra}, false, true, false), nil), 201)
	w = f.request("GET", testBase+"content.xml", nil, nil)
	if !strings.Contains(w.Body.String(), "<units size='4'>") || !strings.Contains(w.Body.String(), "com.feature.group") {
		t.Fatal(w.Body.String())
	}
	diff := "<unit id='com.feature.group' version='2.0.0'><properties size='0'/></unit>"
	assertCode(t, f.request("PUT", up, repoZip(t, more, []string{diff}, false, true, false), nil), 409)
	// Read-only token cannot publish.
	ro, err := (identity.Tokens{DB: f.pool}).Issue(context.Background(), f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PUT", up, bytes.NewReader(good))
	req.SetBasicAuth("x", ro.Secret)
	w = httptest.NewRecorder()
	f.registry.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
