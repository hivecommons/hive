package automerge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// SweepQueuedAutoMerges consumes the configured Hive merger-queue label. It
// only squashes open, labelled, non-draft PRs in managed repos after GitHub
// reports them mergeable, commit statuses/check-runs are green, the latest
// Hive App-authored queue approval proves the queuer is not the PR author, and
// the queuer is a TRUSTED merger (audit F3 — see MergerAuthorizer). Without an
// authorizer installed the sweep fails closed and merges nothing.
func (c *Engine) SweepQueuedAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	c.reconcileInactiveNativeQueues(ctx)
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	label := c.transport.AutoMergeLabel()
	result := &AutoMergeSweepResult{}
	noAppBotLoginWarned := false
	noMergerAuthzWarned := false
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)

	// activeRepos: an operator-paused repo receives no automerges (#6203). This
	// sweep is hive-driven, not kick-driven, so leaving it on Repositories()
	// would have kept merging into a repo during its release freeze.
	for _, repo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		if !c.repoAutoMergeAllowed(repo) {
			c.info("automerge sweep skipped repo", "repo", repo, "reason", "repo-auto-merge-disabled")
			continue
		}
		owner, repoName := c.transport.SplitRepo(repo)
		issues, err := c.listQueuedPullRequestIssues(ctx, owner, repoName, label)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Merged) >= maxMerges {
				break
			}
			if issue == nil || !issue.IsPullRequest() {
				continue
			}
			result.Seen++
			if reason := c.prefilterQueuedIssue(issue, label); reason != "" {
				// Only an opted-in repo may hold an App queue entry worth
				// withdrawing; the extra PR read is skipped everywhere else.
				if c.nativeQueue(owner+"/"+repoName) != nil {
					if pr, _, err := c.gh.PullRequests.Get(ctx, owner, repoName, issue.GetNumber()); err == nil {
						c.reconcileNativeQueueGate(ctx, nativeQueueLaneQueued, owner, repoName, pr, reason)
					}
				}
				if reason == "held" || reason == "exempt-label" {
					c.debug("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				} else {
					c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				}
				result.Skipped++
				continue
			}
			c.sweepClock.evaluating(repo, issue.GetNumber())
			event, reason, err := c.trySweepQueuedPR(ctx, repo, owner, repoName, issue.GetNumber(), label)
			c.sweepClock.evaluated()
			if err != nil {
				c.warn("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason == autoMergeReasonNoAppBotLogin {
				if !noAppBotLoginWarned {
					c.warn(autoMergeWarnNoAppBotLogin, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoAppBotLoginOperatorRemediation)
					noAppBotLoginWarned = true
				}
				result.Skipped++
				continue
			}
			if reason == autoMergeReasonNoMergerAuthz {
				if !noMergerAuthzWarned {
					c.warn(autoMergeWarnNoMergerAuthz, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoMergerAuthzRemediation)
					noMergerAuthzWarned = true
				}
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				result.Skipped++
				continue
			}
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

// SweepSelfAuthoredAutoMerges merges the App's OWN open PRs directly on green
// CI, without a human "Approved ... for Hive auto-merge" queue review and
// without waiting on tide.
//
// Why this must exist as a SEPARATE path from SweepQueuedAutoMerges: Prow
// structurally forbids self-approval — tide requires lgtm+approved labels
// from a reviewer distinct from the PR author, and the author here is always
// the App itself. A human queuer can supply that for someone else's PR (the
// existing sweep above), but nobody can supply it for the App's own PR: the
// App cannot review its own work, and asking a human to rubber-stamp every
// App PR defeats the point of automation. So the App must self-merge
// directly over the GitHub REST API (squash), the same bypass-tide mechanism
// SweepQueuedAutoMerges already uses for tide-pending/unstable states (see
// mergeableFromState) — this path just skips the human-queue-approval lookup
// entirely rather than needing one.
//
// Every OTHER safety property is identical to the human queue: mergeability
// (mergeableFromState), green required checks (commitGreen), the intent tier
// gate (selfMergeIntentGate, #6258 — the same refusal predicate
// writeMergeEligible applies before a PR can be queued), and a head-SHA
// re-check immediately before the merge call so a push landing between
// enumeration and merge can never be squashed unreviewed — mirrored below via
// re-fetching the PR right before calling Merge and comparing SHAs, the same
// pattern trySweepQueuedPR uses via the queue-approval's recorded HeadSHA.
// There is no queuedBy in this path, so the author==queuedBy self-merge-ban
// in trySweepQueuedPR simply does not apply — there is no queuer to compare
// against.
func (c *Engine) SweepSelfAuthoredAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	ctx = lowPriority(ctx)
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	result := &AutoMergeSweepResult{}
	if strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		// No usable App identity: there is no "self" to authenticate PRs as
		// App-authored, so this sweep has nothing safe to do. Warn once per
		// call (matching the human-queue sweep's per-call warn cadence) rather
		// than per-repo, since the cause is hive-wide, not per-repo.
		c.warn(autoMergeWarnNoAppBotLogin, "reason", autoMergeReasonNoAppBotLogin, "cause", autoMergeNoAppBotLoginOperatorRemediation)
		return result, nil
	}

	// See the queued sweep above: paused repos are out of scope for automerge.
	selfAuthReleaseBudget := c.selfAuthorizationReleaseBudget()
	branchUpdateAttempts := 0
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)
	for _, repo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(repo)
		if !opts.Targets.selectsRepo(owner + "/" + repoName) {
			continue
		}
		if !c.repoAutoMergeAllowed(repo) {
			c.info("self-authored automerge sweep skipped repo", "repo", repo, "reason", "repo-auto-merge-disabled")
			continue
		}
		prs, skipReason, err := c.openPullRequests(ctx, owner, repoName, opts.Targets)
		if err != nil {
			return result, err
		}
		if skipReason != "" {
			c.info("self-authored automerge sweep skipped repo", "repo", repo, "reason", skipReason)
			continue
		}
		repoSeen := 0
		repoSkipped := 0
		repoCandidates := 0
		repoMergedBefore := len(result.Merged)
		repoSkipReasons := make(map[string]int)
		train := c.planMergeTrains(ctx, stageLaneSelf, repo, owner, repoName, prs, c.prefilterSelfAuthoredPR)
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			repoSeen++
			stage := func(reason string, merged bool, err error) {
				c.reconcileNativeQueueGate(ctx, stageLaneSelf, owner, repoName, pr, reason)
				c.recordStage(stageLaneSelf, repo, owner, repoName, pr, reason, merged, err)
			}
			if _, held := train.duplicateOf(number); held {
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainDuplicate]++
				stage(c.mergeTrainDuplicateSkip(train, number), false, nil)
				continue
			}
			if _, held := train.claimConflictOf(number); held {
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainClaimConflict]++
				stage(c.mergeTrainClaimConflictSkip(train, number), false, nil)
				continue
			}
			if train.isSibling(number) {
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainQueued]++
				stage(c.mergeTrainSkip(train, number), false, nil)
				continue
			}
			if reason, handled := c.mergeTrainFront(ctx, train, pr); handled {
				c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[autoMergeReasonMergeTrainRebasing]++
				stage(reason, false, nil)
				continue
			}
			// #5117 hold release must run before the prefilter: the
			// prefilter skips held PRs outright, and an eligible
			// self-authorization hold has to be released, not skipped.
			if selfAuthReleaseBudget > 0 && pr != nil && !c.selfAuthorizationHoldActive(repo) && c.isHeld(labelNames(pr.Labels)) {
				released, err := c.releaseSelfAuthorizationHoldIfEligible(ctx, repo, owner, repoName, number)
				if err != nil {
					c.warn("self-authored automerge sweep could not evaluate #5117 hold release", "repo", repo, "pr", number, "error", err)
					result.Skipped++
					repoSkipped++
					repoSkipReasons["self-authorization-release-error"]++
					stage("self-authorization-release-error", false, err)
					continue
				}
				if released {
					selfAuthReleaseBudget--
					result.Skipped++
					repoSkipped++
					repoSkipReasons["self-authorization-hold-released"]++
					stage("self-authorization-hold-released", false, nil)
					continue
				}
			}
			// Level-applied holds (#7060) must also be evaluated before the
			// generic prefilter skips held PRs outright. If a hold is still
			// required (or is not a level-applied hold), count it as the returned
			// skip reason; if it was released, skip this tick and let the next
			// sweep evaluate the now-unheld PR.
			if pr != nil && c.isHeld(labelNames(pr.Labels)) {
				if _, reason, err := c.releaseLevelHoldIfEligible(ctx, owner, repoName, pr); err != nil {
					c.warn("self-authored automerge sweep could not evaluate level hold release", "repo", repo, "pr", number, "reason", reason, "error", err)
					result.Skipped++
					repoSkipped++
					if reason == "" {
						reason = "level-hold-release-error"
					}
					repoSkipReasons[reason]++
					stage(reason, false, err)
					continue
				} else if reason != "" {
					if reason == "hold" {
						reason = "held"
					}
					result.Skipped++
					repoSkipped++
					repoSkipReasons[reason]++
					stage(reason, false, nil)
					continue
				}
			}
			if reason := c.detectTrustedRegistryClankerWorkflowApprovalRequired(ctx, repo, owner, repoName, pr); reason != "" {
				if isTerminalRequiredCheckHold(reason) {
					c.info("automerge hold", "repo", repo, "pr", number, "sha", pr.GetHead().GetSHA(), "reason", reason)
				} else {
					c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				}
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, nil)
				continue
			}
			if reason := c.prefilterSelfAuthoredPR(pr); reason != "" {
				c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, nil)
				continue
			}
			result.Candidates++
			repoCandidates++
			c.sweepClock.evaluating(repo, number)
			event, reason, err := c.trySweepSelfAuthoredPR(withMergeTrainCandidateHold(ctx, c.mergeTrainCandidateReason(train, number)), repo, owner, repoName, number, branchUpdateAttempts < selfAuthoredSweepMaxBranchUpdates)
			c.sweepClock.evaluated()
			if event.BranchUpdated || reason == "updated-branch" || reason == "update-branch" {
				branchUpdateAttempts++
			}
			if err != nil {
				c.warnSelfAuthoredSkipOnce(repo, number, reason, err)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, err)
				continue
			}
			if event.BranchUpdated || reason == "updated-branch" {
				result.UpdatedBranches++
			}
			c.noteMergeTrainOutcome(stageLaneSelf, owner, repoName, number, pr.GetHead().GetSHA(), reason)
			if reason != "" {
				if isTerminalRequiredCheckHold(reason) || isPermanentMergeRefusal(reason) {
					if c.markTerminalHoldLogged(owner, repoName, number, pr.GetHead().GetSHA(), reason) {
						c.info("automerge hold", "repo", repo, "pr", number, "sha", pr.GetHead().GetSHA(), "reason", reason)
					}
				} else {
					c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				}
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				stage(reason, false, nil)
				continue
			}
			stage("", true, nil)
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			if train.isFront(number) {
				c.mergeTrainAfterFrontMerge(ctx, train, number)
			}
		}
		if repoSeen > 0 || repoCandidates > 0 || len(result.Merged) > repoMergedBefore {
			args := []any{
				"repo", repo,
				"seen", repoSeen,
				"candidates", repoCandidates,
				"merged", len(result.Merged) - repoMergedBefore,
				"updated_branches", result.UpdatedBranches,
				"skipped", repoSkipped,
			}
			skipReasonOrder := []string{"label:hold", "label:needs-human", "label:needs-rebase", "held", "exempt-label", "draft", "closed", "not-app-authored", "missing-head-sha", "updated-branch", autoMergeReasonBehindBudgetExhausted, "conflicting", autoMergeReasonForkNotEditable, "not-mergeable", autoMergeReasonClosedSuperseded, autoMergeReasonClosedEmptyDiff, autoMergeReasonMergeQueueRequired, autoMergeReasonMergeRefusedReview, autoMergeReasonMergeRefusedMethod, autoMergeReasonMergeTrainQueued, autoMergeReasonMergeTrainRebasing}
			if sentinelLabel := c.sentinelLabelName(); sentinelLabel != "" {
				skipReasonOrder = append([]string{sentinelLabel}, skipReasonOrder...)
			}
			for _, reason := range skipReasonOrder {
				if count := repoSkipReasons[reason]; count > 0 {
					args = append(args, reason, count)
				}
			}
			c.info("self-authored automerge sweep tick", args...)
		}
	}
	return result, nil
}

