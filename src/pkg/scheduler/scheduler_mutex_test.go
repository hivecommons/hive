package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestGetSetPrimer(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if s.GetPrimer() != nil {
		t.Error("initial primer should be nil")
	}
	s.SetPrimer(nil)
	if s.GetPrimer() != nil {
		t.Error("primer should still be nil")
	}
}

func TestGetSetInception(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if s.GetInception() != nil {
		t.Error("initial inception should be nil")
	}
	s.SetInception(nil)
	if s.GetInception() != nil {
		t.Error("inception should still be nil")
	}
}

func TestGetSetLastActionable(t *testing.T) {
	s := New(&config.Config{}, slog.Default())
	if s.GetLastActionable() != nil {
		t.Error("initial lastActionable should be nil")
	}
	a := &github.ActionableResult{}
	s.SetLastActionable(a)
	if s.GetLastActionable() != a {
		t.Error("lastActionable should be set")
	}
	s.SetLastActionable(nil)
	if s.GetLastActionable() != nil {
		t.Error("lastActionable should be nil after reset")
	}
}

func TestBuildAgentMessageWithActionable(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org: "testorg", Repos: []string{"testrepo"}, PrimaryRepo: "testrepo", AIAuthor: "bot",
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {}, "quality": {}, "ci-maintainer": {}, "architect": {}, "outreach": {},
		},
	}
	s := New(cfg, slog.Default())
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 5, Items: []github.Issue{
			{Number: 1, Title: "test issue", Labels: []string{"kind/bug"}},
		}},
		PRs: github.PRResult{Count: 2},
	}

	for _, agent := range []string{"scanner", "quality", "ci-maintainer", "architect", "outreach"} {
		msg := s.BuildAgentMessage(agent, nil, actionable)
		if msg == "" {
			t.Errorf("BuildAgentMessage(%s) returned empty", agent)
		}
	}
}

func TestBuildKickMessagesMultiAgent(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org: "testorg", Repos: []string{"testrepo"}, PrimaryRepo: "testrepo", AIAuthor: "bot",
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {}, "quality": {},
		},
	}
	s := New(cfg, slog.Default())
	actionable := &github.ActionableResult{Issues: github.IssueResult{Count: 3}}

	msgs := s.BuildKickMessages(actionable, []string{"scanner", "quality"})
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2", len(msgs))
	}
}

func TestBuildAgentMessageFallback(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "testorg",
			Repos:       []string{"testrepo"},
			PrimaryRepo: "testrepo",
			AIAuthor:    "bot",
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "claude-sonnet-4-6"},
		},
	}
	s := New(cfg, slog.Default())
	msg := s.BuildAgentMessage("scanner", nil, nil)
	if msg == "" {
		t.Error("BuildAgentMessage should return non-empty fallback")
	}
}

func TestLoadNamedTemplate(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	os.MkdirAll(agentDir, 0o755)
	os.WriteFile(filepath.Join(agentDir, "test-agent.md"), []byte("hello ${AGENT_NAME}"), 0o644)

	cfg := &config.Config{
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, slog.Default())
	tmpl := s.loadNamedTemplate("test-agent.md")
	if tmpl != "hello ${AGENT_NAME}" {
		t.Errorf("loadNamedTemplate = %q", tmpl)
	}

	empty := s.loadNamedTemplate("nonexistent.md")
	if empty != "" {
		t.Errorf("nonexistent should return empty, got %q", empty)
	}
}

func TestBuildAgentMessageAllTypes(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org: "org", Repos: []string{"repo"}, PrimaryRepo: "repo", AIAuthor: "bot",
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {}, "quality": {}, "ci-maintainer": {},
			"architect": {}, "outreach": {}, "guide": {},
			"supervisor": {}, "sec-check": {}, "strategist": {},
		},
	}
	s := New(cfg, slog.Default())
	actionable := &github.ActionableResult{
		Issues: github.IssueResult{Count: 3, Items: []github.Issue{
			{Number: 1, Title: "bug", Labels: []string{"kind/bug"}},
		}},
		PRs: github.PRResult{Count: 1},
	}

	for name := range cfg.Agents {
		msg := s.BuildAgentMessage(name, nil, actionable)
		if msg == "" {
			t.Errorf("BuildAgentMessage(%q) empty", name)
		}
	}

	unknown := s.BuildAgentMessage("unknown-agent", nil, actionable)
	if unknown == "" {
		t.Error("unknown agent should get generic message")
	}
}

func TestKeywordSample(t *testing.T) {
	sample := keywordSample([]string{"a", "b", "c", "d", "e", "f"})
	if sample == "" {
		t.Error("keywordSample should return non-empty")
	}

	long := make([]string, 15)
	for i := range long {
		long[i] = "kw"
	}
	truncated := keywordSample(long)
	if strings.Count(truncated, "kw") != 8 {
		t.Errorf("should truncate to 8, got %q", truncated)
	}

	empty := keywordSample(nil)
	if empty != "" {
		t.Errorf("nil should return empty, got %q", empty)
	}
}

func TestIsNoiseLabelExtended(t *testing.T) {
	if !isNoiseLabel("kind/bug") {
		t.Error("kind/bug is noise")
	}
	if isNoiseLabel("priority/high") {
		t.Error("priority/high is not noise")
	}
}
