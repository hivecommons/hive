package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// stepUsage is the `crux step` synopsis.
const stepUsage = `usage: crux --server ADDR step run <run> --backend B (--prompt P | --prompt-file F) --schema FILE
                                  [--model M] [--tool T]... [--turn-limit N] [--retry N]
                                  [--timeout-ms N] [--json] [-- CMD ARGS...]
       crux --server ADDR step list [--json] <run>
       crux --server ADDR step show [--json] <run> <n>

run posts one agent step to an open agent-profile session (crux runs
start --profile agent) and waits for it: the backend (claude, codex or
process) reads the prompt inside the session's sandbox and its answer is
checked against the JSON Schema in --schema. --retry N re-asks up to N
times with the violations when the answer does not match. --prompt-file
is a path relative to the session's workspace. The process backend runs
the command after -- with the prompt on stdin and its stdout as the
answer. list prints one line per step; show prints one step with its
answer.`

// remoteStep is `crux --server … step`: the agent-step family (RFC
// crux#114 S6). Steps run on crux serve; there is no in-process form.
func (e *Env) remoteStep(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, stepUsage)
		return ExitUsage
	}
	switch args[0] {
	case "run":
		return e.remoteStepRun(ctx, c, args[1:])
	case "list":
		return e.remoteStepList(ctx, c, args[1:])
	case "show":
		return e.remoteStepShow(ctx, c, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(e.Stderr, stepUsage)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: step: unknown subcommand %q\n%s\n", args[0], stepUsage)
		return ExitUsage
	}
}

func (e *Env) remoteStepRun(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("step run")
	var req api.StepRequest
	var tools stringList
	var schemaFile string
	fs.StringVar(&req.Backend, "backend", "", "claude, codex or process")
	fs.StringVar(&req.Model, "model", "", "model the backend should use")
	fs.StringVar(&req.Prompt, "prompt", "", "the prompt text")
	fs.StringVar(&req.PromptPath, "prompt-file", "", "prompt file, relative to the session's workspace")
	fs.StringVar(&schemaFile, "schema", "", "JSON Schema file the answer must match (required)")
	fs.Var(&tools, "tool", "tool the backend may use (repeatable; none means no tools)")
	fs.IntVar(&req.TurnLimit, "turn-limit", 0, "most agent turns per attempt (0: backend default)")
	fs.IntVar(&req.RetryOnMismatch, "retry", 0, "re-ask up to N times when the answer misses the schema")
	fs.Int64Var(&req.TimeoutMs, "timeout-ms", 0, "per-attempt wall clock in ms (server default when unset)")
	asJSON := fs.Bool("json", false, "print the step as JSON")
	var run string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		run, args = args[0], args[1:]
	}
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	rest := fs.Args()
	if run == "" && len(rest) > 0 {
		run, rest = rest[0], rest[1:]
	}
	// The flag package only consumes "--" while it is still parsing flags;
	// after a bare <run> it is ours to drop.
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	req.Command = rest
	req.Tools = tools
	if run == "" || req.Backend == "" || schemaFile == "" {
		fmt.Fprintln(e.Stderr, stepUsage)
		return ExitUsage
	}
	schema, err := os.ReadFile(schemaFile)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crux: step run: --schema: %v\n", err)
		return ExitUsage
	}
	if !json.Valid(schema) {
		fmt.Fprintf(e.Stderr, "crux: step run: --schema %s is not valid JSON\n", schemaFile)
		return ExitUsage
	}
	req.OutputSchema = json.RawMessage(schema)
	step, err := c.RunStep(ctx, run, req)
	if err != nil {
		return e.stepErr("run", run, err)
	}
	if *asJSON {
		return e.printJSON(step)
	}
	e.printStep(step)
	if step.Status != api.StepSucceeded {
		return ExitError
	}
	return ExitOK
}

