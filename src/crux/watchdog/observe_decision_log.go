package watchdog

import "time"

// observeDecisionRelogInterval bounds how often observe mode re-reports the
// same would-be decision for one agent (hivecommons/hive#19393). Observe mode
// takes no action, so a dead agent stays dead and every sweep reaches the
// same verdict; logging and auditing it each sweep wrote 79 WARNs in 10m for
// one agent stuck on a modal overlay and self-reported that as a fleet
// defect. An hour keeps a persistent verdict visible in the log and audit
// ring while staying below the self-report repeat threshold (5 in 10m).
const observeDecisionRelogInterval = time.Hour

// observeDecisionLog records the last observe-mode decision reported for an
// agent. It is cleared once the agent is no longer dead, so a recovered agent
// that dies again is reported afresh.
type observeDecisionLog struct {
	key      string
	loggedAt time.Time
}

// shouldReportObserveDecisionLocked reports whether an observe-mode decision
// (action + class) should be logged and audited now, and records it when so:
// a new decision, or the same one after observeDecisionRelogInterval, is
// reported; a repeat inside the window is not. Caller holds r.mu.
func shouldReportObserveDecisionLocked(rec *agentRecord, action string, cls PaneClass, now time.Time) bool {
	key := action + "|" + string(cls)
	log := &rec.observeDecision
	if log.key == key && !log.loggedAt.IsZero() &&
		now.Sub(log.loggedAt) < observeDecisionRelogInterval {
		return false
	}
	log.key, log.loggedAt = key, now
	return true
}
