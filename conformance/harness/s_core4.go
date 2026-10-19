package harness

import "strings"

// Docker/OCI (F08), Cargo (F23), Go (F10) and npm (F14) scenarios.

func init() {
	Register(Scenario{Format: "docker", Contract: "F08", Clients: []string{"crane"}, Run: runDocker})
	Register(Scenario{Format: "cargo", Contract: "F23", Clients: []string{"cargo"}, Run: runCargo})
	Register(Scenario{Format: "go", Contract: "F10", Clients: []string{"go"}, Run: runGo})
	Register(Scenario{Format: "npm", Contract: "F14", Clients: []string{"node"}, Run: runNpm})
}

func runDocker(c *Ctx) {
	insec := ""
	if strings.HasPrefix(c.Env.ClientBase, "http://") {
		insec = "--insecure"
	}
	e := RunOpt{Env: map[string]string{"INSEC": insec}}
	fail := RunOpt{Env: e.Env, ExpectFail: true}
	slug := c.MustFeed("oci", "local", nil)
	ref := "$HOST/" + slug + "/demo-" + c.RunID
	login := `echo "$TOKEN" | crane auth login $HOST -u service --password-stdin >/dev/null`
	mkLayer := `mkdir -p l && echo "kuvryn conformance ` + c.RunID + `" > l/fixture.txt && tar -C l -cf layer.tar fixture.txt`
	sh := func(s string, o RunOpt) func() error { return func() error { return c.Sh("crane", s, o) } }

	c.Step("client-version", func() error { return c.Version("crane", "crane version") })
	c.Step("publish: crane append (login + push image)", sh(F(`@login@; @layer@; crane append --oci-empty-base -f layer.tar -t @ref@:v1 $INSEC`, "login", login, "layer", mkLayer, "ref", ref), e))
	c.Step("consume: crane digest/manifest/pull/ls same endpoint", sh(F(`@login@; D=$(crane digest @ref@:v1 $INSEC); echo $D; crane manifest @ref@:v1 $INSEC >/dev/null; crane pull @ref@:v1 out.tar $INSEC && tar tf out.tar | head -3; crane ls @ref@ $INSEC | grep -x v1`, "login", login, "ref", ref), e))
	c.Step("same-endpoint write then read: retag, platform mutate, multi-arch index", sh(F(
		`@login@; crane tag @ref@:v1 v2 $INSEC; test "$(crane digest @ref@:v1 $INSEC)" = "$(crane digest @ref@:v2 $INSEC)"; `+
			`crane mutate @ref@:v1 --set-platform linux/arm64 -t @ref@:arm $INSEC; crane index append -m @ref@:v1 -m @ref@:arm -t @ref@:multi $INSEC; crane manifest @ref@:multi $INSEC | grep -q manifests`,
		"login", login, "ref", ref), e))
	c.Step("delete: crane delete manifest by tag, tag no longer resolves", sh(F(`@login@; crane delete @ref@:v2 $INSEC; ! crane digest @ref@:v2 $INSEC`, "login", login, "ref", ref), e))
	c.Step("delete/unlist: management API unlist then relist", func() error {
		if err := c.MgmtVersion(slug, "demo-"+c.RunID, "", "unlist"); err != nil {
			return err
		}
		return c.MgmtVersion(slug, "demo-"+c.RunID, "", "relist")
	})

	// pull-through against real public upstreams: anonymous mirror.gcr.io and Docker Hub (bearer-token challenge)
	type up struct {
		tag, label string
		r          *Remote
	}
	var slugs []string
	for _, u := range []up{
		{"remote", "mirror.gcr.io", &Remote{Origin: "https://mirror.gcr.io"}},
		{"hub", "registry-1.docker.io", &Remote{Origin: "https://registry-1.docker.io", Redirect: []string{"https://auth.docker.io", "https://production.cloudflare.docker.com", "https://production.cloudfront.docker.com"}}},
	} {
		rslug := c.MustFeed("oci", u.tag, u.r)
		rsrc := c.LastSource
		slugs = append(slugs, rslug)
		rref := "$HOST/" + rslug + "/library/hello-world"
		digest := ""
		ok := c.Step("remote pull-through miss via "+u.label+": crane digest+pull of library/hello-world", func() error {
			if err := c.Sh("crane", login+"; crane digest "+rref+":latest $INSEC", e); err != nil {
				return err
			}
			lines := strings.Split(strings.TrimSpace(c.Out()), "\n")
			digest = lines[len(lines)-1]
			return c.Sh("crane", F(`@login@; crane pull @r@@@d@ out.tar $INSEC; tar tf out.tar | head -3`, "login", login, "r", rref, "d", digest), e)
		})
		c.Step("remote pull-through hit via "+u.label+": pull by digest with upstream unreachable", func() error {
			if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
				return err
			}
			return c.Sh("crane", F(`@login@; crane pull @r@@@d@ out2.tar $INSEC; tar tf out2.tar | head -3`, "login", login, "r", rref, "d", digest), e)
		}, SkipUnless(ok))
	}
	// Denial steps last: failed authentication attempts are rate limited per client address.
	c.Step("auth denial: invalid token rejected by crane", sh(
		`mkdir -p /work/bad; printf '{"auths":{"%s":{"auth":"%s"}}}' "$HOST" "$(printf service:invalid-token | base64)" > /work/bad/config.json; DOCKER_CONFIG=/work/bad crane digest `+ref+`:v1 $INSEC`, fail))
	c.Step("auth denial: bad token and anonymous private-feed reads return 401 (anonymous /v2/ may be 200 for the dockerd handshake)", func() error {
		if err := c.Expect("GET", "/v2/", NoAuth, 200, 401); err != nil {
			return err
		}
		if err := c.Expect("GET", "/v2/", BadToken, 401); err != nil {
			return err
		}
		return c.Expect("GET", "/v2/"+slug+"/demo-"+c.RunID+"/manifests/v1", NoAuth, 401)
	})
	for _, s := range slugs {
		c.Step("auth denial on "+s+": no credentials rejected", func() error {
			return c.Expect("GET", "/v2/"+s+"/library/hello-world/manifests/latest", NoAuth, 401)
		})
	}
}

