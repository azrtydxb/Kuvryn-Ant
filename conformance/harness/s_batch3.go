package harness

import (
	"bytes"
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// Alpine apk (F01), APT (F03), RPM/dnf (F26), Terraform (F25).

func init() {
	Register(Scenario{Format: "apk", Contract: "F01", Clients: []string{"alpine"}, Run: runApk})
	Register(Scenario{Format: "apt", Contract: "F03", Clients: []string{"debian"}, Run: runApt})
	Register(Scenario{Format: "rpm", Contract: "F26", Clients: []string{"fedora"}, Run: runRpm})
	Register(Scenario{Format: "terraform", Contract: "F25", Clients: []string{"terraform"}, Run: runTerraform})
}

func hostArch() (apk, deb string) {
	if runtime.GOARCH == "arm64" {
		return "aarch64", "arm64"
	}
	return "x86_64", "amd64"
}

// ---- Alpine APK

func runApk(c *Ctx) {
	slug := c.MustFeed("apk", "local", nil)
	arch, _ := hostArch()
	pkg := "kcdemo" + c.RunID
	feedURL := c.Env.ClientBase + "/feeds/" + slug + "/apk"
	repoLine := strings.Replace(feedURL, "://", "://service:$TOKEN@", 1) + "/v3.24/main"
	keyName := "ant-" + c.feedIDs[slug][:8] + ".rsa.pub"
	// Both versions are built in one container run: installing alpine-sdk is the slow part.
	buildAll := F(`export PACKAGER="Kuvryn <ops@example.invalid>"; apk add --no-cache alpine-sdk >/dev/null && abuild-keygen -a -n >/dev/null 2>&1; cp $HOME/.abuild/*.pub /etc/apk/keys/; for spec in "@p@ all 1.0 1.0" "@p@ all 1.1 1.1" "@p@na noarch 1.0 na"; do set -- $spec; n=$1; a=$2; v=$3; out=$4; rm -rf /work/ab/$out && mkdir -p /work/ab/$out/pkg && cd /work/ab/$out/pkg && `+
		`printf 'pkgname=%s\npkgver=%s\npkgrel=0\npkgdesc="conformance fixture"\nurl="https://example.invalid"\narch="%s"\nlicense="MIT"\noptions="!check"\nbuild() { :; }\npackage() { mkdir -p "$pkgdir"/usr/share/%s; echo 42 > "$pkgdir"/usr/share/%s/answer; }\n' "$n" "$v" "$a" "$n" "$n" > APKBUILD && `+
		`abuild -F checksum >/dev/null && abuild -F -d >/dev/null && cp $(find /work/home/packages -name "$n-$v-r0.apk" | head -1) /work/pkg-$out.apk || exit 1; done; ls /work/*.apk`, "p", pkg)
	upload := func(v string) error {
		data, err := c.ReadFile("pkg-" + v + ".apk")
		if err != nil {
			return err
		}
		_, _, _, err = c.Do(Req{Method: "PUT", Path: fmt.Sprintf("/feeds/%s/apk/v3.24/main/%s/%s-%s-r0.apk", slug, arch, pkg, v), Body: data, Auth: Basic, Want: []int{200, 201, 204}})
		return err
	}
	setup := func(repo string) string {
		return F(`mkdir -p /etc/apk/keys && cp /work/home/.abuild/*.pub /etc/apk/keys/ && wget -q -O /etc/apk/keys/@k@ --header "Authorization: Bearer $TOKEN" @f@/keys/@k@ && echo "@r@" > /etc/apk/repositories && apk update`, "k", keyName, "f", "$ANT/feeds/"+slug+"/apk", "r", repo)
	}
	al := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("alpine", s, o...) } }
	c.Step("client-version", func() error { return c.Version("alpine", "apk --version") })
	c.Step("publish: abuild a noarch apk, then PUT {branch}/{repo}/{arch}/{file}.apk (admin ingestion)", Seq(al(buildAll), func() error { return upload("1.0") }))
	c.Step("consume: apk update + apk add from the signed feed index", al(setup(repoLine)+F(` && apk add --no-cache @p@ && test "$(cat /usr/share/@p@/answer)" = 42`, "p", pkg)))
	c.Step("edge: a noarch package listed in the arch index is downloadable (apk requests it under the noarch path)", Seq(
		func() error {
			data, err := c.ReadFile("pkg-na.apk")
			if err != nil {
				return err
			}
			_, _, _, err = c.Do(Req{Method: "PUT", Path: fmt.Sprintf("/feeds/%s/apk/v3.24/main/%s/%sna-1.0-r0.apk", slug, arch, pkg), Body: data, Auth: Basic, Want: []int{200, 201, 204}})
			return err
		},
		al(setup(repoLine)+F(` && apk add --no-cache @p@na`, "p", pkg))))
	c.Step("same-endpoint write then read: upload 1.1, apk add resolves it", Seq(func() error { return upload("1.1") },
		al(setup(repoLine)+F(` && apk add --no-cache @p@=1.1-r0 && apk info -v | grep @p@-1.1`, "p", pkg))))
	c.Step("delete: DELETE {file}.apk then apk cannot add 1.1", Seq(
		func() error {
			return c.Expect("DELETE", fmt.Sprintf("/feeds/%s/apk/v3.24/main/%s/%s-1.1-r0.apk", slug, arch, pkg), Basic, 200, 204)
		},
		al(setup(repoLine)+F(` && ! apk add --no-cache @p@=1.1-r0`, "p", pkg))))
	c.Step("delete/unlist: management API delete 1.0", func() error { return c.MgmtVersion(slug, "main", "", "delete") })
	c.Step("remote pull-through: apk (real upstream pending)", func() error { return nil }, Skip("pending: apk remote pull-through is implemented (httptest-covered); not yet run against a real upstream, see #288"))
	c.Step("auth denial: apk update with invalid credentials rejected", al(setup(strings.Replace(repoLine, "$TOKEN", "$BADTOKEN", 1)), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|unauthori|forbidden|credential|denied|UNTRUSTED|not found|failed`)}))
	c.Step("auth denial: unauthenticated APKINDEX returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/apk/v3.24/main/"+arch+"/APKINDEX.tar.gz", NoAuth, 401, 403)
	})
}

// ---- APT

func runApt(c *Ctx) {
	slug := c.MustFeed("apt", "local", nil)
	_, darch := hostArch()
	_ = darch
	pkg := "kcdemo" + c.RunID
	pool := func(v string) string {
		return fmt.Sprintf("/feeds/%s/apt/pool/main/k/%s/%s_%s_all.deb", slug, pkg, pkg, v)
	}
	build := func(v string) string {
		return F(`rm -rf /work/deb && mkdir -p /work/deb/DEBIAN /work/deb/usr/share/@p@ && echo 42 > /work/deb/usr/share/@p@/answer && printf 'Package: @p@\nVersion: @v@\nArchitecture: all\nMaintainer: Kuvryn <ops@example.invalid>\nDescription: conformance fixture\n' > /work/deb/DEBIAN/control && dpkg-deb -Zgzip --build /work/deb /work/@p@_@v@_all.deb >/dev/null; rm -rf /work/debz && cp -r /work/deb /work/debz && sed -i "s/^Package: .*/Package: @p@z/" /work/debz/DEBIAN/control && dpkg-deb --build /work/debz /work/@p@z_@v@_all.deb >/dev/null`, "p", pkg, "v", v)
	}
	ingest := func(v string) error {
		data, err := c.ReadFile(pkg + "_" + v + "_all.deb")
		if err != nil {
			return err
		}
		_, _, _, err = c.Do(Req{Method: "PUT", Path: pool(v) + "?distribution=stable", Body: data, Auth: Basic, Want: []int{200, 201, 204}})
		return err
	}
	base := c.Env.ClientBase + "/feeds/" + slug + "/apt"
	setup := func(key, repoBase, dist, tok string) string {
		return F(`if [ "$SCHEME" = https ] && [ ! -s /etc/ssl/certs/ca-certificates.crt ]; then apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq ca-certificates >/dev/null 2>&1; fi; rm -f /etc/apt/sources.list.d/* /etc/apt/sources.list; if [ -n "$CA_PEM" ]; then printf 'Acquire::https::CaInfo "%s";\n' "$CA_PEM" > /etc/apt/apt.conf.d/99ca; fi; mkdir -p /etc/apt/auth.conf.d; printf 'deb [signed-by=@k@] @b@ @d@ main\n' > /etc/apt/sources.list.d/ant.list; printf 'machine %s login service password %s\n' "$SCHEME://${HOST}/feeds/@s@/apt/" "@t@" > /etc/apt/auth.conf.d/ant.conf; chmod 600 /etc/apt/auth.conf.d/ant.conf; apt-get update`,
			"k", key, "b", repoBase, "d", dist, "s", slug, "t", tok)
	}
	ap := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("debian", s, o...) } }
	c.Step("client-version", func() error { return c.Version("debian", "apt-get --version | head -1") })
	c.Step("publish: dpkg-deb build, then PUT pool/main/k/{pkg}/{file}.deb?distribution=stable (admin ingestion)", Seq(ap(build("1.0")), func() error { return ingest("1.0") }))
	c.Step("edge: package built with the dpkg-deb default compression (zstd on trixie) is accepted", func() error {
		data, err := c.ReadFile(pkg + "z_1.0_all.deb")
		if err != nil {
			return err
		}
		_, _, _, err = c.Do(Req{Method: "PUT", Path: fmt.Sprintf("/feeds/%s/apt/pool/main/k/%sz/%sz_1.0_all.deb?distribution=edge", slug, pkg, pkg), Body: data, Auth: Basic, Want: []int{200, 201, 204}})
		return err
	})
	c.Step("consume: apt-get update (signed Release, by-hash) + apt-get install", Seq(
		func() error {
			b, err := c.Get("/feeds/" + slug + "/apt/signing-key.gpg")
			if err != nil {
				return err
			}
			return c.WriteFile("key.gpg", b)
		},
		ap(setup("/work/key.gpg", base, "stable", "$TOKEN")+F(` && apt-get install -y @p@ && test "$(cat /usr/share/@p@/answer)" = 42`, "p", pkg))))
	c.Step("same-endpoint write then read: PUT 1.1, apt-get update, install upgrade", Seq(ap(build("1.1")), func() error { return ingest("1.1") },
		ap(setup("/work/key.gpg", base, "stable", "$TOKEN")+F(` && apt-get install -y @p@=1.1 && dpkg -s @p@ | grep -x 'Version: 1.1'`, "p", pkg))))
	c.Step("delete/unlist: DELETE pool path unlists 1.1 into a new signed generation", Seq(
		func() error { return c.Expect("DELETE", pool("1.1")+"?distribution=stable", Basic, 200, 204) },
		ap(setup("/work/key.gpg", base, "stable", "$TOKEN")+F(` && ! apt-cache madison @p@ | grep -q 1.1`, "p", pkg))))
	c.Step("delete/unlist: management API unlist then relist 1.0", Seq(
		func() error { return c.MgmtVersion(slug, pkg, "", "unlist") },
		func() error { return c.MgmtVersion(slug, pkg, "", "relist") }))

	rslug := c.MustFeed("apt", "remote", &Remote{Origin: "https://deb.debian.org/debian"})
	rsrc := c.LastSource
	rbase := c.Env.ClientBase + "/feeds/" + rslug + "/apt"
	rsetup := strings.ReplaceAll(setup("/usr/share/keyrings/debian-archive-keyring.gpg", rbase, "trixie", "$TOKEN"), "/feeds/"+slug+"/apt/", "/feeds/"+rslug+"/apt/")
	debPath := ""
	ok := c.Step("remote pull-through miss: apt-get update + install hello from deb.debian.org via feed", func() error {
		if err := c.Sh("debian", rsetup+` && apt-get install -y --no-install-recommends hello && hello | grep -q Hello && apt-get install --reinstall --print-uris -y hello | grep -o "http[^']*\.deb"`); err != nil {
			return err
		}
		m := regexp.MustCompile(`/pool/\S+\.deb`).FindString(c.Out())
		if m == "" {
			return fmt.Errorf("no pool path in apt output")
		}
		debPath = "/feeds/" + rslug + "/apt" + m
		return nil
	})
	c.Step("remote pull-through hit: cached .deb served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: debPath, Auth: Basic, Want: []int{200}})
		if err == nil && len(data) < 1000 {
			err = fmt.Errorf("short deb")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: apt-get update with invalid credentials rejected", ap(setup("/work/key.gpg", base, "stable", "$BADTOKEN"), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|unauthori|forbidden|credential|denied|not signed|failed`)}))
	c.Step("auth denial: unauthenticated Release returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/apt/dists/stable/Release", NoAuth, 401, 403)
	})
}

// ---- RPM / dnf

func runRpm(c *Ctx) {
	slug := c.MustFeed("rpm", "local", nil)
	arch, _ := hostArch()
	pkg := "kcdemo" + c.RunID
	ca := ""
	if c.Env.CA != "" {
		ca = "sslcacert=/ca/ca.pem\\n"
	}
	build := func(v string) string {
		return F(`dnf -y install rpm-build >/dev/null 2>&1; rm -rf /work/home/rpmbuild && mkdir -p /work/home/rpmbuild/SPECS && cd /work/home/rpmbuild && `+
			`printf 'Name: @p@\nVersion: @v@\nRelease: 1\nSummary: conformance fixture\nLicense: MIT\nBuildArch: noarch\n%%description\nfixture\n%%install\nmkdir -p %%{buildroot}/usr/share/@p@\necho 42 > %%{buildroot}/usr/share/@p@/answer\n%%files\n/usr/share/@p@/answer\n' > SPECS/x.spec && `+
			`HOME=/work/home rpmbuild -bb SPECS/x.spec >/dev/null 2>&1; cp RPMS/noarch/@p@-@v@-1.noarch.rpm /work/`, "p", pkg, "v", v)
	}
	ingest := func(v string) error {
		f := fmt.Sprintf("%s-%s-1.noarch.rpm", pkg, v)
		data, err := c.ReadFile(f)
		if err != nil {
			return err
		}
		_, _, _, err = c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/rpm/Packages/" + f, Body: data, Hdr: map[string]string{"X-Checksum-Sha256": Sha256Hex(data)}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	repo := func(id, base, tok string) string {
		return F(`printf '[%s]\nname=%s\nbaseurl=%s\nenabled=1\ngpgcheck=0\nrepo_gpgcheck=0\nskip_if_unavailable=0\nusername=service\npassword=%s\n@ca@' '@i@' '@i@' '@b@' "@t@" > /etc/yum.repos.d/@i@.repo; `, "i", id, "b", base, "t", tok, "ca", ca)
	}
	base := c.Env.ClientBase + "/feeds/" + slug + "/rpm"
	dnf := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("fedora", s, o...) } }
	only := func(id string) string { return "--disablerepo='*' --enablerepo=" + id }
	c.Step("client-version", func() error { return c.Version("fedora", "dnf --version | head -1") })
	c.Step("publish: rpmbuild a noarch rpm, then PUT Packages/{nevra}.rpm (admin ingestion)", Seq(dnf(build("1.0")), func() error { return ingest("1.0") }))
	c.Step("consume: dnf makecache + install from the feed (repomd, primary, filelists, other)", dnf(repo("ant", base, "$TOKEN")+"dnf -y "+only("ant")+" makecache && dnf -y "+only("ant")+F(` install @p@ && test "$(cat /usr/share/@p@/answer)" = 42`, "p", pkg)))
	c.Step("same-endpoint write then read: PUT 1.1, dnf upgrade", Seq(dnf(build("1.1")), func() error { return ingest("1.1") },
		dnf(repo("ant", base, "$TOKEN")+"dnf -y "+only("ant")+" clean metadata && dnf -y "+only("ant")+F(` install -y @p@-1.1 && rpm -q @p@ | grep 1.1`, "p", pkg))))
	c.Step("delete: DELETE Packages/{file}.rpm republishes repodata without it", Seq(
		func() error {
			return c.Expect("DELETE", fmt.Sprintf("/feeds/%s/rpm/Packages/%s-1.1-1.noarch.rpm", slug, pkg), Basic, 200, 204)
		},
		dnf(repo("ant", base, "$TOKEN")+"dnf -y "+only("ant")+" clean metadata && ! dnf -y "+only("ant")+F(` install @p@-1.1`, "p", pkg))))
	c.Step("delete/unlist: management API delete 1.0", func() error { return c.MgmtVersion(slug, pkg, "", "delete") })

	// Rocky 9 BaseOS (gz/xz metadata; Fedora's zstd primary is a documented limitation).
	rslug := c.MustFeed("rpm", "remote", &Remote{Origin: "https://dl.rockylinux.org/pub/rocky/9/BaseOS/" + arch + "/os"})
	rsrc := c.LastSource
	rbase := c.Env.ClientBase + "/feeds/" + rslug + "/rpm"
	rpath := ""
	ok := c.Step("remote pull-through miss: dnf download acl from the Rocky 9 BaseOS repo via feed", func() error {
		if err := c.Sh("fedora", repo("rocky", rbase, "$TOKEN")+"dnf -y "+only("rocky")+" download --url acl"); err != nil {
			return err
		}
		m := regexp.MustCompile(`/Packages/\S+\.rpm`).FindString(c.Out())
		if m == "" {
			return fmt.Errorf("no package url in dnf output")
		}
		rpath = "/feeds/" + rslug + "/rpm" + m
		return c.Sh("fedora", repo("rocky", rbase, "$TOKEN")+"cd /work && dnf -y "+only("rocky")+" download acl && ls /work/acl-*.rpm")
	})
	c.Step("remote pull-through hit: cached rpm served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: rpath, Auth: Basic, Want: []int{200}})
		if err == nil && len(data) < 1000 {
			err = fmt.Errorf("short rpm")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: dnf makecache with invalid credentials rejected", dnf(repo("bad", base, "$BADTOKEN")+"dnf -y "+only("bad")+" makecache", RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|unauthori|forbidden|credential|denied|failed to download metadata|cannot download`)}))
	c.Step("auth denial: unauthenticated repomd.xml returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/rpm/repodata/repomd.xml", NoAuth, 401, 403)
	})
}

// ---- Terraform

func runTerraform(c *Ctx) {
	slug := c.MustFeed("terraform", "local", nil)
	arch, _ := hostArch()
	tfarch := "arm64"
	if arch == "x86_64" {
		tfarch = "amd64"
	}
	ptype := "kc" + c.RunID
	root := c.Env.ClientBase + "/feeds/" + slug + "/terraform"
	rc := func(name, base, tok string) string {
		return F(`printf 'host "%s" {\n  services = {\n    "modules.v1"   = "%s/v1/modules/"\n    "providers.v1" = "%s/v1/providers/"\n  }\n}\ncredentials "%s" {\n  token = "%s"\n}\n' '@n@' '@b@' '@b@' '@n@' "@t@" > /work/tf.rc; export TF_CLI_CONFIG_FILE=/work/tf.rc; `, "n", name, "b", base, "t", tok)
	}
	// Signed provider release built on the host: zips + SHA256SUMS + detached signature + armored key.
	ent, err := openpgp.NewEntity("kuvryn-conformance", "", "conformance@example.invalid", nil)
	if err != nil {
		panic(err)
	}
	var keyBuf bytes.Buffer
	aw, _ := armor.Encode(&keyBuf, openpgp.PublicKeyType, nil)
	_ = ent.Serialize(aw)
	_ = aw.Close()
	provider := func(v string) error {
		var sums bytes.Buffer
		parts := []Part{}
		for _, a := range []string{"amd64", "arm64"} {
			fn := fmt.Sprintf("terraform-provider-%s_%s_linux_%s.zip", ptype, v, a)
			bin := fmt.Sprintf("terraform-provider-%s_v%s", ptype, v)
			z := ZipBytes([]string{bin}, map[string]string{bin: "#!/bin/sh\nexit 0\n"})
			fmt.Fprintf(&sums, "%s  %s\n", Sha256Hex(z), fn)
			parts = append(parts, Part{Name: "zip", Filename: fn, ContentType: "application/zip", Data: z})
		}
		var sig bytes.Buffer
		if err := openpgp.ArmoredDetachSign(&sig, ent, bytes.NewReader(sums.Bytes()), nil); err != nil {
			return err
		}
		parts = append([]Part{{Name: "shasums", Filename: "SHA256SUMS", Data: sums.Bytes()}, {Name: "shasums_sig", Filename: "SHA256SUMS.sig", Data: sig.Bytes()},
			{Name: "signing_key", Data: keyBuf.Bytes()}, {Name: "protocols", Data: []byte("5.0")}}, parts...)
		body, ct := Multipart(parts...)
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/terraform/v1/providers/kcdemo/" + ptype + "/" + v, Body: body, Hdr: map[string]string{"Content-Type": ct}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	module := func(v string) error {
		tgz := TarGz([]string{"main.tf"}, map[string]string{"main.tf": "output \"answer\" { value = 42 }\n"})
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/terraform/v1/modules/kcdemo/mod" + c.RunID + "/aws/" + v, Body: tgz, Hdr: map[string]string{"X-Checksum-Sha256": Sha256Hex(tgz)}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	tf := func(s string, o ...RunOpt) func() error {
		return func() error {
			return c.Sh("terraform", strings.ReplaceAll(s, "ant.example", c.Env.ClientBase[strings.Index(c.Env.ClientBase, "//")+2:]), o...)
		}
	}
	c.Step("client-version", func() error { return c.Version("terraform", "terraform version | head -1") })
	c.Step("publish: PUT signed provider release 1.0.0 (admin ingestion; Terraform has no native publish)", func() error { return provider("1.0.0") })
	c.Step("publish: PUT module archive 1.0.0 (admin ingestion)", func() error { return module("1.0.0") })
	c.Step("consume: terraform init installs the provider (signature verified) from the feed", tf(rc("ant.example", root, "$TOKEN")+F(
		`rm -rf /work/tfp && mkdir -p /work/tfp && cd /work/tfp && printf 'terraform {\n  required_providers {\n    @t@ = {\n      source  = "ant.example/kcdemo/@t@"\n      version = "1.0.0"\n    }\n  }\n}\n' > main.tf && terraform init -no-color && ls .terraform/providers/ant.example/kcdemo/@t@/1.0.0/linux_@a@/`,
		"t", ptype, "a", tfarch)))
	c.Step("consume: terraform init downloads the module (the archive URL is fetched with .netrc credentials)", tf(rc("ant.example", root, "$TOKEN")+F(
		`printf 'machine %s login service password %s\nmachine %s login service password %s\n' "${HOST%%:*}" "$TOKEN" "$HOST" "$TOKEN" > $HOME/.netrc; chmod 600 $HOME/.netrc; rm -rf /work/tfm && mkdir -p /work/tfm && cd /work/tfm && printf 'module "m" {\n  source  = "ant.example/kcdemo/mod@r@/aws"\n  version = "1.0.0"\n}\n' > main.tf && terraform init -no-color && ls .terraform/modules`,
		"r", c.RunID)))
	c.Step("same-endpoint write then read: publish provider 1.1.0 then terraform init -upgrade resolves it", Seq(func() error { return provider("1.1.0") },
		tf(rc("ant.example", root, "$TOKEN")+F(`cd /work/tfp && sed -i 's/version = "1.0.0"\n    }/X/' main.tf && printf 'terraform {\n  required_providers {\n    @t@ = {\n      source  = "ant.example/kcdemo/@t@"\n      version = ">= 1.0.0"\n    }\n  }\n}\n' > main.tf && rm -rf .terraform .terraform.lock.hcl && terraform init -no-color && ls .terraform/providers/ant.example/kcdemo/@t@/1.1.0`, "t", ptype))))
	c.Step("delete: DELETE v1/providers/{ns}/{type}/{version} and v1/modules/.../{version}", func() error {
		if err := c.Expect("DELETE", "/feeds/"+slug+"/terraform/v1/providers/kcdemo/"+ptype+"/1.1.0", Basic, 200, 204); err != nil {
			return err
		}
		return c.Expect("DELETE", "/feeds/"+slug+"/terraform/v1/modules/kcdemo/mod"+c.RunID+"/aws/1.0.0", Basic, 200, 204)
	})
	c.Step("delete/unlist: management API delete provider 1.0.0", func() error { return c.MgmtVersion(slug, ptype, "", "delete") })

	rslug := c.MustFeed("terraform", "remote", &Remote{Origin: "https://registry.terraform.io", Redirect: []string{"https://releases.hashicorp.com"}})
	rsrc := c.LastSource
	rroot := c.Env.ClientBase + "/feeds/" + rslug + "/terraform"
	ok := c.Step("remote pull-through miss: terraform init hashicorp/random 3.6.3 from registry.terraform.io via feed", tf(rc("reg.example", rroot, "$TOKEN")+F(
		`rm -rf /work/tfr && mkdir -p /work/tfr && cd /work/tfr && printf 'terraform {\n  required_providers {\n    random = {\n      source  = "reg.example/hashicorp/random"\n      version = "3.6.3"\n    }\n  }\n}\n' > main.tf && terraform init -no-color && ls .terraform/providers/reg.example/hashicorp/random/3.6.3/linux_@a@/`, "a", tfarch)))
	c.Step("remote pull-through hit: cached provider zip served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: fmt.Sprintf("/feeds/%s/terraform/v1/providers/hashicorp/random/3.6.3/files/terraform-provider-random_3.6.3_linux_%s.zip", rslug, tfarch), Auth: Basic, Want: []int{200}})
		if err == nil && len(data) < 100000 {
			err = fmt.Errorf("short provider zip")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: terraform init with invalid credentials rejected", tf(rc("ant.example", root, "$BADTOKEN")+F(`rm -rf /work/tfb && mkdir -p /work/tfb && cd /work/tfb && printf 'terraform {\n  required_providers {\n    @t@ = {\n      source  = "ant.example/kcdemo/@t@"\n      version = "1.0.0"\n    }\n  }\n}\n' > main.tf && terraform init -no-color`, "t", ptype),
		RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|unauthori|forbidden|credential|denied|authenticat|could not|failed to`)}))
	c.Step("auth denial: unauthenticated provider versions returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/terraform/v1/providers/kcdemo/"+ptype+"/versions", NoAuth, 401, 403)
	})
}
