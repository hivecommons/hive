// Package worksource defines the WorkSource interface — the Step 01 seam in
// hive's "Issue Filed → fix → merge" loop. A WorkSource enumerates actionable
// work items from an external planning tool (GitHub Issues, GitHub Projects,
// Linear, Jira). Steps 03–07 (fix agent, PR, review, merge) are source-agnostic
// and unchanged.
package worksource

import (
	"context"
	"time"
)

// Issue is a source-neutral work item. Fields absent on a given source are left
// at their zero value.
type Issue struct {
	// SourceType identifies which work source produced this item
	// ("github", "github_projects", "linear", "jira").
	SourceType string `json:"source_type"`
	// Repo is the GitHub repo (owner/name) agents should clone and open PRs
	// against for this issue.
	Repo string `json:"repo"`
	// ExternalID is the source-native identifier. For GitHub: "42". For Linear:
	// "ENG-123". For Jira: "ENG-42". Never empty.
	ExternalID string `json:"external_id"`
	// Number is the integer issue number when the source uses numeric IDs
	// (GitHub). Zero for sources with string identifiers (Linear, Jira).
	Number int `json:"number,omitempty"`
	// Title is the issue title / summary.
	Title string `json:"title"`
	// Author is the login/username of who filed the issue.
	Author string `json:"author"`
	// Labels are the issue labels / tags.
	Labels []string `json:"labels,omitempty"`
	// Assignees are the logins/usernames currently assigned.
	Assignees []string `json:"assignees,omitempty"`
	// Priority is a normalized priority string: "urgent", "high", "medium",
	// "low", "none". Empty when the source does not provide priority.
	Priority string `json:"priority,omitempty"`
	// State is the issue state string from the source ("open", "Todo", etc.).
	State string `json:"state"`
	// CreatedAt is when the issue was filed.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the last-activity timestamp. Zero when unknown.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// URL is the canonical web URL of the issue.
	URL string `json:"url"`
}

// WorkSource is the Step 01 abstraction: it enumerates actionable work items
// from an external planning tool. The returned slice contains only open /
// actionable items — filtering by state, label, hold, etc. is the adapter's
// responsibility.
type WorkSource interface {
	// SourceType returns the kind string ("github", "github_projects", "linear",
	// "jira") for use in dashboard badges and log fields.
	SourceType() string
	// ListIssues returns the current actionable work items.
	ListIssues(ctx context.Context) ([]Issue, error)
}
