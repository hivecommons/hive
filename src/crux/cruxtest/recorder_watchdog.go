package cruxtest

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/watchdog"
)

// ---- watchdog.Source ----

// WatchdogSource returns the recorder as the runtime half of a watchdog
// fleet, the way *agent.Manager does.
func (r *Recorder) WatchdogSource() watchdog.Source {
	r.record("WatchdogSource")
	return watchdogSource{r}
}

type watchdogSource struct{ r *Recorder }

func (s watchdogSource) AgentNames() []string {
	s.r.record("AgentNames")
	return append([]string(nil), s.r.WatchdogNames...)
}

func (s watchdogSource) Observe(name string) (watchdog.Observation, error) {
	s.r.record("Observe", name)
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	o, ok := s.r.Observations[name]
	if !ok {
		return watchdog.Observation{}, fmt.Errorf("agent %s not found", name)
	}
	return o, nil
}

func (s watchdogSource) LastProduction(name string) (time.Time, bool) {
	s.r.record("LastProduction", name)
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	t, ok := s.r.Production[name]
	return t, ok
}

func (s watchdogSource) SetConditions(name string, conds []crux.Condition) {
	s.r.record("SetConditions", name, append([]crux.Condition(nil), conds...))
	s.r.mu.Lock()
	s.r.SetConds[name] = append([]crux.Condition(nil), conds...)
	s.r.mu.Unlock()
}
