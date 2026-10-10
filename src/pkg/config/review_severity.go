package config

import (
	"fmt"
	"strings"
)

// ReviewSeverityConfig is `review.severity` (hivecommons/hive#11088): the one
// blocking line that governs the hive reviewer's own findings and external
// review-bot findings alike. Findings at or above BlockAt block merge; those
// below it are posted as non-blocking comments (CommentBelow) and/or filed to
// the backlog (BacklogBelow, destination in review.backlog).
//
// BlockAt also drives classification.review_bots.min_priority when that key
// is not set explicitly in hive.yaml or hive-project.yaml; see
// Config.EffectiveReviewBots for the precedence.
type ReviewSeverityConfig struct {
	// BlockAt is the lowest priority that still blocks: P1 (P0–P1 block,
	// "ship fast"), P2 (P0–P2 block, "strict") or P3 (everything blocks).
	// Empty means no blocking line is configured and existing behaviour is
	// unchanged.
	BlockAt string `yaml:"block_at,omitempty" json:"block_at,omitempty"`
	// CommentBelow posts findings below BlockAt as non-blocking comments.
	// Pointer so unset (default true) is distinguishable from false.
	CommentBelow *bool `yaml:"comment_below,omitempty" json:"comment_below,omitempty"`
	// BacklogBelow files findings below BlockAt to review.backlog.
	// Pointer so unset (default true) is distinguishable from false.
	BacklogBelow *bool `yaml:"backlog_below,omitempty" json:"backlog_below,omitempty"`
}

// ValidReviewSeverityBlockAt lists the accepted review.severity.block_at values.
var ValidReviewSeverityBlockAt = []string{"P1", "P2", "P3"}

// NormalizeReviewSeverityBlockAt canonicalises block_at ("" or P1-P3, case and
// surrounding whitespace ignored). ok is false for anything else.
func NormalizeReviewSeverityBlockAt(v string) (string, bool) {
	v = strings.ToUpper(strings.TrimSpace(v))
	if v == "" {
		return "", true
	}
	for _, p := range ValidReviewSeverityBlockAt {
		if v == p {
			return v, true
		}
	}
	return "", false
}

// CommentBelowEnabled returns comment_below with the default (true) applied.
func (s ReviewSeverityConfig) CommentBelowEnabled() bool {
	return s.CommentBelow == nil || *s.CommentBelow
}

// BacklogBelowEnabled returns backlog_below with the default (true) applied.
func (s ReviewSeverityConfig) BacklogBelowEnabled() bool {
	return s.BacklogBelow == nil || *s.BacklogBelow
}

// Validate rejects an unrecognised block_at, naming the field.
func (s ReviewSeverityConfig) Validate() error {
	if _, ok := NormalizeReviewSeverityBlockAt(s.BlockAt); !ok {
		return fmt.Errorf("review.severity.block_at: %q is not valid (must be %s, or empty)", s.BlockAt, strings.Join(ValidReviewSeverityBlockAt, ", "))
	}
	return nil
}

// Review backlog destinations (review.backlog.destination).
const (
	ReviewBacklogGitHubIssue   = "github_issue"
	ReviewBacklogGitHubProject = "github_project"
	ReviewBacklogLinear        = "linear"
	ReviewBacklogJira          = "jira"
)

// ValidReviewBacklogDestinations lists the accepted destinations in UI order.
var ValidReviewBacklogDestinations = []string{ReviewBacklogGitHubIssue, ReviewBacklogGitHubProject, ReviewBacklogLinear, ReviewBacklogJira}

// DefaultReviewBacklogLabel is the label applied when review.backlog.labels
// is unset; it matches the out-of-scope backlog filer's label.
const DefaultReviewBacklogLabel = "from-review"

// DefaultReviewBacklogMaxPerPRPerDay caps backlog items filed per PR per day
// when review.backlog.max_per_pr_per_day is unset.
const DefaultReviewBacklogMaxPerPRPerDay = 10

