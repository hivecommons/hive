package sessiontaint

import (
	"container/list"
	"sync"
)

// Default bounds. Each is a named constant, overridable through Options, so a
// busy hive cannot grow the store without limit (design: Cost, Storage).
const (
	// DefaultMaxSessions bounds how many agent sessions the store holds. At
	// the bound the least recently touched session is evicted.
	DefaultMaxSessions = 256
	// DefaultMaxLabelsPerSession bounds the labels one session holds. The
	// label kinds are a closed set, so the bound is on distinct origins.
	DefaultMaxLabelsPerSession = 32
	// DefaultMaxDecisions bounds the recent would-act ring.
	DefaultMaxDecisions = 128
	// maxDecisionKeysPerSession bounds how many distinct would-act decisions
	// per session are logged and audited; later ones are only counted.
	maxDecisionKeysPerSession = 64
)

// session is one agent's taint state for the life of the process. A process
// restart is the epoch boundary in this phase: the store is in memory only.
type session struct {
	agent  string
	labels []LabelRecord
	seen   map[string]struct{} // label+origin keys, for the union
	logged map[string]struct{} // would-act decisions already logged
	elem   *list.Element
}

// store is the bounded in-memory session map with least-recently-used
// eviction. All methods are safe for concurrent use.
type store struct {
	mu          sync.Mutex
	maxSessions int
	maxLabels   int
	sessions    map[string]*session
	lru         *list.List // front = most recently touched, values are agent names

	evicted       uint64
	labelsDropped uint64
}

func newStore(maxSessions, maxLabels int) *store {
	return &store{
		maxSessions: maxSessions,
		maxLabels:   maxLabels,
		sessions:    make(map[string]*session),
		lru:         list.New(),
	}
}

// touch returns the agent's session, creating it (and evicting the least
// recently used one at the bound) when absent. The caller holds s.mu.
func (s *store) touch(agent string) *session {
	if sess, ok := s.sessions[agent]; ok {
		s.lru.MoveToFront(sess.elem)
		return sess
	}
	for len(s.sessions) >= s.maxSessions {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		name, ok := oldest.Value.(string)
		s.lru.Remove(oldest)
		if ok {
			delete(s.sessions, name)
			s.evicted++
		}
	}
	sess := &session{
		agent:  agent,
		seen:   make(map[string]struct{}),
		logged: make(map[string]struct{}),
	}
	sess.elem = s.lru.PushFront(agent)
	s.sessions[agent] = sess
	return sess
}

// add joins one label into the agent's session (set union: a repeat is a
// no-op). It reports whether the label is new and whether it was dropped
// because the session is at its label bound.
func (s *store) add(agent string, rec LabelRecord) (added, dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.touch(agent)
	key := string(rec.Label) + "\x00" + rec.Origin
	if _, dup := sess.seen[key]; dup {
		return false, false
	}
	if len(sess.labels) >= s.maxLabels {
		s.labelsDropped++
		return false, true
	}
	sess.seen[key] = struct{}{}
	sess.labels = append(sess.labels, rec)
	return true, false
}

// labelsOf returns a copy of the agent's labels without creating or touching a
// session: a read must not make a clean agent look like it has state.
func (s *store) labelsOf(agent string) []LabelRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[agent]
	if !ok {
		return nil
	}
	return append([]LabelRecord(nil), sess.labels...)
}

// firstDecision reports whether this is the first time the session logs the
// given decision key, and remembers it. Past the per-session bound it reports
// false, so a long session cannot flood the log or the audit trail.
func (s *store) firstDecision(agent, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[agent]
	if !ok {
		return false
	}
	if _, dup := sess.logged[key]; dup {
		return false
	}
	if len(sess.logged) >= maxDecisionKeysPerSession {
		return false
	}
	sess.logged[key] = struct{}{}
	return true
}

type storeCounts struct {
	sessions, labels       int
	evicted, labelsDropped uint64
}

func (s *store) counts() storeCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := storeCounts{sessions: len(s.sessions), evicted: s.evicted, labelsDropped: s.labelsDropped}
	for _, sess := range s.sessions {
		c.labels += len(sess.labels)
	}
	return c
}
