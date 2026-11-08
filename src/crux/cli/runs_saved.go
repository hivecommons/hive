package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/crux/api"
)

// `crux runs list|restart|history`: the saved-run-results verbs (RFC
// crux#114 S7). Like the rest of the family they exist only against a
// server.

func (e *Env) remoteRunsList(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("runs list")
	status := fs.String("status", "", "only runs in this status")
	limit := fs.Int("limit", 0, "at most this many runs, newest first (server default when 0)")
	asJSON := fs.Bool("json", false, "print the list as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR runs list [--status S] [--limit N] [--json]")
		return ExitUsage
	}
	list, err := c.ListRuns(ctx, *status, *limit)
	if err != nil {
		return e.remoteErr("runs list", "", err)
	}
	if *asJSON {
		return e.printJSON(list)
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tOWNER\tPROFILE\tCREATED\tCOMMAND")
	for _, run := range list.Runs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", run.ID, run.Status, run.Owner, run.Profile, run.CreatedAt.UTC().Format(time.RFC3339), strings.Join(run.Command, " "))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	return ExitOK
}

func (e *Env) remoteRunsRestart(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("runs restart")
	var req api.RunRestartRequest
	fs.StringVar(&req.Reason, "reason", "", "why the run is re-run; recorded and audited")
	fs.StringVar(&req.IdempotencyKey, "idempotency-key", "", "return the restart an earlier identical request started")
	fs.BoolVar(&req.Detach, "detach", false, "return the accepted record instead of waiting")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR runs restart [--reason TEXT] [--idempotency-key KEY] [--detach] [--json] <id>")
		return ExitUsage
	}
	run, err := c.RestartRun(ctx, fs.Arg(0), req)
	if err != nil {
		return e.remoteErr("runs restart", fs.Arg(0), err)
	}
	if !*asJSON {
		fmt.Fprintf(e.Stderr, "crux: run %s restarted as %s\n", fs.Arg(0), run.ID)
	}
	return e.printRun(run, *asJSON)
}

func (e *Env) remoteRunsHistory(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("runs history")
	asJSON := fs.Bool("json", false, "print the history as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(e.Stderr, "usage: crux --server ADDR runs history [--json] <id>")
		return ExitUsage
	}
	hist, err := c.RunHistory(ctx, fs.Arg(0))
	if err != nil {
		return e.remoteErr("runs history", fs.Arg(0), err)
	}
	if *asJSON {
		return e.printJSON(hist)
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tSTATUS\tREASON")
	for _, tr := range hist.Transitions {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", tr.At.UTC().Format(time.RFC3339), tr.Status, tr.Reason)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	return ExitOK
}
