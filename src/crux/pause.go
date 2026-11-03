package crux

import "time"

// BreakerTrigger is the distinct PausedTrigger stamped on every pause the fleet
// breaker performs. It serves two jobs: the audit log attributes the pause to
// the breaker, and the runtime's breaker release uses it as the guard that
// distinguishes a pause the breaker still owns (safe to auto-resume) from one
// an operator re-applied during the breaker window (must stay paused).
// Anything whose current Process.PausedTrigger != BreakerTrigger was last
// paused by something else.
const BreakerTrigger = "fleet-breaker"

// PauseCausation is hook causation metadata carried in agent pause/resume
// events without making the runtime import the host's hook package.
type PauseCausation struct {
	Depth            int
	HookName         string
	OriginTransition string
}

// PauseTransitionEvent describes a durable agent pause/resume transition, as
// delivered to the observer the host registers with the runtime.
type PauseTransitionEvent struct {
	Agent     string
	Paused    bool
	Trigger   string
	Reason    string
	By        string
	Causation PauseCausation
	At        time.Time
}
