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
//
// Reverted experiment IDs form a local deny-list that Apply preserves: a hub
// assignment for a reverted experiment is kept for attribution but forced off,
// so a heartbeat cannot undo a local fast-fail or end-of-experiment revert.
// The deny-list is in memory; the scheduler executor rebuilds it from the
// durable experiment ledger at startup (see scheduler.Executor.Restore).
type SelfFlagGate struct {
	mu         sync.RWMutex
	assignment SelfFlagAssignment
	reverted   map[string]bool
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
	if g.reverted[next.ExperimentID] {
		next.Enabled = false
	}
	g.assignment = next
	g.mu.Unlock()
}

// Revert locally and permanently (for this process) disables experimentID,
// including the current assignment and any later Apply of the same experiment.
// It is idempotent and does not affect other experiments.
func (g *SelfFlagGate) Revert(experimentID string) {
	if experimentID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reverted == nil {
		g.reverted = map[string]bool{}
	}
	g.reverted[experimentID] = true
	if g.assignment.ExperimentID == experimentID {
		g.assignment.Enabled = false
	}
}

// Reverted reports whether experimentID is on the local revert deny-list.
func (g *SelfFlagGate) Reverted(experimentID string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.reverted[experimentID]
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
