package agent

// Measuring what a restart COSTS, not just how often it happens (#4002).
//
// RFC #4002 ("re-entrant conversation-as-state agent turn model") argues from
// "frequent spoke rolls discard agent work". The step-1 spike
// (src/docs/design/agent-turn-model.md) found that claim unsized and recorded
// it as open question 3: `RestartCount` (src/pkg/snapshot/state.go) counts
// restarts but says nothing about what each one cost, and the spike's own
// closing line makes step 4's feasibility judgement depend on answering it.
//
// This file is that instrument. It records, durably, what hive discarded each
// time it tore down an agent that was mid-turn.
//
// WHAT COUNTS AS AN INTERRUPTION
//
// A turn is interrupted when hive kills an agent's CLI and tmux session while
// a delivered kick's output has not yet been rotated (`kickLogPending`) — the
// restart and shutdown paths. Normal kick rotation is deliberately NOT counted:
// a kick is only delivered to a pane sitting at its input marker, so rotation
// means the previous turn finished. Counting it would drown the signal in
// completed work.
//
// WHY TWO CLOCKS, AND WHY NEITHER ALONE IS HONEST
//
// `kickLogPending` means "output not yet archived", which is NOT the same as
// "the turn was still running": an agent that finished its turn and then sat
// idle for an hour still carries pending output, and charging that whole hour
// to the restart would overstate the loss — the exact overclaim this RFC does
// not need. So each record carries two independent quantities and one
// threshold-free discriminator:
//
//   - SinceKick is teardown minus kick delivery. It is an UPPER BOUND on what
//     the interruption could have cost, never a measurement of what it did.
//   - SinceOutput is teardown minus the last observed pane change — how recently
//     the agent was visibly doing anything. A large SinceOutput next to a large
//     SinceKick says the agent was idle and little was actually lost.
//   - Producing is true when the pane changed AFTER the kick landed, i.e. the
//     agent produced something during this turn. It is binary and needs no
//     threshold, which is why the aggregate carries it: "interruptions that hit
//     an agent doing work" is answerable from the fleet without anyone first
//     agreeing on how many idle seconds mean idle.
//
// Analysis discounts with SinceOutput; nothing here decides for it. Deciding
// would be taking the RFC's step-4 judgement inside an instrument built to
// inform it.

import "time"

// turnLossNow is the clock this file reads, seamed for tests in the same
// var-not-const style as procRoot above it. Production value is time.Now and
// nothing on the teardown path mutates it.
var turnLossNow = time.Now

// turnLossRecentCapacity bounds the per-agent ring of individual records that
// rides the persisted state file. The aggregate counters are unbounded and
// cheap; the individual records exist so an operator can see the shape of the
// loss (a few long turns vs. many short ones), which a handful conveys.
const turnLossRecentCapacity = 20

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

// TurnLoss is the per-agent accumulation that survives the restart it measures.
// Persisted through snapshot.AgentState into /data/hive-state.json — the store
// that already carries RestartCount — rather than a new sidecar file, per the
// RFC's explicit caution against adding durable store number five.
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
	// Recent is the newest turnLossRecentCapacity records, oldest first.
	Recent []TurnInterruption
}

