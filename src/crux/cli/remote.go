package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// clientFactory builds the API client remote mode uses. Production uses
// api.NewClient; tests may substitute one bound to an httptest server.
type clientFactory func(server, token string) (*api.Client, error)

// remoteSelected reports whether this invocation drives a `crux serve`
// instead of an in-process runtime: --server was given or CRUX_SERVER is
// set. The token comes from --token, then CRUX_TOKEN. `help` and `serve`
// ignore the selection; everything else goes through remote.
func (o *globalOptions) remoteSelected() bool {
	if o.server == "" {
		o.server = os.Getenv(config.ServerEnvVar)
	}
	if o.server != "" && o.token == "" {
		o.token = os.Getenv(config.ServeTokenEnvVar)
	}
	return o.server != ""
}

// remote routes a subcommand to a `crux serve` over the Crux API. Every
// verb maps onto one client call; `run` without -detach follows the log
// stream and stops the agent when ctx ends, as the in-process `run` does.
func (e *Env) remote(ctx context.Context, opts globalOptions, name string, args []string) int {
	c, err := e.newClient(opts.server, opts.token)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crux: --server %q: %v\n", opts.server, err)
		return ExitUsage
	}
	switch name {
	case "run":
		return e.remoteRun(ctx, c, opts, args)
	case "ps":
		return e.remotePS(ctx, c, args)
	case "logs":
		return e.remoteLogs(ctx, c, args)
	case "kick":
		return e.remoteKick(ctx, c, args)
	case "stop":
		return e.remoteStop(ctx, c, args)
	default:
		fmt.Fprintf(e.Stderr, "crux: %q is not available with --server; it runs where the runtime is\n", name)
		return ExitUsage
	}
}

// remoteErr prints a server-side failure. Usage-class answers (404 for an
// unknown agent, 400 for a bad request) are the caller's mistake; the rest
// are runtime errors.
func (e *Env) remoteErr(verb, name string, err error) int {
	if name != "" {
		fmt.Fprintf(e.Stderr, "crux: %s %s: %v\n", verb, name, err)
	} else {
		fmt.Fprintf(e.Stderr, "crux: %s: %v\n", verb, err)
	}
	if api.IsUnauthorized(err) {
		fmt.Fprintf(e.Stderr, "crux: the server wants a bearer token; pass --token or set %s\n", config.ServeTokenEnvVar)
	}
	return ExitError
}

// remoteRun is `crux --server … run`: a definition file is loaded here and
// sent as RunRequest.Definition; a bare name must already be defined on the
// server. Without -detach the ring buffer is followed over SSE until ctx
// ends, then the agent is stopped.
func (e *Env) remoteRun(ctx context.Context, c *api.Client, opts globalOptions, args []string) int {
	fs := e.subFlags("run")
	detach := fs.Bool("detach", false, "return after Start; do not follow or stop the agent")
	_ = fs.Duration("interval", defaultStreamInterval, "ignored with --server; the server paces the stream")
	_ = fs.Bool("no-socket", false, "ignored with --server; no control socket is served")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR run [-detach] <definition.yaml | agent>")
		return ExitUsage
	}
	target := fs.Arg(0)
	req := api.RunRequest{Name: target}
	if isDefinitionPath(target) {
		defName, cfg, err := e.Loader.LoadDefinitionFile(target)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux:", err)
			return ExitError
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux: encoding definition:", err)
			return ExitError
		}
		req.Name, req.Definition = defName, raw
	}

	resp, err := c.Run(ctx, req)
	if err != nil {
		return e.remoteErr("start", req.Name, err)
	}
	name := resp.Agent
	fmt.Fprintf(e.Stdout, "started %s\n", name)
	if *detach {
		return ExitOK
	}

	followErr := c.FollowLogs(ctx, name, func(chunk api.LogChunk) error {
		for _, line := range chunk.Lines {
			fmt.Fprintln(e.Stdout, line)
		}
		return nil
	})
	if followErr != nil {
		fmt.Fprintf(e.Stderr, "crux: output %s: %v\n", name, followErr)
	}

	// ctx is done (or the stream broke); stop with a fresh context so the
	// request still goes out.
	if _, err := c.Stop(context.WithoutCancel(ctx), name); err != nil {
		return e.remoteErr("stop", name, err)
	}
	fmt.Fprintf(e.Stdout, "stopped %s\n", name)
	return ExitOK
}

