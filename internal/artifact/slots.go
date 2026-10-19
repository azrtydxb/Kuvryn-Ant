package artifact

import (
	"runtime"
	"sync/atomic"
)

var uploadSlots atomic.Int32

// SetUploadSlots sets how many uploads each package format admits at once
// (configuration, ANT_UPLOAD_SLOTS). Zero restores the CPU-scaled default. Call
// before constructing the adapters.
func SetUploadSlots(n int) { uploadSlots.Store(int32(max(n, 0))) }

// UploadSlots is the effective per-format upload concurrency.
func UploadSlots() int {
	if n := int(uploadSlots.Load()); n > 0 {
		return n
	}
	return min(max(2*runtime.GOMAXPROCS(0), 4), 32)
}

// NewUploadSlots returns the semaphore an adapter uses to bound concurrent uploads.
func NewUploadSlots() chan struct{} { return make(chan struct{}, UploadSlots()) }
