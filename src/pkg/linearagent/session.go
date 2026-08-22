package linearagent

import (
	"sort"
	"sync"
	"time"
)

// Session states surfaced to the dashboard. Linear derives ITS session state
// from the activities we emit; this tracker only records what the hive knows
// locally, so the dashboard can show active sessions without a Linear query.
const (
	SessionStateAcked    = "acked"    // thought emitted, kick not yet delivered
	SessionStateWorking  = "working"  // kick delivered to an agent
	SessionStateFinished = "finished" // the agent's run for this kick ended
	SessionStateFailed   = "failed"   // ack or kick failed
)

// Session is one tracked Linear agent session.
type Session struct {
	ID              string    `json:"id"`
	IssueID         string    `json:"issue_id,omitempty"`
	IssueIdentifier string    `json:"issue_identifier,omitempty"`
	IssueTitle      string    `json:"issue_title,omitempty"`
	IssueURL        string    `json:"issue_url,omitempty"`
	Agent           string    `json:"agent,omitempty"`
	State           string    `json:"state"`
	CreatedAt       time.Time `json:"created_at"`
	LastEventAt     time.Time `json:"last_event_at"`
	// LastActivity is the last activity type the hive emitted on the session.
	LastActivity string `json:"last_activity,omitempty"`
}

// trackerCapacity bounds retained sessions. Old finished sessions are evicted
// oldest-first; this is dashboard state, not a durable record.
const trackerCapacity = 100

// Tracker is the in-memory session registry shared by the responder (writes)
// and the dashboard (reads).
type Tracker struct {
	mu       sync.Mutex
	sessions map[string]*Session
	// activeByAgent maps an agent name to its most recent non-finished
	// session, which is how a kick-log archive event finds the session it
	// completes (see Responder.HandleAgentEvent).
	activeByAgent map[string]string
	now           func() time.Time
}

// NewTracker builds an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{
		sessions:      make(map[string]*Session),
		activeByAgent: make(map[string]string),
		now:           time.Now,
	}
}

// SetClock overrides the tracker's clock. Tests only.
func (t *Tracker) SetClock(f func() time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = f
}

// Observe records a session event, creating the session if new.
func (t *Tracker) Observe(ev SessionEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s, ok := t.sessions[ev.AgentSession.ID]
	if !ok {
		s = &Session{ID: ev.AgentSession.ID, State: SessionStateAcked, CreatedAt: now, LastEventAt: now}
		t.sessions[s.ID] = s
		t.evictLocked()
	}
	if ev.AgentSession.Issue.Identifier != "" {
		s.IssueID = ev.AgentSession.Issue.ID
		s.IssueIdentifier = ev.AgentSession.Issue.Identifier
		s.IssueTitle = ev.AgentSession.Issue.Title
		s.IssueURL = ev.AgentSession.Issue.URL
	}
	s.LastEventAt = now
}

// SetAgent binds the session to the agent that was kicked for it and marks it
// working.
func (t *Tracker) SetAgent(sessionID, agent string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[sessionID]
	if !ok {
		return
	}
	s.Agent = agent
	s.State = SessionStateWorking
	s.LastEventAt = t.now()
	t.activeByAgent[agent] = sessionID
}

// SetState transitions a session's state and records the last emitted
// activity type. Finishing (or failing) a session releases its agent binding.
func (t *Tracker) SetState(sessionID, state, lastActivity string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[sessionID]
	if !ok {
		return
	}
	s.State = state
	if lastActivity != "" {
		s.LastActivity = lastActivity
	}
	s.LastEventAt = t.now()
	if state == SessionStateFinished || state == SessionStateFailed {
		if s.Agent != "" && t.activeByAgent[s.Agent] == sessionID {
			delete(t.activeByAgent, s.Agent)
		}
	}
}

// ActiveSessionForAgent returns the agent's bound non-finished session id.
func (t *Tracker) ActiveSessionForAgent(agent string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.activeByAgent[agent]
	return id, ok
}

// Snapshot returns all tracked sessions, newest first.
func (t *Tracker) Snapshot() []Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Session, 0, len(t.sessions))
	for _, s := range t.sessions {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastEventAt.After(out[j].LastEventAt) })
	return out
}

// evictLocked drops the oldest sessions past capacity.
func (t *Tracker) evictLocked() {
	for len(t.sessions) > trackerCapacity {
		oldestID := ""
		var oldest time.Time
		for id, s := range t.sessions {
			if oldestID == "" || s.LastEventAt.Before(oldest) {
				oldestID, oldest = id, s.LastEventAt
			}
		}
		if s := t.sessions[oldestID]; s.Agent != "" && t.activeByAgent[s.Agent] == oldestID {
			delete(t.activeByAgent, s.Agent)
		}
		delete(t.sessions, oldestID)
	}
}
