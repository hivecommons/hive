package automerge

// The promotion lane merges a branch-promotion pull request
// (auto_merge.promotions): one that promotes a protected source branch to a
// target branch (main into stable, say), opened by a listed author from a
// listed head ref, whose head tree is exactly the source branch's current
// tree and whose head is either the source tip (or an ancestor of it) or a
// single commit on the live target tip (hgithub.VerifyPromotion). Its
// content is the source branch's, which took changes only through pull
// requests, so the lane needs no linked issue and no author tier. It is a narrow content-equality path, not
// a trusted author: the author earns nothing anywhere else, and a label on
// the pull request is never read as trust.
//
// The source branch must take changes only through a pull request (classic
// protection requiring pull request reviews without force pushes, or an
// active ruleset pull_request rule), because trust carries over: the target
// inherits exactly the source's review guarantee, with the approvals and
// bypass actors the owner configured.
//
// The lane lives inside the self-authored sweep and passes the same gates as
// the App's own pull requests: required checks green on the target branch,
// the human-merge title/label rule (needs-owner-merge), holds and
// blocked/exempt/sentinel labels, human_merge_paths, the review verdict hold,
// the intent tier gate, the linked-issue holds, the approval desk, the
// repository's merge method (squash_only), the merge train, and the
// expected-head SHA re-read before the merge. human_merge_paths is the one
// gate a rule can set aside, with inherit_human_merge_paths
// (automerge_promotion_human_merge_paths.go). The lane then proves what it
// proves for every promotion, that the tree is the source's current tree,
// and nothing about how a listed file reached the source. The hive never writes to a
// promotion branch, so the lane never updates it: a behind promotion (the
// target requires up-to-date branches) holds as promotion-behind-target, a
// serialized repository's promotion as promotion-serialized-repo, and
// require_current_base is replaced by a stronger check of the merge result.
// Immediately before the merge or enqueue call it asks GitHub again whether
// the head's tree still equals the protected source tip's tree, and whether
// GitHub's test merge onto the live target tip carries exactly that tree
// (hgithub.VerifyPromotionMerge): if not, nothing merges this pass. A queued
// promotion is checked again on every pass and withdrawn from the queue
// when it no longer matches.
//
// Routing (which lane owns the pull request) also uses the live check, held
// for promotionRouteTTL per head so one sweep pass does not repeat it for
// each lane, and only while a self-authored sweep runs. A pull request that
// does not match is never handed off: the trusted-author and contributor
// lanes evaluate it exactly as before. A head the lane refused for a reason
// no later pass cures (promotionHandBackReasons) is handed back the same
// way, so a person is told to merge it.

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	// lanePromotion is the self-authored sweep's lane for a matching
	// branch-promotion pull request.
	lanePromotion = "promotion"
	// promotionLaneSkipReason is the trusted-author and contributor lanes'
	// hand-off reason for a pull request the promotion lane owns.
	promotionLaneSkipReason = prstage.PromotionLaneSkipReason
	// autoMergeReasonPromotionSourceMoved: the live check right before the
	// merge no longer matched (usually the source branch moved), so the
	// lane merges nothing this pass. The next pass routes the pull request
	// again from a fresh check.
	autoMergeReasonPromotionSourceMoved = "promotion-source-moved"
	// autoMergeReasonPromotionBehindTarget: the target requires up-to-date
	// branches and the promotion is behind it. The hive never updates a
	// promotion branch, so a person merges it (or the workflow refreshes it).
	autoMergeReasonPromotionBehindTarget = "promotion-behind-target"
	// autoMergeReasonPromotionSerialized: the repository merges through the
	// serialized lane, which updates its front branch; a person merges a
	// promotion there.
	autoMergeReasonPromotionSerialized = "promotion-serialized-repo"
	// autoMergeReasonPromotionNoLongerMatches: a pull request a promotion
	// rule names no longer passes the live check; recorded only to withdraw
	// a native merge queue entry the lane made earlier.
	autoMergeReasonPromotionNoLongerMatches = "promotion-no-longer-matches"
	// promotionRouteTTL bounds how long a routing check is reused for the
	// same head. It never applies to the check before the merge call.
	promotionRouteTTL = 30 * time.Second
	// promotionCheckTimeout bounds one live promotion check (two to five
	// REST calls).
	promotionCheckTimeout = 30 * time.Second
)

