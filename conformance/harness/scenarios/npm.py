"""F14 npm registry protocol with pnpm (publish, install, same-endpoint, unpublish, auth denial, registry.npmjs.org pull-through)."""

from core import CLIENTS

NAME = "F14"
CLIENTS_USED = ["node"]
NPMJS = {"origin": "https://registry.npmjs.org", "redirect": []}


def run(c):
    pnpm = CLIENTS["node"]["pnpm"]
    slug = c.feed("npm", "local")
    pkg = "@kcdemo/pkg-%s" % c.run_id
    reg = "$SCHEME://$HOST/feeds/%s/npm/" % slug
    # pnpm is installed once from the public registry into the shared workdir, never through the Ant feed under test.
    install = (
        "export PATH=/work/npm-global/bin:$PATH; test -x /work/npm-global/bin/pnpm || npm i -g --prefix /work/npm-global pnpm@%s "
        "--registry https://registry.npmjs.org --userconfig /dev/null >/dev/null" % pnpm
    )

    def rc(slug_, token="$TOKEN"):
        return (
            'printf "registry=$SCHEME://$HOST/feeds/%s/npm/\\n//$HOST/feeds/%s/npm/:_authToken=%s\\n" > $HOME/.npmrc'
            % (slug_, slug_, token)
        )

    def mk(version):
        return (
            'rm -rf pub && mkdir pub && cd pub && printf \'{"name":"%s","version":"%s","main":"index.js","license":"MIT"}\' > package.json && '
            'echo "module.exports = { answer: 42 };" > index.js' % (pkg, version)
        )

    c.step(
        "client-version",
        lambda: c.version(
            "node",
            '%s; echo "pnpm $(pnpm --version) / node $(node --version) / npm $(npm --version)"'
            % install,
        ),
    )
    c.step(
        "publish: pnpm publish 0.1.0",
        lambda: c.client(
            "node",
            "%s; %s; %s && pnpm publish --no-git-checks"
            % (install, rc(slug), mk("0.1.0")),
        ),
    )
    c.step(
        "consume: pnpm add + require from the feed",
        lambda: c.client(
            "node",
            '%s; %s; rm -rf cons && mkdir cons && cd cons && echo \'{"name":"cons","version":"1.0.0"}\' > package.json && pnpm add %s@0.1.0 && '
            "node -e 'console.log(require(\"%s\").answer)' | grep -x 42"
            % (install, rc(slug), pkg, pkg),
        ),
    )
    c.step(
        "same-endpoint write then read: publish 0.1.1 with default (latest) tag",
        lambda: c.client(
            "node",
            "%s; %s; %s && pnpm publish --no-git-checks"
            % (install, rc(slug), mk("0.1.1")),
        ),
        known_unsupported="internal/npmregistry README: initial publish tags immutable; a second version using latest conflicts",
    )
    c.step(
        "same-endpoint write then read: publish 0.1.2 with --tag next, then pnpm add @next",
        lambda: c.client(
            "node",
            "%s; %s; %s && pnpm publish --no-git-checks --tag next && cd /work/cons && pnpm add %s@next && grep -q 0.1.2 package.json"
            % (install, rc(slug), mk("0.1.2"), pkg),
        ),
    )
    c.step(
        "delete/unlist: npm unpublish 0.1.2",
        lambda: c.client(
            "node", "%s; %s; npm unpublish %s@0.1.2 --force" % (install, rc(slug), pkg)
        ),
        known_unsupported="internal/npmregistry README: unpublish returns 501",
    )
    c.step(
        "auth denial: invalid token rejected by pnpm view",
        lambda: c.client(
            "node",
            "%s; %s; pnpm view %s version" % (install, rc(slug, "invalid-token"), pkg),
            expect_fail=True,
        ),
    )
    c.step(
        "auth denial: unauthenticated packument returns 401",
        lambda: c.http(
            "GET",
            "/feeds/%s/npm/%s" % (slug, pkg.replace("/", "%2f")),
            [401, 403],
            token=None,
        ),
    )

    rslug = c.feed("npm", "remote", NPMJS)
    rsrc = c.last_source
    ok = c.step(
        "remote pull-through miss: pnpm add left-pad@1.3.0 from registry.npmjs.org via feed",
        lambda: c.client(
            "node",
            '%s; %s; rm -rf rc && mkdir rc && cd rc && echo \'{"name":"rc","version":"1.0.0"}\' > package.json && pnpm add left-pad@1.3.0 && '
            'node -e \'console.log(require("left-pad")("x",3))\' | grep -x "  x"'
            % (install, rc(rslug)),
        ),
    )

    def hit():
        c.set_remote_origin(rsrc, "https://127.0.0.1:9", [])
        return c.client(
            "curl",
            'curl -fsS -o /work/lp.tgz -H "Authorization: Bearer $TOKEN" "$ANT/feeds/%s/npm/left-pad/-/left-pad-1.3.0.tgz" && tar tzf /work/lp.tgz | head -3'
            % rslug,
        )

    c.step(
        "remote pull-through hit: tarball served with upstream unreachable",
        hit,
        skip_reason=None if ok else "miss step failed",
    )
