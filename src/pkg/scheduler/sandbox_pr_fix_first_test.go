package scheduler

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Regression for hivecommons/hive#18575: a PR the hive opened from a
// sandboxed kick used to carry no agent credit, so red CI on it fell to
// scanner's FIX-BEFORE-NEW block and the sandboxed agent never saw it. The
// PR is credited through the same agent_pr_created record a relay-opened PR
// gets, the red-CI resolver reads it back, and the PR lands in the opening
// agent's block, not scanner's.
func TestSandboxOpenedPRAppearsInItsAgentsFixBeforeNew(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	client := github.NewClient("fake-token", "o", []string{"o/r"}, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	client.SetAttributionAuditRecord(func(rec github.AuditRecord) {
		line, err := json.Marshal(map[string]any{
			"ts": time.Now().UTC().Format(time.RFC3339), "user": "system",
			"action": rec.Action, "detail": rec.Detail, "agent": rec.Agent, "repo": rec.Repo, "target": rec.Target,
		})
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	})
	const head = "hive/dep-bump/20261116120000"
	// The step CreateAgentPR runs once GitHub has opened the PR.
	client.RecordAgentPROpened(github.InvocationMeta{Agent: "dep-bump", Backend: "claude"}, "o/r", head, "Bump torch", "Bumps torch.",
		github.CreatePRResult{Number: 42, URL: "https://github.com/o/r/pull/42", Author: "hive-app[bot]"})

	owner := github.AuthorAgentForPRFromAudit(auditPath, "o/r", 42)
	if owner != "dep-bump" {
		t.Fatalf("red-CI resolver credits the sandbox PR to %q, want dep-bump", owner)
	}
	signals := []github.CIFailureSignal{{Repo: "o/r", Number: 42, HeadSHA: "abc", Check: "build", Conclusion: "failure", Title: "Bump torch", Agent: owner, At: time.Now()}}
	block := formatRedPRFixDataWithSignals(nil, "dep-bump", signals)
	if !strings.Contains(block, "FIX-BEFORE-NEW") || !strings.Contains(block, "#42 o/r") || !strings.Contains(block, "build") {
		t.Fatalf("the sandboxed agent's block does not list its red PR:\n%s", block)
	}
	if got := formatRedPRFixDataWithSignals(nil, "scanner", signals); got != "" {
		t.Fatalf("scanner still gets the sandbox PR:\n%s", got)
	}
}
