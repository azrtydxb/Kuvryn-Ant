import { describe, expect, it } from "vitest";
import { brandIcons } from "./brandIcons";
import { formats } from "./generated";
import {
  defaultIcons,
  iconForSource,
  matchUpstream,
  originHost,
  presetDraft,
  presetsFor,
  upstreams,
} from "./upstreams";

const idOf = (origin: string, format?: (typeof formats)[number]) =>
  matchUpstream(origin, format)?.id;

describe("upstream catalog matching", () => {
  it("matches Docker Hub by its registry host and its aliases", () => {
    for (const host of [
      "https://registry-1.docker.io",
      "https://docker.io",
      "https://index.docker.io/v1/",
      "https://REGISTRY.HUB.DOCKER.COM",
    ])
      expect(idOf(host, "oci"), host).toBe("dockerhub");
  });

  it("matches the OCI registries", () => {
    expect(idOf("https://ghcr.io")).toBe("ghcr");
    expect(idOf("https://quay.io")).toBe("quay");
    expect(idOf("https://gcr.io")).toBe("gcr");
    expect(idOf("https://eu.gcr.io")).toBe("gcr");
    expect(idOf("https://europe-west1-docker.pkg.dev")).toBe(
      "artifact-registry",
    );
    expect(idOf("https://public.ecr.aws")).toBe("ecr-public");
    expect(idOf("https://registry.k8s.io")).toBe("k8s-registry");
    expect(idOf("https://mcr.microsoft.com")).toBe("mcr");
    expect(idOf("https://registry.gitlab.com")).toBe("gitlab-registry");
    expect(idOf("https://nvcr.io")).toBe("ngc");
  });

  it("matches language and OS repositories", () => {
    const cases: [string, string][] = [
      ["https://registry.npmjs.org", "npm"],
      ["https://pypi.org/simple", "pypi"],
      ["https://files.pythonhosted.org", "pypi"],
      ["https://repo1.maven.org/maven2", "maven-central"],
      ["https://repo.maven.apache.org/maven2", "maven-central"],
      ["https://maven.google.com", "google-maven"],
      ["https://plugins.gradle.org/m2", "gradle-plugins"],
      ["https://proxy.golang.org", "go-proxy"],
      ["https://sum.golang.org", "go-proxy"],
      ["https://index.crates.io", "crates"],
      ["https://static.crates.io", "crates"],
      ["https://api.nuget.org", "nuget"],
      ["https://repo.packagist.org", "packagist"],
      ["https://rubygems.org", "rubygems"],
      ["https://conda.anaconda.org/conda-forge", "conda-forge"],
      ["https://center2.conan.io", "conancenter"],
      ["https://cloud.r-project.org", "cran"],
      ["https://pub.dev", "pub"],
      ["https://cdn.cocoapods.org", "cocoapods"],
      ["https://registry.terraform.io", "terraform"],
      ["https://galaxy.ansible.com/api/", "ansible-galaxy"],
      ["https://deb.debian.org/debian", "debian"],
      ["https://archive.ubuntu.com/ubuntu", "ubuntu"],
      ["https://dl.fedoraproject.org/pub", "fedora"],
      ["https://dl.rockylinux.org/pub/rocky", "rocky"],
      ["https://repo.almalinux.org/almalinux", "alma"],
      ["https://mirror.stream.centos.org", "centos"],
      ["https://dl-cdn.alpinelinux.org/alpine", "alpine"],
      ["https://huggingface.co", "huggingface"],
      ["https://cdn-lfs-us-1.hf.co", "huggingface"],
      ["https://cas-bridge.xethub.hf.co", "huggingface"],
      ["https://github.com", "github-releases"],
      ["https://objects.githubusercontent.com", "github-releases"],
      ["https://raw.githubusercontent.com", "github-raw"],
      ["https://artifacthub.io", "artifacthub"],
    ];
    for (const [origin, id] of cases) expect(idOf(origin), origin).toBe(id);
  });

  it("does not let a wildcard match its bare domain or lookalikes", () => {
    expect(idOf("https://quay.io.evil.example")).toBeUndefined();
    expect(idOf("https://notquay.io")).toBeUndefined();
    expect(idOf("https://docker.io.example.com")).toBeUndefined();
    expect(idOf("https://evil-docker.pkg.dev.example.com")).toBeUndefined();
  });

  it("returns nothing for unknown, empty or malformed origins", () => {
    expect(matchUpstream("https://artifacts.example.com")).toBeUndefined();
    expect(matchUpstream("")).toBeUndefined();
    expect(matchUpstream(undefined)).toBeUndefined();
    expect(matchUpstream("not a url")).toBeUndefined();
    expect(originHost("https://Example.COM:8443/x")).toBe("example.com");
  });

  it("prefers the upstream that serves the source format", () => {
    expect(matchUpstream("https://github.com", "raw")?.id).toBe(
      "github-releases",
    );
    expect(matchUpstream("https://ghcr.io", "oci")?.id).toBe("ghcr");
  });
});

