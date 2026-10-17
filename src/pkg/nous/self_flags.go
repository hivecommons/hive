package nous

import (
	"fmt"
	"strings"
	"sync"
)

// SelfFlagAssignment selects one Hive-self behaviour, never a repo experiment
// or governor setting. Enabled=false is an explicit fast-fail/revert instruction.
// Release-channel experiments can use the existing upgrade protocol instead.
type SelfFlagAssignment struct {
	ExperimentID string `json:"experiment_id"`
	Flag         string `json:"flag"`
	Enabled      bool   `json:"enabled"`
}

func (a SelfFlagAssignment) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"experiment_id", a.ExperimentID},
		{"flag", a.Flag},
	} {
		if field.value == "" || field.value != strings.TrimSpace(field.value) || len(field.value) > 128 {
			return fmt.Errorf("invalid self-experiment %s", field.name)
		}
	}
	return nil
}

// SelfFlagGate is an in-memory, default-off view of the latest heartbeat's
// assignment. Replacing rather than merging assignments makes withdrawal,
// opt-out, and switching experiments revoke the previous behaviour immediately.
// A process restart starts off; assignments are deliberately not persisted here.
// There is no assignment TTL: failed or unverified heartbeats leave the last
// assignment active. The next successful verified heartbeat replaces it (and
// withdraws it if no assignment is returned). Local opt-out clears it on the
// next heartbeat attempt, even when the hub is unreachable.
type SelfFlagGate struct {
	mu         sync.RWMutex
	assignment SelfFlagAssignment
}

// SelfExperiments is the runtime hook for background Hive-self behaviour.
// Call Enabled at the point of use, not once at startup, so reverts take effect.
// It is independent of the user's Experiment Lab and governor configuration.
var SelfExperiments SelfFlagGate

func (g *SelfFlagGate) Apply(optedIn bool, assignment *SelfFlagAssignment) {
	var next SelfFlagAssignment
	if optedIn && assignment != nil && assignment.Validate() == nil {
		next = *assignment
	}
	g.mu.Lock()
	g.assignment = next
	g.mu.Unlock()
}

func (g *SelfFlagGate) Enabled(flag string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return flag != "" && g.assignment.Flag == flag && g.assignment.Enabled
}

// Assignment returns a value copy for background experiment attribution.
func (g *SelfFlagGate) Assignment() SelfFlagAssignment {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.assignment
}
