package spoke

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Upgrade progress states. One scheduled upgrade walks
// scheduled (or delayed) → draining → pulling → rolling → verifying → done,
// or ends in missed. The hub derives the state from its registry latch plus the spoke's
// heartbeat; the spoke derives the same vocabulary from what it knows locally
// (its drain latch, upgrade marker and the hub's policy), so both dashboards
// speak the same words about the same upgrade.
const (
	// UpgradeStateScheduled — nothing in flight; NextWindowAt says when the
	// hub will next consider this hive (or "instant"/"next poll").
	UpgradeStateScheduled = "scheduled"
	// UpgradeStateDelayed — a scheduled upgrade is waiting for a gate to clear.
	UpgradeStateDelayed = "delayed"
	// UpgradeStateDraining — the spoke received the instruction and is
	// waiting for in-flight agent turns before it restarts.
	UpgradeStateDraining = "draining"
	// UpgradeStatePulling — the instruction was delivered and the spoke is
	// (or should be) acting on it: patching its Deployment, waiting for the
	// image, or on an older build draining without reporting it.
	UpgradeStatePulling = "pulling"
	// UpgradeStateRolling — the spoke has gone quiet since the instruction:
	// the pod is restarting onto the new image.
	UpgradeStateRolling = "rolling"
	// UpgradeStateVerifying — the spoke is back and reports the target
	// commit, but the hub has not yet closed the attempt.
	UpgradeStateVerifying = "verifying"
	// UpgradeStateDone — the most recent attempt landed.
	UpgradeStateDone = "done"
	// UpgradeStateMissed — the most recent attempt or window did not land;
	// Reason says why (drain lapsed, hub restarted, unreachable, image pull
	// failed, …). Temporary schedule holds are delayed, not missed.
	UpgradeStateMissed = "missed"
)

// Upgrade phases qualify an in-flight state for rolls the spoke did not run
// itself (hivecommons/hive#15788): a hub-managed kubectl roll, an operator's
// `kubectl rollout restart`, a plain SIGTERM. Both render through the same
// spoke/hub state machine as a self-upgrade.
const (
	// UpgradePhaseAnnounced — someone said a roll is coming (POST
	// /api/upgrade/announce or the heartbeat's upgrade_announce); the pod
	// has not been killed yet.
	UpgradePhaseAnnounced = "announced"
	// UpgradePhaseRestarting — the process received SIGTERM and is exiting;
	// the dashboard should expect the stream to drop and reconnect.
	UpgradePhaseRestarting = "restarting"
)

// Upgrade announce sources recorded in UpgradeProgress.Source.
const (
	UpgradeSourceHub      = "hub"
	UpgradeSourceOperator = "operator"
	// UpgradeSourceSignal — nobody announced; the SIGTERM itself is the
	// first the spoke heard of the roll.
	UpgradeSourceSignal = "signal"
)

// Upgrade attempt outcomes recorded in UpgradeAttempt.Outcome.
const (
	UpgradeOutcomeDone    = "done"
	UpgradeOutcomeMissed  = "missed"
	UpgradeOutcomeDelayed = "delayed"
)

// Upgrade attempt triggers recorded in UpgradeAttempt.Trigger.
const (
	UpgradeTriggerScheduled = "scheduled"
	UpgradeTriggerInstant   = "instant"
	UpgradeTriggerManual    = "manual"
	// UpgradeTriggerObserved — the hub saw the spoke upgrading without
	// having armed it (a spoke-managed or operator-restarted rollout).
	UpgradeTriggerObserved = "observed"
	// UpgradeTriggerWindow — not an attempt at all: a daily/weekly window
	// opened and the hub held the upgrade instead of firing it.
	UpgradeTriggerWindow = "window"
)

// UpgradeTurn is one agent whose kicked turn is holding a drain, with how
// long that turn has been running.
type UpgradeTurn struct {
	Agent    string `json:"agent"`
	ElapsedS int    `json:"elapsedS"`
}

