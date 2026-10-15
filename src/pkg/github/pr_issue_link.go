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
	// Skipped names why no link was attempted ("pr-not-open",
	// "issue-not-open", "cached"); empty when a mutation ran or the pair was
	// already linked.
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

func (c *closingLinkCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, key)
}

// retractKey namespaces the memo of pairs whose hive-made closing link has
// been checked for retraction, so it cannot collide with the ensure memo for
// the same pair.
func (c *closingLinkCache) retractKey(owner, repo string, issue, pr int) string {
	return "retract:" + c.key(owner, repo, issue, pr)
}

// EnsureClosingIssueLink makes GitHub's Development sidebar — and therefore
// the linked-PR glyph in the issue list — show repo#prNumber on
// repo#issueNumber whenever GitHub has not already derived that link
// (hivecommons/hive#11888).
//
// GitHub only derives closingIssuesReferences from `Fixes #N` keywords, and
// only for PRs that target the repository's default branch, so two kinds of
// PR hive labels `🐝 covered by PR` are invisible in the issue list: every
// release-line PR (`v6` fixes while `v5` is the default) and any PR whose
// claim hive recovered from evidence other than a closing keyword (the
// issue number in the branch name). The `addCloseIssueReferences` GraphQL
// mutation is the API behind the sidebar's manual "link a pull request"
// action and works for any base branch; it is idempotent on GitHub's side,
// and this client additionally memoises each pair so repeated syncs cost
// nothing. The base branch is reported but no longer gates the call: the
// lookup's closingIssuesReferences is the source of truth, so a
// default-branch PR GitHub already linked is a no-op and one it did not
// link is linked here.
//
// The call never links when either side is not open or when the lookup
// fails. Callers treat it as best-effort — a failure here must never fail a
// PR open or an enumeration tick — and failures are not memoised, so the
// next PR-claim sweep retries the pair.
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
			c.logger.Warn("CreatePR: linking issue to PR failed; the PR-claim sweep will retry",
				slog.String("repo", repo), slog.Int("number", prNumber), slog.Int("issue", issue), slog.String("error", err.Error()))
			continue
		}
		if res.Linked {
			c.logger.Info("CreatePR: linked issue to PR so the issue list shows it",
				slog.String("repo", repo), slog.Int("number", prNumber), slog.Int("issue", issue), slog.String("base", res.Base))
		}
	}
}

// ClosingLinkRetraction reports what RetractHiveClosingIssueLink did for one
// issue↔PR pair.
type ClosingLinkRetraction struct {
	// Retracted is true when this call removed a closing link the hive had
	// added.
	Retracted bool
	// Skipped names why nothing was removed: "not-linked" (GitHub lists no
	// closing link for the pair), "not-hive-linked" (the link exists but
	// the connecting actor is not this hive's App account, or the client
	// has no App login to compare against), or "cached".
	Skipped string
	// LinkedBy is the login that connected the pair, when GitHub recorded
	// one.
	LinkedBy string
}

