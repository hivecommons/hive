// Package cli is the `crux` command (hivecommons/hive#13536, step 5 of the
// Stage 1 roadmap in src/docs/design/crux.md): run, ps, logs, kick and stop,
// each a thin mapping onto one or two crux.Runtime methods, and serve, which
// exposes the Runtime over the Crux API (crux/api, crux/serve; Stage 3).
//
// The runtime is in-process. `crux run` is therefore the long-lived command:
// it builds a Runtime, starts the agent, streams its output and stops it when
// interrupted. While it runs it serves a control socket in the work dir, and
// the other subcommands talk to that socket when it is present so they see
// the running runtime rather than a fresh one. Without a socket they build
// their own runtime from --config, which can observe definitions and the
// pane of a surviving tmux session but holds no lifecycle state.
//
// Everything here programs against crux.Runtime; *agent.Manager appears only
// in runtime.go, where it is constructed. The package lives under pkg/crux so
// the boundary ratchet treats it as part of Crux.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// Exit codes. Usage errors are distinct from runtime failures so wrappers can
// tell a typo from a refused kick.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

const (
	// envWorkDir is the manager's own work-dir variable; --workdir sets it so
	// the manager and the CLI agree on where agents live.
	envWorkDir = "HIVE_WORK_DIR"
	// defaultWorkDirName is the per-user fallback when HIVE_WORK_DIR is unset.
	defaultWorkDirName = ".crux"
	// controlSocketName is the control socket's file name inside the work dir.
	controlSocketName = "crux.sock"
	// defaultStreamInterval is how often `run` polls the output buffer.
	defaultStreamInterval = time.Second
	// streamWindow is how many buffer lines each poll asks for.
	streamWindow = 200
	// defaultLogLines is the default -n for `logs`.
	defaultLogLines = 100
	// DefsEnvVar is the environment default for --defs / --config: the
	// definition source the host's DefinitionLoader reads when neither flag
	// is given (a directory or file of agent definitions for the module's
	// own loader, a hive.yaml for the Hive-backed one).
	DefsEnvVar = "CRUX_DEFS"
	// DefaultVersion is what --version reports when the host did not set
	// Env.Version (a `go build` without -ldflags "-X main.version=...").
	DefaultVersion = "dev"
)

// globalOptions are the flags that precede the subcommand.
type globalOptions struct {
	workDir string
	config  string
	socket  string
	local   bool
	version bool
	// server and token select remote mode: drive a `crux serve` over the
	// Crux API instead of an in-process runtime.
	server string
	token  string
}

// runtimeFactory builds the Runtime a command operates on. Production uses
// newManagerRuntime; tests inject a fake.
type runtimeFactory func(defs map[string]config.AgentConfig, project crux.ProjectContext) crux.Runtime

// tickerFactory returns a channel that fires every d and a stop function.
// Tests substitute a channel they drive by hand.
type tickerFactory func(d time.Duration) (<-chan time.Time, func())

// Env is what a command needs beyond its arguments: the streams, the
// definition loader, the runtime factory and the clock.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Loader reads --config and definition files. Nil means this build cannot
	// read them (see noLoader); cmd/crux wires the Hive-backed one.
	Loader DefinitionLoader
	// Version is what --version prints; empty means DefaultVersion. Hosts set
	// it from their ldflags-injected main.version.
	Version string

	newRuntime runtimeFactory
	ticker     tickerFactory
	// dial connects to a control socket; nil uses the unix dialer.
	dial dialFunc
	// newClient builds the API client for --server; nil uses api.NewClient.
	newClient clientFactory
}

// Main runs the crux command line and returns its exit code. ctx ending
// interrupts `run` (which then stops its agent) and any in-flight command.
// loader is how --config and definition files are read; see DefinitionLoader.
func Main(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, loader DefinitionLoader) int {
	env := &Env{Stdin: stdin, Stdout: stdout, Stderr: stderr, Loader: loader}
	return env.Run(ctx, args)
}

