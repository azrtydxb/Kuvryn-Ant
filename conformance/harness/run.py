#!/usr/bin/env python3
"""Conformance runner: run.py <target> [format ...]. Configuration via environment:
ANT_BASE_URL, ANT_TOKEN or ANT_TOKEN_FILE, optional ANT_CA_BUNDLE (private CA), ANT_VERSION."""

import datetime, importlib, json, os, pathlib, subprocess, sys, time, uuid, traceback

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import core  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parents[1]
ORDER = [
    "docker",
    "cargo",
    "go",
    "npm",
    "raw",
    "helm",
    "oras",
    "pypi",
    "maven",
    "nuget",
]


def available():
    return sorted(
        p.stem
        for p in (ROOT / "harness" / "scenarios").glob("*.py")
        if not p.stem.startswith("_")
    )


def main():
    if len(sys.argv) < 2:
        raise SystemExit(
            "usage: run.py <target> [formats...]; available: " + " ".join(available())
        )
    target, want = sys.argv[1], sys.argv[2:]
    base = os.environ.get("ANT_BASE_URL")
    token = os.environ.get("ANT_TOKEN")
    if not token and os.environ.get("ANT_TOKEN_FILE"):
        token = pathlib.Path(os.environ["ANT_TOKEN_FILE"]).read_text().strip()
        if token.startswith("{"):  # credential JSON: use the secret (or token) field
            doc = json.loads(token)
            token = doc.get("secret") or doc.get("token") or ""
    if not base or not token:
        raise SystemExit("ANT_BASE_URL and ANT_TOKEN (or ANT_TOKEN_FILE) are required")
    ca = os.environ.get("ANT_CA_BUNDLE") or None
    if ca:
        ca = str(pathlib.Path(ca).resolve())
    avail = available()
    formats = want or [f for f in ORDER if f in avail] + [
        f for f in avail if f not in ORDER
    ]
    run_id = uuid.uuid4().hex[:6]
    env = core.Env(target, base, token, ca, run_id)
    date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
    out_dir = ROOT / "results" / target / date
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        sha = subprocess.run(
            ["git", "-C", str(ROOT), "rev-parse", "--short", "HEAD"],
            capture_output=True,
            text=True,
        ).stdout.strip()
    except Exception:
        sha = ""
    status = env.api.req("GET", "/api/v1/status")[2]
    ant = {
        "api_version": status.get("api_version") if isinstance(status, dict) else None,
        "version": os.environ.get("ANT_VERSION") or ("git:" + sha),
        "target_base_url": base,
    }
    env.start()
    try:
        # Pre-pull pinned client images so pull time never counts against (or times out) scenario steps.
        for fmt in formats:
            if fmt in avail:
                mod = importlib.import_module("scenarios." + fmt)
                for client in getattr(mod, "CLIENTS_USED", []) + ["curl"]:
                    img = core.CLIENTS[client]["image"]
                    print("pull", img, flush=True)
                    subprocess.run(
                        ["docker", "pull", "-q", img], capture_output=True, timeout=1800
                    )
        for fmt in formats:
            if fmt not in avail:
                print("no scenario for", fmt)
                continue
            print("== %s" % fmt, flush=True)
            ev = {
                "format": fmt,
                "target": target,
                "run_id": run_id,
                "started": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                "ant": ant,
                "clients": {},
                "steps": [],
            }
            ctx = core.Ctx(env, fmt, ev)
            t = time.time()
            try:
                importlib.import_module("scenarios." + fmt).run(ctx)
            except Exception:
                ctx.step(
                    "scenario-setup",
                    lambda: (_ for _ in ()).throw(
                        RuntimeError(traceback.format_exc()[-1500:])
                    ),
                )
            ev["duration_ms"] = int((time.time() - t) * 1000)
            ev["steps"] = [s.as_dict() for s in ctx.steps]
            counts = {
                k: sum(1 for s in ctx.steps if s.status == k)
                for k in ("pass", "fail", "skip", "unsupported")
            }
            ev["summary"] = counts
            ev["result"] = (
                "fail" if counts["fail"] else ("pass" if counts["pass"] else "skip")
            )
            text = env.redact(json.dumps(ev, indent=2))
            assert token not in text
            (out_dir / (fmt + ".json")).write_text(text + "\n")
            ctx.cleanup()
    finally:
        env.stop()
    subprocess.run(
        [sys.executable, str(ROOT / "harness" / "report.py"), str(out_dir)], check=False
    )


if __name__ == "__main__":
    main()
