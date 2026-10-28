package crux

import (
	"errors"
	"time"
)

// ErrAgentBusy means no kick was delivered because an agent is mid-turn.
// Callers should defer the kick without recording a delivery failure.
var ErrAgentBusy = errors.New("agent turn in progress")

// Kick dispatch phases. A dispatch is INDETERMINATE while it is pending: the
// prompt may still be delivered. Only KickPhaseFailed is a definitive failure.
const (
	// KickPhasePending means the kick passed its preconditions and a delivery
	// goroutine is waiting for the CLI's input prompt.
	KickPhasePending = "pending"
	// KickPhaseDelivered means the message was typed into the agent's pane.
	KickPhaseDelivered = "delivered"
	// KickPhaseFailed means delivery will not happen: the CLI never reached
	// its input prompt in time, a restart failed, or the agent disappeared
	// mid-flight.
	KickPhaseFailed = "failed"
)

// KickDispatch is the observable outcome of one asynchronous kick.
type KickDispatch struct {
	// Agent is the resolved agent name.
	Agent string `json:"agent"`
	// Phase is one of KickPhasePending / KickPhaseDelivered / KickPhaseFailed.
	Phase string `json:"phase"`
	// Error carries the failure reason when Phase is KickPhaseFailed. It is
	// empty in every other phase.
	Error string `json:"error,omitempty"`
	// QueuedAt is when the dispatch passed its preconditions.
	QueuedAt time.Time `json:"queuedAt"`
	// SettledAt is when the dispatch reached a terminal phase. Zero while
	// pending.
	SettledAt time.Time `json:"settledAt,omitempty"`
}

// Pending reports whether this dispatch is still in flight — the state in
// which the outcome is INDETERMINATE and must never be rendered as a failure.
func (d KickDispatch) Pending() bool { return d.Phase == KickPhasePending }

// Kick outcome kinds. pkg/governor mirrors these strings (governor.KickOutcome*)
// so the two packages need no import edge between them.
const (
	// KickOutcomeQuestion: the agent ended its turn asking the operator what to
	// do. A kick already told it; ending on a question is a defect.
	KickOutcomeQuestion = "question"
	// KickOutcomeStandDown: the agent ended its turn with an explicit policy
	// stand-down. A legitimate refusal — blocked, not idle.
	KickOutcomeStandDown = "stand-down"
	// KickOutcomeNoOp: the agent explicitly reported producing nothing (the
	// "no issue opened, no PR opened, no bead created" triple) without a
	// stand-down.
	KickOutcomeNoOp = "no-op"
	// KickOutcomeEnded: the turn ended with no recognised no-op signature. Not
	// a productivity claim.
	KickOutcomeEnded = "ended"
)

// KickOutcome is how an agent's most recent kicked turn ended.
type KickOutcome struct {
	Kind string `json:"kind"`
	// Reason is the pane line that decided a no-op verdict; empty for "ended".
	Reason string `json:"reason,omitempty"`
	// At is when the turn was observed to have ended.
	At time.Time `json:"at"`
	// KickAt is the delivery time of the kick this outcome belongs to. An
	// outcome whose KickAt differs from the agent's LastKick describes an
	// earlier turn; the current one has not ended yet.
	KickAt time.Time `json:"kickAt"`
}

// Settled reports whether this outcome belongs to the kick delivered at
// kickAt — i.e. that turn has ended and been classified.
func (o KickOutcome) Settled(kickAt *time.Time) bool {
	return kickAt != nil && o.Kind != "" && o.KickAt.Equal(*kickAt)
}

// KickLogInfo describes one archived kick log, as listed by the runtime's
// ListKickLogs and the dashboard's kick-history endpoint.
type KickLogInfo struct {
	// ID is the archive's filename — the handle ReadKickLog accepts.
	ID string `json:"id"`
	// Timestamp is when the archived kick STARTED (the agent's LastKick at
	// archive time), parsed from the filename, falling back to the file
	// mtime. #9580: this used to be when the archive was WRITTEN — which for
	// the "kick" reason is the NEXT kick's delivery time, often hours after
	// this kick ran, and useless for lining a kick up against e.g. a budget
	// reset.
	Timestamp time.Time `json:"timestamp"`
	// SizeBytes is the archive size on disk.
	SizeBytes int64 `json:"size_bytes"`
	// Reason says what triggered the snapshot: "kick" (a newer kick rotated
	// this one out), "restart", or "shutdown" (pod roll / hive upgrade).
	Reason string `json:"reason"`
	// Source is the header's source: line — "scrollback" or "transcript"
	// (#11762) — so a listing can say which without opening the file. Empty
	// for archives written before the header carried one.
	Source string `json:"source,omitempty"`
}
