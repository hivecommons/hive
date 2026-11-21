package watchdog

import (
	"fmt"
	"time"
)

// observeDecisionRelogInterval bounds how often observe mode re-reports the
// same would-be decision for one agent (hivecommons/hive#19393). Observe mode
// takes no action, so a dead agent stays dead and every sweep reaches the
// same verdict; logging and auditing it each sweep wrote 79 WARNs in 10m for
// one agent stuck on a modal overlay and self-reported that as a fleet
// defect. An hour keeps a persistent verdict visible in the log and audit
// ring while staying below the self-report repeat threshold (5 in 10m).
const observeDecisionRelogInterval = time.Hour

// observeDecisionLog records when each observe-mode decision (action +
// class) was last reported for an agent. It keeps one slot per decision, so
// an agent flapping between dead classes (stuck-overlay, no-output, ...)
// without ever recovering is still reported at most once per decision per
// window rather than on every flip. It is cleared once the agent is ready
// again, so a recovered agent that dies again is reported afresh.
type observeDecisionLog struct {
	loggedAt map[string]time.Time
}

// shouldReportObserveDecisionLocked reports whether an observe-mode decision
// (action + class) should be logged and audited now, and records it when so:
// a new decision, or the same one after observeDecisionRelogInterval, is
// reported; a repeat inside the window is not. Caller holds r.mu.
func shouldReportObserveDecisionLocked(rec *agentRecord, action string, cls PaneClass, now time.Time) bool {
	key := action + "|" + string(cls)
	log := &rec.observeDecision
	if at, ok := log.loggedAt[key]; ok && now.Sub(at) < observeDecisionRelogInterval {
		return false
	}
	if log.loggedAt == nil {
		log.loggedAt = make(map[string]time.Time)
	}
	log.loggedAt[key] = now
	return true
}

// resetOnReady clears the record once the agent is ready again, so a later
// death is news to observe mode (#19393). Only a ready pane counts as a
// recovery: a transient unknown or auth-required capture between two dead
// sweeps must not reopen the window, or a flapping agent re-reports each cycle.
// The map holds at most one entry per (action, dead class), so it stays tiny.
func (l *observeDecisionLog) resetOnReady(cls PaneClass) {
	if cls == ClassReady {
		l.loggedAt = nil
	}
}

// reportObservePauseLocked logs and audits an observe-mode would-pause
// verdict, at most once per observeDecisionRelogInterval. Caller holds r.mu.
func (r *Reconciler) reportObservePauseLocked(name string, rec *agentRecord, cls Classification, detail string, now time.Time) {
	if !shouldReportObserveDecisionLocked(rec, auditActionPause, cls.Class, now) {
		return
	}
	r.logger.Warn("watchdog: would escalate to pause (observe mode; no action taken)",
		"agent", name, "failures", rec.Failures, "class", string(cls.Class), "mode", string(r.settings.Mode))
	r.audit(auditActionFor(auditActionPause, false), "would pause: "+detail, name)
}

// reportObserveRestartLocked logs and audits an observe-mode would-restart
// verdict, at most once per observeDecisionRelogInterval. Caller holds r.mu.
func (r *Reconciler) reportObserveRestartLocked(name string, rec *agentRecord, cls Classification, now time.Time) {
	if !shouldReportObserveDecisionLocked(rec, auditActionRestart, cls.Class, now) {
		return
	}
	r.logger.Warn("watchdog: would restart dead agent (observe mode; no action taken)",
		"agent", name, "class", string(cls.Class), "reason", cls.Reason, "mode", string(r.settings.Mode))
	r.audit(auditActionFor(auditActionRestart, false),
		fmt.Sprintf("would restart: class=%s reason=%s", cls.Class, cls.Reason), name)
}