// Run executes args against this Env, filling in production defaults for any
// collaborator left nil.
func (e *Env) Run(ctx context.Context, args []string) int {
	e.defaults()
	fs := flag.NewFlagSet("crux", flag.ContinueOnError)
	fs.SetOutput(e.Stderr)
	var opts globalOptions
	fs.StringVar(&opts.workDir, "workdir", "", "agent work dir (default $HIVE_WORK_DIR, then $HOME/"+defaultWorkDirName+")")
	fs.StringVar(&opts.config, "config", "", "definition source the loader reads (a hive.yaml for the hive-backed crux; default $"+DefsEnvVar+")")
	fs.StringVar(&opts.config, "defs", "", "synonym of -config: a directory or file of agent definitions for this build's loader (default $"+DefsEnvVar+")")
	fs.StringVar(&opts.socket, "socket", "", "control socket path (default <workdir>/"+controlSocketName+")")
	fs.BoolVar(&opts.local, "local", false, "never use a running `crux run` control socket; build a fresh runtime")
	fs.StringVar(&opts.server, "server", "", "drive a `crux serve` at this address (unix socket path, unix:///path, host:port or http://…) instead of an in-process runtime (default $"+config.ServerEnvVar+")")
	fs.StringVar(&opts.token, "token", "", "bearer token for --server (default $"+config.ServeTokenEnvVar+")")
	fs.BoolVar(&opts.version, "version", false, "print the crux version and exit")
	fs.Usage = func() { e.usage(fs) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if opts.version {
		fmt.Fprintln(e.Stdout, "crux", e.version())
		return ExitOK
	}
	rest := fs.Args()
	if len(rest) == 0 {
		e.usage(fs)
		return ExitUsage
	}
	if err := opts.resolve(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	if opts.remoteSelected() {
		switch rest[0] {
		case "help", "-h", "--help", "serve", agent.AgyTurnSubcommand:
			// serve is where the runtime lives; a CRUX_SERVER left in a shell
			// profile must not stop a local server from starting. agy-turn is
			// the pane-side subprocess the serving runtime itself spawns.
		default:
			return e.remote(ctx, opts, rest[0], rest[1:])
		}
	}
	return e.dispatch(ctx, opts, rest[0], rest[1:], false)
}

// dispatch routes a subcommand. remote is true when the call arrived over the
// control socket, in which case the command must run against the serving
// runtime and never dial out again.
func (e *Env) dispatch(ctx context.Context, opts globalOptions, name string, args []string, remote bool) int {
	switch name {
	case "run":
		if remote {
			fmt.Fprintln(e.Stderr, "crux: run is not available over the control socket")
			return ExitUsage
		}
		return e.cmdRun(ctx, opts, args)
	case "serve":
		if remote {
			fmt.Fprintln(e.Stderr, "crux: serve is not available over the control socket")
			return ExitUsage
		}
		return e.cmdServe(ctx, opts, args)
	case agent.AgyTurnSubcommand:
		if remote {
			fmt.Fprintf(e.Stderr, "crux: %s is not available over the control socket\n", name)
			return ExitUsage
		}
		// The manager types `<own executable> agy-turn …` into a headless agy
		// pane; under `crux serve` that executable is crux.
		return agent.RunAgyTurn(args, e.Stdout, e.Stderr)
	case "ps", "logs", "kick", "stop":
		if !remote && !opts.local {
			if code, ok := e.forward(ctx, opts, append([]string{name}, args...)); ok {
				return code
			}
		}
		return e.local(ctx, opts, name, args)
	case "help", "-h", "--help":
		fs := flag.NewFlagSet("crux", flag.ContinueOnError)
		e.usage(fs)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: unknown command %q\n", name)
		return ExitUsage
	}
}

// local runs an observer/control subcommand against a runtime built from
// --config in this process.
func (e *Env) local(ctx context.Context, opts globalOptions, name string, args []string) int {
	defs, project, err := e.Loader.LoadConfig(opts.config)
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	if len(defs) == 0 {
		fmt.Fprintf(e.Stderr, "crux: no `crux run` is serving %s and no --config was given; the runtime has no agents\n", opts.socket)
	}
	rt := e.newRuntime(defs, project)
	return e.observe(ctx, rt, name, args)
}

// observe runs one of the observer/control subcommands against rt. It is
// shared by the local path and the control-socket server.
func (e *Env) observe(ctx context.Context, rt crux.Runtime, name string, args []string) int {
	switch name {
	case "ps":
		return e.cmdPS(rt, args)
	case "logs":
		return e.cmdLogs(rt, args)
	case "kick":
		return e.cmdKick(rt, args)
	case "stop":
		return e.cmdStop(rt, args)
	default:
		fmt.Fprintf(e.Stderr, "crux: %q is not available over the control socket\n", name)
		return ExitUsage
	}
}

func (e *Env) defaults() {
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Loader == nil {
		e.Loader = noLoader{}
	}
	if e.newRuntime == nil {
		e.newRuntime = newManagerRuntime
	}
	if e.ticker == nil {
		e.ticker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	if e.dial == nil {
		e.dial = dialControl
	}
	if e.newClient == nil {
		e.newClient = api.NewClient
	}
}

func (e *Env) version() string {
	if e.Version == "" {
		return DefaultVersion
	}
	return e.Version
}

// SignalContext returns a context that ends on SIGINT or SIGTERM, so a
// foreground `crux run` or `crux serve` stops its agents before exiting. Both
// hosts (cmd/crux in the root module, crux/cmd/crux in this one) hand it to
// Env.Run.
func SignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// resolve fills the work dir and socket defaults, the definition source from
// CRUX_DEFS, and exports the work dir to the manager through HIVE_WORK_DIR.
func (o *globalOptions) resolve() error {
	if o.config == "" {
		o.config = os.Getenv(DefsEnvVar)
	}
	if o.workDir == "" {
		o.workDir = os.Getenv(envWorkDir)
	}
	if o.workDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolving work dir: %w (set --workdir or %s)", err, envWorkDir)
		}
		o.workDir = filepath.Join(home, defaultWorkDirName)
	}
	if err := os.Setenv(envWorkDir, o.workDir); err != nil {
		return fmt.Errorf("exporting %s: %w", envWorkDir, err)
	}
	if o.socket == "" {
		o.socket = filepath.Join(o.workDir, controlSocketName)
	}
	return nil
}

func (e *Env) usage(fs *flag.FlagSet) {
	fmt.Fprint(e.Stderr, `usage: crux [global flags] <command> [command flags] [args]

Commands (each maps onto crux.Runtime):
  run  <definition.yaml | agent>  Definitions.AddAgent + Lifecycle.Start, then stream
                                  output and Lifecycle.Stop on interrupt
  ps                              Observer.AllStatuses
  logs <agent>                    Observer.GetOutput (--buffer: GetBufferOutput,
                                  --full: CaptureFullLog)
  kick <agent> <message | ->      Kicker.SendKick
  stop <agent>                    Lifecycle.Stop
  serve                           expose the runtime over the Crux API (HTTP+JSON
                                  on a unix socket; --listen adds TCP with --token)
  agy-turn                        one headless agy turn; typed into a pane by the
                                  runtime that spawned it, not run by hand

While 'crux run' is in the foreground it serves <workdir>/crux.sock; ps, logs,
kick and stop use that socket when present so they act on the live runtime.
'crux serve' is the API server other harnesses drive (crux/api, crux/serve).
With --server ADDR (or CRUX_SERVER set) run, ps, logs, kick and stop drive that
server over the API instead of an in-process runtime; --token / CRUX_TOKEN
carries its bearer token, and 'logs -f' follows the agent's output. 'serve'
itself always runs locally, whatever CRUX_SERVER says.

--config and --defs name the same thing: where this build's definition loader
reads agents from (default $CRUX_DEFS). The hive-backed crux reads a hive.yaml;
the module's own crux reads a directory or file of agent definition YAML.

Global flags:
`)
	fs.PrintDefaults()
}
