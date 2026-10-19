package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxPostBody bounds a query body.
const MaxPostBody = 1 << 20

// Post sends one read-only query to the source origin: a JSON POST whose
// response is a document, not a mutation (the Git LFS batch API with operation
// "download"). It is deliberately narrower than Fetch: the target must be on the
// source origin, only Accept and Content-Type are forwarded, the body is bounded
// and held in memory so a retry can replay it, and only 307/308 redirects to the
// same origin are followed. Callers own and must close a successful response
// body. Anything that changes remote state must never use this.
func (f *Fetcher) Post(ctx context.Context, target string, headers http.Header, body []byte) (*http.Response, error) {
	if len(body) == 0 || len(body) > MaxPostBody {
		return nil, ErrPolicy
	}
	v, e := url.Parse(target)
	if e != nil {
		return nil, ErrPolicy
	}
	u, e := parse(f.origin.ResolveReference(v).String(), f.cfg.AllowHTTP)
	if e != nil {
		return nil, e
	}
	if origin(u) != origin(f.origin) {
		return nil, ErrPolicy
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	e = f.permitted(check, u)
	cancel()
	if e != nil {
		return nil, e
	}
	if !f.admit() {
		return nil, ErrCircuitOpen
	}
	outcome := 0
	defer func() { f.finish(outcome) }()
	for attempt := 0; attempt < 3; attempt++ {
		response, e := f.postOnce(ctx, u, headers, body)
		if e == nil && response.StatusCode == http.StatusTooManyRequests {
			rl := rateLimit(response)
			response.Body.Close()
			return nil, rl
		}
		if e == nil && !retryable(response.StatusCode) {
			outcome = -1
			return response, nil
		}
		if response != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(e, ErrPolicy) {
			return nil, ErrPolicy
		}
		if attempt < 2 {
			jitter, _ := rand.Int(rand.Reader, big.NewInt(100))
			delay := time.Duration(100*(1<<attempt)) * time.Millisecond
			if jitter != nil {
				delay += time.Duration(jitter.Int64()) * time.Millisecond
			}
			if e = f.cfg.Sleep(ctx, delay); e != nil {
				return nil, e
			}
		}
	}
	outcome = 1
	return nil, ErrUnavailable
}

func (f *Fetcher) postOnce(ctx context.Context, start *url.URL, headers http.Header, body []byte) (*http.Response, error) {
	u := start
	for redirects := 0; redirects <= 3; redirects++ {
		check, cancel := context.WithTimeout(ctx, 5*time.Second)
		e := f.permitted(check, u)
		cancel()
		if e != nil {
			return nil, e
		}
		requestCtx, cancelRequest := context.WithCancel(ctx)
		req, e := http.NewRequestWithContext(requestCtx, http.MethodPost, u.String(), bytes.NewReader(body))
		if e != nil {
			cancelRequest()
			return nil, ErrPolicy
		}
		for _, k := range []string{"Accept", "Content-Type"} {
			for _, v := range headers.Values(k) {
				if len(v) > 256 || strings.ContainsAny(v, "\r\n") {
					cancelRequest()
					return nil, ErrPolicy
				}
				req.Header.Add(k, v)
			}
		}
		for k, v := range f.cfg.Credentials {
			req.Header[k] = append([]string(nil), v...)
		}
		headerTimer := time.AfterFunc(30*time.Second, cancelRequest)
		resp, e := f.client.Do(req)
		headerTimer.Stop()
		if e != nil {
			cancelRequest()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrUnavailable
		}
		resp.Body = &responseBody{ReadCloser: resp.Body, ctx: requestCtx, cancel: cancelRequest, timer: time.AfterFunc(30*time.Second, cancelRequest)}
		if resp.StatusCode != http.StatusTemporaryRedirect && resp.StatusCode != http.StatusPermanentRedirect {
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				resp.Body.Close()
				return nil, ErrPolicy
			}
			return resp, nil
		}
		location, e := resp.Location()
		resp.Body.Close()
		if e != nil {
			return nil, ErrPolicy
		}
		next, e := parse(location.String(), f.cfg.AllowHTTP)
		if e != nil || origin(next) != origin(f.origin) {
			return nil, ErrPolicy
		}
		u = next
	}
	return nil, ErrPolicy
}
