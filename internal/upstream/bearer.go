package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry bearer-token handshake (the Docker/OCI "token authentication"
// scheme). An upstream answers 401 with
//
//	WWW-Authenticate: Bearer realm="https://auth.example/token",service="s",scope="repository:a/b:pull"
//
// The fetcher requests a token from the realm (anonymously, or with the
// source's configured Basic credential), caches it per (realm, service, scope)
// until it expires, and retries the request with "Authorization: Bearer".
//
// Trust rules: the realm must be the source origin or one of its configured
// redirect origins (so the handshake adds no egress surface), the realm
// response is never redirected, a configured Basic credential is sent only to
// such a realm, and the bearer token is attached only to requests for the
// source origin, never to a redirected CDN host. Token failures are cached for
// a few seconds at most so a corrected credential takes effect promptly.

// ErrAuth reports that the upstream refused to issue or accept a token.
var ErrAuth = errors.New("upstream authentication failed")

// RateLimitError is returned when the upstream (or its token service) answers
// 429. RetryAfter is zero when the upstream gave no usable hint.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return "upstream rate limited" }
func (e *RateLimitError) Is(target error) bool {
	return target == ErrUnavailable
}

const (
	maxTokenBody     = 64 << 10
	maxTokens        = 256
	defaultTokenLife = 60 * time.Second
	maxTokenLife     = time.Hour
	authFailureTTL   = 5 * time.Second
)

type challenge struct{ realm, service, scope string }

func (c challenge) key() string { return c.realm + "\x00" + c.service + "\x00" + c.scope }

type token struct {
	value   string
	expires time.Time
}

type tokenCall struct {
	done chan struct{}
	tok  token
	err  error
}

type tokenState struct {
	mu       sync.Mutex
	tokens   map[string]token
	failures map[string]time.Time
	calls    map[string]*tokenCall
	hint     challenge // realm and service last advertised, for preemptive use
}

var paramPattern = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9_-]*)\s*=\s*`)

// parseChallenge extracts the first Bearer challenge from WWW-Authenticate.
func parseChallenge(values []string) (challenge, bool) {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < 7 || !strings.EqualFold(v[:6], "bearer") || (v[6] != ' ' && v[6] != '\t') {
			continue
		}
		rest := v[7:]
		var c challenge
		for {
			m := paramPattern.FindStringSubmatchIndex(rest)
			if m == nil {
				break
			}
			name := strings.ToLower(rest[m[2]:m[3]])
			rest = rest[m[1]:]
			var val string
			if strings.HasPrefix(rest, `"`) {
				var b strings.Builder
				i := 1
				for ; i < len(rest) && rest[i] != '"'; i++ {
					if rest[i] == '\\' && i+1 < len(rest) {
						i++
					}
					b.WriteByte(rest[i])
				}
				if i >= len(rest) {
					return challenge{}, false
				}
				val = b.String()
				rest = rest[i+1:]
			} else {
				i := strings.IndexByte(rest, ',')
				if i < 0 {
					i = len(rest)
				}
				val = strings.TrimSpace(rest[:i])
				rest = rest[i:]
			}
			switch name {
			case "realm":
				c.realm = val
			case "service":
				c.service = val
			case "scope":
				c.scope = val
			}
			rest = strings.TrimLeft(rest, ", \t")
		}
		if c.realm != "" {
			return c, true
		}
	}
	return challenge{}, false
}

var repoPath = regexp.MustCompile(`^/v2/(.+)/(?:manifests|blobs|tags|referrers)/`)

// scopeFor derives the pull scope a registry would demand for a request path.
func scopeFor(path string) string {
	m := repoPath.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return "repository:" + m[1] + ":pull"
}

func (f *Fetcher) cachedToken(u *url.URL) (token, string) {
	s := &f.tokens
	s.mu.Lock()
	defer s.mu.Unlock()
	scope := scopeFor(u.Path)
	if scope == "" || s.hint.realm == "" {
		return token{}, ""
	}
	key := challenge{s.hint.realm, s.hint.service, scope}.key()
	t, ok := s.tokens[key]
	if !ok || !f.cfg.Now().Before(t.expires) {
		delete(s.tokens, key)
		return token{}, ""
	}
	return t, key
}

func (f *Fetcher) dropToken(key string) {
	f.tokens.mu.Lock()
	delete(f.tokens.tokens, key)
	f.tokens.mu.Unlock()
}

