package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestBuildAgentMessageWithKickTemplate(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	os.MkdirAll(agentsDir, 0755)
	os.WriteFile(filepath.Join(agentsDir, "custom-kick.md"), []byte("Review all open issues for ${ORG}/${REPO}"), 0644)

	level := 1
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {Role: "scanner", KickTemplate: "custom-kick.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, slog.Default())

	msg := s.BuildAgentMessage("scanner", nil, nil)
	if msg == "" {
		t.Error("should use kick_template and return non-empty")
	}
	if len(msg) > 0 && !contains(msg, "[agent:scanner]") {
		t.Error("should contain agent header")
	}
	if !contains(msg, "Do not run `gh pr list`, `gh issue list`, or `gh search` to discover work") {
		t.Fatalf("custom kick template should receive shared governor work-source rule:\n%s", msg)
	}
	if strings.Index(msg, "Do not run `gh pr list`") > strings.Index(msg, "Review all open issues") {
		t.Fatalf("shared governor work-source rule should precede custom template body:\n%s", msg)
	}
}

func TestBuildAgentMessageWithPromptTemplate(t *testing.T) {
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	os.MkdirAll(agentsDir, 0755)
	os.WriteFile(filepath.Join(agentsDir, "helper.md"), []byte("Help with ${REPO} issues"), 0644)

	level := 1
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Agents: map[string]config.AgentConfig{
			"helper": {Role: "helper"},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, slog.Default())

	msg := s.BuildAgentMessage("helper", nil, nil)
	if msg == "" {
		t.Error("should use prompt template and return non-empty")
	}
}

func TestBuildAgentMessageWithACMMPackTemplate(t *testing.T) {
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	os.MkdirAll(agentsDir, 0755)

	level := 2
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Agents: map[string]config.AgentConfig{
			"scanner": {Role: "scanner"},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, slog.Default())

	msg := s.BuildAgentMessage("scanner", nil, &ghpkg.ActionableResult{
		Issues: ghpkg.IssueResult{Count: 5},
		PRs:    ghpkg.PRResult{Count: 3},
	})
	if msg == "" {
		t.Error("should return non-empty message")
	}
}

func TestLoadNamedTemplateFromLocalDir(t *testing.T) {
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	os.MkdirAll(agentsDir, 0755)
	os.WriteFile(filepath.Join(agentsDir, "test-template.md"), []byte("template content"), 0644)

	cfg := &config.Config{
		Project:  config.ProjectConfig{Org: "testorg", Name: "test"},
		Agents:   map[string]config.AgentConfig{},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, slog.Default())

	result := s.loadNamedTemplate("test-template.md")
	if result != "template content" {
		t.Errorf("expected 'template content', got %q", result)
	}
}

func TestLoadNamedTemplateNotFound(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "test"},
		Agents:  map[string]config.AgentConfig{},
	}
	s := New(cfg, slog.Default())

	result := s.loadNamedTemplate("nonexistent-template.md")
	// May return empty or embedded default
	_ = result
}

func TestLoadPromptTemplateFromLocalDir(t *testing.T) {
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	os.MkdirAll(agentsDir, 0755)
	os.WriteFile(filepath.Join(agentsDir, "my-agent.md"), []byte("agent prompt"), 0644)

	cfg := &config.Config{
		Project:  config.ProjectConfig{Org: "testorg", Name: "test"},
		Agents:   map[string]config.AgentConfig{},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	s := New(cfg, slog.Default())

	result := s.loadPromptTemplate("my-agent")
	if result != "agent prompt" {
		t.Errorf("expected 'agent prompt', got %q", result)
	}
}

func TestSubstituteTemplateVars(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo", "other"},
		},
		Agents: map[string]config.AgentConfig{},
	}
	s := New(cfg, slog.Default())

	template := "Review ${PROJECT_ORG}/${PROJECT_PRIMARY_REPO} — issues: ${QUEUE_ISSUES}, PRs: ${QUEUE_PRS}"
	result, _ := s.substituteTemplateWithPolicy(template, &ghpkg.ActionableResult{
		Issues: ghpkg.IssueResult{Count: 10},
		PRs:    ghpkg.PRResult{Count: 5},
	}, "scanner", nil)

	if !contains(result, "testorg") {
		t.Error("should substitute ORG")
	}
	if !contains(result, "testrepo") {
		t.Error("should substitute PROJECT_PRIMARY_REPO")
	}
	if !contains(result, "10") {
		t.Error("should substitute QUEUE_ISSUES")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstring(s, substr))
}

func findSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
