package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/codescanning"
)

// DefaultCodeScanningInterval is how often the code scanning sweep runs.
// Alerts change when an analysis runs (a push or a weekly schedule), so an
// hourly pass is prompt enough, and its listings are ETag-cached reads that
// cost nothing when nothing changed.
const DefaultCodeScanningInterval = time.Hour

// minCodeScanningInterval keeps an operator from turning the sweep into a
// rate-limit problem.
const minCodeScanningInterval = 10 * time.Minute

// CodeScanningConfig (top-level `code_scanning:`) turns a repository's open
// GitHub code scanning alerts into grouped hive issues the lanes can work
// (src/docs/code-scanning.md). Off by default, and per repo: the top-level
// keys are the defaults, and repos.<repo> turns a repo on or off and
// overrides any of them.
type CodeScanningConfig struct {
	// Enabled is the default for every repo in project.repos.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Interval between passes. Zero means DefaultCodeScanningInterval.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	// MinSeverity is the lowest level filed: critical, high (default),
	// medium, low or note; error and warning are read as medium and low.
	MinSeverity string `yaml:"min_severity,omitempty" json:"min_severity,omitempty"`
	// IncludeTools files only alerts from these tools (CodeQL, Scorecard,
	// Trivy, …). Empty means every tool.
	IncludeTools []string `yaml:"include_tools,omitempty" json:"include_tools,omitempty"`
	// ExcludeRules are rule-ID patterns left out on top of the default
	// list (OSSF Scorecard's repo-posture checks); "!<rule>" takes one of
	// the defaults back out.
	ExcludeRules []string `yaml:"exclude_rules,omitempty" json:"exclude_rules,omitempty"`
	// MaxNewIssuesPerRun caps new issues per repo per pass. Zero means 5.
	MaxNewIssuesPerRun int `yaml:"max_new_issues_per_run,omitempty" json:"max_new_issues_per_run,omitempty"`
	// Repos holds per-repo overrides keyed by owner/repo or bare repo name.
	Repos map[string]CodeScanningRepoConfig `yaml:"repos,omitempty" json:"repos,omitempty"`
}

// CodeScanningRepoConfig is code_scanning.repos.<repo>: every field left
// empty inherits the top-level value.
type CodeScanningRepoConfig struct {
	Enabled            *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MinSeverity        string   `yaml:"min_severity,omitempty" json:"min_severity,omitempty"`
	IncludeTools       []string `yaml:"include_tools,omitempty" json:"include_tools,omitempty"`
	ExcludeRules       []string `yaml:"exclude_rules,omitempty" json:"exclude_rules,omitempty"`
	MaxNewIssuesPerRun int      `yaml:"max_new_issues_per_run,omitempty" json:"max_new_issues_per_run,omitempty"`
}

// IntervalOrDefault resolves the sweep cadence.
func (c CodeScanningConfig) IntervalOrDefault() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultCodeScanningInterval
}

// repoOverride finds repo's override, matching owner/repo or bare names
// against project.org.
func (c CodeScanningConfig) repoOverride(org, repo string) (CodeScanningRepoConfig, bool) {
	want := codeScanningRepoKey(org, repo)
	for k, v := range c.Repos {
		if codeScanningRepoKey(org, k) == want {
			return v, true
		}
	}
	return CodeScanningRepoConfig{}, false
}

func codeScanningRepoKey(org, repo string) string {
	bare, _ := NormalizeRepoForOrg(org, repo)
	return strings.ToLower(strings.TrimSpace(bare))
}

// For returns repo's effective policy and whether the sweep runs there.
func (c CodeScanningConfig) For(org, repo string) (codescanning.Policy, bool) {
	on := c.Enabled
	sev, tools, excl, maxNew := c.MinSeverity, c.IncludeTools, c.ExcludeRules, c.MaxNewIssuesPerRun
	if ov, ok := c.repoOverride(org, repo); ok {
		if ov.Enabled != nil {
			on = *ov.Enabled
		}
		if ov.MinSeverity != "" {
			sev = ov.MinSeverity
		}
		if len(ov.IncludeTools) > 0 {
			tools = ov.IncludeTools
		}
		if len(ov.ExcludeRules) > 0 {
			excl = ov.ExcludeRules
		}
		if ov.MaxNewIssuesPerRun > 0 {
			maxNew = ov.MaxNewIssuesPerRun
		}
	}
	level, _ := codescanning.ParseLevel(sev)
	return codescanning.Policy{MinLevel: level, IncludeTools: tools, ExcludeRules: excl, MaxNewIssues: maxNew}, on
}

// AnyEnabled reports whether the sweep runs on at least one of repos.
func (c CodeScanningConfig) AnyEnabled(repos []string, org string) bool {
	for _, r := range repos {
		if _, on := c.For(org, r); on {
			return true
		}
	}
	return false
}

func (c *Config) validateCodeScanning() error {
	cs := c.CodeScanning
	if cs.Interval < 0 || (cs.Interval > 0 && cs.Interval < minCodeScanningInterval) {
		return fmt.Errorf("code_scanning.interval: must be at least %s, got %s", minCodeScanningInterval, cs.Interval)
	}
	check := func(where, sev string, maxNew int, excl []string) error {
		if sev != "" {
			if _, ok := codescanning.ParseLevel(sev); !ok {
				return fmt.Errorf("%s.min_severity: %q is not one of critical, high, medium, low, note (or error, warning)", where, sev)
			}
		}
		if maxNew < 0 {
			return fmt.Errorf("%s.max_new_issues_per_run: must not be negative, got %d", where, maxNew)
		}
		if err := codescanning.ValidateRulePatterns(excl); err != nil {
			return fmt.Errorf("%s.exclude_rules: %w", where, err)
		}
		return nil
	}
	if err := check("code_scanning", cs.MinSeverity, cs.MaxNewIssuesPerRun, cs.ExcludeRules); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, r := range c.Project.Repos {
		known[codeScanningRepoKey(c.Project.Org, r)] = true
	}
	keys := make([]string, 0, len(cs.Repos))
	for k := range cs.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !known[codeScanningRepoKey(c.Project.Org, k)] {
			return fmt.Errorf("code_scanning.repos: %q is not listed in project.repos", k)
		}
		r := cs.Repos[k]
		if err := check("code_scanning.repos["+k+"]", r.MinSeverity, r.MaxNewIssuesPerRun, r.ExcludeRules); err != nil {
			return err
		}
	}
	return nil
}
