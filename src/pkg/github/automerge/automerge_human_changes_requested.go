package automerge

import (
	"context"
	"strings"
	"time"

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

// latestTrustedHumanReview is the newest standing trusted review in state
// that binds to head (standingTrustedHumanReviews). A change request binds
// to its commit; an approval binds to the content it approved, which a base
// merge carries and a content push does not, whatever commit_id GitHub
// reports (automerge_review_head_binding.go).
func (c *Engine) latestTrustedHumanReview(ctx context.Context, owner, repo string, number int, head, state string) (*gh.PullRequestReview, bool) {
	if c == nil || c.gh == nil || strings.TrimSpace(head) == "" {
		return nil, false
	}
	reviews := c.standingTrustedHumanReviews(ctx, owner, repo, number)
	if !strings.EqualFold(state, "APPROVED") {
		found := newestReviewOnHead(reviews, state, head)
		return found, found != nil
	}
	var found *gh.PullRequestReview
	for _, rv := range reviews {
		if !strings.EqualFold(rv.GetState(), state) || !c.reviewReachesHead(ctx, owner, repo, number, head, rv).ok {
			continue
		}
		if found == nil || rv.GetSubmittedAt().After(found.GetSubmittedAt().Time) {
			found = rv
		}
	}
	return found, found != nil
}

// standingTrustedHumanReviews returns each trusted human's standing review:
// the login's latest opinionated review (APPROVED / CHANGES_REQUESTED /
// DISMISSED), kept only when the login is a person (not the App, not a bot)
// isTrustedMerger says is configured and trusted. It reads all pages before
// selecting, so an older approval never shadows a later change request or
// dismissal. A read failure returns nothing.
func (c *Engine) standingTrustedHumanReviews(ctx context.Context, owner, repo string, number int) []*gh.PullRequestReview {
	out, _ := c.standingTrustedHumanReviewsRead(ctx, owner, repo, number)
	return out
}

// standingTrustedHumanReviewsRead is standingTrustedHumanReviews that also
// reports whether the review list was read: false means nothing is known.
func (c *Engine) standingTrustedHumanReviewsRead(ctx context.Context, owner, repo string, number int) ([]*gh.PullRequestReview, bool) {
	if c == nil || c.gh == nil {
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
	var out []*gh.PullRequestReview
	for login, rv := range latest {
		if c.isHiveAppReviewAuthor(rv) || strings.HasSuffix(login, "[bot]") {
			continue
		}
		trusted, configured := c.isTrustedMerger(login)
		if !configured || !trusted {
			continue
		}
		out = append(out, rv)
	}
	return out, true
}

// newestReviewOnHead picks the most recently submitted review in state whose
// commit is head; nil when none is.
func newestReviewOnHead(reviews []*gh.PullRequestReview, state, head string) *gh.PullRequestReview {
	var found *gh.PullRequestReview
	for _, rv := range reviews {
		if !strings.EqualFold(rv.GetState(), state) || strings.TrimSpace(rv.GetCommitID()) != strings.TrimSpace(head) {
			continue
		}
		if found == nil || rv.GetSubmittedAt().After(found.GetSubmittedAt().Time) {
			found = rv
		}
	}
	return found
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
//
// A change request survives the hive's own base merge: when head is nothing
// but the App's merge of the base branch into the head a trusted maintainer
// asked for changes on (carriedTrustedHumanChangesRequested), the review
// still stands for head, so the fixer is kicked for it. Any other push ends
// it, as before.
func (c *Engine) humanChangesRequestedForHead(ctx context.Context, owner, repo string, number int, head string) bool {
	changed, _ := c.humanChangesRequestedForHeadRead(ctx, owner, repo, number, head)
	return changed
}

// humanChangesRequestedForHeadRead is humanChangesRequestedForHead that also
// reports whether a false answer is settled: every read behind it (the
// review list, the PR, the base-refresh proof) succeeded. A caller memoises
// "no change request" only then.
func (c *Engine) humanChangesRequestedForHeadRead(ctx context.Context, owner, repo string, number int, head string) (bool, bool) {
	head = strings.TrimSpace(head)
	var reviews []*gh.PullRequestReview
	readOK := false
	if c != nil && c.gh != nil && head != "" {
		reviews, readOK = c.standingTrustedHumanReviewsRead(ctx, owner, repo, number)
	}
	rv := newestReviewOnHead(reviews, "CHANGES_REQUESTED", head)
	carriedFrom := ""
	if rv == nil {
		if c.humanChangeRequests == nil || head == "" {
			return false, readOK
		}
		standing, found := c.reconcileReviewBackedApproval(ctx, owner, repo, number, head)
		if found && standing.IsApproval() {
			return false, readOK
		}
		if found {
			// A later trusted approval on this same head can acknowledge a body-
			// only fix. Persist it so the planner also stops scheduling a fix.
			if approval, approved := c.laterTrustedApproval(ctx, owner, repo, number, head, standing.RecordedAt); approved {
				if _, recorded := c.recordTrustedApproval(owner, repo, number, head, approval, false); recorded {
					return false, readOK
				}
			}
			c.noteStageHumanChangeRequest(owner, repo, number, head, standing.Actor)
			return true, true
		}
		if !readOK {
			return false, false
		}
		carried, from, ok, definite := c.carriedTrustedHumanChangesRequested(ctx, owner, repo, number, head, reviews)
		if !ok {
			return false, definite
		}
		rv, carriedFrom = carried, from
	}
	c.noteStageHumanChangeRequest(owner, repo, number, head, hgithub.SafeGetLogin(rv.GetUser()))
	if c.humanChangeRequests == nil {
		return true, true
	}
	req := review.HumanChangeRequest{
		Repo:            owner + "/" + repo,
		Number:          number,
		HeadSHA:         head,
		Source:          review.HumanChangeRequestSourceReview,
		SourceID:        rv.GetID(),
		Actor:           strings.TrimSpace(hgithub.SafeGetLogin(rv.GetUser())),
		Body:            rv.GetBody(),
		URL:             rv.GetHTMLURL(),
		RecordedAt:      rv.GetSubmittedAt().Time,
		CarriedFromHead: carriedFrom,
	}
	if existing, found := c.humanChangeRequests.For(req.Repo, number, req.HeadSHA); found && existing.Source == req.Source && existing.SourceID == req.SourceID {
		return true, true
	}
	if err := c.humanChangeRequests.Record(req); err != nil {
		c.warn("automerge self lane could not record human changes_requested", "owner", owner, "repo", repo, "pr", number, "head", head, "error", err)
		return true, true
	}
	if carriedFrom != "" {
		c.info(humanChangesRequestedCarriedMessage, "repo", owner+"/"+repo, "pr", number, "from_head", carriedFrom, "head", head, "by", req.Actor, "review", req.SourceID)
	}
	return true, true
}

// humanChangesRequestedCarriedMessage is the one INFO line logged when a
// person's change request follows the PR over the hive's own base merge.
const humanChangesRequestedCarriedMessage = "human change request carried over the hive's base merge"

// carriedTrustedHumanChangesRequested returns a trusted maintainer's standing
// CHANGES_REQUESTED review (among reviews, standingTrustedHumanReviews) on an
// earlier head of which head is nothing but the hive's own base refresh, and
// that earlier head. "Base refresh" is hgithub.BaseRefreshChecker's proof:
// every commit from the reviewed head to head is a GitHub-verified merge
// commit the App made, whose parents are the previous head and a commit on
// the base branch, and the PR's diff against the base is unchanged (at most
// a few such hops). A content push, a rebase, anyone else's merge or a
// changed diff answers false, and the change request ends there as before.
// The review must still be the reviewer's standing opinion: a later approval
// or a dismissal already removed it from reviews. The newest qualifying
// review wins. Read failures answer false.
//
// The request follows the head across every App base-merge hop (the proof
// walks back to the reviewed head), so the stage keeps reading it as the
// author's turn. The planner does not kick it again while the fix already
// kicked for it has no recorded attempt (review.carriedRequestFixPending). The refresh proof is memoised
// for definite answers (BaseRefreshChecker.Known), so a known refresh costs
// no PR read. definite is false when a read failed (the PR, a commit or a
// compare), so a false answer is not settled.
func (c *Engine) carriedTrustedHumanChangesRequested(ctx context.Context, owner, repo string, number int, head string, reviews []*gh.PullRequestReview) (*gh.PullRequestReview, string, bool, bool) {
	if c == nil || c.gh == nil || c.transport == nil || head == "" {
		return nil, "", false, true
	}
	app := strings.TrimSpace(c.transport.AppBotLogin())
	if app == "" {
		return nil, "", false, true
	}
	newer := func(found, rv *gh.PullRequestReview) bool {
		return found == nil || rv.GetSubmittedAt().After(found.GetSubmittedAt().Time)
	}
	var found *gh.PullRequestReview
	var unknown []*gh.PullRequestReview
	for _, rv := range reviews {
		from := strings.TrimSpace(rv.GetCommitID())
		if !strings.EqualFold(rv.GetState(), "CHANGES_REQUESTED") || from == "" || from == head {
			continue
		}
		refresh, seen := c.baseRefresh.Known(owner, repo, number, from, head)
		switch {
		case seen && refresh:
			if newer(found, rv) {
				found = rv
			}
		case !seen:
			unknown = append(unknown, rv)
		}
	}
	definite := true
	if found == nil && len(unknown) > 0 {
		pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:decision_base_refresh"), owner, repo, number)
		if err != nil {
			return nil, "", false, false
		}
		if strings.TrimSpace(pr.GetHead().GetSHA()) != head {
			// The head moved since the caller read it: nothing settled.
			return nil, "", false, false
		}
		baseRef := pr.GetBase().GetRef()
		for _, rv := range unknown {
			refresh, sure := c.baseRefresh.CheckBaseRefresh(ctx, c.gh.Repositories, owner, repo, number, baseRef, rv.GetCommitID(), head, app)
			if !sure {
				definite = false
			}
			if refresh && newer(found, rv) {
				found = rv
			}
		}
	}
	if found == nil {
		return nil, "", false, definite
	}
	return found, strings.TrimSpace(found.GetCommitID()), true, true
}

// humanMergeRequiredUnlessHumanChangesRequested refines a lane's
// human-merge-rule skip (a breaking-change title, or a
// auto_merge.human_merge_labels label such as the needs-owner-merge the
// human-merge-path rule applies) the way
// sentinelBlockedUnlessHumanChangesRequested refines a sentinel block: the
// lane stops before it reads the PR's reviews, so a trusted maintainer's
// CHANGES_REQUESTED review on the current head (or carried over the hive's
// own base merge) was never recorded and the fixer never kicked. With one,
// the lane reason is human-changes-requested and the stage is the author's
// fixing stage; only a person merges the PR either way. Any other reason is
// returned unchanged.
//
// It reads nothing for a PR the cheaper checks already stop: a hold, a pause,
// do-not-merge, needs-human, blocked or an exempt label is a person's (or a
// gate's) stop, and a change request must not route the fixer past it. An
// observe-only pass never refines (it would record a request the planner
// acts on in a repository the hive does not merge). And a head whose PR has
// not changed since a read found no change request is not read again
// (StageTracker.humanMergeNoChangeRequest, keyed on the head and the PR's
// updated_at, which a new review or a dismissal moves).
func (c *Engine) humanMergeRequiredUnlessHumanChangesRequested(ctx context.Context, owner, repo string, pr *gh.PullRequest, reason string) string {
	if reason != autoMergeReasonHumanMergeRequired || pr == nil || pr.GetHead() == nil || c == nil || c.observeOnly {
		return reason
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" {
		return reason
	}
	labels := labelNames(pr.Labels)
	if c.labelBlockReason(labels, true) != "" || (c.transport != nil && c.transport.IsExemptLabels(labels)) {
		return reason
	}
	tracker := c.stageTrackerOrShared()
	key := stageKey(owner, repo, pr.GetNumber())
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	seen := ""
	if pr.UpdatedAt != nil {
		seen = head + "@" + pr.GetUpdatedAt().UTC().Format(time.RFC3339Nano)
		if tracker.humanMergeNoChangeRequestKnown(key, seen, now) {
			return reason
		}
	}
	changed, settled := c.humanChangesRequestedForHeadRead(ctx, owner, repo, pr.GetNumber(), head)
	if changed {
		tracker.forgetHumanMergeNoChangeRequest(key)
		return autoMergeReasonHumanChangesRequested
	}
	// Only a settled "none" is remembered: a failed read must not hide a
	// review until updated_at moves.
	if seen != "" && settled {
		tracker.noteHumanMergeNoChangeRequest(key, seen, now)
	} else {
		tracker.forgetHumanMergeNoChangeRequest(key)
	}
	return reason
}

// humanMergeNoChangeRequestCap bounds StageTracker.humanMergeNoChangeRequest:
// past it the memory starts over (each PR is read once more).
const humanMergeNoChangeRequestCap = 2048

// humanMergeNoChangeRequestTTL bounds how long a settled "no change
// request" stands for an unchanged PR: a trusted-merger list change, or a
// review that did not move updated_at, is read within it.
const humanMergeNoChangeRequestTTL = 30 * time.Minute

// humanMergeNoChangeRequestSeen is one remembered settled "none".
type humanMergeNoChangeRequestSeen struct {
	seen string
	at   time.Time
}

func (t *StageTracker) humanMergeNoChangeRequestKnown(key, seen string, now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	got, ok := t.humanMergeNoChangeRequest[key]
	return ok && got.seen == seen && now.Sub(got.at) < humanMergeNoChangeRequestTTL
}

func (t *StageTracker) noteHumanMergeNoChangeRequest(key, seen string, now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.humanMergeNoChangeRequest == nil || len(t.humanMergeNoChangeRequest) >= humanMergeNoChangeRequestCap {
		t.humanMergeNoChangeRequest = map[string]humanMergeNoChangeRequestSeen{}
	}
	t.humanMergeNoChangeRequest[key] = humanMergeNoChangeRequestSeen{seen: seen, at: now}
}

func (t *StageTracker) forgetHumanMergeNoChangeRequest(key string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.humanMergeNoChangeRequest, key)
}

// reconcileReviewBackedApproval returns the decision standing against head
// after dropping an approval-kind record that was persisted from a GitHub
// review which no longer stands: the reviewer dismissed it, or their latest
// opinion on head is not APPROVED any more. The revoked approval would
// otherwise keep answering requires_human and shadow the instruction
// recorded before it. Comment and dashboard decisions are durable and are
// never revalidated here.
//
// The record is dropped only on a definite answer: the review list was
// read and either the record's own review no longer stands as an approval
// in it, or every approval in it was placed against head. A failed review
// read, or an approval whose reach to head could not be read
// (reviewReachesHeadKnown), keeps the record until a read settles it, so a
// GitHub outage or rate-limit window never revokes a person's approval
// (it used to, on every lane that reconciles).
func (c *Engine) reconcileReviewBackedApproval(ctx context.Context, owner, repo string, number int, head string) (review.HumanChangeRequest, bool) {
	if c == nil || c.humanChangeRequests == nil || strings.TrimSpace(head) == "" {
		return review.HumanChangeRequest{}, false
	}
	key := owner + "/" + repo
	standing, found := c.humanChangeRequests.For(key, number, head)
	if !found || !standing.IsApproval() || standing.Source != review.HumanChangeRequestSourceReview {
		return standing, found
	}
	// latestTrustedHumanReview's APPROVED pick, keeping track of whether
	// every read behind it succeeded.
	reviews, definite := c.standingTrustedHumanReviewsRead(ctx, owner, repo, number)
	// own: the record's review still stands as an approval in a list that
	// was read. When the list was read and it does not, the record is
	// definitely revoked (dismissed, or the reviewer's latest opinion
	// changed): no other approval's unread reach can make it live again.
	own := false
	for _, rv := range reviews {
		if rv.GetID() == standing.SourceID && strings.EqualFold(rv.GetState(), "APPROVED") {
			own = true
		}
	}
	var live *gh.PullRequestReview
	for _, rv := range reviews {
		if !own || !strings.EqualFold(rv.GetState(), "APPROVED") {
			continue
		}
		reach, known := c.reviewReachesHeadKnown(ctx, owner, repo, number, head, rv)
		if !known {
			definite = false
			continue
		}
		if reach.ok && (live == nil || rv.GetSubmittedAt().After(live.GetSubmittedAt().Time)) {
			live = rv
		}
	}
	if live != nil && live.GetID() == standing.SourceID {
		return standing, true
	}
	if !definite {
		c.debug("review-backed approval kept: its review could not be read", "repo", key, "pr", number, "head", head, "review", standing.SourceID)
		return standing, true
	}
	if err := c.humanChangeRequests.Forget(key, number, standing.Source, standing.SourceID); err != nil {
		c.warn("could not drop revoked review approval", "pr", number, "review", standing.SourceID, "error", err)
		return review.HumanChangeRequest{}, false
	}
	return c.humanChangeRequests.For(key, number, head)
}

// laterTrustedApproval is the newest standing trusted APPROVED review that
// reaches head (latestTrustedHumanReview), when it was submitted after
// after: the approval that can supersede a decision recorded at that time.
func (c *Engine) laterTrustedApproval(ctx context.Context, owner, repo string, number int, head string, after time.Time) (*gh.PullRequestReview, bool) {
	approval, approved := c.latestTrustedHumanReview(ctx, owner, repo, number, head, "APPROVED")
	if !approved || !approval.GetSubmittedAt().After(after) {
		return nil, false
	}
	return approval, true
}

// recordTrustedApproval records approval as an approval-kind decision on
// head and returns the record. It is then the newest decision on head for
// every reader: the stage reads the head as approved, and the planner kicks
// no fix for the instruction it superseded. A dismissal of the review drops
// the record again wherever reconcileReviewBackedApproval runs: the self
// lane's change-request read, the requires_human release
// (trustedHumanApprovalForRequiresHuman) and, for a record made over a
// delegated direction (supersedesDelegated), every stage pass that reads
// it (foldRecordedDecision). False when the record could not be written.
func (c *Engine) recordTrustedApproval(owner, repo string, number int, head string, approval *gh.PullRequestReview, supersedesDelegated bool) (review.HumanChangeRequest, bool) {
	if c == nil || c.humanChangeRequests == nil || approval == nil || strings.TrimSpace(head) == "" {
		return review.HumanChangeRequest{}, false
	}
	req := review.HumanChangeRequest{
		Repo: owner + "/" + repo, Number: number, HeadSHA: head,
		Source: review.HumanChangeRequestSourceReview, SourceID: approval.GetID(),
		Actor: hgithub.SafeGetLogin(approval.GetUser()), Body: approval.GetBody(), URL: approval.GetHTMLURL(),
		Kind: review.HumanChangeRequestKindApprove, RecordedAt: approval.GetSubmittedAt().Time,
		SupersedesDelegated: supersedesDelegated,
	}
	if err := c.humanChangeRequests.Record(req); err != nil {
		c.warn("could not record approval clearing human change request", "pr", number, "error", err)
		return review.HumanChangeRequest{}, false
	}
	c.noteStageTrustedHumanApproval(owner, repo, number, head, req.Actor)
	return req, true
}

// sentinelBlockedUnlessHumanChangesRequested refines the contributor lane's
// sentinel-blocked prefilter skip: a sentinel alert parks the PR on a person
// (085), and a trusted maintainer's CHANGES_REQUESTED review on the current
// head IS that person's answer — the author acts next, so the lane reason
// becomes human-changes-requested and the stage pass derives the fixing
// stage (072 for a clanker's PR) instead of re-asserting 085 over it every
// sweep (hivecommons/hive#15159). The review is read only for a sentinel-
// blocked PR, and the standing request is noted on the stage detail and
// recorded for the nudge like the self lane's. The human-merge-rule skip is
// refined the same way (humanMergeRequiredUnlessHumanChangesRequested). Any
// other reason is returned unchanged; a new head without a review on it is
// blocked again.
func (c *Engine) sentinelBlockedUnlessHumanChangesRequested(ctx context.Context, owner, repo string, pr *gh.PullRequest, reason string) string {
	if reason == autoMergeReasonHumanMergeRequired {
		return c.humanMergeRequiredUnlessHumanChangesRequested(ctx, owner, repo, pr, reason)
	}
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