// promotionHandBackReasons are the promotion lane's refusals no later pass
// cures on the same head: the target requires up-to-date branches and the
// hive never updates the branch, the repository merges through the
// serialized lane, or GitHub's test merge does not carry the source tree.
// The lane remembers the head (promotionHandBack) and stops claiming it,
// so the other lanes route it as they do any pull request (today's
// hand-merge path, shown to a person). A new head is judged afresh.
var promotionHandBackReasons = map[string]bool{
	autoMergeReasonPromotionBehindTarget:     true,
	autoMergeReasonPromotionSerialized:       true,
	hgithub.PromotionReasonMergeTreeMismatch: true,
}

func promotionHandBackKey(owner, repo string, number int, head string) string {
	return "promotion-hand-back:" + strings.ToLower(owner+"/"+repo) + "#" + strconv.Itoa(number) + ":" + strings.ToLower(strings.TrimSpace(head))
}

// promotionHandBack records that the lane refused head for a reason no
// later pass cures (promotionHandBackReasons), logging and auditing it once.
// The mark lives in the sweep memory every engine over the same client
// shares, so the one-shot lanes and the merge relay see it too.
func (c *Engine) promotionHandBack(displayRepo, owner, repo string, number int, head, reason string) {
	if c == nil || !promotionHandBackReasons[reason] || head == "" {
		return
	}
	if c.memory().markOnce(promotionHandBackKey(owner, repo, number, head), c.clock()) {
		c.info("promotion lane: handing the pull request back to a person", "repo", displayRepo, "pr", number, "head", head, "reason", reason)
		c.recordPromotionDecision(owner+"/"+repo, number, "handed-back:"+reason, hgithub.PromotionCheck{HeadSHA: head})
	}
}

func (c *Engine) promotionHandedBack(owner, repo string, number int, head string) bool {
	return c != nil && head != "" && c.memory().hasOnce(promotionHandBackKey(owner, repo, number, head), c.clock())
}

// selfAuthoredLaneStateName is the client state that counts the running
// self-authored sweeps (StartSelfAuthoredAutoMergeSweep with acmmAllowed),
// shared by every engine over the same client.
const selfAuthoredLaneStateName = "automerge.self-authored-lane-running"

// selfAuthoredLaneState returns the transport's running-sweep counter, or
// nil when the transport keeps no shared state.
func selfAuthoredLaneState(t Transport) *atomic.Int32 {
	provider, ok := t.(interface {
		SharedState(name string, build func() any) any
	})
	if !ok {
		return nil
	}
	n, _ := provider.SharedState(selfAuthoredLaneStateName, func() any { return new(atomic.Int32) }).(*atomic.Int32)
	return n
}

// markSelfAuthoredLaneRunning counts one running self-authored sweep and
// returns the call that uncounts it. A counter, not a flag: a restart may
// start the new sweep before the old one has stopped.
func (c *Engine) markSelfAuthoredLaneRunning() func() {
	n := selfAuthoredLaneState(c.transport)
	if n == nil {
		return func() {}
	}
	n.Add(1)
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			n.Add(-1)
		}
	}
}

// selfAuthoredLaneRunning reports whether a self-authored sweep (and so the
// promotion lane) runs over this client, with an App login to merge as.
func (c *Engine) selfAuthoredLaneRunning() bool {
	if c == nil {
		return false
	}
	return SelfAuthoredLaneRunning(c.transport)
}

// SelfAuthoredLaneRunning is selfAuthoredLaneRunning for a caller outside
// the engine (the sentinel's promotion exemption), over the same transport
// the self-authored sweep runs on. A transport that keeps no shared state,
// or another client than the sweep's, reads as not running.
func SelfAuthoredLaneRunning(t Transport) bool {
	if t == nil || strings.TrimSpace(t.AppBotLogin()) == "" {
		return false
	}
	n := selfAuthoredLaneState(t)
	return n != nil && n.Load() > 0
}

// promotionHandoffKey marks a context in which the self-authored sweep (and
// so the promotion lane) is running this pass: only then do the other lanes
// hand a promotion to it.
type promotionHandoffKey struct{}

func contextWithPromotionHandoff(ctx context.Context) context.Context {
	return context.WithValue(ctx, promotionHandoffKey{}, true)
}

