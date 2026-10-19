package federation

import (
	"context"
	"net/http"
	"time"

	"github.com/azrtydxb/Kuvryn-Ant/internal/identity"
)

// Routing abuse limits (docs/planning/phase-2-federation.md §14, D: slow
// clients and fan-out amplification). The global transfer budget
// (RouteMaxStreams) is shared by every caller of an entry member, so it is
// partitioned: one principal and one client address may hold only a few of
// its slots, every routed read is charged against a per-principal token
// bucket, and a proxied transfer must keep a minimum average rate after a
// grace period and finish within a total deadline. A client that stops
// reading (or a holder that stops sending) loses its slot instead of pinning
// it with one byte per write timeout.

// Routing limit defaults (Config fields override them; zero means default).
const (
	DefaultRouteStreamsPerPrincipal = 8                // concurrent proxied streams per principal
	DefaultRouteStreamsPerClient    = 16               // concurrent proxied streams per client address
	DefaultRoutePrincipalRate       = 50               // routed reads per second per principal (burst 2x)
	DefaultRouteMinTransferRate     = 16 << 10         // bytes per second, averaged from the start of the body
	DefaultRouteTransferGrace       = 30 * time.Second // before the minimum rate applies
	DefaultRouteTransferTimeout     = 2 * time.Hour    // total body transfer deadline
	routeBucketLimit                = 8192             // tracked rate buckets before pruning
)

type routeLimits struct {
	perPrincipal, perClient, rate int
	minRate                       int64
	grace, total                  time.Duration
}

func (s *Service) routeLimits() routeLimits {
	c := s.cfg
	l := routeLimits{perPrincipal: c.RouteMaxStreamsPerPrincipal, perClient: c.RouteMaxStreamsPerClient, rate: c.RoutePrincipalRate,
		minRate: c.RouteMinTransferRate, grace: c.RouteTransferGrace, total: c.RouteTransferTimeout}
	if l.perPrincipal <= 0 {
		l.perPrincipal = DefaultRouteStreamsPerPrincipal
	}
	if l.perClient <= 0 {
		l.perClient = DefaultRouteStreamsPerClient
	}
	if l.rate <= 0 {
		l.rate = DefaultRoutePrincipalRate
	}
	if l.minRate <= 0 {
		l.minRate = DefaultRouteMinTransferRate
	}
	if l.grace <= 0 {
		l.grace = DefaultRouteTransferGrace
	}
	if l.total <= 0 {
		l.total = DefaultRouteTransferTimeout
	}
	return l
}

// clientKey is the client address of a routed request (trusted proxies
// honored), or "unknown".
func clientKey(r *http.Request) string { return identity.ClientKey(r) }

// principalKey identifies the caller for per-principal limits: the local
// principal, or the client address for anonymous callers (they share no
// identity, so one anonymous client cannot starve another).
func principalKey(c caller, r *http.Request) string {
	if c.principal.PrincipalID != "" && !c.principal.Anonymous {
		return "p:" + c.principal.PrincipalID
	}
	return "anon:" + clientKey(r)
}

// Stream admission outcomes.
const (
	streamBusy          = "busy"           // the global transfer budget is exhausted
	streamPrincipalBusy = "principal_busy" // the caller holds its share already
	streamClientBusy    = "client_busy"    // the client address holds its share already
)

// acquireStream claims a proxied-stream slot for a principal and client
// address: a global slot plus one of each partition's share. It returns the
// release function, or the refusal outcome.
func (s *Service) acquireStream(principal, client string) (func(), string) {
	streams := s.routeStreams()
	l := s.routeLimits()
	rs := &s.route
	rs.mu.Lock()
	if rs.byPrincipal == nil {
		rs.byPrincipal, rs.byClient = map[string]int{}, map[string]int{}
	}
	if rs.byPrincipal[principal] >= l.perPrincipal {
		rs.mu.Unlock()
		return nil, streamPrincipalBusy
	}
	if rs.byClient[client] >= l.perClient {
		rs.mu.Unlock()
		return nil, streamClientBusy
	}
	select {
	case streams <- struct{}{}:
	default:
		rs.mu.Unlock()
		return nil, streamBusy
	}
	rs.byPrincipal[principal]++
	rs.byClient[client]++
	rs.mu.Unlock()
	return func() {
		rs.mu.Lock()
		if rs.byPrincipal[principal]--; rs.byPrincipal[principal] <= 0 {
			delete(rs.byPrincipal, principal)
		}
		if rs.byClient[client]--; rs.byClient[client] <= 0 {
			delete(rs.byClient, client)
		}
		rs.mu.Unlock()
		<-streams
	}, ""
}

// routeAllow charges one request to a token bucket of rate per second with
// a burst of twice the rate. Idle (full) buckets are pruned when the table
// grows, so many keys cannot reset an active caller's bucket.
func (s *Service) routeAllow(key string, rate int) bool {
	rs := &s.route
	now := time.Now() // monotonic wall time, like the search limiter
	burst := float64(2 * rate)
	rs.limitMu.Lock()
	defer rs.limitMu.Unlock()
	if rs.buckets == nil {
		rs.buckets = map[string]*bucket{}
	}
	b := rs.buckets[key]
	if b == nil {
		if len(rs.buckets) >= routeBucketLimit {
			for k, o := range rs.buckets {
				if o.tokens+now.Sub(o.last).Seconds()*float64(rate) >= burst {
					delete(rs.buckets, k)
				}
			}
		}
		b = &bucket{tokens: burst, last: now}
		if len(rs.buckets) < routeBucketLimit {
			rs.buckets[key] = b
		}
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*float64(rate))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// transferGuard bounds a body transfer: after grace the cumulative average
// rate must stay at or above minRate, and the transfer ends at end.
type transferGuard struct {
	start   time.Time
	grace   time.Duration
	minRate int64
	end     time.Time
}

func (s *Service) newTransferGuard() transferGuard {
	l := s.routeLimits()
	now := time.Now()
	return transferGuard{start: now, grace: l.grace, minRate: l.minRate, end: now.Add(l.total)}
}

// deadline is the latest time by which n bytes must have been transferred.
func (g transferGuard) deadline(n int64) time.Time {
	d := g.start.Add(g.grace)
	if g.minRate > 0 {
		d = d.Add(time.Duration(float64(n) / float64(g.minRate) * float64(time.Second)))
	}
	if !g.end.IsZero() && d.After(g.end) {
		d = g.end
	}
	return d
}

// watchdog cancels the upstream request when the guard deadline for the
// bytes moved so far passes without progress (a stalled holder, or a
// client write that the write deadline already failed).
type watchdog struct {
	g     transferGuard
	timer *time.Timer
}

func (g transferGuard) watch(cancel context.CancelFunc) *watchdog {
	return &watchdog{g: g, timer: time.AfterFunc(time.Until(g.deadline(0)), cancel)}
}

func (w *watchdog) progress(n int64) { w.timer.Reset(time.Until(w.g.deadline(n))) }
func (w *watchdog) stop()            { w.timer.Stop() }
