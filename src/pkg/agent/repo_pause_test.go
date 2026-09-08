package agent

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func pausedProject(paused ...string) ProjectContext {
	set := make(map[string]bool, len(paused))
	for _, r := range paused {
		set[r] = true
	}
	p := ProjectContext{
		Org:             "testorg",
		Repos:           []string{"console", "dashboard", "docs"},
		PrimaryRepoName: "console",
		ACMMLevel:       4,
		PRsAllowed:      true,
	}
	if len(set) > 0 {
		p.RepoPaused = func(repo string) bool { return set[repo] }
	}
	return p
}

// The work scope narrows; the hive's identity does not. $HIVE_REPO and the
// agent's workdir keep naming the primary repo even while it is paused —
// re-pointing them would be a far larger surprise than the pause itself.
func TestProjectContext_ActiveReposNarrowsScopeNotPrimary(t *testing.T) {
	p := pausedProject("console")

	got := p.ActiveRepos()
	if len(got) != 2 || got[0] != "dashboard" || got[1] != "docs" {
		t.Fatalf("ActiveRepos() = %v, want the unpaused repos in order", got)
	}
	if p.PrimaryRepo() != "console" {
		t.Errorf("PrimaryRepo() = %q, want console even though it is paused", p.PrimaryRepo())
	}
	if len(p.Repos) != 3 {
		t.Errorf("Repos was mutated to %v", p.Repos)
	}
}

func TestProjectContext_ActiveReposUnfilteredWithoutPredicate(t *testing.T) {
	p := pausedProject()
	got := p.ActiveRepos()
	if len(got) != 3 {
		t.Fatalf("ActiveRepos() = %v, want all three with no pause predicate", got)
	}
	if &got[0] != &p.Repos[0] {
		t.Error("ActiveRepos allocated a copy with nothing paused")
	}
}

// $HIVE_REPOS is the work scope templates iterate; $HIVE_REPO is identity.
// A pause narrows the former and must leave the latter alone — an agent whose
// workdir is a checkout of the primary repo still needs to be told which repo
// that is, even while the operator has it frozen.
//
// On v4 this behaviour was also asserted against buildProjectPreamble, whose
// AUTHORIZED REPOS text v5 removed from pkg/agent; that half of the coverage
// now lives in pkg/scheduler's repo_pause_test.go, which exercises the kick
// text that replaced it.
func TestAgentEnvPairs_HiveReposOmitsPausedRepos(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "claude-sonnet-5"},
	}, discardLogger(), pausedProject("dashboard"))
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if agent == nil {
		t.Fatal("fixture agent not registered")
	}

	env := map[string]string{}
	for _, pair := range m.agentEnvPairs(agent) {
		env[pair.Key] = pair.Value
	}

	if got := env["HIVE_REPOS"]; got != "testorg/console,testorg/docs" {
		t.Errorf("HIVE_REPOS = %q, want the paused repo omitted", got)
	}
	if got := env["HIVE_REPO"]; got != "testorg/console" {
		t.Errorf("HIVE_REPO = %q, want the primary repo — identity does not narrow", got)
	}
}

// Every repo paused leaves an empty work scope. HIVE_REPO still names the
// primary: the variable answers "which repo is this hive", not "what may you
// write to", and the deterministic refusals are what actually stop the writes.
func TestAgentEnvPairs_HiveReposAllPaused(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "claude-sonnet-5"},
	}, discardLogger(), pausedProject("console", "dashboard", "docs"))
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if agent == nil {
		t.Fatal("fixture agent not registered")
	}

	env := map[string]string{}
	for _, pair := range m.agentEnvPairs(agent) {
		env[pair.Key] = pair.Value
	}

	if got, ok := env["HIVE_REPOS"]; !ok || got != "" {
		t.Errorf("HIVE_REPOS = %q (present=%v), want empty when every repo is paused", got, ok)
	}
	if got := env["HIVE_REPO"]; got != "testorg/console" {
		t.Errorf("HIVE_REPO = %q, want the primary repo even with every repo paused", got)
	}
}
