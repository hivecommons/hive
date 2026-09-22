package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestTaskMCPPromptPointerGatedByURL(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hivecommons/hive"}, PrimaryRepo: "hive"},
		Agents:  map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
	}
	actionable := &github.ActionableResult{Issues: github.IssueResultFromItems([]github.Issue{{Repo: "hivecommons/hive", Number: 8033, Title: "task MCP scope"}})}

	without := New(cfg, testLogger()).BuildAgentMessage("scanner", actionable.Issues.Items, actionable)
	if strings.Contains(without, "hive-task") || strings.Contains(without, "context_bundle") {
		t.Fatalf("prompt without URL contains task MCP pointer:\n%s", without)
	}

	s := New(cfg, testLogger())
	s.SetTaskMCPURL("http://hub/api/contribute/mcp")
	with := s.BuildAgentMessage("scanner", actionable.Issues.Items, actionable)
	if !strings.Contains(with, "`hive-task` MCP server") || !strings.Contains(with, "Call `context_bundle` first") {
		t.Fatalf("prompt with URL missing task MCP pointer:\n%s", with)
	}
}
