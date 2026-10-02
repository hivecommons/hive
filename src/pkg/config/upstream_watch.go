package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Upstream watch source names (hivecommons/hive#9963).
const (
	UpstreamSourceReleases = "releases"
	UpstreamSourcePRs      = "prs"

	DefaultUpstreamWatchInterval = 6 * time.Hour
	DefaultUpstreamWatchLabel    = "upstream/port"
)

// UpstreamWatchConfig declares, per repo, the upstream a fork follows and the
// volume-control filters the upstream watch applies (hivecommons/hive#9966).
// Config only: polling and issue filing live elsewhere. Zero value: off.
type UpstreamWatchConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Interval is how often the watch polls. Zero means
	// DefaultUpstreamWatchInterval.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	// Repos is keyed by a repo listed in project.repos (bare name or
	// org-qualified).
	Repos map[string]UpstreamWatchRepo `yaml:"repos,omitempty" json:"repos,omitempty"`
}

// UpstreamWatchRepo is the per-repo upstream declaration and filters.
type UpstreamWatchRepo struct {
	// Upstream is the owner/repo this repo follows. Empty means it is resolved
	// from the GitHub fork parent at runtime; an explicit value wins.
	Upstream string `yaml:"upstream,omitempty" json:"upstream,omitempty"`
	// Sources selects what to watch: "releases", "prs". Empty means both.
	Sources []string `yaml:"sources,omitempty" json:"sources,omitempty"`
	// PRLabels restricts merged PRs to those carrying one of these labels.
	// Empty means all merged PRs.
	PRLabels []string `yaml:"pr_labels,omitempty" json:"pr_labels,omitempty"`
	// MaxIssuesPerRun caps issues filed per run. Zero means no cap.
	MaxIssuesPerRun int `yaml:"max_issues_per_run,omitempty" json:"max_issues_per_run,omitempty"`
	// Label is applied to filed issues. Empty means DefaultUpstreamWatchLabel.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`
}

var upstreamSources = []string{UpstreamSourceReleases, UpstreamSourcePRs}

func (c *Config) applyUpstreamWatchDefaults() {
	w := &c.UpstreamWatch
	// An absent block stays the zero value so Save() does not materialise it.
	if !w.Enabled && len(w.Repos) == 0 {
		return
	}
	if w.Interval == 0 {
		w.Interval = DefaultUpstreamWatchInterval
	}
	for key, r := range w.Repos {
		if len(r.Sources) == 0 {
			r.Sources = append([]string(nil), upstreamSources...)
		}
		if strings.TrimSpace(r.Label) == "" {
			r.Label = DefaultUpstreamWatchLabel
		}
		w.Repos[key] = r
	}
}

func (c *Config) validateUpstreamWatch() error {
	w := c.UpstreamWatch
	if w.Interval < 0 {
		return fmt.Errorf("upstream_watch: interval must not be negative, got %s", w.Interval)
	}
	known := make(map[string]bool, len(c.Project.Repos))
	for _, r := range c.Project.Repos {
		known[strings.ToLower(r)] = true
	}
	keys := make([]string, 0, len(w.Repos))
	for k := range w.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := w.Repos[key]
		bare, _ := NormalizeRepoForOrg(c.Project.Org, key)
		if !known[strings.ToLower(bare)] {
			return fmt.Errorf("upstream_watch.repos: %q is not listed in project.repos", key)
		}
		if r.Upstream != "" && !validOwnerRepo(r.Upstream) {
			return fmt.Errorf("upstream_watch.repos[%s]: invalid upstream %q (must be owner/repo)", key, r.Upstream)
		}
		for _, s := range r.Sources {
			if s != UpstreamSourceReleases && s != UpstreamSourcePRs {
				return fmt.Errorf("upstream_watch.repos[%s]: invalid source %q (must be %s or %s)", key, s, UpstreamSourceReleases, UpstreamSourcePRs)
			}
		}
		if r.MaxIssuesPerRun < 0 {
			return fmt.Errorf("upstream_watch.repos[%s]: max_issues_per_run must be positive, got %d", key, r.MaxIssuesPerRun)
		}
	}
	return nil
}

func validOwnerRepo(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	return ok && owner != "" && repo != "" && !strings.ContainsAny(s, " \t") && strings.Count(s, "/") == 1
}
