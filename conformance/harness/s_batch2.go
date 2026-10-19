package harness

import (
	"fmt"
	"regexp"
	"strings"
)

// oci-artifacts/oras (F16), RubyGems (F22), Composer (F05), Conan (F06), Conda (F07).

func init() {
	Register(Scenario{Format: "oras", Contract: "F16", Clients: []string{"oras"}, Run: runOras})
	Register(Scenario{Format: "rubygems", Contract: "F22", Clients: []string{"ruby"}, Run: runRubygems})
	Register(Scenario{Format: "composer", Contract: "F05", Clients: []string{"composer"}, Run: runComposer})
	Register(Scenario{Format: "conan", Contract: "F06", Clients: []string{"python"}, Run: runConan})
	Register(Scenario{Format: "conda", Contract: "F07", Clients: []string{"conda"}, Run: runConda})
}

// ---- oci-artifacts with oras

const orasSetup = `test -x /work/bin/oras || (mkdir -p /work/bin; A=$(uname -m); case $A in aarch64) A=arm64;; x86_64) A=amd64;; esac; ` +
	`wget -qO- https://github.com/oras-project/oras/releases/download/v1.3.4/oras_1.3.4_linux_$A.tar.gz | tar xz -C /work/bin oras); export PATH=/work/bin:$PATH; `

func runOras(c *Ctx) {
	slug := c.MustFeed("oci-artifacts", "local", nil)
	flags := ""
	if c.isHTTP() {
		flags += " --plain-http"
	}
	if c.Env.CA != "" {
		flags += " --ca-file /ca/ca.pem"
	}
	cpf := ""
	if c.isHTTP() {
		cpf += " --from-plain-http --to-plain-http"
	}
	if c.Env.CA != "" {
		cpf += " --from-ca-file /ca/ca.pem --to-ca-file /ca/ca.pem"
	}
	ref := "$HOST/" + slug + "/art" + c.RunID
	login := orasSetup + `echo "$TOKEN" | oras login $HOST -u service --password-stdin` + flags + ` >/dev/null; `
	o := func(s string, opt ...RunOpt) func() error {
		return func() error { return c.Sh("oras", login+s, opt...) }
	}
	digestOf := `D=$(oras manifest fetch --descriptor ` + ref + `:v1` + flags + ` | sed -n 's/.*"digest": *"\([^"]*\)".*/\1/p' | head -1); `
	c.Step("client-version", func() error { return c.Version("oras", orasSetup+"oras version | head -1") })
	c.Step("publish: oras push artifact with artifact-type", o(F(`echo "artifact @r@" > a.txt && oras push @ref@:v1 --artifact-type application/vnd.kuvryn.demo a.txt:text/plain@f@`, "r", c.RunID, "ref", ref, "f", flags)))
	c.Step("consume: oras pull returns identical content and manifest fetch works", o(F(`rm -rf pulled && mkdir pulled && oras pull @ref@:v1 -o pulled@f@ && grep -x "artifact @r@" pulled/a.txt && oras manifest fetch @ref@:v1@f@ | grep -q artifactType`, "ref", ref, "f", flags, "r", c.RunID)))
	c.Step("same-endpoint write then read: attach a referrer and discover it, retag and copy", o(F(
		`echo '{"sbom":true}' > sbom.json && oras attach --artifact-type application/vnd.kuvryn.sbom @ref@:v1 sbom.json:application/json@f@ && oras discover @ref@:v1@f@ | grep -q vnd.kuvryn.sbom && `+
			`oras tag @ref@:v1 v2@f@ && oras cp @ref@:v1 @ref@:copied@cf@ && oras manifest fetch @ref@:copied@f@ >/dev/null`, "ref", ref, "f", flags, "cf", cpf)))
	c.Step("delete: oras manifest delete by digest removes the manifest and its tags", o(F(`@d@ oras manifest delete -f @ref@@$D@f@ && ! oras manifest fetch @ref@:v1@f@`, "d", digestOf, "ref", ref, "f", flags)))

	rslug := c.MustFeed("oci-artifacts", "remote", &Remote{Origin: "https://ghcr.io", Redirect: []string{"https://pkg-containers.githubusercontent.com"}})
	rsrc := c.LastSource
	rref := "$HOST/" + rslug + "/oras-project/oras:v1.3.4"
	digest := ""
	ok := c.Step("remote pull-through miss: oras manifest fetch + blob fetch of ghcr.io/oras-project/oras:v1.3.4", func() error {
		if err := c.Sh("oras", login+F(`oras manifest fetch --descriptor @r@@f@ | sed -n 's/.*"digest": *"\([^"]*\)".*/\1/p' | head -1`, "r", rref, "f", flags)); err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(c.Out()), "\n")
		digest = lines[len(lines)-1]
		return c.Sh("oras", login+F(`oras manifest fetch @r@@f@ > m.json && oras pull @r@@f@ -o rpull >/dev/null 2>&1; test -s m.json`, "r", rref, "f", flags))
	})
	c.Step("remote pull-through hit: manifest by digest served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		return c.Sh("oras", login+F(`oras manifest fetch $HOST/@s@/oras-project/oras@@d@@f@ | grep -q schemaVersion`, "s", rslug, "d", digest, "f", flags))
	}, SkipUnless(ok))

	bad := RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}
	c.Step("auth denial: oras login with invalid credentials rejected", func() error {
		return c.Sh("oras", orasSetup+`echo "$BADTOKEN" | oras login $HOST -u service --password-stdin`+flags, bad)
	})
	c.Step("auth denial: unauthenticated manifest fetch returns 401", func() error {
		return c.Expect("GET", "/v2/"+slug+"/art"+c.RunID+"/manifests/v1", NoAuth, 401)
	})
}

