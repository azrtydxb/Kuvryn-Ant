package prewarm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// errAborted marks a response the handler abandoned mid-stream (a truncated or
// digest-mismatching upstream transfer aborts the connection).
var errAborted = errors.New("response aborted by the handler")

// response is an in-process HTTP exchange with a streaming body.
type response struct {
	status int
	header http.Header
	body   io.ReadCloser
	cancel context.CancelFunc
}

func (r *response) Close() error {
	r.cancel()
	return r.body.Close()
}

type pipeResponseWriter struct {
	header http.Header
	snap   http.Header
	status int
	ready  chan struct{}
	once   sync.Once
	pw     *io.PipeWriter
}

func (p *pipeResponseWriter) Header() http.Header { return p.header }
func (p *pipeResponseWriter) WriteHeader(code int) {
	p.once.Do(func() {
		p.status, p.snap = code, p.header.Clone()
		close(p.ready)
	})
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return p.pw.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// serve dispatches one GET to the feed data plane as principal, exactly as an
// authenticated package client would, and returns once the response headers are
// known. The body streams; the caller must Close the response.
func serve(ctx context.Context, h http.Handler, principal identity.Principal, rawPath, accept string) (*response, error) {
	u, err := url.Parse("http://prewarm.invalid" + rawPath)
	if err != nil {
		return nil, errors.New("invalid request path")
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("User-Agent", "kuvryn-ant-prewarm")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	ticket, drop := identity.IssueTicket(principal)
	req.Header.Set("Authorization", "Bearer "+ticket)
	pr, pw := io.Pipe()
	w := &pipeResponseWriter{header: http.Header{}, ready: make(chan struct{}), pw: pw}
	go func() {
		defer drop()
		defer func() {
			if recover() != nil {
				pw.CloseWithError(errAborted)
			} else {
				pw.Close()
			}
			w.WriteHeader(http.StatusOK)
		}()
		h.ServeHTTP(w, req)
	}()
	select {
	case <-w.ready:
		return &response{status: w.status, header: w.snap, body: pr, cancel: cancel}, nil
	case <-ctx.Done():
		cancel()
		pr.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
}