// prMergeableStateConflicting reads GitHub's conflict flag off a single-PR
// GET payload.
// staleNeedsRebaseLabel: prow's needs-rebase label lags GitHub's
// mergeable_state. When GitHub says the branch is merely "behind" (clean
// update), the label must not block the lane — the lane's UpdateBranch is
// exactly what clears it, and before hivecommons/hive#15103 no lane ever
// reached that call under the label.
func staleNeedsRebaseLabel(blocked string, pr *gh.PullRequest) bool {
	return blocked == "label:needs-rebase" && strings.EqualFold(strings.TrimSpace(pr.GetMergeableState()), "behind")
}

func prMergeableStateConflicting(pr *gh.PullRequest) bool {
	state := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	return state == "dirty" || state == "conflicting"
}

// SweepTrustedAuthorAutoMerges merges green PRs whose human author already
// holds the configured authorized-users role and, by default, GitHub
// repository write/maintain/admin permission. This is an operator opt-in tier:
// unlike App self-merge, a human PR has no structural self-approval problem, so
// the sweep must prove Hive is only executing a merge the author could perform
// themselves.
func (c *Engine) SweepTrustedAuthorAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	ctx = lowPriority(ctx)
	policy := c.currentTrustedAuthorPolicy()
	result := &AutoMergeSweepResult{}
	if !policy.Enabled {
		return result, nil
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)
	for _, displayRepo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(displayRepo)
		if !policy.repoAllowed(displayRepo) || !opts.Targets.selectsRepo(owner+"/"+repoName) {
			continue
		}
		if !c.repoAutoMergeAllowed(displayRepo) {
			c.info("trusted-author automerge sweep skipped repo", "repo", displayRepo, "reason", "repo-auto-merge-disabled")
			continue
		}
		prs, skipReason, err := c.openPullRequests(ctx, owner, repoName, opts.Targets)
		if err != nil {
			return result, err
		}
		if skipReason != "" {
			c.info("trusted-author automerge sweep skipped repo", "repo", displayRepo, "reason", skipReason)
			continue
		}
		train := c.planMergeTrains(ctx, stageLaneTrusted, displayRepo, owner, repoName, prs, func(pr *gh.PullRequest) string {
			login := hgithub.SafeGetLogin(pr.GetUser())
			if opts.SelfAuthoredLaneActive && c.isTrustedBotLaneAuthor(login) {
				return trustedBotLaneSkipReason
			}
			if lane, _ := c.sweepLaneForAuthor(login); lane == stageLaneSelf {
				return trustedAuthorReasonAppAuthored
			}
			return c.prefilterTrustedAuthorPR(owner, repoName, pr, policy)
		})
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			stage := func(reason string, merged bool, err error) {
				c.reconcileNativeQueueGate(ctx, stageLaneTrusted, owner, repoName, pr, reason)
				c.recordStage(stageLaneTrusted, displayRepo, owner, repoName, pr, reason, merged, err)
			}
			if _, held := train.duplicateOf(number); held {
				result.Skipped++
				stage(c.mergeTrainDuplicateSkip(train, number), false, nil)
				continue
			}
			if _, held := train.claimConflictOf(number); held {
				result.Skipped++
				stage(c.mergeTrainClaimConflictSkip(train, number), false, nil)
				continue
			}
			if train.isSibling(number) {
				result.Skipped++
				stage(c.mergeTrainSkip(train, number), false, nil)
				continue
			}
			if reason, handled := c.mergeTrainFront(ctx, train, pr); handled {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			if opts.SelfAuthoredLaneActive && c.isTrustedBotLaneAuthor(hgithub.SafeGetLogin(pr.GetUser())) {
				// The self-authored sweep owns trusted-bot PRs and records
				// their eligibility verdict; the tier hand-off is logged here
				// so every Seen PR still has a reason and Skipped stays
				// consistent, without a misleading untrusted-author-role.
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", trustedBotLaneSkipReason, "tier", "trusted-author")
				result.Skipped++
				stage(trustedBotLaneSkipReason, false, nil)
				continue
			}
			if lane, _ := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); lane == stageLaneSelf {
				// The self-authored lane owns the App's own PRs, so hand
				// off under this tier's own reason rather than a
				// contradictory untrusted-author-role or the contributor
				// lane's reason, which read as if that lane had routed the
				// PR (hivecommons/hive#14482).
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", trustedAuthorReasonAppAuthored, "tier", "trusted-author")
				result.Skipped++
				stage(trustedAuthorReasonAppAuthored, false, nil)
				continue
			}
			if reason := c.prefilterTrustedAuthorPR(owner, repoName, pr, policy); reason != "" {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			result.Candidates++
			c.sweepClock.evaluating(displayRepo, number)
			event, reason, err := c.trySweepTrustedAuthorPR(withMergeTrainCandidateHold(ctx, c.mergeTrainCandidateReason(train, number)), displayRepo, owner, repoName, number, policy)
			c.sweepClock.evaluated()
			if err == nil {
				c.noteMergeTrainOutcome(stageLaneTrusted, owner, repoName, number, pr.GetHead().GetSHA(), reason)
			}
			if err != nil {
				c.warn("trusted-author automerge sweep skipped PR", "repo", displayRepo, "pr", number, "reason", reason, "error", err)
				result.Skipped++
				stage(reason, false, err)
				continue
			}
			if reason != "" {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			stage("", true, nil)
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			if train.isFront(number) {
				c.mergeTrainAfterFrontMerge(ctx, train, number)
			}
		}
	}
	return result, nil
}

