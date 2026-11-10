package watchdog

import "time"

// classifyLocked runs the pane state machine for one agent and layers the
// reconciler's per-agent login streak on top: auth-required only stands once
// the pane has shown login chrome on Settings.LoginStreak consecutive polls.
// A relaunched CLI flashes login chrome for a fraction of a second while it
// re-validates its token, and the manager cannot date every relaunch (the CLI
// re-executing itself, for one), so one sighting must not page an operator
// (#17320). Caller holds r.mu.
func (r *Reconciler) classifyLocked(rec *agentRecord, obs Observation, now time.Time) Classification {
	cls := Classify(obs, now, r.settings)
	if cls.Class == ClassAuthRequired {
		rec.loginStreak++
	} else {
		rec.loginStreak = 0
	}
	return ApplyLoginStreak(cls, rec.loginStreak, r.settings.LoginStreak)
}
