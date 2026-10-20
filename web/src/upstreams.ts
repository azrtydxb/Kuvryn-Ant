import type { BrandSlug } from "./brandIcons";
import type { Source } from "./generated";

type Format = Source["format"];

/** A brand logo (Simple Icons) or a neutral glyph from the console icon font. */
export type IconSpec =
  | { brand: BrandSlug }
  | { glyph: string; tone?: "local" | "replica" | "cache" };

export type Upstream = {
  id: string;
  name: string;
  /** Formats whose feeds can use this upstream. */
  formats: readonly Format[];
  icon: IconSpec;
  homepage: string;
  /**
   * Hosts this upstream is reached on. `*.example.com` matches any subdomain
   * (and not example.com itself).
   */
  hosts: readonly string[];
  /** Origin and redirect origins a new source starts from. */
  preset?: {
    /** Suggested source name, for example `dockerhub-remote`. */
    name: string;
    origin: string;
    redirect_origins: readonly string[];
    /** Shown beside the form after picking the preset. */
    note?: string;
  };
};

const oci = ["oci", "oci-artifacts"] as const;
const brand = (b: BrandSlug): IconSpec => ({ brand: b });
const glyph = (g: string): IconSpec => ({ glyph: g });

// Origins and redirect origins come from docs/implementation/pull-through.md,
// docs/implementation/oci-mirror.md, the adapter READMEs and the real-client
// conformance harness. Only hosts an adapter can actually use are preset.
export const upstreams: readonly Upstream[] = [
  // OCI registries
  {
    id: "dockerhub",
    name: "Docker Hub",
    formats: oci,
    icon: brand("docker"),
    homepage: "https://hub.docker.com",
    hosts: [
      "registry-1.docker.io",
      "docker.io",
      "index.docker.io",
      "registry.hub.docker.com",
      "hub.docker.com",
      "auth.docker.io",
    ],
    preset: {
      name: "dockerhub-remote",
      origin: "https://registry-1.docker.io",
      redirect_origins: [
        "https://auth.docker.io",
        "https://production.cloudflare.docker.com",
        "https://production.cloudfront.docker.com",
      ],
    },
  },
  {
    id: "ghcr",
    name: "GitHub Container Registry",
    formats: oci,
    icon: brand("github"),
    homepage: "https://github.com/features/packages",
    hosts: ["ghcr.io", "pkg-containers.githubusercontent.com"],
    preset: {
      name: "ghcr-remote",
      origin: "https://ghcr.io",
      redirect_origins: ["https://pkg-containers.githubusercontent.com"],
    },
  },
  {
    id: "quay",
    name: "Quay",
    formats: oci,
    icon: glyph("container"),
    homepage: "https://quay.io",
    hosts: ["quay.io", "*.quay.io"],
    preset: {
      name: "quay-remote",
      origin: "https://quay.io",
      redirect_origins: [
        "https://cdn.quay.io",
        "https://cdn01.quay.io",
        "https://cdn02.quay.io",
        "https://cdn03.quay.io",
      ],
    },
  },
  {
    id: "gcr",
    name: "Google Container Registry",
    formats: oci,
    icon: brand("googlecloud"),
    homepage: "https://cloud.google.com/artifact-registry",
    hosts: ["gcr.io", "*.gcr.io"],
    preset: {
      name: "gcr-remote",
      origin: "https://gcr.io",
      redirect_origins: [],
    },
  },
  {
    id: "artifact-registry",
    name: "Google Artifact Registry",
    formats: oci,
    icon: brand("googlecloud"),
    homepage: "https://cloud.google.com/artifact-registry",
    hosts: ["*-docker.pkg.dev"],
    preset: {
      name: "artifact-registry-remote",
      origin: "https://us-docker.pkg.dev",
      redirect_origins: [],
      note: "Replace us with your region. Private repositories need a credential.",
    },
  },
  {
    id: "ecr-public",
    name: "Amazon ECR Public",
    formats: oci,
    icon: glyph("cloud"),
    homepage: "https://gallery.ecr.aws",
    hosts: ["public.ecr.aws"],
    preset: {
      name: "ecr-public-remote",
      origin: "https://public.ecr.aws",
      redirect_origins: ["https://d1uvnnlqvlnzwp.cloudfront.net"],
      note: "CDN hostnames change. If a pull fails with a policy error, the access log shows the redirect target to add.",
    },
  },
  {
    id: "k8s-registry",
    name: "registry.k8s.io",
    formats: oci,
    icon: brand("kubernetes"),
    homepage: "https://registry.k8s.io",
    hosts: ["registry.k8s.io", "k8s.gcr.io"],
    preset: {
      name: "k8s-registry-remote",
      origin: "https://registry.k8s.io",
      redirect_origins: [
        "https://prod-registry-k8s-io-us-east-1.s3.dualstack.us-east-1.amazonaws.com",
        "https://prod-registry-k8s-io-us-west-2.s3.dualstack.us-west-2.amazonaws.com",
        "https://prod-registry-k8s-io-eu-west-1.s3.dualstack.eu-west-1.amazonaws.com",
      ],
      note: "Blobs redirect to a regional bucket. Keep the regions you run in; the access log shows others.",
    },
  },
  {
    id: "mcr",
    name: "Microsoft Container Registry",
    formats: oci,
    icon: glyph("container"),
    homepage: "https://mcr.microsoft.com",
    hosts: ["mcr.microsoft.com", "*.data.mcr.microsoft.com"],
    preset: {
      name: "mcr-remote",
      origin: "https://mcr.microsoft.com",
      redirect_origins: [
        "https://westus2.data.mcr.microsoft.com",
        "https://eastus.data.mcr.microsoft.com",
      ],
      note: "Layers are served from regional data hosts. The access log shows the host to add if a pull fails.",
    },
  },
  {
    id: "gitlab-registry",
    name: "GitLab Container Registry",
    formats: oci,
    icon: brand("gitlab"),
    homepage: "https://docs.gitlab.com/user/packages/container_registry/",
    hosts: ["registry.gitlab.com"],
    preset: {
      name: "gitlab-registry-remote",
      origin: "https://registry.gitlab.com",
      redirect_origins: ["https://gitlab.com"],
      note: "The token realm is gitlab.com. Add a credential for private projects.",
    },
  },
  {
    id: "ngc",
    name: "NVIDIA NGC",
    formats: oci,
    icon: brand("nvidia"),
    homepage: "https://catalog.ngc.nvidia.com",
    hosts: ["nvcr.io", "authn.nvidia.com"],
    preset: {
      name: "ngc-remote",
      origin: "https://nvcr.io",
      redirect_origins: ["https://authn.nvidia.com"],
      note: "Most NGC images need an API key credential.",
    },
  },
  // Language registries
  {
    id: "npm",
    name: "npm",
    formats: ["npm"],
    icon: brand("npm"),
    homepage: "https://www.npmjs.com",
    hosts: [
      "registry.npmjs.org",
      "registry.npmjs.com",
      "registry.yarnpkg.com",
      "npmjs.com",
      "www.npmjs.com",
    ],
    preset: {
      name: "npm-remote",
      origin: "https://registry.npmjs.org",
      redirect_origins: [],
    },
  },
  {
    id: "pypi",
    name: "PyPI",
    formats: ["pypi"],
    icon: brand("pypi"),
    homepage: "https://pypi.org",
    hosts: [
      "pypi.org",
      "files.pythonhosted.org",
      "pypi.python.org",
      "pythonhosted.org",
    ],
    preset: {
      name: "pypi-remote",
      origin: "https://pypi.org",
      redirect_origins: ["https://files.pythonhosted.org"],
    },
  },
  {
    id: "maven-central",
    name: "Maven Central",
    formats: ["maven"],
    icon: brand("apachemaven"),
    homepage: "https://central.sonatype.com",
    hosts: [
      "repo1.maven.org",
      "repo.maven.apache.org",
      "central.maven.org",
      "search.maven.org",
      "central.sonatype.com",
    ],
    preset: {
      name: "maven-central-remote",
      origin: "https://repo1.maven.org/maven2",
      redirect_origins: [],
    },
  },
  {
    id: "google-maven",
    name: "Google Maven",
    formats: ["maven"],
    icon: brand("google"),
    homepage: "https://maven.google.com",
    hosts: ["maven.google.com", "dl.google.com"],
    preset: {
      name: "google-maven-remote",
      origin: "https://maven.google.com",
      redirect_origins: [],
    },
  },
  {
    id: "gradle-plugins",
    name: "Gradle Plugin Portal",
    formats: ["maven"],
    icon: brand("gradle"),
    homepage: "https://plugins.gradle.org",
    hosts: [
      "plugins.gradle.org",
      "plugins-artifacts.gradle.org",
      "repo.gradle.org",
    ],
    preset: {
      name: "gradle-plugins-remote",
      origin: "https://plugins.gradle.org/m2",
      redirect_origins: [],
    },
  },
  {
    id: "go-proxy",
    name: "Go module proxy",
    formats: ["go"],
    icon: brand("go"),
    homepage: "https://proxy.golang.org",
    // sum.golang.org is fetched by the adapter itself; it is listed so an
    // origin pointing at it still shows the Go logo.
    hosts: [
      "proxy.golang.org",
      "sum.golang.org",
      "index.golang.org",
      "golang.org",
      "pkg.go.dev",
    ],
    preset: {
      name: "go-proxy-remote",
      origin: "https://proxy.golang.org",
      redirect_origins: ["https://storage.googleapis.com"],
      note: "Large module archives redirect to Google Cloud Storage. The checksum database is reached automatically.",
    },
  },
  {
    id: "crates",
    name: "crates.io",
    formats: ["cargo"],
    icon: brand("rust"),
    homepage: "https://crates.io",
    hosts: ["index.crates.io", "static.crates.io", "crates.io"],
    preset: {
      name: "crates-remote",
      origin: "https://index.crates.io",
      redirect_origins: ["https://static.crates.io"],
    },
  },
  {
    id: "nuget",
    name: "NuGet",
    formats: ["nuget"],
    icon: brand("nuget"),
    homepage: "https://www.nuget.org",
    hosts: [
      "api.nuget.org",
      "globalcdn.nuget.org",
      "nuget.org",
      "www.nuget.org",
    ],
    preset: {
      name: "nuget-remote",
      origin: "https://api.nuget.org",
      redirect_origins: ["https://globalcdn.nuget.org"],
    },
  },
  {
    id: "rubygems",
    name: "RubyGems",
    formats: ["rubygems"],
    icon: brand("rubygems"),
    homepage: "https://rubygems.org",
    hosts: ["rubygems.org", "index.rubygems.org", "*.rubygems.org"],
    preset: {
      name: "rubygems-remote",
      origin: "https://rubygems.org",
      redirect_origins: [],
    },
  },
  {
    id: "packagist",
    name: "Packagist",
    formats: ["composer"],
    icon: brand("packagist"),
    homepage: "https://packagist.org",
    hosts: ["repo.packagist.org", "packagist.org"],
    preset: {
      name: "packagist-remote",
      origin: "https://repo.packagist.org",
      redirect_origins: [
        "https://api.github.com",
        "https://codeload.github.com",
      ],
      note: "Dist archives live on GitHub. Add gitlab.com or bitbucket.org if you use packages hosted there.",
    },
  },
  {
    id: "conda-forge",
    name: "conda-forge",
    formats: ["conda"],
    icon: brand("condaforge"),
    homepage: "https://conda-forge.org",
    hosts: ["conda.anaconda.org", "conda-forge.org"],
    preset: {
      name: "conda-forge-remote",
      origin: "https://conda.anaconda.org/conda-forge",
      redirect_origins: [],
      note: "Repodata above the metadata limit cannot be proxied, so very large channels may not work.",
    },
  },
  {
    id: "anaconda",
    name: "Anaconda",
    formats: ["conda"],
    icon: brand("anaconda"),
    homepage: "https://www.anaconda.com",
    hosts: ["repo.anaconda.com", "anaconda.org", "www.anaconda.com"],
  },
  {
    id: "conancenter",
    name: "ConanCenter",
    formats: ["conan"],
    icon: brand("conan"),
    homepage: "https://conan.io/center",
    hosts: ["center2.conan.io", "center.conan.io", "conan.io"],
    preset: {
      name: "conancenter-remote",
      origin: "https://center2.conan.io",
      redirect_origins: [],
    },
  },
  {
    id: "cran",
    name: "CRAN",
    formats: ["cran"],
    icon: brand("r"),
    homepage: "https://cran.r-project.org",
    hosts: ["cloud.r-project.org", "cran.r-project.org", "*.r-project.org"],
    preset: {
      name: "cran-remote",
      origin: "https://cloud.r-project.org",
      redirect_origins: [],
    },
  },
  {
    id: "pub",
    name: "pub.dev",
    formats: ["pub"],
    icon: brand("dart"),
    homepage: "https://pub.dev",
    hosts: ["pub.dev", "pub.dartlang.org"],
    preset: {
      name: "pub-remote",
      origin: "https://pub.dev",
      redirect_origins: ["https://storage.googleapis.com", "https://pub.dev"],
    },
  },
  {
    id: "cocoapods",
    name: "CocoaPods",
    formats: ["cocoapods"],
    icon: brand("cocoapods"),
    homepage: "https://cocoapods.org",
    hosts: ["cdn.cocoapods.org", "cocoapods.org"],
    preset: {
      name: "cocoapods-remote",
      origin: "https://cdn.cocoapods.org",
      redirect_origins: ["https://cdn.jsdelivr.net"],
      note: "Podspecs redirect to jsDelivr. Pod sources are still fetched by clients from their original hosts.",
    },
  },
  {
    id: "swift",
    name: "Swift registry",
    formats: ["swift"],
    icon: brand("swift"),
    homepage: "https://www.swift.org/blog/swift-package-registry/",
    hosts: [],
    preset: {
      name: "swift-remote",
      origin: "",
      redirect_origins: [],
      note: "There is no public default registry. Enter the registry root a client passes to swift package-registry set.",
    },
  },
  {
    id: "terraform",
    name: "Terraform Registry",
    formats: ["terraform"],
    icon: brand("terraform"),
    homepage: "https://registry.terraform.io",
    hosts: ["registry.terraform.io", "releases.hashicorp.com"],
    preset: {
      name: "terraform-remote",
      origin: "https://registry.terraform.io",
      redirect_origins: [
        "https://releases.hashicorp.com",
        "https://codeload.github.com",
      ],
      note: "Providers download from releases.hashicorp.com; modules resolve to GitHub archives.",
    },
  },
  {
    id: "ansible-galaxy",
    name: "Ansible Galaxy",
    formats: ["ansible"],
    icon: brand("ansible"),
    homepage: "https://galaxy.ansible.com",
    hosts: [
      "galaxy.ansible.com",
      "ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com",
    ],
    preset: {
      name: "ansible-galaxy-remote",
      origin: "https://galaxy.ansible.com/api/",
      redirect_origins: [
        "https://galaxy.ansible.com",
        "https://ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com",
      ],
    },
  },
  {
    id: "huggingface",
    name: "Hugging Face",
    formats: ["huggingface"],
    icon: brand("huggingface"),
    homepage: "https://huggingface.co",
    hosts: ["huggingface.co", "*.huggingface.co", "hf.co", "*.hf.co"],
    preset: {
      name: "huggingface-remote",
      origin: "https://huggingface.co",
      redirect_origins: [
        "https://cdn-lfs.huggingface.co",
        "https://cdn-lfs-us-1.hf.co",
        "https://cdn-lfs.hf.co",
        "https://cas-bridge.xethub.hf.co",
      ],
      note: "A credential is only needed for gated or private repositories.",
    },
  },
  // Helm charts
  {
    id: "artifacthub",
    name: "Artifact Hub",
    formats: ["helm"],
    icon: brand("artifacthub"),
    homepage: "https://artifacthub.io",
    hosts: ["artifacthub.io"],
  },
  {
    id: "bitnami",
    name: "Bitnami charts",
    formats: ["helm"],
    icon: brand("helm"),
    homepage: "https://github.com/bitnami/charts",
    hosts: ["charts.bitnami.com"],
  },
  {
    id: "prometheus-charts",
    name: "Prometheus community charts",
    formats: ["helm"],
    icon: brand("prometheus"),
    homepage: "https://prometheus-community.github.io/helm-charts",
    hosts: ["prometheus-community.github.io"],
    preset: {
      name: "prometheus-charts-remote",
      origin: "https://prometheus-community.github.io/helm-charts",
      redirect_origins: [
        "https://github.com",
        "https://objects.githubusercontent.com",
        "https://release-assets.githubusercontent.com",
      ],
      note: "Chart archives are GitHub release assets.",
    },
  },
  {
    id: "grafana-charts",
    name: "Grafana charts",
    formats: ["helm"],
    icon: brand("grafana"),
    homepage: "https://grafana.github.io/helm-charts",
    hosts: ["grafana.github.io"],
    preset: {
      name: "grafana-charts-remote",
      origin: "https://grafana.github.io/helm-charts",
      redirect_origins: [
        "https://github.com",
        "https://objects.githubusercontent.com",
        "https://release-assets.githubusercontent.com",
      ],
      note: "Chart archives are GitHub release assets.",
    },
  },
  // OS package repositories
  {
    id: "debian",
    name: "Debian",
    formats: ["apt"],
    icon: brand("debian"),
    homepage: "https://www.debian.org",
    hosts: [
      "deb.debian.org",
      "ftp.debian.org",
      "security.debian.org",
      "*.debian.org",
    ],
    preset: {
      name: "debian-remote",
      origin: "https://deb.debian.org/debian",
      redirect_origins: [],
    },
  },
  {
    id: "ubuntu",
    name: "Ubuntu",
    formats: ["apt"],
    icon: brand("ubuntu"),
    homepage: "https://ubuntu.com",
    hosts: [
      "archive.ubuntu.com",
      "security.ubuntu.com",
      "ports.ubuntu.com",
      "*.archive.ubuntu.com",
    ],
    preset: {
      name: "ubuntu-remote",
      origin: "https://archive.ubuntu.com/ubuntu",
      redirect_origins: [],
    },
  },
  {
    id: "fedora",
    name: "Fedora",
    formats: ["rpm"],
    icon: brand("fedora"),
    homepage: "https://fedoraproject.org",
    // No preset: Fedora repodata is zstd compressed, which the RPM adapter
    // does not support. Sources pointing at a mirror still show the logo.
    hosts: [
      "dl.fedoraproject.org",
      "download.fedoraproject.org",
      "mirrors.fedoraproject.org",
      "*.fedoraproject.org",
    ],
  },
  {
    id: "rocky",
    name: "Rocky Linux",
    formats: ["rpm"],
    icon: brand("rockylinux"),
    homepage: "https://rockylinux.org",
    hosts: ["dl.rockylinux.org", "download.rockylinux.org", "rockylinux.org"],
    preset: {
      name: "rocky-remote",
      origin: "https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os",
      redirect_origins: [],
      note: "One repository per source. Change the release, repository and architecture in the URL as needed.",
    },
  },
  {
    id: "alma",
    name: "AlmaLinux",
    formats: ["rpm"],
    icon: brand("almalinux"),
    homepage: "https://almalinux.org",
    hosts: ["repo.almalinux.org", "almalinux.org", "mirrors.almalinux.org"],
    preset: {
      name: "alma-remote",
      origin: "https://repo.almalinux.org/almalinux/9/BaseOS/x86_64/os",
      redirect_origins: [],
      note: "One repository per source. Change the release, repository and architecture in the URL as needed.",
    },
  },
  {
    id: "centos",
    name: "CentOS Stream",
    formats: ["rpm"],
    icon: brand("centos"),
    homepage: "https://www.centos.org",
    hosts: [
      "mirror.stream.centos.org",
      "mirror.centos.org",
      "vault.centos.org",
      "*.centos.org",
    ],
    preset: {
      name: "centos-remote",
      origin: "https://mirror.stream.centos.org/9-stream/BaseOS/x86_64/os",
      redirect_origins: [],
      note: "One repository per source. Change the release, repository and architecture in the URL as needed.",
    },
  },
  {
    id: "alpine",
    name: "Alpine Linux",
    formats: ["apk"],
    icon: brand("alpinelinux"),
    homepage: "https://alpinelinux.org",
    hosts: [
      "dl-cdn.alpinelinux.org",
      "dl.alpinelinux.org",
      "*.alpinelinux.org",
    ],
    preset: {
      name: "alpine-remote",
      origin: "https://dl-cdn.alpinelinux.org/alpine",
      redirect_origins: [],
    },
  },
  // Files
  {
    id: "github-releases",
    name: "GitHub releases",
    formats: ["raw", "git-lfs"],
    icon: brand("github"),
    homepage: "https://github.com",
    hosts: [
      "github.com",
      "objects.githubusercontent.com",
      "release-assets.githubusercontent.com",
      "codeload.github.com",
      "api.github.com",
    ],
    preset: {
      name: "github-releases-remote",
      origin: "https://github.com",
      redirect_origins: [
        "https://objects.githubusercontent.com",
        "https://release-assets.githubusercontent.com",
      ],
      note: "Release downloads redirect to GitHub's asset hosts.",
    },
  },
  {
    id: "github-raw",
    name: "GitHub raw files",
    formats: ["raw"],
    icon: brand("github"),
    homepage: "https://raw.githubusercontent.com",
    hosts: ["raw.githubusercontent.com"],
    preset: {
      name: "github-raw-remote",
      origin: "https://raw.githubusercontent.com",
      redirect_origins: [],
    },
  },
  {
    id: "eclipse",
    name: "Eclipse downloads",
    formats: ["p2"],
    icon: brand("eclipseide"),
    homepage: "https://download.eclipse.org",
    hosts: ["download.eclipse.org", "*.eclipse.org"],
  },
];

