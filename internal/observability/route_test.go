package observability

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouteForNamesPackageAndOCITraffic(t *testing.T) {
	for _, tc := range []struct{ pattern, method, path, want string }{
		{"/feeds/{slug}/npm/", "GET", "/feeds/web/npm/left-pad", "npm.metadata"},
		{"/feeds/{slug}/npm/", "GET", "/feeds/web/npm/@scope%2fpkg", "npm.metadata"},
		{"/feeds/{slug}/npm/", "GET", "/feeds/web/npm/left-pad/-/left-pad-1.0.0.tgz", "npm.download"},
		{"/feeds/{slug}/npm/", "PUT", "/feeds/web/npm/left-pad", "npm.publish"},
		{"/feeds/{slug}/npm/", "DELETE", "/feeds/web/npm/left-pad/-rev/1", "npm.delete"},
		{"/feeds/{slug}/npm/", "GET", "/feeds/web/npm/-/ping", "npm.other"},
		{"/feeds/{slug}/pypi/", "GET", "/feeds/p/pypi/simple/requests/", "pypi.metadata"},
		{"/feeds/{slug}/pypi/", "GET", "/feeds/p/pypi/packages/ab/cd/requests-2.0-py3-none-any.whl", "pypi.download"},
		{"/feeds/{slug}/pypi/", "POST", "/feeds/p/pypi/legacy/", "pypi.publish"},
		{"/feeds/{slug}/maven/", "GET", "/feeds/m/maven/org/acme/lib/maven-metadata.xml", "maven.metadata"},
		{"/feeds/{slug}/maven/", "GET", "/feeds/m/maven/org/acme/lib/1.0/lib-1.0.jar", "maven.download"},
		{"/feeds/{slug}/maven/", "PUT", "/feeds/m/maven/org/acme/lib/1.0/lib-1.0.jar", "maven.publish"},
		{"/feeds/{slug}/go/", "GET", "/feeds/g/go/example.com/mod/@v/list", "go.metadata"},
		{"/feeds/{slug}/go/", "GET", "/feeds/g/go/example.com/mod/@v/v1.0.0.info", "go.metadata"},
		{"/feeds/{slug}/go/", "GET", "/feeds/g/go/example.com/mod/@v/v1.0.0.zip", "go.download"},
		{"/feeds/{slug}/cargo/", "GET", "/feeds/c/cargo/index/se/rd/serde", "cargo.metadata"},
		{"/feeds/{slug}/cargo/", "GET", "/feeds/c/cargo/api/v1/crates/serde/1.0.0/download", "cargo.download"},
		{"/feeds/{slug}/helm/", "GET", "/feeds/h/helm/index.yaml", "helm.metadata"},
		{"/feeds/{slug}/helm/", "GET", "/feeds/h/helm/charts/app-1.0.0.tgz", "helm.download"},
		{"/feeds/{slug}/nuget/", "GET", "/feeds/n/nuget/v3/index.json", "nuget.metadata"},
		{"/feeds/{slug}/nuget/", "GET", "/feeds/n/nuget/v3-flatcontainer/pkg/1.0.0/pkg.1.0.0.nupkg", "nuget.download"},
		{"/feeds/{slug}/apt/", "GET", "/feeds/a/apt/dists/stable/Release", "apt.metadata"},
		{"/feeds/{slug}/apt/", "GET", "/feeds/a/apt/pool/main/p/pkg/pkg_1.0_amd64.deb", "apt.download"},
		{"/feeds/{slug}/huggingface/", "GET", "/feeds/h/huggingface/org/model/resolve/main/model.safetensors", "huggingface.download"},
		{"/feeds/{slug}/raw/", "GET", "/feeds/r/raw/some/dir/file.txt", "raw.metadata"},
		{"/feeds/{slug}/raw/", "PUT", "/feeds/r/raw/some/dir/file.txt", "raw.publish"},
		{"/feeds/{slug}/git-lfs/", "POST", "/feeds/l/git-lfs/org/repo/objects/batch", "git-lfs.publish"},
		{"GET /feeds/{slug}/npm/", "GET", "/feeds/web/npm/left-pad", "npm.metadata"},
		{"/v2/", "GET", "/v2/", "oci.base"},
		{"/v2/", "GET", "/v2/library/alpine/blobs/sha256:abc", "oci.blob"},
		{"/v2/", "HEAD", "/v2/library/alpine/blobs/sha256:abc", "oci.blob"},
		{"/v2/", "POST", "/v2/library/alpine/blobs/uploads/", "oci.upload"},
		{"/v2/", "PATCH", "/v2/a/b/c/blobs/uploads/uuid", "oci.upload"},
		{"/v2/", "GET", "/v2/library/alpine/manifests/latest", "oci.manifest"},
		{"/v2/", "PUT", "/v2/library/alpine/manifests/latest", "oci.manifest"},
		{"/v2/", "GET", "/v2/library/alpine/tags/list", "oci.tags"},
		{"/v2/", "GET", "/v2/library/alpine/referrers/sha256:abc", "oci.referrers"},
		{"/v2/", "GET", "/v2/_catalog", "oci.catalog"},
		{"/v2/", "GET", "/v2/odd", "oci.other"},
		{"GET /federation/blob/{name}", "GET", "/federation/blob/abc", "federation.blob"},
		{"/federation/v1/proxy/", "GET", "/federation/v1/proxy/anything", "federation.proxy"},
		{"GET /federation/v1/proxy/feeds/{fed}/{slug}/{rest...}", "GET", "/federation/v1/proxy/feeds/a/b/c", "federation.proxy"},
		{"GET /federation/v1/replication/feeds/{fed}/{slug}", "GET", "/federation/v1/replication/feeds/a/b", "federation.replication"},
		{"POST /federation/v1/search", "POST", "/federation/v1/search", "federation.search"},
		{"POST /federation/v1/join", "POST", "/federation/v1/join", "federation.control"},
		// Existing routes are untouched.
		{"GET /api/v1/feeds/{id}", "GET", "/api/v1/feeds/x", "/api/v1/feeds/{id}"},
		{"/health/live", "GET", "/health/live", "/health/live"},
		// Unknown formats, nested patterns and unregistered patterns are never trusted.
		{"/feeds/{slug}/madeup/", "GET", "/feeds/s/madeup/x", "unmatched"},
		{"/feeds/{slug}/npm/extra/", "GET", "/feeds/s/npm/extra/x", "unmatched"},
		{"/feeds/{slug}/", "GET", "/feeds/s/", "unmatched"},
		{"", "GET", "/anything", "unmatched"},
		{"/v2/library/alpine", "GET", "/v2/library/alpine", "unmatched"},
	} {
		r := httptest.NewRequest(tc.method, "http://ant.test"+tc.path, nil)
		if got := RouteFor(tc.pattern, r); got != tc.want {
			t.Errorf("%s %s (%q): got %q, want %q", tc.method, tc.path, tc.pattern, got, tc.want)
		}
	}
}

