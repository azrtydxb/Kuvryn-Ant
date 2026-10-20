import { defineConfig } from "vitepress";

const repo = "https://github.com/azrtydxb/Kuvryn-Ant";

/** @type {import("vitepress").DefaultTheme.SidebarItem[]} */
const formats = [
  { text: "Overview and support matrix", link: "/formats/" },
  { text: "OCI / Docker", link: "/formats/oci" },
  { text: "Helm (classic and OCI)", link: "/formats/helm" },
  { text: "npm / pnpm / Yarn", link: "/formats/npm" },
  { text: "PyPI (pip / uv / Poetry)", link: "/formats/pypi" },
  { text: "Maven / Gradle", link: "/formats/maven" },
  { text: "Go modules and sumdb", link: "/formats/go" },
  { text: "Cargo", link: "/formats/cargo" },
  { text: "NuGet", link: "/formats/nuget" },
  { text: "RubyGems", link: "/formats/rubygems" },
  { text: "Composer", link: "/formats/composer" },
  { text: "Conda", link: "/formats/conda" },
  { text: "Conan", link: "/formats/conan" },
  { text: "CRAN", link: "/formats/cran" },
  { text: "pub (Dart / Flutter)", link: "/formats/pub" },
  { text: "CocoaPods", link: "/formats/cocoapods" },
  { text: "Swift packages", link: "/formats/swift" },
  { text: "Terraform / OpenTofu", link: "/formats/terraform" },
  { text: "Ansible Galaxy", link: "/formats/ansible" },
  { text: "APT (Debian / Ubuntu)", link: "/formats/apt" },
  { text: "RPM (DNF / YUM / Zypper)", link: "/formats/rpm" },
  { text: "APK (Alpine)", link: "/formats/apk" },
  { text: "Git LFS", link: "/formats/git-lfs" },
  { text: "P2 (Eclipse)", link: "/formats/p2" },
  { text: "Hugging Face", link: "/formats/huggingface" },
  { text: "Raw / WebDAV", link: "/formats/raw" },
];

