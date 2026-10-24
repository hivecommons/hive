package agent

import "github.com/hivecommons/hive/crux"

// RingBuffer is the bounded output buffer behind Process.OutputBuffer. It is
// defined in the crux root package so crux.Process can carry it without the
// root importing this package.
type RingBuffer = crux.RingBuffer

// NewRingBuffer returns a ring buffer holding the last capacity lines.
func NewRingBuffer(capacity int) *RingBuffer {
	return crux.NewRingBuffer(capacity)
}
