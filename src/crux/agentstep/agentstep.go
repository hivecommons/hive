// Package agentstep is the schema-checked agent step service of the Crux
// work services (S6 in hivecommons/crux#114): run one agent turn in a
// contained workspace — a prompt, a backend CLI, a model, an allowlist of
// tools, a turn limit — and hand back the answer already validated against
// the caller's JSON Schema, with the token usage, a transcript of every
// attempt and a typed status. The loop is agent.SandboxExecutor.Run
// generalised: write the prompt into the workspace, build the backend's
// command (Backend carries the per-CLI flags), run it through
// sandbox.Launcher, read the answer back, check it, and on a mismatch retry
// with a corrective prompt.
//
// Steps belong to an agent-profile contained run (runs.Service, S2), which
// supplies the workspace, the launcher, the command log (S4) and the signal
// book (S5); this package never looks those up itself. Records live under
// the run's directory so a caller recovers them by run id and step number
// after a disconnect (S7).
package agentstep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/crux/sandbox"
)

// Limits.
const (
	// MaxRetries bounds retry_on_mismatch.
	MaxRetries = 5
	// MaxPromptBytes bounds an inline prompt.
	MaxPromptBytes = 1 << 20
	// MaxPromptFileBytes bounds the workspace files the host reads whole: a
	// prompt_path file (the run owner's) and a backend's answer file (the
	// sandboxed agent's). Everything else a caller or an agent can shape
	// is bounded; without this the two files were the one way to make the
	// server allocate without limit.
	MaxPromptFileBytes = 16 << 20
	// workspaceDir is where prompts and backend answer files go inside the
	// workspace, next to the agent sandbox's .hive.
	workspaceDir = ".crux/steps"
	// launchNamePrefix names the container after the run, step and attempt
	// so a teardown can find it.
	launchNamePrefix = "crux-step-"
	// teardownTimeout bounds the container removal after a cancel or a
	// timeout.
	teardownTimeout = 30 * time.Second
)

