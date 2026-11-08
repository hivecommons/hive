package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Schema-checked agent steps (hivecommons/crux#114 S6): run one agent turn
// in the workspace of an agent-profile contained run and get the answer
// back validated against a JSON shape.
//
//	POST /v1/runs/{id}/steps          run a step in the session's workspace
//	GET  /v1/runs/{id}/steps          every step of the session, oldest first
//	GET  /v1/runs/{id}/steps/{n}      one step's saved record
//
// A session is a run created with profile "agent" and no command: it
// provisions its workspace, reports "waiting" and stays open until it is
// cancelled, which is how a driver closes it. The routes admit the run's
// owner and the operator, like the run itself. Each step is appended to the
// run's command log (S4) and announced with a "step" turn signal (S5).

// RunStepsPath is the route of one run's steps.
func RunStepsPath(id string) string { return RunPath(id) + "/steps" }

// RunStepPath is the route of one step's saved record.
func RunStepPath(id string, n int) string { return RunStepsPath(id) + "/" + strconv.Itoa(n) }

// Step statuses, the closed set every record reports. Every terminal status
// carries the attempts made, their usage and the transcript path, so a
// driver budgets and debugs from a failure as well as a success.
const (
	// StepRunning: the step is in flight.
	StepRunning = "running"
	// StepSucceeded: the answer matched the schema.
	StepSucceeded = "succeeded"
	// StepSchemaMismatch: every attempt's answer failed validation; the
	// violations of the last one are on the record, no partial answer is.
	StepSchemaMismatch = "schema_mismatch"
	// StepTimeout: the backend did not finish within timeout_ms.
	StepTimeout = "timeout"
	// StepBackendError: the backend exited non-zero or its output was not
	// what the adapter expects.
	StepBackendError = "backend_error"
	// StepToolDenied: the backend reported a tool outside the allowlist
	// was requested and refused.
	StepToolDenied = "tool_denied"
	// StepTurnLimitExceeded: the backend stopped at the turn limit before
	// answering.
	StepTurnLimitExceeded = "turn_limit_exceeded"
	// StepCancelled: the session was cancelled while the step ran.
	StepCancelled = "cancelled"
	// StepUnsupported: the request asked for something the backend cannot
	// enforce (a tool allowlist, a turn limit); the step never ran.
	StepUnsupported = "unsupported"
	// StepUnfinished: the serving process stopped while the step ran, so
	// its outcome is not knowable (S7 reconciliation); never a guess.
	StepUnfinished = "unfinished"
)

// Step backends shipped with crux. Process is the generic adapter: the
// caller names the command, crux feeds the prompt on stdin and reads the
// answer from stdout; it enforces neither tools nor turns and says so.
const (
	StepBackendClaude  = "claude"
	StepBackendCodex   = "codex"
	StepBackendProcess = "process"
)

// CodeStepBusy is a step posted while the session already runs one (409).
const CodeStepBusy = "step_busy"

// CodeNotSession is a step posted to a run that is not an agent-profile
// session, or whose session has ended (409).
const CodeNotSession = "not_session"

// SignalStep is the runner's turn signal for a finished step; Data is
// SignalStepData and the message the step's reason.
const SignalStep = "step"

// SignalStepData is the data of a step signal.
type SignalStepData struct {
	Step     int    `json:"step"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
}

// StepRequest is POST /v1/runs/{id}/steps.
type StepRequest struct {
	// Prompt is the step's instruction; PromptPath names a file in the
	// workspace holding it instead (for large prompts). Exactly one.
	Prompt     string `json:"prompt,omitempty"`
	PromptPath string `json:"prompt_path,omitempty"`
	// Backend is StepBackendClaude, StepBackendCodex or StepBackendProcess.
	Backend string `json:"backend"`
	// Model, when set, is passed to the backend; empty is its default.
	Model string `json:"model,omitempty"`
	// Command is the process backend's argv, run in the workspace with the
	// prompt on stdin; other backends refuse it.
	Command []string `json:"command,omitempty"`
	// OutputSchema is the JSON Schema the answer must match. Required.
	OutputSchema json.RawMessage `json:"output_schema"`
	// Tools is the allowlist of tools offered to the agent; empty is the
	// backend's default set. A backend that cannot restrict its tools
	// refuses a non-empty list (StepUnsupported).
	Tools []string `json:"tools,omitempty"`
	// TurnLimit stops the backend after this many turns; zero is the
	// backend default. A backend that cannot enforce it refuses it.
	TurnLimit int `json:"turn_limit,omitempty"`
	// TimeoutMs bounds each attempt; zero uses the session's timeout.
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
	// RetryOnMismatch is how many corrective attempts follow an answer that
	// fails the schema; zero means one attempt only.
	RetryOnMismatch int `json:"retry_on_mismatch,omitempty"`
}

// StepUsage is the token usage a backend reported, summed over attempts.
type StepUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// SchemaViolation is one way an answer missed the schema.
type SchemaViolation struct {
	// Path is the JSON pointer-ish location ("$", "$.items[2].name").
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Step is the saved record of one agent step: the response of every steps
// route. Answer is present only on StepSucceeded; Violations only on
// StepSchemaMismatch.
type Step struct {
	Run    string `json:"run"`
	N      int    `json:"n"`
	Status string `json:"status"`
	// Reason explains any status other than succeeded.
	Reason     string     `json:"reason,omitempty"`
	Backend    string     `json:"backend"`
	Model      string     `json:"model,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Attempts is how many times the backend ran; each is one line of
	// the transcript.
	Attempts int             `json:"attempts"`
	Answer   json.RawMessage `json:"answer,omitempty"`
	Usage    StepUsage       `json:"usage"`
	// TranscriptPath is the attempts' raw output, relative to the run's
	// directory.
	TranscriptPath string            `json:"transcript_path,omitempty"`
	Violations     []SchemaViolation `json:"violations,omitempty"`
}

// StepList is GET /v1/runs/{id}/steps.
type StepList struct {
	Run   string `json:"run"`
	Steps []Step `json:"steps"`
}

// RunStep is POST /v1/runs/{id}/steps: the finished step.
func (c *Client) RunStep(ctx context.Context, run string, req StepRequest) (Step, error) {
	var out Step
	err := c.Do(ctx, http.MethodPost, RunStepsPath(run), req, &out)
	return out, err
}

// ListSteps is GET /v1/runs/{id}/steps.
func (c *Client) ListSteps(ctx context.Context, run string) (StepList, error) {
	var out StepList
	err := c.Do(ctx, http.MethodGet, RunStepsPath(run), nil, &out)
	return out, err
}

// GetStep is GET /v1/runs/{id}/steps/{n}.
func (c *Client) GetStep(ctx context.Context, run string, n int) (Step, error) {
	var out Step
	err := c.Do(ctx, http.MethodGet, RunStepPath(run, n), nil, &out)
	return out, err
}