func runCargo(c *Ctx) {
	slug := c.MustFeed("cargo", "local", nil)
	crate := "kcdemo-" + c.RunID
	idx := c.Env.ClientBase + "/feeds/" + slug + "/cargo/"
	e := map[string]string{
		"CARGO_REGISTRIES_ANT_CREDENTIAL_PROVIDER":  "cargo:token",
		"CARGO_REGISTRIES_ANTR_CREDENTIAL_PROVIDER": "cargo:token",
		"CARGO_REGISTRIES_ANT_INDEX":                "sparse+" + idx,
		"CARGO_REGISTRIES_ANT_TOKEN":                c.Env.Token,
		"CARGO_NET_GIT_FETCH_WITH_CLI":              "true",
		"CARGO_HOME":                                "/work/cargo-home",
		"CARGO_TARGET_DIR":                          "/work/target",
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range e {
			m[a] = b
		}
		m[k] = v
		return m
	}
	mk := func(v string) string {
		return F(`mkdir -p pub && cd pub && printf '[package]\nname = "@c@"\nversion = "@v@"\nedition = "2021"\ndescription = "Kuvryn Ant conformance fixture"\nlicense = "MIT"\npublish = ["ant"]\n' > Cargo.toml && mkdir -p src && echo "pub fn answer() -> u32 { 42 }" > src/lib.rs`, "c", crate, "v", v)
	}
	consumer := func(v string) string {
		return F(`rm -rf cons && mkdir -p cons/src && cd cons && printf '[package]\nname = "consumer"\nversion = "0.1.0"\nedition = "2021"\n[dependencies]\n@c@ = { version = "@v@", registry = "ant" }\n' > Cargo.toml && echo 'fn main(){ println!("answer={}", @m@::answer()); }' > src/main.rs`,
			"c", crate, "v", v, "m", strings.ReplaceAll(crate, "-", "_"))
	}
	cargo := func(s string, env map[string]string) func() error {
		return func() error { return c.Sh("cargo", s, RunOpt{Env: env}) }
	}
	c.Step("client-version", func() error { return c.Version("cargo", "cargo --version") })
	c.Step("publish: cargo publish 0.1.0 to feed", cargo(mk("0.1.0")+" && cargo publish --registry ant --allow-dirty", e))
	c.Step("consume: cargo run with dependency from feed (fresh CARGO_HOME)", cargo("rm -rf /work/cargo-home; "+consumer("0.1.0")+" && cargo run --quiet | grep answer=42", e))
	c.Step("same-endpoint write then read: publish 0.1.1 then consumer resolves it", cargo(
		mk("0.1.1")+" && cargo publish --registry ant --allow-dirty && cd /work && "+consumer("^0.1")+` && cargo update && cargo tree | grep "`+crate+` v0.1.1"`, e))
	c.Step("delete/unlist: cargo yank 0.1.1", cargo("cd pub && cargo yank --registry ant "+crate+"@0.1.1", e))
	c.Step("delete/unlist: cargo yank --undo 0.1.1", cargo("cd pub && cargo yank --undo --registry ant "+crate+"@0.1.1", e))
	c.Step("delete: management API delete 0.1.1", func() error { return c.MgmtVersion(slug, crate, "0.1.1", "delete") })

	rslug := c.MustFeed("cargo", "remote", &Remote{Origin: "https://index.crates.io", Redirect: []string{"https://static.crates.io"}})
	rsrc := c.LastSource
	r := map[string]string{
		"CARGO_REGISTRIES_ANTR_CREDENTIAL_PROVIDER": "cargo:token",
		"CARGO_REGISTRIES_ANTR_INDEX":               "sparse+" + c.Env.ClientBase + "/feeds/" + rslug + "/cargo/",
		"CARGO_REGISTRIES_ANTR_TOKEN":               c.Env.Token,
		"CARGO_HOME":                                "/work/cargo-home-r",
	}
	rcons := `rm -rf rcons && mkdir -p rcons/src && cd rcons && printf '[package]\nname = "rconsumer"\nversion = "0.1.0"\nedition = "2021"\n[dependencies]\nitoa = { version = "=1.0.11", registry = "antr" }\n' > Cargo.toml && echo 'fn main(){ println!("{}", itoa::Buffer::new().format(7u8)); }' > src/main.rs`
	ok := c.Step("remote pull-through miss: cargo fetch/build itoa from crates.io via feed", cargo(rcons+" && cargo run --quiet | grep -x 7", r))
	rr := map[string]string{}
	for k, v := range r {
		rr[k] = v
	}
	rr["CARGO_HOME"] = "/work/cargo-home-r2"
	c.Step("remote pull-through hit: fresh CARGO_HOME fetch with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		return c.Sh("cargo", "rm -rf /work/cargo-home-r2; cd rcons && rm -f Cargo.lock; cargo fetch", RunOpt{Env: rr})
	}, SkipUnless(ok))

	c.Step("auth denial: invalid token rejected by cargo", func() error {
		return c.Sh("cargo", "rm -rf /work/cargo-home; cd /work/cons && cargo fetch", RunOpt{Env: with("CARGO_REGISTRIES_ANT_TOKEN", "invalid-token"), ExpectFail: true})
	})
	c.Step("auth denial: index config.json requires credentials (401)", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/cargo/config.json", NoAuth, 401)
	})
}

