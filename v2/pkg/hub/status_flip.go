package hub

import "time"

// Status-flip detection: a hive whose reported App-auth state keeps
// ALTERNATING (ok → key-invalid → ok → …) is not transitioning, it is
// oscillating — in practice two spoke instances alternating under one
// hive_id, or a spoke whose auth check flaps every beat. Either way the
// dashboard flips with it and every glance at the row tells a different
// story.
//
// This is the reporter-blind sibling of noteReporter: it needs NOTHING from
// the spoke (old builds included), because it watches the states the hub
// already receives. When the spoke is new enough to also report its pod name,
// the duplicate-spoke signal names the culprits; this signal only says the
// row cannot be trusted.

// statusFlipsToConfirm is how many returns to a previously reported state are
// required before a hive is declared flipping. A single bounce (degraded →
// ok → degraded hours later) is a hive having a bad day; three returns on a
// heartbeat cadence is an oscillation.
const statusFlipsToConfirm = 3

// statusFlipWindow bounds how recent the last alternation must be for the
// flag to persist. Beats are 1–2 minutes apart, so a genuine oscillation
// re-arms this continuously; a hive that settles stops flapping and the flag
// ages out on its own.
const statusFlipWindow = 10 * time.Minute

// noteStatusFlip records the App-auth state this beat reported and returns
// true while the state is oscillating between two values. Unlike reporters,
// an EMPTY state is meaningful here — it is "healthy" — so it participates in
// alternation tracking rather than being skipped.
func (s *HubServer) noteStatusFlip(hiveID, state string) bool {
	s.statusFlipMu.Lock()
	defer s.statusFlipMu.Unlock()
	if s.statusFlipSeen == nil {
		s.statusFlipSeen = make(map[string]*reporterFlipState)
	}
	st := s.statusFlipSeen[hiveID]
	if st == nil {
		s.statusFlipSeen[hiveID] = &reporterFlipState{last: state}
		return false
	}
	if state != st.last {
		if state == st.prev {
			st.flips++
		} else {
			// A state never seen adjacent to this one: a normal transition,
			// not an oscillation.
			st.flips = 0
		}
		st.prev, st.last = st.last, state
		st.lastFlip = time.Now()
	}
	if time.Since(st.lastFlip) >= statusFlipWindow {
		st.flips = 0
		return false
	}
	return st.flips >= statusFlipsToConfirm
}
