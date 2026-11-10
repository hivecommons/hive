package config

// Governance tests for the `redteam` lane (hivecommons/hive#17420): an agent
// whose job is writing hostile text. Two invariants keep it a drill rather than
// an attack on the hive it serves — it ships switched off, and it can write
// nowhere until an operator names the repository it may write to.

import (
	"strings"
	"testing"
	"time"
)

func redteamIn(t *testing.T, level int) (PackAgent, map[string]map[string]string) {
	t.Helper()
	p, err := ACMMPackByLevel(level)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(%d): %v", level, err)
	}
	for _, a := range p.Agents {
		if a.Name == "redteam" {
			return a, p.Governor.Cadences
		}
	}
	return PackAgent{}, nil
}

// The lane ships at L5/L6 only, opt-in: paused in every governor mode, so no
// hive starts generating attack text because it moved up a level.
func TestRedTeamLaneShipsPausedAtHighTrustLevels(t *testing.T) {
	for _, lvl := range []int{1, 2, 3, 4} {
		if a, _ := redteamIn(t, lvl); a.Name != "" {
			t.Errorf("L%d pack defines redteam; the lane ships only at L5/L6", lvl)
		}
	}
	for _, lvl := range []int{5, 6} {
		a, cadences := redteamIn(t, lvl)
		if a.Name == "" {
			t.Fatalf("L%d pack is missing the redteam agent", lvl)
		}
		if a.Role != RedTeamRole {
			t.Errorf("L%d: role = %q, want %q — the scope guard keys on it", lvl, a.Role, RedTeamRole)
		}
		if a.KickTemplate != "redteam-holdgated.md" {
			t.Errorf("L%d: kick_template = %q, want redteam-holdgated.md at every level", lvl, a.KickTemplate)
		}
		if a.Mode == "ISSUES_PRS_MERGE" {
			t.Errorf("L%d: redteam must never hold merge rights", lvl)
		}
		if len(cadences) == 0 {
			t.Fatalf("L%d governor has no cadence modes", lvl)
		}
		for mode, c := range cadences {
			raw, ok := c["redteam"]
			if !ok {
				t.Errorf("L%d cadence %q has no redteam entry; an absent entry is not an explicit opt-in pause", lvl, mode)
				continue
			}
			if !NewIntervalCadence(raw).IsPaused() {
				t.Errorf("L%d cadence %q = %q; redteam must ship paused until an operator opts in", lvl, mode, raw)
			}
		}
		// The opt-in guide suggests a daily cadence; a run must not be
		// reaped as stale before it finishes.
		if time.Duration(a.StaleTimeout)*time.Second <= 24*time.Hour {
			t.Errorf("L%d: stale_timeout %ds must exceed the suggested 24h cadence", lvl, a.StaleTimeout)
		}
	}
}

// An unscoped red-team agent serves nothing: the proxy, the relays and kick
// assembly all ask AgentServesRepo, so this is what keeps a mis-enabled lane
// from writing attack text into a managed repository.
func TestAgentServesRepo_UnscopedRedTeamServesNothing(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"scanner": {Backend: "claude"},
		// A hand-written roster entry that forgot the role is still the lane.
		"redteam": {Backend: "claude"},
		// A replica of it carries the same refusal.
		"redteam-2": {Backend: "claude", ReplicaOf: "redteam"},
		// So does an agent under another name that holds the role, however
		// it is spelled.
		"attacker": {Backend: "claude", Role: " RedTeam "},
	})

	for _, agent := range []string{"redteam", "redteam-2", "attacker"} {
		for _, repo := range []string{"console", "acme/dashboard", "laredo/cuga-agent", ""} {
			if cfg.AgentServesRepo(agent, repo) {
				t.Errorf("unscoped red-team agent %q served %q", agent, repo)
			}
		}
		if !cfg.AgentRequiresRepoScope(agent) || !cfg.AgentRepoRestricted(agent) {
			t.Errorf("%q must report that it requires a scope", agent)
		}
		got := cfg.ReposForAgent(agent)
		// Non-nil matters: crux.ProjectContext.ReposFor treats nil as
		// "unscoped" and would put every repo in the agent's $HIVE_REPOS.
		if got == nil || len(got) != 0 {
			t.Errorf("ReposForAgent(%q) = %#v, want a non-nil empty list", agent, got)
		}
	}

	// Every other agent is untouched.
	if !cfg.AgentServesRepo("scanner", "console") || cfg.AgentRequiresRepoScope("scanner") || cfg.AgentRepoRestricted("scanner") {
		t.Error("an ordinary unscoped agent must keep serving the whole hive")
	}
	if got := cfg.ReposForAgent("scanner"); len(got) != 3 {
		t.Errorf("ReposForAgent(scanner) = %v, want all three", got)
	}
	if got := cfg.AgentsForRepo("console"); len(got) != 1 || got[0] != "scanner" {
		t.Errorf("AgentsForRepo(console) = %v, want [scanner] — no unscoped red-team agent serves it", got)
	}
}

// Scoping the lane is the opt-in: it then serves exactly its scope, like any
// other repo-scoped agent.
func TestAgentServesRepo_ScopedRedTeamServesOnlyItsScope(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"redteam": {Backend: "claude", Role: RedTeamRole, Repos: []string{"dashboard"}},
	})
	if !cfg.AgentServesRepo("redteam", "acme/dashboard") {
		t.Error("scoped red-team agent refused its own repo")
	}
	if cfg.AgentServesRepo("redteam", "console") {
		t.Error("scoped red-team agent served a repo outside its scope")
	}
	if got := cfg.ReposForAgent("redteam"); len(got) != 1 || got[0] != "dashboard" {
		t.Errorf("ReposForAgent = %v, want [dashboard]", got)
	}
}

func TestAgentRepoScopeWarnings_UnscopedRedTeam(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"scanner": {Backend: "claude"},
		"redteam": {Backend: "claude", Role: RedTeamRole},
	})
	warnings := AgentRepoScopeWarnings(cfg)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"redteam"`) {
		t.Fatalf("warnings = %v, want exactly one naming the unscoped redteam agent", warnings)
	}

	cfg.Agents["redteam"] = AgentConfig{Backend: "claude", Role: RedTeamRole, Repos: []string{"console"}}
	if warnings := AgentRepoScopeWarnings(cfg); len(warnings) != 0 {
		t.Errorf("a correctly scoped redteam agent must not warn, got %v", warnings)
	}
}
