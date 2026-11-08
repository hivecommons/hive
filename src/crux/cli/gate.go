package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// `crux gate open|list|answer|wait` is the human-gate work service
// (hivecommons/crux#114 S3). It does not touch crux.Runtime and, like
// `runs`, exists only against a `crux serve`: the server is the one
// authority that records who opened and who answered, journals the
// decision and publishes gate.* events, so a second in-process writer on
// the same work dir would undercut all three.

const gateUsage = `usage: crux --server ADDR gate open --subject S [--choice C]… [--timeout D] [--context JSON] [--opened-by L] [--idempotency-key K] [--run RUN] [--json]
       crux --server ADDR gate list [--status waiting|answered|expired] [--json]
       crux --server ADDR gate answer <id> --choice C [--note TEXT] [--as-operator] [--json]
       crux --server ADDR gate wait <id> [--timeout D] [--json]

open prints the gate id; choices default to approve and deny, and timeout
0 (the default) waits indefinitely. --run ties the gate to a contained
run the caller owns: opening, answering and expiry become that run's
needs-input, input and blocked turn signals. answer records one of the
offered choices as the operator; --as-operator is the audited override
that lets the principal that opened a gate answer it. wait blocks until
the gate is answered (prints the answer, exit 0) or expires (exit 1,
nothing printed): expiry is neither approval nor denial.`

// remoteGate is `crux --server … gate`.
func (e *Env) remoteGate(ctx context.Context, c *api.Client, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, gateUsage)
		return ExitUsage
	}
	switch args[0] {
	case "open":
		return e.remoteGateOpen(ctx, c, args[1:])
	case "list":
		return e.remoteGateList(ctx, c, args[1:])
	case "answer":
		return e.remoteGateAnswer(ctx, c, args[1:])
	case "wait":
		return e.remoteGateWait(ctx, c, args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(e.Stderr, gateUsage)
		return ExitOK
	default:
		fmt.Fprintf(e.Stderr, "crux: gate: unknown subcommand %q\n%s\n", args[0], gateUsage)
		return ExitUsage
	}
}

func (e *Env) remoteGateOpen(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("gate open")
	var req api.GateRequest
	var choices stringList
	var payload string
	fs.StringVar(&req.Subject, "subject", "", "what the person is asked to decide")
	fs.Var(&choices, "choice", "an answer the person may give (repeatable; default approve, deny)")
	fs.StringVar(&req.Timeout, "timeout", "0", "how long the gate waits, e.g. 24h; 0 waits indefinitely")
	fs.StringVar(&payload, "context", "", "JSON payload shown beside the subject (at most 64 KiB)")
	fs.StringVar(&req.OpenedBy, "opened-by", "", "the driver's own label (default: the caller)")
	fs.StringVar(&req.IdempotencyKey, "idempotency-key", "", "identify the gate by this key instead of its contents")
	fs.StringVar(&req.Run, "run", "", "the contained run the gate holds up; its turn signals record the gate")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if strings.TrimSpace(req.Subject) == "" || fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, gateUsage)
		return ExitUsage
	}
	if payload != "" {
		if !json.Valid([]byte(payload)) {
			fmt.Fprintln(e.Stderr, "crux: gate open: --context is not valid JSON")
			return ExitUsage
		}
		req.Context = json.RawMessage(payload)
	}
	req.Choices = choices
	g, err := c.OpenGate(ctx, req)
	if err != nil {
		return e.gateErr("open", err)
	}
	if *asJSON {
		return e.writeJSON(g)
	}
	fmt.Fprintf(e.Stdout, "%s\t%s\n", g.ID, g.Status)
	return ExitOK
}

