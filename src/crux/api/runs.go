package api

import "time"

// Contained runs and saved run results (hivecommons/crux#114, S2 and S7):
//
//	POST /v1/runs               run an untrusted command in a sandbox
//	GET  /v1/runs/{id}          the saved record, whatever the process state
//	POST /v1/runs/{id}/cancel   cancel; after completion returns the result
//	                            unchanged with already_finished: true
//
// Routes beyond these (restart, listing) are deferred; see
// src/docs/design/crux-work-services.md.
const PathRuns = "/" + Version + "/runs"

// RunPath is the route of one run's saved record.
func RunPath(id string) string { return PathRuns + "/" + id }

// RunCancelPath is the route that cancels one run.
func RunCancelPath(id string) string { return RunPath(id) + "/cancel" }

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
	// RunProfileAgent is S6's execution half and is not yet accepted
	// (CodeUnsupported).
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
// WorkspacePath names the workspace. Checkout (an S1 id) is accepted by the
// wire shape but answers CodeUnsupported until the checkout service lands.
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

	Profile  string   `json:"profile,omitempty"`
	Network  string   `json:"network,omitempty"`
	Backend  string   `json:"backend,omitempty"`
	Command  []string `json:"command,omitempty"`
	Workdir  string   `json:"workspace,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	TimedOut bool     `json:"timed_out,omitempty"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
	// StdoutTruncatedAt and StderrTruncatedAt are the byte offsets at which
	// the response output was cut; absent when nothing was truncated. The
	// full, scrubbed output is the stdout.txt / stderr.txt artifact in the
	// run's directory.
	StdoutTruncatedAt *int64 `json:"stdout_truncated_at,omitempty"`
	StderrTruncatedAt *int64 `json:"stderr_truncated_at,omitempty"`
	// AlreadyFinished is set on a cancel response when the run had already
	// reached a terminal status; the record is returned unchanged.
	AlreadyFinished bool `json:"already_finished,omitempty"`
}
