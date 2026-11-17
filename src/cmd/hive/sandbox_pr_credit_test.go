package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// A sandbox PR's work branch is hive/<agent>/<stamp>. Before
// hivecommons/hive#18575 the head-ref fallback credited every one of them to
// an agent named "hive".
func TestPRFixAgentReadsSandboxWorkBranch(t *testing.T) {
	pr := github.PullRequest{HeadRef: "hive/dep-bump/20261116120000"}
	if got := prFixAgent(pr, ""); got != "dep-bump" {
		t.Fatalf("prFixAgent = %q, want dep-bump", got)
	}
	if got := prFixAgent(github.PullRequest{HeadRef: "quality/fix-1"}, ""); got != "quality" {
		t.Fatalf("lane branch fallback changed: %q", got)
	}
	if got := prFixAgent(pr, "audited"); got != "audited" {
		t.Fatalf("the audit record must win: %q", got)
	}
}

// The runtime's fallback credit (a PR client that cannot record it, such as
// the sidecar's) reaches the audit log through the sandbox audit callback,
// which writes the agent as both user and agent. The eval-tick resolver that
// builds ci-failing.json maps it to the agent.
func TestAuditPRAgentsReadsSandboxFallbackCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	now := time.Now().UTC()
	line := `{"ts":"` + now.Format(time.RFC3339) + `","user":"dep-bump","action":"` + github.AuditActionAgentPRCreated +
		`","detail":"repo=o/r, number=42, author=hive-app[bot], url=https://github.com/o/r/pull/42, reused=false, head=hive/dep-bump/20261116120000, via=sandbox, agent=dep-bump, backend=claude","agent":"dep-bump"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got := auditPRAgents("o", now.Add(-auditPRAttributionWindow), path)
	if got["o/r#42"] != "dep-bump" {
		t.Fatalf("auditPRAgents = %v", got)
	}
	if owner := github.AuthorAgentForPRFromAudit(path, "o/r", 42); owner != "dep-bump" {
		t.Fatalf("red-CI resolver = %q", owner)
	}
}
