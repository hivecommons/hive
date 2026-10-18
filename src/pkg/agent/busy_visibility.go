package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

const (
	busyNoActivityCondition        = "busy-no-activity"
	busyOverCeilingCondition       = "busy-over-ceiling"
	copilotQuestionFormCondition   = "copilot-question-form"
	busyUndeliverableThreshold     = 3
	transcriptLivenessScanInterval = 10 * time.Second
	// busyNoActivityRecoveryKicks and busyNoActivityRecoveryFactor bound when
	// a busy-no-activity pane stops being visibility-only and is restarted
	// (#12505): twice the kicks that raise the condition, and twice the
	// configured silence threshold (60m by default). A live turn writes
	// transcript events, so only a wedged CLI clears both bars.
	busyNoActivityRecoveryKicks  = 2 * busyUndeliverableThreshold
	busyNoActivityRecoveryFactor = 2
)

const copilotQuestionFormMessage = "Copilot CLI asked the user a question; no human is attached — dismissed with Escape so the kick can be delivered"

type transcriptActivityScan struct {
	name string
	uid  int
}

type transcriptActivityResult struct {
	name string
	at   time.Time
	ok   bool
}

// newestCopilotTranscriptActivity reports the newest events.jsonl mtime under
// the agent's own Copilot session-state tree. Sub-agents write separate
// session directories under the same HOME, so scanning every session catches
// their activity without parsing JSON.
func newestCopilotTranscriptActivity(home string) (time.Time, bool) {
	root := filepath.Join(home, ".copilot", "session-state")
	entries, err := os.ReadDir(root)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(root, entry.Name(), "events.jsonl"))
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, false
	}
	return newest, true
}

// markKickUndeliverableLocked records that a kick attempt found the pane busy
// or otherwise unsafe for input. reason says why delivery failed and is carried
// in the self-report. Caller holds m.mu.
func (m *Manager) markKickUndeliverableLocked(agent *AgentProcess, now time.Time, reason string) {
	if agent.KicksUndeliverable == 0 {
		agent.BusySince = now
	}
	agent.KicksUndeliverable++
	m.refreshBusyVisibilityLocked(agent, now)
	if agent.BusyCondition != "" {
		reason = agent.BusyCondition
	}
	if reason == "" {
		reason = "unknown"
	}
	// A pane that is busy because the agent is mid-turn is not a stall
	// (#12268): the governor re-kicks an agent whose previous turn is still
	// streaming, each attempt waits out the prompt timeout, and three of those
	// inside one long review turn used to file an upstream issue that cleared
	// itself the moment the turn ended. Only self-report once the agent's own
	// transcript has been silent for the busy-no-activity threshold, the same
	// bar busyCondition applies before it calls the pane stuck.
	silentFor, live := kickUndeliverableTranscriptLiveness(agent, now)
	if live {
		if m.logger != nil {
			m.logger.Info("kick undeliverable while agent turn in progress; not self-reporting",
				"agent", agent.Name,
				"reason", reason,
				"kicks_undeliverable", agent.KicksUndeliverable,
				"transcript_silent_for", roundDurationForHumans(silentFor))
		}
		return
	} else if silentFor > 0 {
		reason = fmt.Sprintf("%s; transcript silent for %s", reason, roundDurationForHumans(silentFor))
	}
	// Decision B on #12787: a stall that busyNoActivityStall is about to
	// restart is not yet a defect worth a fleet issue — most self-recover.
	// Until that restart has been attempted the condition stays a dashboard
	// warning (BusyCondition) and a log line; only kicks that keep failing
	// after the restart are self-reported.
	if agent.busyNoActivityRestartAt.IsZero() {
		if m.logger != nil {
			m.logger.Warn("kick undeliverable; busy-no-activity restart not yet attempted, not self-reporting",
				"agent", agent.Name,
				"reason", reason,
				"kicks_undeliverable", agent.KicksUndeliverable,
				"transcript_silent_for", roundDurationForHumans(silentFor))
		}
		return
	}
	// Self-report (#11868): the tracker applies the ≥3-consecutive threshold.
	fleetreport.Observe(fleetreport.ClassKickFailedRepeatedly, agent.Name,
		busyNoActivityRestartFailedMessage(agent, now, reason), "")
}

// busyNoActivityRestartFailedMessage is the kick_failed_repeatedly self-report
// text once the stall restart has been attempted and kicks still fail. It
// carries the restart time and count so the fleet issue says the recovery
// already ran and did not help, not merely that kicks are failing.
func busyNoActivityRestartFailedMessage(agent *AgentProcess, now time.Time, reason string) string {
	restartAgo := now.Sub(agent.busyNoActivityRestartAt)
	if restartAgo < 0 {
		restartAgo = 0
	}
	return fmt.Sprintf("kick undeliverable %d times in a row after busy-no-activity restart did not recover the session (%s; restarted %s ago at %s, %d restart(s) this episode)",
		agent.KicksUndeliverable, reason, roundDurationForHumans(restartAgo),
		agent.busyNoActivityRestartAt.UTC().Format(time.RFC3339), agent.busyNoActivityRestarts)
}

