package oidclogin

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// peerLimiter is per long-lived Client, with no eviction of active entries.
// Saturation denies new peers instead of permitting churn to bypass limits.
// Behind proxies the actual proxy peer shares the limit deliberately; forwarded
// headers must never expand the attacker's request budget.
type peerLimiter struct {
	mu    sync.Mutex
	peers map[string]peerBudget
}
type peerBudget struct {
	until              time.Time
	attempts, failures int
}

func peer(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}
func (l *peerLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.peers == nil {
		l.peers = make(map[string]peerBudget)
	}
	b, ok := l.peers[key]
	if !ok || !now.Before(b.until) {
		for k, v := range l.peers {
			if !now.Before(v.until) {
				delete(l.peers, k)
			}
		}
		if len(l.peers) >= 4096 {
			return false
		}
		b = peerBudget{until: now.Add(time.Minute)}
	}
	if b.attempts >= 20 || b.failures >= 5 {
		return false
	}
	b.attempts++
	l.peers[key] = b
	return true
}
func (l *peerLimiter) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.peers[key]
	if ok {
		b.failures++
		l.peers[key] = b
	}
}
func (c *Client) allowLogin(w http.ResponseWriter, r *http.Request) bool {
	if c.limiter.allow(peer(r), time.Now()) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "browser login rate limit exceeded", http.StatusTooManyRequests)
	return false
}
func (c *Client) loginFailure(w http.ResponseWriter, r *http.Request) {
	c.limiter.failed(peer(r))
	if c.Sessions.RecordLoginFailure(r.Context()) != nil {
		http.Error(w, "browser authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	failure(w)
}
