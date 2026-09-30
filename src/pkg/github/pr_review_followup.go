package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Human "Changes requested" reviews on PRs the hive opened
// (hivecommons/hive#9802).
//
// A held PR carries GitHub's review decision already (protection_facts.go),
// but "a maintainer requested changes" is not by itself actionable: the
// owning agent may have answered it on an earlier tick. The question every
// consumer actually asks is whether the review is still UNADDRESSED, and both
// consumers — the follow-up router (prfollowupwire.go) and the hold-gated PR
// list (pkg/scheduler/policy_overlays.go) — must answer it the same way, or
// an agent is told to fix a review it already fixed.
//
// The rule lives here, next to the fetch that feeds it, because both consumers
// already depend on this package and the facts it needs (commit parents,
// comment authorship) are GitHub's.

// ReviewAddressedBy names the evidence that answered a review.
const (
	// ReviewAddressedByCommit is a non-merge commit pushed after the review.
	ReviewAddressedByCommit = "commit"
	// ReviewAddressedByReply is a hive reply on the PR after the review.
	ReviewAddressedByReply = "reply"
)

// prReviewFollowUpCommitWindow and prReviewFollowUpCommentWindow bound how
// much of a PR's tail one fetch reads. Only activity AFTER the review matters,
// and a review that is this far behind the branch tip has been overtaken by
// ordinary work anyway.
const (
	prReviewFollowUpCommitWindow  = 50
	prReviewFollowUpCommentWindow = 50
)

// PRReviewFollowUp is the latest opinionated HUMAN review on a PR the hive
// opened, plus whether the owning agent has answered it yet.
//
// It is stamped only on PRs whose review decision is CHANGES_REQUESTED, so a
// nil pointer means "no unaddressed human change request is known", never
// "approved".
type PRReviewFollowUp struct {
	// Reviewer is the login whose latest opinionated review requested
	// changes, and SubmittedAt is when they submitted it.
	Reviewer    string    `json:"reviewer,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitempty"`
	// Addressed reports whether the review has been answered per
	// ReviewAddressed, and AddressedBy names the evidence
	// (ReviewAddressedByCommit / ReviewAddressedByReply) or is empty.
	Addressed   bool   `json:"addressed,omitempty"`
	AddressedBy string `json:"addressed_by,omitempty"`
}

// PRCommitSummary is one commit on a PR's head branch: when it landed and how
// many parents it has. Parents > 1 is a merge commit.
type PRCommitSummary struct {
	CommittedAt time.Time
	Parents     int
}

// Merge reports whether the commit is a merge commit (more than one parent).
func (c PRCommitSummary) Merge() bool { return c.Parents > 1 }

// ReviewAddressed reports whether a review submitted at reviewedAt has been
// answered, and by what.
//
// A review counts as addressed only when, strictly after reviewedAt, there is
// either a non-merge commit on the PR's head branch or a reply the hive
// posted on the PR. A merge commit alone NEVER addresses a review: the
// held-PR CI/conflict repair path (hivecommons/hive#7438) pushes
// merge-from-base commits onto held PRs purely to keep them mergeable, and
// treating one as an answer is exactly how Danathar/goodreads-mcp#252 looked
// handled while the review had not been read at all.
func ReviewAddressed(reviewedAt time.Time, commits []PRCommitSummary, replies []time.Time) (bool, string) {
	if reviewedAt.IsZero() {
		return false, ""
	}
	for _, c := range commits {
		if c.Merge() || c.CommittedAt.IsZero() {
			continue
		}
		if c.CommittedAt.After(reviewedAt) {
			return true, ReviewAddressedByCommit
		}
	}
	for _, at := range replies {
		if !at.IsZero() && at.After(reviewedAt) {
			return true, ReviewAddressedByReply
		}
	}
	return false, ""
}

// Summary renders the review state for a kick's PR list, e.g.
// "CHANGES REQUESTED by @maintainer (unaddressed)".
func (f *PRReviewFollowUp) Summary() string {
	if f == nil {
		return ""
	}
	who := strings.TrimSpace(f.Reviewer)
	if who != "" {
		who = " by @" + who
	}
	state := "unaddressed"
	if f.Addressed {
		state = "addressed"
		if f.AddressedBy != "" {
			state += " by " + f.AddressedBy
		}
	}
	return fmt.Sprintf("CHANGES REQUESTED%s (%s)", who, state)
}

const prReviewFollowUpQuery = `query($owner: String!, $repo: String!, $number: Int!, $commits: Int!, $comments: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      latestOpinionatedReviews(first: 50) { nodes { state submittedAt authorAssociation author { __typename login } } }
      commits(last: $commits) { nodes { commit { committedDate parents(first: 1) { totalCount } } } }
      comments(last: $comments) { nodes { createdAt body author { __typename login } } }
      reviews(last: $comments) { nodes { createdAt body author { __typename login } } }
    }
  }
}`

