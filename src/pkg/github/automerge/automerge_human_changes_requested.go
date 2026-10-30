package automerge

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// latestTrustedHumanChangesRequested finds a trusted maintainer's standing
// CHANGES_REQUESTED review on head (hivecommons/hive#13041). The trust test
// is latestTrustedHumanApproval's: a human login (not the App, not a bot)
// that isTrustedMerger says is configured and trusted. The standing test is
// hasOutstandingChangesRequested's: only a login's latest opinionated review
// (APPROVED / CHANGES_REQUESTED / DISMISSED) counts, so a dismissal or a
// later approval clears it. On top of both, the review must be on head —
// a changes_requested left on an older commit is outdated: the author
// already pushed past it, and the new head goes through the normal swarm
// re-review rather than another kick.
func (c *Engine) latestTrustedHumanChangesRequested(ctx context.Context, owner, repo string, number int, head string) (*gh.PullRequestReview, bool) {
	return c.latestTrustedHumanReview(ctx, owner, repo, number, head, "CHANGES_REQUESTED")
}

// latestTrustedHumanReview reads all pages before selecting an opinion. An
// older approval must never shadow a later change request or dismissal.
func (c *Engine) latestTrustedHumanReview(ctx context.Context, owner, repo string, number int, head, state string) (*gh.PullRequestReview, bool) {
	if c == nil || c.gh == nil || strings.TrimSpace(head) == "" {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts := &gh.ListOptions{PerPage: 100}
	latest := map[string]*gh.PullRequestReview{}
	var seen []*gh.PullRequestReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			c.warn("automerge self lane could not read PR reviews for human changes_requested", "owner", owner, "repo", repo, "pr", number, "error", err)
			return nil, false
		}
		seen = append(seen, reviews...)
		for _, rv := range reviews {
			login := strings.ToLower(strings.TrimSpace(hgithub.SafeGetLogin(rv.GetUser())))
			if login == "" {
				continue
			}
			switch strings.ToUpper(strings.TrimSpace(rv.GetState())) {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
				latest[login] = rv
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.noteStageReviews(owner, repo, number, seen)
	var found *gh.PullRequestReview
	for login, rv := range latest {
		if !strings.EqualFold(rv.GetState(), state) || strings.TrimSpace(rv.GetCommitID()) != strings.TrimSpace(head) {
			continue
		}
		if c.isHiveAppReviewAuthor(rv) || strings.HasSuffix(login, "[bot]") {
			continue
		}
		trusted, configured := c.isTrustedMerger(login)
		if !configured || !trusted {
			continue
		}
		if found == nil || rv.GetSubmittedAt().After(found.GetSubmittedAt().Time) {
			found = rv
		}
	}
	return found, found != nil
}

// humanChangesRequestedForHead reports whether a trusted maintainer has
// asked for changes on head, recording the request for the review planner
// the first time it is seen. The store dedupes on (head, review id), so a
// head is kicked once however many sweeps observe the same review. A
// request already standing in the store for this head — a `/hive decision`
// the un-park sweep recorded (pr_unpark_decision.go) — counts the same way,
// so the text path reaches 070 through the lane reason like a review does.
// A bare `/hive decision approve` recorded there is not a change request
// (hivecommons/hive#14679). Either way the requester is noted on the stage
// detail so the reviewer's decision-requested verdict on this head stops
// deriving 085.
func (c *Engine) humanChangesRequestedForHead(ctx context.Context, owner, repo string, number int, head string) bool {
	rv, ok := c.latestTrustedHumanChangesRequested(ctx, owner, repo, number, head)
	if !ok {
		if c.humanChangeRequests == nil || strings.TrimSpace(head) == "" {
			return false
		}
		standing, found := c.reconcileReviewBackedApproval(ctx, owner, repo, number, strings.TrimSpace(head))
		if !found || standing.IsApproval() {
			return false
		}
		// A later trusted approval on this same head can acknowledge a body-
		// only fix. Persist it so the planner also stops scheduling a fix.
		if approval, approved := c.latestTrustedHumanReview(ctx, owner, repo, number, head, "APPROVED"); approved && approval.GetSubmittedAt().After(standing.RecordedAt) {
			req := review.HumanChangeRequest{
				Repo: owner + "/" + repo, Number: number, HeadSHA: head,
				Source: review.HumanChangeRequestSourceReview, SourceID: approval.GetID(),
				Actor: hgithub.SafeGetLogin(approval.GetUser()), Body: approval.GetBody(), URL: approval.GetHTMLURL(),
				Kind: review.HumanChangeRequestKindApprove, RecordedAt: approval.GetSubmittedAt().Time,
			}
			if err := c.humanChangeRequests.Record(req); err == nil {
				c.noteStageTrustedHumanApproval(owner, repo, number, head, req.Actor)
				return false
			} else {
				c.warn("could not record approval clearing human change request", "pr", number, "error", err)
			}
		}
		c.noteStageHumanChangeRequest(owner, repo, number, head, standing.Actor)
		return true
	}
	c.noteStageHumanChangeRequest(owner, repo, number, head, hgithub.SafeGetLogin(rv.GetUser()))
	if c.humanChangeRequests == nil {
		return true
	}
	req := review.HumanChangeRequest{
		Repo:       owner + "/" + repo,
		Number:     number,
		HeadSHA:    strings.TrimSpace(head),
		Source:     review.HumanChangeRequestSourceReview,
		SourceID:   rv.GetID(),
		Actor:      strings.TrimSpace(hgithub.SafeGetLogin(rv.GetUser())),
		Body:       rv.GetBody(),
		URL:        rv.GetHTMLURL(),
		RecordedAt: rv.GetSubmittedAt().Time,
	}
	if existing, found := c.humanChangeRequests.For(req.Repo, number, req.HeadSHA); found && existing.Source == req.Source && existing.SourceID == req.SourceID {
		return true
	}
	if err := c.humanChangeRequests.Record(req); err != nil {
		c.warn("automerge self lane could not record human changes_requested", "owner", owner, "repo", repo, "pr", number, "head", head, "error", err)
	}
	return true
}

// reconcileReviewBackedApproval returns the decision standing against head
// after dropping an approval-kind record that was persisted from a GitHub
// review which no longer stands: the reviewer dismissed it, or their latest
// opinion on head is not APPROVED any more. The revoked approval would
// otherwise keep answering requires_human and shadow the instruction
// recorded before it. Comment and dashboard decisions are durable and are
// never revalidated here.
func (c *Engine) reconcileReviewBackedApproval(ctx context.Context, owner, repo string, number int, head string) (review.HumanChangeRequest, bool) {
	if c == nil || c.humanChangeRequests == nil || strings.TrimSpace(head) == "" {
		return review.HumanChangeRequest{}, false
	}
	key := owner + "/" + repo
	standing, found := c.humanChangeRequests.For(key, number, head)
	if !found || !standing.IsApproval() || standing.Source != review.HumanChangeRequestSourceReview {
		return standing, found
	}
	if live, ok := c.latestTrustedHumanReview(ctx, owner, repo, number, head, "APPROVED"); ok && live.GetID() == standing.SourceID {
		return standing, true
	}
	if err := c.humanChangeRequests.Forget(key, number, standing.Source, standing.SourceID); err != nil {
		c.warn("could not drop revoked review approval", "pr", number, "review", standing.SourceID, "error", err)
		return review.HumanChangeRequest{}, false
	}
	return c.humanChangeRequests.For(key, number, head)
}

// sentinelBlockedUnlessHumanChangesRequested refines the contributor lane's
// sentinel-blocked prefilter skip: a sentinel alert parks the PR on a person
// (085), and a trusted maintainer's CHANGES_REQUESTED review on the current
// head IS that person's answer — the author acts next, so the lane reason
// becomes human-changes-requested and the stage pass derives the fixing
// stage (072 for a clanker's PR) instead of re-asserting 085 over it every
// sweep (hivecommons/hive#15159). The review is read only for a sentinel-
// blocked PR, and the standing request is noted on the stage detail and
// recorded for the nudge like the self lane's. Any other reason is returned
// unchanged; a new head without a review on it is blocked again.
func (c *Engine) sentinelBlockedUnlessHumanChangesRequested(ctx context.Context, owner, repo string, pr *gh.PullRequest, reason string) string {
	if reason != contributorLaneReasonSentinelBlocked || pr == nil || pr.GetHead() == nil {
		return reason
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" {
		return reason
	}
	if c.humanChangesRequestedForHead(ctx, owner, repo, pr.GetNumber(), head) {
		return autoMergeReasonHumanChangesRequested
	}
	return reason
}