// remotePS is `crux --server … ps`: GET /v1/agents.
func (e *Env) remotePS(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("ps")
	asJSON := fs.Bool("json", false, "emit a JSON array instead of a table")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	procs, err := c.List(ctx)
	if err != nil {
		return e.remoteErr("ps", "", err)
	}
	rows := make([]processRow, 0, len(procs))
	for _, p := range procs {
		rows = append(rows, processRow{
			Name:         p.Name,
			ID:           p.ID,
			State:        p.State,
			Backend:      p.Backend,
			Model:        p.Model,
			Paused:       p.Paused,
			StartedAt:    p.StartedAt,
			LastKick:     p.LastKick,
			RestartCount: p.RestartCount,
		})
	}
	if *asJSON {
		return e.writeJSON(rows)
	}
	return e.printPS(rows)
}

// remoteLogs is `crux --server … logs`: GET /v1/agents/{name}/logs, with -f
// following the ring buffer over SSE until ctx ends.
func (e *Env) remoteLogs(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("logs")
	lines := fs.Int("n", defaultLogLines, "lines to print from the pane or buffer")
	buffer := fs.Bool("buffer", false, "read the ring buffer instead of the live pane")
	full := fs.Bool("full", false, "capture the whole scrollback")
	follow := fs.Bool("f", false, "follow the ring buffer until interrupted")
	asJSON := fs.Bool("json", false, "emit JSON instead of raw lines")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR logs [-n N] [-buffer] [-full] [-f] [-json] <agent>")
		return ExitUsage
	}
	if *lines < 0 {
		fmt.Fprintln(e.Stderr, "crux: -n must not be negative")
		return ExitUsage
	}
	if (*buffer && *full) || (*follow && (*full || *asJSON)) {
		fmt.Fprintln(e.Stderr, "crux: -buffer and -full are exclusive; -f takes neither -full nor -json")
		return ExitUsage
	}
	name := fs.Arg(0)
	if *follow {
		err := c.FollowLogs(ctx, name, func(chunk api.LogChunk) error {
			for _, line := range chunk.Lines {
				fmt.Fprintln(e.Stdout, line)
			}
			return nil
		})
		if err != nil {
			return e.remoteErr("logs", name, err)
		}
		return ExitOK
	}
	source := api.LogSourcePane
	switch {
	case *full:
		source = api.LogSourceFull
	case *buffer:
		source = api.LogSourceBuffer
	}
	logs, err := c.Logs(ctx, name, source, *lines)
	if err != nil {
		return e.remoteErr("logs", name, err)
	}
	out := logsOutput{Agent: logs.Agent, Source: logs.Source, Header: logs.Header, Lines: logs.Lines, Content: logs.Content}
	if *asJSON {
		return e.writeJSON(out)
	}
	e.printLogs(out, *full)
	return ExitOK
}

// remoteKick is `crux --server … kick`: POST /v1/agents/{name}/kick.
func (e *Env) remoteKick(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("kick")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(e.Stderr, "usage: crux kick <agent> <message...|->")
		return ExitUsage
	}
	name := fs.Arg(0)
	message := strings.Join(fs.Args()[1:], " ")
	if message == "-" {
		data, err := io.ReadAll(e.Stdin)
		if err != nil {
			fmt.Fprintln(e.Stderr, "crux: reading kick from stdin:", err)
			return ExitError
		}
		message = strings.TrimRight(string(data), "\n")
	}
	if strings.TrimSpace(message) == "" {
		fmt.Fprintln(e.Stderr, "crux: kick message is empty")
		return ExitUsage
	}
	resp, err := c.Kick(ctx, name, message)
	if err != nil {
		return e.remoteErr("kick", name, err)
	}
	fmt.Fprintf(e.Stdout, "kicked %s\n", resp.Agent)
	return ExitOK
}

// remoteStop is `crux --server … stop`: POST /v1/agents/{name}/stop.
func (e *Env) remoteStop(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("stop")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux stop <agent>")
		return ExitUsage
	}
	name := fs.Arg(0)
	resp, err := c.Stop(ctx, name)
	if err != nil {
		return e.remoteErr("stop", name, err)
	}
	fmt.Fprintf(e.Stdout, "stopped %s\n", resp.Agent)
	return ExitOK
}