// ---- RubyGems

func runRubygems(c *Ctx) {
	slug := c.MustFeed("rubygems", "local", nil)
	name := "kcdemo-" + c.RunID
	host := "$ANT/feeds/" + slug + "/rubygems"
	src := c.UserCred("/feeds/" + slug + "/rubygems/")
	rb := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("ruby", s, o...) } }
	build := func(v string) string {
		return F(`rm -rf /work/gem && mkdir -p /work/gem/lib && cd /work/gem && echo 'module Kc; ANSWER = 42; end' > lib/kcdemo.rb && printf "Gem::Specification.new do |s|\n s.name='@n@'\n s.version='@v@'\n s.summary='conformance fixture'\n s.authors=['kuvryn']\n s.files=['lib/kcdemo.rb']\n s.license='MIT'\nend\n" > x.gemspec && gem build x.gemspec >/dev/null && ls *.gem`, "n", name, "v", v)
	}
	push := func(key string) string {
		return F(`GEM_HOST_API_KEY=@k@ gem push /work/gem/@n@-*.gem --host @h@`, "k", key, "n", name, "h", host)
	}
	install := func(dir, v string) string {
		return F(`rm -rf /work/@d@ && gem install @n@ -v @v@ --clear-sources --source @s@ --no-document --install-dir /work/@d@ && ls /work/@d@/gems | grep @n@-@v@`, "d", dir, "n", name, "v", v, "s", src)
	}
	c.Step("client-version", func() error {
		return c.Version("ruby", `echo "RubyGems $(gem --version) / $(ruby --version | cut -d' ' -f1-2) / Bundler $(bundle --version)"`)
	})
	c.Step("publish: gem build + gem push 0.1.0", rb(build("0.1.0")+" && "+push("$TOKEN")))
	c.Step("consume: gem install 0.1.0 from the feed", rb(install("g1", "0.1.0")))
	c.Step("consume: bundler resolves the gem through the compact index", rb(F(`rm -rf /work/bund && mkdir /work/bund && cd /work/bund && printf 'source "%s"\ngem "%s", "0.1.0"\n' "@s@" "@n@" > Gemfile && export BUNDLE_PATH=/work/bund/vendor BUNDLE_ALLOW_INSECURE=true; bundle install && bundle exec ruby -e 'require "kcdemo"; puts Kc::ANSWER' | grep -x 42`, "s", src, "n", name)))
	c.Step("same-endpoint write then read: push 0.1.1, gem install latest", rb(build("0.1.1")+" && "+push("$TOKEN")+" && "+F(`rm -rf /work/g2 && gem install @n@ --clear-sources --source @s@ --no-document --install-dir /work/g2 && ls /work/g2/gems | grep @n@-0.1.1`, "n", name, "s", src)))
	c.Step("delete: gem yank 0.1.1 (native) removes the version", rb(F(`GEM_HOST_API_KEY=$TOKEN gem yank @n@ -v 0.1.1 --host @h@ && ! (rm -rf /work/g3; gem install @n@ -v 0.1.1 --clear-sources --source @s@ --no-document --install-dir /work/g3)`, "n", name, "h", host, "s", src)))
	c.Step("delete/unlist: management API delete 0.1.0", func() error { return c.MgmtVersion(slug, name, "0.1.0", "delete") })

	rslug := c.MustFeed("rubygems", "remote", &Remote{Origin: "https://rubygems.org"})
	rsrc := c.LastSource
	rsrcURL := c.UserCred("/feeds/" + rslug + "/rubygems/")
	ok := c.Step("remote pull-through miss: gem install rake 13.2.1 from rubygems.org via feed", rb(F(`rm -rf /work/gr1 && gem install rake -v 13.2.1 --clear-sources --source @s@ --no-document --install-dir /work/gr1 && ls /work/gr1/gems | grep rake-13.2.1`, "s", rsrcURL)))
	c.Step("remote pull-through hit: cached gem served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/rubygems/gems/rake-13.2.1.gem", Auth: Basic, Want: []int{200}})
		if err == nil && len(data) < 10000 {
			err = fmt.Errorf("short gem")
		}
		return err
	}, SkipUnless(ok))

	bad := RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}
	c.Step("auth denial: gem push with invalid key rejected", rb(build("0.9.0")+" && "+push("$BADTOKEN"), bad))
	c.Step("auth denial: gem install with invalid credentials rejected", rb(F(`rm -rf /work/g4; gem install @n@ -v 0.1.0 --clear-sources --source @s@ --no-document --install-dir /work/g4`, "n", name, "s", strings.Replace(src, "$TOKEN", "$BADTOKEN", 1)), bad))
	c.Step("auth denial: unauthenticated compact index returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/rubygems/versions", NoAuth, 401, 403)
	})
}