const zipperGo = `package main

import (
	"os"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

func main() {
	f, err := os.Create(os.Args[3])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := zip.CreateFromDir(f, module.Version{Path: os.Args[1], Version: os.Args[2]}, os.Args[4]); err != nil {
		panic(err)
	}
}
`

func runGo(c *Ctx) {
	slug := c.MustFeed("go", "local", nil)
	mod := "example.com/kc/demo" + c.RunID
	base := c.Env.ClientBase + "/feeds/" + slug + "/go"
	if err := c.WriteFile("zipper/main.go", []byte(zipperGo)); err != nil {
		panic(err)
	}
	netrc := `printf "machine ${HOST%%:*} login service password $TOKEN\n" > $HOME/.netrc; chmod 600 $HOME/.netrc`
	clean := "chmod -R u+w /work/gopath 2>/dev/null; rm -rf /work/gopath"
	e := map[string]string{"GOPROXY": c.Env.Bridge(c.Env.Token) + "/feeds/" + slug + "/go", "GOSUMDB": "off", "GOFLAGS": "-mod=mod", "GOINSECURE": "*", "GOAUTH": "netrc", "GOPATH": "/work/gopath", "GOTOOLCHAIN": "local"}
	buildZipper := "cd /work/zipper && (test -f go.mod || go mod init zipper) && GOPROXY=https://proxy.golang.org GOPATH=/work/gopath-z go get golang.org/x/mod@latest && GOPROXY=https://proxy.golang.org GOPATH=/work/gopath-z go build -o /work/zipper-bin . && cd /work"
	mkmod := func(v, body string) string {
		return F(buildZipper+` && rm -rf m && mkdir -p m && printf "module @mod@\n\ngo 1.22\n" > m/go.mod && printf "package demo\n\nfunc Answer() int { return @b@ }\n" > m/demo.go && /work/zipper-bin @mod@ @v@ v.zip m && printf '{"Version":"@v@","Time":"2026-01-01T00:00:00Z"}' > v.info && cp m/go.mod v.mod`, "mod", mod, "b", body, "v", v)
	}
	put := func(v string) string {
		return F(`curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -F info=@v.info -F mod=@v.mod -F zip=@v.zip "@base@/@mod@/@v/@v@"`, "base", base, "mod", mod, "v", v)
	}
	consume := func(v string) string {
		return F(netrc+"; "+clean+`; rm -rf c && mkdir c && cd c && printf "module consumer\n\ngo 1.22\n" > go.mod && printf "package main\n\nimport (\"fmt\"\n\"@mod@\")\n\nfunc main() { fmt.Println(\"answer=\", demo.Answer()) }\n" > main.go && go get @mod@@@v@ && go run . | tee out.txt`, "mod", mod, "v", v)
	}
	g := func(s string, env map[string]string) func() error {
		return func() error { return c.Sh("go", s, RunOpt{Env: env}) }
	}
	c.Step("client-version", func() error { return c.Version("go", "go version") })
	c.Step("publish: build module zip and PUT info/mod/zip (admin ingestion; Go has no native publish)", g(mkmod("v0.1.0", "42")+" && "+put("v0.1.0"), e))
	c.Step("consume: go get + go run via GOPROXY feed (fresh module cache)", g(consume("v0.1.0")+` && grep -q "answer= 42" out.txt`, e))
	c.Step("same-endpoint write then read: publish v0.1.1; go list -versions and @latest", g(
		mkmod("v0.1.1", "43")+" && "+put("v0.1.1")+" && "+netrc+"; "+clean+"; cd /work/c && "+F(`go list -m -versions @mod@ | grep -q "v0.1.0 v0.1.1" && go list -m @mod@@latest | grep -q v0.1.1`, "mod", mod), e))
	c.Step("delete: DELETE @v/{version} bundle resource, version disappears from the list", func() error {
		if err := c.Expect("DELETE", "/feeds/"+slug+"/go/"+mod+"/@v/v0.1.1", Bearer, 200, 204); err != nil {
			return err
		}
		out, _, _, err := c.Do(Req{Path: "/feeds/" + slug + "/go/" + mod + "/@v/list", Want: []int{200}})
		if err != nil {
			return err
		}
		if strings.Contains(string(out), "v0.1.1") {
			return Contains("", "v0.1.1 removed from list, got "+string(out))
		}
		return nil
	})
	c.Step("delete/unlist: management API delete v0.1.0", func() error { return c.MgmtVersion(slug, mod, "v0.1.0", "delete") })

	rslug := c.MustFeed("go", "remote", &Remote{Origin: "https://proxy.golang.org"})
	rsrc := c.LastSource
	r := map[string]string{}
	for k, v := range e {
		r[k] = v
	}
	r["GOPROXY"] = c.Env.Bridge(c.Env.Token) + "/feeds/" + rslug + "/go"
	ok := c.Step("remote pull-through miss: go mod download github.com/google/uuid@v1.6.0 via feed", g(
		netrc+"; "+clean+`; rm -rf rc; mkdir -p rc && cd rc && printf "module rc\n\ngo 1.22\n" > go.mod && go mod download github.com/google/uuid@v1.6.0`, r))
	c.Step("remote pull-through hit: fresh module cache download with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		return c.Sh("go", netrc+"; "+clean+"; cd rc && go mod download github.com/google/uuid@v1.6.0", RunOpt{Env: r})
	}, SkipUnless(ok))

	bad := map[string]string{}
	for k, v := range e {
		bad[k] = v
	}
	bad["GOPROXY"] = c.Env.Bridge("invalid-token") + "/feeds/" + slug + "/go"
	c.Step("auth denial: invalid token rejected by go", func() error {
		return c.Sh("go", `printf "machine ${HOST%%:*} login service password invalid-token\n" > $HOME/.netrc; chmod 600 $HOME/.netrc; `+clean+"; cd /work/c && go list -m "+mod+"@v0.1.0", RunOpt{Env: bad, ExpectFail: true})
	})
	c.Step("auth denial: unauthenticated list returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/go/"+mod+"/@v/list", NoAuth, 401, 403)
	})
}

