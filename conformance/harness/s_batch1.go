package harness

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// PyPI (F19), Maven (F13), NuGet (F15), Raw (F21), Helm (F11).

func init() {
	Register(Scenario{Format: "pypi", Contract: "F19", Clients: []string{"python"}, Run: runPypi})
	Register(Scenario{Format: "maven", Contract: "F13", Clients: []string{"maven"}, Run: runMaven})
	Register(Scenario{Format: "nuget", Contract: "F15", Clients: []string{"dotnet"}, Run: runNuget})
	Register(Scenario{Format: "raw", Contract: "F21", Clients: []string{"curl", "cargo"}, Run: runRaw})
	Register(Scenario{Format: "helm", Contract: "F11", Clients: []string{"helm"}, Run: runHelm})
}

// UserCred returns the client-side base URL with embedded service credentials ($TOKEN expanded by the shell).
func (c *Ctx) UserCred(p string) string {
	return strings.Replace(c.Env.ClientBase, "://", "://service:$TOKEN@", 1) + p
}

func (c *Ctx) isHTTP() bool { return strings.HasPrefix(c.Env.ClientBase, "http://") }

func hostOnly(base string) string {
	u, _ := url.Parse(base)
	return u.Hostname()
}

// ---- PyPI

const pySetup = "test -x /work/v/bin/twine || (python -m venv /work/v && /work/v/bin/pip install -q pip==26.2.1 && /work/v/bin/pip install -q twine==7.0.0 build setuptools wheel); export PATH=/work/v/bin:$PATH; "

