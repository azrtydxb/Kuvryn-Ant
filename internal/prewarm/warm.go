package prewarm

import (
	"context"
	"crypto/sha1" // #nosec G505 -- npm lockfiles may pin sha1 integrity values.
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Target is what a run warms: one feed and the principal it acts as.
type Target struct {
	FeedID, Slug, Format string
	Principal            identity.Principal
	Options              Options
}

// Warmer warms items through the feed data plane.
type Warmer struct {
	// Feeds serves /feeds/{slug}/<format>/... and /v2/... exactly as the public
	// listener would, without any middleware in front of it.
	Feeds http.Handler
	// RequestTimeout bounds one request, including its body (default 30 minutes).
	RequestTimeout time.Duration
}

// Result accumulates what warming one item did.
type Result struct {
	bytes  atomic.Int64
	filled atomic.Int64 // objects fetched from an upstream while warming the item
}

// Bytes is the number of bytes read from the feed.
func (r *Result) Bytes() int64 { return r.bytes.Load() }

// Filled is the number of objects the feed had to fetch from an upstream.
func (r *Result) Filled() int64 { return r.filled.Load() }

func (r *Result) addBytes(n int64) { r.bytes.Add(n) }

// Filled implements artifact.FillObserver.
func (r *Result) ObserveFill(int64) { r.filled.Add(1) }

// ItemError is a failed or skipped item with a code from the closed set.
type ItemError struct {
	Code, Detail string
	Skip         bool
}

func (e *ItemError) Error() string {
	if e.Detail != "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code
}

func itemErr(code, detail string) error { return &ItemError{Code: code, Detail: detail} }

func statusError(status int) error {
	switch {
	case status == http.StatusNotFound || status == http.StatusGone:
		return itemErr(ErrNotFound, "")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return itemErr(ErrDenied, "")
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return itemErr(ErrTimeout, "")
	default:
		return itemErr(ErrUnavail, fmt.Sprintf("status %d", status))
	}
}

// Retryable reports whether a failure is worth another attempt.
func Retryable(err error) bool {
	var ie *ItemError
	return errors.As(err, &ie) && (ie.Code == ErrUnavail || ie.Code == ErrTimeout)
}

// expectation is the integrity data a source pins for the bytes of one request.
type expectation struct {
	sha256 []string // hex
	sha512 []string // hex
	sha1   []string // hex
	// check, when set, receives the complete body from a temporary file for
	// formats whose hash is not a plain digest (the go.sum h1: hash of a zip).
	check func(path string) error
}

func (e expectation) empty() bool {
	return len(e.sha256)+len(e.sha512)+len(e.sha1) == 0 && e.check == nil
}

type hashers struct {
	sha256, sha512, sha1 hash.Hash
}

func newHashers(e expectation) (hashers, io.Writer) {
	var h hashers
	var ws []io.Writer
	if len(e.sha256) > 0 {
		h.sha256 = sha256.New()
		ws = append(ws, h.sha256)
	}
	if len(e.sha512) > 0 {
		h.sha512 = sha512.New()
		ws = append(ws, h.sha512)
	}
	if len(e.sha1) > 0 {
		h.sha1 = sha1.New() // #nosec G401
		ws = append(ws, h.sha1)
	}
	if len(ws) == 0 {
		return h, io.Discard
	}
	return h, io.MultiWriter(ws...)
}

func (h hashers) verify(e expectation) error {
	match := func(hs hash.Hash, want []string) bool {
		if hs == nil {
			return true
		}
		got := hex.EncodeToString(hs.Sum(nil))
		for _, w := range want {
			if strings.EqualFold(w, got) {
				return true
			}
		}
		return false
	}
	if !match(h.sha256, e.sha256) || !match(h.sha512, e.sha512) || !match(h.sha1, e.sha1) {
		return itemErr(ErrIntegrity, "the bytes do not match the pinned hash")
	}
	return nil
}

// parseIntegrity understands SRI values (sha512-<base64>, space separated),
// algo:hex values (comma separated) and plain hex sha256 values.
func parseIntegrity(s string) expectation {
	var e expectation
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' }) {
		switch {
		case strings.HasPrefix(part, "sha512-"):
			if raw, err := base64.StdEncoding.DecodeString(part[len("sha512-"):]); err == nil && len(raw) == 64 {
				e.sha512 = append(e.sha512, hex.EncodeToString(raw))
			}
		case strings.HasPrefix(part, "sha256-"):
			if raw, err := base64.StdEncoding.DecodeString(part[len("sha256-"):]); err == nil && len(raw) == 32 {
				e.sha256 = append(e.sha256, hex.EncodeToString(raw))
			}
		case strings.HasPrefix(part, "sha1-"):
			if raw, err := base64.StdEncoding.DecodeString(part[len("sha1-"):]); err == nil && len(raw) == 20 {
				e.sha1 = append(e.sha1, hex.EncodeToString(raw))
			}
		case strings.HasPrefix(part, "sha256:") && len(part) == len("sha256:")+64:
			e.sha256 = append(e.sha256, strings.ToLower(part[len("sha256:"):]))
		}
	}
	// A weaker hash is checked only when nothing stronger is pinned.
	if len(e.sha512) > 0 || len(e.sha256) > 0 {
		e.sha1 = nil
	}
	return e
}

