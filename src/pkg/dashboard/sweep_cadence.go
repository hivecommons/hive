package dashboard

import "time"

type sweepCadence struct {
	LastRunAt  *time.Time `json:"last_run_at"`
	NextDueInS float64    `json:"next_due_in_s"`
	Running    bool       `json:"running"`
	Skipped    bool       `json:"skipped"`
	nextDue    time.Time
}

// RecordSweepCadence records attempts, including budget-skipped passes. A
// skipped pass advances the next attempt but never invents a successful run.
// last_run_at is the start of the latest pass, not its completion, so an
// in-flight stall remains observable along with running=true.
func (s *Server) RecordSweepCadence(name string, at time.Time, interval time.Duration, ran, running bool) {
	if s == nil {
		return
	}
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	if s.sweepCadence == nil {
		s.sweepCadence = make(map[string]sweepCadence)
	}
	st := s.sweepCadence[name]
	if ran {
		st.LastRunAt = &at
	}
	st.nextDue = at.Add(interval)
	st.Running, st.Skipped = running, !ran
	s.sweepCadence[name] = st
}

func (s *Server) sweepCadences(now time.Time) map[string]sweepCadence {
	s.sweepMu.RLock()
	defer s.sweepMu.RUnlock()
	out := make(map[string]sweepCadence, len(s.sweepCadence))
	for name, st := range s.sweepCadence {
		st.NextDueInS = max(0, st.nextDue.Sub(now).Seconds())
		out[name] = st
	}
	return out
}