func runPypi(c *Ctx) {
	slug := c.MustFeed("pypi", "local", nil)
	name := "kcdemo-" + c.RunID
	mod := strings.ReplaceAll(name, "-", "_")
	idx := c.UserCred("/feeds/" + slug + "/pypi/simple/")
	th := ""
	if c.isHTTP() {
		th = "--trusted-host $HOSTNAME_ONLY"
	}
	e := RunOpt{Env: map[string]string{"HOSTNAME_ONLY": hostOnly(c.Env.ClientBase)}}
	upload := "$ANT/feeds/" + slug + "/pypi/"
	// The default setuptools METADATA of a project without a long description has no blank-line terminator after the
	// headers; the adapter rejects that (see the "edge" step), so the main fixture carries a README body.
	build := func(v string, readme bool, pname string) string {
		pmod := strings.ReplaceAll(pname, "-", "_")
		rd := ""
		if readme {
			rd = `\nreadme = "README.md"`
		}
		return F(`rm -rf pkg dist && mkdir -p pkg/@m@ && cd pkg && echo 'ANSWER = 42' > @m@/__init__.py && echo 'conformance fixture readme' > README.md && `+
			`printf '[build-system]\nrequires = ["setuptools"]\nbuild-backend = "setuptools.build_meta"\n[project]\nname = "@n@"\nversion = "@v@"\nrequires-python = ">=3.8"\ndescription = "conformance fixture"@rd@\n[tool.setuptools]\npackages = ["@m@"]\n' > pyproject.toml && python -m build --no-isolation -q . >/dev/null && cd ..`,
			"m", pmod, "n", pname, "v", v, "rd", rd)
	}
	py := func(s string, o ...RunOpt) func() error {
		return func() error {
			opt := e
			if len(o) > 0 {
				opt = o[0]
				opt.Env = e.Env
			}
			return c.Sh("python", pySetup+s, opt)
		}
	}
	twine := " && twine upload --non-interactive --repository-url " + upload + " -u service -p $TOKEN pkg/dist/*"
	installIn := func(venv, extra string) string {
		return F(`rm -rf /work/@v@ && python -m venv /work/@v@ && /work/@v@/bin/pip install --no-cache-dir @th@ --index-url @idx@ @x@`, "v", venv, "th", th, "idx", idx, "x", extra)
	}
	c.Step("client-version", func() error {
		return c.Version("python", pySetup+`echo "pip $(pip --version | cut -d" " -f2) / twine $(twine --version | head -1) / $(python --version)"`)
	})
	c.Step("publish: twine upload wheel + sdist 0.1.0", py(build("0.1.0", true, name)+twine))
	c.Step("consume: pip install pinned 0.1.0 from the feed and import", py(installIn("t1", name+"==0.1.0")+F(` && /work/t1/bin/python -c 'import @m@; print(@m@.ANSWER)' | grep -x 42`, "m", mod)))
	c.Step("same-endpoint write then read: twine upload 0.1.1, pip resolves latest", py(build("0.1.1", true, name)+twine+
		F(` && /work/t1/bin/pip install --no-cache-dir -U @th@ --index-url @idx@ @n@ && /work/t1/bin/pip show @n@ | grep -x 'Version: 0.1.1'`, "th", th, "idx", idx, "n", name)))
	c.Step("edge: twine upload of a default setuptools wheel without long description", py(build("0.1.0", false, "kcdemo-nodesc-"+c.RunID)+" && twine upload --non-interactive --repository-url "+upload+" -u service -p $TOKEN pkg/dist/*.whl"))
	// F19: API only. Yank (PEP 592), pip skips the yanked release, restore, delete the files.
	whl, sdist := mod+"-0.1.1-py3-none-any.whl", mod+"-0.1.1.tar.gz"
	fbase := "/feeds/" + slug + "/pypi/files/" + name
	each := func(method, suffix string, want ...int) func() error {
		return func() error {
			for _, f := range []string{whl, sdist} {
				if err := c.Expect(method, fbase+"/"+f+suffix, Bearer, want...); err != nil {
					return err
				}
			}
			return nil
		}
	}
	pick := func(venv string) func() error {
		return py(installIn(venv, name) + F(` && /work/@v@/bin/pip show @n@ | grep -x 'Version: 0.1.0'`, "v", venv, "n", name))
	}
	c.Step("delete/unlist: yank 0.1.1 wheel and sdist (PUT files/.../yanked)", each("PUT", "/yanked", 200, 204))
	c.Step("delete/unlist: pip skips yanked 0.1.1 and resolves 0.1.0", pick("t2"))
	c.Step("delete/unlist: restore yank (DELETE .../yanked)", each("DELETE", "/yanked", 200, 204))
	c.Step("delete: DELETE files/{project}/{file} for the 0.1.1 wheel and sdist", each("DELETE", "", 200, 204))
	c.Step("delete: deleted 0.1.1 no longer resolvable (pip picks 0.1.0)", pick("t3"))
	c.Step("delete/unlist: management API unlist then relist", Seq(
		func() error { return c.MgmtVersion(slug, name, "", "unlist") },
		func() error { return c.MgmtVersion(slug, name, "", "relist") }))

	rslug := c.MustFeed("pypi", "remote", &Remote{Origin: "https://pypi.org", Redirect: []string{"https://files.pythonhosted.org"}})
	rsrc := c.LastSource
	ridx := c.UserCred("/feeds/" + rslug + "/pypi/simple/")
	ok := c.Step("remote pull-through miss: pip install six==1.16.0 from pypi.org via feed", py(
		F(`rm -rf /work/r1 && python -m venv /work/r1 && /work/r1/bin/pip install --no-cache-dir @th@ --index-url @idx@ six==1.16.0 && /work/r1/bin/python -c 'import six; print(six.__version__)' | grep -x 1.16.0`, "th", th, "idx", ridx)))
	c.Step("remote pull-through hit: cached wheel served by feed URL with upstream unreachable", func() error {
		// The index is a mutable document in the metadata cache (invalidated by re-pointing the origin), so the cached
		// artifact is read by its feed URL: that is the immutable pull-through cache under test.
		b, _, _, err := c.Do(Req{Path: "/feeds/" + rslug + "/pypi/simple/six/", Hdr: map[string]string{"Accept": "application/vnd.pypi.simple.v1+json"}, Want: []int{200}})
		if err != nil {
			return err
		}
		var doc struct {
			Files []struct{ Filename, URL string } `json:"files"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return err
		}
		furl := ""
		for _, f := range doc.Files {
			if f.Filename == "six-1.16.0-py2.py3-none-any.whl" {
				furl = f.URL
			}
		}
		if furl == "" {
			return fmt.Errorf("six-1.16.0 wheel not in the feed index")
		}
		p := furl
		if !strings.HasPrefix(furl, "/") {
			p = path.Clean("/feeds/" + rslug + "/pypi/simple/six/" + furl)
		}
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: p, Want: []int{200}})
		if err == nil && len(data) < 5000 {
			err = fmt.Errorf("short wheel (%d bytes)", len(data))
		}
		return err
	}, SkipUnless(ok))

	badIdx := strings.Replace(idx, "$TOKEN", "$BADTOKEN", 1)
	bad := RunOpt{Env: map[string]string{"HOSTNAME_ONLY": hostOnly(c.Env.ClientBase), "BADTOKEN": "invalid-token"}, ExpectFail: true}
	c.Step("auth denial: pip with invalid token rejected", py(F(`/work/t1/bin/pip install --no-cache-dir @th@ --index-url @idx@ @n@==0.1.0`, "th", th, "idx", badIdx, "n", name), bad))
	c.Step("auth denial: twine upload with invalid token rejected", py(build("0.9.0", true, name)+" && twine upload --non-interactive --repository-url "+upload+" -u service -p $BADTOKEN pkg/dist/*", bad))
	c.Step("auth denial: unauthenticated simple index returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/pypi/simple/"+name+"/", NoAuth, 401, 403)
	})
}

// ---- Maven

const mvnSetup = "mkdir -p $HOME/.m2; printf '<settings><servers><server><id>ant</id><username>service</username><password>${env.TOKEN}</password></server></servers></settings>' > $HOME/.m2/settings.xml; " +
	"if [ -f /ca/ca.pem ]; then keytool -importcert -noprompt -cacerts -storepass changeit -alias antca -file /ca/ca.pem >/dev/null 2>&1; fi; "

func runMaven(c *Ctx) {
	slug := c.MustFeed("maven", "local", nil)
	aid := "demo-" + c.RunID
	g := "org.kcdemo"
	repo := "$ANT/feeds/" + slug + "/maven"
	gpath := "/feeds/" + slug + "/maven/org/kcdemo/" + aid
	pom := func(v string) string {
		return F(`<?xml version="1.0"?><project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>@g@</groupId><artifactId>@a@</artifactId><version>@v@</version><packaging>jar</packaging></project>`, "g", g, "a", aid, "v", v)
	}
	jar := func(v string) []byte {
		return ZipBytes([]string{"META-INF/MANIFEST.MF", "kcdemo/answer.txt"}, map[string]string{
			"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\r\nCreated-By: kuvryn-conformance\r\n\r\n", "kcdemo/answer.txt": "42 " + v + "\n"})
	}
	stage := func(v string) error {
		if err := c.WriteFile("demo-"+v+".pom", []byte(pom(v))); err != nil {
			return err
		}
		return c.WriteFile("demo-"+v+".jar", jar(v))
	}
	bundle := func(v string) error {
		if err := stage(v); err != nil {
			return err
		}
		body, ct := Multipart(Part{Name: "asset", Filename: aid + "-" + v + ".pom", ContentType: "application/xml", Data: []byte(pom(v))},
			Part{Name: "asset", Filename: aid + "-" + v + ".jar", ContentType: "application/java-archive", Data: jar(v)})
		_, _, _, err := c.Do(Req{Method: "POST", Path: gpath + "/" + v + "/_bundle", Body: body, Hdr: map[string]string{"Content-Type": ct}, Auth: Basic, Want: []int{201, 204}})
		return err
	}
	get := func(v, local, rid, u string, env map[string]string) error {
		return c.Sh("maven", mvnSetup+F(`mvn -B -ntp -s /work/home/.m2/settings.xml org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get -Dartifact=@g@:@a@:@v@ -DremoteRepositories=@rid@::default::@u@ -Dtransitive=false -Dmaven.repo.local=@l@ && test -f @l@/org/kcdemo/@a@/@v@/@a@-@v@.jar`,
			"g", g, "a", aid, "v", v, "rid", rid, "u", u, "l", local), RunOpt{Env: env})
	}
	c.Step("client-version", func() error { return c.Version("maven", "mvn -v | head -1") })
	if err := stage("1.0"); err != nil {
		panic(err)
	}
	c.Step("publish: mvn deploy:deploy-file (native multi-PUT)", func() error {
		return c.Sh("maven", mvnSetup+"mvn -B -ntp -s /work/home/.m2/settings.xml org.apache.maven.plugins:maven-deploy-plugin:3.1.4:deploy-file -Dfile=/work/demo-1.0.jar -DpomFile=/work/demo-1.0.pom -DrepositoryId=ant -Durl="+repo)
	}, Limitation("internal/maven README: native Maven deploy multi-PUT returns 405; release bundles via POST version/_bundle"))
	c.Step("publish: POST version/_bundle (adapter bundle API) 1.0", func() error { return bundle("1.0") })
	c.Step("consume: mvn dependency:get 1.0 with fresh local repo", func() error { return get("1.0", "/tmp/m2-a", "ant", repo, nil) })
	c.Step("same-endpoint write then read: bundle 1.1, maven-metadata.xml lists it, mvn resolves 1.1", Seq(
		func() error { return bundle("1.1") },
		func() error {
			b, err := c.Get(gpath + "/maven-metadata.xml")
			if err != nil {
				return err
			}
			return Contains(string(b), "1.1")
		},
		func() error { return get("1.1", "/tmp/m2-b", "ant", repo, nil) }))
	c.Step("delete: DELETE version directory 1.1 then mvn cannot resolve it", Seq(
		func() error { return c.Expect("DELETE", gpath+"/1.1/", Basic, 200, 204) },
		func() error {
			return c.Sh("maven", mvnSetup+F(`! mvn -B -ntp -s /work/home/.m2/settings.xml org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get -Dartifact=@g@:@a@:1.1 -DremoteRepositories=ant::default::@r@ -Dtransitive=false -Dmaven.repo.local=/tmp/m2-c 2>&1`, "g", g, "a", aid, "r", repo))
		}))
	c.Step("delete/unlist: management API unlist then relist 1.0", Seq(
		func() error { return c.MgmtVersion(slug, aid, "1.0", "unlist") },
		func() error { return c.MgmtVersion(slug, aid, "1.0", "relist") }))

	rslug := c.MustFeed("maven", "remote", &Remote{Origin: "https://repo1.maven.org/maven2"})
	rsrc := c.LastSource
	rrepo := "$ANT/feeds/" + rslug + "/maven"
	cc := func(local string) error {
		return c.Sh("maven", mvnSetup+F(`mvn -B -ntp -s /work/home/.m2/settings.xml org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get -Dartifact=commons-codec:commons-codec:1.16.1 -DremoteRepositories=ant::default::@r@ -Dtransitive=false -Dmaven.repo.local=@l@ && test -f @l@/commons-codec/commons-codec/1.16.1/commons-codec-1.16.1.jar`, "r", rrepo, "l", local))
	}
	ok := c.Step("remote pull-through miss: mvn dependency:get commons-codec 1.16.1 from Maven Central via feed", func() error { return cc("/tmp/m2-r1") })
	c.Step("remote pull-through hit: fresh local repo resolves with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		return cc("/tmp/m2-r2")
	}, SkipUnless(ok))

	c.Step("auth denial: mvn with invalid token rejected", func() error {
		return c.Sh("maven", mvnSetup+F(`mvn -B -ntp -s /work/home/.m2/settings.xml org.apache.maven.plugins:maven-dependency-plugin:3.8.1:get -Dartifact=@g@:@a@:1.0 -DremoteRepositories=ant::default::@r@ -Dtransitive=false -Dmaven.repo.local=/tmp/m2-d`, "g", g, "a", aid, "r", repo),
			RunOpt{Env: map[string]string{"TOKEN": "invalid-token"}, ExpectFail: true, ExpectRe: regexp.MustCompile(`(?i)401|403|Unauthorized|Forbidden|not authorized|authentication`)})
	})
	c.Step("auth denial: unauthenticated pom returns 401", func() error {
		return c.Expect("GET", gpath+"/1.0/"+aid+"-1.0.pom", NoAuth, 401, 403)
	})
}

// ---- NuGet

func runNuget(c *Ctx) {
	slug := c.MustFeed("nuget", "local", nil)
	pid := "Kc.Demo" + c.RunID
	lower := strings.ToLower(pid)
	src := "$ANT/feeds/" + slug + "/nuget/v3/index.json"
	insecure := ""
	if c.isHTTP() {
		insecure = "--allow-insecure-connections"
	}
	env := RunOpt{Env: map[string]string{"DOTNET_CLI_TELEMETRY_OPTOUT": "1", "DOTNET_NOLOGO": "1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE": "1", "NUGET_PACKAGES": "/work/pk-global"}}
	cfgf := func(n string) string {
		if n == "ant" {
			return "$HOME/.nuget/NuGet/NuGet.Config"
		}
		return "/work/nuget-" + n + ".config"
	}
	addsrc := func(name, u, pw string) string {
		return F(`mkdir -p $HOME/.nuget/NuGet; rm -f @c@; printf '<configuration><packageSources><clear/></packageSources></configuration>' > @c@; dotnet nuget add source @u@ -n @n@ -u service -p @pw@ --store-password-in-clear-text @i@ --configfile @c@ >/dev/null; `,
			"c", cfgf(name), "u", u, "n", name, "pw", pw, "i", insecure)
	}
	pack := func(v string) string {
		return F(`rm -rf /work/lib && mkdir -p /work/lib && cd /work/lib && dotnet new classlib -n @p@ --framework net10.0 -o . >/dev/null && dotnet pack -c Release -p:PackageVersion=@v@ -p:Description=conformance -p:Authors=kuvryn -o /work/out >/dev/null && ls /work/out`, "p", pid, "v", v)
	}
	push := func(v, key string) string {
		return F(`dotnet nuget push /work/out/@p@.@v@.nupkg -s ant -k @k@ @i@`, "p", pid, "v", v, "s", "", "k", key, "i", insecure)
	}
	restore := func(name, pkg, v, pk string) string {
		return F(`rm -rf /work/cons && mkdir -p /work/cons && cd /work/cons && printf '<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup><ItemGroup><PackageReference Include="@p@" Version="@v@"/></ItemGroup></Project>' > cons.csproj && dotnet restore --configfile @c@ --packages @pk@ --no-http-cache`,
			"p", pkg, "v", v, "c", cfgf(name), "pk", pk)
	}
	dn := func(s string, o ...RunOpt) func() error {
		return func() error {
			opt := env
			if len(o) > 0 {
				opt = o[0]
				for k, v := range env.Env {
					if _, set := opt.Env[k]; !set {
						if opt.Env == nil {
							opt.Env = map[string]string{}
						}
						opt.Env[k] = v
					}
				}
			}
			return c.Sh("dotnet", s, opt)
		}
	}
	c.Step("client-version", func() error { return c.Version("dotnet", "dotnet --version") })
	// `dotnet pack` writes the .nuspec with a UTF-8 byte-order mark, which the adapter's manifest parser rejects (HTTP 400).
	// The first step records that; the following steps repack the identical package without the BOM to exercise the rest.
	stripBOM := func(v string) error {
		f := "out/" + pid + "." + v + ".nupkg"
		data, err := c.ReadFile(f)
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return err
		}
		var out bytes.Buffer
		zw := zip.NewWriter(&out)
		for _, e := range zr.File {
			rc, err := e.Open()
			if err != nil {
				return err
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return err
			}
			if strings.HasSuffix(e.Name, ".nuspec") {
				b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
			}
			w, err := zw.Create(e.Name)
			if err != nil {
				return err
			}
			if _, err = w.Write(b); err != nil {
				return err
			}
		}
		if err = zw.Close(); err != nil {
			return err
		}
		return c.WriteFile(f, out.Bytes())
	}
	// NuGet clients start every request unauthenticated and retry after the 401 challenge, and the instance counts each
	// challenged request as a failed authentication (10 per minute per address, then 429 for everyone), so every client
	// step starts after the window has drained.
	cool := func(f func() error) func() error {
		return func() error { time.Sleep(65 * time.Second); return f() }
	}
	apiPush := func(v string) error {
		data, err := c.ReadFile("out/" + pid + "." + v + ".nupkg")
		if err != nil {
			return err
		}
		body, ct := Multipart(Part{Name: "package", Filename: "package.nupkg", ContentType: "application/octet-stream", Data: data})
		_, _, _, err = c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/nuget/v2/package/", Body: body, Hdr: map[string]string{"Content-Type": ct, "X-NuGet-ApiKey": c.Env.Token}, Auth: Basic, Want: []int{200, 201, 202}})
		return err
	}
	c.Step("publish: dotnet pack + dotnet nuget push 1.0.0 (unmodified pack output)", cool(dn(addsrc("ant", src, "$TOKEN")+pack("1.0.0")+" && "+push("1.0.0", "$TOKEN"))))
	c.Step("publish: repack 1.0.0 without the nuspec BOM, then dotnet nuget push", Seq(func() error { return stripBOM("1.0.0") }, cool(dn(addsrc("ant", src, "$TOKEN")+push("1.0.0", "$TOKEN")))))
	c.Step("publish: PUT v2/package/ multipart with the BOM-free package (direct API, independent of the client)", func() error { return apiPush("1.0.0") })
	c.Step("consume: dotnet restore 1.0.0 from the feed into a fresh package folder", cool(dn(addsrc("ant", src, "$TOKEN")+restore("ant", pid, "1.0.0", "/work/pk1")+" && test -d /work/pk1/"+lower+"/1.0.0")))
	c.Step("same-endpoint write then read: publish 1.0.1, restore 1.0.* resolves it", Seq(dn(pack("1.0.1")), func() error { return stripBOM("1.0.1") }, func() error { return apiPush("1.0.1") },
		cool(dn(addsrc("ant", src, "$TOKEN")+restore("ant", pid, "1.0.*", "/work/pk2")+" && test -d /work/pk2/"+lower+"/1.0.1"))))
	c.Step("delete: dotnet nuget delete 1.0.1 (native), restore of 1.0.1 then fails", cool(dn(
		F(`dotnet nuget delete @p@ 1.0.1 -s ant -k $TOKEN --non-interactive && `, "p", pid)+addsrc("ant", src, "$TOKEN")+"! ("+restore("ant", pid, "1.0.1", "/work/pk3")+")")))
	c.Step("delete/unlist: management API unlist then relist 1.0.0", Seq(
		func() error { return c.MgmtVersion(slug, lower, "", "unlist") },
		func() error { return c.MgmtVersion(slug, lower, "", "relist") }))

	rslug := c.MustFeed("nuget", "remote", &Remote{Origin: "https://api.nuget.org", Redirect: []string{"https://globalcdn.nuget.org"}})
	rsrc := c.LastSource
	rurl := "$ANT/feeds/" + rslug + "/nuget/v3/index.json"
	ok := c.Step("remote pull-through miss: dotnet restore Newtonsoft.Json 13.0.3 from api.nuget.org via feed", cool(dn(addsrc("rem", rurl, "$TOKEN")+restore("rem", "Newtonsoft.Json", "13.0.3", "/work/pk-r1")+" && test -d /work/pk-r1/newtonsoft.json/13.0.3")))
	c.Step("remote pull-through hit: cached nupkg served with upstream unreachable", func() error {
		b, err := c.Get("/feeds/" + rslug + "/nuget/v3/index.json")
		if err != nil {
			return err
		}
		var doc struct {
			Resources []struct {
				ID   string `json:"@id"`
				Type string `json:"@type"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return err
		}
		base := ""
		for _, r := range doc.Resources {
			if strings.HasPrefix(r.Type, "PackageBaseAddress") {
				base = r.ID
			}
		}
		u, err := url.Parse(base)
		if err != nil || base == "" {
			return fmt.Errorf("no PackageBaseAddress in the feed service index")
		}
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: u.Path + "newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg", Want: []int{200}})
		if err == nil && len(data) < 100000 {
			err = fmt.Errorf("short nupkg")
		}
		return err
	}, SkipUnless(ok))

	badEnv := RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}
	c.Step("auth denial: dotnet nuget push with invalid API key rejected", dn(addsrc("ant", src, "$TOKEN")+push("1.0.0", "$BADTOKEN"), badEnv))
	c.Step("auth denial: dotnet restore with invalid credentials rejected", dn(addsrc("bad", src, "$BADTOKEN")+restore("bad", pid, "1.0.0", "/work/pk4"), badEnv))
	c.Step("auth denial: unauthenticated service index returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/nuget/v3/index.json", NoAuth, 401, 403)
	})
}

