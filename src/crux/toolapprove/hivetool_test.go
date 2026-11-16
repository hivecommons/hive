package toolapprove

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
)

// TestHiveToolClasses pins the hive-tool/v1 (RFC #18464) classes: reads are
// read-only, issue writes are issue operations, PR writes are PR operations.
func TestHiveToolClasses(t *testing.T) {
	for _, tool := range []string{"issue.read", "repo.list_files", "repo.read_file", "repo.search_code", "checks.read", "task.complete", "task.abort"} {
		if !IsReadOnly(tool) || IsGitHubWrite(tool) || !IsHiveTool(tool) {
			t.Errorf("%s: want read-only hive tool", tool)
		}
	}
	for _, tool := range []string{"issue.comment", "issue.label"} {
		if IsReadOnly(tool) || !IsGitHubWrite(tool) || !IsGitHubIssueOperation(tool) || IsGitHubPROperation(tool) {
			t.Errorf("%s: want issue write", tool)
		}
	}
	for _, tool := range []string{"repo.patch_files", "pr.create", "pr.comment"} {
		if IsReadOnly(tool) || !IsGitHubWrite(tool) || !IsGitHubPROperation(tool) || IsGitHubIssueOperation(tool) {
			t.Errorf("%s: want PR write", tool)
		}
	}
	// Hive-mediated PR creation is not the hard-denied direct path.
	if IsDirectPRCreation("pr.create") || IsDirectPRMerge("pr.create") {
		t.Error("pr.create must not be classified as direct PR creation/merge")
	}
}

// TestHiveToolModeGatesHoldAtHighAutonomy is the regression the classes
// exist for: before them, an ADVISORY agent's pr.create / issue.comment
// reached the ACMM ladder and auto-approved at L5/L6 on a green scan.
func TestHiveToolModeGatesHoldAtHighAutonomy(t *testing.T) {
	ctx := context.Background()
	for _, level := range []int{5, 6} {
		advisory := AgentIdentity{Name: "free-tier", Mode: agentmode.ModeAdvisory}
		for _, tool := range []string{"issue.comment", "issue.label", "pr.create", "repo.patch_files", "pr.comment"} {
			if v := Decide(ctx, ToolRequest{Tool: tool}, level, advisory); !v.Denied() {
				t.Errorf("L%d advisory %s = %s, want deny", level, tool, v.Decision)
			}
		}
		issuesOnly := AgentIdentity{Name: "triage", Mode: agentmode.ModeIssuesOnly}
		for _, tool := range []string{"pr.create", "repo.patch_files", "pr.comment"} {
			if v := Decide(ctx, ToolRequest{Tool: tool}, level, issuesOnly); !v.Denied() {
				t.Errorf("L%d issues-only %s = %s, want deny", level, tool, v.Decision)
			}
		}
		if v := Decide(ctx, ToolRequest{Tool: "issue.comment"}, level, issuesOnly); v.Denied() {
			t.Errorf("L%d issues-only issue.comment denied: %s", level, v.Rationale)
		}
	}
	// Reads auto-approve at every level and mode (they were operator-gated
	// at L1-L4 before, which stalls a hosted session on every file read).
	for level := 0; level <= 6; level++ {
		v := Decide(ctx, ToolRequest{Tool: "repo.read_file"}, level, AgentIdentity{Name: "r"})
		if !v.AutoApproved() {
			t.Errorf("L%d repo.read_file = %s, want auto-approve", level, v.Decision)
		}
	}
}
