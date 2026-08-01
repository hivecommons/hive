package hub

import (
	"strings"
	"time"
)

// Running-but-inactive agent detection.
//
// An agent process being "up" says almost nothing about whether it is doing
// the job it exists to do. Five situations are visually identical in a status
// table that only shows State, and diagnosing them by hand has repeatedly cost
// real time:
//
//  1. Session exists, CLI never launched — the operator PAUSED it. Not a
//     fault. Must never alarm.
//  2. Session exists, CLI running, but sitting on a login / device-code
//     prompt. State reads "running" throughout; the agent can do nothing.
//  3. Session exists, CLI running and authenticated, but producing no output.
//     A fault only when there is work waiting.
//  4. Session exists, CLI running and working. Healthy.
//  5. The manager believes the agent is running but its tmux session is gone —
//     a zombie.
//
// This file decides which of those the hub raises. The governing constraint is
// that a facet which lights up constantly gets ignored, which is worse than
// not having the facet at all — so the bar for each rule is "an operator would
// agree something is wrong" rather than "an agent is not currently busy".
//
// The evidence all arrives in the heartbeat's AgentSummary; nothing here
// re-derives a spoke-side condition, and no second channel exists.

const (
	// inactiveAgentNeedsLoginGrace is how long an agent may show a login
	// prompt before it is reported. A CLI briefly renders its auth screen
	// during a normal restart, and an operator part-way through a device-code
	// flow is mid-fix, not broken. Comfortably longer than a restart, short
	// enough that a genuinely unauthenticated agent surfaces within one shift.
	inactiveAgentNeedsLoginGrace = 20 * time.Minute

	// inactiveAgentIdleThreshold is how long a running, authenticated agent
	// may produce NO pane output before it counts as idle. Agents legitimately
	// think, wait on CI, and sit between kicks for long stretches, so this is
	// deliberately far above any single operation: it marks "this agent has
	// produced nothing for most of an hour", not "this agent is between
	// tasks".
	//
	// Idleness ALONE is never reported — see queuedWorkForInactivity.
	inactiveAgentIdleThreshold = 45 * time.Minute

	// inactiveAgentMinQueued is how much actionable work must be waiting
	// before an idle agent is called a problem. A hive with an empty queue is
	// SUPPOSED to have idle agents; reporting those would light the facet on
	// every healthy quiet hive and train operators to ignore it. One waiting
	// item is enough to make "and nothing is happening" wrong.
	inactiveAgentMinQueued = 1

	// inactiveAgentStartupGrace is how long after an agent's own launch its
	// signals are ignored. A freshly launched CLI has not yet painted a pane,
	// so its activity clock is legitimately unset; without this, every restart
	// would raise a burst of alerts that clear themselves a minute later.
	inactiveAgentStartupGrace = 10 * time.Minute

	// inactiveAgentsNamedInReason caps how many agent names are listed in the
	// alert text before it summarises the remainder, so one badly-wedged hive
	// cannot push a paragraph into the panel.
	inactiveAgentsNamedInReason = 3
)

// agentInactivityKind classifies why one agent is not working. Ordered by how
// firmly it is a fault: a dead session and a login prompt are unambiguous,
// while idleness depends on there being work to do.
type agentInactivityKind int

const (
	agentInactiveNone agentInactivityKind = iota
	// agentInactiveSessionMissing — state 5. The spoke expected a live tmux
	// session for a running agent and found none.
	agentInactiveSessionMissing
	// agentInactiveNeedsLogin — state 2. Running and unauthenticated.
	agentInactiveNeedsLogin
	// agentInactiveIdleWithWork — state 3, and only while work is queued.
	agentInactiveIdleWithWork
)

// label describes the kind in the words the alert row uses.
func (k agentInactivityKind) label() string {
	switch k {
	case agentInactiveSessionMissing:
		return "session gone"
	case agentInactiveNeedsLogin:
		return "not logged in"
	case agentInactiveIdleWithWork:
		return "idle with work queued"
	default:
		return ""
	}
}

