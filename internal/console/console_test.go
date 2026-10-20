package console

import (
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShellSecurityAndRouting(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/console/", 200}, {"HEAD", "/console/", 200}, {"GET", "/console", 308},
		{"GET", "/console/index.html", 200}, {"POST", "/console/", 405},
		{"GET", "/api/v1/no-such-operation", 404}, {"GET", "/console/assets/", 404},
		{"GET", "/console/assets/missing.js", 404}, {"GET", "/console/../console.go", 404},
		{"GET", "/console/assets/../../console.go", 404}, {"GET", "/console/package.json", 404},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status %d, expected %d", w.Code, tc.status)
			}
			csp := w.Header().Get("Content-Security-Policy")
			for _, part := range []string{"default-src 'none'", "frame-ancestors 'none'", "connect-src 'self'", "form-action 'none'", "img-src 'self' https:"} {
				if !strings.Contains(csp, part) {
					t.Errorf("missing CSP %s", part)
				}
			}
			if strings.Contains(csp, "unsafe-") {
				t.Fatal("unsafe CSP")
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing security/cache headers")
			}
			if tc.method == "HEAD" && w.Body.Len() != 0 {
				t.Fatal("HEAD emitted a body")
			}
			if tc.method == "GET" && tc.status == 200 && !strings.Contains(w.Body.String(), "/console/assets/") {
				t.Fatal("shell must reference built assets")
			}
		})
	}
}
func TestEmbeddedAssets(t *testing.T) {
	count := 0
	err := fs.WalkDir(assets, "dist/assets", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		count++
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest("GET", "/console/"+strings.TrimPrefix(path, "dist/"), nil))
		if w.Code != 200 {
			t.Errorf("asset %s returned %d", path, w.Code)
		}
		if w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Error("asset is not immutable")
		}
		if strings.HasSuffix(path, ".js") && !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
			t.Error("wrong script MIME")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Fatal("missing built JS/CSS")
	}
}
