package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// ContributorPRLinkResult reports how LinkContributorPRToIssue made a relay
// contributor's PR visible as the implementation of the issue it was
// dispatched for.
type ContributorPRLinkResult struct {
	// EditedBody is true when the hub appended `Fixes #N` to the PR body.
	EditedBody bool
	// SidebarLinked is true when this call added the GitHub closing reference
	// (Development sidebar) for the pair.
	SidebarLinked bool
	// Skipped names why the body was left alone: "pr-not-open",
	// "already-closing" (a closing keyword for the issue is already there),
	// "partial-reference" (the body says the issue stays open),
	// "author-mismatch" (the PR is not the verified contributor's). Empty
	// when the body was edited.
	Skipped string
	// Base is the PR's base branch as observed by the lookup.
	Base string
}

// LinkContributorPRToIssue makes the link between a relay-dispatched issue
// and the PR the contributor reported for it durable (hivecommons/hive#14763).
//
// The relay's task prompt only ASKS the contributor's model for `Closes #N`;
// hosted hives saw PRs (hivecommons/hive#14701 for #14633) arrive with an
// empty body, so the PR-claim ledger — which derives claims from PR text and
// branch names only — never saw the issue as covered. The issue stayed at
// stage/010-actionable for hours while its implementation sat in review as
// "no linked issue", and every lane was free to re-claim it.
//
// The caller has already verified the PR against GitHub (open, same base
// repo, authored by the contributor it dispatched to): this method only
// edits a PR whose author is that contributor login, never a stranger's.
// It appends `Fixes #N` when the body carries no closing keyword for the
// issue and does not say the issue stays open (`Refs`/`Part of`/leave-open
// wording — those already reach the ledger as reference claims). Whatever
// happened to the body, it then ensures the GitHub sidebar link so a
// non-default-base PR is visible in the issue list too. Best-effort: a
// failure must never affect the completion outcome.
func (c *Client) LinkContributorPRToIssue(ctx context.Context, repo string, prNumber, issueNumber int, contributorLogin string) (ContributorPRLinkResult, error) {
	if c == nil || c.client == nil {
		return ContributorPRLinkResult{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" || prNumber <= 0 || issueNumber <= 0 {
		return ContributorPRLinkResult{}, fmt.Errorf("LinkContributorPRToIssue: repo, pr and issue are required")
	}
	contributorLogin = strings.TrimSpace(contributorLogin)
	if contributorLogin == "" {
		return ContributorPRLinkResult{Skipped: "author-mismatch"}, nil
	}

	var out struct {
		Repository struct {
			PullRequest struct {
				State       string `json:"state"`
				BaseRefName string `json:"baseRefName"`
				Body        string `json:"body"`
				Author      struct {
					Login string `json:"login"`
				} `json:"author"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	query := `query($owner:String!, $name:String!, $pr:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$pr) {
      state
      baseRefName
      body
      author { login }
    }
  }
}`
	vars := map[string]any{"owner": owner, "name": name, "pr": prNumber}
	if err := c.graphQL(WithRESTCaller(ctx, "hive:contributor_pr_link"), query, vars, &out); err != nil {
		return ContributorPRLinkResult{}, fmt.Errorf("LinkContributorPRToIssue %s#%d->#%d lookup: %w", repo, issueNumber, prNumber, err)
	}
	pr := out.Repository.PullRequest
	res := ContributorPRLinkResult{Base: strings.TrimSpace(pr.BaseRefName)}
	fullRepo := owner + "/" + name
	switch {
	case !strings.EqualFold(pr.State, "OPEN"):
		res.Skipped = "pr-not-open"
		return res, nil
	case !strings.EqualFold(strings.TrimSpace(pr.Author.Login), contributorLogin):
		res.Skipped = "author-mismatch"
		return res, nil
	}
	if _, ok := FindClosingKeywordReference(pr.Body, fullRepo, fullRepo, issueNumber); ok {
		res.Skipped = "already-closing"
	} else if bodySaysIssueStaysOpen(pr.Body, fullRepo, issueNumber) {
		res.Skipped = "partial-reference"
	} else {
		body := strings.TrimRight(pr.Body, " \t\r\n")
		if body != "" {
			body += "\n\n"
		}
		body += fmt.Sprintf("Fixes #%d", issueNumber)
		if _, _, err := c.client.PullRequests.Edit(WithRESTCaller(ctx, "hive:contributor_pr_link"), owner, name, prNumber, &gh.PullRequest{Body: gh.Ptr(body)}); err != nil {
			return res, fmt.Errorf("LinkContributorPRToIssue %s#%d->#%d edit body: %w", repo, issueNumber, prNumber, err)
		}
		res.EditedBody = true
	}
	if res.Skipped == "partial-reference" {
		// A deliberately partial PR must not become the issue's closer.
		return res, nil
	}
	link, err := c.EnsureClosingIssueLink(ctx, fullRepo, prNumber, issueNumber)
	if err != nil {
		return res, err
	}
	res.SidebarLinked = link.Linked
	return res, nil
}
