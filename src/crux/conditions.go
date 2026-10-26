package crux

import "errors"

// ErrRuntimeUnavailable marks a Runtime call that could not reach the
// runtime at all (a sidecar whose `crux serve` is gone), as opposed to one
// the runtime answered with an error. The in-process runtime never returns
// it. A kick that fails with it was REFUSED, not queued: the host's governor
// re-evaluates on its next tick and the kick is re-derived from current
// state rather than replayed stale (hivecommons/hive#14138 stage 4).
var ErrRuntimeUnavailable = errors.New("crux runtime unavailable")

// ErrGitHubAppNotInstalled means a token mint was asked for while the GitHub
// App has no installation ID (installation_id 0). It is a static operator
// configuration condition, not a transient failure; pkg/github aliases it as
// ErrAppNotInstalled and the runtime classifies it separately.
var ErrGitHubAppNotInstalled = errors.New("github app has no installation for this hive's org")

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
	ClassGitHubAppNotInstalled    = "github_app_not_installed"
	ClassAgentStall               = "agent_stall"
	ClassAgentCrashLoop           = "agent_crash_loop"
	ClassCIPollNudge              = "ci_poll_nudge"
	// ClassRuntimeUnavailable is raised by the HOST, not the runtime: a
	// sidecar host whose runtime API stopped answering (hivecommons/hive#14138
	// stage 4). The runtime cannot report its own absence.
	ClassRuntimeUnavailable = "crux_runtime_unavailable"
)
