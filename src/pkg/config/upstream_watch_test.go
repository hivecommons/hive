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
		"      max_issues_per_run: 5\n      label: upstream/custom\n")
	w := cfg.UpstreamWatch
	r := w.Repos["acme/forked-thing"]
	if !w.Enabled || w.Interval != 2*time.Hour {
		t.Errorf("enabled/interval = %v/%s", w.Enabled, w.Interval)
	}
	if r.Upstream != "origin/thing" || len(r.Sources) != 1 || r.Sources[0] != "releases" ||
		len(r.PRLabels) != 2 || r.MaxIssuesPerRun != 5 || r.Label != "upstream/custom" {
		t.Errorf("repo parsed wrong: %+v", r)
	}
	if err := cfg.validateUpstreamWatch(); err != nil {
		t.Errorf("valid config rejected: %v", err)
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
