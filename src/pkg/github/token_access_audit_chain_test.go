package github

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/auditchain"
)

// #19439: every row the ingester writes is hash-chained with pkg/auditchain,
// rows written before the chain existed stay as legacy rows, the chain
// resumes across a restart, and an event cannot choose its own chain fields.
func TestTokenAccessAudit_IngestHashChainsRows(t *testing.T) {
	spool, logPath := testTokenAccessPaths(t)
	if !PrepareTokenAccessAudit(quietLogger()) {
		t.Fatal("PrepareTokenAccessAudit returned false")
	}
	legacy := `{"ts":"2026-09-01T00:00:00Z","agent":"guide","uid":1201,"op":"gh","cmd":"gh pr list"}` + "\n"
	if err := os.WriteFile(logPath, []byte(legacy), tokenAccessLogMode); err != nil {
		t.Fatal(err)
	}

	dropTokenAccessEvent(t, spool, "1000000000000000001-11.json",
		`{"ts":"2026-09-08T00:00:00Z","agent":"scanner","op":"gh","cmd":"gh pr view 1"}`)
	// An agent must not be able to pick its own place in the chain.
	dropTokenAccessEvent(t, spool, "1000000000000000002-12.json",
		`{"ts":"2026-09-08T00:00:01Z","agent":"scanner","op":"gh","cmd":"gh pr view 2","seq":99,"prev_hash":"abc","row_hash":"def"}`)
	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, time.Now()); n != 2 {
		t.Fatalf("ingested %d events, want 2", n)
	}

	// Simulate a restart: the head must be reloaded from the file, with no
	// second chain start.
	tokenAccessChain.mu.Lock()
	delete(tokenAccessChain.heads, logPath)
	tokenAccessChain.mu.Unlock()
	dropTokenAccessEvent(t, spool, "1000000000000000003-13.json",
		`{"ts":"2026-09-08T00:00:02Z","agent":"scanner","op":"git-credential","host":"github.com"}`)
	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, time.Now()); n != 1 {
		t.Fatalf("ingested %d events after restart, want 1", n)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	report := auditchain.VerifyLines(logPath, data)
	if !report.OK() {
		t.Fatalf("chain does not verify: %+v", report.Problems)
	}
	if report.Legacy != 1 || report.Chained != 3 || report.FirstSeq != 1 || report.Anchor != "" || report.Head.Seq != 3 {
		t.Fatalf("report = %+v, want 1 legacy row then a genesis-anchored chain of 3", report)
	}
	lines := readLogLines(t, logPath)
	if got := lines[2]["seq"]; got != float64(2) {
		t.Fatalf("agent-supplied seq kept: %v, want 2", got)
	}
	if got := lines[2]["prev_hash"]; got != lines[1]["row_hash"] {
		t.Fatalf("agent-supplied prev_hash kept: %v", got)
	}
	if got := lines[2]["cmd"]; got != "gh pr view 2" {
		t.Fatalf("event payload not preserved: cmd = %v", got)
	}
	assertLogNotAgentWritable(t, logPath)

	// Editing an ingested row is detected.
	edited := bytes.Replace(data, []byte("gh pr view 1"), []byte("gh pr view 7"), 1)
	if r := auditchain.VerifyLines(logPath, edited); r.OK() {
		t.Fatal("edited row verified as intact")
	}
}

// A log rewritten behind the hive's back must not be extended as if it were
// genuine: the next row links to the head the hive remembers, so the rewrite
// shows up as a broken link.
func TestTokenAccessAudit_RewrittenLogBreaksChain(t *testing.T) {
	spool, logPath := testTokenAccessPaths(t)
	if !PrepareTokenAccessAudit(quietLogger()) {
		t.Fatal("PrepareTokenAccessAudit returned false")
	}
	for _, name := range []string{"1000000000000000001-11.json", "1000000000000000002-12.json"} {
		dropTokenAccessEvent(t, spool, name, `{"ts":"2026-09-08T00:00:00Z","agent":"scanner","op":"gh","cmd":"gh pr merge 1"}`)
	}
	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, time.Now()); n != 2 {
		t.Fatalf("ingested %d events, want 2", n)
	}
	// Someone with write access to the file drops both rows.
	if err := os.WriteFile(logPath, nil, tokenAccessLogMode); err != nil {
		t.Fatal(err)
	}
	dropTokenAccessEvent(t, spool, "1000000000000000003-13.json", `{"ts":"2026-09-08T00:00:02Z","agent":"scanner","op":"gh","cmd":"gh pr list"}`)
	if n := IngestTokenAccessEventsOnce(quietLogger(), spool, logPath, time.Now()); n != 1 {
		t.Fatalf("ingested %d events, want 1", n)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	report := auditchain.VerifyLines(logPath, data)
	if report.FirstSeq != 3 || report.Anchor == "" {
		t.Fatalf("report = %+v, want the surviving row at seq 3 still linking to the dropped rows", report)
	}
}