// ---- Composer

func runComposer(c *Ctx) {
	slug := c.MustFeed("composer", "local", nil)
	pkg := "pkg-" + c.RunID
	full := "kcdemo/" + pkg
	repo := c.Env.ClientBase + "/feeds/" + slug + "/composer"
	secure := ""
	if c.isHTTP() {
		secure = `,"config":{"secure-http":false}`
	}
	auth := `export COMPOSER_AUTH=$(printf '{"http-basic":{"%s":{"username":"service","password":"%s"}}}' "$HOST" "$TOKEN"); `
	zipFor := func(v string) []byte {
		return ZipBytes([]string{"composer.json", "src/Answer.php"}, map[string]string{
			"composer.json":  fmt.Sprintf(`{"name":"%s","version":"%s","description":"conformance fixture","autoload":{"psr-4":{"Kcdemo\\":"src/"}}}`, full, v),
			"src/Answer.php": "<?php\nnamespace Kcdemo;\nclass Answer { public static function value(): int { return 42; } }\n"})
	}
	ingest := func(v string) error {
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/composer/ingest/" + full + "/" + v, Body: zipFor(v), Hdr: map[string]string{"X-Checksum-Sha256": Sha256Hex(zipFor(v))}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	cmp := func(s string, o ...RunOpt) func() error {
		return func() error { return c.Sh("composer", auth+s, o...) }
	}
	proj := func(dir, url, req string) string {
		return F(`rm -rf /work/@d@ && mkdir -p /work/@d@ && cd /work/@d@ && printf '{"repositories":[{"type":"composer","url":"@u@"},{"packagist.org":false}]@sec@}' > composer.json && composer require @r@ --no-interaction`, "d", dir, "u", url, "r", req, "sec", secure)
	}
	c.Step("client-version", func() error { return c.Version("composer", "composer --version --no-ansi") })
	c.Step("publish: PUT ingest/{vendor}/{name}/{version} (admin ingestion; Composer has no native publish) 0.1.0", func() error { return ingest("0.1.0") })
	c.Step("consume: composer require 0.1.0 from the feed and autoload", cmp(proj("c1", repo, full+":0.1.0")+` && php -r 'require "vendor/autoload.php"; echo Kcdemo\Answer::value(), "\n";' | grep -x 42`))
	c.Step("same-endpoint write then read: ingest 0.1.1, composer update resolves it", Seq(
		func() error { return ingest("0.1.1") },
		cmp(`cd /work/c1 && composer require `+full+`:^0.1 --no-interaction && composer show `+full+` | grep -E "^versions +: +\* 0.1.1"`)))
	c.Step("delete: DELETE ingest/{vendor}/{name}/{version} then composer cannot resolve 0.1.1", Seq(
		func() error {
			return c.Expect("DELETE", "/feeds/"+slug+"/composer/ingest/"+full+"/0.1.1", Basic, 200, 204)
		},
		cmp(`! (`+proj("c2", repo, full+":0.1.1")+`)`)))
	c.Step("delete/unlist: management API unlist then relist 0.1.0", Seq(
		func() error { return c.MgmtVersion(slug, pkg, "0.1.0", "unlist") },
		func() error { return c.MgmtVersion(slug, pkg, "0.1.0", "relist") }))

	rslug := c.MustFeed("composer", "remote", &Remote{Origin: "https://repo.packagist.org", Redirect: []string{"https://api.github.com", "https://codeload.github.com"}})
	rsrc := c.LastSource
	ok := c.Step("remote pull-through miss: composer require psr/log 3.0.0 from packagist via feed", cmp(proj("cr1", c.Env.ClientBase+"/feeds/"+rslug+"/composer", "psr/log:3.0.0")+` && test -f vendor/psr/log/src/LoggerInterface.php`))
	c.Step("remote pull-through hit: cached dist served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/composer/dist/psr/log/3.0.0/archive.zip", Auth: Basic, Want: []int{200}})
		if err == nil && len(data) < 1000 {
			err = fmt.Errorf("short dist")
		}
		return err
	}, SkipUnless(ok))

	bad := RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}
	c.Step("auth denial: composer with invalid credentials rejected", func() error {
		return c.Sh("composer", strings.Replace(auth, "$TOKEN", "$BADTOKEN", 1)+proj("c3", repo, full+":0.1.0"), bad)
	})
	c.Step("auth denial: unauthenticated packages.json returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/composer/packages.json", NoAuth, 401, 403)
	})
}