// markBusyNoActivityRestartLocked records that busyNoActivityStall restarted
// the agent at now. Caller holds m.mu.
func (m *Manager) markBusyNoActivityRestartLocked(agent *AgentProcess, now time.Time) {
	agent.busyNoActivityRestartAt = now
	agent.busyNoActivityRestarts++
}

// reportBusyNoActivityRestartFailed self-reports a stall restart that itself
// errored: the recovery never ran, so there is nothing left to wait for.
func (m *Manager) reportBusyNoActivityRestartFailed(name, reason string, err error) {
	m.mu.RLock()
	agent, ok := m.agents[name]
	var kicks, restarts int
	if ok {
		kicks, restarts = agent.KicksUndeliverable, agent.busyNoActivityRestarts
	}
	m.mu.RUnlock()
	if !ok {
		return
	}
	fleetreport.Observe(fleetreport.ClassKickFailedRepeatedly, name,
		fmt.Sprintf("kick undeliverable %d times in a row and busy-no-activity restart failed: %v (%s; %d restart(s) this episode)",
			kicks, err, reason, restarts), "")
}

// kickUndeliverableTranscriptLiveness reports how long the agent's transcript
// has been silent and whether that is short enough to call the busy pane a
// turn in progress. Agents without a transcript cache (non-Copilot backends,
// or a scan that has not run yet) report (0, false): the caller cannot tell a
// live turn from a stall, so it keeps self-reporting as before.
func kickUndeliverableTranscriptLiveness(agent *AgentProcess, now time.Time) (time.Duration, bool) {
	last := agent.LastTranscriptActivity
	if last.IsZero() {
		return 0, false
	}
	silentFor := now.Sub(last)
	if silentFor < 0 {
		silentFor = 0
	}
	return silentFor, silentFor < agent.Config.EffectiveBusyNoActivityThreshold()
}

// markCopilotQuestionFormLocked surfaces a question-form dismissal through the
// same dashboard busy-condition fields as other no-deliverable-kick states.
// Caller holds m.mu.
func (m *Manager) markCopilotQuestionFormLocked(agent *AgentProcess, now time.Time) {
	if agent.KicksUndeliverable == 0 {
		agent.BusySince = now
	}
	agent.KicksUndeliverable++
	agent.BusyCondition = copilotQuestionFormCondition
	agent.BusyConditionMessage = copilotQuestionFormMessage
	agent.busyConditionWarned = copilotQuestionFormCondition
}

// resetBusyVisibilityLocked clears the busy-delivery episode because a kick
// was delivered (or cancelled): the session is active again, so any
// kick_failed_repeatedly condition is cleared and the stall-restart marker
// dropped. Caller holds m.mu.
func (m *Manager) resetBusyVisibilityLocked(agent *AgentProcess) {
	if agent.KicksUndeliverable > 0 || !agent.busyNoActivityRestartAt.IsZero() {
		fleetreport.Clear(fleetreport.ClassKickFailedRepeatedly, agent.Name)
	}
	agent.busyNoActivityRestartAt = time.Time{}
	agent.busyNoActivityRestarts = 0
	m.resetBusyCountersLocked(agent)
}

// resetBusyVisibilityForRestartLocked is the restart teardown's variant: the
// counters start over for the new session, but the stall-restart marker and
// any open self-report stay until a kick is actually delivered — a restart is
// the recovery attempt, not proof that it worked. Caller holds m.mu.
func (m *Manager) resetBusyVisibilityForRestartLocked(agent *AgentProcess) {
	m.resetBusyCountersLocked(agent)
}

func (m *Manager) resetBusyCountersLocked(agent *AgentProcess) {
	agent.KicksUndeliverable = 0
	agent.BusySince = time.Time{}
	agent.BusyCondition = ""
	agent.BusyConditionMessage = ""
	agent.busyConditionWarned = ""
}

// refreshBusyVisibilityLocked derives the visibility-only busy condition from
// cached transcript liveness. Caller holds m.mu.
func (m *Manager) refreshBusyVisibilityLocked(agent *AgentProcess, now time.Time) {
	condition, message := busyCondition(agent, now)
	agent.BusyCondition = condition
	agent.BusyConditionMessage = message
	if condition == "" {
		agent.busyConditionWarned = ""
		return
	}
	if agent.busyConditionWarned == condition {
		return
	}
	agent.busyConditionWarned = condition
	if m.logger == nil {
		return
	}
	m.logger.Warn("agent busy with no deliverable kick visibility condition",
		"agent", agent.Name,
		"condition", condition,
		"message", message,
		"kicks_undeliverable", agent.KicksUndeliverable,
		"busy_since", agent.BusySince.UTC().Format(time.RFC3339),
		"last_transcript_activity", formatTimeForLog(agent.LastTranscriptActivity))
}

