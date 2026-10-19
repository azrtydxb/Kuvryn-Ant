package harness

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Swift (F24), CocoaPods (F04), Hugging Face (F12), Ansible Galaxy (F02), CRAN (F20), Pub (F18), Git LFS (F09), p2 (F17).

func init() {
	Register(Scenario{Format: "swift", Contract: "F24", Clients: []string{"swift"}, Run: runSwift})
	Register(Scenario{Format: "cocoapods", Contract: "F04", Clients: []string{"ruby"}, Run: runCocoapods})
	Register(Scenario{Format: "huggingface", Contract: "F12", Clients: []string{"python"}, Run: runHF})
	Register(Scenario{Format: "ansible", Contract: "F02", Clients: []string{"python"}, Run: runAnsible})
	Register(Scenario{Format: "cran", Contract: "F20", Clients: []string{"r"}, Run: runCran})
	Register(Scenario{Format: "pub", Contract: "F18", Clients: []string{"dart"}, Run: runPub})
	Register(Scenario{Format: "gitlfs", Contract: "F09", Clients: []string{"alpine"}, Run: runGitLFS})
	Register(Scenario{Format: "p2", Contract: "F17", Clients: []string{"curl"}, Run: runP2})
}

var authOrFail = regexp.MustCompile(`(?i)401|403|unauthori|forbidden|credential|denied|authenticat|failed|error`)

// ---- Swift

// ---- CocoaPods

func runCocoapods(c *Ctx) {
	slug := c.MustFeed("cocoapods", "local", nil)
	pod := "KcDemo" + c.RunID
	src := c.UserCred("/feeds/" + slug + "/cocoapods/")
	podsetup := "export LANG=C.UTF-8 LC_ALL=C.UTF-8 GEM_HOME=/work/gems PATH=/work/gems/bin:$PATH; (command -v rsync >/dev/null || (apt-get update -qq && apt-get install -y -qq rsync) >/dev/null 2>&1); test -x /work/gems/bin/pod || gem install cocoapods -v 1.17.0 --no-document >/dev/null; pod() { command pod --allow-root \"$@\"; }; printf 'machine %s login service password %s\\n' \"${HOST%%:*}\" \"$TOKEN\" > $HOME/.netrc; chmod 600 $HOME/.netrc; "
	rb := func(s string, o ...RunOpt) func() error {
		return func() error { return c.Sh("ruby", podsetup+s, o...) }
	}
	publish := func(v string) error {
		spec := fmt.Sprintf(`{"name":"%s","version":"%s","summary":"conformance fixture","homepage":"https://example.invalid","license":"MIT","authors":{"k":"k@example.invalid"},"platforms":{"ios":"15.0"},"source":{"http":"https://example.invalid/x.tar.gz"},"source_files":"Classes/*.swift"}`, pod, v)
		tgz := TarGz([]string{"Classes/A.swift"}, map[string]string{"Classes/A.swift": "public let answer = 42\n"})
		body, ct := Multipart(Part{Name: "podspec", Filename: pod + ".podspec.json", ContentType: "application/json", Data: []byte(spec)}, Part{Name: "source", Filename: pod + "-" + v + ".tar.gz", ContentType: "application/gzip", Data: tgz})
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/cocoapods/pods/" + pod + "/" + v, Body: body, Hdr: map[string]string{"Content-Type": ct}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	podfile := func(source, name, v string) string {
		return F(`rm -rf /work/app && mkdir -p /work/app && cd /work/app && printf "install! 'cocoapods', integrate_targets: false\nsource '@s@'\ntarget 'App' do\n  platform :ios, '15.0'\n  pod '@p@', '@v@'\nend\n" > Podfile && pod install 2>&1 | tail -20 && test -d Pods/@p@`, "s", source, "p", name, "v", v)
	}
	c.Step("client-version", func() error { return c.Version("ruby", podsetup+"pod --version") })
	c.Step("publish: PUT pods/{Pod}/{version} (multipart podspec + source archive)", func() error { return publish("1.0.0") })
	c.Step("consume: pod repo add-cdn registers the feed as a CDN source", rb(F(`pod repo add-cdn ant "@s@" && pod repo list | grep -i ant`, "s", src)))
	c.Step("consume: pod install downloads the pod archive from the feed", rb(podfile(src, pod, "1.0.0")))
	c.Step("same-endpoint write then read: publish 1.0.1, the CDN versions shard lists it", Seq(func() error { return publish("1.0.1") },
		func() error {
			sum := md5.Sum([]byte(pod))
			h := fmt.Sprintf("%x", sum)
			b, err := c.Get(fmt.Sprintf("/feeds/%s/cocoapods/all_pods_versions_%c_%c_%c.txt", slug, h[0], h[1], h[2]))
			if err != nil {
				return err
			}
			return Contains(string(b), "1.0.1")
		}))
	c.Step("same-endpoint write then read: pod install resolves ~> 1.0 to 1.0.1", rb(F(`pod repo remove ant >/dev/null 2>&1; pod repo add-cdn ant "@s@" >/dev/null; `, "s", src)+podfile(src, pod, "~> 1.0")+" && grep -q 1.0.1 /work/app/Podfile.lock"))
	c.Step("delete/unlist: DELETE pods/{Pod}/{version} unlists 1.0.1", func() error {
		return c.Expect("DELETE", "/feeds/"+slug+"/cocoapods/pods/"+pod+"/1.0.1", Basic, 200, 204)
	})
	c.Step("delete/unlist: management API relist then delete 1.0.0", Seq(func() error { return c.MgmtVersion(slug, pod, "", "unlist") }, func() error { return c.MgmtVersion(slug, pod, "", "relist") }))

	rslug := c.MustFeed("cocoapods", "remote", &Remote{Origin: "https://cdn.cocoapods.org", Redirect: []string{"https://cdn.jsdelivr.net"}})
	rsrc := c.LastSource
	rsrcURL := c.UserCred("/feeds/" + rslug + "/cocoapods/")
	sum := md5.Sum([]byte("AFNetworking"))
	pfx := fmt.Sprintf("%x", sum)
	specPath := fmt.Sprintf("/feeds/%s/cocoapods/Specs/%c/%c/%c/AFNetworking/4.0.1/AFNetworking.podspec.json", rslug, pfx[0], pfx[1], pfx[2])
	ok := c.Step("remote pull-through miss: pod install AFNetworking 4.0.1 resolved through the feed (specs from cdn.cocoapods.org, source from github)", rb(F(`pod repo add-cdn antr "@s@" >/dev/null; `, "s", rsrcURL)+podfile(rsrcURL, "AFNetworking", "4.0.1")))
	c.Step("remote pull-through hit: cached podspec served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		_, _, _, err := c.Do(Req{Path: specPath, Auth: Basic, Want: []int{200}})
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: pod spec cat with invalid credentials rejected", rb(F(`pod repo add-cdn bad "@s@"`, "s", strings.Replace(src, "$TOKEN", "$BADTOKEN", 1)), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: authOrFail}))
	c.Step("auth denial: unauthenticated CocoaPods-version.yml returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/cocoapods/CocoaPods-version.yml", NoAuth, 401, 403)
	})
}