// classifyInactiveAgent decides whether ONE agent is running-but-not-working,
// and why.
//
// queuedWork is the hive's actionable issue+PR backlog; it gates the idle rule
// only. now is passed in so the whole evaluation is deterministic under test.
//
// Returns agentInactiveNone for every agent that is fine — including every
// paused one, which is checked first and unconditionally.
func classifyInactiveAgent(a AgentSummary, queuedWork int, now time.Time) agentInactivityKind {
	// --- Paused is never a fault. The operator chose it. ---
	//
	// Checked before anything else and against BOTH signals: State carries
	// "paused" once Start() has run, but an agent parked before that reports
	// its earlier state with Paused set. Either one means hands off.
	if a.Paused || strings.EqualFold(a.State, agentStatePaused) {
		return agentInactiveNone
	}

	// Only agents the spoke believes are RUNNING can be "running but
	// inactive". A stopped or failed agent is a different condition, already
	// visible as a zero agent count and not this facet's job.
	if !strings.EqualFold(a.State, agentStateRunning) {
		return agentInactiveNone
	}

	// On-demand agents are meant to sit still until called. Idleness is their
	// designed behaviour, so they are exempt from the idle rule — but NOT from
	// the two unambiguous faults below, which are wrong for any agent.
	onDemand := strings.EqualFold(a.Mode, agentModeOnDemand)

	// --- A running agent with no session is a zombie. Unambiguous. ---
	if a.SessionMissing {
		return agentInactiveSessionMissing
	}

	startedAt, startedOK := parseAgentTime(a.StartedAt)

	// Everything below is about an agent that has had time to settle. Within
	// the startup grace its signals are still converging.
	if startedOK && now.Sub(startedAt) < inactiveAgentStartupGrace {
		return agentInactiveNone
	}

	// --- Running, but sitting on a login prompt. ---
	if a.NeedsLogin {
		// Without a start time there is no way to tell a momentary auth screen
		// from a wedged one, so require the grace to have demonstrably passed
		// rather than assuming it has.
		if startedOK && now.Sub(startedAt) >= inactiveAgentNeedsLoginGrace {
			return agentInactiveNeedsLogin
		}
		return agentInactiveNone
	}

	// --- Idle. Reported ONLY when the hive has work waiting. ---
	//
	// This is the rule most likely to produce noise, so it carries the most
	// gating: an empty queue means idleness is correct, an on-demand agent is
	// supposed to be idle, and an unknown activity time is not evidence.
	if onDemand || queuedWork < inactiveAgentMinQueued {
		return agentInactiveNone
	}
	lastActivity, activityOK := parseAgentTime(a.LastActivityAt)
	if !activityOK {
		// The spoke has never observed a pane change for this agent. That is
		// the normal state of a spoke too old to report the field, so it must
		// read as UNKNOWN — inferring idleness here would light the facet for
		// every agent on every hive that has not upgraded yet.
		return agentInactiveNone
	}
	if now.Sub(lastActivity) >= inactiveAgentIdleThreshold {
		return agentInactiveIdleWithWork
	}
	return agentInactiveNone
}

// Wire values for the agent fields the classifier reads. Named so a typo
// cannot silently turn a rule off — a misspelled literal here would simply
// never match, and the facet would go quiet without failing anything.
const (
	agentStateRunning = "running"
	agentStatePaused  = "paused"
	agentModeOnDemand = "on_demand"
)

// parseAgentTime parses a spoke-reported RFC3339 timestamp. An empty,
// unparseable or future value returns ok=false so every caller treats it as
// UNKNOWN. A future timestamp is clock skew, not activity, and must not be
// read as either fresh or stale.
func parseAgentTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// inactiveAgentReport is the per-hive result: which agents are running but not
// working, and a sentence naming them.
type inactiveAgentReport struct {
	// Count is how many agents are affected. Zero means the hive is fine and
	// nothing is rendered.
	Count int
	// Reason is the human sentence for the alert row and the row pill.
	Reason string
}

// evaluateInactiveAgents classifies every agent on one hive.
//
// queuedWork gates the idle rule; pass the hive's actionable issue+PR total.
// A hive whose spoke reports no agents at all yields an empty report — that is
// a different condition (zero agents configured) with its own signal.
func evaluateInactiveAgents(agents []AgentSummary, queuedWork int, now time.Time) inactiveAgentReport {
	var affected []string
	for _, a := range agents {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		kind := classifyInactiveAgent(a, queuedWork, now)
		if kind == agentInactiveNone {
			continue
		}
		affected = append(affected, name+" ("+kind.label()+")")
	}
	if len(affected) == 0 {
		return inactiveAgentReport{}
	}

	shown := affected
	suffix := ""
	if len(shown) > inactiveAgentsNamedInReason {
		shown = shown[:inactiveAgentsNamedInReason]
		suffix = " (+" + itoa(len(affected)-inactiveAgentsNamedInReason) + " more)"
	}
	noun := "agent is"
	if len(affected) != 1 {
		noun = "agents are"
	}
	return inactiveAgentReport{
		Count:  len(affected),
		Reason: itoa(len(affected)) + " " + noun + " running but not working: " + strings.Join(shown, ", ") + suffix,
	}
}