func promotionHandoffAllowed(ctx context.Context) bool {
	allowed, _ := ctx.Value(promotionHandoffKey{}).(bool)
	return allowed
}

// promotionRoute is one memoized routing check.
type promotionRoute struct {
	head  string
	check hgithub.PromotionCheck
	at    time.Time
}

// promotionAuditor is the optional transport hook that records one
// promotion decision in the audit log (hgithub.Client implements it).
type promotionAuditor interface {
	RecordPromotionDecision(repo string, number int, decision string, check hgithub.PromotionCheck)
}

// prBaseRepo returns the owner and name of the repository pr targets, from
// its base payload. Callers that know the repository they listed pass it to
// promotionLanePRIn instead; this is for the few that only hold the payload.
// A payload rebuilt from the ForgeState cache carries no base repository, so
// the head repository stands in: PromotionRuleFor only ever matches a head
// that lives in that same repository.
func prBaseRepo(pr *gh.PullRequest) (string, string) {
	if pr == nil {
		return "", ""
	}
	r := pr.GetBase().GetRepo()
	if r == nil {
		r = pr.GetHead().GetRepo()
	}
	if r == nil {
		return "", ""
	}
	if owner, name, ok := strings.Cut(strings.TrimSpace(r.GetFullName()), "/"); ok && owner != "" && name != "" {
		return owner, name
	}
	return strings.TrimSpace(r.GetOwner().GetLogin()), strings.TrimSpace(r.GetName())
}

// promotionRule returns the auto_merge.promotions rule pr names (base,
// author, head ref, same-repository head), with no GitHub call.
func (c *Engine) promotionRule(owner, repo string, pr *gh.PullRequest) (config.PromotionRule, bool) {
	if c == nil || c.promotions == nil || c.gh == nil || owner == "" || repo == "" {
		return config.PromotionRule{}, false
	}
	return hgithub.PromotionRuleFor(c.promotions(owner+"/"+repo), owner, repo, pr)
}

// promotionBranch reports whether any configured auto_merge.promotions rule
// names pr (base, author, head ref, same-repository head; no GitHub call),
// whether or not the promotion lane runs (Options.PromotionBranches, read
// without the lane's ACMM and self_authored gate). The hive never writes to
// such a branch.
func (c *Engine) promotionBranch(owner, repo string, pr *gh.PullRequest) bool {
	if c == nil || owner == "" || repo == "" || pr == nil {
		return false
	}
	rules := c.promotionBranches
	if rules == nil {
		rules = c.promotions
	}
	if rules == nil {
		return false
	}
	_, ok := hgithub.PromotionRuleFor(rules(owner+"/"+repo), owner, repo, pr)
	return ok
}

// promotionLanePR reports whether the promotion lane owns pr: a rule names
// it and the live check matches. The check is reused for the same head for
// promotionRouteTTL; a read error is never remembered and never a match.
func (c *Engine) promotionLanePR(ctx context.Context, pr *gh.PullRequest) (hgithub.PromotionCheck, bool) {
	owner, repo := prBaseRepo(pr)
	return c.promotionLanePRIn(ctx, owner, repo, pr)
}