describe("upstream catalog contents", () => {
  it("has unique ids and valid brand icons", () => {
    const seen = new Set<string>();
    for (const u of upstreams) {
      expect(seen.has(u.id), u.id).toBe(false);
      seen.add(u.id);
      if ("brand" in u.icon)
        expect(brandIcons[u.icon.brand], `${u.id} icon`).toBeDefined();
      for (const f of u.formats) expect(formats).toContain(f);
    }
  });

  it("has well-formed presets", () => {
    for (const u of upstreams) {
      const p = u.preset;
      if (!p) continue;
      expect(p.name, u.id).toMatch(/^[a-z0-9][a-z0-9-]*-remote$/);
      if (p.origin) expect(p.origin, u.id).toMatch(/^https:\/\/[^/\s]+/);
      expect(p.redirect_origins.length, u.id).toBeLessThanOrEqual(16);
      expect(new Set(p.redirect_origins).size, u.id).toBe(
        p.redirect_origins.length,
      );
      for (const r of p.redirect_origins)
        expect(r, u.id).toMatch(/^https:\/\/[^/\s]+$/);
      // A preset's own origin resolves back to its upstream.
      if (p.origin)
        expect(matchUpstream(p.origin, u.formats[0])?.id, u.id).toBe(u.id);
    }
  });

  it("covers the well-known upstreams with the required redirect origins", () => {
    const preset = (id: string) => upstreams.find((u) => u.id === id)?.preset;
    expect(preset("dockerhub")?.origin).toBe("https://registry-1.docker.io");
    expect(preset("dockerhub")?.redirect_origins).toContain(
      "https://auth.docker.io",
    );
    expect(preset("go-proxy")?.redirect_origins).toEqual([
      "https://storage.googleapis.com",
    ]);
    expect(preset("pypi")?.redirect_origins).toEqual([
      "https://files.pythonhosted.org",
    ]);
    expect(preset("crates")?.redirect_origins).toEqual([
      "https://static.crates.io",
    ]);
    expect(preset("huggingface")?.redirect_origins).toEqual([
      "https://cdn-lfs.huggingface.co",
      "https://cdn-lfs-us-1.hf.co",
      "https://cdn-lfs.hf.co",
      "https://cas-bridge.xethub.hf.co",
    ]);
    // Fedora repodata is zstd, which the RPM adapter cannot read: no preset.
    expect(preset("fedora")).toBeUndefined();
    expect(upstreams.find((u) => u.id === "fedora")).toBeDefined();
  });

  it("lists presets per format", () => {
    expect(presetsFor("oci").map((u) => u.id)).toContain("dockerhub");
    expect(presetsFor("oci").map((u) => u.id)).not.toContain("npm");
    expect(presetsFor("npm").map((u) => u.id)).toEqual(["npm"]);
    expect(presetsFor("maven").length).toBe(3);
    expect(presetsFor("rpm").map((u) => u.id)).not.toContain("fedora");
  });
});

describe("preset prefill", () => {
  it("fills name, origin and redirect origins", () => {
    const d = presetDraft(upstreams.find((u) => u.id === "dockerhub")!);
    expect(d.name).toBe("dockerhub-remote");
    expect(d.origin).toBe("https://registry-1.docker.io");
    expect(d.redirects.split("\n")).toContain(
      "https://production.cloudflare.docker.com",
    );
  });
});

describe("iconForSource", () => {
  it("brands known remotes and falls back to the defaults", () => {
    expect(
      iconForSource({
        kind: "remote",
        format: "oci",
        origin: "https://ghcr.io",
      }).icon,
    ).toEqual({ brand: "github" });
    expect(
      iconForSource({
        kind: "remote",
        format: "oci",
        origin: "https://registry.example.com",
      }).icon,
    ).toBe(defaultIcons.remote);
    expect(iconForSource({ kind: "remote", format: "oci" }).icon).toBe(
      defaultIcons.remote,
    );
    expect(
      iconForSource({ kind: "local", format: "oci", origin: "https://ghcr.io" })
        .icon,
    ).toBe(defaultIcons.local);
    expect(iconForSource({ kind: "replica", format: "oci" }).icon).toBe(
      defaultIcons.replica,
    );
  });
});

describe("brand icon legibility", () => {
  const lum = (hex: string) => {
    const c = [1, 3, 5].map((i) => {
      const s = parseInt(hex.slice(i, i + 2), 16) / 255;
      return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * c[0]! + 0.7152 * c[1]! + 0.0722 * c[2]!;
  };
  const ratio = (a: string, b: string) => {
    const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
    return (hi! + 0.05) / (lo! + 0.05);
  };
  it("keeps 3:1 against the tile in both themes", () => {
    for (const [slug, b] of Object.entries(brandIcons)) {
      expect(ratio(b.dark, "#151c29"), `${slug} dark`).toBeGreaterThanOrEqual(
        3,
      );
      expect(ratio(b.light, "#eef1f5"), `${slug} light`).toBeGreaterThanOrEqual(
        3,
      );
    }
  });
});
