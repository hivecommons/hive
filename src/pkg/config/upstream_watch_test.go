package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func parseUpstreamWatch(t *testing.T, src string) *Config {
	t.Helper()
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.applyDefaults()
	return &cfg
}

const upstreamWatchBase = "project:\n  org: acme\n  repos: [forked-thing, other]\n"

func TestUpstreamWatchAbsentStaysZero(t *testing.T) {
	cfg := parseUpstreamWatch(t, upstreamWatchBase)
	if cfg.UpstreamWatch.Enabled || cfg.UpstreamWatch.Interval != 0 || cfg.UpstreamWatch.Repos != nil {
		t.Errorf("absent block must stay zero, got %+v", cfg.UpstreamWatch)
	}
	if err := cfg.validateUpstreamWatch(); err != nil {
		t.Errorf("absent block must validate: %v", err)
	}
}

func TestUpstreamWatchParsesAllKnobs(t *testing.T) {
	cfg := parseUpstreamWatch(t, upstreamWatchBase+
		"upstream_watch:\n  enabled: true\n  interval: 2h\n  repos:\n    acme/forked-thing:\n"+
		"      upstream: origin/thing\n      sources: [releases]\n      pr_labels: [bug, security]\n"+
		"      max_issues_per_run: 5\n      label: upstream/custom\n      start_from: 2026-06-01\n")
	w := cfg.UpstreamWatch
	r := w.Repos["acme/forked-thing"]
	if !w.Enabled || w.Interval != 2*time.Hour {
		t.Errorf("enabled/interval = %v/%s", w.Enabled, w.Interval)
	}
	if r.Upstream != "origin/thing" || len(r.Sources) != 1 || r.Sources[0] != "releases" ||
		len(r.PRLabels) != 2 || r.MaxIssuesPerRun != 5 || r.Label != "upstream/custom" || r.StartFrom != "2026-06-01" {
		t.Errorf("repo parsed wrong: %+v", r)
	}
	if err := cfg.validateUpstreamWatch(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	if at, ok := r.StartFromTime(); !ok || !at.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("StartFromTime = %v/%v, want 2026-06-01 00:00 UTC", at, ok)
	}
}

func TestUpstreamWatchStartFromTimeUnset(t *testing.T) {
	if at, ok := (UpstreamWatchRepo{}).StartFromTime(); ok || !at.IsZero() {
		t.Errorf("unset start_from = %v/%v, want zero/false", at, ok)
	}
}

func TestUpstreamWatchDefaults(t *testing.T) {
	cfg := parseUpstreamWatch(t, upstreamWatchBase+"upstream_watch:\n  enabled: true\n  repos:\n    forked-thing: {}\n")
	w := cfg.UpstreamWatch
	r := w.Repos["forked-thing"]
	if w.Interval != DefaultUpstreamWatchInterval {
		t.Errorf("interval = %s, want %s", w.Interval, DefaultUpstreamWatchInterval)
	}
	if len(r.Sources) != 2 || r.Sources[0] != "releases" || r.Sources[1] != "prs" {
		t.Errorf("sources = %v, want [releases prs]", r.Sources)
	}
	if r.Label != DefaultUpstreamWatchLabel {
		t.Errorf("label = %q", r.Label)
	}
	if r.Upstream != "" || r.MaxIssuesPerRun != 0 {
		t.Errorf("upstream/cap must stay unset: %+v", r)
	}
}