// promotionLanePRIn is promotionLanePR for a pull request listed from
// owner/repo.
func (c *Engine) promotionLanePRIn(ctx context.Context, owner, repo string, pr *gh.PullRequest) (hgithub.PromotionCheck, bool) {
	rule, ok := c.promotionRule(owner, repo, pr)
	if !ok {
		return hgithub.PromotionCheck{}, false
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if c.promotionHandedBack(owner, repo, pr.GetNumber(), head) {
		// Refused for good on this head: a person merges it.
		return hgithub.PromotionCheck{HeadSHA: head, Rule: rule}, false
	}
	key := strings.ToLower(owner+"/"+repo) + "#" + strconv.Itoa(pr.GetNumber())
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	c.promotionRoutesMu.Lock()
	if e, ok := c.promotionRoutes[key]; ok && head != "" && e.head == head && now.Sub(e.at) < promotionRouteTTL {
		c.promotionRoutesMu.Unlock()
		return e.check, e.check.Matched
	}
	c.promotionRoutesMu.Unlock()
	checkCtx, cancel := context.WithTimeout(ctx, promotionCheckTimeout)
	check, err := hgithub.VerifyPromotion(checkCtx, c.gh, owner, repo, pr, rule)
	cancel()
	if err != nil {
		c.warn("promotion lane: live tree check failed; the pull request takes its usual path", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "head", head, "source", rule.Source, "reason", check.Reason, "error", err)
		return check, false
	}
	c.promotionRoutesMu.Lock()
	if c.promotionRoutes == nil {
		c.promotionRoutes = map[string]promotionRoute{}
	}
	c.promotionRoutes[key] = promotionRoute{head: head, check: check, at: now}
	c.promotionRoutesMu.Unlock()
	if !check.Matched {
		c.debug("promotion lane: pull request does not match its rule", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "head", head, "source", rule.Source, "source_sha", check.SourceSHA, "tree", check.TreeSHA, "head_tree", check.HeadTreeSHA, "reason", check.Reason)
	}
	return check, check.Matched
}

// promotionLaneOwns reports whether another lane should hand pr, listed
// from owner/repo, to the promotion lane: the author holds no
// self-authored-sweep lane of its own (the App and trusted bots keep
// theirs) and pr matches a promotion rule, read live.
//
// It is false unless a self-authored sweep runs over this client with an
// App login (selfAuthoredLaneRunning), so neither a lane hand-off, the merge
// relay nor integration execution ever claims a pull request for a lane
// that is not running. The trusted-author lane also asks only while
// AutoMergeSweepOptions.SelfAuthoredLaneActive; the contributor lane asks
// promotionLaneOwnsInPass, which needs the same signal on its context.
func (c *Engine) promotionLaneOwns(ctx context.Context, owner, repo string, pr *gh.PullRequest) bool {
	if c == nil || pr == nil || c.promotions == nil || !c.selfAuthoredLaneRunning() {
		return false
	}
	if _, own := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); own {
		return false
	}
	_, ok := c.promotionLanePRIn(ctx, owner, repo, pr)
	return ok
}

// promotionMergeRecheck asks GitHub again, bypassing the routing memo,
// whether pr (the payload re-read right before the merge, at the head the
// lane evaluated) still carries exactly the protected source branch's tree,
// and whether GitHub's test merge onto the live target tip carries exactly
// that tree too (hgithub.VerifyPromotionMerge). It returns "" and the fresh
// check when both hold. Otherwise it returns
// autoMergeReasonPromotionSourceMoved when the head no longer matches the
// source, or the merge check's own reason (promotion-merge-tree-mismatch,
// promotion-merge-unknown); the refusal is logged and audited and the
// routing memo dropped, so the next pass decides again. Every refusal reason
// withdraws a native merge queue entry (nativeQueueWithdrawReason).
func (c *Engine) promotionMergeRecheck(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) (hgithub.PromotionCheck, string) {
	rule, ok := c.promotionRule(owner, repo, pr)
	if !ok {
		c.forgetPromotionRoute(owner, repo, pr.GetNumber())
		c.info("promotion lane: pull request no longer matches a promotion rule; not merging this pass", "repo", displayRepo, "pr", pr.GetNumber(), "head", pr.GetHead().GetSHA())
		return hgithub.PromotionCheck{}, autoMergeReasonPromotionSourceMoved
	}
	ctx, cancel := context.WithTimeout(ctx, promotionCheckTimeout)
	defer cancel()
	check, err := hgithub.VerifyPromotion(ctx, c.gh, owner, repo, pr, rule)
	reason := autoMergeReasonPromotionSourceMoved
	if check.Matched {
		check, err = hgithub.VerifyPromotionMerge(ctx, c.gh, owner, repo, pr, check)
		if check.Matched {
			return check, ""
		}
		reason = check.Reason
	}
	c.forgetPromotionRoute(owner, repo, pr.GetNumber())
	c.info("promotion lane: the head or its merge result no longer equals the source tip; not merging this pass", "repo", displayRepo, "pr", pr.GetNumber(), "head", check.HeadSHA, "source", rule.Source, "source_sha", check.SourceSHA, "tree", check.TreeSHA, "head_tree", check.HeadTreeSHA, "merge_commit", check.MergeSHA, "merge_tree", check.MergeTreeSHA, "target_sha", check.TargetSHA, "reason", check.Reason, "error", err)
	c.recordPromotionDecision(owner+"/"+repo, pr.GetNumber(), "merge-refused:"+check.Reason, check)
	return check, reason
}