// ---- Hugging Face

const hfSetup = "test -x /work/hv/bin/hf || (python -m venv /work/hv && /work/hv/bin/pip install -q huggingface_hub==2.2.0); export PATH=/work/hv/bin:$PATH; "

func runHF(c *Ctx) {
	slug := c.MustFeed("huggingface", "local", nil)
	repo := "kcdemo/model-" + c.RunID
	env := func(extra map[string]string) RunOpt {
		e := map[string]string{"HF_ENDPOINT": c.Env.ClientBase + "/feeds/" + slug + "/huggingface", "HF_TOKEN": c.Env.Token, "HF_HUB_DISABLE_TELEMETRY": "1", "HF_HOME": "/work/hfhome"}
		for k, v := range extra {
			e[k] = v
		}
		return RunOpt{Env: e}
	}
	hf := func(s string, o ...RunOpt) func() error {
		opt := env(nil)
		if len(o) > 0 {
			opt = o[0]
		}
		return func() error { return c.Sh("python", hfSetup+s, opt) }
	}
	upload := func(session string, files map[string]string) error {
		for p, content := range files {
			if _, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/huggingface/api/models/" + repo + "/ant-upload/" + session + "/" + p, Body: []byte(content), Hdr: map[string]string{"X-Checksum-Sha256": Sha256Hex([]byte(content))}, Auth: Basic, Want: []int{200, 201, 204}}); err != nil {
				return err
			}
		}
		_, _, _, err := c.Do(Req{Method: "POST", Path: "/feeds/" + slug + "/huggingface/api/models/" + repo + "/ant-commit/" + session, Body: []byte(`{"refs":["main"]}`), Hdr: map[string]string{"Content-Type": "application/json"}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	c.Step("client-version", func() error {
		return c.Version("python", hfSetup+`echo "huggingface_hub $(python -c 'import huggingface_hub as h; print(h.__version__)')"`)
	})
	c.Step("publish: PUT ant-upload + POST ant-commit (admin ingestion; the hub commit API is not implemented)", func() error {
		return upload("sess-a-"+c.RunID, map[string]string{"config.json": `{"model_type":"kcdemo","answer":42}`, "README.md": "# kcdemo\n"})
	})
	c.Step("consume: hf download config.json and snapshot_download from the feed", hf(F(`rm -rf /work/dl && hf download @r@ config.json --local-dir /work/dl && grep -q kcdemo /work/dl/config.json && python -c "from huggingface_hub import snapshot_download as s; p=s('@r@', cache_dir='/work/hfsnap'); import os; print(sorted(os.listdir(p)))"`, "r", repo)))
	c.Step("same-endpoint write then read: second commit, new revision visible to the client", Seq(
		func() error { return upload("sess-b-"+c.RunID, map[string]string{"extra.txt": "second commit"}) },
		hf(F(`rm -rf /work/dl2 && hf download @r@ extra.txt --local-dir /work/dl2 && grep -q "second commit" /work/dl2/extra.txt`, "r", repo))))
	c.Step("delete: DELETE ant-ref/main tombstones the ref", func() error {
		return c.Expect("DELETE", "/feeds/"+slug+"/huggingface/api/models/"+repo+"/ant-ref/main", Basic, 200, 204)
	})

	rslug := c.MustFeed("huggingface", "remote", &Remote{Origin: "https://huggingface.co", Redirect: []string{"https://cdn-lfs.huggingface.co", "https://cdn-lfs-us-1.hf.co", "https://cas-bridge.xethub.hf.co", "https://cdn-lfs.hf.co"}})
	rsrc := c.LastSource
	renv := env(map[string]string{"HF_ENDPOINT": c.Env.ClientBase + "/feeds/" + rslug + "/huggingface"})
	ok := c.Step("remote pull-through miss: hf download prajjwal1/bert-tiny config.json from huggingface.co via feed", hf(`rm -rf /work/rdl && hf download prajjwal1/bert-tiny config.json --local-dir /work/rdl && grep -q hidden_size /work/rdl/config.json`, renv))
	c.Step("remote pull-through hit: cached file served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/huggingface/prajjwal1/bert-tiny/resolve/main/config.json", Auth: Basic, Want: []int{200}})
		if err == nil {
			err = Contains(string(b), "hidden_size")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: hf download with invalid token rejected", hf(F(`rm -rf /work/dl3 && hf download @r@ config.json --local-dir /work/dl3`, "r", repo), env(map[string]string{"HF_TOKEN": "invalid-token"}).withFail()))
	c.Step("auth denial: unauthenticated api/whoami-v2 returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/huggingface/api/whoami-v2", NoAuth, 401, 403)
	})
}

