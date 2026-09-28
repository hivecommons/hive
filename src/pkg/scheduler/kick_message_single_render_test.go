package scheduler

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestBuildKickMessagesDoesNotDoubleRenderWithDropStuffedContext is a
// regression test for hivecommons/hive#9177: BuildKickMessages used to render
// the full (non-elided) kick message a second time purely to log a byte
// count when drop_stuffed_context was on. buildAgentMessage is not pure —
// ioscan.EnforceInput audits blocked/fail-closed findings and the classifier
// consumes its per-cycle budget — so the discarded second render doubled
// audit rows and classifier spend for a message that was never sent. Setting
// the task MCP URL (required for drop_stuffed_context to take effect at all)
// plus a blocking issue title must trigger exactly one audit call per kick.
func TestBuildKickMessagesDoesNotDoubleRenderWithDropStuffedContext(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "test-org", Repos: []string{"test-org/console"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", TaskMCP: &config.AgentTaskMCPConfig{DropStuffedContext: true}},
		},
	}
	s := New(cfg, testLogger())
	s.SetTaskMCPURL("http://hub/api/contribute/mcp")

	var auditCalls int
	s.SetAuditFunc(func(action, detail, agent string) { auditCalls++ })

	actionable := &github.ActionableResult{
		Issues: github.IssueResultFromItems([]github.Issue{{
			Repo: "test-org/console", Number: 1, Title: blockingTitle,
		}}),
	}

	msgs := s.BuildKickMessages(actionable, []string{"scanner"})
	if len(msgs) != 1 {
		t.Fatalf("got %d kick messages, want 1", len(msgs))
	}

	if auditCalls != 1 {
		t.Fatalf("audit calls = %d, want 1 (a discarded second full render would double ioscan audit rows)", auditCalls)
	}
}