// RetractHiveClosingIssueLink removes a GitHub closing link repo#prNumber →
// repo#issueNumber that this hive added itself and that its current claim no
// longer justifies (hivecommons/hive#12144). The PR-claim sweep calls it for
// every Reference claim ("Refs #N", "Part of #N"): such a PR deliberately
// does not close the issue, so a closing link — which makes GitHub close the
// issue when the PR merges — is wrong, whether it was added while the body
// still said `Closes #N` or by a sweep that graded the claim as strong (the
// #12025→#12005 case: a branch-name claim outranked the body's `Refs`).
//
// Only links the hive made are removed. GitHub records a ConnectedEvent on
// the PR for every manual link, and the sidebar's manual link and
// addCloseIssueReferences are the same action, so the most recent connect
// for the issue tells who linked it: when that actor is this hive's App
// account (Client.SetAppBotLogin, compared without the `[bot]` suffix
// GraphQL omits) the pair is unlinked with removeCloseIssueReferences;
// otherwise — a person linked it, or the hive authenticates with a PAT and
// cannot tell its own links from a person's — it is left alone and reported
// as "not-hive-linked" so the caller can log it. A link GitHub derived from
// a closing keyword never reaches this path: the body would have graded the
// claim as Closing, not Reference.
//
// Like EnsureClosingIssueLink the call is best-effort and memoised per pair,
// and failures are not memoised so the next sweep retries. A retraction also
// forgets the ensure memo for the pair, so a body edited back to `Closes #N`
// is linked again on the following sweep.
func (c *Client) RetractHiveClosingIssueLink(ctx context.Context, repo string, prNumber, issueNumber int) (ClosingLinkRetraction, error) {
	if c == nil || c.client == nil {
		return ClosingLinkRetraction{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	if owner == "" || name == "" || prNumber <= 0 || issueNumber <= 0 {
		return ClosingLinkRetraction{}, fmt.Errorf("RetractHiveClosingIssueLink: repo, pr and issue are required")
	}
	key := c.closingLinks.retractKey(owner, name, issueNumber, prNumber)
	if c.closingLinks.has(key) {
		return ClosingLinkRetraction{Skipped: "cached"}, nil
	}

	var out struct {
		Repository struct {
			PullRequest struct {
				ID                      string `json:"id"`
				ClosingIssuesReferences struct {
					Nodes []struct {
						Number     int `json:"number"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
				TimelineItems struct {
					Nodes []struct {
						Actor struct {
							Login string `json:"login"`
						} `json:"actor"`
						Subject struct {
							Number int `json:"number"`
						} `json:"subject"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
			Issue struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"repository"`
	}
	query := `query($owner:String!, $name:String!, $pr:Int!, $issue:Int!) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$pr) {
      id
      closingIssuesReferences(first:50) {
        nodes { number repository { nameWithOwner } }
      }
      timelineItems(last:100, itemTypes:[CONNECTED_EVENT]) {
        nodes { ... on ConnectedEvent { actor { login } subject { ... on Issue { number } } } }
      }
    }
    issue(number:$issue) { id }
  }
}`
	vars := map[string]any{"owner": owner, "name": name, "pr": prNumber, "issue": issueNumber}
	if err := c.graphQL(WithRESTCaller(ctx, "hive:retract_closing_link"), query, vars, &out); err != nil {
		return ClosingLinkRetraction{}, fmt.Errorf("RetractHiveClosingIssueLink %s/%s#%d->#%d lookup: %w", owner, name, issueNumber, prNumber, err)
	}
	pr := out.Repository.PullRequest
	res := ClosingLinkRetraction{}
	wantRepo := owner + "/" + name
	linked := false
	for _, node := range pr.ClosingIssuesReferences.Nodes {
		if node.Number == issueNumber && strings.EqualFold(node.Repository.NameWithOwner, wantRepo) {
			linked = true
			break
		}
	}
	if !linked {
		res.Skipped = "not-linked"
		c.closingLinks.mark(key)
		return res, nil
	}
	// timelineItems(last:N) is oldest→newest, so the last matching node is
	// the connect that produced the current link.
	for _, node := range pr.TimelineItems.Nodes {
		if node.Subject.Number == issueNumber {
			res.LinkedBy = node.Actor.Login
		}
	}
	if res.LinkedBy == "" || !c.isOwnBotLogin(res.LinkedBy) {
		res.Skipped = "not-hive-linked"
		c.closingLinks.mark(key)
		return res, nil
	}
	if pr.ID == "" || out.Repository.Issue.ID == "" {
		return res, fmt.Errorf("RetractHiveClosingIssueLink %s/%s#%d->#%d: lookup returned no node ids", owner, name, issueNumber, prNumber)
	}

	mutation := `mutation($issue:ID!, $prs:[ID!]!) {
  removeCloseIssueReferences(input:{issueId:$issue, pullRequestIds:$prs}) {
    issue { number }
  }
}`
	mvars := map[string]any{"issue": out.Repository.Issue.ID, "prs": []string{pr.ID}}
	if err := c.graphQL(WithRESTCaller(ctx, "hive:retract_closing_link"), mutation, mvars, nil); err != nil {
		return res, fmt.Errorf("RetractHiveClosingIssueLink %s/%s#%d->#%d removeCloseIssueReferences: %w", owner, name, issueNumber, prNumber, err)
	}
	res.Retracted = true
	c.closingLinks.mark(key)
	c.closingLinks.forget(c.closingLinks.key(owner, name, issueNumber, prNumber))
	return res, nil
}

// isOwnBotLogin reports whether login is this hive's App bot account.
// GraphQL actor logins omit the `[bot]` suffix REST appends, so both
// spellings compare equal.
func (c *Client) isOwnBotLogin(login string) bool {
	login = strings.TrimSuffix(strings.TrimSpace(login), "[bot]")
	if login == "" {
		return false
	}
	for _, own := range c.hiveBotLogins() {
		if strings.EqualFold(strings.TrimSuffix(own, "[bot]"), login) {
			return true
		}
	}
	return false
}