// promotionWithdrawIfStale withdraws a native merge queue entry the lane
// made for a pull request a promotion rule names, listed from owner/repo,
// once it no longer passes the routing check: a queued entry merges later,
// on GitHub's schedule, so it must not outlive the match. The check itself
// is the routing check (memoized per head); the queue is read only where
// the repository's base has a native queue. A head that still matches is
// re-checked in full by the lane itself before anything else happens, and
// its refusal reasons withdraw the entry the same way.
func (c *Engine) promotionWithdrawIfStale(ctx context.Context, lane, owner, repo string, pr *gh.PullRequest) {
	if c == nil || pr == nil {
		return
	}
	if _, ok := c.promotionRule(owner, repo, pr); !ok {
		return
	}
	if _, matched := c.promotionLanePRIn(ctx, owner, repo, pr); matched {
		return
	}
	if c.markTerminalHoldLogged(owner, repo, pr.GetNumber(), pr.GetHead().GetSHA(), autoMergeReasonPromotionNoLongerMatches) {
		c.info("promotion lane: pull request no longer matches; withdrawing any queue entry the hive made", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "head", pr.GetHead().GetSHA())
		c.recordPromotionDecision(owner+"/"+repo, pr.GetNumber(), "queue-withdraw:"+autoMergeReasonPromotionNoLongerMatches, hgithub.PromotionCheck{HeadSHA: pr.GetHead().GetSHA()})
	}
	c.reconcileNativeQueueGate(ctx, lane, owner, repo, pr, autoMergeReasonPromotionNoLongerMatches)
}

func (c *Engine) forgetPromotionRoute(owner, repo string, number int) {
	if c == nil {
		return
	}
	c.promotionRoutesMu.Lock()
	delete(c.promotionRoutes, strings.ToLower(owner+"/"+repo)+"#"+strconv.Itoa(number))
	c.promotionRoutesMu.Unlock()
}

// recordPromotionDecision audits one promotion decision through the
// transport when it can.
func (c *Engine) recordPromotionDecision(repo string, number int, decision string, check hgithub.PromotionCheck) {
	if c == nil || c.transport == nil {
		return
	}
	if a, ok := c.transport.(promotionAuditor); ok {
		a.RecordPromotionDecision(repo, number, decision, check)
	}
}

// promotionLaneOwnsInPass is promotionLaneOwns for a lane whose context says
// whether the self-authored sweep runs this pass (contextWithPromotionHandoff):
// a pull request is never handed to a lane that is not running.
func (c *Engine) promotionLaneOwnsInPass(ctx context.Context, owner, repo string, pr *gh.PullRequest) bool {
	return promotionHandoffAllowed(ctx) && c.promotionLaneOwns(ctx, owner, repo, pr)
}

// promotionVerifyMergedTree reads the commit a promotion merge landed and
// compares its tree to the test merge tree the lane verified (defense in
// depth: GitHub merges what it tested, but a target that moved between the
// check and the merge would show here). A mismatch, or a commit that cannot
// be read, is logged and audited; nothing is rolled back.
func (c *Engine) promotionVerifyMergedTree(ctx context.Context, displayRepo, owner, repo string, number int, mergeSHA string, check hgithub.PromotionCheck) {
	if c == nil || c.gh == nil || strings.TrimSpace(mergeSHA) == "" || check.MergeTreeSHA == "" {
		return
	}
	ctx, cancel := context.WithTimeout(hgithub.WithRESTCaller(ctx, "hive:promotion_check"), promotionCheckTimeout)
	defer cancel()
	merged, _, err := c.gh.Git.GetCommit(ctx, owner, repo, mergeSHA)
	tree := strings.TrimSpace(merged.GetTree().GetSHA())
	if err == nil && tree == check.MergeTreeSHA {
		return
	}
	c.warn("promotion lane: the merged commit's tree is not the tree the lane verified", "repo", displayRepo, "pr", number, "merge_sha", mergeSHA, "merged_tree", tree, "verified_tree", check.MergeTreeSHA, "source_sha", check.SourceSHA, "error", err)
	mismatch := check
	mismatch.MergeSHA, mismatch.MergeTreeSHA = mergeSHA, tree
	c.recordPromotionDecision(owner+"/"+repo, number, "merged-tree-mismatch", mismatch)
}
