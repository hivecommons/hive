package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// signalUsage is the `crux signal` synopsis.
const signalUsage = `usage: crux --server ADDR signal emit <run> --kind K [--message M] [--data JSON] [--json]
       crux --server ADDR signal list [--since N] [--limit N] [--all] [--json] <run>

emit reports where the run's turn is, as the run's owner: kind is one of
thinking, tool, blocked, needs-input or input (started and done are the
runner's). list prints one line per signal: seq, time, kind/source,
message and data. --since N starts after seq N; --all follows next_seq
until the server has no more. A run's last signal is done.`

// remoteSignal is `crux --server … signal`: the turn-signal family (RFC
// crux#114 S5). Signals are written by crux serve and read through its
// API; there is no in-process form.
func (e *Env) remoteSignal(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitUsage
	}
	switch args[0] {
	case "emit":
		return e.remoteSignalEmit(ctx, c, args[1:])
	case "list":
		return e.remoteSignalList(ctx, c, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: signal: unknown subcommand %q\n%s\n", args[0], signalUsage)
		return ExitUsage
	}
}

func (e *Env) remoteSignalEmit(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("signal emit")
	var in api.RunSignalRequest
	var data string
	fs.StringVar(&in.Kind, "kind", "", "thinking, tool, blocked, needs-input or input")
	fs.StringVar(&in.Message, "message", "", "what the turn is doing (scrubbed, at most 1 KiB)")
	fs.StringVar(&data, "data", "", "one JSON value recorded beside the message (at most 8 KiB)")
	asJSON := fs.Bool("json", false, "print the recorded signal as JSON")
	run, code, ok := parseWithID(fs, args)
	if !ok {
		return code
	}
	if run == "" || in.Kind == "" {
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitUsage
	}
	if data != "" {
		if !json.Valid([]byte(data)) {
			fmt.Fprintln(e.Stderr, "crux: signal emit: --data is not valid JSON")
			return ExitUsage
		}
		in.Data = json.RawMessage(data)
	}
	sig, err := c.EmitRunSignal(ctx, run, in)
	if err != nil {
		return e.signalErr("emit", run, err)
	}
	if *asJSON {
		return e.printJSON(sig)
	}
	fmt.Fprintln(e.Stdout, signalLine(sig))
	return ExitOK
}

func (e *Env) remoteSignalList(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("signal list")
	since := fs.Int64("since", 0, "start after this seq")
	limit := fs.Int("limit", 0, "signals per page (server default when unset)")
	all := fs.Bool("all", false, "follow next_seq until no more pages")
	asJSON := fs.Bool("json", false, "print the signals as JSON")
	run, code, ok := parseWithID(fs, args)
	if !ok {
		return code
	}
	if run == "" {
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitUsage
	}
	sigs := []api.RunSignal{}
	sinceSeq := *since
	for {
		page, err := c.RunSignals(ctx, run, sinceSeq, *limit)
		if err != nil {
			return e.signalErr("list", run, err)
		}
		sigs = append(sigs, page.Signals...)
		if !*all || !page.More {
			break
		}
		sinceSeq = page.NextSeq
	}
	if *asJSON {
		return e.printJSON(sigs)
	}
	for _, sig := range sigs {
		fmt.Fprintln(e.Stdout, signalLine(sig))
	}
	return ExitOK
}

// signalLine is the one-line form list and emit print.
func signalLine(sig api.RunSignal) string {
	line := fmt.Sprintf("%d %s %s/%s", sig.Seq, sig.At.UTC().Format(timeLayout), sig.Kind, sig.Source)
	if sig.Message != "" {
		line += "  " + sig.Message
	}
	if len(sig.Data) > 0 {
		line += "  " + string(sig.Data)
	}
	return line
}

// signalErr prints a failure; refusals the caller can fix are usage
// errors, everything else a runtime failure.
func (e *Env) signalErr(verb, run string, err error) int {
	fmt.Fprintf(e.Stderr, "crux: signal %s %s: %v\n", verb, run, err)
	var se *api.StatusError
	if errors.As(err, &se) {
		switch se.Body.Code {
		case api.CodeBadRequest, api.CodeForbidden:
			return ExitUsage
		}
	}
	if api.IsUnauthorized(err) {
		fmt.Fprintf(e.Stderr, "crux: the server wants a bearer token; pass --token or set %s\n", config.ServeTokenEnvVar)
	}
	return ExitError
}
