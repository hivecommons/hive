package scheduler

import (
	"os"
	"path/filepath"
	"slices"
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

func TestTaskMCPDropStuffedContextGatesRefsOnlyAndIssueRefs(t *testing.T) {
	actionable := &github.ActionableResult{
		Issues: github.IssueResultFromItems([]github.Issue{{
			Repo: "hivecommons/hive", Number: 8261, Title: "drop stuffed issue title", AgeMinutes: 17, Labels: []string{"kind/feature"},
		}}),
		PRs: github.PRResult{
			Count: 1,
			Items: []github.PullRequest{{
				Repo: "hivecommons/hive", Number: 8262, Title: "drop stuffed pr title", Author: "alice",
			}},
		},
	}
	canaries := false
	baseCfg := &config.Config{
		Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hivecommons/hive"}, PrimaryRepo: "hive"},
		Agents:  map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
		Ioscan:  config.IoscanConfig{Canaries: &canaries},
	}
	defaultOff := New(baseCfg, testLogger())
	defaultOff.SetTaskMCPURL("http://hub/api/contribute/mcp")
	flagFalse := New(&config.Config{
		Project: baseCfg.Project,
		Agents:  map[string]config.AgentConfig{"scanner": {Backend: "claude", TaskMCP: &config.AgentTaskMCPConfig{DropStuffedContext: false}}},
		Ioscan:  config.IoscanConfig{Canaries: &canaries},
	}, testLogger())
	flagFalse.SetTaskMCPURL("http://hub/api/contribute/mcp")
	defaultMsg := defaultOff.BuildKickMessages(actionable, []string{"scanner"})[0]
	flagFalseMsg := flagFalse.BuildKickMessages(actionable, []string{"scanner"})[0]
	if defaultMsg.Message != flagFalseMsg.Message {
		t.Fatalf("explicit false changed prompt bytes:\n--- default ---\n%s\n--- flag false ---\n%s", defaultMsg.Message, flagFalseMsg.Message)
	}

	dropCfg := &config.Config{
		Project: baseCfg.Project,
		Agents:  map[string]config.AgentConfig{"scanner": {Backend: "claude", TaskMCP: &config.AgentTaskMCPConfig{DropStuffedContext: true}}},
		Ioscan:  config.IoscanConfig{Canaries: &canaries},
	}
	noURLFull := New(baseCfg, testLogger()).BuildKickMessages(actionable, []string{"scanner"})[0]
	noURLElisionRequested := New(dropCfg, testLogger()).BuildKickMessages(actionable, []string{"scanner"})[0]
	if noURLFull.Message != noURLElisionRequested.Message {
		t.Fatalf("drop_stuffed_context without task MCP URL changed prompt bytes")
	}

	withURL := New(dropCfg, testLogger())
	withURL.SetTaskMCPURL("http://hub/api/contribute/mcp")
	elided := withURL.BuildKickMessages(actionable, []string{"scanner"})[0]
	if !slices.Equal(defaultMsg.IssueRefs, elided.IssueRefs) {
		t.Fatalf("IssueRefs changed with elision: full=%v elided=%v", defaultMsg.IssueRefs, elided.IssueRefs)
	}
	for _, want := range []string{
		"Stuffed work lists in this prompt were elided to `repo#N` refs only",
		"`context_bundle` and `related_work` are the source of truth",
		"  hivecommons/hive#8261\n",
		"  hivecommons/hive#8262\n",
	} {
		if !strings.Contains(elided.Message, want) {
			t.Fatalf("elided prompt missing %q:\n%s", want, elided.Message)
		}
	}
	for _, forbidden := range []string{"17m", "kind/feature", "drop stuffed issue title", "by @alice", "drop stuffed pr title"} {
		if strings.Contains(elided.Message, forbidden) {
			t.Fatalf("elided prompt retained stuffed context %q:\n%s", forbidden, elided.Message)
		}
	}
	if !strings.Contains(defaultMsg.Message, "drop stuffed issue title") || !strings.Contains(defaultMsg.Message, "drop stuffed pr title") {
		t.Fatalf("flag-off prompt did not include full stuffed context:\n%s", defaultMsg.Message)
	}
	t.Logf("task MCP drop_stuffed_context test byte count: full=%d elided=%d", len(defaultMsg.Message), len(elided.Message))
}

func TestTaskMCPElidesTemplateMergeEligibleToRefsOnly(t *testing.T) {
	oldPath := mergeEligiblePath
	dir := t.TempDir()
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	t.Cleanup(func() { mergeEligiblePath = oldPath })
	if err := os.WriteFile(mergeEligiblePath, []byte(`{"merge_eligible":[{"number":7,"repo":"hivecommons/hive","title":"ready to merge title","queued":true}]}`), 0o600); err != nil {
		t.Fatalf("write merge eligible fixture: %v", err)
	}
	s := New(&config.Config{
		Project: config.ProjectConfig{Org: "hivecommons", Repos: []string{"hivecommons/hive"}, PrimaryRepo: "hive"},
		Agents:  map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
	}, testLogger())
	actionable := &github.ActionableResult{}
	full, failClosed := s.substituteTemplateWithPolicy("${MERGE_ELIGIBLE}", actionable, "scanner", nil)
	if failClosed {
		t.Fatal("full merge-eligible substitution fail-closed")
	}
	elided, failClosed := s.substituteTemplateWithPolicy("${MERGE_ELIGIBLE}", actionable, "scanner", nil, true)
	if failClosed {
		t.Fatal("elided merge-eligible substitution fail-closed")
	}
	if !strings.Contains(full, "#7 hivecommons/hive [queued for auto-merge] — ready to merge title") {
		t.Fatalf("full merge-eligible list missing stuffed context: %q", full)
	}
	if elided != "  hivecommons/hive#7\n" {
		t.Fatalf("elided merge-eligible list = %q, want ref only", elided)
	}
}