// ---- Raw

const sccacheSetup = "test -x /work/bin/sccache || (mkdir -p /work/bin && cd /work && A=$(uname -m) && curl -fsSL https://github.com/mozilla/sccache/releases/download/v0.17.0/sccache-v0.17.0-$A-unknown-linux-musl.tar.gz | tar xz && cp sccache-v0.17.0-$A-unknown-linux-musl/sccache /work/bin/); export PATH=/work/bin:$PATH; "

func runRaw(c *Ctx) {
	slug := c.MustFeed("raw", "local", nil)
	base := "$ANT/feeds/" + slug + "/raw"
	auth := `-H "Authorization: Bearer $TOKEN"`
	key := "kcdemo/" + c.RunID + "/payload.bin"
	cu := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("curl", s, o...) } }
	c.Step("client-version", func() error { return c.Version("curl", "curl --version | head -1") })
	c.Step("publish: curl PUT "+key, cu(F(`head -c 4096 /dev/urandom > payload.bin; curl -fsS -X PUT @a@ --data-binary @payload.bin @b@/@k@ -o /dev/null -w '%{http_code}\n'`, "a", auth, "b", base, "k", key)))
	c.Step("consume: curl GET returns identical bytes, HEAD and Range work", cu(F(`curl -fsS @a@ @b@/@k@ -o got.bin && cmp got.bin payload.bin && curl -fsSI @a@ @b@/@k@ | head -1 && curl -fsS @a@ -r 0-9 @b@/@k@ | wc -c | grep -x 10`, "a", auth, "b", base, "k", key)))
	c.Step("same-endpoint write then read: PUT a second key and read it back", cu(F(`echo second-@r@ > second.txt; curl -fsS -X PUT @a@ --data-binary @second.txt @b@/kcdemo/@r@/second.txt && curl -fsS @a@ @b@/kcdemo/@r@/second.txt | grep -x second-@r@`, "r", c.RunID, "a", auth, "b", base)))
	c.Step("immutability: PUT different bytes at an existing key conflicts (409)", cu(F(`echo other > other.bin; test "$(curl -s -o /dev/null -w '%{http_code}' -X PUT @a@ --data-binary @other.bin @b@/@k@)" = 409`, "a", auth, "b", base, "k", key)))
	c.Step("sccache WebDAV: cold compile writes, warm compile hits the feed", func() error {
		return c.Sh("cargo", sccacheSetup+F(`export SCCACHE_WEBDAV_ENDPOINT=@b@ SCCACHE_WEBDAV_KEY_PREFIX=sccache-@r@ SCCACHE_WEBDAV_TOKEN=$TOKEN RUSTC_WRAPPER=sccache CARGO_INCREMENTAL=0 CARGO_TARGET_DIR=/work/tgt SCCACHE_DIR=/work/sccdir; `+
			`sccache --stop-server >/dev/null 2>&1; rm -rf proj && cargo new --lib proj -q && cd proj && cargo build -q && sccache --show-stats | head -20 && cargo clean -q && sccache --stop-server >/dev/null 2>&1; cargo build -q && sccache --show-stats | grep -E 'Cache hits +[1-9]'`, "b", base, "r", c.RunID))
	})
	c.Step("delete: curl DELETE key then GET returns 404", cu(F(`curl -fsS -X DELETE @a@ @b@/@k@ -o /dev/null && test "$(curl -s -o /dev/null -w '%{http_code}' @a@ @b@/@k@)" = 404`, "a", auth, "b", base, "k", key)))
	c.Step("delete/unlist: management API delete of second.txt", func() error { return c.MgmtVersion(slug, "second.txt", "", "delete") })

	rslug := c.MustFeed("raw", "remote", &Remote{Origin: "https://raw.githubusercontent.com"})
	rsrc := c.LastSource
	p := "golang/go/go1.22.0/LICENSE"
	ok := c.Step("remote pull-through miss: curl GET raw.githubusercontent.com/"+p+" via feed", cu(F(`curl -fsS @a@ $ANT/feeds/@s@/raw/@p@ -o lic1.txt && grep -q 'Redistribution and use' lic1.txt`, "a", auth, "s", rslug, "p", p)))
	c.Step("remote pull-through hit: same bytes served with upstream unreachable", func() error {
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		return c.Sh("curl", F(`curl -fsS @a@ $ANT/feeds/@s@/raw/@p@ -o lic2.txt && cmp lic1.txt lic2.txt`, "a", auth, "s", rslug, "p", p))
	}, SkipUnless(ok))

	c.Step("auth denial: invalid token rejected by curl", cu(F(`curl -fsS -H "Authorization: Bearer $BADTOKEN" @b@/kcdemo/@r@/second.txt`, "b", base, "r", c.RunID), RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}))
	c.Step("auth denial: unauthenticated PUT rejected", func() error {
		_, _, _, err := c.Do(Req{Method: "PUT", Path: "/feeds/" + slug + "/raw/denied.txt", Body: []byte("x"), Auth: NoAuth, Want: []int{401, 403}})
		return err
	})
}

