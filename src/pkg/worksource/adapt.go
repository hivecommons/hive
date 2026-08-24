package worksource

import (
	"time"

	"github.com/kubestellar/hive/pkg/github"
)

// ToGitHubIssues converts a worksource Issue slice into the github.Issue shape
// that the scheduler and governor consume. GitHub-native fields not present in
// the worksource type (IsTracker, ComplexityTier, ModelRec, Lane) are left at
// their zero values and will be populated downstream by classify. AgeMinutes is
// derived here because downstream SLA accounting consumes this projection.
//
// SourceType and ExternalID are carried across (kubestellar/hive#4245). They are
// the item's ONLY identity when Number is 0, which is every Linear and Jira
// item: without them the projection is lossy and every downstream site collapses
// distinct work onto "repo#0". GitHub-backed items keep Number, so their keys
// stay byte-identical "repo#number".
func ToGitHubIssues(issues []Issue) []github.Issue {
	out := make([]github.Issue, len(issues))
	now := time.Now()
	for i, ws := range issues {
		ageMinutes := 0
		if !ws.CreatedAt.IsZero() {
			ageMinutes = int(now.Sub(ws.CreatedAt).Minutes())
			if ageMinutes < 0 {
				ageMinutes = 0
			}
		}
		out[i] = github.Issue{
			Repo:       ws.Repo,
			Number:     ws.Number,
			SourceType: ws.SourceType,
			ExternalID: ws.ExternalID,
			Title:      ws.Title,
			Author:     ws.Author,
			Labels:     ws.Labels,
			Assignees:  ws.Assignees,
			CreatedAt:  ws.CreatedAt,
			UpdatedAt:  ws.UpdatedAt,
			AgeMinutes: ageMinutes,
			URL:        ws.URL,
		}
	}
	return out
}
