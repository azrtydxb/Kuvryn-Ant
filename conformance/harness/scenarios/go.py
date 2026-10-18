"""F10 Go module proxy with the real go client (publish by admin ingestion, consume, auth denial, proxy.golang.org pull-through)."""

NAME = "F10"
CLIENTS_USED = ["go"]
GOPROXY_UP = {"origin": "https://proxy.golang.org", "redirect": []}

ZIPPER = r"""package main

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
"""

CLEAN = "chmod -R u+w /work/gopath 2>/dev/null; rm -rf /work/gopath"


def run(c):
    slug = c.feed("go", "local")
    mod = "example.com/kc/demo%s" % c.run_id
    base = "%s/feeds/%s/go" % (c.env.client_base, slug)
    (c.workdir / "zipper").mkdir()
    (c.workdir / "zipper" / "main.go").write_text(ZIPPER)
    # netrc carries the credential for plain-HTTP (loopback) and HTTPS targets; GOINSECURE allows the http proxy URL.
    netrc = 'printf "machine ${HOST%%:*} login service password $TOKEN\\n" > $HOME/.netrc; chmod 600 $HOME/.netrc'
    e = {
        "GOPROXY": c.env.bridge(c.env.token) + "/feeds/%s/go" % slug,
        "GOSUMDB": "off",
        "GOFLAGS": "-mod=mod",
        "GOINSECURE": "*",
        "GOAUTH": "netrc",
        "GOPATH": "/work/gopath",
        "GOTOOLCHAIN": "local",
    }
    build_zipper = (
        "cd /work/zipper && (test -f go.mod || go mod init zipper) && GOPROXY=https://proxy.golang.org GOPATH=/work/gopath-z "
        "go get golang.org/x/mod@latest && GOPROXY=https://proxy.golang.org GOPATH=/work/gopath-z go build -o /work/zipper-bin . && cd /work"
    )

    def mkmod(version, body):
        return (
            '%s && rm -rf m && mkdir -p m && printf "module %s\\n\\ngo 1.22\\n" > m/go.mod && '
            'printf "package demo\\n\\nfunc Answer() int { return %s }\\n" > m/demo.go && '
            '/work/zipper-bin %s %s v.zip m && printf \'{"Version":"%s","Time":"2026-01-01T00:00:00Z"}\' > v.info && cp m/go.mod v.mod'
            % (build_zipper, mod, body, mod, version, version)
        )

    def put(version):
        return (
            'curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -F info=@v.info -F mod=@v.mod -F zip=@v.zip "%s/%s/@v/%s"'
            % (base, mod, version)
        )

    consume = (
        '%s; %s; rm -rf c && mkdir c && cd c && printf "module consumer\\n\\ngo 1.22\\n" > go.mod && '
        'printf "package main\\n\\nimport (\\"fmt\\"\\n\\"%s\\")\\n\\nfunc main() { fmt.Println(\\"answer=\\", demo.Answer()) }\\n" > main.go && '
        "go get %s@VERSION && go run . | tee out.txt" % (netrc, CLEAN, mod, mod)
    )

    c.step("client-version", lambda: c.version("go", "go version"))
    c.step(
        "publish: build module zip and PUT info/mod/zip (admin ingestion; Go has no native publish)",
        lambda: c.client("go", mkmod("v0.1.0", "42") + " && " + put("v0.1.0"), e),
    )
    c.step(
        "consume: go get + go run via GOPROXY feed (fresh module cache)",
        lambda: c.client(
            "go",
            consume.replace("VERSION", "v0.1.0") + ' && grep -q "answer= 42" out.txt',
            e,
        ),
    )
    c.step(
        "same-endpoint write then read: publish v0.1.1; go list -versions and @latest",
        lambda: c.client(
            "go",
            mkmod("v0.1.1", "43")
            + " && "
            + put("v0.1.1")
            + " && "
            + netrc
            + "; "
            + CLEAN
            + "; cd /work/c && "
            'go list -m -versions %s | grep -q "v0.1.0 v0.1.1" && go list -m %s@latest | grep -q v0.1.1'
            % (mod, mod),
            e,
        ),
    )
    c.step(
        "delete/unlist: GOPROXY protocol has no delete operation",
        lambda: None,
        skip_reason="not part of the Go module proxy protocol",
    )
    c.step(
        "auth denial: invalid token rejected by go",
        lambda: c.client(
            "go",
            'printf "machine ${HOST%%:*} login service password invalid-token\\n" > $HOME/.netrc; chmod 600 $HOME/.netrc; '
            + CLEAN
            + "; cd /work/c && go list -m "
            + mod
            + "@v0.1.0",
            dict(e, GOPROXY=c.env.bridge("invalid-token") + "/feeds/%s/go" % slug),
            expect_fail=True,
        ),
    )
    c.step(
        "auth denial: unauthenticated list returns 401",
        lambda: c.http(
            "GET", "/feeds/%s/go/%s/@v/list" % (slug, mod), [401, 403], token=None
        ),
    )

    rslug = c.feed("go", "remote", GOPROXY_UP)
    rsrc = c.last_source
    r = dict(e, GOPROXY=c.env.bridge(c.env.token) + "/feeds/%s/go" % rslug)
    ok = c.step(
        "remote pull-through miss: go mod download github.com/google/uuid@v1.6.0 via feed",
        lambda: c.client(
            "go",
            netrc
            + "; "
            + CLEAN
            + '; rm -rf rc; mkdir -p rc && cd rc && printf "module rc\\n\\ngo 1.22\\n" > go.mod && go mod download github.com/google/uuid@v1.6.0',
            r,
        ),
    )

    def hit():
        c.set_remote_origin(rsrc, "https://127.0.0.1:9", [])
        return c.client(
            "go",
            netrc
            + "; "
            + CLEAN
            + "; cd rc && go mod download github.com/google/uuid@v1.6.0",
            r,
        )

    c.step(
        "remote pull-through hit: fresh module cache download with upstream unreachable",
        hit,
        skip_reason=None if ok else "miss step failed",
    )