// get requests one path from the feed and reads the body to its end, so the
// adapter's pull-through completes (and caches) before this returns.
func (w *Warmer) get(ctx context.Context, t Target, path, accept string, exp expectation, res *Result) error {
	_, err := w.getBody(ctx, t, path, accept, exp, res, 0)
	return err
}

// getBody is get that also returns up to keep bytes of the body (documents).
func (w *Warmer) getBody(ctx context.Context, t Target, path, accept string, exp expectation, res *Result, keep int64) ([]byte, error) {
	timeout := w.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(artifact.WithFillObserver(ctx, res), timeout)
	defer cancel()
	resp, err := serve(ctx, w.Feeds, t.Principal, path, accept)
	if err != nil {
		if ctx.Err() != nil {
			return nil, itemErr(ErrTimeout, "")
		}
		return nil, itemErr(ErrInternal, "")
	}
	defer resp.Close()
	if resp.status != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.body, 64<<10))
		return nil, statusError(resp.status)
	}
	h, sink := newHashers(exp)
	var tmp *spool
	if exp.check != nil {
		if tmp, err = newSpool(); err != nil {
			return nil, itemErr(ErrInternal, "")
		}
		defer tmp.Close()
		sink = io.MultiWriter(sink, tmp)
	}
	var cw *capWriter
	if keep > 0 {
		cw = &capWriter{limit: keep}
		sink = io.MultiWriter(sink, cw)
	}
	n, err := io.Copy(sink, resp.body)
	res.addBytes(n)
	if err != nil {
		return nil, bodyError(ctx, err)
	}
	if err = h.verify(exp); err != nil {
		return nil, err
	}
	if exp.check != nil {
		if err = exp.check(tmp.Name()); err != nil {
			return nil, err
		}
	}
	if cw != nil {
		if cw.over {
			return nil, itemErr(ErrUnavail, "document too large")
		}
		return cw.buf, nil
	}
	return nil, nil
}

func bodyError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return itemErr(ErrTimeout, "")
	}
	return itemErr(ErrUnavail, "the transfer was interrupted")
}

type capWriter struct {
	limit int64
	buf   []byte
	over  bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(c.buf)+len(p)) > c.limit {
		c.over = true
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func escapePath(segments ...string) string {
	parts := make([]string, 0, len(segments))
	for _, s := range segments {
		for _, p := range strings.Split(s, "/") {
			parts = append(parts, url.PathEscape(p))
		}
	}
	return strings.Join(parts, "/")
}

func feedPath(t Target, format string, rest ...string) string {
	return "/feeds/" + url.PathEscape(t.Slug) + "/" + format + "/" + strings.Join(rest, "/")
}

// spool is a temporary file holding a body for hash schemes that need a file.
type spool struct{ f *os.File }

func newSpool() (*spool, error) {
	f, err := os.CreateTemp("", "ant-prewarm-*")
	if err != nil {
		return nil, err
	}
	return &spool{f}, nil
}
func (s *spool) Write(p []byte) (int, error) { return s.f.Write(p) }
func (s *spool) Name() string                { return s.f.Name() }
func (s *spool) Close() error {
	name := s.f.Name()
	err := s.f.Close()
	_ = os.Remove(name)
	return err
}
