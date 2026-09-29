package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func gitIdentityPairs(pairs []agentEnvPair) map[string]string {
	got := map[string]string{}
	for _, p := range pairs {
		switch p.Key {
		case "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL":
			got[p.Key] = p.Value
		}
	}
	return got
}

// Each per-UID lane must commit as itself, pinned in the environment so a
// shared ~/.gitconfig or repo-local user.* write by another agent cannot
// re-attribute its commits (#9478).
func TestAgentEnvPairsPinGitIdentityPerLane(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "")
	m := testManager(2)

	for _, name := range []string{"scanner", "sec-check"} {
		agent := &AgentProcess{
			Name:   name,
			UID:    1001,
			Config: config.AgentConfig{Role: name, Backend: "claude", Model: "sonnet"},
		}
		got := gitIdentityPairs(m.agentEnvPairs(agent))
		wantEmail := name + "@hive.kubestellar.io"
		want := map[string]string{
			"GIT_AUTHOR_NAME":     name,
			"GIT_AUTHOR_EMAIL":    wantEmail,
			"GIT_COMMITTER_NAME":  name,
			"GIT_COMMITTER_EMAIL": wantEmail,
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
}

func TestAgentEnvPairsGitIdentityHonoursEmailDomain(t *testing.T) {
	t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", "hive.example.com")
	m := testManager(2)
	agent := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}

	got := gitIdentityPairs(m.agentEnvPairs(agent))
	if got["GIT_AUTHOR_EMAIL"] != "scanner@hive.example.com" || got["GIT_COMMITTER_EMAIL"] != "scanner@hive.example.com" {
		t.Errorf("git identity = %v, want scanner@hive.example.com", got)
	}
}

func TestAgentEnvPairsGitIdentityNotPinnedForSharedUID(t *testing.T) {
	m := testManager(2)
	agent := &AgentProcess{Name: "scanner", UID: 0, Config: config.AgentConfig{Backend: "claude"}}

	if got := gitIdentityPairs(m.agentEnvPairs(agent)); len(got) != 0 {
		t.Errorf("UID 0 agent should not pin a git identity, got %v", got)
	}
}

func TestAgentGitIdentity(t *testing.T) {
	tests := []struct {
		name      string
		agent     string
		domain    string
		wantEmail string
		wantOK    bool
	}{
		{"default domain", "scanner", "", "scanner@hive.kubestellar.io", true},
		{"custom domain", "guide-agent", "hive.example.com", "guide-agent@hive.example.com", true},
		{"unsafe domain falls back", "scanner", "evil.com\n[core]", "scanner@hive.kubestellar.io", true},
		{"unsafe agent name", "bad name", "", "", false},
		{"empty agent name", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HIVE_GIT_BOT_EMAIL_DOMAIN", tt.domain)
			name, email, ok := agentGitIdentity(tt.agent)
			if ok != tt.wantOK || email != tt.wantEmail {
				t.Fatalf("agentGitIdentity(%q) = (%q, %q, %v), want email %q ok %v", tt.agent, name, email, ok, tt.wantEmail, tt.wantOK)
			}
			if ok && name != tt.agent {
				t.Errorf("name = %q, want %q", name, tt.agent)
			}
		})
	}
}