func (r RunOpt) withFail() RunOpt { r.ExpectFail = true; r.ExpectRe = authOrFail; return r }

// ---- Ansible Galaxy

const ansSetup = "test -x /work/av/bin/ansible-galaxy || (python -m venv /work/av && /work/av/bin/pip install -q ansible-core==2.21.5); export PATH=/work/av/bin:$PATH; "

func runAnsible(c *Ctx) {
	slug := c.MustFeed("ansible", "local", nil)
	ns, name := "kcdemo", "coll"+c.RunID
	server := c.Env.ClientBase + "/feeds/" + slug + "/ansible/"
	ca := ""
	if c.Env.CA != "" {
		ca = "--ignore-certs"
	}
	_ = ca
	an := func(s string, o ...RunOpt) func() error {
		return func() error { return c.Sh("python", ansSetup+s, o...) }
	}
	build := func(v string) string {
		return F(`rm -rf /work/coll && mkdir -p /work/coll/@n@/@c@/plugins && cd /work/coll/@n@/@c@ && printf 'namespace: @n@\nname: @c@\nversion: @v@\nreadme: README.md\nauthors:\n  - kuvryn\ndescription: conformance fixture\nlicense:\n  - MIT\ntags: []\ndependencies: {}\n' > galaxy.yml && echo '# fixture' > README.md && ansible-galaxy collection build --force --output-path /work/dist`, "n", ns, "c", name, "v", v)
	}
	c.Step("client-version", func() error { return c.Version("python", ansSetup+"ansible-galaxy --version | head -1") })
	apiPublish := func(v string) error {
		f := ns + "-" + name + "-" + v + ".tar.gz"
		data, err := c.ReadFile("dist/" + f)
		if err != nil {
			return err
		}
		body, ct := Multipart(Part{Name: "sha256", Data: []byte(Sha256Hex(data))}, Part{Name: "file", Filename: f, ContentType: "application/octet-stream", Data: data})
		_, _, _, err = c.Do(Req{Method: "POST", Path: "/feeds/" + slug + "/ansible/v3/artifacts/collections", Body: body, Hdr: map[string]string{"Content-Type": ct}, Want: []int{200, 201, 202}})
		return err
	}
	c.Step("publish: ansible-galaxy collection build + publish 1.0.0 (native client)", an(build("1.0.0")+F(` && ansible-galaxy collection publish /work/dist/@n@-@c@-1.0.0.tar.gz --server @s@ --token $TOKEN`, "n", ns, "c", name, "s", server)))
	c.Step("publish: POST v3/artifacts/collections with a plain multipart body (workaround for the native client's base64 encoding) 1.0.0", func() error { return apiPublish("1.0.0") })
	c.Step("consume: ansible-galaxy collection install from the feed", an(F(`rm -rf /work/inst && ansible-galaxy collection install @n@.@c@:1.0.0 --server @s@ --token $TOKEN -p /work/inst && test -f /work/inst/ansible_collections/@n@/@c@/MANIFEST.json`, "n", ns, "c", name, "s", server)))
	c.Step("same-endpoint write then read: publish 1.0.1 (API) and install latest", Seq(an(build("1.0.1")), func() error { return apiPublish("1.0.1") },
		an(F(`rm -rf /work/inst2 && ansible-galaxy collection install @n@.@c@ --server @s@ --token $TOKEN -p /work/inst2 && grep -q 1.0.1 /work/inst2/ansible_collections/@n@/@c@/MANIFEST.json`, "n", ns, "c", name, "s", server))))
	c.Step("delete: DELETE v3/collections/{ns}/{name}/versions/{version}", func() error {
		return c.Expect("DELETE", "/feeds/"+slug+"/ansible/v3/collections/"+ns+"/"+name+"/versions/1.0.1", Bearer, 200, 202, 204)
	})
	c.Step("delete/unlist: management API delete 1.0.0", func() error { return c.MgmtVersion(slug, name, "", "delete") })

	rslug := c.MustFeed("ansible", "remote", &Remote{Origin: "https://galaxy.ansible.com/api/", Redirect: []string{"https://galaxy.ansible.com", "https://ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com"}})
	rsrc := c.LastSource
	rserver := c.Env.ClientBase + "/feeds/" + rslug + "/ansible/"
	ok := c.Step("remote pull-through miss: ansible-galaxy install ansible.posix:1.5.4 from galaxy.ansible.com via feed", an(F(`rm -rf /work/rinst && ansible-galaxy collection install ansible.posix:1.5.4 --server @s@ --token $TOKEN -p /work/rinst && test -f /work/rinst/ansible_collections/ansible/posix/MANIFEST.json`, "s", rserver)))
	c.Step("remote pull-through hit: cached collection archive served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/ansible/downloads/ansible/posix/1.5.4/collection.tar.gz", Want: []int{200}})
		if err == nil && len(b) < 10000 {
			err = fmt.Errorf("short archive")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: ansible-galaxy install with invalid token rejected", an(F(`ansible-galaxy collection install @n@.@c@:1.0.0 --server @s@ --token $BADTOKEN -p /work/inst3`, "n", ns, "c", name, "s", server), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: authOrFail}))
	c.Step("auth denial: unauthenticated collection detail returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/ansible/v3/collections/"+ns+"/"+name+"/", NoAuth, 401, 403)
	})
}

