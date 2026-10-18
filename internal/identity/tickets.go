package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"
	"time"
)

// Tickets carry a principal that the client-authentication middleware has
// already verified into the package adapters' existing Bearer pipeline. A
// ticket is 256 random bits, single use, process-local and lives for seconds,
// so it can neither be guessed nor replayed and never reaches storage.
const ticketTTL = 30 * time.Second

type ticketEntry struct {
	principal Principal
	expires   time.Time
}

var tickets = struct {
	sync.Mutex
	m map[[32]byte]ticketEntry
}{m: map[[32]byte]ticketEntry{}}

func ticketKey(raw string) [32]byte { return sha256.Sum256([]byte(raw)) }

func mintTicket(p Principal) string {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		panic("secure randomness unavailable")
	}
	raw := "ant_" + base64.RawURLEncoding.EncodeToString(secret[:])
	p.ticket = true
	now := time.Now()
	tickets.Lock()
	defer tickets.Unlock()
	if len(tickets.m) > 4096 {
		for k, v := range tickets.m {
			if now.After(v.expires) {
				delete(tickets.m, k)
			}
		}
	}
	tickets.m[ticketKey(raw)] = ticketEntry{p, now.Add(ticketTTL)}
	return raw
}

func takeTicket(raw string) (Principal, bool) {
	k := ticketKey(raw)
	tickets.Lock()
	defer tickets.Unlock()
	e, ok := tickets.m[k]
	if !ok {
		return Principal{}, false
	}
	delete(tickets.m, k)
	if time.Now().After(e.expires) {
		return Principal{}, false
	}
	return e.principal, true
}

func dropTicket(raw string) {
	tickets.Lock()
	delete(tickets.m, ticketKey(raw))
	tickets.Unlock()
}
