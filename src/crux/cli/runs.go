package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/api"
)

// runsUsage is the `crux runs` synopsis. The verb is `runs`, not `run`,
// because `crux run` already starts an agent from a definition.
const runsUsage = `usage: crux --server ADDR runs start [flags] -- <command> [args…]
       crux --server ADDR runs status <id>
       crux --server ADDR runs cancel <id>
       crux --server ADDR runs list [--status S] [--limit N] [--json]
       crux --server ADDR runs restart [--reason TEXT] [--idempotency-key KEY] [--detach] [--json] <id>
       crux --server ADDR runs history [--json] <id>

start flags:
  --checkout ID             run in a pinned checkout you own (crux checkout add)
  --repo URL --commit SHA   clone URL at SHA into a fresh workspace
  --workspace PATH          run in an existing directory on the server
  --env NAME                pass this environment variable through (repeatable)
  --network none|restricted network for the command (default none)
  --timeout DURATION        wall-clock limit (server default when unset)
  --max-output BYTES        bytes of stdout/stderr returned (server default)
  --profile experiment|agent
                            agent opens a step session: no command, answered
                            as accepted, steps via 'crux step run', closed by
                            'runs cancel'
  --idempotency-key KEY     return the existing run for a repeated request
  --detach                  return the accepted record instead of waiting
  --json                    print the record as JSON`

// remoteRuns is `crux --server … runs`: the contained-run family (RFC
// crux#114 S2 + S7). It only exists against a server; the runtime that
// launches sandboxes and keeps run records lives there.
func (e *Env) remoteRuns(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, runsUsage)
		return ExitUsage
	}
	switch args[0] {
	case "start":
		return e.remoteRunsStart(ctx, c, args[1:])
	case "status":
		return e.remoteRunsGet(ctx, c, args[1:])
	case "cancel":
		return e.remoteRunsCancel(ctx, c, args[1:])
	case "list":
		return e.remoteRunsList(ctx, c, args[1:])
	case "restart":
		return e.remoteRunsRestart(ctx, c, args[1:])
	case "history":
		return e.remoteRunsHistory(ctx, c, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(e.Stderr, runsUsage)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: runs: unknown subcommand %q\n%s\n", args[0], runsUsage)
		return ExitUsage
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func (e *Env) remoteRunsStart(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("runs start")
	var req api.ContainedRunRequest
	var wt api.ContainedRunWorktree
	var env stringList
	fs.StringVar(&req.Checkout, "checkout", "", "pinned checkout id to run in")
	fs.StringVar(&wt.Repo, "repo", "", "repository URL to clone")
	fs.StringVar(&wt.Commit, "commit", "", "full commit SHA to check out")
	fs.StringVar(&req.WorkspacePath, "workspace", "", "existing directory on the server")
	fs.Var(&env, "env", "environment variable to pass through (repeatable)")
	fs.StringVar(&req.Network, "network", "", "none or restricted")
	timeout := fs.Duration("timeout", 0, "wall-clock limit")
	fs.Int64Var(&req.MaxOutputBytes, "max-output", 0, "bytes of output returned")
	fs.StringVar(&req.Profile, "profile", "", "experiment or agent")
	fs.StringVar(&req.IdempotencyKey, "idempotency-key", "", "idempotency key")
	fs.BoolVar(&req.Detach, "detach", false, "return the accepted record")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	// An agent session takes no command: its steps are posted with
	// `crux step run`.
	if fs.NArg() == 0 && req.Profile != api.RunProfileAgent {
		fmt.Fprintln(e.Stderr, runsUsage)
		return ExitUsage
	}
	req.Command = fs.Args()
	req.EnvAllowlist = env
	req.TimeoutMs = timeout.Milliseconds()
	if wt.Repo != "" || wt.Commit != "" {
		req.Worktree = &wt
	}
	run, err := c.CreateRun(ctx, req)
	if err != nil {
		return e.remoteErr("runs start", "", err)
	}
	return e.printRun(run, *asJSON)
}

func (e *Env) remoteRunsGet(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("runs status")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR runs status [--json] <id>")
		return ExitUsage
	}
	run, err := c.GetRun(ctx, fs.Arg(0))
	if err != nil {
		return e.remoteErr("runs status", fs.Arg(0), err)
	}
	return e.printRun(run, *asJSON)
}

func (e *Env) remoteRunsCancel(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("runs cancel")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR runs cancel [--json] <id>")
		return ExitUsage
	}
	run, err := c.CancelRun(ctx, fs.Arg(0))
	if err != nil {
		return e.remoteErr("runs cancel", fs.Arg(0), err)
	}
	if run.AlreadyFinished {
		fmt.Fprintf(e.Stderr, "crux: run %s had already finished\n", run.ID)
	}
	return e.printRun(run, *asJSON)
}

// printRun writes the record: JSON when asked, otherwise a one-line
// summary on stdout with the command's own output following so a caller
// can pipe `crux runs start` like the command itself. The exit code is
// the command's when it is known; an unknown exit is ExitError, never a
// guess.
func (e *Env) printRun(run api.ContainedRun, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(run); err != nil {
			fmt.Fprintln(e.Stderr, "crux:", err)
			return ExitError
		}
		return runExit(run)
	}
	line := fmt.Sprintf("%s %s", run.ID, run.Status)
	if run.ExitCode != nil {
		line += fmt.Sprintf(" exit=%d", *run.ExitCode)
	}
	if run.TimedOut {
		line += " timed_out"
	}
	if run.Reason != "" {
		line += " (" + run.Reason + ")"
	}
	fmt.Fprintln(e.Stderr, line)
	if run.Stdout != "" {
		fmt.Fprint(e.Stdout, run.Stdout)
		if run.StdoutTruncatedAt != nil {
			fmt.Fprintf(e.Stderr, "\n[stdout truncated at byte %d; the full output is kept with the run]\n", *run.StdoutTruncatedAt)
		}
	}
	if run.Stderr != "" {
		fmt.Fprint(e.Stderr, run.Stderr)
		if run.StderrTruncatedAt != nil {
			fmt.Fprintf(e.Stderr, "\n[stderr truncated at byte %d; the full output is kept with the run]\n", *run.StderrTruncatedAt)
		}
	}
	return runExit(run)
}

// runExit maps a record onto the process exit code.
func runExit(run api.ContainedRun) int {
	switch run.Status {
	case api.RunCompleted:
		if run.ExitCode != nil && *run.ExitCode != 0 {
			return ExitError
		}
		return ExitOK
	case api.RunAccepted, api.RunRunning, api.RunWaiting:
		return ExitOK
	default:
		return ExitError
	}
}
