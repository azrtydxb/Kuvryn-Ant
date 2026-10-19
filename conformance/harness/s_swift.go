package harness

// Swift Package Manager registry (F24).

func runSwift(c *Ctx) {
	slug := c.MustFeed("swift", "local", nil)
	scope, name := "kcdemo", "Lib"+c.RunID
	reg := c.Env.ClientBase + "/feeds/" + slug + "/swift"
	insec := ""
	httpOnly := c.isHTTP()
	if httpOnly {
		insec = "--allow-insecure-http"
	}
	const accept = "application/vnd.swift.registry.v1+json"
	sw := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("swift", s, o...) } }
	login := func(tok string) string {
		s := F(`mkdir -p /work/scratch; (command -v zip >/dev/null || (apt-get update -qq && apt-get install -y -qq zip) >/dev/null 2>&1); printf "machine %s login service password %s\n" "${HOST%%:*}" "@t@" > $HOME/.netrc; chmod 600 $HOME/.netrc; swift package-registry set @r@ --global @i@ >/dev/null; `, "r", reg, "t", tok, "i", insec)
		if !httpOnly {
			s += F(`swift package-registry login @r@ --token "@t@" --no-confirm >/dev/null; `, "r", reg, "t", tok)
		}
		return s
	}
	mkpkg := `rm -rf /work/lib && mkdir -p /work/lib/Sources/@n@ && cd /work/lib && printf '// swift-tools-version:5.9\nimport PackageDescription\nlet package = Package(name: "@n@", products: [.library(name: "@n@", targets: ["@n@"])], targets: [.target(name: "@n@")])\n' > Package.swift && echo 'public let answer = 42' > Sources/@n@/a.swift && git init -q . && git config user.email a@b.c && git config user.name a && git add -A && git commit -qm init && git tag 1.0.0 && `
	manifest := F("// swift-tools-version:5.9\nimport PackageDescription\nlet package = Package(name: \"@n@\", products: [.library(name: \"@n@\", targets: [\"@n@\"])], targets: [.target(name: \"@n@\")])\n", "n", name)
	archive := func() []byte {
		return ZipBytes([]string{name + "/Package.swift", name + "/Sources/" + name + "/a.swift"}, map[string]string{name + "/Package.swift": manifest, name + "/Sources/" + name + "/a.swift": "public let answer = 42\n"})
	}
	apiPublish := func(v string) error {
		body, ct := Multipart(Part{Name: "source-archive", Filename: name + "-" + v + ".zip", ContentType: "application/zip", Data: archive()},
			Part{Name: "metadata", ContentType: "application/json", Data: []byte(`{"description":"conformance fixture","author":{"name":"kuvryn"}}`)})
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/swift/" + scope + "/" + name + "/" + v, Body: body, Hdr: map[string]string{"Content-Type": ct, "Accept": accept}, Auth: Basic, Want: []int{200, 201}})
		return err
	}
	get := func(p string) ([]byte, error) {
		b, _, _, err := c.Do(Req{Path: "/feeds/" + slug + "/swift/" + p, Hdr: map[string]string{"Accept": accept}, Auth: Basic, Want: []int{200}})
		return b, err
	}
	httpSkip := ""
	if httpOnly {
		httpSkip = "SwiftPM does not send registry credentials over plain HTTP; this target is http, so the native client steps run only on https targets"
	}
	c.Step("client-version", func() error { return c.Version("swift", "swift --version 2>&1 | head -1") })
	c.Step("publish: swift package-registry publish "+scope+"."+name+" 1.0.0 (native client)", sw(login("$TOKEN")+F(mkpkg, "n", name)+F(`swift package-registry publish @s@.@n@ 1.0.0 --url @r@ @i@ --scratch-directory /work/scratch`, "s", scope, "n", name, "r", reg, "i", insec)), Skip(httpSkip))
	c.Step("publish: PUT {scope}/{name}/{version} multipart (source-archive + metadata) 1.0.1", func() error { return apiPublish("1.0.1") })
	c.Step("consume: swift package resolve with an id: dependency from the registry", sw(login("$TOKEN")+F(`rm -rf /work/cons && mkdir -p /work/cons/Sources/App && cd /work/cons && printf '// swift-tools-version:5.9\nimport PackageDescription\nlet package = Package(name: "App", dependencies: [.package(id: "@s@.@n@", from: "1.0.0")], targets: [.executableTarget(name: "App", dependencies: [.product(name: "@n@", package: "@s@.@n@")])])\n' > Package.swift && echo 'print(1)' > Sources/App/main.swift && swift package resolve && swift package show-dependencies | grep @n@`, "s", scope, "n", name)), Skip(httpSkip))
	c.Step("same-endpoint write then read: release list, metadata and source archive over the registry API", func() error {
		b, err := get(scope + "/" + name)
		if err != nil {
			return err
		}
		if err := Contains(string(b), "1.0.1"); err != nil {
			return err
		}
		if _, err = get(scope + "/" + name + "/1.0.1"); err != nil {
			return err
		}
		z, _, _, err := c.Do(Req{Path: "/feeds/" + slug + "/swift/" + scope + "/" + name + "/1.0.1.zip", Hdr: map[string]string{"Accept": "application/vnd.swift.registry.v1+zip"}, Auth: Basic, Want: []int{200}})
		if err == nil && len(z) < 100 {
			err = Contains("", "zip bytes")
		}
		return err
	})
	c.Step("delete/unlist: management API unlist then relist 1.0.1", Seq(
		func() error { return c.MgmtVersion(slug, "kcdemo.lib", "1.0.1", "unlist") },
		func() error { return c.MgmtVersion(slug, "kcdemo.lib", "1.0.1", "relist") }))
	c.Step("delete: DELETE {scope}/{name}/{version}", func() error {
		return c.Expect("DELETE", "/feeds/"+slug+"/swift/"+scope+"/"+name+"/1.0.1", Basic, 200, 204)
	})
	c.Step("remote pull-through: swift registry adapter has no remote source support", func() error { return nil }, Skip("not supported: internal/swiftrepo has no PullThrough hook (docs/implementation/pull-through.md lists no swift)"))
	c.Step("auth denial: package resolve with invalid credentials rejected", sw(login("$BADTOKEN")+`cd /work/cons && swift package resolve`, RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: authOrFail}), Skip(httpSkip))
	c.Step("auth denial: unauthenticated release list returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/swift/"+scope+"/"+name, NoAuth, 400, 401, 403)
	})
}