// ---- Conan

const conanSetup = "test -x /work/cv/bin/conan || (python -m venv /work/cv && /work/cv/bin/pip install -q conan==2.33.0); export PATH=/work/cv/bin:$PATH; " +
	`export CONAN_HOME=${CONAN_HOME:-/work/conan-home}; conan profile detect --force >/dev/null 2>&1; if [ -f /ca/ca.pem ]; then echo 'core.net.http:cacert_path=/ca/ca.pem' > $CONAN_HOME/global.conf; fi; `

func runConan(c *Ctx) {
	slug := c.MustFeed("conan", "local", nil)
	name := "kcdemo" + c.RunID
	url := c.Env.ClientBase + "/feeds/" + slug + "/conan"
	cn := func(s string, o ...RunOpt) func() error {
		return func() error { return c.Sh("python", conanSetup+s, o...) }
	}
	remote := func(rn, u string) string {
		return F(`conan remote add @n@ @u@ --force >/dev/null && conan remote login @n@ service -p $TOKEN >/dev/null; `, "n", rn, "u", u)
	}
	create := func(v string) string {
		return F(`rm -rf /work/rec && mkdir -p /work/rec && cd /work/rec && printf 'from conan import ConanFile\nclass P(ConanFile):\n    name = "@n@"\n    version = "@v@"\n    description = "conformance fixture"\n' > conanfile.py && conan create . >/dev/null`, "n", name, "v", v)
	}
	c.Step("client-version", func() error { return c.Version("python", conanSetup+"conan --version") })
	c.Step("publish: conan create + conan upload "+name+"/1.0", cn(create("1.0")+" && "+remote("ant", url)+F(`conan upload "@n@/1.0" -r ant -c`, "n", name)))
	c.Step("consume: conan install --requires from the feed with a fresh cache", cn(F(`export CONAN_HOME=/work/conan-c1; conan profile detect --force >/dev/null 2>&1; `)+remote("ant", url)+F(`conan install --requires=@n@/1.0 -r ant`, "n", name)))
	c.Step("same-endpoint write then read: upload 1.1 and list remote revisions", cn(create("1.1")+" && "+remote("ant", url)+F(`conan upload "@n@/1.1" -r ant -c && conan list "@n@/*" -r ant | grep -F /1.1`, "n", name)))
	c.Step("delete: conan remove "+name+"/1.1 from the remote (native REST v2 DELETE)", cn(remote("ant", url)+F(`conan remove "@n@/1.1" -r ant -c && ! (conan list "@n@/*" -r ant | grep -F /1.1)`, "n", name)))
	c.Step("delete/unlist: management API delete 1.0", func() error { return c.MgmtVersion(slug, name, "", "delete") })

	rslug := c.MustFeed("conan", "remote", &Remote{Origin: "https://center2.conan.io"})
	rsrc := c.LastSource
	rurl := c.Env.ClientBase + "/feeds/" + rslug + "/conan"
	ok := c.Step("remote pull-through miss: conan download zlib/1.3.1 recipe from center2.conan.io via feed", cn(`export CONAN_HOME=/work/conan-r1; conan profile detect --force >/dev/null 2>&1; `+remote("antr", rurl)+`conan download zlib/1.3.1 --only-recipe -r antr`))
	c.Step("remote pull-through hit: fresh cache downloads the recipe with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		return c.Sh("python", conanSetup+`export CONAN_HOME=/work/conan-r2; conan profile detect --force >/dev/null 2>&1; `+remote("antr", rurl)+`conan download zlib/1.3.1 --only-recipe -r antr`)
	}, SkipUnless(ok))

	bad := RunOpt{Env: map[string]string{"TOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)wrong user|password|401|403|unauthori|forbidden`)}
	c.Step("auth denial: conan remote login with invalid token rejected", cn(`export CONAN_HOME=/work/conan-c2; conan profile detect --force >/dev/null 2>&1; conan remote add ant `+url+` --force >/dev/null; conan remote login ant service -p $TOKEN`, bad))
	c.Step("auth denial: unauthenticated search returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/conan/v2/conans/search?q=*", NoAuth, 401, 403)
	})
}

