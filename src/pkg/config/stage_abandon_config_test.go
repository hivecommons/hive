package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// governor.labels.stage.abandon_after / abandon_close_repos default off: a
// config without them changes nothing.
func TestStageAbandonDefaultsOff(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("project:\n  org: example\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stage := cfg.Governor.Labels.Stage
	if got := stage.EffectiveAbandonAfter(); got != 0 {
		t.Errorf("abandon_after = %v, want 0 (off)", got)
	}
	if len(stage.AbandonCloseRepos) != 0 || stage.AbandonCloseListed("example/testrepo") {
		t.Error("abandon_close_repos must default empty")
	}
	if cfg.AbandonCloseAllowed("example/testrepo") {
		t.Error("auto-close allowed with nothing configured")
	}
}

func TestStageAbandonParsesKnobs(t *testing.T) {
	src := `acmm_level: 6
governor:
  labels:
    stage:
      abandon_after: 168h
      abandon_close_repos: [example/testrepo, other]
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stage := cfg.Governor.Labels.Stage
	if got := stage.EffectiveAbandonAfter(); got != 168*time.Hour {
		t.Fatalf("abandon_after = %v, want 168h", got)
	}
	for repo, want := range map[string]bool{
		"example/testrepo": true,
		"EXAMPLE/TestRepo": true,
		"example/other":    true, // bare name matches any owner
		"example/unlisted": false,
		"":                 false,
	} {
		if got := stage.AbandonCloseListed(repo); got != want {
			t.Errorf("AbandonCloseListed(%q) = %v, want %v", repo, got, want)
		}
	}
	if !cfg.AbandonCloseAllowed("example/testrepo") {
		t.Error("listed repo at L6 with abandon_after set must allow the close")
	}
	if cfg.AbandonCloseAllowed("example/unlisted") {
		t.Error("an unlisted repo must never auto-close")
	}
}

// The auto-close is L6-only and needs the idle clock on.
func TestStageAbandonCloseGates(t *testing.T) {
	level := func(n int) *int { return &n }
	base := func() *Config {
		c := &Config{ACMMLevel: level(6)}
		c.Governor.Labels.Stage.AbandonAfter = "72h"
		c.Governor.Labels.Stage.AbandonCloseRepos = []string{"example/testrepo"}
		return c
	}
	if !base().AbandonCloseAllowed("example/testrepo") {
		t.Fatal("baseline must allow")
	}
	below := base()
	below.ACMMLevel = level(5)
	if below.AbandonCloseAllowed("example/testrepo") {
		t.Error("allowed below L6")
	}
	repoBelow := base()
	repoBelow.Project.Org = "example"
	repoBelow.Project.RepoPolicies = []RepoPolicy{{Repo: "testrepo", ACMMLevel: level(4)}}
	if repoBelow.AbandonCloseAllowed("example/testrepo") {
		t.Error("allowed where the repo policy lowers the level below L6")
	}
	off := base()
	off.Governor.Labels.Stage.AbandonAfter = "0"
	if off.AbandonCloseAllowed("example/testrepo") {
		t.Error("allowed with abandon_after off")
	}
	var nilCfg *Config
	if nilCfg.AbandonCloseAllowed("example/testrepo") {
		t.Error("nil config allowed")
	}
}
