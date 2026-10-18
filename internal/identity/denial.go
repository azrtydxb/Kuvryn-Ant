package identity

import (
	"context"
	"sync"
)

type denialKey struct{}

// DenialNote lets the outermost audit middleware learn which principal (if
// any) a request authenticated as, without trusting request data. Authenticators
// inside the request call noteAuthenticated; the middleware reads it afterwards.
type DenialNote struct {
	mu sync.Mutex
	p  Principal
}

func WithDenialNote(ctx context.Context) (context.Context, *DenialNote) {
	n := &DenialNote{}
	return context.WithValue(ctx, denialKey{}, n), n
}

func (n *DenialNote) Principal() Principal {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.p
}

// NoteAuthenticated records the authenticated principal for denial auditing.
func NoteAuthenticated(ctx context.Context, p Principal) {
	if n, ok := ctx.Value(denialKey{}).(*DenialNote); ok {
		n.mu.Lock()
		n.p = p
		n.mu.Unlock()
	}
}
