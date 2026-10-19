package terraform

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

type providerFixture struct {
	entity *openpgp.Entity
	pub    string
	zips   map[string][]byte
	sums   []byte
	sig    []byte
}

func newEntity(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	e, err := openpgp.NewEntity("Release", "", "release@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w, err := armor.Encode(&b, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return e, b.String()
}

func providerZip(t *testing.T, binary, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, err := z.Create(binary)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(content))
	_ = z.Close()
	return b.Bytes()
}

func sumLine(name string, b []byte) []byte {
	h := sha256.Sum256(b)
	return []byte(hex.EncodeToString(h[:]) + "  " + name + "\n")
}

func newProvider(t *testing.T, typ, ver string, platforms ...string) providerFixture {
	t.Helper()
	e, pub := newEntity(t)
	p := providerFixture{entity: e, pub: pub, zips: map[string][]byte{}}
	names := []string{}
	for _, pl := range platforms {
		o, a, _ := strings.Cut(pl, "/")
		name := zipName(typ, ver, o, a)
		p.zips[name] = providerZip(t, "terraform-provider-"+typ+"_v"+ver, "binary for "+pl)
		names = append(names, name)
	}
	sort.Strings(names)
	for _, n := range names {
		p.sums = append(p.sums, sumLine(n, p.zips[n])...)
	}
	p.sign(t, e)
	return p
}

func (p *providerFixture) sign(t *testing.T, e *openpgp.Entity) {
	t.Helper()
	var s bytes.Buffer
	if err := openpgp.DetachSign(&s, e, bytes.NewReader(p.sums), nil); err != nil {
		t.Fatal(err)
	}
	p.sig = s.Bytes()
}

