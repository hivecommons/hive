// Package escalate carries the typed escalation event that long-running run
// machinery raises when Hive's own retry budget is exhausted and a person has
// to decide what happens next (hivecommons/hive#8303).
//
// It is deliberately tiny and dependency-free: the stage runner emits an
// Event, the dashboard turns it into an audit entry and a timeline marker, and
// nothing in here reaches for a store, a socket, or a clock.
package escalate

import (
	"fmt"
	"time"
)

// Severity classifies how urgently a person is needed.
type Severity string

const (
	// SeverityInfo is a notice: no decision is pending.
	SeverityInfo Severity = "info"
	// SeverityDecision means Hive has stopped acting on the run and a person
	// must choose (reset the stage, revise the artifact, or abandon the run).
	SeverityDecision Severity = "decision"
)

// Valid reports whether s is one of the catalogued severities.
func (s Severity) Valid() bool {
	switch s {
	case SeverityInfo, SeverityDecision:
		return true
	}
	return false
}

// Event is one escalation raised against a run stage.
type Event struct {
	// Severity is the urgency class. The stage runner only ever raises
	// SeverityDecision.
	Severity Severity `json:"severity"`
	// RunKey is the stable run identity (Spektacular's data.name).
	RunKey string `json:"run_key"`
	// Stage is the lease stage that exhausted its retry budget.
	Stage string `json:"stage"`
	// Artifact is the Spektacular artifact name that never reached final.
	Artifact string `json:"artifact,omitempty"`
	// Gen is the lease generation at the time of escalation.
	Gen uint64 `json:"gen"`
	// Attempts is how many generations ran without reaching final.
	Attempts int `json:"attempts"`
	// Reason is a short human-readable explanation.
	Reason string `json:"reason"`
	// At is the escalation instant, supplied by the caller's clock.
	At time.Time `json:"at"`
}

// String renders a one-line summary suitable for logs and audit details.
func (e Event) String() string {
	return fmt.Sprintf("%s escalation for run %s stage %s gen %d after %d attempts: %s",
		e.Severity, e.RunKey, e.Stage, e.Gen, e.Attempts, e.Reason)
}
