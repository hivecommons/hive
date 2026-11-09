package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/cmdlog"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/gates"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/crux/signals"
	"github.com/nats-io/nats.go"
)

// cmdServe is `crux serve`: build the runtime from --config and expose it
// over the Crux API (crux/api, served by crux/serve) until ctx ends. It is
// the process boundary of Stage 3: a harness other than hive drives agents
// through the socket or, with a token, over TCP; the other `crux`
// subcommands reach it with --server / CRUX_SERVER.
//
// The socket defaults to config.DefaultServeSocket (not the per-work-dir
// control socket `crux run` serves); --listen adds TCP and is refused
// without --token / CRUX_TOKEN, and refused off loopback (plain http would
// carry the token in the clear; remote access goes through a TLS proxy).
func (e *Env) cmdServe(ctx context.Context, opts globalOptions, args []string) int {
	fs := e.subFlags("serve")
	var cfg config.ServeConfig
	fs.StringVar(&cfg.Socket, "socket", "", "unix socket to serve on (default $"+config.ServeSocketEnvVar+", then "+config.DefaultServeSocket()+")")
	fs.StringVar(&cfg.Listen, "listen", "", "also serve on this loopback TCP host:port (plain http); requires a token")
	fs.StringVar(&cfg.Token, "token", "", "bearer token every request must carry (default $"+config.ServeTokenEnvVar+")")
	var hiveAddr string
	var laneConfig string
	var operatorName string
	var checkoutAllow, checkoutDir string
	fs.StringVar(&checkoutAllow, "checkout-allow", "", "comma-separated repo URL prefixes the pinned-checkout service may clone (default $"+config.CheckoutAllowlistEnvVar+"; none disables the service)")
	fs.StringVar(&checkoutDir, "checkout-dir", "", "where pinned checkouts live (default $"+config.CheckoutDirEnvVar+", then <workdir>/"+checkout.DirName+")")
	checkoutMaxBytes := fs.Int64("checkout-max-bytes", 0, "ceiling on a checkout's clone plus worktree in bytes, and the cap when a request names none (default $"+config.CheckoutMaxBytesEnvVar+", then 5 GiB); the private-repo token is $"+config.CheckoutTokenEnvVar)
	fs.StringVar(&operatorName, "operator-name", "", "name of the principal behind the control token on work-services routes (default $"+config.ServeOperatorNameEnvVar+", then "+serve.DefaultOperatorName+")")
	fs.StringVar(&laneConfig, "lanes-config", "", "opt-in durable lane configuration file (default $CRUX_LANES_CONFIG)")
	var runsConfig string
	fs.StringVar(&runsConfig, "runs-config", "", "opt-in contained-run backend configuration file (default $"+runsConfigEnvVar+")")
	fs.StringVar(&hiveAddr, "hive-callback", "", "hive capability endpoint to dial (default $"+callback.SocketEnvVar+"; none when unset)")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, "usage: crux serve [-socket PATH] [-listen HOST:PORT] [-token T] [-hive-callback ADDR] [-lanes-config PATH] [-operator-name NAME] [-checkout-allow PREFIXES] [-checkout-dir DIR] [-checkout-max-bytes N] [-runs-config PATH]")
		return ExitUsage
	}
	if cfg.Token == "" {
		cfg.Token = os.Getenv(config.ServeTokenEnvVar)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitUsage
	}
	if hiveAddr == "" {
		hiveAddr = os.Getenv(callback.SocketEnvVar)
	}
	var hive *hiveCallback
	if hiveAddr != "" {
		var err error
		if hive, err = dialHiveCallback(hiveAddr, os.Getenv(callback.TokenEnvVar)); err != nil {
			fmt.Fprintln(e.Stderr, "crux:", err)
			return ExitUsage
		}
	}

	defs, project, err := e.Loader.LoadConfig(opts.config)
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	rt := e.newRuntime(defs, project)

	srv := serve.New(rt)
	srv.Token = cfg.Token
	if operatorName == "" {
		operatorName = os.Getenv(config.ServeOperatorNameEnvVar)
	}
	srv.OperatorName = operatorName
	srv.Logger = slog.New(slog.NewTextHandler(e.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if laneConfig == "" {
		laneConfig = os.Getenv("CRUX_LANES_CONFIG")
	}
	if laneConfig != "" {
		lc, err := lanes.LoadConfig(laneConfig)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux: lanes:", err)
			return ExitUsage
		}
		auth, err := lanes.Bind(lc, os.Getenv, cfg.Token)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux: lanes:", err)
			return ExitUsage
		}
		var options []nats.Option
		if lc.CredentialsEnv != "" {
			path := os.Getenv(lc.CredentialsEnv)
			if path == "" {
				fmt.Fprintln(e.Stderr, "crux: lanes: missing broker credential file binding")
				return ExitUsage
			}
			options = append(options, nats.UserCredentials(path))
		}
		broker, err := lanes.Open(ctx, lc, options...)
		if err != nil {
			return e.laneBrokerFailure(err)
		}
		defer broker.Close()
		srv.Lanes, srv.LaneAuth = broker, auth
	}
	runSvc, err := e.openRuns(opts.workDir, runsConfig)
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux: runs:", err)
		return ExitUsage
	}
	srv.Runs = runSvc
	// S1 and S2 are wired both ways: a run may execute in a checkout, and a
	// checkout with a live run refuses deletion.
	if checkouts, err := newCheckoutService(checkoutAllow, checkoutDir, *checkoutMaxBytes, opts.workDir, runSvc.InUse); err != nil {
		fmt.Fprintln(e.Stderr, "crux: checkouts:", err)
		return ExitUsage
	} else if checkouts != nil {
		srv.Checkouts = checkouts
		runSvc.Checkouts = checkouts
	}
	// S3: gates need no configuration; records under <workdir>/gates
	// outlive the process, so a gate opened before a restart is answerable
	// after it.
	gateSvc, err := gates.New(gates.Config{Dir: gates.DefaultDir(opts.workDir)})
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux: gates:", err)
		return ExitError
	}
	srv.Gates = gateSvc
	// S5: the last kick outcome of every agent is saved under
	// <workdir>/signals/agents before it is published, so a driver that
	// missed the event reads it after a reconnect or a restart.
	outcomes, err := signals.OpenAgentOutcomes(signals.AgentsDir(opts.workDir), signals.Policy{})
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux: signals:", err)
		return ExitError
	}
	srv.KickOutcomes = outcomes
	if hive != nil {
		hive.install(ctx, rt, srv, e.Stdout)
		hive.watchPolicy(ctx, rt, e.Stdout, e.Stderr)
	}

	ls, err := serve.Listen(cfg)
	if err != nil {
		fmt.Fprintln(e.Stderr, "crux: serve:", err)
		return ExitError
	}
	fmt.Fprintf(e.Stdout, "serving %d agent definition(s) on %s\n", len(defs), strings.Join(ls.Addrs(), " and "))
	if err := srv.Serve(ctx, ls); err != nil {
		fmt.Fprintln(e.Stderr, "crux: serve:", err)
		return ExitError
	}
	fmt.Fprintln(e.Stdout, "serve stopped")
	return ExitOK
}