// StartSelfAuthoredAutoMergeSweep runs a loop that periodically calls
// SweepSelfAuthoredAutoMerges. It returns immediately; the loop runs until ctx
// is cancelled. A nil client is a no-op. maxMerges is passed straight through
// to AutoMergeSweepOptions.MaxMerges (<=0 falls back to
// DefaultAutoMergeSweepMaxMerges there). Mirrors
// StartMergeRequestWatcher/StartPRRequestWatcher's own-ticker-goroutine
// pattern so all three App-identity-dependent watchers share one shape.
//
// acmmAllowed is the caller-computed
// config.AutoMergeConfig.SelfAuthoredAutoMergeAllowed(acmmLevel) result (both
// the auto_merge.self_authored flag AND the hive's ACMM level gate self-merge
// authority — see config.SelfMergeMinACMMLevel). When false the loop is never
// started at all: an ACMM L4/L5 hive (l4.md/l5.md both forbid the App
// merging its own PRs) must not self-merge, matching console's L6 hive which
// is unaffected and keeps self-merging as before.
// refreshRateLimitCache re-reads GitHub's real rate limits and writes them back
// into the go-github client's cache.
//
// WHY THIS IS NEEDED. go-github refuses requests PRE-EMPTIVELY: once it has seen
// Remaining==0 it returns a synthetic 403 ("not making remote request") for
// every call until the cached Reset time passes, without contacting GitHub.
// That cache lives on the Client and is only updated by responses the Client
// itself receives.
//
// Hive's App client is created ONCE (NewClientFromApp) while appTransport
// injects a freshly minted INSTALLATION TOKEN per request, and installation
// tokens rotate roughly hourly. A new token gets a new allowance — but the
// Client's cache still says Remaining==0 with the old token's reset, so
// go-github keeps refusing requests the new token could happily serve.
//
// Observed live: the dashboard reported core remaining=6613 of 6900 while every
// sweep tick failed with "API rate limit of 6900 still exceeded", and the fleet
// consumed ZERO requests over six minutes — not rate-limited, just refusing.
// Merges stalled for the remainder of the window each time.
//
// GET /rate_limit does not count against any limit, and RateLimitService.Get
// writes the result back into the client's cache, so this is a cheap, exact
// correction rather than a guess.
func (c *Engine) refreshRateLimitCache(ctx context.Context) {
	if c == nil || c.gh == nil {
		return
	}
	if _, _, err := c.gh.RateLimit.Get(ctx); err != nil {
		c.warn("could not refresh rate-limit cache", "error", err)
		return
	}
	c.info("refreshed rate-limit cache after a pre-emptive rate-limit refusal")
}

