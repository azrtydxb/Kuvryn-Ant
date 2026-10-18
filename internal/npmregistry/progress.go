package npmregistry

import (
	"io"
	"net/http"
	"time"
)

// Refresh deadlines on actual transfer progress, including chunked uploads.
type progressBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (p *progressBody) Read(b []byte) (int, error) {
	if len(b) > 64<<10 {
		b = b[:64<<10]
	}
	_ = p.controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	return p.ReadCloser.Read(b)
}

type progressWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
}

func (p *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 64<<10 {
			chunk = chunk[:64<<10]
		}
		_ = p.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		n, e := p.ResponseWriter.Write(chunk)
		total += n
		b = b[n:]
		if e != nil {
			return total, e
		}
		if n != len(chunk) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
func (p *progressWriter) WriteHeader(n int) {
	_ = p.controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	p.ResponseWriter.WriteHeader(n)
}