func (e *Env) remoteGateList(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("gate list")
	status := fs.String("status", "", "only gates in this status: waiting, answered or expired")
	asJSON := fs.Bool("json", false, "print a JSON array instead of a table")
	if code, ok := parseSub(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.Stderr, gateUsage)
		return ExitUsage
	}
	list, err := c.ListGates(ctx, *status)
	if err != nil {
		return e.gateErr("list", err)
	}
	if *asJSON {
		return e.writeJSON(list)
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tOPENED BY\tOPENED\tANSWER\tCHOICES\tSUBJECT")
	for _, g := range list {
		answer := "-"
		if g.Answer != "" {
			answer = g.Answer + " (" + g.AnsweredBy + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.ID, g.Status, g.OpenedByPrincipal, g.OpenedAt.UTC().Format(time.RFC3339), answer, strings.Join(g.Choices, "|"), g.Subject)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(e.Stderr, "crux:", err)
		return ExitError
	}
	return ExitOK
}

func (e *Env) remoteGateAnswer(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("gate answer")
	var in api.GateAnswerRequest
	fs.StringVar(&in.Answer, "choice", "", "one of the gate's choices")
	fs.StringVar(&in.Note, "note", "", "free text recorded beside the choice")
	fs.BoolVar(&in.AsOperator, "as-operator", false, "audited override: answer a gate this principal opened")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	id, code, ok := parseWithID(fs, args)
	if !ok {
		return code
	}
	if id == "" || in.Answer == "" {
		fmt.Fprintln(e.Stderr, gateUsage)
		return ExitUsage
	}
	g, err := c.AnswerGate(ctx, id, in)
	if err != nil {
		return e.gateErr("answer", err)
	}
	if *asJSON {
		return e.writeJSON(g)
	}
	fmt.Fprintf(e.Stdout, "%s\t%s\t%s\n", g.ID, g.Status, g.Answer)
	return ExitOK
}

// remoteGateWait polls the long-poll route until the gate leaves waiting
// or --timeout passes; each poll is bounded server-side.
func (e *Env) remoteGateWait(ctx context.Context, c *api.Client, args []string) int {
	fs := e.subFlags("gate wait")
	timeout := fs.Duration("timeout", 0, "give up after this long (default: wait indefinitely)")
	asJSON := fs.Bool("json", false, "print the record as JSON")
	id, code, ok := parseWithID(fs, args)
	if !ok {
		return code
	}
	if id == "" {
		fmt.Fprintln(e.Stderr, gateUsage)
		return ExitUsage
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	for {
		g, err := c.WaitGate(ctx, id, 0)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintf(e.Stderr, "crux: gate wait: %s is still waiting\n", id)
				return ExitError
			}
			return e.gateErr("wait", err)
		}
		if g.Status == api.GateWaiting {
			continue
		}
		if *asJSON {
			if code := e.writeJSON(g); code != ExitOK {
				return code
			}
		} else if g.Status == api.GateAnswered {
			fmt.Fprintln(e.Stdout, g.Answer)
		}
		if g.Status != api.GateAnswered {
			fmt.Fprintf(e.Stderr, "crux: gate wait: %s %s without an answer\n", g.ID, g.Status)
			return ExitError
		}
		return ExitOK
	}
}

// parseWithID accepts the gate id before or after the flags.
func parseWithID(fs *flag.FlagSet, args []string) (id string, code int, ok bool) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if code, ok := parseSub(fs, args); !ok {
		return "", code, false
	}
	switch {
	case id == "" && fs.NArg() == 1:
		id = fs.Arg(0)
	case fs.NArg() != 0:
		return "", ExitUsage, true
	}
	return id, ExitOK, true
}

// gateErr prints a failure; refusals the caller can fix are usage errors,
// everything else a runtime failure.
func (e *Env) gateErr(verb string, err error) int {
	fmt.Fprintf(e.Stderr, "crux: gate %s: %v\n", verb, err)
	var se *api.StatusError
	if errors.As(err, &se) {
		switch se.Body.Code {
		case api.CodeBadRequest, api.CodeForbidden, api.CodeGateInvalidChoice, api.CodeGateSelfAnswer, api.CodeGateContextTooLarge:
			return ExitUsage
		}
	}
	if api.IsUnauthorized(err) {
		fmt.Fprintf(e.Stderr, "crux: the server wants a bearer token; pass --token or set %s\n", config.ServeTokenEnvVar)
	}
	return ExitError
}