// isRateLimited reports whether err is a primary/secondary rate-limit error,
// including go-github's pre-emptive synthetic one.
// sweepTickDecision is what one self-authored sweep tick does about the
// GitHub pacer before any request is made.
type sweepTickDecision struct {
	// tick is the ticker period to run at from now; reset says it changed.
	tick  time.Duration
	reset bool
	// cautionTick is non-zero while running at the caution floor.
	cautionTick time.Duration
	// skip sheds this tick: the pacing queue is deeper than it could wait.
	skip bool
}

// sweepTickGate backs the tick off to the caution floor while GitHub is
// pacing the hive after a rate-limit refusal, sheds the tick outright when
// the pacing queue is already deeper than selfAuthoredSweepTickBudget, and
// resumes the adaptive interval once the window ends. shed is
// hgithub.ShedLowPriorityTick (injected by tests).
func (c *Engine) sweepTickGate(interval, cautionTick time.Duration, shed func(string, time.Duration) bool) sweepTickDecision {
	d := sweepTickDecision{tick: interval, cautionTick: cautionTick}
	if !c.cautionEngaged() {
		if cautionTick != 0 {
			d.cautionTick = 0
			d.reset = true
		}
		return d
	}
	if want := cautionSweepInterval(interval); cautionTick != want {
		d.cautionTick = want
		d.tick = want
		d.reset = true
		c.info("self-authored automerge sweep backing off while rate-limit caution window is engaged", "interval", want)
	} else {
		d.tick = want
	}
	if shed != nil && shed("automerge:self-authored-sweep", selfAuthoredSweepTickBudget) {
		d.skip = true
	}
	return d
}

