package crux

// ConditionReporter receives the runtime's self-reported error conditions:
// a (class, key) pair observed with a message and a scrubbed diagnostics
// excerpt, and cleared when the condition recovers. The runtime opts an
// error site in by naming one of the Class* constants below; what counts as
// reportable (streaks, windows, persistence) and where a report goes is the
// host's business. Hive wires pkg/fleetreport's runtime tracker through
// Manager.SetConditionReporter; a nil reporter drops observations.
type ConditionReporter interface {
	Observe(class, key, message, diagnostics string)
	Clear(class, key string)
}

// Runtime error classes the agent runtime and its watchdog emit. Each is an
// explicit allow-list entry on the host side (pkg/fleetreport.Classes); the
// values are the wire names and must not change.
const (
	ClassRestartStorm             = "restart_storm"
	ClassKickFailedRepeatedly     = "kick_failed_repeatedly"
	ClassDataDiskPressure         = "data_disk_pressure"
	ClassGitHubAppTokenMintFailed = "github_app_token_mint_failed"
	ClassAgentStall               = "agent_stall"
	ClassAgentCrashLoop           = "agent_crash_loop"
	ClassCIPollNudge              = "ci_poll_nudge"
)
