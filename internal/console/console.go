// Package console serves the built administration console at /console/.
// It does not authenticate API requests or grant access to backend resources.
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed dist
var assets embed.FS

// Handler returns a handler mounted without StripPrefix at /console/ and /console.
// The shell contains no secrets. All management data uses the separately registered
// authenticated API. Build assets with `npm ci && npm run build` inside web/.
func Handler() http.Handler {
	root, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' https:; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/console" {
			http.Redirect(w, r, "/console/", http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/console/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/console/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) || strings.Contains(name, "\\") {
			http.NotFound(w, r)
			return
		}
		// Only the entry document and compiled assets are public. Never list directories
		// or return the SPA shell for missing API routes or asset requests.
		if name != "index.html" && !strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
		info, err := fs.Stat(root, name)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		clone := r.Clone(r.Context())
		urlCopy := *r.URL
		clone.URL = &urlCopy
		clone.URL.Path = "/" + name
		if name == "index.html" {
			clone.URL.Path = "/"
		}
		files.ServeHTTP(w, clone)
	})
}
