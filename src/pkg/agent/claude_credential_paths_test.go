package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// claude_credential_paths_test.go covers ClaudeCredentialCandidatePaths, the
// single answer to "where do this hive's claude credentials live?" (#4699).
//
// It exists because that question previously had TWO answers. This package's
// auth probe learned about per-UID agent homes; the dashboard's model-discovery
// probe did not, and kept stat'ing only the shared path. On a per-UID fleet the
// two disagreed and the operator got a self-contradicting UI — "✓ logged in"
// beside a model list where every entry read "(common alias, unverified)".

// withTestAgentHomes redirects the per-agent home root at a temp dir so these
// tests never depend on a real /data volume.
func withTestAgentHomes(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	orig := sharedAgentHome
	sharedAgentHome = root
	t.Cleanup(func() { sharedAgentHome = orig })
	return root
}

func claudeAgent(name string, uid int) *AgentProcess {
	return &AgentProcess{Name: name, UID: uid, Config: config.AgentConfig{Backend: "claude"}}
}

func indexOfPath(paths []string, want string) int {
	for i, p := range paths {
		if p == want {
			return i
		}
	}
	return -1
}

// TestClaudeCredentialCandidatePathsPerUIDFirst is the fix: the per-UID home an
// agent's own CLI writes to must be tried BEFORE the shared legacy path. On a
// per-UID spoke the shared path is empty while the agent is authenticated, so
// ordering is what separates "found the token" from the all-unverified list.
func TestClaudeCredentialCandidatePathsPerUIDFirst(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)
	m.agents["scanner"] = claudeAgent("scanner", 2007)

	paths := m.ClaudeCredentialCandidatePaths()

	perUID := filepath.Join(AgentHome("scanner", 2007, "claude"), ".claude", ".credentials.json")
	pi, si := indexOfPath(paths, perUID), indexOfPath(paths, sharedClaudeCredentialPath)
	if pi < 0 {
		t.Fatalf("per-UID path %q absent from %v — this is the #4699 bug", perUID, paths)
	}
	if si < 0 {
		t.Fatalf("shared path absent from %v", paths)
	}
	if pi > si {
		t.Errorf("per-UID path at %d comes AFTER the shared path at %d — the agent's own credential must win", pi, si)
	}
}

// TestClaudeCredentialCandidatePathsMatchesAuthProbe pins the anti-drift
// property that is the whole reason this helper exists: whatever
// agentClaudeCredentialPaths would try for an agent, the backend-level list
// must also try. If these two ever diverge again, the dashboard and the agent
// card go back to disagreeing.
func TestClaudeCredentialCandidatePathsMatchesAuthProbe(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)
	m.agents["scanner"] = claudeAgent("scanner", 2007)

	got := m.ClaudeCredentialCandidatePaths()
	for _, want := range agentClaudeCredentialPaths("scanner", 2007, "claude") {
		if indexOfPath(got, want) < 0 {
			t.Errorf("auth probe would try %q but model discovery would not — the two probes have drifted (%v)", want, got)
		}
	}
}

// TestClaudeCredentialCandidatePathsNoAgents: a hive whose agents have not
// started yet still has a shared login worth checking.
func TestClaudeCredentialCandidatePathsNoAgents(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)

	got := m.ClaudeCredentialCandidatePaths()
	if len(got) != 1 || got[0] != sharedClaudeCredentialPath {
		t.Errorf("got %v, want just the shared path — an agentless hive must still probe it", got)
	}
}

// TestClaudeCredentialCandidatePathsNilManager: callers without an agent
// manager must be no worse off than before the change.
func TestClaudeCredentialCandidatePathsNilManager(t *testing.T) {
	var m *Manager
	got := m.ClaudeCredentialCandidatePaths()
	if len(got) != 1 || got[0] != sharedClaudeCredentialPath {
		t.Errorf("got %v, want the shared path alone (pre-per-UID behavior)", got)
	}
}

// TestClaudeCredentialCandidatePathsSkipsOtherBackends: a copilot agent's home
// holds no claude credential, so probing it is a wasted stat that would also
// make the list grow with every unrelated agent.
func TestClaudeCredentialCandidatePathsSkipsOtherBackends(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)
	m.agents["ci"] = &AgentProcess{Name: "ci", UID: 2010, Config: config.AgentConfig{Backend: "copilot"}}

	got := m.ClaudeCredentialCandidatePaths()
	for _, p := range got {
		if strings.Contains(p, "/ci/") {
			t.Errorf("copilot agent's home %q probed for a claude credential (%v)", p, got)
		}
	}
}

// TestClaudeCredentialCandidatePathsHonoursBackendOverride: an agent overridden
// to claude at runtime is a claude agent for this purpose — the same
// effective-backend rule the auth probe applies.
func TestClaudeCredentialCandidatePathsHonoursBackendOverride(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)
	m.agents["quality"] = &AgentProcess{
		Name: "quality", UID: 2011,
		Config:          config.AgentConfig{Backend: "copilot"},
		BackendOverride: "claude",
	}

	got := m.ClaudeCredentialCandidatePaths()
	want := filepath.Join(AgentHome("quality", 2011, "claude"), ".claude", ".credentials.json")
	if indexOfPath(got, want) < 0 {
		t.Errorf("override to claude not honoured: %q absent from %v", want, got)
	}
}

// TestClaudeCredentialCandidatePathsDeterministic: m.agents iterates randomly,
// so without sorting the probe would consult a different credential per call —
// making an expired token an intermittent failure and this suite flaky.
func TestClaudeCredentialCandidatePathsDeterministic(t *testing.T) {
	withTestAgentHomes(t)
	m := testManager(4)
	for _, n := range []string{"scanner", "quality", "guide", "ci-maintainer"} {
		m.agents[n] = claudeAgent(n, 2000+len(n))
	}

	first := strings.Join(m.ClaudeCredentialCandidatePaths(), "\n")
	for i := 0; i < 20; i++ {
		if got := strings.Join(m.ClaudeCredentialCandidatePaths(), "\n"); got != first {
			t.Fatalf("order changed between calls:\n%s\nvs\n%s", first, got)
		}
	}
}

// TestClaudeCredentialCandidatePathsDedupes: under HIVE_SHARED_AGENT_HOME every
// agent resolves to the same home, so without dedup the list would repeat one
// path per agent and the probe would stat it N times.
func TestClaudeCredentialCandidatePathsDedupes(t *testing.T) {
	withTestAgentHomes(t)
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	m := testManager(4)
	for _, n := range []string{"scanner", "quality", "guide"} {
		m.agents[n] = claudeAgent(n, 2000+len(n))
	}

	got := m.ClaudeCredentialCandidatePaths()
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("duplicate path %q in %v", p, got)
		}
		seen[p] = true
	}
}
