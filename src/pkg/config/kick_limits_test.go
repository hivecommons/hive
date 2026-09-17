package config

import (
	"os"
	"path/filepath"
	"testing"
)

// governor.kick_limits (#7368, #7455): an absent or negative value means the
// default; an explicit 0 means unlimited.
func TestKickLimitsConfig_Defaults(t *testing.T) {
	var k KickLimitsConfig
	if k.IssuesPerKick() != DefaultMaxIssuesPerKick || k.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Errorf("zero value = (%d, %d), want defaults (%d, %d)", k.IssuesPerKick(), k.PRsPerKick(), DefaultMaxIssuesPerKick, DefaultMaxPRsPerKick)
	}
	k = KickLimitsConfig{MaxIssues: intPtr(-5), MaxPRs: intPtr(-1)}
	if k.IssuesPerKick() != DefaultMaxIssuesPerKick || k.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Errorf("negative values must fall back to the defaults, got (%d, %d)", k.IssuesPerKick(), k.PRsPerKick())
	}
	k = KickLimitsConfig{MaxIssues: intPtr(25), MaxPRs: intPtr(10)}
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
	k := KickLimitsConfig{MaxIssues: intPtr(MaxKickListCap + 1), MaxPRs: intPtr(100000)}
	if got := k.IssuesPerKick(); got != MaxKickListCap {
		t.Errorf("IssuesPerKick() = %d, want it pinned to the ceiling %d", got, MaxKickListCap)
	}
	if got := k.PRsPerKick(); got != MaxKickListCap {
		t.Errorf("PRsPerKick() = %d, want it pinned to the ceiling %d", got, MaxKickListCap)
	}
}

// The ceiling itself is a legal value — the clamp must not be off by one.
func TestKickLimitsAcceptsTheCeilingExactly(t *testing.T) {
	k := KickLimitsConfig{MaxIssues: intPtr(MaxKickListCap), MaxPRs: intPtr(MaxKickListCap)}
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
	k := KickLimitsConfig{MaxIssues: intPtr(MinKickListCap), MaxPRs: intPtr(MinKickListCap)}
	if got := k.IssuesPerKick(); got != 1 {
		t.Errorf("IssuesPerKick() = %d, want 1", got)
	}
	if got := k.PRsPerKick(); got != 1 {
		t.Errorf("PRsPerKick() = %d, want 1", got)
	}
}

// An explicit 0 is the operator's opt-out of capping (#7455). It must be
// distinguishable from an absent key, which still resolves to the default —
// otherwise every spoke that never configured kick_limits silently uncaps.
func TestKickLimitsZeroMeansUnlimited(t *testing.T) {
	k := KickLimitsConfig{MaxIssues: intPtr(0), MaxPRs: intPtr(0)}
	if got := k.IssuesPerKick(); got != KickListUnlimited {
		t.Errorf("IssuesPerKick() = %d, want KickListUnlimited (%d)", got, KickListUnlimited)
	}
	if got := k.PRsPerKick(); got != KickListUnlimited {
		t.Errorf("PRsPerKick() = %d, want KickListUnlimited (%d)", got, KickListUnlimited)
	}

	var absent KickLimitsConfig
	if absent.IssuesPerKick() != DefaultMaxIssuesPerKick || absent.PRsPerKick() != DefaultMaxPRsPerKick {
		t.Errorf("an absent key must stay at the defaults, got (%d, %d)", absent.IssuesPerKick(), absent.PRsPerKick())
	}
}

// max_prs: 0 in YAML must survive the round trip as unlimited, not be lost to
// omitempty or read back as "absent".
func TestKickLimitsZeroLoadsFromYAML(t *testing.T) {
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
    max_prs: 0
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Governor.KickLimits.PRsPerKick(); got != KickListUnlimited {
		t.Errorf("max_prs: 0 loaded as %d, want unlimited", got)
	}
	if got := cfg.Governor.KickLimits.IssuesPerKick(); got != DefaultMaxIssuesPerKick {
		t.Errorf("an unset max_issues alongside max_prs: 0 = %d, want the default", got)
	}
}
