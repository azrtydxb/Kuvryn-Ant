package management

import (
	"strconv"
	"sync/atomic"
)

var idkCounter atomic.Int64

// idk returns a fresh Idempotency-Key for tests that exercise mutating routes.
func idk() string { return "test-key-" + strconv.FormatInt(idkCounter.Add(1), 10) }