// ---- Helm

func runHelm(c *Ctx) {
	slug := c.MustFeed("helm", "local", nil)
	chart := "kcdemo" + c.RunID
	plain := ""
	if c.isHTTP() {
		plain = "--plain-http"
	}
	ca := ""
	if c.Env.CA != "" {
		ca = "--ca-file /ca/ca.pem"
	}
	repoURL := "$ANT/feeds/" + slug + "/helm"
	h := func(s string, o ...RunOpt) func() error { return func() error { return c.Sh("helm", s, o...) } }
	pkg := func(v string) string {
		return F(`rm -rf /work/@c@ && cd /work && helm create @c@ >/dev/null && helm package @c@ --version @v@ --app-version @v@ -d /work/pkgs >/dev/null`, "c", chart, "v", v)
	}
	repoAdd := func(name, u string) string {
		return F(`helm repo add @n@ @u@ --username service --password $TOKEN --pass-credentials @ca@ >/dev/null && helm repo update @n@ >/dev/null; `, "n", name, "u", u, "ca", ca)
	}
	publish := func(v string) error {
		data, err := c.ReadFile("pkgs/" + chart + "-" + v + ".tgz")
		if err != nil {
			return err
		}
		body, ct := Multipart(Part{Name: "chart", Filename: chart + "-" + v + ".tgz", ContentType: "application/gzip", Data: data})
		_, _, _, err = c.Do(Req{Method: "POST", Path: "/feeds/" + slug + "/helm/charts/" + chart + "/" + v, Body: body, Hdr: map[string]string{"Content-Type": ct}, Auth: Basic, Want: []int{200, 201, 204}})
		return err
	}
	c.Step("client-version", func() error { return c.Version("helm", "helm version --short") })
	c.Step("publish: helm package, then POST charts/{name}/{version} (ChartMuseum-style)", Seq(h(pkg("0.1.0")), func() error { return publish("0.1.0") }))
	c.Step("consume: helm repo add/update, search, pull and template the chart", h(repoAdd("ant", repoURL)+
		F(`helm search repo ant/@c@ --versions | grep 0.1.0 && rm -rf /work/pulled && mkdir -p /work/pulled && helm pull ant/@c@ --version 0.1.0 -d /work/pulled --pass-credentials @ca@ && ls /work/pulled/@c@-0.1.0.tgz && helm template x /work/pulled/@c@-0.1.0.tgz >/dev/null`, "c", chart, "ca", ca)))
	c.Step("same-endpoint write then read: publish 0.1.1, helm repo update, pull latest", Seq(h(pkg("0.1.1")), func() error { return publish("0.1.1") },
		h(repoAdd("ant", repoURL)+F(`rm -rf /work/pulled2 && mkdir -p /work/pulled2 && helm pull ant/@c@ -d /work/pulled2 --pass-credentials @ca@ && ls /work/pulled2/@c@-0.1.1.tgz`, "c", chart, "ca", ca))))
	c.Step("delete: DELETE charts/{name}/{version} then index no longer lists 0.1.1", Seq(
		func() error {
			return c.Expect("DELETE", "/feeds/"+slug+"/helm/charts/"+chart+"/0.1.1", Basic, 200, 204)
		},
		h(repoAdd("ant", repoURL)+F(`! (helm search repo ant/@c@ --versions | grep 0.1.1)`, "c", chart))))
	c.Step("delete/unlist: management API unlist then relist 0.1.0", Seq(
		func() error { return c.MgmtVersion(slug, chart, "0.1.0", "unlist") },
		func() error { return c.MgmtVersion(slug, chart, "0.1.0", "relist") }))

	// helm push / pull against an OCI feed.
	oslug := c.MustFeed("oci", "helmoci", nil)
	login := F(`echo "$TOKEN" | helm registry login $HOST -u service --password-stdin @p@ @ca@ >/dev/null; `, "p", plain, "ca", ca)
	c.Step("helm push: registry login and helm push oci://", h(login+pkg("0.2.0")+F(` && helm push /work/pkgs/@c@-0.2.0.tgz oci://$HOST/@s@ @p@ @ca@`, "c", chart, "s", oslug, "p", plain, "ca", ca)))
	c.Step("helm pull oci:// of the pushed chart", h(login+F(`rm -rf /work/ocipull && mkdir -p /work/ocipull && helm pull oci://$HOST/@s@/@c@ --version 0.2.0 -d /work/ocipull @p@ @ca@ && ls /work/ocipull/@c@-0.2.0.tgz`, "s", oslug, "c", chart, "p", plain, "ca", ca)))

	rslug := c.MustFeed("helm", "remote", &Remote{Origin: "https://stefanprodan.github.io/podinfo", Redirect: []string{"https://github.com", "https://objects.githubusercontent.com", "https://release-assets.githubusercontent.com"}})
	rsrc := c.LastSource
	ok := c.Step("remote pull-through miss: helm pull podinfo 6.15.0 from stefanprodan.github.io/podinfo via feed", h(repoAdd("rem", "$ANT/feeds/"+rslug+"/helm")+
		F(`rm -rf /work/rp && mkdir -p /work/rp && helm pull rem/podinfo --version 6.15.0 -d /work/rp --pass-credentials @ca@ && ls /work/rp/podinfo-6.15.0.tgz`, "ca", ca)))
	c.Step("remote pull-through hit: cached chart served with upstream unreachable", func() error {
		idx, err := c.Get("/feeds/" + rslug + "/helm/index.yaml")
		if err != nil {
			return err
		}
		m := regexp.MustCompile(`(/feeds/\S+podinfo-6\.15\.0\.tgz|charts/podinfo/6\.15\.0/\S+podinfo-6\.15\.0\.tgz)`).FindStringSubmatch(string(idx))
		if m == nil {
			return fmt.Errorf("chart url not in feed index: %.300s", idx)
		}
		p := m[1]
		if !strings.HasPrefix(p, "/") {
			p = "/feeds/" + rslug + "/helm/" + p
		}
		if err := c.SetRemoteOrigin(rsrc, Unreachable); err != nil {
			return err
		}
		data, _, _, err := c.Do(Req{Path: p, Want: []int{200}})
		if err == nil && len(data) < 200 {
			err = fmt.Errorf("short chart")
		}
		return err
	}, SkipUnless(ok))

	badEnv := RunOpt{Env: map[string]string{"BADTOKEN": "invalid-token"}, ExpectFail: true}
	c.Step("auth denial: helm repo add with invalid credentials rejected", h(F(`helm repo add bad @u@ --username service --password $BADTOKEN --pass-credentials @ca@`, "u", repoURL, "ca", ca), badEnv))
	// `helm registry login` only pings /v2/, which answers 200 without credentials on the live instances, so the denial is
	// checked on the pull that follows the login.
	c.Step("auth denial: helm pull oci:// with invalid credentials rejected", h(F(`echo "$BADTOKEN" | helm registry login $HOST -u service --password-stdin @p@ @ca@; rm -rf /work/badpull && mkdir -p /work/badpull && helm pull oci://$HOST/@s@/@c@ --version 0.2.0 -d /work/badpull @p@ @ca@`, "p", plain, "ca", ca, "s", oslug, "c", chart), badEnv))
	c.Step("auth denial: unauthenticated index.yaml returns 401", func() error {
		return c.Expect("GET", "/feeds/"+slug+"/helm/index.yaml", NoAuth, 401, 403)
	})
}
