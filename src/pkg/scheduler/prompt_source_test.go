package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/promptsrc"
)

type fakeFetcher struct {
	body string
	err  error
}

func (f fakeFetcher) GetFileContentRef(_ context.Context, _, _, _, _ string) (string, error) {
	return f.body, f.err
}

func newPromptSourceCfg(t *testing.T, extraAgent config.AgentConfig, allow bool, allowlist []string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Inline fallback template on disk for the agent.
	if err := os.WriteFile(filepath.Join(agentsDir, "gh-agent.md"), []byte("INLINE FALLBACK for ${REPO}"), 0o644); err != nil {
		t.Fatal(err)
	}
	extraAgent.KickTemplate = "gh-agent.md"
	return &config.Config{
		Project: config.ProjectConfig{Org: "testorg", Name: "t", PrimaryRepo: "r", Repos: []string{"r"}},
		Agents:  map[string]config.AgentConfig{"gh-agent": extraAgent},
		Policies: config.PoliciesConfig{LocalDir: dir},
		Variables: config.VariablesConfig{Security: config.VarSecurityConfig{
			AllowGitHubPrompt:     allow,
			GitHubPromptAllowlist: allowlist,
		}},
	}
}

// TestBuildAgentMessage_GitHubPromptWins: an allowlisted, reachable prompt source
// is used over the inline kick template.
func TestBuildAgentMessage_GitHubPromptWins(t *testing.T) {
	agent := config.AgentConfig{
		Role:         "gh-agent",
		PromptSource: &config.PromptSourceConfig{Owner: "testorg", Repo: "prompts", Path: "gh-agent.md"},
	}
	cfg := newPromptSourceCfg(t, agent, true, []string{"testorg/prompts"})
	s := New(cfg, slog.Default())
	s.SetGitHubPromptResolver(promptsrc.NewResolver(
		fakeFetcher{body: "REMOTE PROMPT from github"},
		func(slug string) bool { return cfg.GitHubPromptAllowed(slug) },
		nil,
	))

	msg := s.BuildAgentMessage("gh-agent", nil, nil)
	if !contains(msg, "REMOTE PROMPT from github") {
		t.Errorf("expected remote prompt to be used, got: %q", msg)
	}
	if contains(msg, "INLINE FALLBACK") {
		t.Error("inline fallback should not appear when remote source succeeds")
	}
}

// TestBuildAgentMessage_GitHubPromptDeniedFallsBack: a source whose repo is NOT
// allowlisted is ignored and the inline template is used — the fetch never runs.
func TestBuildAgentMessage_GitHubPromptDeniedFallsBack(t *testing.T) {
	agent := config.AgentConfig{
		Role:         "gh-agent",
		PromptSource: &config.PromptSourceConfig{Owner: "attacker", Repo: "evil", Path: "x.md"},
	}
	// Allowlist does NOT include attacker/evil.
	cfg := newPromptSourceCfg(t, agent, true, []string{"testorg/prompts"})
	s := New(cfg, slog.Default())
	s.SetGitHubPromptResolver(promptsrc.NewResolver(
		fakeFetcher{body: "SHOULD NEVER BE FETCHED"},
		func(slug string) bool { return cfg.GitHubPromptAllowed(slug) },
		nil,
	))

	msg := s.BuildAgentMessage("gh-agent", nil, nil)
	if contains(msg, "SHOULD NEVER BE FETCHED") {
		t.Fatal("denied repo must not be fetched (security gate)")
	}
	if !contains(msg, "INLINE FALLBACK") {
		t.Errorf("expected inline fallback, got: %q", msg)
	}
}

// TestBuildAgentMessage_GitHubPromptUnreachableFallsBack: an allowlisted source
// whose fetch errors (and has no cached value) falls back to the inline template
// rather than blanking or crashing the kick.
func TestBuildAgentMessage_GitHubPromptUnreachableFallsBack(t *testing.T) {
	agent := config.AgentConfig{
		Role:         "gh-agent",
		PromptSource: &config.PromptSourceConfig{Owner: "testorg", Repo: "prompts", Path: "gh-agent.md"},
	}
	cfg := newPromptSourceCfg(t, agent, true, []string{"testorg/prompts"})
	s := New(cfg, slog.Default())
	s.SetGitHubPromptResolver(promptsrc.NewResolver(
		fakeFetcher{err: errors.New("network down")},
		func(slug string) bool { return cfg.GitHubPromptAllowed(slug) },
		nil,
	))

	msg := s.BuildAgentMessage("gh-agent", nil, nil)
	if !contains(msg, "INLINE FALLBACK") {
		t.Errorf("unreachable source should fall back to inline, got: %q", msg)
	}
}

// TestBuildAgentMessage_NoResolverFallsBack: with no resolver wired, a prompt
// source is ignored and the inline template is used.
func TestBuildAgentMessage_NoResolverFallsBack(t *testing.T) {
	agent := config.AgentConfig{
		Role:         "gh-agent",
		PromptSource: &config.PromptSourceConfig{Owner: "testorg", Repo: "prompts", Path: "gh-agent.md"},
	}
	cfg := newPromptSourceCfg(t, agent, true, []string{"testorg/prompts"})
	s := New(cfg, slog.Default())
	// No SetGitHubPromptResolver call.

	msg := s.BuildAgentMessage("gh-agent", nil, nil)
	if !contains(msg, "INLINE FALLBACK") {
		t.Errorf("no resolver should fall back to inline, got: %q", msg)
	}
}
