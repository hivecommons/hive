package config

import "testing"

// AgentConfig.CadenceScopeMode is the per-agent counterpart of the tested
// GovernorConfig.CadenceScopeMode (threshold_scaling_test.go). The governor's
// agentUsesRepoScope (pkg/governor/governor.go) gates per-repo timer
// scheduling on it, so an unnoticed default flip would silently reshard every
// hive's kick cadence.
func TestAgentCadenceScopeMode_DefaultsToAggregate(t *testing.T) {
	tests := map[string]string{
		"":          CadenceScopeAggregate,
		"aggregate": CadenceScopeAggregate,
		"per_repo":  CadenceScopePerRepo,
		// Config load rejects other values; reaching here means validation
		// was bypassed, so fail-safe to the historical aggregate behavior.
		"nonsense":  CadenceScopeAggregate,
		"PER_REPO":  CadenceScopeAggregate,
		"per-repo":  CadenceScopeAggregate,
		" per_repo": CadenceScopeAggregate,
	}
	for in, want := range tests {
		a := AgentConfig{CadenceScope: in}
		if got := a.CadenceScopeMode(); got != want {
			t.Errorf("CadenceScopeMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentUsesRepoScopedCadence(t *testing.T) {
	tests := map[string]bool{
		"":          false,
		"aggregate": false,
		"per_repo":  true,
		"nonsense":  false,
	}
	for in, want := range tests {
		a := AgentConfig{CadenceScope: in}
		if got := a.UsesRepoScopedCadence(); got != want {
			t.Errorf("UsesRepoScopedCadence with CadenceScope=%q = %v, want %v", in, got, want)
		}
	}
}

// CadenceTargetKey/SplitCadenceTargetKey shape the PERSISTED governor
// cadence/last-kick state keys. The empty-repo form must stay exactly the bare
// agent name: that is the legacy aggregate key already sitting in every
// deployed governor state file, and normalizeLastKickKeysLocked migrates
// between the two shapes by round-tripping through these helpers.
func TestCadenceTargetKey(t *testing.T) {
	tests := []struct {
		agent, repo, want string
	}{
		{"scanner", "", "scanner"},
		{"scanner", "org/repo", "scanner|org/repo"},
		{"scanner", "github.com/org/repo", "scanner|github.com/org/repo"},
		{"", "", ""},
		{"", "org/repo", "|org/repo"},
	}
	for _, tc := range tests {
		if got := CadenceTargetKey(tc.agent, tc.repo); got != tc.want {
			t.Errorf("CadenceTargetKey(%q, %q) = %q, want %q", tc.agent, tc.repo, got, tc.want)
		}
	}
}

func TestSplitCadenceTargetKey(t *testing.T) {
	tests := []struct {
		key, wantAgent, wantRepo string
	}{
		{"scanner", "scanner", ""}, // legacy aggregate shape
		{"scanner|org/repo", "scanner", "org/repo"},
		{"", "", ""},
		{"|org/repo", "", "org/repo"},
		{"scanner|", "scanner", ""},
		// Only the FIRST separator splits; repo keeps any later ones.
		{"scanner|org/repo|extra", "scanner", "org/repo|extra"},
	}
	for _, tc := range tests {
		agent, repo := SplitCadenceTargetKey(tc.key)
		if agent != tc.wantAgent || repo != tc.wantRepo {
			t.Errorf("SplitCadenceTargetKey(%q) = (%q, %q), want (%q, %q)",
				tc.key, agent, repo, tc.wantAgent, tc.wantRepo)
		}
	}
}

// Round-trip: every key CadenceTargetKey can build with a non-empty,
// separator-free agent must split back to the same (agent, repo) pair —
// otherwise per-repo governor state written by one release could be
// misattributed by the next.
func TestCadenceTargetKeyRoundTrip(t *testing.T) {
	pairs := []struct{ agent, repo string }{
		{"scanner", ""},
		{"scanner", "org/repo"},
		{"quality", "github.com/hivecommons/hive"},
		{"a b c", "repo with spaces"},
	}
	for _, p := range pairs {
		agent, repo := SplitCadenceTargetKey(CadenceTargetKey(p.agent, p.repo))
		if agent != p.agent || repo != p.repo {
			t.Errorf("round-trip (%q, %q) came back as (%q, %q)", p.agent, p.repo, agent, repo)
		}
	}
}
