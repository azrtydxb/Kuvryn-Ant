package npmregistry

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/azrtydxb/Kuvryn-Ant/internal/artifact"
)

// Each way a request can end without an answer is a different status, message
// or header, so an operator can tell shed load from a client that left from a
// genuine outage.
func TestFailServiceClasses(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		status     int
		retryAfter string
	}{
		"busy":              {artifact.ErrBusy, 503, "2"},
		"busy wrapped":      {fmt.Errorf("pull: %w", artifact.ErrBusy), 503, "2"},
		"deadline":          {context.DeadlineExceeded, 503, "2"},
		"client went away":  {context.Canceled, 499, ""},
		"upstream outage":   {artifact.ErrUnavailable, 503, ""},
		"too large":         {artifact.ErrTooLarge, 503, ""},
		"not found":         {artifact.ErrNotFound, 404, ""},
		"denied":            {artifact.ErrDenied, 403, ""},
		"conflict":          {artifact.ErrConflict, 409, ""},
		"invalid":           {artifact.ErrInput, 400, ""},
		"unclassified fail": {fmt.Errorf("boom"), 503, ""},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			failService(w, tc.err)
			if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retryAfter {
				t.Fatalf("status %d Retry-After %q, want %d %q", w.Code, w.Header().Get("Retry-After"), tc.status, tc.retryAfter)
			}
		})
	}
	busy, outage := httptest.NewRecorder(), httptest.NewRecorder()
	failService(busy, artifact.ErrBusy)
	failService(outage, artifact.ErrUnavailable)
	if busy.Body.String() == outage.Body.String() {
		t.Fatal("a shed request and an outage must not carry the same reason")
	}
}