// cautionEngaged reports whether the slow-start caution window is engaged;
// tests inject the predicate through Options.
func (c *Engine) cautionEngaged() bool {
	if c.rateLimitCautious != nil {
		return c.rateLimitCautious()
	}
	return hgithub.RateLimitCautionEngaged()
}

func isRateLimited(err error) bool {
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var ab *gh.AbuseRateLimitError
	return errors.As(err, &ab)
}

// StartSelfAuthoredAutoMergeSweep starts the sweep loop in its own goroutine.
// The returned channel closes once that loop has exited after ctx is
// cancelled, including any sweep tick that was in flight, or immediately when
// the sweep does not start at all. A caller that restarts the sweep on a
// rebuilt client (#9621) waits on it so two sweeps never overlap.
func (c *Engine) StartSelfAuthoredAutoMergeSweep(ctx context.Context, maxMerges int, acmmAllowed bool, acmmLevel *int) <-chan struct{} {
	done := make(chan struct{})
	if !c.ready() {
		close(done)
		return done
	}
	if !acmmAllowed && !c.currentTrustedAuthorPolicy().Enabled && !c.currentContributorLanePolicy().Enabled {
		level := "unset"
		if acmmLevel != nil {
			level = fmt.Sprintf("%d", *acmmLevel)
		}
		c.info("self-authored auto-merge sweep disabled: acmm_level below minimum (or auto_merge.self_authored is off)",
			"acmm_level", level, "min_acmm_level", selfMergeMinACMMLevel)
		close(done)
		return done
	}
	if c.openPRs != nil {
		go func() {
			defer close(done)
			c.runEventDrivenSweep(ctx, maxMerges, acmmAllowed)
		}()
		c.info("automerge lanes started event-driven on the forgestate cache",
			"floor_interval", automergeSweepFloorInterval,
			"merge_freshness_bound", automergeMergeFreshnessBound,
			"repos", len(c.transport.Repositories()))
		return done
	}
	repos := len(c.transport.Repositories())
	interval := selfAuthoredSweepInterval(repos)
	ctx = lowPriority(ctx)
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		c.sweepClock.armFloorEvery(c.now(), interval)
		// cautionTick is non-zero while the ticker is running at the caution
		// floor instead of the adaptive interval.
		var cautionTick time.Duration
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.sweepTrigger.C():
				// A webhook nudge (CI failure on a PR, operator row action)
				// names the PRs; evaluate only those now. A full request
				// (nil targets) is a regular pass brought forward.
				c.sweepPass(ctx, maxMerges, acmmAllowed, c.sweepTrigger.take(), "event")
			case <-t.C:
				gate := c.sweepTickGate(interval, cautionTick, hgithub.ShedLowPriorityTick)
				if gate.reset {
					t.Reset(gate.tick)
				}
				cautionTick = gate.cautionTick
				tick := interval
				if cautionTick != 0 {
					tick = cautionTick
				}
				c.sweepClock.armFloorEvery(c.now(), tick)
				if gate.skip {
					continue
				}
				result := c.sweepPass(ctx, maxMerges, acmmAllowed, nil, "tick")
				if result == nil {
					continue
				}
				if next, changed := nextSelfAuthoredSweepInterval(repos, interval, result); changed {
					t.Reset(next)
					c.sweepClock.armFloorEvery(c.now(), next)
					c.info("self-authored automerge sweep interval adjusted",
						"previous_interval", interval,
						"next_interval", next,
						"repos", repos,
						"candidates", result.Candidates,
						"default_candidate_allowance", selfAuthoredSweepCandidateAllowance)
					interval = next
					if cautionTick != 0 {
						cautionTick = cautionSweepInterval(interval)
						t.Reset(cautionTick)
					}
				}
			}
		}
	}()
	c.info("self-authored automerge sweep started", "interval", interval, "repos", len(c.transport.Repositories()))
	return done
}

