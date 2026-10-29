package agent

import (
	"time"

	"github.com/hivecommons/hive/crux"
)

// turnLossNow is the clock this file reads, seamed for tests in the same
// var-not-const style as procRoot above it. Production value is time.Now and
// nothing on the teardown path mutates it.
var turnLossNow = time.Now

// turnLossRecentCapacity bounds the per-agent ring of individual records that
// rides the persisted state file. The aggregate counters are unbounded and
// cheap; the individual records exist so an operator can see the shape of the
// loss (a few long turns vs. many short ones), which a handful conveys.
const turnLossRecentCapacity = 20

// TurnInterruption and TurnLoss are defined in pkg/crux as part of the
// Crux runtime surface (hivecommons/hive#13536); aliased here. TurnLoss is
// persisted through snapshot.AgentState into /data/hive-state.json — the
// store that already carries RestartCount — rather than a new sidecar file,
// per the RFC's explicit caution against adding durable store number five.
type TurnInterruption = crux.TurnInterruption

// TurnLoss is the per-agent accumulation that survives the restart it
// measures; see crux.TurnLoss.
type TurnLoss = crux.TurnLoss

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
//
// A turn whose end was already observed (KickOutcome settled for this kick,
// which also archived it under "turn-end") is not interrupted: its output is
// still snapshotted so the final scrollback supersedes the turn-end archive,
// but nothing was lost, so no interruption is recorded and the breaker is not
// armed.
func (m *Manager) tearDownTurnLocked(agent *AgentProcess, reason string) (interrupted, producing bool) {
	if !agent.kickLogPending {
		return false, false
	}
	archived := m.archiveKickLogBytesLocked(agent, reason)
	agent.kickLogPending = false
	if agent.KickOutcome.Settled(agent.LastKick) {
		return false, false
	}
	rec := m.noteTurnInterruptedLocked(agent, reason, archived)
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
