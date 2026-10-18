"""F08 Docker/OCI registry with crane (publish, consume, delete, auth denial, pull-through)."""

NAME = "F08"
CLIENTS_USED = ["crane"]
MIRROR = {"origin": "https://mirror.gcr.io", "redirect": []}
HUB = {
    "origin": "https://registry-1.docker.io",
    "redirect": ["https://production.cloudflare.docker.com"],
}


def run(c):
    insec = "--insecure" if c.env.client_base.startswith("http://") else ""
    e = {"INSEC": insec}
    slug = c.feed("oci", "local")
    ref = "$HOST/%s/demo-%s" % (slug, c.run_id)
    login = (
        'echo "$TOKEN" | crane auth login $HOST -u service --password-stdin >/dev/null'
    )
    mk_layer = (
        'mkdir -p l && echo "kuvryn conformance %s" > l/fixture.txt && tar -C l -cf layer.tar fixture.txt'
        % c.run_id
    )

    c.step("client-version", lambda: c.version("crane", "crane version"))
    c.step(
        "publish: crane append (login + push image)",
        lambda: c.client(
            "crane",
            "%s; %s; crane append --oci-empty-base -f layer.tar -t %s:v1 $INSEC"
            % (login, mk_layer, ref),
            e,
        ),
    )
    c.step(
        "consume: crane digest/manifest/pull/ls same endpoint",
        lambda: c.client(
            "crane",
            "%s; D=$(crane digest %s:v1 $INSEC); echo $D; crane manifest %s:v1 $INSEC >/dev/null; "
            "crane pull %s:v1 out.tar $INSEC && tar tf out.tar | head -3; crane ls %s $INSEC | grep -x v1"
            % (login, ref, ref, ref, ref),
            e,
        ),
    )
    c.step(
        "same-endpoint write then read: retag, platform mutate, multi-arch index",
        lambda: c.client(
            "crane",
            '%s; crane tag %s:v1 v2 $INSEC; test "$(crane digest %s:v1 $INSEC)" = "$(crane digest %s:v2 $INSEC)"; '
            "crane mutate %s:v1 --set-platform linux/arm64 -t %s:arm $INSEC; crane index append -m %s:v1 -m %s:arm -t %s:multi $INSEC; "
            "crane manifest %s:multi $INSEC | grep -q manifests"
            % (login, ref, ref, ref, ref, ref, ref, ref, ref, ref),
            e,
        ),
    )
    c.step(
        "auth denial: invalid token rejected by crane",
        lambda: c.client(
            "crane",
            'mkdir -p /work/bad; printf \'{"auths":{"%s":{"auth":"%s"}}}\' "$HOST" "$(printf service:invalid-token | base64)" > /work/bad/config.json; '
            "DOCKER_CONFIG=/work/bad crane digest " + ref + ":v1 $INSEC",
            e,
            expect_fail=True,
        ),
    )
    c.step(
        "auth denial: /v2/ without or with bad token returns 401",
        lambda: (
            (
                c.http("GET", "/v2/", 401, token=None),
                c.http("GET", "/v2/", 401, token="invalid-token"),
            )
            and None
        ),
    )
    c.step(
        "delete: crane delete manifest by tag",
        lambda: c.client(
            "crane",
            "%s; crane delete %s:v2 $INSEC; ! crane digest %s:v2 $INSEC"
            % (login, ref, ref),
            e,
        ),
        known_unsupported="README: unsupported lifecycle operations fail explicitly",
    )

    # pull-through against real public upstreams: anonymous mirror.gcr.io and Docker Hub (bearer-token challenge)
    for tag, remote, label in (
        ("remote", MIRROR, "mirror.gcr.io"),
        ("hub", HUB, "registry-1.docker.io"),
    ):
        rslug = c.feed("oci", tag, remote)
        rsrc = c.last_source
        rref = "$HOST/%s/library/hello-world" % rslug
        state = {}

        def miss(rref=rref, state=state):
            out = c.client(
                "crane", "%s; crane digest %s:latest $INSEC" % (login, rref), e
            )
            state["digest"] = out.strip().splitlines()[-1]
            c.client(
                "crane",
                "%s; crane pull %s@%s out.tar $INSEC; tar tf out.tar | head -3"
                % (login, rref, state["digest"]),
                e,
            )
            return state["digest"]

        def hit(rref=rref, state=state, rsrc=rsrc):
            c.set_remote_origin(rsrc, "https://127.0.0.1:9", [])
            c.client(
                "crane",
                "%s; crane pull %s@%s out2.tar $INSEC; tar tf out2.tar | head -3"
                % (login, rref, state["digest"]),
                e,
            )
            return "served with upstream origin unreachable"

        ok = c.step(
            "remote pull-through miss via %s: crane digest+pull of library/hello-world"
            % label,
            miss,
        )
        c.step(
            "remote pull-through hit via %s: pull by digest with upstream unreachable"
            % label,
            hit,
            skip_reason=None if ok else "miss step failed",
        )
        c.step(
            "auth denial on %s feed: no credentials rejected" % tag,
            lambda rslug=rslug: c.http(
                "GET",
                "/v2/%s/library/hello-world/manifests/latest" % rslug,
                401,
                token=None,
            ),
        )
