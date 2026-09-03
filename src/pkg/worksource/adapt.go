package worksource

import (
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// ToGitHubIssues converts a worksource Issue slice into the github.Issue shape
// that the scheduler and governor consume. Classification fields not present in
// the worksource type (ComplexityTier, ModelRec, Lane) are left at their zero
// values and will be populated downstream by classify.
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
		dependsOn := make([]github.IssueDependency, 0, len(ws.DependsOn))
		for _, dep := range ws.DependsOn {
			if key := dep.Ref.Key(); key != "" {
				dependsOn = append(dependsOn, github.IssueDependency{Key: key, Resolved: dep.Resolved})
			}
		}
		out[i] = github.Issue{
			Repo:       ws.Repo,
			Number:     ws.Number,
			SourceType: ws.SourceType,
			ExternalID: ws.ExternalID,
			DependsOn:  dependsOn,
			Title:      ws.Title,
			Author:     ws.Author,
			Labels:     ws.Labels,
			Assignees:  ws.Assignees,
			Priority:   ws.Priority,
			State:      ws.State,
			CreatedAt:  ws.CreatedAt,
			UpdatedAt:  ws.UpdatedAt,
			AgeMinutes: ageMinutes,
			URL:        ws.URL,
			IsTracker:  ws.IsTracker,
		}
	}
	return out
}