// Host is what the session hands the service for one step: where to run,
// how to launch, where to record. The runs service builds it from the
// agent-profile run the step is posted to.
type Host struct {
	Run       string
	RunDir    string
	Workspace string
	Image     string
	Network   string
	// Env and EnvAllowlist are the session's, already filtered of
	// credentials by sandbox.SanitizedEnv.
	Env          map[string]string
	EnvAllowlist []string
	Launcher     sandbox.Launcher
	// Teardown, when set, removes the launch by name after a cancel or a
	// timeout (runs.Backend.Teardown).
	Teardown func(ctx context.Context, name string) error
	// Timeout is the per-attempt default; MaxTimeout caps a request's.
	Timeout, MaxTimeout time.Duration
	// CmdLog, when set, records each attempt's command (S4).
	CmdLog cmdlog.Recorder
	// Signal, when set, appends a runner signal to the run (S5): the
	// service emits one step signal per finished step.
	Signal func(kind, message string, data json.RawMessage)
	// Scrub redacts output before it is persisted; nil uses logscrub.
	Scrub func(string) string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

func (h Host) scrub(s string) string {
	if h.Scrub != nil {
		return h.Scrub(s)
	}
	return logscrub.ScrubString(s)
}

func (h Host) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// plan is a validated request.
type plan struct {
	backend Backend
	schema  *Schema
	rawSch  json.RawMessage
	prompt  string
	spec    Spec
	timeout time.Duration
	retries int
}

// Run executes one step for host and returns its saved record. A request
// the backend cannot honour is saved with status unsupported and returned
// with an *UnsupportedError, so the refusal is on the record as the RFC
// asks; a malformed request is a *RequestError with no record.
func Run(ctx context.Context, host Host, req api.StepRequest) (api.Step, error) {
	p, err := validate(host, req)
	if err != nil {
		return api.Step{}, err
	}
	store := Store{RunDir: host.RunDir}
	// Build the command once with placeholder paths to learn whether the
	// backend can honour the request before anything is recorded as run.
	probe := p.spec
	probe.PromptRel, probe.AnswerRel = "prompt", "answer"
	if _, cmdErr := p.backend.Command(probe); cmdErr != nil {
		var unsupported *UnsupportedError
		if !errors.As(cmdErr, &unsupported) {
			return api.Step{}, cmdErr
		}
		step, err := store.Create(api.Step{Run: host.Run, Backend: req.Backend, Model: req.Model, CreatedAt: host.now()})
		if err != nil {
			return api.Step{}, err
		}
		step = finish(host, store, step, api.StepUnsupported, unsupported.What, nil)
		return step, unsupported
	}
	step, err := store.Create(api.Step{Run: host.Run, Backend: req.Backend, Model: req.Model, Status: api.StepRunning, CreatedAt: host.now()})
	if err != nil {
		return api.Step{}, err
	}
	step.TranscriptPath = store.TranscriptRel(step.N)
	return execute(ctx, host, store, step, p), nil
}

func validate(host Host, req api.StepRequest) (plan, error) {
	var p plan
	backend, err := Lookup(strings.TrimSpace(req.Backend))
	if err != nil {
		return p, err
	}
	p.backend = backend
	switch {
	case req.Prompt != "" && req.PromptPath != "":
		return p, &RequestError{"prompt and prompt_path are mutually exclusive"}
	case req.Prompt != "":
		if len(req.Prompt) > MaxPromptBytes {
			return p, &RequestError{fmt.Sprintf("prompt is %d bytes; the bound is %d (use prompt_path)", len(req.Prompt), MaxPromptBytes)}
		}
		p.prompt = req.Prompt
	case req.PromptPath != "":
		rel := filepath.Clean(req.PromptPath)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return p, &RequestError{"prompt_path must be relative to the workspace and stay inside it"}
		}
		root, err := os.OpenRoot(host.Workspace)
		if err != nil {
			return p, &RequestError{fmt.Sprintf("prompt_path %q: %v", req.PromptPath, err)}
		}
		data, err := readBounded(root, rel, MaxPromptFileBytes)
		root.Close()
		if err != nil {
			if errors.Is(err, errFileTooLarge) {
				return p, &RequestError{fmt.Sprintf("prompt_path %q is over %d bytes", req.PromptPath, MaxPromptFileBytes)}
			}
			// os.Root's escape error is unexported; a symlink leaving the
			// workspace is reported like a lexical escape.
			if strings.Contains(err.Error(), "escapes from parent") {
				return p, &RequestError{"prompt_path must be relative to the workspace and stay inside it"}
			}
			return p, &RequestError{fmt.Sprintf("prompt_path %q: %v", req.PromptPath, err)}
		}
		p.prompt = string(data)
	default:
		return p, &RequestError{"one of prompt or prompt_path is required"}
	}
	if strings.TrimSpace(p.prompt) == "" {
		return p, &RequestError{"prompt is empty"}
	}
	p.rawSch = req.OutputSchema
	if p.schema, err = ParseSchema(req.OutputSchema); err != nil {
		return p, &RequestError{strings.TrimPrefix(err.Error(), "agentstep: invalid output_schema: ")}
	}
	if req.RetryOnMismatch < 0 || req.RetryOnMismatch > MaxRetries {
		return p, &RequestError{fmt.Sprintf("retry_on_mismatch must be between 0 and %d", MaxRetries)}
	}
	p.retries = req.RetryOnMismatch
	if req.TurnLimit < 0 {
		return p, &RequestError{"turn_limit must not be negative"}
	}
	p.timeout = host.Timeout
	if req.TimeoutMs > 0 {
		d := time.Duration(req.TimeoutMs) * time.Millisecond
		if host.MaxTimeout > 0 && d > host.MaxTimeout {
			return p, &RequestError{fmt.Sprintf("timeout_ms %d exceeds this server's cap of %d", req.TimeoutMs, host.MaxTimeout.Milliseconds())}
		}
		p.timeout = d
	}
	for _, t := range req.Tools {
		if strings.TrimSpace(t) == "" {
			return p, &RequestError{"tools must not contain an empty name"}
		}
	}
	p.spec = Spec{Model: req.Model, Tools: req.Tools, TurnLimit: req.TurnLimit, Command: req.Command}
	return p, nil
}

