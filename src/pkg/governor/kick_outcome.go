package governor

// Kick outcomes (#7421).
//
// RecordKick fires when a kick is DISPATCHED. It is the right moment to stamp
// LastKick — the cadence must not re-kick an agent mid-turn — but it says
// nothing about what the turn produced. The agent manager classifies how each
// kicked turn ended once the CLI is back at its prompt (pkg/agent/kick_outcome.go)
// and hands the verdict here. This file is what the governor DOES with it:
//
//   - a clarifying question ("What should I focus on?") is a defect: the kick
//     told the agent what to do and it asked anyway. The agent is re-kicked
//     after a short delay instead of sitting out the full cadence interval,
//     at most once per questionRekickCooldown so a model that answers every
//     kick with a question cannot turn the cadence into a token-burning loop;
//   - a policy stand-down is a legitimate refusal: recorded as blocked, with
//     its reason, for the dashboard — never re-kicked early (the cause is a
//     policy condition a re-kick would just hit again);
//   - an explicit nothing-produced report is recorded as such;
//   - "ended" means only that no no-op signature was recognised.
//
// Every verdict is also stamped onto the matching kick-history record, so the
// persisted history distinguishes a kick that produced work from one that did
// not, instead of counting both as a kick.

import "time"

// Outcome kinds. These mirror pkg/agent's KickOutcome* strings byte for byte;
// the packages share no import edge, so the strings are the contract.
const (
	KickOutcomeQuestion  = "question"
	KickOutcomeStandDown = "stand-down"
	KickOutcomeNoOp      = "no-op"
	KickOutcomeEnded     = "ended"
)

// questionRekickDelay is how long after a question-ended turn the agent is
// re-kicked. Short, so the defect costs minutes rather than a cadence
// interval; non-zero, so an operator watching the pane has a moment to see
// what happened and intervene.
const questionRekickDelay = 5 * time.Minute

// questionRekickCooldown bounds the re-kick to once per window per agent. A
// second question inside the window is recorded but NOT re-kicked: the
// cadence resumes its normal interval.
const questionRekickCooldown = time.Hour

// KickOutcomeRecord is the latest classified turn ending for an agent.
type KickOutcomeRecord struct {
	Agent  string    `json:"agent"`
	Kind   string    `json:"kind"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	KickAt time.Time `json:"kickAt"`
	// Rekicked is set once this outcome has earned its early re-kick (or
	// inherited an earlier one inside the cooldown), so it never earns two.
	Rekicked bool `json:"rekicked,omitempty"`
}

// Blocked reports whether the outcome is a legitimate refusal the dashboard
// should show as blocked rather than as a completed kick.
func (r KickOutcomeRecord) Blocked() bool {
	return r.Kind == KickOutcomeStandDown
}

// RecordKickOutcome records how an agent's kicked turn ended. kickAt is the
// manager's delivery time for that kick; at is when the turn was seen to end.
func (g *Governor) RecordKickOutcome(agentName, kind, reason string, kickAt, at time.Time) {
	if agentName == "" || kind == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	rec := KickOutcomeRecord{Agent: agentName, Kind: kind, Reason: reason, At: at, KickAt: kickAt}
	if prev, ok := g.kickOutcomes[agentName]; ok && kind == KickOutcomeQuestion &&
		prev.Kind == KickOutcomeQuestion && prev.Rekicked && at.Sub(prev.At) < questionRekickCooldown {
		// The re-kick already happened and the agent asked again: one early
		// retry per window, then back to the cadence.
		rec.Rekicked = true
	}
	g.kickOutcomes[agentName] = rec

	// Stamp the most recent kick-history record for this agent so the history
	// stops counting a fruitless kick as a kick.
	for i := len(g.kickHistory) - 1; i >= 0; i-- {
		if g.kickHistory[i].Agent != agentName || g.kickHistory[i].Timestamp.After(at) {
			continue
		}
		g.kickHistory[i].Outcome = kind
		g.kickHistory[i].OutcomeReason = reason
		break
	}

	if g.logger == nil {
		return
	}
	attrs := []any{"agent", agentName, "outcome", kind, "reason", reason, "rekick_eligible", kind == KickOutcomeQuestion && !rec.Rekicked}
	switch kind {
	case KickOutcomeQuestion:
		g.logger.Warn("kick produced no work: the agent ended its turn asking the operator what to do", attrs...)
	case KickOutcomeStandDown:
		g.logger.Warn("kick produced no work: the agent stood down on policy (recorded as blocked)", attrs...)
	case KickOutcomeNoOp:
		g.logger.Warn("kick produced no work: the agent reported nothing opened", attrs...)
	default:
		g.logger.Debug("kick turn ended", attrs...)
	}
}

// questionRekickDueLocked reports whether the agent's last kick ended on a
// clarifying question that has not yet been retried, and the retry delay has
// elapsed. It marks the outcome as re-kicked when it answers true, so the
// caller's selection of the agent IS the one retry. Callers must hold g.mu.
func (g *Governor) questionRekickDueLocked(agentName string, lastKick, now time.Time) bool {
	rec, ok := g.kickOutcomes[agentName]
	if !ok || rec.Kind != KickOutcomeQuestion || rec.Rekicked {
		return false
	}
	// The outcome must belong to the kick the cadence is waiting on: observed
	// after it was dispatched. An older verdict describes an older turn.
	if !lastKick.IsZero() && !rec.At.After(lastKick) {
		return false
	}
	if now.Sub(rec.At) < questionRekickDelay {
		return false
	}
	rec.Rekicked = true
	g.kickOutcomes[agentName] = rec
	if g.logger != nil {
		g.logger.Info("re-kicking agent early: its last kick ended on a clarifying question (#7421)",
			"agent", agentName, "reason", rec.Reason, "since_question", now.Sub(rec.At).Round(time.Second).String())
	}
	return true
}

// KickOutcomes returns the latest classified turn ending per agent.
func (g *Governor) KickOutcomes() map[string]KickOutcomeRecord {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]KickOutcomeRecord, len(g.kickOutcomes))
	for k, v := range g.kickOutcomes {
		out[k] = v
	}
	return out
}

// seedKickOutcomesFromHistoryLocked rebuilds the per-agent latest outcome
// from restored kick history, so the dashboard still shows "blocked:
// stand-down" for a turn that ended just before a pod roll. The restored
// record carries the DISPATCH time as At (the observation time is not
// persisted), which is deliberately conservative: questionRekickDueLocked
// requires At to be after the kick, so a restored question never earns an
// early re-kick — the cadence decides. Callers must hold g.mu.
func (g *Governor) seedKickOutcomesFromHistoryLocked() {
	for _, rec := range g.kickHistory {
		if rec.Outcome == "" {
			continue
		}
		g.kickOutcomes[rec.Agent] = KickOutcomeRecord{
			Agent: rec.Agent, Kind: rec.Outcome, Reason: rec.OutcomeReason,
			At: rec.Timestamp, KickAt: rec.Timestamp, Rekicked: true,
		}
	}
}
