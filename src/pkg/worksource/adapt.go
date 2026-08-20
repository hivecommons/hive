package worksource

import "github.com/kubestellar/hive/pkg/github"

// ToGitHubIssues converts a worksource Issue slice into the github.Issue shape
// that the scheduler and governor consume. GitHub-native fields not present in
// the worksource type (AgeMinutes, IsTracker, ComplexityTier, ModelRec, Lane)
// are left at their zero values and will be populated downstream by classify.
func ToGitHubIssues(issues []Issue) []github.Issue {
	out := make([]github.Issue, len(issues))
	for i, ws := range issues {
		out[i] = github.Issue{
			Repo:      ws.Repo,
			Number:    ws.Number,
			Title:     ws.Title,
			Author:    ws.Author,
			Labels:    ws.Labels,
			Assignees: ws.Assignees,
			CreatedAt: ws.CreatedAt,
			UpdatedAt: ws.UpdatedAt,
			URL:       ws.URL,
		}
	}
	return out
}
