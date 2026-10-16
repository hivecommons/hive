package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/gfi"
)

// DefaultGoodFirstIssueInterval is how often the curation sweep runs.
const DefaultGoodFirstIssueInterval = time.Hour

// GoodFirstIssueConfig (top-level `good_first_issue:`) is hive-curated
// `good first issue` advertising: a periodic sweep scores open, unclaimed
// issues, applies GitHub's canonical `good first issue` and `help wanted`
// labels to the few that qualify, and leaves ONE welcome comment pointing
// contributors who bring their own coding agent at this hive's contributor
// relay to claim the issue. The label is not a hold — if a hive agent starts
// the issue first the label is removed and the comment marked superseded.
//
// Default OFF: the hive writes to issues on the operator's behalf, so it is
// an explicit opt-in. All numeric knobs fall back to the gfi package
// defaults when zero.
type GoodFirstIssueConfig struct {
	// Enabled turns the sweep on.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Interval between sweeps. Zero means DefaultGoodFirstIssueInterval.
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	// MaxPerRepo caps issues wearing the label per repository.
	MaxPerRepo int `yaml:"max_per_repo,omitempty" json:"max_per_repo,omitempty"`
	// MaxPerSweep caps new selections per repository per pass.
	MaxPerSweep int `yaml:"max_per_sweep,omitempty" json:"max_per_sweep,omitempty"`
	// MinAge is how old an issue must be before the hive advertises it.
	MinAge time.Duration `yaml:"min_age,omitempty" json:"min_age,omitempty"`
	// MinBodyChars / MaxBodyChars bound the issue body length.
	MinBodyChars int `yaml:"min_body_chars,omitempty" json:"min_body_chars,omitempty"`
	MaxBodyChars int `yaml:"max_body_chars,omitempty" json:"max_body_chars,omitempty"`
	// MaxPackages is how many source directories an issue may mention.
	MaxPackages int `yaml:"max_packages,omitempty" json:"max_packages,omitempty"`
	// MinScore is the positive-signal count an issue needs.
	MinScore int `yaml:"min_score,omitempty" json:"min_score,omitempty"`
	// ReselectCooldown keeps a released issue out of the pool.
	ReselectCooldown time.Duration `yaml:"reselect_cooldown,omitempty" json:"reselect_cooldown,omitempty"`
	// ExcludeLabels replaces gfi.DefaultExcludeLabels when set. The hive's
	// hard-suppress labels are always excluded regardless.
	ExcludeLabels []string `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
	// Repos optionally restricts the sweep to these owner/repo slugs. Empty
	// means every watched repo.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// DocsURL overrides the base URL the comment links contributor docs
	// under. Empty means gfi.DefaultDocsBase.
	DocsURL string `yaml:"docs_url,omitempty" json:"docs_url,omitempty"`
	// CommentTemplate overrides the welcome comment (text/template over
	// gfi.Vars). It must contain {{ .Marker }}. Empty means
	// gfi.DefaultTemplate.
	CommentTemplate string `yaml:"comment_template,omitempty" json:"comment_template,omitempty"`
}

// Scorer defaults re-exported so the dashboard can show them without
// importing pkg/gfi (pkg/dashboard import ratchets).
const (
	DefaultGoodFirstIssueMaxPerRepo  = gfi.DefaultMaxPerRepo
	DefaultGoodFirstIssueMaxPerSweep = gfi.DefaultMaxPerSweep
	DefaultGoodFirstIssueMinAge      = gfi.DefaultMinAge
)

// IntervalOrDefault resolves the sweep cadence.
func (g GoodFirstIssueConfig) IntervalOrDefault() time.Duration {
	if g.Interval > 0 {
		return g.Interval
	}
	return DefaultGoodFirstIssueInterval
}

// Policy converts the block into the scorer's policy. hardSuppress is the
// hive's escalation label set, which the scorer always excludes.
func (g GoodFirstIssueConfig) Policy(hardSuppress []string) gfi.Policy {
	return gfi.Policy{
		MaxPerRepo:         g.MaxPerRepo,
		MaxPerSweep:        g.MaxPerSweep,
		MinAge:             g.MinAge,
		MinBodyChars:       g.MinBodyChars,
		MaxBodyChars:       g.MaxBodyChars,
		MaxPackages:        g.MaxPackages,
		MinScore:           g.MinScore,
		ReselectCooldown:   g.ReselectCooldown,
		ExcludeLabels:      g.ExcludeLabels,
		HardSuppressLabels: hardSuppress,
	}.Normalized()
}

// RepoAllowed reports whether repo (owner/repo) is in scope.
func (g GoodFirstIssueConfig) RepoAllowed(repo string) bool {
	if len(g.Repos) == 0 {
		return true
	}
	repo = strings.TrimSpace(repo)
	for _, r := range g.Repos {
		if strings.EqualFold(strings.TrimSpace(r), repo) {
			return true
		}
	}
	return false
}

// ValidateGoodFirstIssue rejects negative knobs and a comment template that
// would not parse or lacks the marker (which would make the sweep post a
// new comment every pass).
func ValidateGoodFirstIssue(g GoodFirstIssueConfig) error {
	for name, v := range map[string]int{
		"max_per_repo": g.MaxPerRepo, "max_per_sweep": g.MaxPerSweep, "min_body_chars": g.MinBodyChars,
		"max_body_chars": g.MaxBodyChars, "max_packages": g.MaxPackages, "min_score": g.MinScore,
	} {
		if v < 0 {
			return fmt.Errorf("good_first_issue.%s: must not be negative", name)
		}
	}
	if g.Interval < 0 || g.MinAge < 0 || g.ReselectCooldown < 0 {
		return fmt.Errorf("good_first_issue: durations must not be negative")
	}
	if g.MinBodyChars > 0 && g.MaxBodyChars > 0 && g.MinBodyChars > g.MaxBodyChars {
		return fmt.Errorf("good_first_issue.min_body_chars (%d) exceeds max_body_chars (%d)", g.MinBodyChars, g.MaxBodyChars)
	}
	if _, err := gfi.ParseTemplate(g.CommentTemplate); err != nil {
		return fmt.Errorf("good_first_issue.comment_template: %w", err)
	}
	return nil
}
