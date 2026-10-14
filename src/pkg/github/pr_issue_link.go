package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// ClosingLinkResult reports what EnsureClosingIssueLink did for one
// issue↔PR pair.
type ClosingLinkResult struct {
	// Linked is true when this call created the GitHub closing reference.
	Linked bool
	// AlreadyLinked is true when GitHub already listed the issue in the PR's
	// closingIssuesReferences, so nothing was sent.
	AlreadyLinked bool
	// Skipped names why no link was attempted ("default-branch",
	// "pr-not-open", "issue-not-open", "cached"); empty when a mutation ran
	// or the pair was already linked.
	Skipped string
	// Base is the PR's base branch as observed by the lookup.
	Base string
}

// closingLinkCache memoises issue↔PR pairs this process has already ensured,
// so the PR-claim label sync does not spend a GraphQL query per pair per
// enumeration tick. Keys are "owner/repo#issue->pr".
type closingLinkCache struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (c *closingLinkCache) key(owner, repo string, issue, pr int) string {
	return fmt.Sprintf("%s/%s#%d->%d", strings.ToLower(owner), strings.ToLower(repo), issue, pr)
}

func (c *closingLinkCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.seen[key]
	return ok
}

func (c *closingLinkCache) mark(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]struct{}{}
	}
	c.seen[key] = struct{}{}
}

// EnsureClosingIssueLink makes GitHub's Development sidebar — and therefore
// the linked-PR glyph in the issue list — show repo#prNumber on
// repo#issueNumber when the PR targets a NON-default branch
// (hivecommons/hive#11888).
//
// GitHub only derives closingIssuesReferences from `Fixes #N` keywords for
// PRs that target the repository's default branch, so every release-line PR
// (`v6` fixes while `v5` is the default) is invisible in the issue list. The
// `addCloseIssueReferences` GraphQL mutation is the API behind the sidebar's
// manual "link a pull request" action and works for any base branch; it is
// idempotent on GitHub's side, and this client additionally memoises each
// pair so repeated syncs cost nothing.
//
// The call is intentionally narrow: it never links when the PR already
// targets the default branch (GitHub does that itself), when either side is
// not open, or when the lookup fails. Callers treat it as best-effort — a
// failure here must never fail a PR open or an enumeration tick.
func (c *Client) EnsureClosingIssueLink(ctx context.Context, repo string, prNumber, issueNumber int) (ClosingLinkResult, error) {
	if c == nil || c.client == nil {
		return ClosingLinkResult{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" || prNumber <= 0 || issueNumber <= 0 {
		return ClosingLinkResult{}, fmt.Errorf("EnsureClosingIssueLink: repo, pr and issue are required")
	}
	key := c.closingLinks.key(owner, name, issueNumber, prNumber)
	if c.closingLinks.has(key) {
		return ClosingLinkResult{Skipped: "cached"}, nil
	}

	var out struct {
		Repository struct {
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
			PullRequest struct {
				ID                      string `json:"id"`
				State                   string `json:"state"`
				BaseRefName             string `json:"baseRefName"`
				ClosingIssuesReferences struct {
					Nodes []struct {
						Number     int `json:"number"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
			} `json:"pullRequest"`
			Issue struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"issue"`
		} `json:"repository"`
	}
	query := `query($owner:String!, $name:String!, $pr:Int!, $issue:Int!) {
  repository(owner:$owner, name:$name) {
    defaultBranchRef { name }
    pullRequest(number:$pr) {
      id
      state
      baseRefName
      closingIssuesReferences(first:50) {
        nodes { number repository { nameWithOwner } }
      }
    }
    issue(number:$issue) { id state }
  }
}`
	vars := map[string]any{"owner": owner, "name": name, "pr": prNumber, "issue": issueNumber}
	if err := c.graphQL(WithRESTCaller(ctx, "hive:ensure_closing_link"), query, vars, &out); err != nil {
		return ClosingLinkResult{}, fmt.Errorf("EnsureClosingIssueLink %s/%s#%d->#%d lookup: %w", owner, name, issueNumber, prNumber, err)
	}
	pr := out.Repository.PullRequest
	res := ClosingLinkResult{Base: strings.TrimSpace(pr.BaseRefName)}
	wantRepo := owner + "/" + name
	for _, node := range pr.ClosingIssuesReferences.Nodes {
		if node.Number == issueNumber && strings.EqualFold(node.Repository.NameWithOwner, wantRepo) {
			res.AlreadyLinked = true
			c.closingLinks.mark(key)
			return res, nil
		}
	}
	if strings.EqualFold(res.Base, strings.TrimSpace(out.Repository.DefaultBranchRef.Name)) {
		// GitHub derives default-branch references itself; a missing one
		// there means the body has no closing keyword, not that the
		// derivation failed, so do not second-guess it.
		res.Skipped = "default-branch"
		c.closingLinks.mark(key)
		return res, nil
	}
	if !strings.EqualFold(pr.State, "OPEN") {
		res.Skipped = "pr-not-open"
		c.closingLinks.mark(key)
		return res, nil
	}
	if !strings.EqualFold(out.Repository.Issue.State, "OPEN") {
		res.Skipped = "issue-not-open"
		c.closingLinks.mark(key)
		return res, nil
	}
	if pr.ID == "" || out.Repository.Issue.ID == "" {
		return res, fmt.Errorf("EnsureClosingIssueLink %s/%s#%d->#%d: lookup returned no node ids", owner, name, issueNumber, prNumber)
	}

	mutation := `mutation($issue:ID!, $prs:[ID!]!) {
  addCloseIssueReferences(input:{issueId:$issue, pullRequestIds:$prs}) {
    issue { number }
  }
}`
	mvars := map[string]any{"issue": out.Repository.Issue.ID, "prs": []string{pr.ID}}
	if err := c.graphQL(WithRESTCaller(ctx, "hive:ensure_closing_link"), mutation, mvars, nil); err != nil {
		return res, fmt.Errorf("EnsureClosingIssueLink %s/%s#%d->#%d addCloseIssueReferences: %w", owner, name, issueNumber, prNumber, err)
	}
	res.Linked = true
	c.closingLinks.mark(key)
	return res, nil
}

// linkClosingIssuesForPR runs EnsureClosingIssueLink for every same-repo
// issue the PR body closes, logging rather than returning failures. It is
// the CreatePR hook: a PR that has just been opened against a release line
// gets its issue links before anyone looks at the issue list.
func (c *Client) linkClosingIssuesForPR(ctx context.Context, owner, repo string, prNumber int, body string) {
	if c == nil || c.client == nil || prNumber <= 0 {
		return
	}
	for issue := range closingIssueRefs(body, owner, repo) {
		res, err := c.EnsureClosingIssueLink(ctx, owner+"/"+repo, prNumber, issue)
		if err != nil {
			c.logger.Warn("CreatePR: linking issue to non-default-branch PR failed",
				slog.String("repo", repo), slog.Int("number", prNumber), slog.Int("issue", issue), slog.String("error", err.Error()))
			continue
		}
		if res.Linked {
			c.logger.Info("CreatePR: linked issue to non-default-branch PR so the issue list shows it",
				slog.String("repo", repo), slog.Int("number", prNumber), slog.Int("issue", issue), slog.String("base", res.Base))
		}
	}
}
