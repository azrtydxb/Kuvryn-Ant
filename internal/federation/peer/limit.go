package peer

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// bucket is a token bucket.
type bucket struct {
	tokens float64
	last   time.Time
}

// take refills at rate tokens per second up to burst and takes one token.
func (b *bucket) take(now time.Time, rate float64, burst int) bool {
	if b.last.IsZero() {
		b.tokens = float64(burst)
	} else if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens = min(float64(burst), b.tokens+d*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// keyedLimiter rate-limits per key (remote IP or member) with a bounded
// number of tracked keys. Idle keys are swept at most once per sweep interval,
// so no request pays for a scan of the whole table.
type keyedLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

const (
	maxLimiterKeys = 8192
	sweepInterval  = 10 * time.Second
)

func (l *keyedLimiter) allow(key string, now time.Time, rate float64, burst int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxLimiterKeys && now.Sub(l.lastSweep) >= sweepInterval {
			l.lastSweep = now
			idle := time.Duration(float64(burst)/rate*float64(time.Second)) + time.Second
			for k, x := range l.buckets {
				if now.Sub(x.last) > idle {
					delete(l.buckets, k)
				}
			}
		}
		if len(l.buckets) >= maxLimiterKeys {
			return false // table full of active keys: refuse new ones
		}
		b = &bucket{}
		l.buckets[key] = b
	}
	return b.take(now, rate, burst)
}

// remoteIP is the client address of a request (no proxy headers: the
// federation listener is reached directly or through TLS passthrough).
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