func (p providerFixture) form(extra map[string]string, drop ...string) ([]byte, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	skip := map[string]bool{}
	for _, d := range drop {
		skip[d] = true
	}
	field := func(name string, v []byte) {
		if !skip[name] {
			w, _ := mw.CreateFormField(name)
			_, _ = w.Write(v)
		}
	}
	field("shasums", p.sums)
	field("shasums_sig", p.sig)
	field("signing_key", []byte(p.pub))
	for k, v := range extra {
		field(k, []byte(v))
	}
	names := make([]string, 0, len(p.zips))
	for n := range p.zips {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="zip"; filename=%q`, n))
		w, _ := mw.CreatePart(h)
		_, _ = w.Write(p.zips[n])
	}
	_ = mw.Close()
	return b.Bytes(), mw.FormDataContentType()
}

func (f fixture) putProvider(path string, p providerFixture, extra map[string]string, drop ...string) *httptest.ResponseRecorder {
	body, ct := p.form(extra, drop...)
	return f.request("PUT", path, body, map[string]string{"Content-Type": ct})
}

func TestProvider(t *testing.T) {
	f := setup(t)
	const base = testBase + "v1/providers/acme/demo/"
	p := newProvider(t, "demo", "1.2.3", "linux/amd64", "darwin/arm64")
	assertCode(t, f.putProvider(base+"1.2.3", p, map[string]string{"protocols": "5.0,6.0"}), 201)
	assertCode(t, f.putProvider(base+"1.2.3", p, map[string]string{"protocols": "5.0,6.0"}), 201)

	w := f.request("GET", base+"versions", nil, nil)
	assertCode(t, w, 200)
	var list struct {
		Versions []struct {
			Version   string
			Protocols []string
			Platforms []struct{ OS, Arch string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Versions) != 1 || list.Versions[0].Version != "1.2.3" || len(list.Versions[0].Platforms) != 2 || len(list.Versions[0].Protocols) != 2 {
		t.Fatal(err, w.Body.String())
	}
	w = f.request("GET", base+"1.2.3/download/linux/amd64", nil, nil)
	assertCode(t, w, 200)
	var dl struct {
		Protocols           []string `json:"protocols"`
		Filename            string   `json:"filename"`
		DownloadURL         string   `json:"download_url"`
		ShasumsURL          string   `json:"shasums_url"`
		ShasumsSignatureURL string   `json:"shasums_signature_url"`
		Shasum              string   `json:"shasum"`
		SigningKeys         struct {
			Keys []struct {
				KeyID      string `json:"key_id"`
				AsciiArmor string `json:"ascii_armor"`
			} `json:"gpg_public_keys"`
		} `json:"signing_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dl); err != nil {
		t.Fatal(err)
	}
	name := zipName("demo", "1.2.3", "linux", "amd64")
	const files = "https://packages.example/feeds/packages/terraform/v1/providers/acme/demo/1.2.3/files/"
	if dl.Filename != name || dl.DownloadURL != files+name || dl.ShasumsURL != files+"terraform-provider-demo_1.2.3_SHA256SUMS" || dl.ShasumsSignatureURL != dl.ShasumsURL+".sig" {
		t.Fatalf("%+v", dl)
	}
	// Do what Terraform does: fetch by the advertised URLs and verify end to end.
	fetch := func(u string) []byte {
		r := f.request("GET", strings.TrimPrefix(u, "https://packages.example"), nil, nil)
		assertCode(t, r, 200)
		return r.Body.Bytes()
	}
	zipBytes, sums, sig := fetch(dl.DownloadURL), fetch(dl.ShasumsURL), fetch(dl.ShasumsSignatureURL)
	h := sha256.Sum256(zipBytes)
	if hex.EncodeToString(h[:]) != dl.Shasum || !bytes.Equal(zipBytes, p.zips[name]) || !strings.Contains(string(sums), dl.Shasum+"  "+name) {
		t.Fatal("download bytes differ from shasum")
	}
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(dl.SigningKeys.Keys[0].AsciiArmor))
	if err != nil || len(ring) != 1 {
		t.Fatal(err)
	}
	if _, err = openpgp.CheckDetachedSignature(ring, bytes.NewReader(sums), bytes.NewReader(sig), nil); err != nil {
		t.Fatal(err)
	}
	if dl.SigningKeys.Keys[0].KeyID != strings.ToUpper(fmt.Sprintf("%016x", p.entity.PrimaryKey.KeyId)) {
		t.Fatal("key id")
	}
	assertCode(t, f.request("HEAD", strings.TrimPrefix(dl.DownloadURL, "https://packages.example"), nil, nil), 200)
	assertCode(t, f.request("GET", base+"1.2.3/download/windows/amd64", nil, nil), 404)
	assertCode(t, f.request("GET", base+"1.2.3/files/terraform-provider-demo_1.2.3_freebsd_386.zip", nil, nil), 404)
	assertCode(t, f.request("GET", base+"1.2.3/files/evil.zip", nil, nil), 404)
	assertCode(t, f.request("GET", base+"9.9.9/download/linux/amd64", nil, nil), 404)
	assertCode(t, f.request("DELETE", base+"9.9.9", nil, nil), 404)

	// Different bytes for an immutable version conflict, even if self-consistent.
	q := newProvider(t, "demo", "1.2.3", "linux/amd64", "darwin/arm64")
	assertCode(t, f.putProvider(base+"1.2.3", q, map[string]string{"protocols": "5.0,6.0"}), 409)

	// Each rejection below must leave the version absent and unadvertised.
	v := "1.3.0"
	bad := func(mut func(*providerFixture), extra map[string]string, drop ...string) {
		t.Helper()
		x := newProvider(t, "demo", v, "linux/amd64", "darwin/arm64")
		mut(&x)
		assertCode(t, f.putProvider(base+v, x, extra, drop...), 400)
		assertCode(t, f.request("GET", base+v+"/download/linux/amd64", nil, nil), 404)
	}
	linux := zipName("demo", v, "linux", "amd64")
	darwin := zipName("demo", v, "darwin", "arm64")
	none := func(*providerFixture) {}
	resum := func(x *providerFixture) {
		x.sums = nil
		for _, n := range []string{darwin, linux} {
			if z, ok := x.zips[n]; ok {
				x.sums = append(x.sums, sumLine(n, z)...)
			}
		}
		x.sign(t, x.entity)
	}
	bad(func(x *providerFixture) { x.zips[linux] = providerZip(t, "terraform-provider-demo_v"+v, "evil") }, nil) // substituted bytes
	bad(func(x *providerFixture) { o, _ := newEntity(t); x.sign(t, o) }, nil)                                    // signed by a different key
	bad(func(x *providerFixture) { _, x.pub = newEntity(t) }, nil)                                               // key does not match
	bad(func(x *providerFixture) { x.sig[len(x.sig)-3] ^= 0xff }, nil)                                           // corrupt signature
	bad(func(x *providerFixture) { x.sums = bytes.Replace(x.sums, []byte("linux"), []byte("Linux"), 1) }, nil)   // sums altered after signing
	bad(func(x *providerFixture) {
		x.sums = append(x.sums, []byte(strings.Repeat("a", 64)+"  "+zipName("demo", v, "windows", "amd64")+"\n")...)
		x.sign(t, x.entity)
	}, nil) // sums list a platform that was not uploaded: incomplete closure
	bad(func(x *providerFixture) { delete(x.zips, darwin) }, nil) // uploaded set smaller than sums
	bad(none, nil, "shasums_sig")
	bad(none, nil, "signing_key")
	bad(none, nil, "shasums")
	bad(none, map[string]string{"protocols": "x"})
	bad(none, map[string]string{"evil": "1"})
	bad(func(x *providerFixture) { x.zips[linux] = providerZip(t, "README", "x"); resum(x) }, nil)
	bad(func(x *providerFixture) { x.zips[linux] = providerZip(t, "terraform-provider-other", "x"); resum(x) }, nil)
	bad(func(x *providerFixture) {
		x.zips["terraform-provider-demo_9.9.9_linux_amd64.zip"] = x.zips[linux]
		delete(x.zips, linux)
	}, nil)
	bad(func(x *providerFixture) {
		var b bytes.Buffer
		w, _ := armor.Encode(&b, openpgp.PrivateKeyType, nil)
		_ = x.entity.SerializePrivate(w, nil)
		_ = w.Close()
		x.pub = b.String()
	}, nil) // never accept secret key material
	assertCode(t, f.request("PUT", base+v, []byte("x"), map[string]string{"Content-Type": "application/json"}), 400)
	w = f.request("GET", base+"versions", nil, nil)
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), v) {
		t.Fatal("rejected version listed")
	}

	// Armored signatures are accepted and normalized to the binary form Terraform reads.
	a := newProvider(t, "demo", "1.4.0", "linux/amd64")
	var armored bytes.Buffer
	if err = openpgp.ArmoredDetachSign(&armored, a.entity, bytes.NewReader(a.sums), nil); err != nil {
		t.Fatal(err)
	}
	a.sig = armored.Bytes()
	assertCode(t, f.putProvider(base+"1.4.0", a, nil), 201)
	if got := f.request("GET", base+"1.4.0/files/terraform-provider-demo_1.4.0_SHA256SUMS.sig", nil, nil); got.Code != 200 || bytes.HasPrefix(got.Body.Bytes(), []byte("-----")) {
		t.Fatal("signature not normalized")
	}

	// A token without Admin cannot ingest.
	ctx := context.Background()
	pub, err := (identity.Tokens{DB: f.pool}).Issue(ctx, f.instance, f.principal, f.principal, []identity.Scope{{Actions: []identity.Action{identity.Read, identity.Publish}}}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	c := newProvider(t, "demo", "1.5.0", "linux/amd64")
	body, ct := c.form(nil)
	req := httptest.NewRequest("PUT", base+"1.5.0", bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer "+pub.Secret)
	rec := httptest.NewRecorder()
	f.registry.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("non-admin upload %d", rec.Code)
	}

	// Stored-byte substitution after publication is detected on resolution.
	var evil string
	if err = f.pool.QueryRow(ctx, `SELECT b.id::text FROM versions v JOIN assets a ON a.version_id=v.id JOIN blobs b ON b.id=a.blob_id WHERE v.version='1.4.0' AND a.asset_key=$1`, zipName("demo", "1.4.0", "linux", "amd64")).Scan(&evil); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE assets SET blob_id=$1 WHERE asset_key=$2`, evil, name); err != nil {
		t.Logf("database refuses asset repointing (immutability enforced): %v", err)
		return
	}
	if w = f.request("GET", base+"1.2.3/download/linux/amd64", nil, nil); w.Code != 502 {
		t.Fatalf("tampered zip served: %d %s", w.Code, w.Body.String())
	}
}

// Terraform fetches download_url, shasums_url and shasums_signature_url without
// credentials; on an authenticated feed the advertised URLs must carry their own
// short-lived authorization.
func TestProviderSignedFileURLs(t *testing.T) {
	f := setup(t)
	f.registry.SetSigningKey(bytes.Repeat([]byte{7}, 32))
	const base = testBase + "v1/providers/acme/signed/"
	p := newProvider(t, "signed", "1.0.0", "linux/amd64")
	assertCode(t, f.putProvider(base+"1.0.0", p, map[string]string{"protocols": "5.0"}), 201)
	w := f.request("GET", base+"1.0.0/download/linux/amd64", nil, nil)
	assertCode(t, w, 200)
	var dl struct {
		DownloadURL         string `json:"download_url"`
		ShasumsURL          string `json:"shasums_url"`
		ShasumsSignatureURL string `json:"shasums_signature_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &dl); err != nil {
		t.Fatal(err)
	}
	anon := func(u string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", strings.TrimPrefix(u, "https://packages.example"), nil)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		f.registry.ServeHTTP(rec, req)
		return rec
	}
	name := zipName("signed", "1.0.0", "linux", "amd64")
	for _, u := range []string{dl.DownloadURL, dl.ShasumsURL, dl.ShasumsSignatureURL} {
		if !strings.Contains(u, "ant-sig=") {
			t.Fatalf("unsigned url %s", u)
		}
		assertCode(t, anon(u), 200)
	}
	if got := anon(dl.DownloadURL).Body.Bytes(); !bytes.Equal(got, p.zips[name]) {
		t.Fatal("zip differs")
	}
	// Unsigned, tampered and re-pointed URLs stay unauthorized.
	assertCode(t, anon(strings.SplitN(dl.DownloadURL, "?", 2)[0]), 401)
	assertCode(t, anon(dl.DownloadURL+"x"), 403)
	sig := strings.SplitN(dl.DownloadURL, "?", 2)[1]
	assertCode(t, anon(strings.SplitN(dl.ShasumsURL, "?", 2)[0]+"?"+sig), 403)
	// Revoking the principal's token ends the URL's usefulness.
	if _, err := f.pool.Exec(context.Background(), `UPDATE service_tokens SET revoked_at=clock_timestamp() WHERE instance_id=$1`, f.instance); err != nil {
		t.Fatal(err)
	}
	if rec := anon(dl.DownloadURL); rec.Code == 200 {
		t.Fatal("revoked token still served")
	}
}