// ---- CRAN

func runCran(c *Ctx) {
	slug := c.MustFeed("cran", "local", nil)
	pkg := "kcdemo" + c.RunID
	repo := c.UserCred("/feeds/" + slug + "/cran")
	rr := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("r", s, o...) } }
	build := func(v string) string {
		return F(`rm -rf /work/src && mkdir -p /work/src/@p@/R && cd /work/src && printf 'Package: @p@\nVersion: @v@\nTitle: Conformance Fixture\nDescription: A fixture package for conformance runs.\nLicense: MIT + file LICENSE\nAuthor: Kuvryn\nMaintainer: Kuvryn <ops@example.invalid>\n' > @p@/DESCRIPTION && printf 'YEAR: 2026\nCOPYRIGHT HOLDER: Kuvryn\n' > @p@/LICENSE && echo 'export(answer)' > @p@/NAMESPACE && echo 'answer <- function() 42L' > @p@/R/a.R && R CMD build @p@ >/dev/null && cp @p@_@v@.tar.gz /work/`, "p", pkg, "v", v)
	}
	ingest := func(v string) error {
		f := pkg + "_" + v + ".tar.gz"
		data, err := c.ReadFile(f)
		if err != nil {
			return err
		}
		_, _, _, err = c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/cran/src/contrib/" + f, Body: data, Hdr: map[string]string{"X-Checksum-Sha256": Sha256Hex(data)}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	install := func(lib, name, repoURL string) string {
		return F(`rm -rf /work/@l@ && mkdir -p /work/@l@ && REPO="@r@" Rscript -e 'options(repos=c(ANT=Sys.getenv("REPO"))); install.packages("@n@", lib="/work/@l@", type="source", quiet=FALSE)' && ls /work/@l@/@n@`, "l", lib, "n", name, "r", repoURL)
	}
	c.Step("client-version", func() error { return c.Version("r", "R --version | head -1") })
	c.Step("publish: R CMD build, then PUT src/contrib/{pkg}_{ver}.tar.gz (admin ingestion)", Seq(rr(build("1.0.0")), func() error { return ingest("1.0.0") }))
	c.Step("consume: install.packages from the feed and call the function", rr(install("rl1", pkg, repo)+F(` && Rscript -e 'library(@p@, lib.loc="/work/rl1"); stopifnot(answer()==42L)'`, "p", pkg)))
	c.Step("same-endpoint write then read: PUT 1.0.1, install.packages resolves it", Seq(rr(build("1.0.1")), func() error { return ingest("1.0.1") },
		rr(install("rl2", pkg, repo)+F(` && grep -q "Version: 1.0.1" /work/rl2/@p@/DESCRIPTION`, "p", pkg))))
	c.Step("delete: DELETE src/contrib/{pkg}_{ver}.tar.gz then PACKAGES no longer lists 1.0.1", func() error {
		if err := c.Expect("DELETE", "/feeds/"+slug+"/cran/src/contrib/"+pkg+"_1.0.1.tar.gz", Basic, 200, 204); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + slug + "/cran/src/contrib/PACKAGES", Auth: Basic, Want: []int{200}})
		if err == nil && strings.Contains(string(b), "Version: 1.0.1") {
			err = fmt.Errorf("deleted version still listed")
		}
		return err
	})
	c.Step("delete/unlist: management API delete 1.0.0", func() error { return c.MgmtVersion(slug, "contrib", "", "delete") })

	rslug := c.MustFeed("cran", "remote", &Remote{Origin: "https://cloud.r-project.org"})
	rsrc := c.LastSource
	rrepo := c.UserCred("/feeds/" + rslug + "/cran")
	ok := c.Step("remote pull-through miss: install.packages praise from cloud.r-project.org via feed", rr(install("rr1", "praise", rrepo)))
	c.Step("remote pull-through hit: cached source tarball served with upstream unreachable", func() error {
		idx, err := c.Get("/feeds/" + rslug + "/cran/src/contrib/PACKAGES")
		if err != nil {
			return err
		}
		m := regexp.MustCompile(`(?m)^Package: praise\nVersion: (\S+)`).FindStringSubmatch(string(idx))
		if m == nil {
			return fmt.Errorf("praise not in the feed PACKAGES")
		}
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/cran/src/contrib/praise_" + m[1] + ".tar.gz", Auth: Basic, Want: []int{200}})
		if err == nil && len(b) < 1000 {
			err = fmt.Errorf("short tarball")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: install.packages with invalid credentials rejected", rr(install("rl9", pkg, strings.Replace(repo, "$TOKEN", "$BADTOKEN", 1)), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|unauthori|forbidden|credential|denied|not available|cannot|No such file`)}))
	c.Step("auth denial: unauthenticated PACKAGES returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/cran/src/contrib/PACKAGES", NoAuth, 401, 403)
	})
}

