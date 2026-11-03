package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// IssueActivity is one issue's open and close times as GitHub records them,
// for the evidence card's work-balance panel (hivecommons/hive#12517): the
// hive's audit log only sees the issue writes the hive mediated, so humans
// and third-party bots were invisible there. This is GitHub truth for every
// author.
type IssueActivity struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Author    string    `json:"author"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	ClosedAt  time.Time `json:"closed_at,omitzero"`
}

// IssueActivityKey is the owner/repo#number key the durable rollup uses.
func IssueActivityKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + fmt.Sprint(number)
}

const (
	issueActivityPerPage  = 100
	issueActivityMaxPages = 20
	// Author kinds; the same vocabulary as evidence.Author.Kind.
	issueActivityKindHuman = "human"
	issueActivityKindAgent = "agent"
	issueActivityKindBot   = "bot"
)

// IssueActivityAuthorKind classifies an issue author the way the evidence
// backfill classifies PR authors: an attribution trailer or hive identity
// footer in the body is an agent, a GitHub Bot account (or [bot] login) is a
// bot, everything else is a human. The hive's own App account files agent
// issues, so the body must be checked before the account type.
func IssueActivityAuthorKind(login, accountType, body string) string {
	if _, ok := ParseAttributionTrailer(body); ok {
		return issueActivityKindAgent
	}
	if attributionIdentityFooter.MatchString(body) || strings.Contains(body, "🐝 **Hive Agent**") {
		return issueActivityKindAgent
	}
	if strings.EqualFold(accountType, "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return issueActivityKindBot
	}
	return issueActivityKindHuman
}

// IssueActivitySince lists every issue in repo updated at or after since
// (opening and closing both bump updated_at, so this is every issue opened
// or closed in the window) and returns their open/close times. Pull
// requests are skipped. Sheds under REST budget pressure like the MTTR
// lookups, returning an error so the caller keeps its last rollup.
func (c *Client) IssueActivitySince(ctx context.Context, repo string, since time.Time) ([]IssueActivity, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("github client not configured")
	}
	if c.shouldShedMTTRIssueFetches() {
		return nil, fmt.Errorf("github REST budget below reserve %d; skipping issue activity for %s", lowValueRESTBudgetFloor, repo)
	}
	owner, name := c.splitRepo(repo)
	opts := &gh.IssueListByRepoOptions{
		State:       "all",
		Since:       since.UTC(),
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: issueActivityPerPage},
	}
	var out []IssueActivity
	for page := 0; page < issueActivityMaxPages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("listing issues for %s since %s: %w", repo, since.Format(time.RFC3339), err)
		}
		for _, is := range issues {
			if is == nil || is.IsPullRequest() || is.GetNumber() <= 0 {
				continue
			}
			a := IssueActivity{
				Repo:      repo,
				Number:    is.GetNumber(),
				Author:    is.GetUser().GetLogin(),
				Kind:      IssueActivityAuthorKind(is.GetUser().GetLogin(), is.GetUser().GetType(), is.GetBody()),
				CreatedAt: is.GetCreatedAt().Time.UTC(),
			}
			if !is.GetClosedAt().IsZero() && strings.EqualFold(is.GetState(), "closed") {
				a.ClosedAt = is.GetClosedAt().Time.UTC()
			}
			out = append(out, a)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}