func (e *Env) laneBrokerFailure(err error) int {
	category := "initialization error"
	switch {
	case errors.Is(err, lanes.ErrConflict):
		category = "state conflict"
	case errors.Is(err, lanes.ErrUnavailable):
		category = "broker unavailable"
	}
	// Backend errors can contain addresses or credentials; only emit a typed category.
	fmt.Fprintf(e.Stderr, "crux: lanes: cannot initialize durable broker (%s)\n", category)
	return ExitError
}

// runsConfigEnvVar names the contained-run configuration file when
// --runs-config is not given.
const runsConfigEnvVar = "CRUX_RUNS_CONFIG"

// openRuns builds the contained-run service. Saved records under
// <workdir>/runs are always served, so a server with no backend configured
// still answers `crux runs status`; the backend comes from the config file
// when one is named. Records left in flight by an earlier process are
// marked unfinished here, before the first request can observe them.
func (e *Env) openRuns(workDir, path string) (*runs.Service, error) {
	if path == "" {
		path = os.Getenv(runsConfigEnvVar)
	}
	var cfg runs.Config
	var backend *runs.Backend
	if path != "" {
		var err error
		if cfg, err = runs.LoadConfig(path); err != nil {
			return nil, err
		}
		if backend, err = runs.BackendFromConfig(cfg); err != nil {
			return nil, err
		}
	}
	store, err := runstate.Open(workDir)
	if err != nil {
		return nil, err
	}
	svc := runs.New(store, backend, cfg)
	// S4: every dispatched command is appended to <run dir>/commands.jsonl
	// by the runner, with the default head/tail output policy. It is set
	// before Reconcile so a command left in flight gets its ended line.
	svc.CmdLog = cmdlog.FileOpener(cmdlog.Policy{})
	// S5: turn signals go to <run dir>/signals.jsonl. The book is set
	// before Reconcile so a run left in flight gets its done signal.
	svc.Signals = signals.NewBook(signals.Policy{})
	reconciled, err := svc.Reconcile()
	if err != nil {
		return nil, err
	}
	for _, rec := range reconciled {
		fmt.Fprintf(e.Stderr, "crux: runs: %s was in flight at the last shutdown; marked unfinished\n", rec.ID)
	}
	return svc, nil
}
