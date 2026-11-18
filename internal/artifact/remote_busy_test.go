package artifact

import (
	"errors"
	"fmt"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/remoteruntime"
)

// Load shedding in the remote runtime reaches adapters as ErrBusy, which still
// counts as ErrUnavailable so every adapter that only knows that class keeps
// answering 503.
func TestMapRemoteErrorBusy(t *testing.T) {
	for _, in := range []error{remoteruntime.ErrBusy, fmt.Errorf("wrapped: %w", remoteruntime.ErrBusy)} {
		got := mapRemoteError(in)
		if !errors.Is(got, ErrBusy) || !errors.Is(got, ErrUnavailable) {
			t.Fatalf("%v mapped to %v", in, got)
		}
	}
	if got := mapRemoteError(remoteruntime.ErrTooLarge); !errors.Is(got, ErrTooLarge) || errors.Is(got, ErrBusy) {
		t.Fatalf("too-large must stay distinct from busy: %v", got)
	}
	if got := mapRemoteError(remoteruntime.ErrCapacity); errors.Is(got, ErrBusy) {
		t.Fatalf("plain capacity errors are not shed requests: %v", got)
	}
}
