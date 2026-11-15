package crux

import (
	"errors"
	"time"
)

// Kick queue phases (#18285). A queued kick walks
// queued → pending → delivered → completed, or ends in failed from any of the
// non-terminal phases. KickPhasePending, KickPhaseDelivered and
// KickPhaseFailed carry the same meaning they have on KickDispatch.
const (
	// KickPhaseQueued means the kick is waiting in the agent's FIFO for the
	// agent to become idle.
	KickPhaseQueued = "queued"
	// KickPhaseCompleted means the kick was delivered and the turn it started
	// has ended (the runtime classified its outcome).
	KickPhaseCompleted = "completed"
)

// ErrKickQueueFull means the agent's kick queue already holds its configured
// maximum of waiting kicks. Nothing was enqueued.
var ErrKickQueueFull = errors.New("kick queue full")

// ErrKickIDInUse means a caller-supplied kick ID is already known to the
// runtime. Kick IDs are unique for as long as the runtime retains their
// status.
var ErrKickIDInUse = errors.New("kick id already in use")

// ErrInvalidKickID means a caller-supplied kick ID is too long or carries
// characters outside [A-Za-z0-9._:-].
var ErrInvalidKickID = errors.New("invalid kick id")

// KickStatus is the observable state of one queued kick, addressed by its
// kick ID.
type KickStatus struct {
	// ID is the kick ID the enqueue call returned.
	ID string `json:"id"`
	// Agent is the agent the kick is queued for.
	Agent string `json:"agent"`
	// Phase is one of KickPhaseQueued, KickPhasePending, KickPhaseDelivered,
	// KickPhaseCompleted or KickPhaseFailed.
	Phase string `json:"phase"`
	// Error carries the failure reason when Phase is KickPhaseFailed.
	Error string `json:"error,omitempty"`
	// Outcome is the KickOutcome kind of the finished turn when Phase is
	// KickPhaseCompleted.
	Outcome string `json:"outcome,omitempty"`
	// QueuedAt is when the kick entered the queue.
	QueuedAt time.Time `json:"queuedAt"`
	// DeliveredAt is when the prompt was typed into the agent. Zero until
	// then.
	DeliveredAt time.Time `json:"deliveredAt,omitzero"`
	// SettledAt is when the kick reached a terminal phase. Zero until then.
	SettledAt time.Time `json:"settledAt,omitzero"`
}

// Terminal reports whether the kick reached a final phase (completed or
// failed) and will not change again.
func (s KickStatus) Terminal() bool {
	return s.Phase == KickPhaseCompleted || s.Phase == KickPhaseFailed
}

// KickQueuer is the optional runtime surface for ID-addressed, queued kicks.
// The in-process runtime (*agent.Manager) implements it; callers type-assert
// a crux.Runtime to it and treat a failed assertion as "queued kicks are not
// available on this runtime".
type KickQueuer interface {
	// EnqueueKick appends message to the agent's kick FIFO and returns the
	// kick ID. kickID may be empty, in which case one is generated. The kick
	// is delivered once the agent is idle.
	EnqueueKick(agent, kickID, message string) (string, error)
	// KickStatus returns the status of a kick by ID and whether it is known.
	KickStatus(kickID string) (KickStatus, bool)
	// KickQueueDepth reports how many kicks are waiting (not yet delivered)
	// for the agent.
	KickQueueDepth(agent string) int
	// InFlightKickID returns the ID of the queued kick whose turn the agent
	// is currently running, or "" when none is.
	InFlightKickID(agent string) string
}
