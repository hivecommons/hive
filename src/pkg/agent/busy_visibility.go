package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	busyNoActivityCondition        = "busy-no-activity"
	busyOverCeilingCondition       = "busy-over-ceiling"
	busyUndeliverableThreshold     = 3
	transcriptLivenessScanInterval = 10 * time.Second
)

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
// or otherwise unsafe for input. Caller holds m.mu.
func (m *Manager) markKickUndeliverableLocked(agent *AgentProcess, now time.Time) {
	if agent.KicksUndeliverable == 0 {
		agent.BusySince = now
	}
	agent.KicksUndeliverable++
	m.refreshBusyVisibilityLocked(agent, now)
}

// resetBusyVisibilityLocked clears the busy-delivery episode. Caller holds
// m.mu.
func (m *Manager) resetBusyVisibilityLocked(agent *AgentProcess) {
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
