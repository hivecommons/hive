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