// attempt is one transcript line: what ran and what came back, scrubbed.
type attempt struct {
	Attempt    int                   `json:"attempt"`
	At         time.Time             `json:"at"`
	Command    []string              `json:"command"`
	ExitCode   *int                  `json:"exit_code,omitempty"`
	Stdout     string                `json:"stdout,omitempty"`
	Stderr     string                `json:"stderr,omitempty"`
	Status     string                `json:"status,omitempty"`
	Reason     string                `json:"reason,omitempty"`
	Violations []api.SchemaViolation `json:"violations,omitempty"`
	Usage      api.StepUsage         `json:"usage"`
}

// execute runs the attempts to a terminal status. Every exit path writes
// the record and then the run's step signal.
func execute(ctx context.Context, host Host, store Store, step api.Step, p plan) api.Step {
	prompt := p.prompt
	var violations []api.SchemaViolation
	for n := 1; n <= 1+p.retries; n++ {
		step.Attempts = n
		a, outcome := runAttempt(ctx, host, store, step, p, n, prompt)
		step.Usage.InputTokens += outcome.Usage.InputTokens
		step.Usage.OutputTokens += outcome.Usage.OutputTokens
		if outcome.Status != "" {
			a.Status, a.Reason = outcome.Status, outcome.Reason
			_ = store.AppendTranscript(step.N, a)
			return finish(host, store, step, outcome.Status, outcome.Reason, nil)
		}
		if outcome.Answer == nil {
			violations = []api.SchemaViolation{{Path: "$", Message: ErrNoAnswer.Error()}}
		} else {
			violations = p.schema.Validate(outcome.Answer)
		}
		if len(violations) == 0 {
			a.Status = api.StepSucceeded
			_ = store.AppendTranscript(step.N, a)
			step.Answer = outcome.Answer
			return finish(host, store, step, api.StepSucceeded, "", nil)
		}
		a.Status, a.Violations = api.StepSchemaMismatch, violations
		_ = store.AppendTranscript(step.N, a)
		prompt = p.prompt + "\n\n" + CorrectivePrompt(p.rawSch, violations, p.retries-n)
	}
	return finish(host, store, step, api.StepSchemaMismatch, fmt.Sprintf("the answer did not match output_schema after %d attempt(s)", step.Attempts), violations)
}

