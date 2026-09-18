package agent

import "sync"

type RingBuffer struct {
	mu    sync.RWMutex
	items []string
	cap   int
	head  int
	count int
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
	r.mu.Unlock()
}

func (r *RingBuffer) Last(n int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n > r.count {
		n = r.count
	}
	if n == 0 {
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
