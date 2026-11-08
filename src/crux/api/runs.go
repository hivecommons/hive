package api

import (
	"net/url"
	"strconv"
	"time"
)

// Contained runs and saved run results (hivecommons/crux#114, S2 and S7):
//
//	POST /v1/runs               run an untrusted command in a sandbox
//	GET  /v1/runs/{id}          the saved record, whatever the process state
//	POST /v1/runs/{id}/cancel   cancel; after completion returns the result
//	                            unchanged with already_finished: true
//	GET  /v1/runs               the caller's saved records (all, for the
//	                            operator), newest first, ?status= ?limit=
//	POST /v1/runs/{id}/restart  explicit, audited re-run of a finished run as
//	                            a new record linked to the old one; 409
//	                            conflict while the saved state is still live
//	GET  /v1/runs/{id}/history  every status the record passed through
//
// See src/docs/design/crux-work-services.md (S7).
const PathRuns = "/" + Version + "/runs"

// RunPath is the route of one run's saved record.
func RunPath(id string) string { return PathRuns + "/" + id }

// RunCancelPath is the route that cancels one run.
func RunCancelPath(id string) string { return RunPath(id) + "/cancel" }

// RunRestartPath is the route that re-runs one finished run (S7).
func RunRestartPath(id string) string { return RunPath(id) + "/restart" }

// RunHistoryPath is the route of one run's status history (S7).
func RunHistoryPath(id string) string { return RunPath(id) + "/history" }

// RunsListQuery is the query string of GET /v1/runs: status filters on one
// status of the closed set, limit caps the page (newest first); either may
// be empty or zero for no filter and the server default.
func RunsListQuery(status string, limit int) string {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(q) == 0 {
		return PathRuns
	}
	return PathRuns + "?" + q.Encode()
}

// The runs routes answer CodeUnsupported (501, shared with the checkout
// routes) when the request asks for something the configured backend cannot
// enforce (a network mode, a profile, a workspace form); the message names it
// and the run never starts with the limit dropped. It differs from
// CodeNotSupported, which is a whole route the server cannot serve (here: a
// server started without a contained-run backend answers POST /v1/runs with
// CodeNotSupported while still serving the saved records).

// Run statuses, the closed set every record reports.
const (
	RunAccepted    = "accepted"
	RunRunning     = "running"
	RunWaiting     = "waiting"
	RunCompleted   = "completed"
	RunFailed      = "failed"
	RunCancelled   = "cancelled"
	RunUnfinished  = "unfinished"
	RunUnsupported = "unsupported"
)

// Run profiles.
const (
	// RunProfileExperiment (the default) runs with network none, no
	// credentials and time and output limits.
	RunProfileExperiment = "experiment"
	// RunProfileAgent opens a step session (S6): no command, network
	// restricted by default, held waiting until cancelled; steps are
	// posted to RunStepsPath.
	RunProfileAgent = "agent"
)

// Network modes a run may request.
const (
	RunNetworkNone       = "none"
	RunNetworkRestricted = "restricted"
)

// ContainedRunWorktree is the ad-hoc workspace form: the service clones repo and
// pins commit (a full 40-hex SHA) under the run's directory.
type ContainedRunWorktree struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
}

// ContainedRunRequest is POST /v1/runs. Exactly one of Checkout, Worktree or
// WorkspacePath names the workspace. Checkout is an S1 checkout id: the run
// executes in that pinned worktree and must be made by the checkout's owner
// (or the operator); the checkout cannot be deleted while the run is live.
type ContainedRunRequest struct {
	Checkout      string                `json:"checkout,omitempty"`
	Worktree      *ContainedRunWorktree `json:"worktree,omitempty"`
	WorkspacePath string                `json:"workspace_path,omitempty"`
	Command       []string              `json:"command"`
	EnvAllowlist  []string              `json:"env_allowlist,omitempty"`
	// Network is RunNetworkNone (default) or RunNetworkRestricted.
	Network string `json:"network,omitempty"`
	// TimeoutMs bounds the command; zero uses the server default.
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
	// MaxOutputBytes caps each of stdout and stderr in the response; zero
	// uses the server default. The full output is kept as an artifact.
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"`
	// Profile is RunProfileExperiment (default) or RunProfileAgent.
	Profile string `json:"profile,omitempty"`
	// IdempotencyKey, when set, makes a repeated request return the
	// existing run instead of starting a second one (S7).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Detach returns as soon as the run is accepted (202) instead of
	// waiting for the result; poll GET /v1/runs/{id}.
	Detach bool `json:"detach,omitempty"`
}

// Run is the saved record of one contained run: the response of every runs
// route. Fields that are not yet known are absent; ExitCode in particular is
// present only when the command's exit is known, never guessed.
type ContainedRun struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Owner  string `json:"owner,omitempty"`
	// Reason explains a failed, unfinished or unsupported status.
	Reason     string     `json:"reason,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	Profile string   `json:"profile,omitempty"`
	Network string   `json:"network,omitempty"`
	Backend string   `json:"backend,omitempty"`
	Command []string `json:"command,omitempty"`
	// Checkout is the S1 checkout id the run executes in, when it was
	// requested that way.
	Checkout string `json:"checkout,omitempty"`
	Workdir  string `json:"workspace,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	// StdoutTruncatedAt and StderrTruncatedAt are the byte offsets at which
	// the response output was cut; absent when nothing was truncated. The
	// full, scrubbed output is the stdout.txt / stderr.txt artifact in the
	// run's directory.
	StdoutTruncatedAt *int64 `json:"stdout_truncated_at,omitempty"`
	StderrTruncatedAt *int64 `json:"stderr_truncated_at,omitempty"`
	// AlreadyFinished is set on a cancel response when the run had already
	// reached a terminal status; the record is returned unchanged.
	AlreadyFinished bool `json:"already_finished,omitempty"`
	// RestartedFrom is the finished run this one re-ran (S7); RestartedBy
	// is the run that re-ran this one. Both are saved, so the chain is
	// readable from either end after a restart of the serving process.
	RestartedFrom string `json:"restarted_from,omitempty"`
	RestartedBy   string `json:"restarted_by,omitempty"`
}

// RunRestartRequest is POST /v1/runs/{id}/restart (S7). The run re-runs
// with the request it was created from; only the bookkeeping is new.
type RunRestartRequest struct {
	// Reason is recorded with the new run and in the audit record.
	Reason string `json:"reason,omitempty"`
	// IdempotencyKey makes a repeated restart return the run the first one
	// started instead of a third run.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Detach returns the accepted record instead of waiting for the result.
	Detach bool `json:"detach,omitempty"`
}

// ContainedRunList is GET /v1/runs: the caller's saved records (every
// record for the operator), newest first, without the command output.
type ContainedRunList struct {
	Runs []ContainedRun `json:"runs"`
}

// RunTransition is one status the record passed through, in order.
type RunTransition struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
}

// RunHistory is GET /v1/runs/{id}/history: the saved record's statuses in
// the order they were written, the proof of what happened to the run.
type RunHistory struct {
	Run         string          `json:"run"`
	Transitions []RunTransition `json:"transitions"`
}
