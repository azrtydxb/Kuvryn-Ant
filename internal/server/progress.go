package server

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// streamIdle is how long a data-plane transfer may go without making progress.
// The server's ReadTimeout and WriteTimeout (10 s) bound the whole exchange,
// which would cut every large upload or download; instead the deadlines move
// forward on each read and write, so a transfer that keeps flowing completes
// and a stalled one is still cut off after streamIdle.
var streamIdle = 60 * time.Second

// withProgressDeadlines applies the idle-based deadlines to every route outside
// the management API, so no streaming handler depends on remembering to extend
// them itself.
func withProgressDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/health/") || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &progressBody{ReadCloser: r.Body, rc: rc}
		}
		next.ServeHTTP(&progressWriter{ResponseWriter: w, rc: rc}, r)
	})
}

type progressWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (p *progressWriter) Write(b []byte) (int, error) {
	_ = p.rc.SetWriteDeadline(time.Now().Add(streamIdle))
	return p.ResponseWriter.Write(b)
}

func (p *progressWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

func (p *progressWriter) Flush() {
	_ = p.rc.SetWriteDeadline(time.Now().Add(streamIdle))
	_ = p.rc.Flush()
}

type progressBody struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (b *progressBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(streamIdle))
	return b.ReadCloser.Read(p)
}
