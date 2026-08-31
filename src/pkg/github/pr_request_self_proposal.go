package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// validateSelfProposalPRRequest is the mechanical backstop for
// kubestellar/hive#5117: an agent that files a proposal issue and then
// implements it itself supplies the APPEARANCE of a rationale — the PR cites
// the issue as justification — while nothing external ever agreed to the
// direction. The incident that motivated this (tuna-os/tunaos-packages#583,
// implementing "phase 1" of tuna-os/tunaos-packages#581, an issue the hive
// itself filed) had the code review pass clean; the gap is governance, not
// code quality, so a code-quality gate cannot see it. This one checks
// provenance and human acknowledgement instead.
//
// The rule: if this PR's title or body references an issue (closing OR
// non-closing — architect's own "Refs #N" convention for multi-phase work is
// exactly the form the incident used, so a closing-only check would miss it),
// and that issue was filed by a bot/App account, and no comment from a
// non-bot account exists on it, the PR request is rejected. A human comment
// of any kind — including a rejection — counts as the missing signal existing
// at all; it is not this gate's job to judge whether the comment approves.
//
// This is deliberately narrow and deliberately not airtight. It catches the
// reported pattern — an agent citing its own unacknowledged proposal as
// rationale — because that citation is what makes the proposal look
// sanctioned. It does NOT catch a PR that implements the same idea while
// citing nothing: no server-side check can require a link the PR chooses not
// to make, and the diff itself carries no signal that it originated from an
// issue at all. That gap is real and is not being oversold: closing it would
// need a rule like "every architectural PR must cite ITS OWN originating
// issue", which does not exist today and is a separate, larger policy
// question. What this gate removes is the specific incentive the issue
// describes — using a self-filed issue as citable-looking cover — without
// claiming to make self-authorization structurally impossible.
func (c *Client) validateSelfProposalPRRequest(ctx context.Context, req PRRequest) (reason string, err error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	owner, repo := c.prRequestRepo(req.Repo)
	defaultRepo := owner + "/" + repo
	text := req.Title + "\n" + req.Body

	refs := ParseClaimedIssues(text, defaultRepo)
	refs = append(refs, ParseReferencedIssues(text, defaultRepo)...)
	if len(refs) == 0 {
		return "", nil
	}

	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		refOwner, refRepo := c.prRequestRepo(ref.Repo)
		key := claimKey(strings.ToLower(refOwner+"/"+refRepo), ref.Issue)
		if seen[key] {
			continue
		}
		seen[key] = true

		issue, _, err := c.client.Issues.Get(ctx, refOwner, refRepo, ref.Issue)
		if err != nil {
			return "", fmt.Errorf("validating self-proposal gate for %s/%s#%d: %w", refOwner, refRepo, ref.Issue, err)
		}
		if !isBotAuthor(issue.GetUser()) {
			continue // human-filed proposal — the precondition this gate exists for is already met
		}
		acknowledged, err := c.issueHasHumanComment(ctx, refOwner, refRepo, ref.Issue)
		if err != nil {
			return "", fmt.Errorf("checking human acknowledgement on %s/%s#%d: %w", refOwner, refRepo, ref.Issue, err)
		}
		if !acknowledged {
			return fmt.Sprintf(
				"%s/%s#%d is a bot-filed proposal with no human comment on it yet — an agent-filed issue does not by itself authorize an agent-implemented PR (kubestellar/hive#5117); get a human acknowledgement on the issue first",
				refOwner, refRepo, ref.Issue,
			), nil
		}
	}
	return "", nil
}

// issueHasHumanComment reports whether any non-bot account has commented on
// the issue. It deliberately does not attempt to classify comment CONTENT as
// approval, rejection, or a question — any of those is evidence a human
// looked at the proposal, which is the precondition this gate is checking
// for, not the outcome of a review.
func (c *Client) issueHasHumanComment(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return false, err
		}
		for _, comment := range comments {
			if !isBotAuthor(comment.GetUser()) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, nil
}

// isBotAuthor reports whether a GitHub user is a bot or App-installation
// account, using the same login-suffix-or-type idiom already established in
// findDigestComment: a "[bot]" login suffix or a GitHub-reported Bot type.
// Unlike findDigestComment this makes no attempt to distinguish "our own bot"
// from "some other bot" — for this gate any bot-authored issue is the
// self-proposal shape being guarded against, regardless of which hive or
// automation filed it.
func isBotAuthor(u *gh.User) bool {
	if u == nil {
		return false
	}
	login := u.GetLogin()
	return strings.HasSuffix(login, "[bot]") || u.GetType() == "Bot"
}