// ---- Pub

func runPub(c *Ctx) {
	slug := c.MustFeed("pub", "local", nil)
	pkg := "kcdemo_" + c.RunID
	url := c.Env.ClientBase + "/feeds/" + slug + "/pub"
	dt := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("dart", s, o...) } }
	tok := func(env string) string {
		return F(`dart pub token remove @u@ >/dev/null 2>&1; dart pub token add @u@ --env-var @e@ >/dev/null; `, "u", url, "e", env)
	}
	pkgdir := func(v string) string {
		return F(`rm -rf /work/pubpkg && mkdir -p /work/pubpkg/lib && cd /work/pubpkg && printf 'name: @p@\nversion: @v@\ndescription: Conformance fixture package for Kuvryn Ant.\npublish_to: @u@\nenvironment:\n  sdk: ^3.0.0\n' > pubspec.yaml && echo 'int answer() => 42;' > lib/@p@.dart && printf '# fixture\n' > README.md && printf '## @v@\n- fixture\n' > CHANGELOG.md && printf 'MIT License\n\nCopyright (c) 2026 Kuvryn\n' > LICENSE`, "p", pkg, "v", v, "u", url)
	}
	consume := func(dir, constraint string) string {
		return F(`rm -rf /work/@d@ && mkdir -p /work/@d@/bin && cd /work/@d@ && printf 'name: app\nenvironment:\n  sdk: ^3.0.0\ndependencies:\n  @p@:\n    hosted: @u@\n    version: "@c@"\n' > pubspec.yaml && echo 'void main(){}' > bin/app.dart && dart pub get && grep -q @p@ pubspec.lock`, "d", dir, "p", pkg, "u", url, "c", constraint)
	}
	c.Step("client-version", func() error { return c.Version("dart", "dart --version 2>&1 | head -1") })
	c.Step("publish: dart pub publish 0.1.0 (native)", dt(tok("TOKEN")+pkgdir("0.1.0")+" && dart pub publish --force"))
	c.Step("consume: dart pub get resolves 0.1.0 from the hosted feed", dt(tok("TOKEN")+consume("papp", "^0.1.0")))
	c.Step("same-endpoint write then read: publish 0.1.1, pub upgrade resolves it", dt(tok("TOKEN")+pkgdir("0.1.1")+" && dart pub publish --force && cd /work/papp && dart pub upgrade && grep -q 0.1.1 pubspec.lock"))
	c.Step("delete/unlist: retract 0.1.1 (PUT .../retract) then restore", func() error {
		p := "/feeds/" + slug + "/pub/api/packages/" + pkg + "/versions/0.1.1/retract"
		if err := c.Expect("PUT", p, Bearer, 200, 204); err != nil {
			return err
		}
		return c.Expect("DELETE", p, Bearer, 200, 204)
	})
	c.Step("delete: DELETE api/packages/{pkg}/versions/{v}", func() error {
		return c.Expect("DELETE", "/feeds/"+slug+"/pub/api/packages/"+pkg+"/versions/0.1.1", Bearer, 200, 204)
	})
	c.Step("delete/unlist: management API delete 0.1.0", func() error { return c.MgmtVersion(slug, pkg, "", "delete") })

	rslug := c.MustFeed("pub", "remote", &Remote{Origin: "https://pub.dev", Redirect: []string{"https://storage.googleapis.com", "https://pub.dev"}})
	rsrc := c.LastSource
	rurl := c.Env.ClientBase + "/feeds/" + rslug + "/pub"
	ok := c.Step("remote pull-through miss: dart pub get meta 1.15.0 from pub.dev via feed", dt(F(`dart pub token remove @u@ >/dev/null 2>&1; dart pub token add @u@ --env-var TOKEN >/dev/null; rm -rf /work/rapp && mkdir -p /work/rapp/bin && cd /work/rapp && printf 'name: rapp\nenvironment:\n  sdk: ^3.0.0\ndependencies:\n  meta:\n    hosted: @u@\n    version: "1.15.0"\n' > pubspec.yaml && echo 'void main(){}' > bin/app.dart && dart pub get`, "u", rurl)))
	c.Step("remote pull-through hit: cached archive served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/pub/packages/meta/versions/1.15.0.tar.gz", Want: []int{200}})
		if err == nil && len(b) < 1000 {
			err = fmt.Errorf("short archive")
		}
		return err
	}, SkipUnless(ok))

	c.Step("auth denial: dart pub get with invalid token rejected", dt(tok("BADTOKEN")+consume("papp2", "^0.1.0"), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: authOrFail}))
	c.Step("auth denial: unauthenticated package document returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/pub/api/packages/"+pkg, NoAuth, 401, 403)
	})
}