// Hostile and random paths cannot create new series: every label is one of the
// fixed <format>.<class> or oci.<class> names.
func TestRouteForCardinalityIsBounded(t *testing.T) {
	classes := map[string]bool{"metadata": true, "download": true, "publish": true, "delete": true, "other": true}
	ociClasses := map[string]bool{"base": true, "catalog": true, "upload": true, "blob": true, "manifest": true, "tags": true, "referrers": true, "other": true}
	seen := map[string]bool{}
	methods := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CUSTOM"}
	for i := range 2000 {
		slug := fmt.Sprintf("slug-%d", i)
		tail := strings.Repeat(fmt.Sprintf("seg%d/", i), i%5) + fmt.Sprintf("file-%d.%s", i, []string{"tgz", "json", "xml", "", "whl", "jar"}[i%6])
		for format := range formats {
			r := httptest.NewRequest(methods[i%len(methods)], "http://ant.test/feeds/"+slug+"/"+format+"/"+tail, nil)
			got := RouteFor("/feeds/{slug}/"+format+"/", r)
			f, c, ok := strings.Cut(got, ".")
			if !ok || f != format || !classes[c] {
				t.Fatalf("unbounded route %q", got)
			}
			seen[got] = true
		}
		r := httptest.NewRequest("GET", fmt.Sprintf("http://ant.test/v2/org%d/img%d/%s/ref%d", i, i, []string{"blobs", "manifests", "tags", "x"}[i%4], i), nil)
		got := RouteFor("/v2/", r)
		if c, ok := strings.CutPrefix(got, "oci."); !ok || !ociClasses[c] {
			t.Fatalf("unbounded oci route %q", got)
		}
		seen[got] = true
	}
	if limit := len(formats)*len(classes) + len(ociClasses); len(seen) > limit {
		t.Fatalf("%d series exceed the bound %d", len(seen), limit)
	}
}

// Through the real middleware and a real ServeMux, with the same resolve
// function the server uses, adapter traffic no longer lands in "unmatched".
func TestWrapLabelsPackageTraffic(t *testing.T) {
	h := NewHTTP()
	mux := http.NewServeMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.Handle("/v2/", ok)
	mux.Handle("/feeds/{slug}/npm/", ok)
	mux.Handle("/feeds/{slug}/maven/", ok)
	mux.Handle("GET /federation/blob/{name}", ok)
	resolve := func(r *http.Request) string { _, p := mux.Handler(r); return p }
	handler := h.Wrap(slog.New(slog.DiscardHandler), resolve, mux)
	for _, target := range []string{"/feeds/a/npm/left-pad", "/feeds/b/npm/right-pad", "/feeds/a/maven/g/a/1/a-1.jar", "/v2/x/y/manifests/latest", "/v2/x/y/blobs/sha256:1", "/federation/blob/n", "/nowhere"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", target, nil))
	}
	metrics := scrape(h)
	for _, want := range []string{
		`ant_http_requests_total{route="npm.metadata",method="GET",status_class="2xx"} 2`,
		`ant_http_requests_total{route="maven.download",method="GET",status_class="2xx"} 1`,
		`ant_http_requests_total{route="oci.manifest",method="GET",status_class="2xx"} 1`,
		`ant_http_requests_total{route="oci.blob",method="GET",status_class="2xx"} 1`,
		`ant_http_requests_total{route="federation.blob",method="GET",status_class="2xx"} 1`,
		`ant_http_requests_total{route="unmatched",method="GET",status_class="4xx"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %s in\n%s", want, metrics)
		}
	}
	if strings.Contains(metrics, "left-pad") || strings.Contains(metrics, "sha256") {
		t.Fatal("request content leaked into labels")
	}
}