/** Default marks: custom remote, local, replica and cache-purpose feeds. */
export const defaultIcons = {
  remote: glyph("globe"),
  local: { glyph: "box", tone: "local" },
  replica: { glyph: "refresh-cw", tone: "replica" },
  cache: { glyph: "zap", tone: "cache" },
} as const satisfies Record<string, IconSpec>;

/** Hostname of an origin URL, lower-cased; empty when it is not a URL. */
export function originHost(origin: string | undefined): string {
  if (!origin) return "";
  try {
    return new URL(origin).hostname.toLowerCase().replace(/\.$/, "");
  } catch {
    return "";
  }
}

function hostMatches(pattern: string, host: string): boolean {
  if (pattern.startsWith("*.")) return host.endsWith(pattern.slice(1));
  if (pattern.startsWith("*-")) return host.endsWith(pattern.slice(1));
  return host === pattern;
}

/**
 * The known upstream an origin points at, or undefined for a custom one.
 * Exact hosts win over wildcard hosts; with a format, an upstream serving that
 * format wins over one that does not (github.com is raw and OCI-adjacent).
 */
export function matchUpstream(
  origin: string | undefined,
  format?: Format,
): Upstream | undefined {
  const host = originHost(origin);
  if (!host) return undefined;
  let best: { u: Upstream; score: number } | undefined;
  for (const u of upstreams) {
    for (const pattern of u.hosts) {
      if (!hostMatches(pattern, host)) continue;
      const exact = pattern === host;
      const score =
        (exact ? 2 : 0) + (format && u.formats.includes(format) ? 4 : 0);
      if (!best || score > best.score) best = { u, score };
    }
  }
  return best?.u;
}

/** Upstreams with a preset for a format, in catalog order. */
export function presetsFor(format: Format): Upstream[] {
  return upstreams.filter((u) => u.preset && u.formats.includes(format));
}

export type SourceLike = {
  kind: Source["kind"];
  format?: Source["format"];
  origin?: string | undefined;
};

/** The mark for a source: brand for a known remote, else the kind default. */
export function iconForSource(s: SourceLike): {
  icon: IconSpec;
  upstream?: Upstream;
} {
  if (s.kind === "local") return { icon: defaultIcons.local };
  if (s.kind === "replica") return { icon: defaultIcons.replica };
  const upstream = matchUpstream(s.origin, s.format);
  return upstream
    ? { icon: upstream.icon, upstream }
    : { icon: defaultIcons.remote };
}

/** Values a preset prefills in the source form. */
export function presetDraft(u: Upstream) {
  return {
    name: u.preset?.name ?? "",
    origin: u.preset?.origin ?? "",
    redirects: (u.preset?.redirect_origins ?? []).join("\n"),
  };
}