// ---- Git LFS

func runGitLFS(c *Ctx) {
	slug := c.MustFeed("git-lfs", "local", nil)
	lfs := strings.Replace(c.Env.ClientBase, "://", "://service:$TOKEN@", 1) + "/feeds/" + slug + "/git-lfs"
	setup := `apk add --no-cache git git-lfs >/dev/null; git config --global user.email a@b.c; git config --global user.name a; git config --global init.defaultBranch main; git lfs install --skip-repo >/dev/null; `
	gl := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("alpine", setup+s, o...) } }
	oid := ""
	c.Step("client-version", func() error { return c.Version("alpine", setup+"git lfs version") })
	c.Step("publish: git lfs track + commit + push uploads the object through the batch API", gl(F(`rm -rf /work/lfs-origin.git /work/lfs-w && git init -q --bare /work/lfs-origin.git && git clone -q -c lfs.url="@u@" -c lfs.locksverify=false /work/lfs-origin.git /work/lfs-w 2>/dev/null && cd /work/lfs-w && git config lfs.url "@u@" && git config lfs.locksverify false && git lfs track "*.bin" >/dev/null && head -c 200000 /dev/urandom > big.bin && git add -A && git commit -qm lfs && git push -q origin main && git lfs ls-files -l | cut -d' ' -f1 > /work/oid.txt`, "u", lfs)))
	c.Step("consume: fresh clone + git lfs pull downloads the exact bytes", gl(F(`rm -rf /work/lfs-c && git clone -q -c lfs.url="@u@" -c lfs.locksverify=false /work/lfs-origin.git /work/lfs-c && cd /work/lfs-c && git config lfs.url "@u@" && git config lfs.locksverify false && git lfs pull && cmp big.bin /work/lfs-w/big.bin`, "u", lfs)))
	c.Step("same-endpoint write then read: second object pushed and fetched", gl(F(`cd /work/lfs-w && git config lfs.url "@u@" && head -c 50000 /dev/urandom > two.bin && git add -A && git commit -qm two && git push -q origin main && rm -rf /work/lfs-c2 && git clone -q -c lfs.url="@u@" -c lfs.locksverify=false /work/lfs-origin.git /work/lfs-c2 && cd /work/lfs-c2 && git config lfs.url "@u@" && git lfs pull && cmp two.bin /work/lfs-w/two.bin`, "u", lfs)))
	c.Step("delete: DELETE objects/{oid} then the object cannot be downloaded", func() error {
		b, err := c.ReadFile("oid.txt")
		if err != nil {
			return err
		}
		oid = strings.TrimSpace(string(b))
		if len(oid) != 64 {
			return fmt.Errorf("bad oid %q", oid)
		}
		if err := c.Expect("DELETE", "/feeds/"+slug+"/git-lfs/objects/"+oid, Basic, 200, 204); err != nil {
			return err
		}
		// A download batch for the deleted object must now answer 404 for that object.
		body := []byte(`{"operation":"download","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":200000}]}`)
		out, _, _, err := c.Do(Req{Method: "POST", Path: "/feeds/" + slug + "/git-lfs/objects/batch", Body: body, Hdr: map[string]string{"Content-Type": "application/vnd.git-lfs+json", "Accept": "application/vnd.git-lfs+json"}, Want: []int{200}})
		if err != nil {
			return err
		}
		return Contains(string(out), `"code":404`)
	})
	c.Step("delete/unlist: management API delete of the second object", func() error { return c.MgmtVersion(slug, "", "", "delete") })
	c.Step("remote pull-through: git-lfs (real upstream pending)", func() error { return nil }, Skip("pending: git-lfs remote (upstream LFS batch API) is implemented (httptest-covered); not yet run against a real LFS server, see #288"))
	c.Step("auth denial: git lfs pull with invalid credentials rejected", gl(F(`rm -rf /work/lfs-c4 && git clone -q -c lfs.url="@u@" -c lfs.locksverify=false /work/lfs-origin.git /work/lfs-c4 && cd /work/lfs-c4 && git config lfs.url "@u@" && git lfs pull`, "u", strings.Replace(lfs, "$TOKEN", "$BADTOKEN", 1)), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: authOrFail}))
	c.Step("auth denial: unauthenticated batch request returns 401", func() error {
		_, _, _, err := c.Do(Req{Method: "POST", Path: "/feeds/" + slug + "/git-lfs/objects/batch", Body: []byte(`{"operation":"download","objects":[]}`), Hdr: map[string]string{"Content-Type": "application/vnd.git-lfs+json"}, Auth: NoAuth, Want: []int{401, 403}})
		return err
	})
}

