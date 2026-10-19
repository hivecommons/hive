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
		if !strings.EqualFold(rv.GetState(), "CHANGES_REQUESTED") || strings.TrimSpace(rv.GetCommitID()) != strings.TrimSpace(head) {
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
func (c *Engine) humanChangesRequestedForHead(ctx context.Context, owner, repo string, number int, head string) bool {
	rv, ok := c.latestTrustedHumanChangesRequested(ctx, owner, repo, number, head)
	if !ok {
		if c.humanChangeRequests == nil || strings.TrimSpace(head) == "" {
			return false
		}
		_, standing := c.humanChangeRequests.For(owner+"/"+repo, number, strings.TrimSpace(head))
		return standing
	}
	if c.humanChangeRequests == nil {
		return true
	}
	req := review.HumanChangeRequest{
		Repo:     owner + "/" + repo,
		Number:   number,
		HeadSHA:  strings.TrimSpace(head),
		Source:   review.HumanChangeRequestSourceReview,
		SourceID: rv.GetID(),
		Actor:    strings.TrimSpace(hgithub.SafeGetLogin(rv.GetUser())),
		Body:     rv.GetBody(),
		URL:      rv.GetHTMLURL(),
	}
	if existing, found := c.humanChangeRequests.For(req.Repo, number, req.HeadSHA); found && existing.Source == req.Source && existing.SourceID == req.SourceID {
		return true
	}
	if err := c.humanChangeRequests.Record(req); err != nil {
		c.warn("automerge self lane could not record human changes_requested", "owner", owner, "repo", repo, "pr", number, "head", head, "error", err)
	}
	return true
}
