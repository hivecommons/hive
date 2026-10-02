package upstreamwatch

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// stateReasonNotPlanned is the GitHub state_reason of an issue closed as
// "not planned".
const stateReasonNotPlanned = "not_planned"

// markerSearchPerPage bounds the marker search: a marker is unique per
// upstream item, so a handful of results is plenty.
const markerSearchPerPage = 10

// GitHubFiler implements Filer over the GitHub REST API against the fork. It
// opens issues only, never PRs.
type GitHubFiler struct {
	client *gh.Client
	owner  string
	repo   string
}

// NewGitHubFiler returns a Filer for the fork at owner/repo.
func NewGitHubFiler(client *gh.Client, owner, repo string) *GitHubFiler {
	return &GitHubFiler{client: client, owner: owner, repo: repo}
}

// FindMarker implements Filer. Search is fuzzy, so every hit is confirmed by
// checking its body actually carries the marker.
func (g *GitHubFiler) FindMarker(ctx context.Context, marker string) (Existing, bool, error) {
	text := strings.TrimSuffix(strings.TrimPrefix(marker, "<!-- "), " -->")
	query := fmt.Sprintf(`repo:%s/%s is:issue in:body %q`, g.owner, g.repo, text)
	result, _, err := g.client.Search.Issues(ctx, query, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: markerSearchPerPage},
	})
	if err != nil {
		return Existing{}, false, fmt.Errorf("search %s/%s for %q: %w", g.owner, g.repo, text, err)
	}
	for _, issue := range result.Issues {
		if issue.IsPullRequest() || !strings.Contains(issue.GetBody(), marker) {
			continue
		}
		return Existing{Number: issue.GetNumber(), Dismissed: issueDismissed(issue)}, true, nil
	}
	return Existing{}, false, nil
}

// File implements Filer.
func (g *GitHubFiler) File(ctx context.Context, issue Issue) (int, error) {
	req := &gh.IssueRequest{Title: gh.Ptr(issue.Title), Body: gh.Ptr(issue.Body)}
	if len(issue.Labels) > 0 {
		labels := append([]string(nil), issue.Labels...)
		req.Labels = &labels
	}
	created, _, err := g.client.Issues.Create(ctx, g.owner, g.repo, req)
	if err != nil {
		return 0, fmt.Errorf("create issue on %s/%s: %w", g.owner, g.repo, err)
	}
	return created.GetNumber(), nil
}

// issueDismissed reports whether a fork issue was closed as "not planned" or
// carries DismissedLabel.
func issueDismissed(issue *gh.Issue) bool {
	if issue.GetState() == "closed" && issue.GetStateReason() == stateReasonNotPlanned {
		return true
	}
	for _, l := range issue.Labels {
		if strings.EqualFold(l.GetName(), DismissedLabel) {
			return true
		}
	}
	return false
}
