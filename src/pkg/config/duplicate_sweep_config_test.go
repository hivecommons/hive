package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The sweep must be inert on every hive that has not asked for it. A config
// with no `duplicate_sweep:` block at all must leave both grants off, because
// the first thing this feature does when enabled is enumerate every open PR in
// every repo, and the second is speak on contributors' PRs.
func TestDuplicateSweepDefaultsOff(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("project:\n  org: acme\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.DuplicateSweep.Enabled {
		t.Error("duplicate_sweep.enabled defaults on; it must be opt-in")
	}
	if cfg.DuplicateSweep.PostComments {
		t.Error("duplicate_sweep.post_comments defaults on; it must be opt-in")
	}
}

// Enabling the scan must NOT imply permission to write. The two grants are
// separate so an operator can read a report-only pass before the hive
// comments on anyone's PR.
func TestDuplicateSweepEnabledDoesNotImplyPostComments(t *testing.T) {
	var cfg Config
	src := "duplicate_sweep:\n  enabled: true\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.DuplicateSweep.Enabled {
		t.Fatal("duplicate_sweep.enabled did not parse")
	}
	if cfg.DuplicateSweep.PostComments {
		t.Error("enabling the scan silently granted the write")
	}
}

func TestDuplicateSweepParsesAllKnobs(t *testing.T) {
	var cfg Config
	src := "duplicate_sweep:\n" +
		"  enabled: true\n" +
		"  post_comments: true\n" +
		"  max_comments: 3\n" +
		"  max_prs_per_repo: 42\n" +
		"  bot_authors:\n    - churn-updater\n"
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := cfg.DuplicateSweep
	if !d.Enabled || !d.PostComments {
		t.Error("grants did not parse")
	}
	if d.MaxComments != 3 {
		t.Errorf("max_comments = %d, want 3", d.MaxComments)
	}
	if d.MaxPRsPerRepo != 42 {
		t.Errorf("max_prs_per_repo = %d, want 42", d.MaxPRsPerRepo)
	}
	if len(d.BotAuthors) != 1 || d.BotAuthors[0] != "churn-updater" {
		t.Errorf("bot_authors = %v, want [churn-updater]", d.BotAuthors)
	}
}