// UpgradeAttempt is one row of a hive's upgrade history: what was attempted,
// what triggered it, and how it ended. The hub persists the last N per hive
// so a missed window is still visible the next morning.
type UpgradeAttempt struct {
	Target    string    `json:"target,omitempty"`
	From      string    `json:"from,omitempty"`
	Trigger   string    `json:"trigger,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	// Outcome is "" while the attempt is still open.
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Open reports whether the attempt has not ended yet.
func (a UpgradeAttempt) Open() bool { return a.Outcome == "" }

// UpgradeProgress is the derived, render-ready picture of one hive's upgrade
// state machine. Every field is additive; consumers must tolerate absent ones.
type UpgradeProgress struct {
	State  string `json:"state"`
	Target string `json:"target,omitempty"`
	From   string `json:"from,omitempty"`
	// StartedAt is when the current attempt was instructed (RFC3339 UTC).
	StartedAt string `json:"startedAt,omitempty"`
	// NextWindowAt is the hive's own next scheduled window (RFC3339 UTC) —
	// the daily/weekly slot, never the channel's promotion estimate.
	NextWindowAt string `json:"nextWindowAt,omitempty"`
	// DrainDeadline is when a draining spoke will proceed regardless
	// (RFC3339 UTC); DrainRemainingS is the same as a countdown.
	DrainDeadline   string `json:"drainDeadline,omitempty"`
	DrainRemainingS int    `json:"drainRemainingS,omitempty"`
	// AgentsInTurn names the agents holding the drain; AgentsTotal is the
	// hive's agent count so the UI can say "2/5 agents in turn".
	AgentsInTurn []UpgradeTurn `json:"agentsInTurn,omitempty"`
	AgentsTotal  int           `json:"agentsTotal,omitempty"`
	// Reason explains a delayed/missed state, or qualifies an in-flight one.
	Reason string `json:"reason,omitempty"`
	// Detail is the one-line human summary the pill shows.
	Detail string `json:"detail"`
	// EndedAt is when a done/missed attempt closed (RFC3339 UTC).
	EndedAt string `json:"endedAt,omitempty"`
	// Phase qualifies an in-flight roll the spoke did not run itself:
	// UpgradePhaseAnnounced or UpgradePhaseRestarting (#15788). Empty for
	// the self-upgrade path, whose State alone says enough.
	Phase string `json:"phase,omitempty"`
	// Source says who announced the roll Phase describes: hub, operator or
	// signal. Empty when Phase is.
	Source string `json:"source,omitempty"`
}

// UpgradeAnnounce is a roll intent delivered to a spoke before the kill —
// by the hub in the heartbeat response or by an operator through POST
// /api/upgrade/announce (#15788). Target may be a commit SHA or a version
// tag; empty means "a restart, onto whatever the Deployment now says".
type UpgradeAnnounce struct {
	Target string `json:"target,omitempty"`
	// Source is UpgradeSourceHub or UpgradeSourceOperator.
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// InFlight reports whether the state describes an attempt still under way.
func (p *UpgradeProgress) InFlight() bool {
	if p == nil {
		return false
	}
	switch p.State {
	case UpgradeStateDraining, UpgradeStatePulling, UpgradeStateRolling, UpgradeStateVerifying:
		return true
	}
	return false
}

// FormatTurns renders "2/5 agents in turn: reviewer 38m, scanner 12m".
// Longest-running turn first; total is omitted when unknown (0).
func FormatTurns(turns []UpgradeTurn, total int) string {
	if len(turns) == 0 {
		return "no agents in turn"
	}
	sorted := append([]UpgradeTurn(nil), turns...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].ElapsedS != sorted[j].ElapsedS {
			return sorted[i].ElapsedS > sorted[j].ElapsedS
		}
		return sorted[i].Agent < sorted[j].Agent
	})
	parts := make([]string, 0, len(sorted))
	for _, t := range sorted {
		if t.ElapsedS > 0 {
			parts = append(parts, t.Agent+" "+FormatShortDuration(time.Duration(t.ElapsedS)*time.Second))
		} else {
			parts = append(parts, t.Agent)
		}
	}
	count := fmt.Sprintf("%d", len(turns))
	if total > 0 {
		count = fmt.Sprintf("%d/%d", len(turns), total)
	}
	noun := "agents"
	if len(turns) == 1 {
		noun = "agent"
	}
	return count + " " + noun + " in turn: " + strings.Join(parts, ", ")
}

// FormatShortDuration renders a duration the way the pills do: "45s", "38m",
// "1h12m". Negative durations read as zero.
func FormatShortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := int(d / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	min := sec / 60
	if min < 60 {
		return fmt.Sprintf("%dm", min)
	}
	return fmt.Sprintf("%dh%dm", min/60, min%60)
}

// TurnsFromNames builds turns with unknown elapsed from a bare name list —
// the shape older spokes report in UpgradeDrainReport.InFlight.
func TurnsFromNames(names []string) []UpgradeTurn {
	if len(names) == 0 {
		return nil
	}
	out := make([]UpgradeTurn, 0, len(names))
	for _, n := range names {
		out = append(out, UpgradeTurn{Agent: n})
	}
	return out
}
