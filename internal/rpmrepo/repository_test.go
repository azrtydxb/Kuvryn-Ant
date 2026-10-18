package rpmrepo

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

func (f fixture) requestAs(token, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	if token != "" {
		req.SetBasicAuth("service", token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	f.registry.ServeHTTP(response, req)
	return response
}
func (f fixture) request(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	return f.requestAs(f.token, method, path, body, headers)
}
func assertCode(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status %d expected %d: %s", response.Code, status, response.Body.String())
	}
}

const base = "/feeds/packages/rpm/"

type repomdData struct {
	Type     string `xml:"type,attr"`
	Checksum struct {
		Value string `xml:",chardata"`
	} `xml:"checksum"`
	Open struct {
		Value string `xml:",chardata"`
	} `xml:"open-checksum"`
	Location struct {
		Href string `xml:"href,attr"`
	} `xml:"location"`
	Size     int64 `xml:"size"`
	OpenSize int64 `xml:"open-size"`
}
type repomdDoc struct {
	Data []repomdData `xml:"data"`
}

// snapshot fetches and fully verifies one repomd generation, returning the
// decompressed indexes keyed by type.
func (f fixture) snapshot(t *testing.T) (map[string]string, []byte) {
	t.Helper()
	w := f.request("GET", base+"repodata/repomd.xml", nil, nil)
	assertCode(t, w, 200)
	repomd := w.Body.Bytes()
	var doc repomdDoc
	if err := xml.Unmarshal(repomd, &doc); err != nil || len(doc.Data) != 3 {
		t.Fatal(err, string(repomd))
	}
	out := map[string]string{}
	for _, d := range doc.Data {
		w := f.request("GET", base+d.Location.Href, nil, nil)
		assertCode(t, w, 200)
		sum := sha256.Sum256(w.Body.Bytes())
		if hex.EncodeToString(sum[:]) != d.Checksum.Value || int64(w.Body.Len()) != d.Size || d.Location.Href != "repodata/"+d.Checksum.Value+"-"+d.Type+".xml.gz" {
			t.Fatalf("%s checksum/size/location mismatch", d.Type)
		}
		z, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(z)
		if err != nil {
			t.Fatal(err)
		}
		sum = sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != d.Open.Value || int64(len(raw)) != d.OpenSize {
			t.Fatalf("%s open checksum mismatch", d.Type)
		}
		if err = xml.Unmarshal(raw, new(struct{ XMLName xml.Name })); err != nil {
			t.Fatal(d.Type, err)
		}
		out[d.Type] = string(raw)
	}
	return out, repomd
}

func mustParse(t *testing.T, b []byte) *Package {
	t.Helper()
	p, err := parseBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPostgres(t *testing.T) {
	f := setup(t, true)
	lib := buildRPM(spec{name: "libdemo", version: "2.0", release: "1", files: []string{"/usr/lib64/libdemo.so.2"}, provides: [][3]string{{"libdemo.so.2()(64bit)", "", ""}}})
	epoch := uint32(1)
	app := buildRPM(spec{name: "demo-app", version: "1.0", release: "3.el9", epoch: &epoch, files: []string{"/usr/bin/demo", "/usr/share/demo/data"}, requires: [][3]string{{"libdemo", "GE", "2.0"}, {"libdemo.so.2()(64bit)", "", ""}}})
	libName, appName := "Packages/libdemo-2.0-1.x86_64.rpm", "Packages/demo-app-1.0-3.el9.x86_64.rpm"

	// No repository content yet.
	assertCode(t, f.request("GET", base+"repodata/repomd.xml", nil, nil), 404)

	assertCode(t, f.request("PUT", base+libName, lib, nil), 201)
	assertCode(t, f.request("PUT", base+appName, app, nil), 201)
	assertCode(t, f.request("PUT", base+appName, app, nil), 201) // identical retry
	// Same NEVRA, different bytes: immutable conflict, repository unchanged.
	changed := buildRPM(spec{name: "demo-app", version: "1.0", release: "3.el9", epoch: &epoch, payload: "other"})
	assertCode(t, f.request("PUT", base+appName, changed, nil), 409)

	idx, repomd1 := f.snapshot(t)
	if !strings.Contains(idx["primary"], `packages="2"`) || !strings.Contains(idx["primary"], "<name>demo-app</name>") || !strings.Contains(idx["primary"], `<rpm:entry name="libdemo" flags="GE" epoch="0" ver="2.0"/>`) || !strings.Contains(idx["primary"], `epoch="1" ver="1.0" rel="3.el9"`) || !strings.Contains(idx["primary"], `href="`+appName+`"`) {
		t.Fatal(idx["primary"])
	}
	if !strings.Contains(idx["filelists"], "<file>/usr/share/demo/data</file>") || !strings.Contains(idx["other"], "- initial") {
		t.Fatal(idx["filelists"], idx["other"])
	}
	sum := sha256.Sum256(app)
	if !strings.Contains(idx["primary"], hex.EncodeToString(sum[:])) {
		t.Fatal("package checksum missing")
	}

	// Signature verifies against the published key, and only that key.
	key := f.request("GET", base+"repodata/repomd.xml.key", nil, nil)
	assertCode(t, key, 200)
	ring, err := openpgp.ReadArmoredKeyRing(key.Body)
	if err != nil || len(ring) != 1 || ring[0].PrivateKey != nil {
		t.Fatal(err, "public key must not carry private material")
	}
	sig := f.request("GET", base+"repodata/repomd.xml.asc", nil, nil)
	assertCode(t, sig, 200)
	if _, err = openpgp.CheckArmoredDetachedSignature(ring, bytes.NewReader(repomd1), bytes.NewReader(sig.Body.Bytes()), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = openpgp.CheckArmoredDetachedSignature(ring, bytes.NewReader(append(repomd1[:len(repomd1):len(repomd1)], ' ')), bytes.NewReader(sig.Body.Bytes()), nil); err == nil {
		t.Fatal("tampered repomd verified")
	}
	// The private key lives only as an encrypted secret.
	var raw string
	if err = f.pool.QueryRow(context.Background(), `SELECT envelope::text FROM encrypted_secrets WHERE purpose LIKE 'rpm.repomd-signing-key:%'`).Scan(&raw); err != nil || strings.Contains(raw, "PGP PRIVATE") {
		t.Fatal(err)
	}

	// Downloads are byte-exact; unknown or mismatched paths are not found.
	w := f.request("GET", base+appName, nil, nil)
	assertCode(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), app) || w.Header().Get("Content-Type") != "application/x-rpm" {
		t.Fatal("package bytes changed")
	}
	assertCode(t, f.request("HEAD", base+appName, nil, nil), 200)
	assertCode(t, f.request("GET", base+"Packages/demo-app-9.9-1.x86_64.rpm", nil, nil), 404)
	assertCode(t, f.request("GET", base+"Packages/not-an-rpm", nil, nil), 404)
	assertCode(t, f.request("GET", base+"repodata/"+strings.Repeat("0", 64)+"-primary.xml.gz", nil, nil), 404)
	assertCode(t, f.request("GET", base+"repodata/../Packages/x", nil, nil), 404)
	assertCode(t, f.request("GET", "/feeds/packages/rpm/repodata/%2e%2e/x", nil, nil), 404)
	assertCode(t, f.request("GET", "/feeds/missing/rpm/repodata/repomd.xml", nil, nil), 404)

	// Malformed and tampered uploads are rejected and leave the generation alone.
	hdrEnd := int(mustParse(t, app).HeaderEnd)
	flip := func(b []byte, i int) []byte { c := append([]byte(nil), b...); c[i] ^= 1; return c }
	newPkg := buildRPM(spec{name: "evil", version: "1", release: "1"})
	newHdrEnd := int(mustParse(t, newPkg).HeaderEnd)
	_ = hdrEnd
	type upload struct {
		path string
		body []byte
		hdr  map[string]string
	}
	for name, c := range map[string]upload{
		"garbage":           {"Packages/evil-1-1.x86_64.rpm", []byte("not an rpm at all, not even close, really not an rpm"), nil},
		"truncated":         {"Packages/evil-1-1.x86_64.rpm", newPkg[:len(newPkg)-5], nil},
		"payload tampered":  {"Packages/evil-1-1.x86_64.rpm", flip(newPkg, len(newPkg)-1), nil},
		"header tampered":   {"Packages/evil-1-1.x86_64.rpm", flip(newPkg, newHdrEnd-3), nil},
		"name mismatch":     {"Packages/other-1-1.x86_64.rpm", newPkg, nil},
		"version mismatch":  {"Packages/evil-2-1.x86_64.rpm", newPkg, nil},
		"arch mismatch":     {"Packages/evil-1-1.aarch64.rpm", newPkg, nil},
		"checksum mismatch": {"Packages/evil-1-1.x86_64.rpm", newPkg, map[string]string{"X-Checksum-Sha256": strings.Repeat("0", 64)}},
		"bad filename":      {"Packages/evil.rpm", newPkg, nil},
	} {
		if w := f.request("PUT", base+c.path, c.body, c.hdr); w.Code != 400 && w.Code != 404 {
			t.Errorf("%s: status %d", name, w.Code)
		}
	}
	if _, repomd := f.snapshot(t); !bytes.Equal(repomd, repomd1) {
		t.Fatal("rejected uploads changed the repository generation")
	}
	assertCode(t, f.request("GET", base+"Packages/evil-1-1.x86_64.rpm", nil, nil), 404)

	// Authorization: read-only and anonymous callers cannot write or delete.
	ctx := context.Background()
	ro, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.requestAs(ro.Secret, "GET", base+"repodata/repomd.xml", nil, nil), 200)
	assertCode(t, f.requestAs(ro.Secret, "PUT", base+"Packages/evil-1-1.x86_64.rpm", newPkg, nil), 403)
	assertCode(t, f.requestAs(ro.Secret, "DELETE", base+libName, nil, nil), 403)
	assertCode(t, f.requestAs("", "GET", base+"repodata/repomd.xml", nil, nil), 401)
	assertCode(t, f.requestAs("not-a-token", "GET", base+"repodata/repomd.xml", nil, nil), 401)
	assertCode(t, f.request("POST", base+libName, nil, nil), 405)

	// A client mid-walk of generation 1 can still fetch its files after a new
	// generation is published; repomd itself always moves forward together.
	var doc1 repomdDoc
	_ = xml.Unmarshal(repomd1, &doc1)
	assertCode(t, f.request("PUT", base+"Packages/extra-1-1.noarch.rpm", buildRPM(spec{name: "extra", version: "1", release: "1", arch: "noarch"}), nil), 201)
	idx2, repomd2 := f.snapshot(t)
	if bytes.Equal(repomd1, repomd2) || !strings.Contains(idx2["primary"], `packages="3"`) {
		t.Fatal("new generation not published")
	}
	assertCode(t, f.request("GET", base+doc1.Data[0].Location.Href, nil, nil), 200)

	// Delete is audited, publishes a generation without the package first, and
	// is reflected by downloads.
	assertCode(t, f.request("DELETE", base+"Packages/extra-1-1.noarch.rpm", nil, nil), 204)
	assertCode(t, f.request("DELETE", base+"Packages/extra-1-1.noarch.rpm", nil, nil), 404)
	assertCode(t, f.request("GET", base+"Packages/extra-1-1.noarch.rpm", nil, nil), 404)
	idx3, _ := f.snapshot(t)
	if strings.Contains(idx3["primary"], "<name>extra</name>") || !strings.Contains(idx3["primary"], `packages="2"`) {
		t.Fatal(idx3["primary"])
	}
	var audits int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action='artifact.delete'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatal(err, audits)
	}
	// The name can be reused after deletion, with different content.
	assertCode(t, f.request("PUT", base+"Packages/extra-1-1.noarch.rpm", buildRPM(spec{name: "extra", version: "1", release: "1", arch: "noarch", payload: "v2"}), nil), 201)

	// Generations are numbered strictly ascending.
	var first string
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT min(v.version),count(*) FROM versions v JOIN components c ON c.id=v.component_id WHERE c.coordinate=$1`, snapshotCoordinate).Scan(&first, &count); err != nil || count < 4 || !strings.HasPrefix(first, "g000000000001-") {
		t.Fatal(err, first, count)
	}

	// Concurrent publishes converge on one generation listing every package.
	var wg sync.WaitGroup
	codes := make(chan int, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "race" + string(rune('a'+i))
			codes <- f.request("PUT", base+"Packages/"+name+"-1-1.x86_64.rpm", buildRPM(spec{name: name, version: "1", release: "1"}), nil).Code
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 201 {
			t.Fatalf("concurrent publish status %d", c)
		}
	}
	idx4, _ := f.snapshot(t)
	for i := range 6 {
		if !strings.Contains(idx4["primary"], "<name>race"+string(rune('a'+i))+"</name>") {
			t.Fatalf("race%c missing from final generation", 'a'+i)
		}
	}

	// dnf-style repo file.
	w = f.request("GET", base+"ant.repo", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "baseurl=https://packages.example/feeds/packages/rpm/") || !strings.Contains(w.Body.String(), "repo_gpgcheck=1") {
		t.Fatal(w.Body.String())
	}
}

func TestUnsignedFeedAndOtherFormat(t *testing.T) {
	f := setup(t, false)
	assertCode(t, f.request("PUT", base+"Packages/a-1-1.x86_64.rpm", buildRPM(spec{name: "a", version: "1", release: "1"}), nil), 201)
	f.snapshot(t)
	assertCode(t, f.request("GET", base+"repodata/repomd.xml.asc", nil, nil), 404)
	assertCode(t, f.request("GET", base+"repodata/repomd.xml.key", nil, nil), 404)
	w := f.request("GET", base+"ant.repo", nil, nil)
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "repo_gpgcheck=0") {
		t.Fatal(w.Body.String())
	}
	// A feed of another format is not served by this adapter.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO feeds(instance_id,slug,format) VALUES($1,'other','raw')`, f.instance); err != nil {
		t.Fatal(err)
	}
	assertCode(t, f.request("GET", "/feeds/other/rpm/repodata/repomd.xml", nil, nil), 404)
}

func TestPublicURL(t *testing.T) {
	r := &Repository{}
	for _, bad := range []string{"", "http://packages.example", "https://u:p@host", "https://host/path", "https://host?x=1", "ftp://host"} {
		if r.SetPublicURL(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, ok := range []string{"https://packages.example", "http://127.0.0.1:8080"} {
		if err := r.SetPublicURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
