"""Conformance harness core: API helper, container client runner, evidence recorder.
Stdlib only. Tokens are passed to containers by environment variable NAME (never in argv)
and redacted from every recorded output."""

import base64, json, os, re, subprocess, time, urllib.request, urllib.error, uuid, ssl, shutil, tempfile, pathlib
from urllib.parse import urlparse

HERE = pathlib.Path(__file__).resolve().parent
CLIENTS = json.loads((HERE.parent / "clients.json").read_text())
TAIL = 6000
AUTH_RE = (
    r"401|403|unauthori[sz]ed|authenticat|denied|forbidden|credential|login|permission"
)


class Api:
    def __init__(self, base, token, ca=None):
        self.base, self.token = base.rstrip("/"), token
        self.ctx = ssl.create_default_context(cafile=ca) if ca else None

    def req(self, method, path, body=None, rev=None, token="default"):
        h = {"Content-Type": "application/json", "Idempotency-Key": uuid.uuid4().hex}
        tok = self.token if token == "default" else token
        if tok:
            h["Authorization"] = "Bearer " + tok
        if rev is not None:
            h["If-Match"] = '"%s"' % rev
        r = urllib.request.Request(
            self.base + path,
            None if body is None else json.dumps(body).encode(),
            h,
            method=method,
        )
        try:
            with urllib.request.urlopen(r, timeout=60, context=self.ctx) as resp:
                data = resp.read()
                return (
                    resp.status,
                    dict(resp.headers),
                    (json.loads(data) if data else None),
                )
        except urllib.error.HTTPError as e:
            data = e.read()
            try:
                return e.code, dict(e.headers), json.loads(data)
            except Exception:
                return e.code, dict(e.headers), data

    def ok(self, method, path, body=None, rev=None):
        s, h, d = self.req(method, path, body, rev)
        if s >= 300:
            raise RuntimeError("%s %s -> %s %s" % (method, path, s, str(d)[:400]))
        return d


class Step:
    def __init__(self, name):
        self.name, self.status, self.ms, self.detail, self.output = (
            name,
            "pass",
            0,
            "",
            "",
        )

    def as_dict(self):
        return {
            "name": self.name,
            "status": self.status,
            "duration_ms": self.ms,
            "detail": self.detail,
            "output_tail": self.output,
        }