// sweepPass runs the three lanes once, in order, sharing one merge budget,
// then reconciles stage labels. targets narrows an event-driven pass to the
// PRs a webhook named (nil: everything); cause names what started the pass
// for the log. It returns nil when the self-authored lane failed outright
// (its callers then keep the current cadence).
func (c *Engine) sweepPass(ctx context.Context, maxMerges int, acmmAllowed bool, targets SweepTargets, cause string) *AutoMergeSweepResult {
	c.sweepClock.begin(cause, targets, c.now())
	defer func() { c.sweepClock.end(c.now()) }()
	result := &AutoMergeSweepResult{}
	if acmmAllowed {
		var err error
		result, err = c.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: maxMerges, Targets: targets})
		if err != nil {
			c.warn("self-authored automerge sweep failed", "error", err, "cause", cause)
			// A rate-limit refusal may be go-github's cached verdict
			// from a token that has since rotated. Re-read the real
			// limits (free, and it updates that cache) so the next tick
			// is decided on current truth instead of repeating a stale
			// refusal for the rest of the window.
			if isRateLimited(err) {
				c.refreshRateLimitCache(ctx)
			}
			return nil
		}
	}
	remaining := maxMerges
	if remaining <= 0 {
		remaining = DefaultAutoMergeSweepMaxMerges
	}
	remaining -= len(result.Merged)
	if remaining > 0 && c.currentTrustedAuthorPolicy().Enabled {
		trustedResult, trustedErr := c.SweepTrustedAuthorAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: remaining, SelfAuthoredLaneActive: acmmAllowed, Targets: targets})
		if trustedErr != nil {
			c.warn("trusted-author automerge sweep failed", "error", trustedErr, "cause", cause)
			if isRateLimited(trustedErr) {
				c.refreshRateLimitCache(ctx)
			}
		} else {
			result.Seen += trustedResult.Seen
			result.Skipped += trustedResult.Skipped
			result.Candidates += trustedResult.Candidates
			result.Merged = append(result.Merged, trustedResult.Merged...)
		}
	}
	remaining = maxMerges
	if remaining <= 0 {
		remaining = DefaultAutoMergeSweepMaxMerges
	}
	remaining -= len(result.Merged)
	if remaining > 0 && c.currentContributorLanePolicy().Enabled {
		laneResult, laneErr := c.SweepContributorLaneAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: remaining, Targets: targets})
		if laneErr != nil {
			c.warn("contributor-lane automerge sweep failed", "error", laneErr, "cause", cause)
			if isRateLimited(laneErr) {
				c.refreshRateLimitCache(ctx)
			}
		} else {
			result.Seen += laneResult.Seen
			result.Skipped += laneResult.Skipped
			result.Candidates += laneResult.Candidates
			result.Merged = append(result.Merged, laneResult.Merged...)
		}
	}
	if stageResult, stageErr := c.ApplyStageLabels(ctx); stageErr != nil {
		c.warn("stage labels reconcile failed", "error", stageErr, "items", stageResult.Items, "writes", stageResult.Writes)
	} else if stageResult.Writes+stageResult.Comments > 0 {
		c.info("stage labels reconciled", "items", stageResult.Items, "writes", stageResult.Writes, "comments", stageResult.Comments)
	}
	return result
}