export default defineConfig({
  title: "Kuvryn Ant",
  description:
    "Self-hosted artifact repository and caching proxy: one artifact core, 26 package formats.",
  base: "/Kuvryn-Ant/",
  lang: "en-US",
  cleanUrls: true,
  lastUpdated: false,
  appearance: true,
  head: [
    ["link", { rel: "icon", type: "image/png", href: "/Kuvryn-Ant/logo.png" }],
    ["meta", { name: "theme-color", content: "#1f7bff" }],
  ],
  vite: {
    build: { chunkSizeWarningLimit: 4096 },
  },
  markdown: {
    theme: { light: "github-light", dark: "github-dark" },
  },
  themeConfig: {
    logo: "/logo.png",
    siteTitle: "Kuvryn Ant",
    search: { provider: "local" },
    editLink: {
      pattern: `${repo}/edit/main/site/:path`,
      text: "Edit this page on GitHub",
    },
    socialLinks: [{ icon: "github", link: repo }],
    footer: {
      message: "Released under the Apache License 2.0.",
      copyright: "Copyright 2026 The Kuvryn Ant Authors",
    },
    outline: { level: [2, 3] },
    nav: [
      {
        text: "Guide",
        link: "/guide/introduction",
        activeMatch: "^/(guide|concepts)/",
      },
      { text: "Install", link: "/install/helm", activeMatch: "^/install/" },
      { text: "Formats", link: "/formats/", activeMatch: "^/formats/" },
      {
        text: "Operate",
        items: [
          { text: "Host-level caching", link: "/host-caching/" },
          { text: "CI integration", link: "/ci/github-actions" },
          { text: "Build caches", link: "/build-caches/" },
          { text: "Identity", link: "/identity/" },
          { text: "Replication and federation", link: "/replication/" },
          { text: "Operations", link: "/operations/backup-restore" },
        ],
      },
      {
        text: "Reference",
        items: [
          { text: "Configuration (ANT_*)", link: "/reference/configuration" },
          { text: "Helm values", link: "/install/helm#values-reference" },
          { text: "CLI", link: "/reference/cli" },
          { text: "Management API", link: "/reference/api" },
          { text: "MCP tools", link: "/reference/mcp" },
        ],
      },
      { text: "FAQ", link: "/about/faq" },
    ],
    sidebar: [
      {
        text: "Getting started",
        items: [
          { text: "Introduction", link: "/guide/introduction" },
          { text: "Quick start", link: "/guide/quick-start" },
          { text: "Console tour", link: "/console/tour" },
        ],
      },
      {
        text: "Concepts",
        collapsed: false,
        items: [
          { text: "Feeds and sources", link: "/concepts/feeds-and-sources" },
          {
            text: "Read order and resolve explain",
            link: "/concepts/resolution",
          },
          { text: "Publish policies", link: "/concepts/publish-policies" },
          { text: "Feed purposes", link: "/concepts/purposes" },
          { text: "Namespaces and scopes", link: "/concepts/namespaces" },
        ],
      },
      {
        text: "Installation",
        collapsed: true,
        items: [
          { text: "Helm chart", link: "/install/helm" },
          { text: "PostgreSQL", link: "/install/postgresql" },
          { text: "Blob storage (disk / S3)", link: "/install/storage" },
          {
            text: "Ingress, Gateway API and TLS",
            link: "/install/ingress-tls",
          },
          { text: "Root key", link: "/install/root-key" },
          { text: "Sizing", link: "/install/sizing" },
        ],
      },
      { text: "Formats", collapsed: true, items: formats },
      {
        text: "Host-level caching",
        collapsed: true,
        items: [
          { text: "Overview", link: "/host-caching/" },
          {
            text: "containerd (k3s, RKE2, generic)",
            link: "/host-caching/containerd",
          },
          { text: "CRI-O", link: "/host-caching/cri-o" },
          { text: "Docker daemon", link: "/host-caching/docker" },
          { text: "Podman", link: "/host-caching/podman" },
          { text: "BuildKit / buildx", link: "/host-caching/buildkit" },
          { text: "CA trust distribution", link: "/host-caching/ca-trust" },
          { text: "Fallback behaviour", link: "/host-caching/fallback" },
        ],
      },
      {
        text: "CI integration",
        collapsed: true,
        items: [
          { text: "GitHub Actions", link: "/ci/github-actions" },
          { text: "Actions Runner Controller", link: "/ci/arc-runners" },
          { text: "Other CI systems", link: "/ci/generic" },
        ],
      },
      {
        text: "Build caches",
        collapsed: true,
        items: [
          { text: "Cache feeds and policies", link: "/build-caches/" },
          { text: "buildx registry cache", link: "/build-caches/buildx" },
          { text: "sccache over WebDAV", link: "/build-caches/sccache" },
          { text: "Measurements", link: "/build-caches/measurements" },
        ],
      },
      {
        text: "Identity and access",
        collapsed: true,
        items: [
          { text: "Overview", link: "/identity/" },
          { text: "Local users", link: "/identity/local-users" },
          { text: "OIDC (Zitadel example)", link: "/identity/oidc" },
          { text: "SAML", link: "/identity/saml" },
          { text: "LDAP", link: "/identity/ldap" },
          { text: "Roles and grants", link: "/identity/roles" },
          { text: "Service accounts and tokens", link: "/identity/tokens" },
          { text: "GitHub OIDC trust", link: "/identity/github-oidc" },
          { text: "Package client auth", link: "/identity/client-auth" },
        ],
      },
      {
        text: "Replication and federation",
        collapsed: true,
        items: [
          { text: "Replication and multi-site", link: "/replication/" },
          { text: "Federation", link: "/replication/federation" },
          {
            text: "Federation runbooks",
            link: "/replication/federation-runbooks",
          },
        ],
      },
      {
        text: "Operations",
        collapsed: true,
        items: [
          {
            text: "Storage maintenance",
            link: "/operations/storage-maintenance",
          },
          { text: "Backup and restore", link: "/operations/backup-restore" },
          { text: "Upgrade", link: "/operations/upgrade" },
          { text: "Observability", link: "/operations/observability" },
          { text: "Diagnostics and repair", link: "/operations/diagnostics" },
          { text: "Break-glass access", link: "/operations/break-glass" },
          {
            text: "Import, export, migration",
            link: "/operations/import-export",
          },
          { text: "Prewarm", link: "/operations/prewarm" },
          { text: "Storage migration", link: "/operations/storage-migration" },
        ],
      },
      {
        text: "Reference",
        collapsed: true,
        items: [
          { text: "Configuration (ANT_*)", link: "/reference/configuration" },
          { text: "CLI", link: "/reference/cli" },
          { text: "Management API", link: "/reference/api" },
          { text: "MCP tools", link: "/reference/mcp" },
        ],
      },
      {
        text: "About",
        collapsed: true,
        items: [
          { text: "FAQ", link: "/about/faq" },
          { text: "Comparison", link: "/about/comparison" },
          { text: "Security model", link: "/about/security" },
          { text: "Roadmap", link: "/about/roadmap" },
        ],
      },
    ],
  },
});
