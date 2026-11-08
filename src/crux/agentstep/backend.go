package agentstep

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/jsonextract"
	"github.com/hivecommons/hive/crux/sandbox"
)

// Spec is what a Backend turns into the command the sandbox runs. Paths
// are relative to the workspace, which is the sandbox's working directory.
type Spec struct {
	// PromptRel is the file holding this attempt's prompt.
	PromptRel string
	// AnswerRel is where a backend that can write its final message to a
	// file should put it; the adapter reads it back with Outcome.
	AnswerRel string
	Model     string
	Tools     []string
	TurnLimit int
	// Command is the process backend's argv; other backends refuse it.
	Command []string
}

// Outcome is what a Backend read out of one attempt. Answer is the
// candidate the schema is checked against; Status is empty when the
// attempt produced one, otherwise a terminal api.Step* status with Reason.
type Outcome struct {
	Answer json.RawMessage
	Usage  api.StepUsage
	Turns  int
	Status string
	Reason string
}

// Backend is the per-CLI adapter: it knows the flags that make the CLI
// read a prompt, pick a model, restrict its tools and stop after N turns,
// and how to read its answer and usage back. What it cannot enforce it
// refuses in Command with an *UnsupportedError; it never runs weakened.
type Backend interface {
	Name() string
	Command(spec Spec) ([]string, error)
	// Parse reads the attempt's result. readFile returns a workspace file
	// by relative path, for adapters that had the CLI write its answer to
	// spec.AnswerRel.
	Parse(spec Spec, res sandbox.Result, readFile func(rel string) ([]byte, error)) Outcome
}

// UnsupportedError names a request the backend cannot honour.
type UnsupportedError struct{ What string }

func (e *UnsupportedError) Error() string { return e.What }

// RequestError is a malformed step request.
type RequestError struct{ Msg string }

func (e *RequestError) Error() string { return e.Msg }

// Backends is the shipped adapter set by name.
var Backends = map[string]Backend{
	api.StepBackendClaude:  claudeBackend{},
	api.StepBackendCodex:   codexBackend{},
	api.StepBackendProcess: processBackend{},
}

// Lookup returns the adapter for name or a RequestError listing the known
// ones.
func Lookup(name string) (Backend, error) {
	if b, ok := Backends[name]; ok {
		return b, nil
	}
	return nil, &RequestError{fmt.Sprintf("unknown backend %q; one of %s", name, strings.Join(backendNames(), ", "))}
}

func backendNames() []string {
	return []string{api.StepBackendClaude, api.StepBackendCodex, api.StepBackendProcess}
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// shell wraps a command line so the prompt file is its stdin.
func shell(cmd, promptRel string) []string {
	return []string{"sh", "-lc", cmd + " < " + shellQuote(promptRel)}
}

// extractJSON finds the JSON value in model output: the whole text when it
// parses, else the text inside a ```json fence, else the first '{' to the
// last '}' (jsonextract.Object). Empty when nothing parses.
func extractJSON(text string) json.RawMessage {
	candidates := []string{strings.TrimSpace(text)}
	if i := strings.Index(text, "```"); i >= 0 {
		rest := text[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			candidates = append(candidates, strings.TrimSpace(rest[:j]))
		}
	}
	candidates = append(candidates, jsonextract.Object(text))
	for _, c := range candidates {
		if c != "" && json.Valid([]byte(c)) {
			return json.RawMessage(c)
		}
	}
	return nil
}

// --- process -----------------------------------------------------------------

// processBackend runs the caller's command with the prompt on stdin and
// takes stdout as the answer. It restricts nothing and enforces no turn
// limit, so a request for either is refused.
type processBackend struct{}

func (processBackend) Name() string { return api.StepBackendProcess }

func (processBackend) Command(spec Spec) ([]string, error) {
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		return nil, &RequestError{"backend process needs command"}
	}
	if len(spec.Tools) > 0 {
		return nil, &UnsupportedError{"backend process cannot restrict tools; it offers whatever the command offers"}
	}
	if spec.TurnLimit > 0 {
		return nil, &UnsupportedError{"backend process cannot enforce a turn limit"}
	}
	if spec.Model != "" {
		return nil, &UnsupportedError{"backend process takes no model; put it in the command"}
	}
	quoted := make([]string, len(spec.Command))
	for i, a := range spec.Command {
		quoted[i] = shellQuote(a)
	}
	return shell(strings.Join(quoted, " "), spec.PromptRel), nil
}

func (processBackend) Parse(_ Spec, res sandbox.Result, _ func(string) ([]byte, error)) Outcome {
	if res.ExitCode != 0 {
		return Outcome{Status: api.StepBackendError, Reason: fmt.Sprintf("command exited %d", res.ExitCode)}
	}
	return Outcome{Answer: extractJSON(res.Stdout), Turns: 1}
}

// --- claude ------------------------------------------------------------------

// claudeBackend is `claude -p --output-format json`: one result object on
// stdout with the final text in result, token usage in usage, the turn
// count in num_turns and, when a tool outside --allowedTools was asked
// for, permission_denials. An empty Tools list runs with
// --dangerously-skip-permissions (every tool, the existing sandbox-kick
// behaviour); a non-empty one passes --allowedTools and leaves permission
// prompting on, so anything else is denied in -p mode.
type claudeBackend struct{}

