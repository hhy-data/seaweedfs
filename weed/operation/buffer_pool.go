package operation

import (
	"sync/atomic"

	"github.com/valyala/bytebufferpool"
)

var bufferCounter int64

// LargeBufferSizeThreshold is the maximum size of buffers that are returned to the pool.
// Buffers larger than this are not returned to avoid keeping large amounts of memory
// in the pool under high concurrency scenarios.
const LargeBufferSizeThreshold = 8 * 1024 * 1024 // 8MB

func GetBuffer() *bytebufferpool.ByteBuffer {
	defer func() {
		atomic.AddInt64(&bufferCounter, 1)
		// println("+", bufferCounter)
	}()
	return bytebufferpool.Get()
}

func PutBuffer(buf *bytebufferpool.ByteBuffer) {
	defer func() {
		atomic.AddInt64(&bufferCounter, -1)
		// println("-", bufferCounter)
	}()
	// Skip returning large buffers to the pool to prevent memory bloat
	// under high concurrency (e.g., 30+ concurrent uploads with maxMB=32).
	// Let these buffers be garbage collected instead of being retained in the pool.
	if cap(buf.B) > LargeBufferSizeThreshold {
		return
	}
	bytebufferpool.Put(buf)
}