// ---- Conda

func runConda(c *Ctx) {
	slug := c.MustFeed("conda", "local", nil)
	name := "kcdemo-" + c.RunID
	chURL := c.UserCred("/feeds/" + slug + "/conda")
	ssl := ""
	if c.Env.CA != "" {
		ssl = "export MAMBA_SSL_VERIFY=$CA_PEM CONDA_SSL_VERIFY=$CA_PEM REQUESTS_CA_BUNDLE=$CA_PEM; conda config --set ssl_verify $CA_PEM; "
	}
	env := RunOpt{Env: map[string]string{"CONDA_PKGS_DIRS": "/work/pkgs", "CONDA_ENVS_PATH": "/work/envs"}}
	cd := func(s string, o ...RunOpt) func() error {
		opt := env
		if len(o) > 0 {
			opt = o[0]
			for k, v := range env.Env {
				if _, set := opt.Env[k]; !set {
					opt.Env[k] = v
				}
			}
		}
		return func() error { return c.Sh("conda", ssl+s, opt) }
	}
	fn := func(v string) string { return name + "-" + v + "-0.tar.bz2" }
	build := func(v string) func() error {
		idx := fmt.Sprintf(`{"name":"%s","version":"%s","build":"0","build_number":0,"depends":[],"subdir":"noarch","noarch":"generic","license":"MIT"}`, name, v)
		return cd(F(`rm -rf /work/cpkg && mkdir -p /work/cpkg/info /work/cpkg/share && echo 42 > /work/cpkg/share/kcdemo.txt && printf '%s' '@idx@' > /work/cpkg/info/index.json && printf 'share/kcdemo.txt\n' > /work/cpkg/info/files && `+
			`printf '{"paths_version":1,"paths":[{"_path":"share/kcdemo.txt","path_type":"hardlink","sha256":"%s","size_in_bytes":3}]}' "$(sha256sum /work/cpkg/share/kcdemo.txt | cut -d' ' -f1)" > /work/cpkg/info/paths.json && tar -cjf /work/@f@ -C /work/cpkg info share`, "idx", idx, "f", fn(v)))
	}
	ingest := func(v string) error {
		data, err := c.ReadFile(fn(v))
		if err != nil {
			return err
		}
		_, _, _, err = c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/conda/ingest/noarch/" + fn(v), Body: data, Hdr: map[string]string{"X-Checksum-Sha256": Sha256Hex(data)}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	create := func(envName, spec, ch, tool string) string {
		return F(`@t@ create -y -n @e@ --override-channels -c "@c@" @s@ --no-deps`, "t", tool, "e", envName, "c", ch, "s", spec)
	}
	c.Step("client-version", func() error {
		return c.Version("conda", `echo "conda $(conda --version | cut -d' ' -f2) / mamba $(mamba --version | head -1)"`)
	})
	c.Step("publish: build .tar.bz2 and PUT ingest/noarch/{file} (admin ingestion; conda has no native publish) 1.0.0", Seq(build("1.0.0"), func() error { return ingest("1.0.0") }))
	c.Step("consume: conda create from the feed channel installs 1.0.0", cd(create("t1", name+"=1.0.0", chURL, "conda")+" && test -f /work/envs/t1/share/kcdemo.txt"))
	c.Step("consume: mamba create from the feed channel", cd(create("t2", name+"=1.0.0", chURL, "mamba")+" && test -f /work/envs/t2/share/kcdemo.txt"))
	c.Step("same-endpoint write then read: ingest 1.0.1, conda resolves latest", Seq(build("1.0.1"), func() error { return ingest("1.0.1") },
		cd(create("t3", name, chURL, "conda")+" && ls /work/pkgs | grep -q "+name+"-1.0.1")))
	c.Step("delete: DELETE ingest/noarch/{file} then repodata no longer lists 1.0.1", func() error {
		if err := c.Expect("DELETE", "/feeds/"+slug+"/conda/ingest/noarch/"+fn("1.0.1"), Basic, 200, 204); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + slug + "/conda/noarch/repodata.json", Auth: Basic, Want: []int{200}})
		if err != nil {
			return err
		}
		if strings.Contains(string(b), fn("1.0.1")) {
			return fmt.Errorf("deleted package still in repodata")
		}
		return nil
	})
	c.Step("delete/unlist: management API unlist then relist 1.0.0", Seq(
		func() error { return c.MgmtVersion(slug, "conda/noarch", "", "unlist") },
		func() error { return c.MgmtVersion(slug, "conda/noarch", "", "relist") }))

	rslug := c.MustFeed("conda", "remote", &Remote{Origin: "https://conda.anaconda.org/conda-test"})
	rsrc := c.LastSource
	rch := c.UserCred("/feeds/" + rslug + "/conda")
	ok := c.Step("remote pull-through miss: conda create font-ttf-inconsolata=2.000 from the conda-test channel via feed", cd(create("r1", "font-ttf-inconsolata=2.000", rch, "conda")+" && ls /work/pkgs | grep -q font-ttf-inconsolata-2.000"))
	c.Step("remote pull-through hit: cached package served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		b, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/conda/noarch/font-ttf-inconsolata-2.000-0.tar.bz2", Auth: Basic, Want: []int{200}})
		if err == nil && len(b) < 5000 {
			err = fmt.Errorf("short package")
		}
		return err
	}, SkipUnless(ok))

	badCh := strings.Replace(chURL, "$TOKEN", "$BADTOKEN", 1)
	c.Step("auth denial: conda create with invalid credentials rejected", cd(create("t9", name+"=1.0.0", badCh, "conda"), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|unauthori|forbidden|authenticat|CondaHTTPError|PackagesNotFound|UnavailableInvalidChannel`)}))
	c.Step("auth denial: unauthenticated repodata returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/conda/noarch/repodata.json", NoAuth, 401, 403)
	})
}
