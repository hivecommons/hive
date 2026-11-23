package watchdog

import "time"

// credentialFailureRelogInterval bounds how often one agent's unchanged
// credential failure is logged at ERROR (#19205). An hour keeps a persistent
// failure visible in the log and below the self-report repeat threshold
// (5 in 10m), which a single misconfigured agent must not trip on its own.
const credentialFailureRelogInterval = time.Hour

// authFailureLog records the last credential failure logged at ERROR, so a
// verdict that keeps re-entering False for the same reason
// (hivecommons/hive#19205) is logged once per credentialFailureRelogInterval
// instead of on every re-entry. Cleared by a True verdict: a recovered
// credential that fails again is news.
type authFailureLog struct {
	reason   string
	loggedAt time.Time
}

// logAuthFailure logs a False auth transition: at ERROR for a new reason or
// once the relog window has passed, at INFO for a repeat inside it.
func (r *Reconciler) logAuthFailure(name, backend, reason, message string, now time.Time) {
	if r.shouldLogAuthFailure(name, reason, now) {
		r.logger.Error("watchdog: agent credential failure", "agent", name, "backend", backend, "reason", reason, "detail", message)
		return
	}
	// The same failure re-entered False inside the relog window: the verdict
	// flapped through Unknown (a copilot/codex agent with no credential file
	// reads "no opinion" while its CLI is running, then "missing" again once
	// it stops), the credential did not change. One ERROR per window is the
	// operator signal; repeating it on every relaunch filed the flap
	// fleet-wide as 8 failures in 10m (#19205). The dashboard alert is still
	// re-raised by the caller.
	r.logger.Info("watchdog: agent credential still failing", "agent", name, "backend", backend, "reason", reason, "detail", message)
}

// shouldLogAuthFailure reports whether a False transition for reason should
// be logged at ERROR, and records it when so: a new reason, or the same one
// after credentialFailureRelogInterval, is logged; a repeat inside the
// window is not.
func (r *Reconciler) shouldLogAuthFailure(name, reason string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	log := &r.recordLocked(name).authFailure
	if log.reason == reason && !log.loggedAt.IsZero() &&
		now.Sub(log.loggedAt) < credentialFailureRelogInterval {
		return false
	}
	log.reason, log.loggedAt = reason, now
	return true
}

// clearAuthFailureLog forgets the last logged failure after a True verdict.
func (r *Reconciler) clearAuthFailureLog(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordLocked(name).authFailure = authFailureLog{}
}