// runAttempt launches the backend once and reads its outcome.
func runAttempt(ctx context.Context, host Host, store Store, step api.Step, p plan, n int, prompt string) (attempt, Outcome) {
	a := attempt{Attempt: n, At: host.now()}
	rel := filepath.ToSlash(filepath.Join(workspaceDir, fmt.Sprint(step.N)))
	spec := p.spec
	spec.PromptRel = rel + fmt.Sprintf("/prompt-%d.md", n)
	spec.AnswerRel = rel + fmt.Sprintf("/answer-%d.txt", n)
	// Every host-side workspace access goes through root so a symlink in the
	// workspace (provisioned, or planted by the sandboxed agent) cannot
	// redirect it outside (#17293).
	root, err := os.OpenRoot(host.Workspace)
	if err != nil {
		return a, Outcome{Status: api.StepBackendError, Reason: "writing the prompt: " + host.scrub(err.Error())}
	}
	defer root.Close()
	if err := root.MkdirAll(rel, 0o770); err != nil {
		return a, Outcome{Status: api.StepBackendError, Reason: "writing the prompt: " + host.scrub(err.Error())}
	}
	if err := root.WriteFile(filepath.FromSlash(spec.PromptRel), []byte(prompt), 0o660); err != nil {
		return a, Outcome{Status: api.StepBackendError, Reason: "writing the prompt: " + host.scrub(err.Error())}
	}
	cmd, err := p.backend.Command(spec)
	if err != nil {
		return a, Outcome{Status: api.StepUnsupported, Reason: err.Error()}
	}
	a.Command = cmd
	var seq int64
	if host.CmdLog != nil {
		if seq, err = host.CmdLog.Started(cmdlog.Start{Command: cmd, Cwd: sandbox.DefaultWorkspaceMount, Capture: api.CommandCaptureRunner}); err != nil {
			seq = 0
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	name := fmt.Sprintf("%s%s-%d-%d", launchNamePrefix, host.Run, step.N, n)
	res, runErr := host.Launcher.Run(runCtx, sandbox.LaunchSpec{
		Image: host.Image, Name: name, Workspace: host.Workspace, Command: cmd,
		Env: host.Env, EnvAllowlist: host.EnvAllowlist, NetworkMode: host.Network,
	})
	cancelled := runErr != nil && ctx.Err() != nil
	timedOut := runErr != nil && !cancelled && errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if (cancelled || timedOut) && host.Teardown != nil {
		tctx, tcancel := context.WithTimeout(context.Background(), teardownTimeout)
		_ = host.Teardown(tctx, name)
		tcancel()
	}
	a.Stdout, a.Stderr = host.scrub(res.Stdout), host.scrub(res.Stderr)
	launched := runErr == nil || res != (sandbox.Result{})
	if launched && res.ExitCode != -1 {
		code := res.ExitCode
		a.ExitCode = &code
	}
	var out Outcome
	switch {
	case cancelled:
		out = Outcome{Status: api.StepCancelled, Reason: "the session was cancelled"}
	case timedOut:
		out = Outcome{Status: api.StepTimeout, Reason: fmt.Sprintf("the backend did not finish within %s", p.timeout)}
	case !launched:
		out = Outcome{Status: api.StepBackendError, Reason: host.scrub(runErr.Error())}
	default:
		out = p.backend.Parse(spec, res, func(rel string) ([]byte, error) {
			return readBounded(root, filepath.FromSlash(rel), MaxPromptFileBytes)
		})
		out.Reason = host.scrub(out.Reason)
	}
	a.Usage = out.Usage
	if host.CmdLog != nil && seq > 0 {
		end := cmdlog.End{ExitCode: a.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr, Reason: out.Reason}
		switch out.Status {
		case api.StepCancelled:
			end.Status = api.CommandCancelled
		case api.StepTimeout, api.StepBackendError:
			end.Status = api.CommandFailed
		}
		_ = host.CmdLog.Ended(seq, end)
	}
	return a, out
}

// errFileTooLarge is readBounded's refusal of a file over its bound.
var errFileTooLarge = errors.New("agentstep: file exceeds the size bound")

// readBounded reads rel through root, refusing with errFileTooLarge once
// more than max bytes are present, so a file in the workspace can never
// make the host allocate without limit.
func readBounded(root *os.Root, rel string, max int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errFileTooLarge
	}
	return data, nil
}

// finish writes the terminal record and announces it on the run.
func finish(host Host, store Store, step api.Step, status, reason string, violations []api.SchemaViolation) api.Step {
	now := host.now()
	step.Status, step.Reason, step.FinishedAt, step.Violations = status, reason, &now, violations
	if status != api.StepSucceeded {
		step.Answer = nil
	}
	_ = store.Write(step)
	if host.Signal != nil {
		data, err := json.Marshal(api.SignalStepData{Step: step.N, Status: status, Attempts: step.Attempts})
		if err == nil {
			host.Signal(api.SignalStep, reason, data)
		}
	}
	return step
}