func (e *Env) remoteStepList(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("step list")
	asJSON := fs.Bool("json", false, "print the steps as JSON")
	run, code, ok := parseWithID(fs, args)
	if !ok {
		return code
	}
	if run == "" {
		fmt.Fprintln(e.Stderr, stepUsage)
		return ExitUsage
	}
	list, err := c.ListSteps(ctx, run)
	if err != nil {
		return e.stepErr("list", run, err)
	}
	if *asJSON {
		return e.printJSON(list.Steps)
	}
	for _, step := range list.Steps {
		fmt.Fprintln(e.Stdout, stepLine(step))
	}
	return ExitOK
}

func (e *Env) remoteStepShow(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("step show")
	asJSON := fs.Bool("json", false, "print the step as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(e.Stderr, stepUsage)
		return ExitUsage
	}
	run := fs.Arg(0)
	n, err := strconv.Atoi(fs.Arg(1))
	if err != nil || n <= 0 {
		fmt.Fprintln(e.Stderr, "crux: step show: <n> must be a positive integer")
		return ExitUsage
	}
	step, err := c.GetStep(ctx, run, n)
	if err != nil {
		return e.stepErr("show", run, err)
	}
	if *asJSON {
		return e.printJSON(step)
	}
	e.printStep(step)
	return ExitOK
}

// stepLine is the one-line form list prints.
func stepLine(step api.Step) string {
	line := fmt.Sprintf("%d %s %s %s", step.N, step.CreatedAt.UTC().Format(timeLayout), step.Backend, step.Status)
	if step.Attempts > 1 {
		line += fmt.Sprintf(" (%d attempts)", step.Attempts)
	}
	if step.Reason != "" {
		line += "  " + step.Reason
	}
	return line
}

// printStep is the multi-line form run and show print.
func (e *Env) printStep(step api.Step) {
	fmt.Fprintf(e.Stdout, "step:     %d\n", step.N)
	fmt.Fprintf(e.Stdout, "run:      %s\n", step.Run)
	fmt.Fprintf(e.Stdout, "status:   %s\n", step.Status)
	if step.Reason != "" {
		fmt.Fprintf(e.Stdout, "reason:   %s\n", step.Reason)
	}
	backend := step.Backend
	if step.Model != "" {
		backend += " (" + step.Model + ")"
	}
	fmt.Fprintf(e.Stdout, "backend:  %s\n", backend)
	fmt.Fprintf(e.Stdout, "attempts: %d\n", step.Attempts)
	fmt.Fprintf(e.Stdout, "created:  %s\n", step.CreatedAt.UTC().Format(timeLayout))
	if step.FinishedAt != nil {
		fmt.Fprintf(e.Stdout, "finished: %s\n", step.FinishedAt.UTC().Format(timeLayout))
	}
	if step.Usage.InputTokens > 0 || step.Usage.OutputTokens > 0 {
		fmt.Fprintf(e.Stdout, "tokens:   %d in, %d out\n", step.Usage.InputTokens, step.Usage.OutputTokens)
	}
	for _, v := range step.Violations {
		fmt.Fprintf(e.Stdout, "violation: %s: %s\n", v.Path, v.Message)
	}
	if len(step.Answer) > 0 {
		fmt.Fprintf(e.Stdout, "answer:   %s\n", string(step.Answer))
	}
}

// stepErr prints a failure; refusals the caller can fix are usage
// errors, everything else a runtime failure.
func (e *Env) stepErr(verb, run string, err error) int {
	fmt.Fprintf(e.Stderr, "crux: step %s %s: %v\n", verb, run, err)
	var se *api.StatusError
	if errors.As(err, &se) {
		switch se.Body.Code {
		case api.CodeBadRequest, api.CodeForbidden, api.CodeNotSession, api.CodeStepBusy:
			return ExitUsage
		}
	}
	if api.IsUnauthorized(err) {
		fmt.Fprintf(e.Stderr, "crux: the server wants a bearer token; pass --token or set %s\n", config.ServeTokenEnvVar)
	}
	return ExitError
}
