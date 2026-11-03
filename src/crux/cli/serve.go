package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/lanes"
	"github.com/hivecommons/hive/crux/serve"
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
	fs.StringVar(&laneConfig, "lanes-config", "", "opt-in durable lane configuration file (default $CRUX_LANES_CONFIG)")
	fs.StringVar(&hiveAddr, "hive-callback", "", "hive capability endpoint to dial (default $"+callback.SocketEnvVar+"; none when unset)")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, "usage: crux serve [-socket PATH] [-listen HOST:PORT] [-token T] [-hive-callback ADDR] [-lanes-config PATH]")
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
