package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/api"
)

// cmdlogUsage is the `crux cmdlog` synopsis.
const cmdlogUsage = `usage: crux --server ADDR cmdlog list [--since N] [--limit N] [--all] [--json] <run>
       crux --server ADDR cmdlog show [--json] <run> <seq>

list prints one line per command the runner dispatched into the run:
seq, status, exit code, duration and the command line. --since N starts
after seq N; --all follows next_seq until the server has no more.
show prints one command with its kept stdout/stderr head and tail.`

// remoteCmdlog is `crux --server … cmdlog`: the command-log family (RFC
// crux#114 S4). The log is written by the runner inside crux serve and
// read through its API; there is no in-process form.
func (e *Env) remoteCmdlog(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, cmdlogUsage)
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return e.remoteCmdlogList(ctx, c, args[1:])
	case "show":
		return e.remoteCmdlogShow(ctx, c, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(e.Stderr, cmdlogUsage)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: cmdlog: unknown subcommand %q\n%s\n", args[0], cmdlogUsage)
		return ExitUsage
	}
}

func (e *Env) remoteCmdlogList(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("cmdlog list")
	since := fs.Int64("since", 0, "start after this seq")
	limit := fs.Int("limit", 0, "records per page (server default when unset)")
	all := fs.Bool("all", false, "follow next_seq until no more pages")
	asJSON := fs.Bool("json", false, "print the records as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, cmdlogUsage)
		return ExitUsage
	}
	run := fs.Arg(0)
	var records []api.CommandRecord
	sinceSeq := *since
	for {
		page, err := c.RunCmdlog(ctx, run, sinceSeq, *limit)
		if err != nil {
			return e.remoteErr("cmdlog list", run, err)
		}
		records = append(records, page.Commands...)
		if !*all || !page.More {
			break
		}
		sinceSeq = page.NextSeq
	}
	if *asJSON {
		return e.printJSON(records)
	}
	for _, rec := range records {
		fmt.Fprintln(e.Stdout, commandLine(rec))
	}
	return ExitOK
}

func (e *Env) remoteCmdlogShow(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("cmdlog show")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(e.Stderr, cmdlogUsage)
		return ExitUsage
	}
	run := fs.Arg(0)
	var seq int64
	if _, err := fmt.Sscanf(fs.Arg(1), "%d", &seq); err != nil || seq <= 0 {
		fmt.Fprintf(e.Stderr, "crux: cmdlog show: seq must be a positive integer, got %q\n", fs.Arg(1))
		return ExitUsage
	}
	// The record with seq N is the first one after N-1.
	page, err := c.RunCmdlog(ctx, run, seq-1, 1)
	if err != nil {
		return e.remoteErr("cmdlog show", run, err)
	}
	if len(page.Commands) == 0 || page.Commands[0].Seq != seq {
		fmt.Fprintf(e.Stderr, "crux: cmdlog show: run %s has no command %d\n", run, seq)
		return ExitError
	}
	rec := page.Commands[0]
	if *asJSON {
		return e.printJSON(rec)
	}
	fmt.Fprintln(e.Stdout, commandLine(rec))
	if rec.Cwd != "" {
		fmt.Fprintf(e.Stdout, "cwd: %s\n", rec.Cwd)
	}
	fmt.Fprintf(e.Stdout, "capture: %s\n", rec.Capture)
	fmt.Fprintf(e.Stdout, "started: %s\n", rec.StartedAt.Format(timeLayout))
	if rec.EndedAt != nil {
		fmt.Fprintf(e.Stdout, "ended:   %s\n", rec.EndedAt.Format(timeLayout))
	}
	if rec.Reason != "" {
		fmt.Fprintf(e.Stdout, "reason: %s\n", rec.Reason)
	}
	printOutput(e, "stdout", rec.Stdout)
	printOutput(e, "stderr", rec.Stderr)
	return ExitOK
}

// timeLayout is how show prints timestamps.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// commandLine is the one-line summary list prints.
func commandLine(rec api.CommandRecord) string {
	line := fmt.Sprintf("%d %s", rec.Seq, rec.Status)
	if rec.ExitCode != nil {
		line += fmt.Sprintf(" exit=%d", *rec.ExitCode)
	}
	if rec.DurationMs != nil {
		line += fmt.Sprintf(" %dms", *rec.DurationMs)
	}
	return line + "  " + strings.Join(rec.Command, " ")
}

// printOutput writes one stream's kept head and tail with a marker where
// the middle was cut.
func printOutput(e *Env, name string, out *api.CommandOutput) {
	if out == nil {
		return
	}
	fmt.Fprintf(e.Stdout, "--- %s (%d bytes", name, out.Bytes)
	if out.Truncated {
		fmt.Fprint(e.Stdout, ", middle omitted")
	}
	fmt.Fprintln(e.Stdout, ")")
	if out.Head != "" {
		fmt.Fprint(e.Stdout, out.Head)
		if !strings.HasSuffix(out.Head, "\n") {
			fmt.Fprintln(e.Stdout)
		}
	}
	if out.Truncated {
		fmt.Fprintln(e.Stdout, "[…]")
	}
	if out.Tail != "" {
		fmt.Fprint(e.Stdout, out.Tail)
		if !strings.HasSuffix(out.Tail, "\n") {
			fmt.Fprintln(e.Stdout)
		}
	}
}

// printJSON writes v indented to stdout.
func (e *Env) printJSON(v any) int {
	enc := json.NewEncoder(e.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	return ExitOK
}
