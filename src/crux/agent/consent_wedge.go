package agent

import (
	"sort"
	"sync"
	"time"
)

// consentWedgeWindow is how recent a consent-screen restart must be for the
// agent to count as wedged. One restart an hour ago that never recurred means
// the operator completed the consent flow; a live loop refreshes the stamp
// every kick attempt.
const consentWedgeWindow = time.Hour

// consentWedgeTracker records the most recent consent-screen restart per
// agent. It has its OWN mutex — the recording sites run while m.mu is held
// (SendKick / deliverKickAsync), and re-acquiring m.mu there would deadlock
// (see the manager's mutex-reentrancy history).
type consentWedgeTracker struct {
	mu          sync.Mutex
	lastByAgent map[string]time.Time
}

func (t *consentWedgeTracker) note(name string, now time.Time) {
	if name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastByAgent == nil {
		t.lastByAgent = make(map[string]time.Time)
	}
	t.lastByAgent[name] = now
}

func (t *consentWedgeTracker) wedged(now time.Time) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Non-nil even when empty: the tracker is always live in-process, so its
	// answer is always a MEASUREMENT — the heartbeat must send [] (clear the
	// hub's carry-forward), never null ("not measured").
	out := []string{}
	for name, at := range t.lastByAgent {
		if now.Sub(at) <= consentWedgeWindow {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// noteConsentWedge records that a kick found this agent's CLI sitting on a
// consent screen and restarted it. Called from the two kick recovery sites
// with m.mu held; safe there because the tracker locks only its own mutex.
func (m *Manager) noteConsentWedge(name string) {
	m.consentWedges.note(name, time.Now())
}

// ConsentWedgedAgents returns the agents whose kick path hit a consent-screen
// restart within the last consentWedgeWindow, sorted. Empty (non-nil callers
// need not distinguish) when none are wedged.
func (m *Manager) ConsentWedgedAgents() []string {
	return m.consentWedges.wedged(time.Now())
}