func TestUpstreamWatchValidation(t *testing.T) {
	cases := []struct {
		name, repo, want string
	}{
		{"unknown repo", "ghost:\n      upstream: a/b\n", "not listed in project.repos"},
		{"bad upstream", "forked-thing:\n      upstream: justname\n", "must be owner/repo"},
		{"deep upstream", "forked-thing:\n      upstream: a/b/c\n", "must be owner/repo"},
		{"bad source", "forked-thing:\n      sources: [commits]\n", "invalid source"},
		{"negative cap", "forked-thing:\n      max_issues_per_run: -1\n", "must be positive"},
		{"bad start_from", "forked-thing:\n      start_from: yesterday\n", "invalid start_from"},
		{"start_from with time", "forked-thing:\n      start_from: \"2026-06-01T00:00:00Z\"\n", "invalid start_from"},
		{"impossible start_from", "forked-thing:\n      start_from: \"2026-02-30\"\n", "invalid start_from"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseUpstreamWatch(t, upstreamWatchBase+"upstream_watch:\n  enabled: true\n  repos:\n    "+tc.repo)
			err := cfg.validateUpstreamWatch()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	cfg := parseUpstreamWatch(t, upstreamWatchBase+"upstream_watch:\n  interval: -1h\n")
	if err := cfg.validateUpstreamWatch(); err == nil {
		t.Error("negative interval accepted")
	}
}

func TestUpstreamWatchPruneToWatched(t *testing.T) {
	cfg := parseUpstreamWatch(t, upstreamWatchBase+
		"upstream_watch:\n  enabled: true\n  repos:\n    acme/forked-thing: {}\n    other: {}\n")
	if cfg.PruneUpstreamWatchToWatched() {
		t.Fatal("nothing to prune, but reported a change")
	}
	cfg.Project.Repos = []string{"other"}
	if !cfg.PruneUpstreamWatchToWatched() {
		t.Fatal("removed repo was not pruned")
	}
	if _, ok := cfg.UpstreamWatch.Repos["acme/forked-thing"]; ok || len(cfg.UpstreamWatch.Repos) != 1 {
		t.Errorf("repos after prune = %v, want only other", cfg.UpstreamWatch.Repos)
	}
	if err := cfg.ValidateUpstreamWatch(); err != nil {
		t.Errorf("pruned config must validate: %v", err)
	}
	cfg.Project.Repos = nil
	if !cfg.PruneUpstreamWatchToWatched() || cfg.UpstreamWatch.Repos != nil {
		t.Errorf("pruning every repo must leave a nil map, got %v", cfg.UpstreamWatch.Repos)
	}
	if !cfg.UpstreamWatch.Enabled {
		t.Error("pruning must not flip enabled")
	}
	var nilCfg *Config
	if nilCfg.PruneUpstreamWatchToWatched() || nilCfg.ClearUpstreamWatchRepos() {
		t.Error("nil config must report no change")
	}
}

func TestUpstreamWatchClearRepos(t *testing.T) {
	cfg := parseUpstreamWatch(t, upstreamWatchBase+"upstream_watch:\n  enabled: true\n  repos:\n    forked-thing: {}\n")
	if !cfg.ClearUpstreamWatchRepos() || cfg.UpstreamWatch.Repos != nil {
		t.Fatalf("clear left %v", cfg.UpstreamWatch.Repos)
	}
	if cfg.ClearUpstreamWatchRepos() {
		t.Error("second clear reported a change")
	}
}

func TestUpstreamWatchExportedDefaultsAndValidate(t *testing.T) {
	cfg := parseUpstreamWatch(t, upstreamWatchBase)
	cfg.UpstreamWatch = UpstreamWatchConfig{Enabled: true, Repos: map[string]UpstreamWatchRepo{"forked-thing": {}}}
	cfg.ApplyUpstreamWatchDefaults()
	r := cfg.UpstreamWatch.Repos["forked-thing"]
	if cfg.UpstreamWatch.Interval != DefaultUpstreamWatchInterval || r.Label != DefaultUpstreamWatchLabel || len(r.Sources) != 2 {
		t.Errorf("defaults not applied: %+v", cfg.UpstreamWatch)
	}
	if err := cfg.ValidateUpstreamWatch(); err != nil {
		t.Errorf("valid block rejected: %v", err)
	}
	cfg.UpstreamWatch.Repos["nope"] = UpstreamWatchRepo{}
	if err := cfg.ValidateUpstreamWatch(); err == nil {
		t.Error("unknown repo accepted")
	}
}
