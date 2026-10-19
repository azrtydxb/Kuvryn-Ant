package observability

import (
	"net/http"
	"strings"
)

// formats are the package-protocol adapters mounted at /feeds/{slug}/<format>/.
// A format reaches the metric label only if it is in this set, so the label
// space is the product of this fixed list and the fixed operation classes below.
var formats = map[string]bool{
	"ansible": true, "apk": true, "apt": true, "cargo": true, "cocoapods": true, "composer": true, "conan": true,
	"conda": true, "cran": true, "git-lfs": true, "go": true, "helm": true, "huggingface": true, "maven": true,
	"npm": true, "nuget": true, "p2": true, "pub": true, "pypi": true, "raw": true, "rpm": true, "rubygems": true,
	"swift": true, "terraform": true,
}

// federationRoutes names the federation endpoints. Peer control traffic is
// grouped by purpose; replication and proxied reads get their own names.
var federationRoutes = map[string]string{
	"/federation/blob/{name}":                           "federation.blob",
	"/federation/v1/proxy/":                             "federation.proxy",
	"/federation/v1/proxy/feeds/{fed}/{slug}/{rest...}": "federation.proxy",
	"/federation/v1/routes/feeds/{fed}/{slug}":          "federation.route",
	"/federation/v1/replication/feeds/{fed}/{slug}":     "federation.replication",
	"/federation/v1/replication/transfer-state":         "federation.replication",
	"/federation/v1/replication/status":                 "federation.replication",
	"/federation/v1/search":                             "federation.search",
	"/federation/v1/audit/query":                        "federation.audit",
	"/federation/v1/access-check":                       "federation.control",
	"/federation/v1/join":                               "federation.control",
	"/federation/v1/join/confirm":                       "federation.control",
	"/federation/v1/leave":                              "federation.control",
	"/federation/v1/heads":                              "federation.control",
	"/federation/v1/documents/{kind}":                   "federation.control",
	"/federation/v1/nudge":                              "federation.control",
	"/federation/v1/status":                             "federation.control",
	"/federation/v1/csr/renew":                          "federation.control",
	"/federation/v1/authority/prepare":                  "federation.control",
	"/federation/v1/authority/commit":                   "federation.control",
}

// RouteFor names the route of a request that ServeMux resolved to pattern.
// Management and operational routes keep their registered pattern (see Route).
// Package-protocol traffic, which a mux registers as one prefix per format,
// is named <format>.<class>, for example npm.metadata, and OCI traffic oci.blob
// or oci.manifest; the class comes from the method and the shape of the path
// below the format prefix, never from its content (no package names, versions,
// digests or slugs). Anything unrecognised collapses to "unmatched".
func RouteFor(pattern string, r *http.Request) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if name, ok := federationRoutes[pattern]; ok {
		return name
	}
	if pattern == "/v2/" {
		return "oci." + ociClass(r.Method, r.URL.Path)
	}
	if rest, ok := strings.CutPrefix(pattern, "/feeds/{slug}/"); ok {
		format, tail, _ := strings.Cut(rest, "/")
		if tail == "" && formats[format] {
			return format + "." + packageClass(format, r.Method, formatPath(r.URL.Path, format))
		}
		return "unmatched"
	}
	return Route(pattern)
}

// formatPath returns the request path below /feeds/<slug>/<format>/.
func formatPath(path, format string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 4)
	if len(parts) < 4 || parts[0] != "feeds" || parts[2] != format {
		return ""
	}
	return parts[3]
}

func writeClass(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return ""
	case http.MethodDelete:
		return "delete"
	case http.MethodOptions:
		return "other"
	default:
		return "publish"
	}
}

// ociClass classifies an OCI Distribution request path (/v2/<name>/...).
func ociClass(method, path string) string {
	rest := strings.TrimPrefix(path, "/v2/")
	switch {
	case rest == "" || rest == "/":
		return "base"
	case rest == "_catalog":
		return "catalog"
	case strings.Contains(rest, "/blobs/uploads"):
		return "upload"
	case strings.Contains(rest, "/blobs/"):
		return "blob"
	case strings.Contains(rest, "/manifests/"):
		return "manifest"
	case strings.HasSuffix(rest, "/tags/list"):
		return "tags"
	case strings.Contains(rest, "/referrers/"):
		return "referrers"
	}
	return "other"
}

var downloadSuffixes = []string{
	".tgz", ".tar.gz", ".tar", ".zip", ".jar", ".war", ".aar", ".pom", ".module", ".whl", ".egg", ".gem", ".crate", ".nupkg",
	".snupkg", ".rpm", ".deb", ".udeb", ".apk", ".conda", ".tar.bz2", ".tar.xz", ".tar.zst", ".vsix", ".bin",
	".safetensors", ".gguf", ".onnx", ".pt", ".so", ".dll", ".exe", ".pkg", ".msi", ".dmg", ".xz", ".bz2", ".zst", ".7z",
}

var downloadSegments = []string{"/download", "/blobs/", "/files/", "/pool/", "/resolve/", "/lfs/objects/", "/archive/"}

// packageClass classifies a non-OCI package-protocol request into a bounded
// vocabulary: metadata (indexes, packuments, listings), download (artifact
// bytes), publish (any other write) and delete.
func packageClass(format, method, path string) string {
	if c := writeClass(method); c != "" {
		return c
	}
	p := strings.ToLower(path)
	switch format {
	case "go":
		// The module proxy protocol: only .zip carries source bytes.
		if strings.HasSuffix(p, ".zip") {
			return "download"
		}
		return "metadata"
	case "npm":
		if strings.HasPrefix(p, "-/") {
			return "other" // ping, whoami, search, audit
		}
		if strings.Contains(p, "/-/") {
			return "download"
		}
		return "metadata"
	case "maven":
		if strings.Contains(p, "maven-metadata.xml") || strings.HasSuffix(p, "/") || p == "" {
			return "metadata"
		}
	case "git-lfs":
		if strings.Contains(p, "/objects/") && !strings.HasSuffix(p, "/batch") {
			return "download"
		}
		return "metadata"
	}
	for _, s := range downloadSuffixes {
		if strings.HasSuffix(p, s) {
			return "download"
		}
	}
	for _, s := range downloadSegments {
		if strings.Contains("/"+p, s) {
			return "download"
		}
	}
	return "metadata"
}