type rawPRReviewFollowUp struct {
	LatestOpinionatedReviews struct {
		Nodes []struct {
			State             string              `json:"state"`
			SubmittedAt       time.Time           `json:"submittedAt"`
			AuthorAssociation string              `json:"authorAssociation"`
			Author            *rawPRCommentAuthor `json:"author"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CommittedDate time.Time `json:"committedDate"`
				Parents       struct {
					TotalCount int `json:"totalCount"`
				} `json:"parents"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Comments struct {
		Nodes []rawPRComment `json:"nodes"`
	} `json:"comments"`
	Reviews struct {
		Nodes []rawPRComment `json:"nodes"`
	} `json:"reviews"`
}

// FetchPRReviewFollowUp returns the latest unanswered human CHANGES_REQUESTED
// review on repo#number, or ok=false when the PR's latest opinionated human
// review is something else (or there is none).
//
// One GraphQL request per PR. Callers must therefore keep the candidate set
// small — EnrichPRReviewFollowUp does, by asking only about held PRs GitHub
// has already reported a CHANGES_REQUESTED decision for.
func (c *Client) FetchPRReviewFollowUp(ctx context.Context, repo string, number int) (PRReviewFollowUp, bool, error) {
	if c == nil {
		return PRReviewFollowUp{}, false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	var data struct {
		Repository struct {
			PullRequest *rawPRReviewFollowUp `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{
		"owner":    owner,
		"repo":     name,
		"number":   number,
		"commits":  prReviewFollowUpCommitWindow,
		"comments": prReviewFollowUpCommentWindow,
	}
	if err := c.graphQL(ctx, prReviewFollowUpQuery, vars, &data); err != nil {
		return PRReviewFollowUp{}, false, err
	}
	if data.Repository.PullRequest == nil {
		return PRReviewFollowUp{}, false, fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
	}
	return c.reviewFollowUpFrom(*data.Repository.PullRequest), true, nil
}

// reviewFollowUpFrom applies the addressed rule to one PR's raw conversation.
// The result has a zero SubmittedAt when no human review currently requests
// changes.
func (c *Client) reviewFollowUpFrom(raw rawPRReviewFollowUp) PRReviewFollowUp {
	var latest PRReviewFollowUp
	for _, r := range raw.LatestOpinionatedReviews.Nodes {
		if !strings.EqualFold(r.State, string(ReviewDecisionChangesRequested)) {
			continue
		}
		// Only a person with a standing relationship to the repository can
		// send an agent back to work, exactly as for routed comments.
		if !trustedCommentAssociations[strings.ToUpper(r.AuthorAssociation)] || !c.isHumanFeedbackAuthor(r.Author) {
			continue
		}
		if !latest.SubmittedAt.IsZero() && !r.SubmittedAt.After(latest.SubmittedAt) {
			continue
		}
		login := ""
		if r.Author != nil {
			login = strings.TrimSpace(r.Author.Login)
		}
		latest = PRReviewFollowUp{Reviewer: login, SubmittedAt: r.SubmittedAt}
	}
	if latest.SubmittedAt.IsZero() {
		return PRReviewFollowUp{}
	}
	commits := make([]PRCommitSummary, 0, len(raw.Commits.Nodes))
	for _, n := range raw.Commits.Nodes {
		commits = append(commits, PRCommitSummary{
			CommittedAt: n.Commit.CommittedDate,
			Parents:     n.Commit.Parents.TotalCount,
		})
	}
	var replies []time.Time
	for _, set := range [][]rawPRComment{raw.Comments.Nodes, raw.Reviews.Nodes} {
		for _, rc := range set {
			if c.isHiveReply(rc) {
				replies = append(replies, rc.CreatedAt)
			}
		}
	}
	latest.Addressed, latest.AddressedBy = ReviewAddressed(latest.SubmittedAt, commits, replies)
	return latest
}

// isHiveReply reports whether a comment is one THIS hive posted: its own
// login, or any body carrying the hive attribution trailer (an agent running
// on a person's credentials signs what it posts). Another bot answering the
// reviewer is not an answer from the owning agent and never counts.
func (c *Client) isHiveReply(rc rawPRComment) bool {
	if strings.TrimSpace(rc.Body) == "" {
		return false
	}
	if HasAttributionTrailer(rc.Body) {
		return true
	}
	return rc.Author != nil && c.isHiveLogin(strings.TrimSpace(rc.Author.Login))
}

// PRReviewFollowUpCandidate reports whether a PR is worth one
// FetchPRReviewFollowUp call: a non-draft, non-fork PR the hive opened that
// GitHub already reports as CHANGES_REQUESTED.
func PRReviewFollowUpCandidate(pr PullRequest) bool {
	if pr.Draft || pr.FromFork || pr.Number <= 0 || pr.Repo == "" {
		return false
	}
	if !pr.AppAuthored && !pr.HiveAttributed && strings.TrimSpace(pr.HiveAgent) == "" {
		return false // a human's PR is never routed back to an agent
	}
	return pr.Protection != nil && pr.Protection.ReviewDecision == ReviewDecisionChangesRequested
}

// EnrichPRReviewFollowUp stamps ReviewFollowUp onto every PR in prs that
// PRReviewFollowUpCandidate accepts, at most limit of them per pass. It must
// run after the review decision is attached (EnrichCIStatus /
// EnrichReviewSignals), since that decision is what selects the candidates.
//
// A PR whose fetch fails is left unstamped: no annotation and no routing is
// the safe direction, and the next tick retries.
func (c *Client) EnrichPRReviewFollowUp(ctx context.Context, prs []PullRequest, limit int) {
	if c == nil || limit <= 0 {
		return
	}
	fetched := 0
	for i := range prs {
		pr := &prs[i]
		pr.ReviewFollowUp = nil
		if !PRReviewFollowUpCandidate(*pr) {
			continue
		}
		if fetched >= limit {
			continue
		}
		fetched++
		state, ok, err := c.FetchPRReviewFollowUp(ctx, pr.Repo, pr.Number)
		if err != nil || !ok || state.SubmittedAt.IsZero() {
			continue
		}
		followUp := state
		pr.ReviewFollowUp = &followUp
	}
}