// noteTurnInterruptedLocked records one interrupted turn against the agent.
// Callers must hold m.mu; it reads pane observations under paneMu, which is
// the same m.mu-then-paneMu order snapshot() takes.
//
// archivedBytes is what archiveKickLogBytesLocked reported for this teardown.
func (m *Manager) noteTurnInterruptedLocked(agent *AgentProcess, reason string, archivedBytes int) TurnInterruption {
	now := turnLossNow()

	rec := TurnInterruption{At: now, Reason: reason, Bytes: archivedBytes}
	if agent.LastKick != nil {
		if d := now.Sub(*agent.LastKick); d > 0 {
			rec.SinceKick = d
		}
	}

	agent.paneMu.RLock()
	lastPaneChange := agent.LastPaneChange
	agent.paneMu.RUnlock()
	if !lastPaneChange.IsZero() {
		d := now.Sub(lastPaneChange)
		if d < 0 {
			d = 0
		}
		rec.SinceOutput = &d
		// Producing needs BOTH clocks: a pane change is only evidence about
		// this turn if it happened after this turn's kick was delivered.
		if agent.LastKick != nil && lastPaneChange.After(*agent.LastKick) {
			rec.Producing = true
		}
	}

	agent.TurnLoss.Interruptions++
	agent.TurnLoss.UpperBound += rec.SinceKick
	agent.TurnLoss.Bytes += int64(rec.Bytes)
	if rec.Producing {
		agent.TurnLoss.Producing++
	}
	if len(agent.TurnLoss.Recent) >= turnLossRecentCapacity {
		agent.TurnLoss.Recent = agent.TurnLoss.Recent[len(agent.TurnLoss.Recent)-turnLossRecentCapacity+1:]
	}
	agent.TurnLoss.Recent = append(agent.TurnLoss.Recent, rec)

	// Logged as well as persisted: the persisted aggregate answers "what has
	// this agent lost", but answering the RFC's question across a fleet means
	// collecting these from log aggregation without reading every spoke's PVC.
	attrs := []any{
		"name", agent.Name,
		"reason", reason,
		"since_kick_s", rec.SinceKick.Seconds(),
		"producing", rec.Producing,
		"bytes", rec.Bytes,
		"interruptions_total", agent.TurnLoss.Interruptions,
	}
	if rec.SinceOutput != nil {
		attrs = append(attrs, "since_output_s", rec.SinceOutput.Seconds())
	}
	m.logger.Info("audit: turn interrupted mid-flight", attrs...)
	return rec
}

// tearDownTurnLocked archives an outgoing session's kick output and records
// what the teardown discarded, then clears the pending flag. It is the single
// funnel for "a turn is about to be destroyed", so the accounting cannot drift
// between the restart paths the way three hand-copied blocks would.
//
// It must run BEFORE whatever destroys the scrollback — kill-session on
// restart, container teardown on shutdown. Callers must hold m.mu.
//
// It reports whether a turn was interrupted at all and whether that turn was
// producing (the pane changed after the kick landed). The restart path uses
// the second answer to arm the restart/kick breaker (#7363): the accounting
// here already knew each restart in the observed loop was destroying a
// producing turn; now something acts on it.
func (m *Manager) tearDownTurnLocked(agent *AgentProcess, reason string) (interrupted, producing bool) {
	if !agent.kickLogPending {
		return false, false
	}
	archived := m.archiveKickLogBytesLocked(agent, reason)
	rec := m.noteTurnInterruptedLocked(agent, reason, archived)
	agent.kickLogPending = false
	return true, rec.Producing
}

// SeedTurnLoss restores an agent's accumulated turn-loss record from persisted
// state at boot. Without it the measurement would reset on exactly the event it
// exists to measure, and every spoke would permanently report the loss of its
// current process lifetime only.
func (m *Manager) SeedTurnLoss(name string, loss TurnLoss) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		return
	}
	if len(loss.Recent) > turnLossRecentCapacity {
		loss.Recent = loss.Recent[len(loss.Recent)-turnLossRecentCapacity:]
	}
	recent := make([]TurnInterruption, len(loss.Recent))
	copy(recent, loss.Recent)
	loss.Recent = recent
	agent.TurnLoss = loss
}

// cloneTurnLoss deep-copies the record for AllStatuses/snapshot, so a reader
// iterating Recent cannot race the next teardown appending to it.
func cloneTurnLoss(loss TurnLoss) TurnLoss {
	if len(loss.Recent) == 0 {
		loss.Recent = nil
		return loss
	}
	recent := make([]TurnInterruption, len(loss.Recent))
	copy(recent, loss.Recent)
	loss.Recent = recent
	return loss
}
