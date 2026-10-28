package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/writeback"
)

func TestWritebackConfigLoadsAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	body := `
project:
  org: acme
  repos: [acme/spyre-inference, torch-spyre/torch-spyre]
github:
  token: x
acmm_level: 4
agents:
  deps:
    backend: copilot
    enabled: true
writeback:
  unsolicited:
    min_level: 5
    require_labels: true
    backfill: false
  per_repo:
    torch-spyre/torch-spyre: { mode: issues-only }
  kinds:
    sentinel: { unlabeled_min_level: 6 }
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Writeback.RepoModeFor("torch-spyre/torch-spyre") != writeback.RepoModeIssuesOnly {
		t.Fatal("per_repo not loaded")
	}
	if p := cfg.Writeback.Policy(writeback.KindSentinel); p.UnlabeledMinLevel != 6 || p.MinLevel != 5 {
		t.Fatalf("kind override %+v", p)
	}
	deps := cfg.WritebackGateDeps()
	if deps.LevelFor("torch-spyre/torch-spyre") != 4 || !deps.RepoListed("TORCH-SPYRE") || deps.RepoListed("acme/other") || deps.RepoListed("") {
		t.Fatal("deps")
	}
	if ls := deps.OptInLabels("x"); len(ls) != 1 || ls[0] != writeback.DefaultOptInLabel {
		t.Fatalf("opt-in fallback %v", ls)
	}
	cfg.Review.RequireLabels = []string{" ok-to-review ", "ok-to-review"}
	if ls := cfg.WritebackOptInLabels("x"); len(ls) != 1 || ls[0] != "ok-to-review" {
		t.Fatalf("opt-in from review.require_labels %v", ls)
	}

	bad := body + "    bogus_kind: { min_level: 1 }\n"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown kind must fail validation")
	}
}

func TestWritebackDefaultsWhenAbsent(t *testing.T) {
	var cfg Config
	if cfg.Writeback.MinLevelOrDefault() != writeback.DefaultMinLevel || !cfg.Writeback.RequireLabelsEnabled() || cfg.Writeback.BackfillEnabled() {
		t.Fatalf("defaults: %+v", cfg.Writeback)
	}
}