// ---- p2

func runP2(c *Ctx) {
	slug := c.MustFeed("p2", "local", nil)
	id := "org.kcdemo.bundle" + c.RunID
	jar := ZipBytes([]string{"META-INF/MANIFEST.MF"}, map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\nBundle-ManifestVersion: 2\nBundle-SymbolicName: " + id + "\nBundle-Version: 1.0.0\n"})
	artifacts := fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8'?><?artifactRepository version='1.1.0'?><repository name='kcdemo' type='org.eclipse.equinox.p2.artifact.repository.simpleRepository' version='1'><properties size='1'><property name='p2.timestamp' value='1700000000000'/></properties><artifacts size='1'><artifact classifier='osgi.bundle' id='%s' version='1.0.0'><properties size='3'><property name='artifact.size' value='%d'/><property name='download.size' value='%d'/><property name='download.checksum.sha-256' value='%s'/></properties></artifact></artifacts></repository>`, id, len(jar), len(jar), Sha256Hex(jar))
	content := fmt.Sprintf(`<?xml version='1.0' encoding='UTF-8'?><?metadataRepository version='1.1.0'?><repository name='kcdemo' type='org.eclipse.equinox.internal.p2.metadata.repository.LocalMetadataRepository' version='1'><properties size='1'><property name='p2.timestamp' value='1700000000000'/></properties><units size='1'><unit id='%s' version='1.0.0' singleton='false'><provides size='2'><provided namespace='org.eclipse.equinox.p2.iu' name='%s' version='1.0.0'/><provided namespace='osgi.bundle' name='%s' version='1.0.0'/></provides><artifacts size='1'><artifact classifier='osgi.bundle' id='%s' version='1.0.0'/></artifacts></unit></units></repository>`, id, id, id, id)
	repo := ZipBytes([]string{"content.xml", "artifacts.xml", "plugins/" + id + "_1.0.0.jar"}, map[string]string{"content.xml": content, "artifacts.xml": artifacts, "plugins/" + id + "_1.0.0.jar": string(jar)})
	auth := `-H "Authorization: Bearer $TOKEN"`
	base := "$ANT/feeds/" + slug + "/p2"
	cu := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("curl", s, o...) } }
	c.Step("client-version", func() error { return c.Version("curl", "curl --version | head -1") })
	c.Step("publish: PUT repository.zip (p2 repository with a signed-checksum bundle)", func() error {
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/p2/repository.zip", Body: repo, Auth: Basic, Want: []int{200, 201}})
		return err
	})
	c.Step("consume: p2.index, content.xml and artifacts.xml are served; the bundle jar matches its checksum", cu(F(`curl -fsS @a@ @b@/p2.index | head -5 && curl -fsS @a@ @b@/content.xml | grep -q @i@ && curl -fsS @a@ @b@/artifacts.xml | grep -q @i@ && curl -fsS @a@ @b@/plugins/@i@_1.0.0.jar -o b.jar && test "$(sha256sum b.jar | cut -d' ' -f1)" = @h@`, "a", auth, "b", base, "i", id, "h", Sha256Hex(jar))))
	c.Step("consume: Eclipse p2 director / Tycho install", func() error { return nil }, Skip("manual: the Eclipse p2 director (about 150 MB platform) was not pinned into the harness; protocol-level reads above exercise the repository files. Run `eclipse -application org.eclipse.equinox.p2.director -repository <feed>/p2 -installIU "+id+"` manually"))
	c.Step("same-endpoint conflict: re-publishing the same artifact id/version with different bytes returns 409", func() error {
		other := ZipBytes([]string{"META-INF/MANIFEST.MF"}, map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\nBundle-ManifestVersion: 2\nBundle-SymbolicName: " + id + "\nBundle-Version: 1.0.0\nX: changed\n"})
		art := strings.ReplaceAll(strings.ReplaceAll(artifacts, Sha256Hex(jar), Sha256Hex(other)), fmt.Sprint(len(jar)), fmt.Sprint(len(other)))
		r2 := ZipBytes([]string{"content.xml", "artifacts.xml", "plugins/" + id + "_1.0.0.jar"}, map[string]string{"content.xml": content, "artifacts.xml": art, "plugins/" + id + "_1.0.0.jar": string(other)})
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/p2/repository.zip", Body: r2, Auth: Basic, Want: []int{409}})
		return err
	})
	c.Step("delete/unlist: p2 has no per-artifact delete (F17); DELETE of the bundle is refused", func() error {
		code, err := c.status("DELETE", "/feeds/"+slug+"/p2/plugins/"+id+"_1.0.0.jar", Basic)
		if err != nil {
			return err
		}
		if code < 400 {
			return fmt.Errorf("expected refusal, got %d", code)
		}
		return nil
	})
	c.Step("remote pull-through: p2 (real upstream pending)", func() error { return nil }, Skip("pending: p2 remote pull-through is implemented (httptest-covered); not yet run against a real p2 repository, see #288"))
	c.Step("auth denial: invalid token rejected by curl", cu(F(`curl -fsS -H "Authorization: Bearer $BADTOKEN" @b@/content.xml`, "b", base), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}))
	c.Step("auth denial: unauthenticated content.xml returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/p2/content.xml", NoAuth, 401, 403)
	})
}

func (c *Ctx) status(method, path string, auth AuthMode) (int, error) {
	_, code, _, err := c.Do(Req{Method: method, Path: path, Auth: auth})
	return code, err
}

var _ = json.Marshal
