package crux

import (
	"sync"
)

type RingBuffer struct {
	mu    sync.RWMutex
	items []string
	cap   int
	head  int
	count int
	total uint64
}

const defaultRingCapacity = 500

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = defaultRingCapacity
	}
	return &RingBuffer{
		items: make([]string, capacity),
		cap:   capacity,
	}
}

func (r *RingBuffer) Write(s string) {
	r.mu.Lock()
	r.items[r.head] = s
	r.head = (r.head + 1) % r.cap
	if r.count < r.cap {
		r.count++
	}
	r.total++
	r.mu.Unlock()
}

func (r *RingBuffer) Last(n int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n > r.count {
		n = r.count
	}
	if n <= 0 {
		return nil
	}

	result := make([]string, n)
	start := (r.head - n + r.cap) % r.cap
	for i := range n {
		result[i] = r.items[(start+i)%r.cap]
	}
	return result
}

func (r *RingBuffer) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// Total returns how many lines were ever written, a monotonic position that
// keeps growing after old lines are overwritten.
func (r *RingBuffer) Total() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.total
}

// Since returns the lines written at positions seq and later, and the
// position to pass next time. Lines already overwritten are skipped.
func (r *RingBuffer) Since(seq uint64) ([]string, uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	oldest := r.total - uint64(r.count)
	if seq < oldest {
		seq = oldest
	}
	if seq >= r.total {
		return nil, r.total
	}
	n := int(r.total - seq)
	result := make([]string, n)
	start := (r.head - n + r.cap) % r.cap
	for i := range n {
		result[i] = r.items[(start+i)%r.cap]
	}
	return result, r.total
}
