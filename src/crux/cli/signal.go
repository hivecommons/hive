package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/api"
)

// signalUsage is the `crux signal` synopsis.
const signalUsage = `usage: crux --server ADDR signal list [--since N] [--limit N] [--all] [--json] <run>
       crux --server ADDR signal emit [--message TEXT] [--data JSON] [--json] <run> <kind>

list prints one line per turn signal recorded on the run: seq, time,
source, kind and message. --since N starts after seq N; --all follows
next_seq until the server has no more.
emit records a signal on a run you own. kind is one of thinking, tool,
blocked, needs-input or input; started and done belong to the runner.`

// remoteSignal is `crux --server … signal`: the turn-signal family (RFC
// crux#114 S5). Signals live with the run on a crux serve; there is no
// in-process form.
func (e *Env) remoteSignal(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return e.remoteSignalList(ctx, c, args[1:])
	case "emit":
		return e.remoteSignalEmit(ctx, c, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: signal: unknown subcommand %q\n%s\n", args[0], signalUsage)
		return ExitUsage
	}
}

func (e *Env) remoteSignalList(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("signal list")
	since := fs.Int64("since", 0, "start after this seq")
	limit := fs.Int("limit", 0, "records per page (server default when unset)")
	all := fs.Bool("all", false, "follow next_seq until no more pages")
	asJSON := fs.Bool("json", false, "print the records as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitUsage
	}
	run := fs.Arg(0)
	var records []api.Signal
	sinceSeq := *since
	for {
		page, err := c.RunSignals(ctx, run, sinceSeq, *limit)
		if err != nil {
			return e.remoteErr("signal list", run, err)
		}
		records = append(records, page.Signals...)
		if !*all || !page.More {
			break
		}
		sinceSeq = page.NextSeq
	}
	if *asJSON {
		return e.printJSON(records)
	}
	for _, rec := range records {
		fmt.Fprintln(e.Stdout, signalLine(rec))
	}
	return ExitOK
}

func (e *Env) remoteSignalEmit(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("signal emit")
	var req api.SignalRequest
	var payload string
	fs.StringVar(&req.Message, "message", "", "a short note kept with the signal")
	fs.StringVar(&payload, "data", "", "JSON payload kept with the signal (at most 16 KiB)")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(e.Stderr, signalUsage)
		return ExitUsage
	}
	run, kind := fs.Arg(0), fs.Arg(1)
	if !callerSignalKind(kind) {
		fmt.Fprintf(e.Stderr, "crux: signal emit: kind must be one of %s, got %q\n", strings.Join(api.CallerSignalKinds, ", "), kind)
		return ExitUsage
	}
	req.Kind = kind
	if payload != "" {
		if !json.Valid([]byte(payload)) {
			fmt.Fprintln(e.Stderr, "crux: signal emit: --data is not valid JSON")
			return ExitUsage
		}
		req.Data = json.RawMessage(payload)
	}
	rec, err := c.EmitSignal(ctx, run, req)
	if err != nil {
		return e.remoteErr("signal emit", run, err)
	}
	if *asJSON {
		return e.printJSON(rec)
	}
	fmt.Fprintln(e.Stdout, signalLine(rec))
	return ExitOK
}

// callerSignalKind reports whether kind is one a caller may emit.
func callerSignalKind(kind string) bool {
	for _, k := range api.CallerSignalKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// signalLine is the one-line summary list and emit print.
func signalLine(rec api.Signal) string {
	line := fmt.Sprintf("%d %s %s %s", rec.Seq, rec.At.Format(timeLayout), rec.Source, rec.Kind)
	if rec.Message != "" {
		line += "  " + rec.Message
	}
	return line
}
