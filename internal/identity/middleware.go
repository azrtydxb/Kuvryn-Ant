package identity

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/api"
)

type Authenticator interface {
	Authenticate(context.Context, string) (Principal, error)
}
type principalKey struct{}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

type attempt struct {
	started            time.Time
	failures, inflight int
}

// Bearer requires credentials; it must only wrap protected routes. Public status
// and explicitly anonymous reads are routed separately. The authenticator must
// honor context cancellation. Only the TCP peer is trusted for rate limiting.
type Bearer struct {
	auth                    Authenticator
	mu                      sync.Mutex
	attempts                map[string]*attempt
	maxEntries, maxFailures int
	window, timeout         time.Duration
	now                     func() time.Time
}

func NewBearer(auth Authenticator) *Bearer {
	return &Bearer{auth: auth, attempts: make(map[string]*attempt), maxEntries: 4096, maxFailures: 10, window: time.Minute, timeout: 2 * time.Second, now: time.Now}
}
func (b *Bearer) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := peerIP(r.RemoteAddr)
		entry, ok := b.reserve(key)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(b.window.Seconds())+1))
			authError(w, r, http.StatusTooManyRequests, api.ErrorCodeUnavailable, "authentication attempts limited")
			return
		}
		success := false
		defer func() { b.finish(entry, success) }()
		values := r.Header.Values("Authorization")
		var p Principal
		var err error
		valid := len(values) == 1 && len(values[0]) <= 1024
		if valid {
			fields := strings.Fields(values[0])
			valid = len(fields) == 2 && strings.EqualFold(fields[0], "Bearer")
			if valid {
				ctx, cancel := context.WithTimeout(r.Context(), b.timeout)
				p, err = b.auth.Authenticate(ctx, fields[1])
				cancel()
				if errors.Is(err, ErrUnavailable) {
					success = true // Infrastructure faults are not credential failures.
					authError(w, r, http.StatusServiceUnavailable, api.ErrorCodeUnavailable, "authentication unavailable")
					return
				}
				valid = err == nil && p.PrincipalID != "" && p.InstanceID != "" && p.TokenID != ""
			}
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kuvryn-ant"`)
			authError(w, r, http.StatusUnauthorized, api.ErrorCodeUnauthenticated, "authentication required")
			return
		}
		success = true
		// Release the admission reservation before running application work.
		b.finish(entry, true)
		entry = nil
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
func peerIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return ip.Unmap().WithZone("").String()
}
func (b *Bearer) reserve(key string) (*attempt, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
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
	if e.failures+e.inflight >= b.maxFailures {
		return nil, false
	}
	e.inflight++
	return e, true
}
func (b *Bearer) finish(e *attempt, success bool) {
	if e == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e.inflight--
	if !success {
		e.failures++
	}
}

func authError(w http.ResponseWriter, r *http.Request, status int, code api.ErrorCode, message string) {
	id := w.Header().Get("X-Request-ID")
	if id == "" {
		id = rand.Text()
		w.Header().Set("X-Request-ID", id)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Error{Code: code, Message: message, RequestId: id})
}
