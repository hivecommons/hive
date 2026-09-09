package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func scopedProject(scopes map[string][]string) ProjectContext {
	p := ProjectContext{
		Org:             "testorg",
		Repos:           []string{"console", "dashboard", "docs"},
		PrimaryRepoName: "console",
		ACMMLevel:       4,
		PRsAllowed:      true,
	}
	if len(scopes) == 0 {
		return p
	}
	p.AgentRepos = func(agent string) []string {
		scope, ok := scopes[agent]
		if !ok {
			return p.Repos
		}
		out := make([]string, 0, len(p.Repos))
		for _, r := range p.Repos {
			for _, s := range scope {
				if r == s {
					out = append(out, r)
				}
			}
		}
		return out
	}
	p.AgentPrimaryRepo = func(agent string) string {
		if repos := p.AgentRepos(agent); len(repos) > 0 {
			return repos[0]
		}
		return ""
	}
	return p
}

func TestProjectContext_ReposForFallsBackWhenUnwired(t *testing.T) {
	p := scopedProject(nil)
	if got := p.ReposFor("anything"); len(got) != 3 {
		t.Fatalf("ReposFor with no scope function = %v, want the hive list", got)
	}
	if got := p.PrimaryRepoFor("anything"); got != "console" {
		t.Errorf("PrimaryRepoFor with no scope function = %q, want console", got)
	}
}

func TestProjectContext_ReposForScopedAgent(t *testing.T) {
	p := scopedProject(map[string][]string{"schema": {"dashboard"}})

	if got := p.ReposFor("schema"); len(got) != 1 || got[0] != "dashboard" {
		t.Errorf("ReposFor(schema) = %v, want [dashboard]", got)
	}
	if got := p.ReposFor("scanner"); len(got) != 3 {
		t.Errorf("ReposFor(unscoped) = %v, want all three", got)
	}
	// $HIVE_REPO must point a specialist at a repo it may actually write to.
	if got := p.PrimaryRepoFor("schema"); got != "dashboard" {
		t.Errorf("PrimaryRepoFor(schema) = %q, want dashboard", got)
	}
	if got := p.PrimaryRepoFor("scanner"); got != "console" {
		t.Errorf("PrimaryRepoFor(unscoped) = %q, want console", got)
	}
	// The hive's own repo list is untouched: the scope narrows a view, not the
	// hive.
	if len(p.Repos) != 3 {
		t.Errorf("Repos was mutated to %v", p.Repos)
	}
}

func TestAgentEnvPairs_QualifiesScopedRepoEnv(t *testing.T) {
	tests := []struct {
		name      string
		repos     []string
		wantRepo  string
		wantRepos string
	}{
		{
			name:      "cross-org primary is not double-qualified",
			repos:     []string{"laredo/cuga-agent"},
			wantRepo:  "laredo/cuga-agent",
			wantRepos: "laredo/cuga-agent",
		},
		{
			name:      "same-org short name is qualified",
			repos:     []string{"console"},
			wantRepo:  "testorg/console",
			wantRepos: "testorg/console",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ProjectContext{
				Org:             "testorg",
				Repos:           tt.repos,
				PrimaryRepoName: "dashboard",
				ACMMLevel:       4,
				AgentRepos:      func(string) []string { return tt.repos },
				AgentPrimaryRepo: func(string) string {
					return tt.repos[0]
				},
			}
			m := NewManager(map[string]config.AgentConfig{
				"schema": {Backend: "claude", Model: "claude-sonnet-5"},
			}, discardLogger(), p)
			m.mu.RLock()
			agent := m.agents["schema"]
			m.mu.RUnlock()
			if agent == nil {
				t.Fatal("fixture agent not registered")
			}

			env := map[string]string{}
			for _, pair := range m.agentEnvPairs(agent) {
				env[pair.Key] = pair.Value
			}
			if got := env["HIVE_REPO"]; got != tt.wantRepo {
				t.Errorf("HIVE_REPO = %q, want %q", got, tt.wantRepo)
			}
			if got := env["HIVE_REPOS"]; got != tt.wantRepos {
				t.Errorf("HIVE_REPOS = %q, want %q", got, tt.wantRepos)
			}
		})
	}
}
