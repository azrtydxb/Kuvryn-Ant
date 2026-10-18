package identity

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// aggregateFactor is how much looser the per-client budget is than the
// per-(client, username) budget.
const aggregateFactor = 10

// usernameHint returns the Basic-auth username presented (bounded), or "" for
// bearer credentials. It only partitions the failure budget; it is never trusted.
func usernameHint(req *http.Request, values []string) string {
	if u, ok := req.Context().Value(usernameKey{}).(string); ok {
		return clipUser(u)
	}
	if len(values) != 1 || len(values[0]) > 1024 {
		return ""
	}
	r := http.Request{Header: http.Header{"Authorization": values}}
	user, _, ok := r.BasicAuth()
	if !ok {
		return ""
	}
	return clipUser(user)
}

func clipUser(u string) string {
	if len(u) > 64 {
		return u[:64]
	}
	return u
}

type usernameKey struct{}

// WithUsernameHint records the username a protocol adapter extracted from
// Basic credentials before rewriting them into a bearer token, so the failure
// budget can partition by it.
func WithUsernameHint(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, usernameKey{}, user)
}

func (b *Bearer) limitFor(key string) int {
	if strings.HasPrefix(key, "ip:") {
		return b.maxFailures * aggregateFactor
	}
	return b.maxFailures
}

// reserve admits one attempt against every key or none.
func (b *Bearer) reserve(keys ...string) ([]*attempt, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	entries := make([]*attempt, 0, len(keys))
	for _, key := range keys {
		e := b.attempts[key]
		if e != nil && e.inflight == 0 && !now.Before(e.started.Add(b.window)) {
			delete(b.attempts, key)
			e = nil
		}
		if e == nil {
			if len(b.attempts) >= b.maxEntries {
				oldest := ""
				var stamp time.Time
				for k, v := range b.attempts {
					if v.inflight == 0 && (oldest == "" || v.started.Before(stamp)) {
						oldest, stamp = k, v.started
					}
				}
				if oldest == "" {
					return nil, false
				}
				delete(b.attempts, oldest)
			}
			e = &attempt{started: now}
			b.attempts[key] = e
		}
		if e.failures >= b.limitFor(key) || e.inflight >= b.maxInflight {
			return nil, false
		}
		entries = append(entries, e)
	}
	for _, e := range entries {
		e.inflight++
	}
	return entries, true
}

func (b *Bearer) finish(entries []*attempt, success bool) {
	if len(entries) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range entries {
		e.inflight--
		if !success {
			e.failures++
		}
	}
}
