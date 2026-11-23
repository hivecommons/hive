package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/hivecommons/hive/pkg/auditchain"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// auditVerifySubcommand checks the audit log's hash chain (#19439):
//
//	hive audit-verify [--json] [--token-access] [path]
//
// path defaults to /data/audit.jsonl; its rotated backups are read too.
// --token-access checks the token-access log instead. Exit
// status 0 means every chained row verified, 1 that a problem was found, 2
// that the log could not be read.
const auditVerifySubcommand = "audit-verify"

// tokenAccessVerifyPath is the log --token-access checks; a var for tests.
var tokenAccessVerifyPath = github.TokenAccessLogPath

func runAuditVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(auditVerifySubcommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	tokenAccess := fs.Bool("token-access", false, "verify the token-access log ("+github.TokenAccessLogPath+") instead of the audit log")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: hive audit-verify [--json] [--token-access] [path]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || (*tokenAccess && fs.NArg() > 0) {
		fs.Usage()
		return 2
	}
	path := fs.Arg(0)
	if *tokenAccess {
		path = tokenAccessVerifyPath
	}
	report, err := dashboard.VerifyAuditLog(path)
	if err != nil {
		fmt.Fprintf(stderr, "audit-verify: %v\n", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			OK bool `json:"ok"`
			auditchain.Report
		}{report.OK(), report})
	} else {
		fmt.Fprintf(stdout, "rows: %d (%d chained, %d legacy before the chain)\n", report.Rows, report.Chained, report.Legacy)
		if report.Chained > 0 {
			fmt.Fprintf(stdout, "chain: seq %d..%d, head %s\n", report.FirstSeq, report.Head.Seq, report.Head.Hash)
			if report.Anchor != "" {
				fmt.Fprintf(stdout, "anchor: oldest row follows %s, which retention has rotated out\n", report.Anchor)
			}
		}
		for _, p := range report.Problems {
			fmt.Fprintf(stdout, "PROBLEM %s\n", p)
		}
		switch {
		case !report.OK():
		case *tokenAccess:
			// The hub holds no copy of this head yet, so a cut-off tail or a
			// rebuilt chain is not visible from here.
			fmt.Fprintln(stdout, "OK: chain intact.")
		default:
			fmt.Fprintln(stdout, "OK: chain intact. Compare the head with the hub's audit_chain_seq/audit_chain_head to rule out a truncated or rebuilt chain.")
		}
	}
	if !report.OK() {
		return 1
	}
	return 0
}