// listOpenAppAuthoredPullRequests returns every open PR in owner/repo. Uses the
// PR list endpoint (not issue search) because the caller needs PullRequest
// objects: PullRequests.List populates Labels and Head.SHA, so cheap gates can
// run before paying for PullRequests.Get. It does not populate MergeableState;
// PRs that survive the cheap gates are fetched once for that safety-critical
// evaluation data and fetched again immediately before merge to re-verify SHA.
func (c *Engine) listOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []*gh.PullRequest
	for {
		prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open PRs for %s/%s: %w", owner, repo, err)
		}
		out = append(out, prs...)
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// trySweepSelfAuthoredPR evaluates and, if eligible, merges one App-authored
// PR. Re-fetches the PR immediately before merging to re-verify the head SHA
// against the one that was evaluated as green — the same
// evaluated-then-re-verified-at-merge-time safety property trySweepQueuedPR
// gets from the queue approval's recorded HeadSHA, just without a stored
// approval record to compare against (there is no queue step in this path).

func (c *Engine) releaseHoldActive(ctx context.Context, owner, repo, branch string) (bool, string, error) {
	branch = strings.TrimSpace(branch)
	if c == nil || c.gh == nil || branch == "" {
		return false, "", nil
	}
	br, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 3)
	if err != nil {
		return false, "release-hold-check", err
	}
	sha := br.GetCommit().GetSHA()
	if sha == "" {
		return false, "", nil
	}
	statuses, _, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, sha, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return false, "release-hold-check", err
	}
	for _, st := range statuses.Statuses {
		if st == nil || !strings.EqualFold(st.GetContext(), releaseInProgressStatusContext) {
			continue
		}
		if !strings.EqualFold(st.GetState(), "pending") {
			return false, "", nil
		}
		updated := st.GetUpdatedAt().Time
		if updated.IsZero() || c.now().Sub(updated) <= releaseInProgressMaxAge {
			return true, releaseInProgressStatusContext, nil
		}
		return false, "", nil
	}
	return false, "", nil
}
