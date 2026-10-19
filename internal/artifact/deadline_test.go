package artifact

import (
	"context"
	"testing"
	"time"
)

func TestAwaitUpstreamExtendsWriteDeadline(t *testing.T) {
	var got time.Duration
	ctx := WithWriteDeadlineExtender(context.Background(), func(d time.Duration) { got = d })
	awaitUpstream(ctx)
	if got < time.Minute {
		t.Fatalf("deadline not extended: %v", got)
	}
	awaitUpstream(context.Background()) // no extender: must not panic
}