// ReviewBacklogConfig is `review.backlog` (hivecommons/hive#11088): where
// below-the-line findings are filed. The review backlog filer in pkg/github
// routes them there (hivecommons/hive#11089).
type ReviewBacklogConfig struct {
	// Destination is github_issue (default), github_project, linear or jira.
	// linear and jira only take effect when governor.work_source.type names
	// the same tracker; otherwise EffectiveDestination falls back to
	// github_issue.
	Destination string `yaml:"destination,omitempty" json:"destination,omitempty"`
	// ProjectColumnID is the GitHub Projects v2 status option id new items
	// land in. Required when Destination is github_project.
	ProjectColumnID string `yaml:"project_column_id,omitempty" json:"project_column_id,omitempty"`
	// LinearState is the Linear workflow state for new items; empty uses the
	// team's default state.
	LinearState string `yaml:"linear_state,omitempty" json:"linear_state,omitempty"`
	// JiraStatus is the Jira status new items are transitioned to; empty
	// leaves the project's initial status.
	JiraStatus string `yaml:"jira_status,omitempty" json:"jira_status,omitempty"`
	// Labels are applied to every backlog item. nil means [from-review].
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// MaxPerPRPerDay caps backlog items filed per PR per day. 0 means
	// DefaultReviewBacklogMaxPerPRPerDay.
	MaxPerPRPerDay int `yaml:"max_per_pr_per_day,omitempty" json:"max_per_pr_per_day,omitempty"`
}

// NormalizeReviewBacklogDestination canonicalises a destination ("" means
// github_issue). ok is false for an unknown value.
func NormalizeReviewBacklogDestination(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ReviewBacklogGitHubIssue, true
	}
	for _, d := range ValidReviewBacklogDestinations {
		if v == d {
			return v, true
		}
	}
	return "", false
}

// ReviewBacklogDestinationAllowed reports whether destination may be used
// with the given governor.work_source.type. github_issue is always allowed;
// github_project, linear and jira need their work source to be active,
// because that is where the project, team and credentials come from.
func ReviewBacklogDestinationAllowed(destination, workSourceType string) bool {
	ws := strings.ToLower(strings.TrimSpace(workSourceType))
	switch destination {
	case ReviewBacklogGitHubIssue:
		return true
	case ReviewBacklogGitHubProject:
		return ws == "github_projects"
	case ReviewBacklogLinear:
		return ws == "linear"
	case ReviewBacklogJira:
		return ws == "jira"
	}
	return false
}

// EffectiveDestination returns the destination to use with the active work
// source, falling back to github_issue when the configured one is unknown or
// its work source is not active.
func (b ReviewBacklogConfig) EffectiveDestination(workSourceType string) string {
	d, ok := NormalizeReviewBacklogDestination(b.Destination)
	if !ok || !ReviewBacklogDestinationAllowed(d, workSourceType) {
		return ReviewBacklogGitHubIssue
	}
	return d
}

// EffectiveLabels returns labels with the default applied.
func (b ReviewBacklogConfig) EffectiveLabels() []string {
	if b.Labels == nil {
		return []string{DefaultReviewBacklogLabel}
	}
	return b.Labels
}

// EffectiveMaxPerPRPerDay returns max_per_pr_per_day with the default applied.
func (b ReviewBacklogConfig) EffectiveMaxPerPRPerDay() int {
	if b.MaxPerPRPerDay <= 0 {
		return DefaultReviewBacklogMaxPerPRPerDay
	}
	return b.MaxPerPRPerDay
}

// Validate checks the backlog block, naming the offending field. A mismatch
// between destination and the active work source is deliberately not an
// error here: switching work source must never stop the hive from loading,
// so EffectiveDestination falls back to github_issue instead.
func (b ReviewBacklogConfig) Validate() error {
	d, ok := NormalizeReviewBacklogDestination(b.Destination)
	if !ok {
		return fmt.Errorf("review.backlog.destination: %q is not valid (must be %s, or empty)", b.Destination, strings.Join(ValidReviewBacklogDestinations, ", "))
	}
	if d == ReviewBacklogGitHubProject && strings.TrimSpace(b.ProjectColumnID) == "" {
		return fmt.Errorf("review.backlog.project_column_id: required when review.backlog.destination is %s", ReviewBacklogGitHubProject)
	}
	for i, l := range b.Labels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("review.backlog.labels[%d]: must not be blank", i)
		}
	}
	if b.MaxPerPRPerDay < 0 {
		return fmt.Errorf("review.backlog.max_per_pr_per_day: must not be negative, got %d", b.MaxPerPRPerDay)
	}
	return nil
}
