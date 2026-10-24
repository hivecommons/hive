package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Use the claim freshness window for reachability too. updated_at is not
// evidence of presence: a bot or another reviewer can update an abandoned PR.
const delegatedContributorActivityWindow = 4 * time.Hour

func (s *Server) gatherDelegatedCompetition(ctx context.Context, repo string, pr *gh.PullRequest, in *delegatedecision.Input, now time.Time) error {
	client := s.deps.GHClient
	api := client.GoGitHub()
	owner, name, _ := strings.Cut(repo, "/")
	refs := ghpkg.ParseClaimedIssues(pr.GetBody(), repo)
	if len(refs) == 0 {
		refs = ghpkg.ParseReferencedIssues(pr.GetBody(), repo)
	}
	// Claims also cover contributors working under dibs without a PR yet.
	// They are presence evidence, not synthetic PRs with a zero number.
	var issues []*gh.Issue
	claimedOnline := false
	for _, ref := range refs {
		o, r, _ := strings.Cut(ref.Repo, "/")
		issue, _, err := api.Issues.Get(ctx, o, r, ref.Number)
		if err != nil {
			return fmt.Errorf("reading linked issue %s#%d: %w", ref.Repo, ref.Number, err)
		}
		issues = append(issues, issue)
		claim, live, err := client.LookupLiveIssueClaim(ctx, ref.Repo, issue, now)
		if err != nil {
			return fmt.Errorf("reading linked issue claim: %w", err)
		}
		if live && strings.EqualFold(claim.Identity, pr.GetUser().GetLogin()) {
			claimedOnline = true
		}
	}
	if !in.HiveAuthored {
		online, err := delegatedContributorOnline(ctx, api, owner, name, pr, refs, issues, now)
		if err != nil {
			return fmt.Errorf("reading contributor activity: %w", err)
		}
		in.ContributorOnline = online || claimedOnline
	}
	if len(refs) == 0 {
		return nil
	}
	// List is paginated: missing a later-page contributor duplicate could
	// otherwise release the hive's competing PR ahead of it.
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: delegatedDecisionGatherLimit}}
	for {
		prs, resp, err := api.PullRequests.List(ctx, owner, name, opts)
		if err != nil {
			return fmt.Errorf("reading competing PRs: %w", err)
		}
		for _, other := range prs {
			if other.GetNumber() == pr.GetNumber() || other.GetState() != "open" || !delegatedSharesIssue(other.GetBody(), repo, refs) {
				continue
			}
			// List responses do not include computed mergeability. Only a
			// known clean, non-draft head is adequate to close a duplicate.
			detail, _, err := api.PullRequests.Get(ctx, owner, name, other.GetNumber())
			if err != nil {
				return fmt.Errorf("reading competing PR #%d: %w", other.GetNumber(), err)
			}
			if detail.GetState() != "open" {
				continue
			}
			state := detail.GetMergeableState()
			in.Competing = append(in.Competing, delegatedecision.CompetingPR{
				Number: detail.GetNumber(), HiveAuthored: client.IsHiveAuthorLogin(detail.GetUser().GetLogin()),
				Mergeable: detail.GetMergeable() && !detail.GetDraft() && (state == "clean" || state == "has_hooks"),
				Current:   !detail.GetDraft() && state != "behind" && state != "unknown" && state != "",
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return nil
}

func delegatedSharesIssue(body, repo string, refs []ghpkg.ClaimedRef) bool {
	other := ghpkg.ParseClaimedIssues(body, repo)
	if len(other) == 0 {
		other = ghpkg.ParseReferencedIssues(body, repo)
	}
	for _, a := range refs {
		for _, b := range other {
			if a.Number == b.Number && strings.EqualFold(a.Repo, b.Repo) {
				return true
			}
		}
	}
	return false
}

func delegatedContributorOnline(ctx context.Context, api *gh.Client, owner, repo string, pr *gh.PullRequest, refs []ghpkg.ClaimedRef, issues []*gh.Issue, now time.Time) (bool, error) {
	author := pr.GetUser().GetLogin()
	if author == "" {
		return false, fmt.Errorf("PR #%d has no author", pr.GetNumber())
	}
	last := pr.GetCreatedAt().Time
	note := func(login string, at time.Time) {
		if author != "" && strings.EqualFold(login, author) && at.After(last) {
			last = at
		}
	}
	comments := func(owner, repo string, number int) error {
		opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: delegatedDecisionGatherLimit}}
		for {
			items, resp, err := api.Issues.ListComments(ctx, owner, repo, number, opts)
			if err != nil {
				return err
			}
			for _, c := range items {
				note(c.GetUser().GetLogin(), c.GetCreatedAt().Time)
				note(c.GetUser().GetLogin(), c.GetUpdatedAt().Time)
			}
			if resp == nil || resp.NextPage == 0 {
				return nil
			}
			opts.Page = resp.NextPage
		}
	}
	if err := comments(owner, repo, pr.GetNumber()); err != nil {
		return false, err
	}
	for i, issue := range issues {
		note(issue.GetUser().GetLogin(), issue.GetCreatedAt().Time)
		o, r, _ := strings.Cut(refs[i].Repo, "/")
		if err := comments(o, r, issue.GetNumber()); err != nil {
			return false, err
		}
	}
	opts := &gh.ListOptions{PerPage: delegatedDecisionGatherLimit}
	for {
		commits, resp, err := api.PullRequests.ListCommits(ctx, owner, repo, pr.GetNumber(), opts)
		if err != nil {
			return false, err
		}
		for _, c := range commits {
			note(c.GetAuthor().GetLogin(), c.GetCommit().GetAuthor().GetDate().Time)
			note(c.GetCommitter().GetLogin(), c.GetCommit().GetCommitter().GetDate().Time)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	commentOpts := &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: delegatedDecisionGatherLimit}}
	for {
		items, resp, err := api.PullRequests.ListComments(ctx, owner, repo, pr.GetNumber(), commentOpts)
		if err != nil {
			return false, err
		}
		for _, c := range items {
			note(c.GetUser().GetLogin(), c.GetCreatedAt().Time)
			note(c.GetUser().GetLogin(), c.GetUpdatedAt().Time)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		commentOpts.Page = resp.NextPage
	}
	opts = &gh.ListOptions{PerPage: delegatedDecisionGatherLimit}
	for {
		reviews, resp, err := api.PullRequests.ListReviews(ctx, owner, repo, pr.GetNumber(), opts)
		if err != nil {
			return false, err
		}
		for _, r := range reviews {
			note(r.GetUser().GetLogin(), r.GetSubmittedAt().Time)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return !last.IsZero() && !last.Before(now.Add(-delegatedContributorActivityWindow)), nil
}
