package automerge

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Skip reasons reported by the review-verdict gate (#12374).
const (
	// autoMergeReasonReviewRequiresHuman: the review swarm's latest decisive
	// verdict on the PR asks for a person, and no person has released it.
	autoMergeReasonReviewRequiresHuman = "review-verdict-requires-human"
	// autoMergeReasonReviewRejected: the latest decisive verdict rejects the
	// PR, and no person has released it.
	autoMergeReasonReviewRejected = "review-verdict-reject"
	// autoMergeReasonReviewHoldEvidence: a verdict holds the PR but its
	// reviews or comments could not be listed to look for a person's
	// release. Fails CLOSED.
	autoMergeReasonReviewHoldEvidence = "review-verdict-hold-evidence"
)

// ReviewVerdictHold is the review swarm's hold on one pull request; see
// hgithub.ReviewVerdictHold. The merge-request relay reads the same hold.
type ReviewVerdictHold = hgithub.ReviewVerdictHold

// ReviewVerdictHoldFunc resolves the review verdict hold for repo#number.
// repo is owner/repo. It is consulted regardless of review.require_approval.
type ReviewVerdictHoldFunc = hgithub.ReviewVerdictHoldFunc

// ReviewHoldReleaseFunc records the person's release the gate found for
// repo#number, so the review pipeline and the dashboard's your-turn decision
// see the hold as released too (#12962). repo is owner/repo.
type ReviewHoldReleaseFunc func(repo string, number int, release hgithub.ReviewHoldRelease)

// reviewVerdictGate refuses an App or trusted-author self-merge while the
// review swarm's verdict holds the PR for a person (#12374). The hold is not
// keyed to the current head: a requires_human verdict followed by a CI fix
// push still holds the new head until a fresh merge-eligible approve is
// recorded (the hook stops reporting it) or a person releases it on GitHub
// with an approving review or a `/hive approve` reply posted after the
// verdict.
//
// It returns ("", nil) when the merge may proceed. No hook or no hold costs
// no API call.
func (c *Engine) reviewVerdictGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) (string, error) {
	if c == nil || c.reviewVerdictHold == nil {
		return "", nil
	}
	number := pr.GetNumber()
	hold := c.reviewVerdictHold(owner+"/"+repo, number)
	if !hold.Held {
		return "", nil
	}
	reason := autoMergeReasonReviewRequiresHuman
	if strings.EqualFold(hold.Verdict, "reject") {
		reason = autoMergeReasonReviewRejected
	}
	release, released, err := c.personReleasedReviewHold(ctx, owner, repo, number, hold.RecordedAt)
	if err != nil {
		return autoMergeReasonReviewHoldEvidence, err
	}
	if released {
		c.info("automerge sweep: review verdict hold released by a person", "repo", displayRepo, "pr", number, "verdict", hold.Verdict, "verdict_head", hold.HeadSHA, "by", release.By)
		if c.recordReviewHoldRelease != nil {
			c.recordReviewHoldRelease(owner+"/"+repo, number, release)
		}
		return "", nil
	}
	c.info("automerge sweep held PR for a person: review verdict", "repo", displayRepo, "pr", number, "verdict", hold.Verdict, "verdict_head", hold.HeadSHA, "current_head", pr.GetHead().GetSHA())
	return reason, nil
}

// contributorLaneReviewHoldRelease lets a trusted merger release a
// contributor-lane PR the review swarm holds for a person, the same way the
// self-authored sweep's reviewVerdictGate does: an approving review or a
// `/hive approve` reply posted after the holding verdict (#13085). Unlike the
// self-authored lane, the release must come from a trusted merger, because
// the PR author here is a contributor who could otherwise release their own
// PR. A release is recorded in the release ledger so the dashboard's
// your-turn decision drops the PR, and noted on the stage tracker so the
// stage label moves on. It returns the releaser's login.
func (c *Engine) contributorLaneReviewHoldRelease(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string, verdict ContributorLaneVerdict) (string, bool) {
	if c == nil || c.reviewVerdictHold == nil || !verdictNeedsHuman(stageDetail{Verdict: verdict, VerdictKnown: true}) {
		return "", false
	}
	hold := c.reviewVerdictHold(owner+"/"+repo, number)
	if !hold.Held || hold.RecordedAt.IsZero() {
		return "", false
	}
	trusted := func(login string) bool {
		ok, configured := c.isTrustedMerger(login)
		return configured && ok
	}
	release, released, err := c.findReviewHoldRelease(ctx, owner, repo, number, hold.RecordedAt, trusted)
	if err != nil {
		c.warn("contributor-lane could not read the PR's reviews and comments for a person's release", "repo", displayRepo, "pr", number, "error", err)
		return "", false
	}
	if !released {
		return "", false
	}
	c.info("contributor-lane review verdict hold released by a trusted merger", "repo", displayRepo, "pr", number, "verdict", hold.Verdict, "verdict_head", hold.HeadSHA, "by", release.By, "via", release.Via)
	if c.recordReviewHoldRelease != nil {
		c.recordReviewHoldRelease(owner+"/"+repo, number, release)
	}
	c.noteStageTrustedHumanApproval(owner, repo, number, headSHA, release.By)
	return release.By, true
}

// personReleasedReviewHold returns the first review or comment on the PR
// that releases a hold whose verdict was recorded at since, by
// hgithub.ReleasesReviewHold: a person's approving review or `/hive approve`
// reply posted after the verdict.
func (c *Engine) personReleasedReviewHold(ctx context.Context, owner, repo string, number int, since time.Time) (hgithub.ReviewHoldRelease, bool, error) {
	return c.findReviewHoldRelease(ctx, owner, repo, number, since, nil)
}

func (c *Engine) findReviewHoldRelease(ctx context.Context, owner, repo string, number int, since time.Time, releaser func(login string) bool) (hgithub.ReviewHoldRelease, bool, error) {
	ctx = hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	return hgithub.FindReviewHoldRelease(ctx, c.gh, owner, repo, number, since, c.isPersonLogin, releaser)
}

// isPersonLogin reports whether u is a person, not the App or another bot.
func (c *Engine) isPersonLogin(u *gh.User) bool {
	appLogin := ""
	if c.transport != nil {
		appLogin = c.transport.AppBotLogin()
	}
	return hgithub.IsPersonLogin(u, appLogin)
}
