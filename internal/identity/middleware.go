package identity

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
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

// WithAuthenticatedPrincipal is only for trusted in-process session middleware,
// after authenticating the session and enforcing its CSRF/origin requirements.
// Request headers and cookies must never be copied directly into this context.
func WithAuthenticatedPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

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
	// maxInflight bounds concurrent credential checks per peer separately from
	// the failure budget: behind an ingress every client shares one peer, and
	// package managers legitimately open dozens of parallel authenticated requests.
	maxInflight     int
	window, timeout time.Duration
	now             func() time.Time
}

func NewBearer(auth Authenticator) *Bearer {
	return &Bearer{auth: auth, attempts: make(map[string]*attempt), maxEntries: 4096, maxFailures: 10, maxInflight: 512, window: time.Minute, timeout: 2 * time.Second, now: time.Now}
}
func (b *Bearer) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Authorization")) == 0 {
			if p, ok := PrincipalFromContext(r.Context()); ok && p.PrincipalID != "" && p.InstanceID != "" && p.TokenID == "" && p.Scopes == nil {
				NoteAuthenticated(r.Context(), p)
				next.ServeHTTP(w, r)
				return
			}
		}
		values := r.Header.Values("Authorization")
		if len(values) == 0 {
			// A credential-free request is the expected first leg of a challenge
			// flow (docker, npm, ...). It verifies nothing, so it neither consumes
			// nor is blocked by the failure budget.
			w.Header().Set("WWW-Authenticate", `Bearer realm="kuvryn-ant"`)
			authError(w, r, http.StatusUnauthorized, api.ErrorCodeUnauthenticated, "authentication required")
			return
		}
		// Failures are budgeted per (client, presented username) with a looser
		// aggregate per client, so one misconfigured client cannot lock out others
		// that share an address and a guessing client cannot rotate usernames.
		client := ClientKey(r)
		entry, ok := b.reserve("u:"+client+"|"+usernameHint(r, values), "ip:"+client)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(b.window.Seconds())+1))
			authError(w, r, http.StatusTooManyRequests, api.ErrorCodeUnavailable, "authentication attempts limited")
			return
		}
		success := false
		defer func() { b.finish(entry, success) }()
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
				valid = err == nil && p.PrincipalID != "" && p.InstanceID != "" && (p.TokenID != "" || p.ticket)
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
		NoteAuthenticated(r.Context(), p)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
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
