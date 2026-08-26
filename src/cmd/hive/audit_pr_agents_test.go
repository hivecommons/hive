package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// auditPRAgents maps relay-opened PRs back to their authoring agent so the
// scheduler's fix-before-new section can route each red PR to the agent that
// created it (kubestellar/console 2026-08-26: ten red PRs, one commit each —
// no agent ever saw its own reds).
func TestAuditPRAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	now := time.Now().UTC()
	recent := now.Format(time.RFC3339)
	old := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	lines := `{"ts":"` + recent + `","user":"app","action":"agent_pr_created","detail":"repo=kubestellar/console, number=22815, author=bot, reused=false","agent":"scanner"}
{"ts":"` + recent + `","user":"app","action":"agent_pr_created","detail":"repo=console, number=7, author=bot","agent":"quality"}
{"ts":"` + recent + `","user":"app","action":"agent_issue_created","detail":"repo=kubestellar/console, number=99","agent":"scanner"}
{"ts":"` + old + `","user":"app","action":"agent_pr_created","detail":"repo=kubestellar/console, number=1, author=bot","agent":"scanner"}
{"ts":"` + recent + `","user":"app","action":"agent_pr_created","detail":"repo=kubestellar/console, number=8","agent":""}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}

	got := auditPRAgents("kubestellar", now.Add(-auditPRAttributionWindow), path)

	if got["kubestellar/console#22815"] != "scanner" {
		t.Errorf("full-repo entry: got %q", got["kubestellar/console#22815"])
	}
	// Bare repo names are org-qualified so lookups by fullRepo hit.
	if got["kubestellar/console#7"] != "quality" {
		t.Errorf("bare-repo entry not org-qualified: got %q", got["kubestellar/console#7"])
	}
	// Non-PR actions, entries outside the window, and agent-less entries are
	// all excluded.
	if _, ok := got["kubestellar/console#99"]; ok {
		t.Error("issue-created entry must not be mapped")
	}
	if _, ok := got["kubestellar/console#1"]; ok {
		t.Error("entry outside the attribution window must not be mapped")
	}
	if _, ok := got["kubestellar/console#8"]; ok {
		t.Error("agent-less entry must not be mapped")
	}
}