func runNpm(c *Ctx) {
	pnpm := Clients["node"].Pnpm
	slug := c.MustFeed("npm", "local", nil)
	pkg := "@kcdemo/pkg-" + c.RunID
	install := "export PATH=/work/npm-global/bin:$PATH; test -x /work/npm-global/bin/pnpm || npm i -g --prefix /work/npm-global pnpm@" + pnpm + " --registry https://registry.npmjs.org --userconfig /dev/null >/dev/null"
	rc := func(s, tok string) string {
		return F(`printf "registry=$SCHEME://$HOST/feeds/@s@/npm/\n//$HOST/feeds/@s@/npm/:_authToken=@t@\n" > $HOME/.npmrc`, "s", s, "t", tok)
	}
	mk := func(v string) string {
		return F(`rm -rf pub && mkdir pub && cd pub && printf '{"name":"@p@","version":"@v@","main":"index.js","license":"MIT"}' > package.json && echo "module.exports = { answer: 42 };" > index.js`, "p", pkg, "v", v)
	}
	n := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("node", s, o...) } }
	c.Step("client-version", func() error {
		return c.Version("node", install+`; echo "pnpm $(pnpm --version) / node $(node --version) / npm $(npm --version)"`)
	})
	c.Step("publish: pnpm publish 0.1.0", n(install+"; "+rc(slug, "$TOKEN")+"; "+mk("0.1.0")+" && pnpm publish --no-git-checks"))
	c.Step("consume: pnpm add + require from the feed", n(install+"; "+rc(slug, "$TOKEN")+"; "+F(`rm -rf cons && mkdir cons && cd cons && echo '{"name":"cons","version":"1.0.0"}' > package.json && pnpm add @p@@0.1.0 && node -e 'console.log(require("@p@").answer)' | grep -x 42`, "p", pkg)))
	c.Step("same-endpoint write then read: publish 0.1.1 with default (latest) tag", n(install+"; "+rc(slug, "$TOKEN")+"; "+mk("0.1.1")+" && pnpm publish --no-git-checks"),
		Limitation("internal/npmregistry README: initial publish tags immutable; a second version using latest conflicts"))
	c.Step("same-endpoint write then read: publish 0.1.2 with --tag next, then pnpm add @next", n(install+"; "+rc(slug, "$TOKEN")+"; "+mk("0.1.2")+" && pnpm publish --no-git-checks --tag next && cd /work/cons && "+F(`pnpm add @p@@next && grep -q 0.1.2 package.json`, "p", pkg)))
	c.Step("dist-tag: npm dist-tag add, ls and rm", n(install+"; "+rc(slug, "$TOKEN")+"; npm dist-tag add "+pkg+"@0.1.2 beta && npm dist-tag ls "+pkg+" | grep -q beta && npm dist-tag rm "+pkg+" beta && ! (npm dist-tag ls "+pkg+" | grep -q beta)"))
	c.Step("delete/unlist: npm unpublish 0.1.2", n(install+"; "+rc(slug, "$TOKEN")+"; npm unpublish "+pkg+"@0.1.2 --force"))
	c.Step("delete/unlist: management API delete 0.1.0", func() error { return c.MgmtVersion(slug, pkg, "0.1.0", "delete") })

	rslug := c.MustFeed("npm", "remote", &Remote{Origin: "https://registry.npmjs.org"})
	rsrc := c.LastSource
	ok := c.Step("remote pull-through miss: pnpm add left-pad@1.3.0 from registry.npmjs.org via feed", n(install+"; "+rc(rslug, "$TOKEN")+"; "+
		`rm -rf rc && mkdir rc && cd rc && echo '{"name":"rc","version":"1.0.0"}' > package.json && pnpm add left-pad@1.3.0 && node -e 'console.log(require("left-pad")("x",3))' | grep -x "  x"`))
	c.Step("audit: pnpm audit via remote feed (security/advisories/bulk forwarded upstream)", n(install+"; "+rc(rslug, "$TOKEN")+"; cd rc && pnpm audit --audit-level critical"), SkipUnless(ok))
	c.Step("audit: npm audit via remote feed", n(install+"; "+rc(rslug, "$TOKEN")+"; "+`rm -rf au && mkdir au && cd au && echo '{"name":"au","version":"1.0.0"}' > package.json && npm install left-pad@1.3.0 --package-lock-only --no-audit && npm audit --audit-level critical`), SkipUnless(ok))
	c.Step("remote pull-through hit: tarball served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		b, err := c.Get("/feeds/" + rslug + "/npm/left-pad/-/left-pad-1.3.0.tgz")
		if err != nil {
			return err
		}
		if len(b) < 500 {
			return Contains("", "tarball bytes")
		}
		return nil
	}, SkipUnless(ok))

	c.Step("auth denial: invalid token rejected by pnpm view", n(install+"; "+rc(slug, "invalid-token")+"; pnpm view "+pkg+" version", RunOpt{ExpectFail: true}))
	c.Step("auth denial: unauthenticated packument returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/npm/"+pkg, NoAuth, 401, 403)
	})
}