// pendingTranscriptActivityScansLocked returns the Copilot agents whose
// transcript cache is stale. Caller holds m.mu; filesystem scans happen after
// releasing it.
func (m *Manager) pendingTranscriptActivityScansLocked(now time.Time) []transcriptActivityScan {
	scans := make([]transcriptActivityScan, 0, len(m.agents))
	for _, agent := range m.agents {
		if effectiveBackend(agent) != "copilot" {
			continue
		}
		if !agent.lastTranscriptScanAt.IsZero() && now.Sub(agent.lastTranscriptScanAt) < transcriptLivenessScanInterval {
			continue
		}
		scans = append(scans, transcriptActivityScan{name: agent.Name, uid: agent.UID})
	}
	return scans
}

func scanTranscriptActivity(scans []transcriptActivityScan) []transcriptActivityResult {
	results := make([]transcriptActivityResult, 0, len(scans))
	for _, scan := range scans {
		at, ok := newestCopilotTranscriptActivity(AgentHome(scan.name, scan.uid, "copilot"))
		results = append(results, transcriptActivityResult{name: scan.name, at: at, ok: ok})
	}
	return results
}

// applyTranscriptActivityResultsLocked updates the transcript cache after an
// out-of-lock scan. Caller holds m.mu.
func (m *Manager) applyTranscriptActivityResultsLocked(results []transcriptActivityResult, scannedAt time.Time) {
	for _, result := range results {
		agent, ok := m.agents[result.name]
		if !ok {
			continue
		}
		agent.lastTranscriptScanAt = scannedAt
		if result.ok {
			agent.LastTranscriptActivity = result.at
		}
	}
}

func busyCondition(agent *AgentProcess, now time.Time) (string, string) {
	threshold := agent.Config.EffectiveBusyNoActivityThreshold()
	lastActivity := agent.LastTranscriptActivity
	if lastActivity.IsZero() {
		lastActivity = agent.BusySince
	}
	silentFor := time.Duration(0)
	if !lastActivity.IsZero() {
		silentFor = now.Sub(lastActivity)
	}

	if ceiling := agent.Config.MaxTurnDuration; ceiling > 0 {
		start := agent.BusySince
		if !start.IsZero() && now.Sub(start) >= ceiling {
			return busyOverCeilingCondition, fmt.Sprintf(
				"pane shows Working for %s, exceeding max_turn_duration %s; %d kicks undeliverable",
				roundDurationForHumans(now.Sub(start)), roundDurationForHumans(ceiling), agent.KicksUndeliverable)
		}
	}

	if agent.KicksUndeliverable >= busyUndeliverableThreshold && !lastActivity.IsZero() && silentFor >= threshold {
		since := ""
		if !agent.BusySince.IsZero() {
			since = agent.BusySince.UTC().Format("15:04Z")
		}
		return busyNoActivityCondition, fmt.Sprintf(
			"pane shows Working but no transcript activity for %s; %d kicks undeliverable since %s",
			roundDurationForHumans(silentFor), agent.KicksUndeliverable, since)
	}

	return "", ""
}

// busyNoActivityStall reports why a pane that still shows Working, refuses
// every kick, and has produced no transcript activity for far longer than the
// busy-no-activity threshold should be restarted, or "" when it should not.
// Without this the condition was visibility-only: a wedged CLI refused kicks
// indefinitely and nothing broke the loop (#12505). Restarts share the
// session-liveness cooldown so a pane that wedges again is not churned.
// Caller holds m.mu (read or write).
func busyNoActivityStall(agent *AgentProcess, pane string, now time.Time) string {
	if agent.KicksUndeliverable < busyNoActivityRecoveryKicks {
		return ""
	}
	if !paneShowsAgentWorking(pane) && !paneShowsActiveWork(pane) {
		return ""
	}
	if now.Sub(agent.lastSessionStallRestart) < sessionStallRestartCooldown {
		return ""
	}
	lastActivity := agent.LastTranscriptActivity
	if lastActivity.IsZero() {
		lastActivity = agent.BusySince
	}
	if lastActivity.IsZero() {
		return ""
	}
	silentFor := now.Sub(lastActivity)
	if silentFor < busyNoActivityRecoveryFactor*agent.Config.EffectiveBusyNoActivityThreshold() {
		return ""
	}
	return fmt.Sprintf("%s: pane shows Working with no transcript activity for %s; %d kicks undeliverable",
		busyNoActivityCondition, roundDurationForHumans(silentFor), agent.KicksUndeliverable)
}

func roundDurationForHumans(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

func formatTimeForLog(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