class Ctx:
    """Per-format scenario context."""

    def __init__(self, env, fmt, evidence):
        self.env, self.fmt, self.ev = env, fmt, evidence
        self.api = env.api
        self.run_id = env.run_id
        self.steps = []
        self.workdir = pathlib.Path(tempfile.mkdtemp(prefix="conf-%s-" % fmt))
        self.workdir.chmod(0o777)
        self._cur_output = ""

    # ---- control plane
    def feed(self, fmt, tag="local", remote=None):
        """Create feed + source (+ remote config). remote={'origin','redirect':[...]}. Returns slug."""
        a = self.api
        slug = ("cf-%s-%s-%s" % (self.fmt, tag, self.run_id))[:60].replace("_", "-")
        feed = a.ok("POST", "/api/v1/feeds", {"slug": slug, "format": fmt})
        src = a.ok(
            "POST",
            "/api/v1/sources",
            {"name": slug, "kind": "remote" if remote else "local", "format": fmt},
        )
        if remote:
            cfg = {
                "origin": remote["origin"],
                "redirect_origins": remote.get("redirect", []),
                "allow_private": False,
                "timeout_seconds": 60,
                "cache": {
                    "metadata_ttl_seconds": 60,
                    "negative_ttl_seconds": 5,
                    "stale_if_error_seconds": 3600,
                    "max_entries": 10000,
                    "max_bytes": 268435456,
                },
            }
            a.ok(
                "PUT",
                "/api/v1/sources/%s/remote-config" % src["id"],
                cfg,
                self._rev("sources", src["id"]),
            )
        a.ok(
            "PUT",
            "/api/v1/feeds/%s/sources/%s" % (feed["id"], src["id"]),
            {"priority": 0},
            self._rev("feeds", feed["id"]),
        )
        if not remote:
            a.ok(
                "PUT",
                "/api/v1/feeds/%s/publish-policy" % feed["id"],
                {"default_source_id": src["id"], "rules": []},
                self._rev("feeds", feed["id"]),
            )
        self.last_feed, self.last_source = feed["id"], src["id"]
        return slug

    def _rev(self, kind, id_):
        return self.api.ok("GET", "/api/v1/%s/%s" % (kind, id_))["revision"]

    def set_remote_origin(self, source_id, origin, redirect=()):
        cfg = self.api.ok("GET", "/api/v1/sources/%s/remote-config" % source_id)
        cfg = cfg.get("config", cfg)
        cfg = {
            k: v
            for k, v in cfg.items()
            if k
            in (
                "origin",
                "redirect_origins",
                "credential_id",
                "ca_ref",
                "allow_private",
                "timeout_seconds",
                "cache",
            )
        }
        cfg["origin"], cfg["redirect_origins"] = origin, list(redirect)
        self.api.ok(
            "PUT",
            "/api/v1/sources/%s/remote-config" % source_id,
            cfg,
            self._rev("sources", source_id),
        )

    # ---- containers
    def client(
        self,
        name,
        script,
        env=None,
        timeout=900,
        expect_fail=False,
        auth=True,
        expect_re=None,
    ):
        timeout = min(timeout, int(os.environ.get("CONF_CLIENT_TIMEOUT", "240")))
        spec = CLIENTS[name]
        cmd = [
            "docker",
            "run",
            "--rm",
            "-v",
            "%s:/work" % self.workdir,
            "-w",
            "/work",
            "--entrypoint",
            spec["shell"],
        ]
        cmd += self.env.net_args()
        u = urlparse(self.env.client_base)
        e = {
            "ANT": self.env.client_base,
            "HOST": u.netloc,
            "SCHEME": u.scheme,
            "RUN": self.run_id,
            "HOME": "/work/home",
        }
        if auth:
            e["TOKEN"] = self.env.token
        e.update(env or {})
        penv = dict(os.environ)
        for k, v in e.items():
            cmd += ["-e", k]
            penv[k] = v
        if self.env.ca:
            cmd += ["-v", "%s:/ca/ca.pem:ro" % self.env.ca]
            for k in (
                "SSL_CERT_FILE",
                "CURL_CA_BUNDLE",
                "NODE_EXTRA_CA_CERTS",
                "REQUESTS_CA_BUNDLE",
                "GIT_SSL_CAINFO",
                "CARGO_HTTP_CAINFO",
            ):
                cmd += ["-e", "%s=/ca/ca.pem" % k]
        cmd += [spec["image"], "-c", "mkdir -p /work/home; " + script]
        name = "conf-%s-%s" % (self.run_id, uuid.uuid4().hex[:6])
        cmd[2:2] = ["--name", name]
        try:
            p = subprocess.run(cmd, env=penv, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired as te:
            subprocess.run(["docker", "rm", "-f", name], capture_output=True)
            partial = self.env.redact(
                ((te.stdout or b"") + (te.stderr or b"")).decode("utf-8", "replace")
            )
            self._cur_output = partial[-TAIL:]
            raise RuntimeError(
                "client timed out after %ds: %s" % (timeout, partial[-1200:])
            )
        out = self.env.redact((p.stdout + p.stderr).decode("utf-8", "replace"))
        self._cur_output = out[-TAIL:]
        if expect_fail:
            if p.returncode == 0:
                raise AssertionError(
                    "expected failure but command succeeded: " + out[-800:]
                )
            # A denial only counts when the output shows an authentication/authorization rejection, not any unrelated failure.
            if not re.search(expect_re or AUTH_RE, out, re.I):
                raise AssertionError(
                    "command failed but not with an auth denial: " + out[-800:]
                )
            return out
        if p.returncode != 0:
            raise RuntimeError("exit %d: %s" % (p.returncode, out[-1800:]))
        return out

    # ---- step recording
    def step(self, name, fn, skip_reason=None, known_unsupported=None):
        s = Step(name)
        self._cur_output = ""
        t = time.time()
        if skip_reason:
            s.status, s.detail = "skip", skip_reason
        else:
            try:
                r = fn()
                if isinstance(r, str):
                    s.detail = self.env.redact(r)[:500]
            except Exception as e:
                s.status = "unsupported" if known_unsupported else "fail"
                s.detail = self.env.redact(str(e))[-2000:]
                if known_unsupported:
                    s.detail = "documented limitation (%s): %s" % (
                        known_unsupported,
                        s.detail,
                    )
            s.output = self._cur_output
        s.ms = int((time.time() - t) * 1000)
        self.steps.append(s)
        mark = {"pass": "PASS", "fail": "FAIL", "skip": "SKIP", "unsupported": "UNSUP"}[
            s.status
        ]
        print(
            "  [%s] %s (%dms)%s"
            % (
                mark,
                name,
                s.ms,
                ""
                if s.status == "pass"
                else " :: " + s.detail[:300].replace("\n", " | "),
            ),
            flush=True,
        )
        return s.status == "pass"

    def version(self, name, cmd):
        out = self.client(name, cmd, auth=False).strip().splitlines()
        v = out[0] if out else ""
        self.ev["clients"][name] = {
            "image": CLIENTS[name]["image"],
            "reported_version": v[:200],
            "pinned_ref": CLIENTS[name]["ref"],
        }
        return v

    def http(self, method, path, want, token="default"):
        h = {}
        tok = self.api.token if token == "default" else token
        if tok:
            h["Authorization"] = "Bearer " + tok
        r = urllib.request.Request(self.api.base + path, headers=h, method=method)
        try:
            with urllib.request.urlopen(r, timeout=60, context=self.api.ctx) as resp:
                code = resp.status
        except urllib.error.HTTPError as e:
            code = e.code
        if code not in (want if isinstance(want, (list, tuple)) else [want]):
            raise AssertionError("%s %s -> %s, want %s" % (method, path, code, want))
        return code

    def cleanup(self):
        shutil.rmtree(self.workdir, ignore_errors=True)


class Env:
    def __init__(self, target, base, token, ca, run_id):
        self.target, self.base, self.token, self.ca, self.run_id = (
            target,
            base.rstrip("/"),
            token,
            ca,
            run_id,
        )
        self.api = Api(base, token, ca)
        self.sidecar = None
        self.loopback = urlparse(base).hostname in ("127.0.0.1", "localhost", "::1")
        self.client_base = self.base

    def start(self):
        if self.loopback:
            port = urlparse(self.base).port
            name = "conf-fwd-%s" % self.run_id
            subprocess.run(
                [
                    "docker",
                    "run",
                    "-d",
                    "--rm",
                    "--name",
                    name,
                    CLIENTS["socat"]["image"],
                    "TCP-LISTEN:%d,fork,reuseaddr" % port,
                    "TCP:host.docker.internal:%d" % port,
                ],
                check=True,
                capture_output=True,
            )
            self.sidecar = name
            time.sleep(1)

    def bridge(self, token):
        """Return a client-reachable base URL that adds the bearer token (for clients like Go that refuse
        credentials over plain HTTP). HTTPS targets return the base with userinfo instead."""
        if self.base.startswith("https://"):
            return self.base.replace("://", "://service:" + token + "@", 1)
        import http.server, threading, urllib.request as ur

        target, tok = self.base, token

        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                r = ur.Request(
                    target + self.path, headers={"Authorization": "Bearer " + tok}
                )
                try:
                    resp = ur.urlopen(r, timeout=120)
                except urllib.error.HTTPError as e:
                    resp = e
                body = resp.read()
                self.send_response(
                    resp.status if hasattr(resp, "status") else resp.code
                )
                for k in ("Content-Type",):
                    if resp.headers.get(k):
                        self.send_header(k, resp.headers[k])
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            do_HEAD = do_GET

            def log_message(self, *a):
                pass

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        port = srv.server_address[1]
        self.bridge_servers = getattr(self, "bridge_servers", []) + [srv]
        return "http://%s:%d" % ("host.docker.internal", port)

    def stop(self):
        for n in getattr(self, "bridges", []):
            subprocess.run(["docker", "rm", "-f", n], capture_output=True)
        if self.sidecar:
            subprocess.run(["docker", "rm", "-f", self.sidecar], capture_output=True)

    def net_args(self):
        return (
            ["--network", "container:" + self.sidecar]
            if self.sidecar
            else ["--add-host", "host.docker.internal:host-gateway"]
        )

    def redact(self, s):
        for t in {
            self.token,
            base64.b64encode(("service:" + self.token).encode()).decode(),
            base64.b64encode((":" + self.token).encode()).decode(),
        }:
            if t:
                s = s.replace(t, "***REDACTED***")
        return s
