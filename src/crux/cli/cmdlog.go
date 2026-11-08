package cli

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// `crux cmdlog list|show` reads the command log of a contained run
// (hivecommons/crux#114 S4). Like `runs`, it exists only against a `crux
// serve`: the log is written by the runtime that dispatched the commands.

const cmdlogUsage = `usage: crux --server ADDR cmdlog list [--since-seq N] [--limit M] [--json] <run>
       crux --server ADDR cmdlog show [--seq N] [--json] <run>

list prints one page of the run's commands (the server's default page size
unless --limit), one line each; when more follow it names the --since-seq
of the next page. show prints every command in full: working directory,
times, exit, byte counts and the kept head and tail of each output stream;
--seq selects one command.

The log holds what the runtime dispatched (capture "runner"). Commands a
shell or build tool starts inside the sandbox are not listed one by one.`

// remoteCmdlog is `crux --server … cmdlog`.
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
	since := fs.Int64("since-seq", 0, "only commands with a line after this sequence number")
	limit := fs.Int("limit", 0, "commands per page (server default when 0)")
	asJSON := fs.Bool("json", false, "print the page as JSON")
	run, code, ok := e.cmdlogRun(fs, args)
	if !ok {
		return code
	}
	page, err := c.RunCommandLog(ctx, run, *since, *limit)
	if err != nil {
		return e.remoteErr("cmdlog list", run, err)
	}
	if *asJSON {
		return e.writeJSON(page)
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tSTATUS\tEXIT\tDURATION\tSTDOUT\tSTDERR\tCAPTURE\tCOMMAND")
	for _, rec := range page.Commands {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rec.Seq, rec.Status, exitText(rec.ExitCode), durationText(rec.DurationMs),
			bytesText(rec.Stdout), bytesText(rec.Stderr), rec.Capture, strings.Join(rec.Command, " "))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	if page.More {
		fmt.Fprintf(e.Stderr, "crux: more commands follow; next page: --since-seq %d\n", page.NextSeq)
	}
	return ExitOK
}

func (e *Env) remoteCmdlogShow(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("cmdlog show")
	seq := fs.Int64("seq", 0, "only the command with this sequence number")
	asJSON := fs.Bool("json", false, "print the records as JSON")
	run, code, ok := e.cmdlogRun(fs, args)
	if !ok {
		return code
	}
	// Follow every page, keeping each command's latest record: a command
	// that ended while paging appears again with its final state.
	var order []int64
	byStart := map[int64]api.CommandRecord{}
	var next int64
	for {
		page, err := c.RunCommandLog(ctx, run, next, 0)
		if err != nil {
			return e.remoteErr("cmdlog show", run, err)
		}
		for _, rec := range page.Commands {
			if _, seen := byStart[rec.Seq]; !seen {
				order = append(order, rec.Seq)
			}
			byStart[rec.Seq] = rec
		}
		if !page.More || page.NextSeq <= next {
			break
		}
		next = page.NextSeq
	}
	recs := make([]api.CommandRecord, 0, len(order))
	for _, s := range order {
		if *seq == 0 || s == *seq {
			recs = append(recs, byStart[s])
		}
	}
	if *seq != 0 && len(recs) == 0 {
		fmt.Fprintf(e.Stderr, "crux: cmdlog show %s: no command with seq %d\n", run, *seq)
		return ExitError
	}
	if *asJSON {
		return e.writeJSON(recs)
	}
	for i, rec := range recs {
		if i > 0 {
			fmt.Fprintln(e.Stdout)
		}
		e.printCommand(rec)
	}
	return ExitOK
}

// cmdlogRun parses fs and returns the single run id argument.
func (e *Env) cmdlogRun(fs *flag.FlagSet, args []string) (string, int, bool) {
	run, code, ok := parseWithID(fs, args)
	if !ok {
		return "", code, false
	}
	if run == "" || code != ExitOK {
		fmt.Fprintln(e.Stderr, cmdlogUsage)
		return "", ExitUsage, false
	}
	return run, ExitOK, true
}

// printCommand writes one record in full.
func (e *Env) printCommand(rec api.CommandRecord) {
	w := e.Stdout
	fmt.Fprintf(w, "seq %d  %s  capture=%s\n", rec.Seq, rec.Status, rec.Capture)
	fmt.Fprintf(w, "command: %s\n", strings.Join(rec.Command, " "))
	if rec.Cwd != "" {
		fmt.Fprintf(w, "cwd:     %s\n", rec.Cwd)
	}
	fmt.Fprintf(w, "started: %s\n", rec.StartedAt.UTC().Format(time.RFC3339Nano))
	if rec.EndedAt != nil {
		fmt.Fprintf(w, "ended:   %s (%s)\n", rec.EndedAt.UTC().Format(time.RFC3339Nano), durationText(rec.DurationMs))
	}
	fmt.Fprintf(w, "exit:    %s\n", exitText(rec.ExitCode))
	if rec.Reason != "" {
		fmt.Fprintf(w, "reason:  %s\n", rec.Reason)
	}
	e.printStream("stdout", rec.Stdout)
	e.printStream("stderr", rec.Stderr)
}

func (e *Env) printStream(name string, out *api.CommandOutput) {
	if out == nil {
		return
	}
	fmt.Fprintf(e.Stdout, "%s:  %d bytes\n", name, out.Bytes)
	if out.Head != "" {
		fmt.Fprintln(e.Stdout, indent(out.Head))
	}
	if out.Truncated {
		fmt.Fprintf(e.Stdout, "    [… %d bytes not kept …]\n", out.Bytes-int64(len(out.Head))-int64(len(out.Tail)))
		fmt.Fprintln(e.Stdout, indent(out.Tail))
	}
}

// indent prefixes each line of text with four spaces.
func indent(text string) string {
	return "    " + strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\n", "\n    ")
}

func exitText(code *int) string {
	if code == nil {
		return "-"
	}
	return strconv.Itoa(*code)
}

func durationText(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return (time.Duration(*ms) * time.Millisecond).String()
}

func bytesText(out *api.CommandOutput) string {
	if out == nil {
		return "-"
	}
	return strconv.FormatInt(out.Bytes, 10)
}
