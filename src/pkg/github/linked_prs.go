package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var configuredVersionLinePattern = regexp.MustCompile(`^v[0-9]+$`)

// PRCloseEvidence describes why a PR is accepted as closing an issue.
type PRCloseEvidence struct {
	Closes                 bool
	GitHubRelation         bool
	ConfiguredLineKeyword  bool
	Branch                 string
	MatchedClosingFragment string
}

// PRClosesIssue reports whether GitHub's own closingIssuesReferences relation
// links repo#prNumber to repo#issueNumber, or whether a merged PR targeting a
// configured release line carries an explicit GitHub closing keyword for the
// issue. GitHub only populates closingIssuesReferences for default-branch PRs;
// the configured-line fallback preserves that behavior for release lines hive
// itself scans.
func (c *Client) PRClosesIssue(ctx context.Context, repo string, prNumber, issueNumber int) (bool, error) {
	evidence, err := c.PRCloseEvidence(ctx, repo, prNumber, issueNumber)
	return evidence.Closes, err
}

// PRCloseEvidence returns the closing relationship evidence used by
// PRClosesIssue. GitHub's relation remains authoritative and is checked first;
// only when it is absent do we parse explicit closing keywords on configured
// release lines.
func (c *Client) PRCloseEvidence(ctx context.Context, repo string, prNumber, issueNumber int) (PRCloseEvidence, error) {
	if c == nil || c.client == nil {
		return PRCloseEvidence{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" || prNumber <= 0 || issueNumber <= 0 {
		return PRCloseEvidence{}, nil
	}
	var out struct {
		Repository struct {
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			PullRequest struct {
				Body                    string `json:"body"`
				BaseRefName             string `json:"baseRefName"`
				Merged                  bool   `json:"merged"`
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
    defaultBranchRef { name }
    pullRequest(number:$pr) {
      body
      baseRefName
      merged
      closingIssuesReferences(first:50) {
        nodes { number repository { nameWithOwner } }
      }
    }
  }
}`
	if err := c.graphQL(ctx, query, map[string]any{"owner": owner, "name": name, "pr": prNumber}, &out); err != nil {
		return PRCloseEvidence{}, err
	}
	wantRepo := owner + "/" + name
	for _, node := range out.Repository.PullRequest.ClosingIssuesReferences.Nodes {
		if node.Number == issueNumber && strings.EqualFold(node.Repository.NameWithOwner, wantRepo) {
			return PRCloseEvidence{Closes: true, GitHubRelation: true, Branch: out.Repository.PullRequest.BaseRefName}, nil
		}
	}
	base := strings.TrimSpace(out.Repository.PullRequest.BaseRefName)
	if !out.Repository.PullRequest.Merged {
		return PRCloseEvidence{Branch: base}, nil
	}
	if !IsConfiguredIssueClosingLine(base, out.Repository.DefaultBranchRef.Name) {
		return PRCloseEvidence{Branch: base}, nil
	}
	fragment, ok := FindClosingKeywordReference(out.Repository.PullRequest.Body, wantRepo, wantRepo, issueNumber)
	if !ok {
		return PRCloseEvidence{Branch: base}, nil
	}
	return PRCloseEvidence{
		Closes:                 true,
		ConfiguredLineKeyword:  true,
		Branch:                 base,
		MatchedClosingFragment: fragment,
	}, nil
}

// IsConfiguredIssueClosingLine reports whether a PR base branch is a repo line
// hive may treat like the default branch for explicit closing keywords.
func IsConfiguredIssueClosingLine(branch, defaultBranch string) bool {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false
	}
	if strings.EqualFold(branch, strings.TrimSpace(defaultBranch)) {
		return true
	}
	return configuredVersionLinePattern.MatchString(branch)
}

// CloseIssueForConfiguredLinePR posts the release-line audit comment and closes
// the issue for a PRCloseEvidence fallback produced from an explicit keyword.
func (c *Client) CloseIssueForConfiguredLinePR(ctx context.Context, repo string, issueNumber, prNumber int, evidence PRCloseEvidence) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	branch := strings.TrimSpace(evidence.Branch)
	if branch == "" {
		branch = "the configured release line"
	}
	fragment := strings.TrimSpace(evidence.MatchedClosingFragment)
	if fragment == "" {
		fragment = fmt.Sprintf("Fixes #%d", issueNumber)
	}
	body := fmt.Sprintf("Closed by #%d (merged to `%s`). GitHub only auto-closes for default-branch PRs; hive applied the PR's `%s` on its configured `%s` line.",
		prNumber, branch, fragment, branch)
	if ok, err := c.IssueCommentsContain(ctx, repo, issueNumber, body); err != nil {
		return err
	} else if !ok {
		if err := c.CreateIssueComment(ctx, repo, issueNumber, body); err != nil {
			return err
		}
	}
	return c.CloseIssue(ctx, repo, issueNumber, IssueCloseOptions{
		OverrideReason:          fmt.Sprintf("explicit PR closing keyword on configured %s line via #%d", branch, prNumber),
		SuppressOverrideComment: true,
	})
}
