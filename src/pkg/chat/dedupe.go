package chat

import "sync"

const recentMessageLimit = 4096

// recentMessages bounds dedupe memory for the lifetime of a Service. IDs are
// claimed before dispatch so concurrent retries cannot both execute a handler.
// Evicted IDs (and IDs seen before a process restart) may be delivered again.
type recentMessages struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	next  int
}

func (r *recentMessages) remember(id string) bool {
	if id == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.seen[id]; ok {
		return false
	}
	if r.seen == nil {
		r.seen = make(map[string]struct{})
	}
	if len(r.order) < recentMessageLimit {
		r.order = append(r.order, id)
	} else {
		delete(r.seen, r.order[r.next])
		r.order[r.next] = id
		r.next = (r.next + 1) % recentMessageLimit
	}
	r.seen[id] = struct{}{}
	return true
}
