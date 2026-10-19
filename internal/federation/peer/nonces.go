package peer

import (
	"sync"
	"time"
)

// MaxNoncesPerMember caps the request nonces remembered per member within
// NonceTTL. A member that exceeds it is refused (429 rate_limited) instead of
// growing the cache; the per-member request rate limit keeps honest members
// far below it.
const MaxNoncesPerMember = 65536

type nonceEntry struct {
	nonce string
	at    time.Time
}

// memberNonces holds one member's nonces in insertion order, so expiry pops
// from the front instead of scanning the table.
type memberNonces struct {
	seen  map[string]time.Time
	order []nonceEntry
	head  int
}

// nonceCache remembers request nonces for NonceTTL, per member.
type nonceCache struct {
	mu      sync.Mutex
	members map[string]*memberNonces
}

// add records a nonce. It reports false with full=false for a replay and
// full=true when the member's cache is at its cap.
func (c *nonceCache) add(member, nonce string, now time.Time) (ok, full bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.members == nil {
		c.members = map[string]*memberNonces{}
	}
	m := c.members[member]
	if m == nil {
		m = &memberNonces{seen: map[string]time.Time{}}
		c.members[member] = m
	}
	for m.head < len(m.order) && now.Sub(m.order[m.head].at) > NonceTTL {
		e := m.order[m.head]
		if t, ok := m.seen[e.nonce]; ok && t.Equal(e.at) {
			delete(m.seen, e.nonce)
		}
		m.order[m.head] = nonceEntry{}
		m.head++
	}
	if m.head > 1024 && m.head*2 > len(m.order) {
		m.order = append([]nonceEntry(nil), m.order[m.head:]...)
		m.head = 0
	}
	if t, ok := m.seen[nonce]; ok && now.Sub(t) <= NonceTTL {
		return false, false
	}
	if len(m.seen) >= MaxNoncesPerMember {
		return false, true
	}
	m.seen[nonce] = now
	m.order = append(m.order, nonceEntry{nonce, now})
	return true, false
}
