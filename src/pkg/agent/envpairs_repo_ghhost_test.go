package agent

import (
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// Tests for the HIVE_REPO / HIVE_REPOS / GH_HOST env exports added in the
// GHE-auth fix (gh-wrapper-list-block-msg): policy templates instruct agents
// to run `gh issue create --repo "$HIVE_REPO"`, so the manager must export it
// (and the full project repo list, and GH_HOST for GHE spokes) to every agent.

func envPairsFor(t *testing.T, project ProjectContext) map[string]string {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude"},
	}, discardLogger(), project)
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	found := map[string]string{}
	for _, p := range m.agentEnvPairs(agent) {
		found[p.Key] = p.Value
	}
	return found
}

// TestAgentEnvPairs_HiveRepoExports: org + repos configured with an explicit
// primary repo ⇒ HIVE_REPO is org/primary and HIVE_REPOS lists every repo,
// comma-separated, each org-qualified.
func TestAgentEnvPairs_HiveRepoExports(t *testing.T) {
	found := envPairsFor(t, ProjectContext{
		Org:             "kubestellar",
		Repos:           []string{"hive", "kubeflex", "docs"},
		PrimaryRepoName: "kubeflex",
	})
	if got := found["HIVE_REPO"]; got != "kubestellar/kubeflex" {
		t.Errorf("HIVE_REPO = %q, want kubestellar/kubeflex (explicit primary)", got)
	}
	if got := found["HIVE_REPOS"]; got != "kubestellar/hive,kubestellar/kubeflex,kubestellar/docs" {
		t.Errorf("HIVE_REPOS = %q, want all three org-qualified repos", got)
	}
}

// TestAgentEnvPairs_HiveRepoPrimaryFallback: no explicit primary ⇒ HIVE_REPO
// falls back to the first configured repo.
func TestAgentEnvPairs_HiveRepoPrimaryFallback(t *testing.T) {
	found := envPairsFor(t, ProjectContext{
		Org:   "kubestellar",
		Repos: []string{"hive", "docs"},
	})
	if got := found["HIVE_REPO"]; got != "kubestellar/hive" {
		t.Errorf("HIVE_REPO = %q, want kubestellar/hive (Repos[0] fallback)", got)
	}
}

// TestAgentEnvPairs_HiveRepoPrimaryAlreadyQualified: a PrimaryRepoName that
// arrives already org-qualified ("org/repo") must not double the org prefix.
func TestAgentEnvPairs_HiveRepoPrimaryAlreadyQualified(t *testing.T) {
	found := envPairsFor(t, ProjectContext{
		Org:             "kubestellar",
		Repos:           []string{"hive"},
		PrimaryRepoName: "kubestellar/hive",
	})
	if got := found["HIVE_REPO"]; got != "kubestellar/hive" {
		t.Errorf("HIVE_REPO = %q, want kubestellar/hive (no doubled org prefix)", got)
	}
}

// TestAgentEnvPairs_HiveRepoAbsentWithoutProject: no org or no repos ⇒ neither
// HIVE_REPO nor HIVE_REPOS is exported (agents fall back to their own logic;
// an empty or "/"-prefixed value would be worse than absence).
func TestAgentEnvPairs_HiveRepoAbsentWithoutProject(t *testing.T) {
	for name, project := range map[string]ProjectContext{
		"no org":   {Repos: []string{"hive"}},
		"no repos": {Org: "kubestellar"},
		"empty":    {},
	} {
		found := envPairsFor(t, project)
		if v, ok := found["HIVE_REPO"]; ok {
			t.Errorf("%s: HIVE_REPO exported as %q, want absent", name, v)
		}
		if v, ok := found["HIVE_REPOS"]; ok {
			t.Errorf("%s: HIVE_REPOS exported as %q, want absent", name, v)
		}
	}
}

// TestAgentEnvPairs_GHHost: GHHost set (GHE spoke) ⇒ GH_HOST exported so the
// gh CLI targets the right forge; empty (public github.com) ⇒ absent, because
// an exported empty GH_HOST would break gh entirely.
func TestAgentEnvPairs_GHHost(t *testing.T) {
	found := envPairsFor(t, ProjectContext{GHHost: "github.ibm.com"})
	if got := found["GH_HOST"]; got != "github.ibm.com" {
		t.Errorf("GH_HOST = %q, want github.ibm.com", got)
	}

	found = envPairsFor(t, ProjectContext{})
	if v, ok := found["GH_HOST"]; ok {
		t.Errorf("GH_HOST exported as %q for public github.com, want absent", v)
	}
}
