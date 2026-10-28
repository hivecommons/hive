package dashboard

import (
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
)

// Sandbox run health verdicts. A sandboxed agent (podman or Kubernetes Job
// runtime) has no tmux session or in-pod process between kicks, so the
// process-state rule the SPA applies to every other agent — anything but
// "running" is down — reads a perfectly healthy sandbox agent as "Stopped —
// see the agent's log" with a red dot and "session down". These verdicts
// derive instead from the last run and the cadence, which is all a Job
// agent has.
const (
	// SandboxHealthOK: the last run succeeded (or none has run yet) and the
	// next scheduled kick is still in the future.
	SandboxHealthOK = "ok"
	// SandboxHealthRunning: a kick is in flight (Job pending or running).
	SandboxHealthRunning = "running"
	// SandboxHealthFailed: the last run failed or timed out.
	SandboxHealthFailed = "failed"
	// SandboxHealthOverdue: a scheduled kick is due and the grace has passed
	// without a run starting — the cadence was missed.
	SandboxHealthOverdue = "overdue"
)

const (
	// sandboxOverdueGraceMin / Max bound the grace after a due occurrence
	// before a missed cadence is reported: half the period, clamped so a
	// 15-minute interval is not flagged by normal governor jitter and a
	// daily cron is not left red for twelve hours.
	sandboxOverdueGraceMin = 15 * time.Minute
	sandboxOverdueGraceMax = 2 * time.Hour
	sandboxOverdueGraceDiv = 2
)

// FrontendSandboxRun is the sandbox run record and health verdict on a
// sandboxed agent's status payload.
type FrontendSandboxRun struct {
	// Health is one of the SandboxHealth* verdicts.
	Health string `json:"health"`
	// Runtime is "podman" or "job" for the last run; empty before the first.
	Runtime string `json:"runtime,omitempty"`
	// LastRunAt / LastRunEndedAt bracket the last run (RFC 3339, UTC).
	LastRunAt      string `json:"lastRunAt,omitempty"`
	LastRunEndedAt string `json:"lastRunEndedAt,omitempty"`
	// DurationS is the last run's wall time in seconds.
	DurationS   int    `json:"durationS,omitempty"`
	Succeeded   bool   `json:"succeeded"`
	TimedOut    bool   `json:"timedOut,omitempty"`
	Error       string `json:"error,omitempty"`
	CommitCount int    `json:"commitCount,omitempty"`
	Branch      string `json:"branch,omitempty"`
	PRURL       string `json:"prURL,omitempty"`
	// PRWithheld is why commits from the last run were not published (agent
	// reported failure / non-zero exit); empty when nothing was withheld.
	PRWithheld string `json:"prWithheld,omitempty"`
	// LogID is the kick-log archive id of the last run's captured output
	// (/api/agents/{name}/kick-logs/{id}); empty when none was archived.
	LogID string `json:"logId,omitempty"`
	// Summary is the agent's report summary or the transcript tail.
	Summary string `json:"summary,omitempty"`
	// Tokens is the usage the run's transcript reported; 0 when it carried
	// none (the collector then has no session for this run either).
	Tokens int64  `json:"tokens,omitempty"`
	Model  string `json:"model,omitempty"`
	// DueAt is the scheduled occurrence that has been missed when Health is
	// overdue; empty otherwise.
	DueAt string `json:"dueAt,omitempty"`
}

// sandboxOverdueGrace is how long after a due occurrence a missed cadence is
// tolerated before the agent is reported overdue.
func sandboxOverdueGrace(cadence config.Cadence, now time.Time) time.Duration {
	period, ok := cadence.Period(now)
	if !ok || period <= 0 {
		return sandboxOverdueGraceMax
	}
	grace := period / sandboxOverdueGraceDiv
	if grace < sandboxOverdueGraceMin {
		grace = sandboxOverdueGraceMin
	}
	if grace > sandboxOverdueGraceMax {
		grace = sandboxOverdueGraceMax
	}
	return grace
}

// sandboxRunHealth derives a sandboxed agent's health from its process state
// (running = kick in flight), its last run record and its cadence:
//
//   - running while a kick is in flight;
//   - failed when the last run failed or timed out;
//   - overdue when the cadence has a due occurrence older than the grace and
//     nothing is running — the kick never started;
//   - ok otherwise, including before the first run on a hive whose cadence
//     has not come due yet.
//
// cadence is the agent's cadence for the CURRENT governor mode; a paused/off
// cadence or an on-demand agent has nothing to be overdue against.
func sandboxRunHealth(proc *crux.Process, cadence config.Cadence, onDemand bool, now time.Time) FrontendSandboxRun {
	out := FrontendSandboxRun{Health: SandboxHealthOK}
	if proc == nil {
		return out
	}
	if run := proc.SandboxLastRun; run != nil {
		out.Runtime = run.Runtime
		out.LastRunAt = formatOptionalTime(run.StartedAt)
		out.LastRunEndedAt = formatOptionalTime(run.EndedAt)
		out.DurationS = int(run.Duration() / time.Second)
		out.Succeeded = run.Succeeded
		out.TimedOut = run.TimedOut
		out.Error = redactTokens(run.Error)
		out.CommitCount = run.CommitCount
		out.Branch = run.Branch
		out.PRURL = run.PRURL
		out.PRWithheld = redactTokens(run.PRWithheld)
		out.LogID = run.LogID
		out.Summary = redactTokens(run.Summary)
		out.Tokens = run.Usage.Total()
		out.Model = run.Usage.Model
	}
	if proc.State == crux.StateRunning {
		out.Health = SandboxHealthRunning
		return out
	}
	if proc.SandboxLastRun != nil && !proc.SandboxLastRun.Succeeded {
		out.Health = SandboxHealthFailed
		return out
	}
	if onDemand || proc.Paused || proc.State == crux.StatePaused || cadence == "" || cadence.IsPaused() || cadence.IsContinuous() {
		return out
	}
	var lastKick time.Time
	if proc.LastKick != nil {
		lastKick = *proc.LastKick
	}
	due, ok := cadence.DueOccurrence(lastKick, now, 0)
	if !ok {
		return out
	}
	// A never-kicked interval agent is "due now" by definition; it is not
	// overdue until the first period has elapsed since the agent came up.
	if lastKick.IsZero() && proc.StartedAt != nil {
		if next, nok := cadence.NextAfter(*proc.StartedAt); nok {
			due = next
		}
	}
	if now.Sub(due) > sandboxOverdueGrace(cadence, now) {
		out.Health = SandboxHealthOverdue
		out.DueAt = formatOptionalTime(due)
	}
	return out
}