// token returns a bearer token for c, fetching one (coalesced across callers)
// when none is cached or fresh is set.
func (f *Fetcher) token(ctx context.Context, c challenge, fresh bool) (token, error) {
	s := &f.tokens
	key := c.key()
	s.mu.Lock()
	s.hint = challenge{realm: c.realm, service: c.service}
	if !fresh {
		if t, ok := s.tokens[key]; ok && f.cfg.Now().Before(t.expires) {
			s.mu.Unlock()
			return t, nil
		}
	}
	if until, ok := s.failures[key]; ok {
		if f.cfg.Now().Before(until) {
			s.mu.Unlock()
			return token{}, ErrAuth
		}
		delete(s.failures, key)
	}
	if call := s.calls[key]; call != nil {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.tok, call.err
		case <-ctx.Done():
			return token{}, ctx.Err()
		}
	}
	call := &tokenCall{done: make(chan struct{})}
	if s.calls == nil {
		s.calls = map[string]*tokenCall{}
	}
	s.calls[key] = call
	s.mu.Unlock()

	// The leader must not be cancelled by one waiter's context going away
	// first, but it is bounded by its own timeout.
	call.tok, call.err = f.requestToken(ctx, c)

	s.mu.Lock()
	delete(s.calls, key)
	switch {
	case call.err == nil:
		if s.tokens == nil || len(s.tokens) >= maxTokens {
			s.tokens = map[string]token{}
		}
		s.tokens[key] = call.tok
	case errors.Is(call.err, ErrAuth):
		if s.failures == nil || len(s.failures) >= maxTokens {
			s.failures = map[string]time.Time{}
		}
		s.failures[key] = f.cfg.Now().Add(authFailureTTL)
	}
	s.mu.Unlock()
	close(call.done)
	return call.tok, call.err
}

func (f *Fetcher) basicCredential() string {
	v := f.cfg.Credentials.Get("Authorization")
	if len(v) > 6 && strings.EqualFold(v[:6], "basic ") {
		return v
	}
	return ""
}

func (f *Fetcher) requestToken(ctx context.Context, c challenge) (token, error) {
	realm, err := url.Parse(c.realm)
	if err != nil {
		return token{}, ErrPolicy
	}
	realm, err = parse(realm.String(), f.cfg.AllowHTTP)
	if err != nil || realm.Fragment != "" {
		return token{}, ErrPolicy
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = f.permitted(check, realm)
	cancel()
	if err != nil {
		return token{}, err
	}
	q := realm.Query()
	if c.service != "" {
		q.Set("service", c.service)
	}
	if c.scope != "" {
		q.Set("scope", c.scope)
	}
	realm.RawQuery = q.Encode()
	ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return token{}, ErrPolicy
	}
	req.Header.Set("Accept", "application/json")
	if basic := f.basicCredential(); basic != "" {
		req.Header.Set("Authorization", basic)
	}
	resp, err := f.client.Do(req) // redirects are not followed
	if err != nil {
		if ctx.Err() != nil {
			return token{}, ctx.Err()
		}
		return token{}, ErrUnavailable
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return token{}, rateLimit(resp)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return token{}, ErrAuth
	case resp.StatusCode != http.StatusOK:
		return token{}, ErrUnavailable
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxTokenBody)).Decode(&body) != nil {
		return token{}, ErrUnavailable
	}
	value := body.Token
	if value == "" {
		value = body.AccessToken
	}
	if value == "" || len(value) > 8192 || strings.ContainsAny(value, "\r\n") {
		return token{}, ErrAuth
	}
	life := defaultTokenLife
	if body.ExpiresIn > 0 {
		life = time.Duration(body.ExpiresIn) * time.Second
	}
	life = min(life, maxTokenLife)
	// Refresh slightly early so a token never expires mid-request.
	life -= min(life/10, 10*time.Second)
	return token{value: value, expires: f.cfg.Now().Add(life)}, nil
}

func rateLimit(resp *http.Response) *RateLimitError {
	e := &RateLimitError{}
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		e.RetryAfter = min(time.Duration(n)*time.Second, time.Hour)
	}
	return e
}

// bearerRetry handles a 401 for a request to the source origin. It returns the
// response to hand back to the caller (the original 401 when no token could be
// obtained or the token was refused).
func (f *Fetcher) bearerRetry(ctx context.Context, method string, u *url.URL, headers http.Header, first *http.Response, usedKey string) (*http.Response, error) {
	c, ok := parseChallenge(first.Header.Values("WWW-Authenticate"))
	if !ok {
		return first, nil
	}
	if c.scope == "" {
		c.scope = scopeFor(u.Path)
	}
	if usedKey != "" {
		f.dropToken(usedKey) // the cached token was refused
	}
	tok, err := f.token(ctx, c, usedKey != "")
	if err != nil {
		if errors.Is(err, ErrAuth) || errors.Is(err, ErrPolicy) || errors.Is(err, ErrUnavailable) {
			var rl *RateLimitError
			if errors.As(err, &rl) {
				first.Body.Close()
				return nil, err
			}
			return first, nil
		}
		first.Body.Close()
		return nil, err
	}
	first.Body.Close()
	return f.once(ctx, method, u, headers, "Bearer "+tok.value)
}
