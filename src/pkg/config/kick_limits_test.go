package config

import (
	"os"
	"path/filepath"
	"testing"
)

// governor.kick_limits (#7368): absent, zero and negative values mean the
// defaults — there is deliberately no way to spell "uncapped".
func TestKickLimitsConfig_Defaults(t *testing.T) {
	var k KickLimitsConfig
	if k.IssuesPerKick() != DefaultMaxIssuesPerKick || k.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Errorf("zero value = (%d, %d), want defaults (%d, %d)", k.IssuesPerKick(), k.PRsPerKick(), DefaultMaxIssuesPerKick, DefaultMaxPRsPerKick)
	}
	k = KickLimitsConfig{MaxIssues: -5, MaxPRs: -1}
	if k.IssuesPerKick() != DefaultMaxIssuesPerKick || k.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Errorf("negative values must fall back to the defaults, got (%d, %d)", k.IssuesPerKick(), k.PRsPerKick())
	}
	k = KickLimitsConfig{MaxIssues: 25, MaxPRs: 10}
	if k.IssuesPerKick() != 25 || k.PRsPerKick() != 10 {
		t.Errorf("explicit values not honoured: (%d, %d)", k.IssuesPerKick(), k.PRsPerKick())
	}
}

func TestKickLimitsConfig_LoadsFromGovernorBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hive.yaml")
	doc := `project:
  org: acme
  repos: [acme/app]
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
governor:
  kick_limits:
    max_issues: 40
    max_prs: 15
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Governor.KickLimits; got.IssuesPerKick() != 40 || got.PRsPerKick() != 15 {
		t.Errorf("governor.kick_limits not loaded: %+v", got)
	}
}

// A cap above the ceiling is pinned to it rather than honoured. Without this a
// setting of max_prs: 100000 is obeyed verbatim and reproduces the unbounded
// prompt the cap exists to prevent, through the control meant to prevent it
// (hivecommons/hive#7368).
func TestKickLimitsClampsAboveCeiling(t *testing.T) {
	k := KickLimitsConfig{MaxIssues: MaxKickListCap + 1, MaxPRs: 100000}
	if got := k.IssuesPerKick(); got != MaxKickListCap {
		t.Errorf("IssuesPerKick() = %d, want it pinned to the ceiling %d", got, MaxKickListCap)
	}
	if got := k.PRsPerKick(); got != MaxKickListCap {
		t.Errorf("PRsPerKick() = %d, want it pinned to the ceiling %d", got, MaxKickListCap)
	}
}

// The ceiling itself is a legal value — the clamp must not be off by one.
func TestKickLimitsAcceptsTheCeilingExactly(t *testing.T) {
	k := KickLimitsConfig{MaxIssues: MaxKickListCap, MaxPRs: MaxKickListCap}
	if got := k.IssuesPerKick(); got != MaxKickListCap {
		t.Errorf("IssuesPerKick() = %d, want %d", got, MaxKickListCap)
	}
	if got := k.PRsPerKick(); got != MaxKickListCap {
		t.Errorf("PRsPerKick() = %d, want %d", got, MaxKickListCap)
	}
}

// The floor is a real cap of one item, not a fallback to the default — an
// operator throttling a huge backlog must be able to ask for a single item.
func TestKickLimitsHonoursTheMinimum(t *testing.T) {
	k := KickLimitsConfig{MaxIssues: MinKickListCap, MaxPRs: MinKickListCap}
	if got := k.IssuesPerKick(); got != 1 {
		t.Errorf("IssuesPerKick() = %d, want 1", got)
	}
	if got := k.PRsPerKick(); got != 1 {
		t.Errorf("PRsPerKick() = %d, want 1", got)
	}
}
