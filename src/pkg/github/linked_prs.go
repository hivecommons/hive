package github

import (
	"context"
	"strings"
)

// PRClosesIssue reports whether GitHub's own closingIssuesReferences relation
// links repo#prNumber to repo#issueNumber. This is intentionally stronger than
// parsing PR text: only GitHub's resolved relationship (or an operator action)
// may turn a pending "likely done" PR claim into an already-done issue.
func (c *Client) PRClosesIssue(ctx context.Context, repo string, prNumber, issueNumber int) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" || prNumber <= 0 || issueNumber <= 0 {
		return false, nil
	}
	var out struct {
		Repository struct {
			PullRequest struct {
				ClosingIssuesReferences struct {
					Nodes []struct {
						Number     int `json:"number"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	query := `query($owner:String!, $name:String!, $pr:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$pr) {
      closingIssuesReferences(first:50) {
        nodes { number repository { nameWithOwner } }
      }
    }
  }
}`
	if err := c.graphQL(ctx, query, map[string]any{"owner": owner, "name": name, "pr": prNumber}, &out); err != nil {
		return false, err
	}
	wantRepo := owner + "/" + name
	for _, node := range out.Repository.PullRequest.ClosingIssuesReferences.Nodes {
		if node.Number == issueNumber && strings.EqualFold(node.Repository.NameWithOwner, wantRepo) {
			return true, nil
		}
	}
	return false, nil
}
