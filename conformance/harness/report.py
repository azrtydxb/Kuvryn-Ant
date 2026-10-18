#!/usr/bin/env python3
"""Generate SUMMARY.md from the <format>.json evidence files in a results directory."""

import json, pathlib, sys

d = pathlib.Path(sys.argv[1])
rows, detail = [], []
for f in sorted(d.glob("*.json")):
    e = json.loads(f.read_text())
    s = e["summary"]
    clients = ", ".join(
        "%s `%s`" % (k, v.get("reported_version") or v["image"])
        for k, v in e["clients"].items()
    )
    rows.append(
        "| %s | %s | %d | %d | %d | %d | %s |"
        % (
            e["format"],
            e["result"].upper(),
            s["pass"],
            s["fail"],
            s["unsupported"],
            s["skip"],
            clients,
        )
    )
    bad = [st for st in e["steps"] if st["status"] in ("fail", "unsupported")]
    if bad:
        detail.append("### %s" % e["format"])
        for st in bad:
            detail.append(
                "- **%s** `%s`: %s"
                % (
                    st["status"],
                    st["name"],
                    st["detail"].strip().replace("\n", " ")[:400],
                )
            )
first = json.loads(next(d.glob("*.json")).read_text()) if rows else {}
out = (
    [
        "# Conformance summary",
        "",
        "Target: `%s`  Ant: `%s`  Date: %s"
        % (d.parent.name, first.get("ant", {}).get("version"), d.name),
        "",
        "| Format | Result | Pass | Fail | Documented limitation | Skip | Clients |",
        "|---|---|---|---|---|---|---|",
    ]
    + rows
    + [""]
    + detail
)
(d / "SUMMARY.md").write_text("\n".join(out) + "\n")
print("wrote", d / "SUMMARY.md")
