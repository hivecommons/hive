package crux

import "time"

// ProcessState is the runtime's intent for an agent (running, stopped,
// paused, …), as distinct from what its pane is observed to be doing.
type ProcessState string

// Process states.
const (
	StateIdle    ProcessState = "idle"
	StateRunning ProcessState = "running"
	StateStopped ProcessState = "stopped"
	StateFailed  ProcessState = "failed"
	StatePaused  ProcessState = "paused"
)

// KickRecord is one entry of an agent's kick history.
type KickRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
	Snippet   string    `json:"snippet"`
}

// RestartEvent is one restart of an agent and why it happened.
type RestartEvent struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// SubAgentModel is an explicit model observation, not the model requested by
// task(). Copilot 1.0.88 (Dockerfile pin) prints these in its terminal task
// rows. The session-state layout is not a stable effective-model contract, so
// the runtime uses those rows as a read-only fallback; missing or changed
// layouts yield no rows.
type SubAgentModel struct {
	AgentType       string `json:"agentType"`
	Model           string `json:"model"`
	OlderGeneration bool   `json:"olderGeneration,omitempty"`
}

// StartFailureClass is the stable, machine-comparable kind of a start failure.
// The class — not the human sentence — is what consecutive-failure counting
// compares, so re-phrasing a message can never reset the ladder, and two
// genuinely different faults can never be counted as one recurrence.
type StartFailureClass string

// Start failure classes.
const (
	// StartFailureLoginRequired: the CLI came up but has no usable interactive
	// session — Copilot's "/login to sign in", a device-code prompt. A human
	// must authenticate; no number of relaunches will do it.
	StartFailureLoginRequired StartFailureClass = "login-required"
	// StartFailureCredentialRejected: a credential was presented and the server
	// refused it — bob's "Invalid or expired API Key", a 401. Also terminal
	// until a human replaces the credential.
	StartFailureCredentialRejected StartFailureClass = "credential-rejected"
	// StartFailureCredentialMissing: no credential is configured at all.
	StartFailureCredentialMissing StartFailureClass = "credential-missing"
	// StartFailureBackendMismatch: the agent is launched as one CLI and
	// health-checked as another, because backend and launch_cmd disagree
	// (#5921's root cause 1). Relaunching cannot converge — the readiness probe
	// is watching for a prompt the launched binary never prints.
	StartFailureBackendMismatch StartFailureClass = "backend-mismatch"
	// StartFailureBinaryMissing: the backend's CLI is not on PATH.
	StartFailureBinaryMissing StartFailureClass = "binary-missing"
	// StartFailureNoOutput: the CLI produced no readiness marker and the
	// diagnostic could not say why. The honest residual class — it means "we
	// know it did not start and we do not know why", which is different from
	// every named class above and must not borrow their wording.
	StartFailureNoOutput StartFailureClass = "no-output"
)

// TurnInterruption is one teardown that killed a turn in flight.
//
// Durations are seconds rather than time.Duration because this crosses into
// the persisted state file, where a float is legible to `jq` and stable across
// Go's duration-marshalling.
type TurnInterruption struct {
	// At is when the teardown happened.
	At time.Time
	// Reason is the teardown reason, matching the kick-log archive reason
	// ("restart", "shutdown").
	Reason string
	// SinceKick is teardown minus kick delivery — the upper bound on cost.
	// Zero when the agent had no recorded kick.
	SinceKick time.Duration
	// SinceOutput is teardown minus the last observed pane change. Nil when
	// the pane poller never saw two differing captures, which reads as
	// "unknown" and must never be read as "idle".
	SinceOutput *time.Duration
	// Producing reports whether the pane changed after the kick landed — that
	// the agent did something this turn. Unknown counts as false: an
	// instrument that guesses "yes" would inflate exactly the number the RFC
	// is trying to size.
	Producing bool
	// Bytes is the scrollback archived at teardown, a proxy for the volume of
	// turn output discarded. Zero when kick-log archiving is disabled or the
	// capture failed — the interruption is still recorded, because undercounting
	// interruptions wherever archiving is off would bias the measurement toward
	// the fleet's best-instrumented spokes.
	Bytes int
}

// TurnLoss is the per-agent accumulation of what teardowns discarded from
// in-flight turns; it survives the restart it measures.
type TurnLoss struct {
	// Interruptions is every teardown that hit a turn with pending output.
	Interruptions int
	// Producing is the subset where the agent was observably working. This is
	// the honest headline number: interruptions that certainly discarded work.
	Producing int
	// UpperBound is the summed SinceKick across all interruptions. Named for
	// what it is: the most this agent's restarts could have cost, not what
	// they did cost.
	UpperBound time.Duration
	// Bytes is the summed archived scrollback across all interruptions.
	Bytes int64
	// Recent is the newest records, oldest first; the runtime bounds the ring.
	Recent []TurnInterruption
}

// SandboxRunUsage is the token usage one sandboxed kick reported, when its
// transcript carried any. Zero when the backend printed no usage footer.
type SandboxRunUsage struct {
	Model       string `json:"model,omitempty"`
	Input       int64  `json:"input,omitempty"`
	Output      int64  `json:"output,omitempty"`
	CacheRead   int64  `json:"cacheRead,omitempty"`
	CacheCreate int64  `json:"cacheCreate,omitempty"`
}

// Total is the sum of the four counters.
func (u SandboxRunUsage) Total() int64 {
	return u.Input + u.Output + u.CacheRead + u.CacheCreate
}

// SandboxRun is the outcome of the most recent sandboxed kick of an agent
// (podman or Kubernetes Job runtime). A sandboxed agent has no tmux session
// or in-pod process between kicks, so this record — not the process state —
// is what its health derives from.
type SandboxRun struct {
	Runtime     string    `json:"runtime,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	EndedAt     time.Time `json:"endedAt"`
	Succeeded   bool      `json:"succeeded"`
	TimedOut    bool      `json:"timedOut,omitempty"`
	Error       string    `json:"error,omitempty"`
	CommitCount int       `json:"commitCount,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	PRURL       string    `json:"prURL,omitempty"`
	// Summary is the agent's own report summary when it left one, else the
	// scrubbed tail of the transcript. Bounded; see sandboxRunSummaryMax.
	Summary string          `json:"summary,omitempty"`
	Usage   SandboxRunUsage `json:"usage,omitempty"`
}

// Duration is how long the run took, zero when the record is incomplete.
func (r SandboxRun) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.EndedAt.Before(r.StartedAt) {
		return 0
	}
	return r.EndedAt.Sub(r.StartedAt)
}