func (claudeBackend) Name() string { return api.StepBackendClaude }

func (claudeBackend) Command(spec Spec) ([]string, error) {
	if len(spec.Command) > 0 {
		return nil, &RequestError{"backend claude builds its own command; command is for backend process"}
	}
	cmd := "claude -p --output-format json"
	if spec.Model != "" {
		cmd += " --model " + shellQuote(spec.Model)
	}
	if len(spec.Tools) > 0 {
		cmd += " --allowedTools " + shellQuote(strings.Join(spec.Tools, ","))
	} else {
		cmd += " --dangerously-skip-permissions"
	}
	if spec.TurnLimit > 0 {
		cmd += fmt.Sprintf(" --max-turns %d", spec.TurnLimit)
	}
	return shell(cmd, spec.PromptRel), nil
}

// claudeResult is the subset of the -p JSON result crux reads.
type claudeResult struct {
	Type     string `json:"type"`
	Subtype  string `json:"subtype"`
	IsError  bool   `json:"is_error"`
	Result   string `json:"result"`
	NumTurns int    `json:"num_turns"`
	Usage    struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

func (claudeBackend) Parse(_ Spec, res sandbox.Result, _ func(string) ([]byte, error)) Outcome {
	raw := extractJSON(res.Stdout)
	var r claudeResult
	if raw == nil || json.Unmarshal(raw, &r) != nil || r.Type != "result" {
		return Outcome{Status: api.StepBackendError, Reason: fmt.Sprintf("claude exited %d without a result object", res.ExitCode)}
	}
	out := Outcome{Turns: r.NumTurns}
	out.Usage.InputTokens, out.Usage.OutputTokens = r.Usage.InputTokens, r.Usage.OutputTokens
	switch {
	case len(r.PermissionDenials) > 0:
		names := make([]string, len(r.PermissionDenials))
		for i, d := range r.PermissionDenials {
			names[i] = d.ToolName
		}
		out.Status, out.Reason = api.StepToolDenied, "tool outside the allowlist requested: "+strings.Join(names, ", ")
	case r.Subtype == "error_max_turns":
		out.Status, out.Reason = api.StepTurnLimitExceeded, fmt.Sprintf("stopped after %d turns without an answer", r.NumTurns)
	case r.IsError || (r.Subtype != "" && r.Subtype != "success"):
		out.Status, out.Reason = api.StepBackendError, "claude reported "+r.Subtype
	default:
		out.Answer = extractJSON(r.Result)
	}
	return out
}

// --- codex -------------------------------------------------------------------

// codexBackend is `codex exec --json`: a JSONL event stream on stdout and,
// with --output-last-message, the final message in a file crux names
// under the workspace. Usage comes from the turn.completed event. codex
// exec is one turn by construction, so a TurnLimit above 1 is refused, and
// it has no per-tool allowlist, so Tools is refused. The CLI's own sandbox
// and approvals are bypassed: crux's sandbox is the containment.
type codexBackend struct{}

func (codexBackend) Name() string { return api.StepBackendCodex }

func (codexBackend) Command(spec Spec) ([]string, error) {
	if len(spec.Command) > 0 {
		return nil, &RequestError{"backend codex builds its own command; command is for backend process"}
	}
	if len(spec.Tools) > 0 {
		return nil, &UnsupportedError{"backend codex cannot restrict tools to an allowlist"}
	}
	if spec.TurnLimit > 1 {
		return nil, &UnsupportedError{"backend codex runs exactly one turn; turn_limit above 1 cannot be enforced"}
	}
	cmd := "codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox"
	if spec.Model != "" {
		cmd += " --model " + shellQuote(spec.Model)
	}
	cmd += " --output-last-message " + shellQuote(spec.AnswerRel) + " -"
	return shell(cmd, spec.PromptRel), nil
}

// codexEvent is the subset of the exec --json events crux reads.
type codexEvent struct {
	Type  string `json:"type"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Message string `json:"message"`
}

func (codexBackend) Parse(spec Spec, res sandbox.Result, readFile func(string) ([]byte, error)) Outcome {
	out := Outcome{Turns: 1}
	var last, failure string
	for _, line := range strings.Split(res.Stdout, "\n") {
		var ev codexEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Usage != nil {
			out.Usage.InputTokens += ev.Usage.InputTokens
			out.Usage.OutputTokens += ev.Usage.OutputTokens
		}
		if ev.Item != nil && ev.Item.Type == "agent_message" {
			last = ev.Item.Text
		}
		if ev.Type == "error" || ev.Type == "turn.failed" {
			failure = ev.Message
		}
	}
	if res.ExitCode != 0 {
		if failure == "" {
			failure = fmt.Sprintf("codex exited %d", res.ExitCode)
		}
		out.Status, out.Reason = api.StepBackendError, failure
		return out
	}
	if readFile != nil {
		if data, err := readFile(spec.AnswerRel); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			last = string(data)
		}
	}
	out.Answer = extractJSON(last)
	return out
}

// ErrNoAnswer is the reason an attempt that ran clean produced nothing a
// schema could be checked against.
var ErrNoAnswer = errors.New("the backend returned no JSON value")
