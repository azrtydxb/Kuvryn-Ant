package maven

import (
	"context"
	"io"
	"net/http"
	"time"
)

const idleTimeout = 30 * time.Second

// Reset progress deadlines rather than inheriting the control API's fixed
// overall timeout. TLS/header limits remain the owning server's responsibility.
type idleBody struct {
	io.ReadCloser
	ctx        context.Context
	controller *http.ResponseController
}

func (b idleBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	_ = b.controller.SetReadDeadline(time.Now().Add(idleTimeout))
	return b.ReadCloser.Read(p)
}

type idleWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
}

func (w *idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *idleWriter) WriteHeader(code int) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(idleTimeout))
	w.ResponseWriter.WriteHeader(code)
}
func (w *idleWriter) Write(p []byte) (int, error) {
	_ = w.controller.SetWriteDeadline(time.Now().Add(idleTimeout))
	return w.ResponseWriter.Write(p)
}
func (w *idleWriter) ReadFrom(r io.Reader) (int64, error) {
	// Hide ReaderFrom to prevent io.CopyBuffer from recursing.
	return io.CopyBuffer(struct{ io.Writer }{w}, r, make([]byte, 64*1024))
}
