package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

func (c *Engine) trySweepQueuedPR(ctx context.Context, displayRepo, owner, repo string, number int, label string) (eventOut AutoMergeSweepEvent, reasonOut string, errOut error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	defer func() { c.reconcileNativeQueueGate(ctx, nativeQueueLaneQueued, owner, repo, pr, reasonOut) }()
	c.noteStagePR(owner, repo, number, pr)
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	labels := hgithub.ExtractPRLabels(pr.Labels)
	if !hasLabel(labels, label) {
		return AutoMergeSweepEvent{}, "label-removed", nil
	}
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	// Hold labels outrank the merger queue (#5589): a hold applied AFTER a
	// merger queued the PR — including the hold guard re-applying one because
	// the branch moved while hold-gated — must stop the sweep, not race it.
	if c.isHeld(labels) {
		return AutoMergeSweepEvent{}, "held", nil
	}
	if c.transport.IsExemptLabels(labels) {
		return AutoMergeSweepEvent{}, "exempt-label", nil
	}

	author := hgithub.SafeGetLogin(pr.GetUser())
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	method, methodReason := c.mergeMethodWithReason(ctx, owner, repo, pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(method, methodReason, headSHA, "queued"),
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
	if strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return AutoMergeSweepEvent{}, autoMergeReasonNoAppBotLogin, nil
	}
	approval, ok, reason, err := c.latestHiveQueueApproval(ctx, owner, repo, number)
	if err != nil {
		return AutoMergeSweepEvent{}, "queue-approval-check", err
	}
	if !ok {
		if reason != "" {
			return AutoMergeSweepEvent{}, reason, nil
		}
		return AutoMergeSweepEvent{}, autoMergeReasonNoHiveQueueApproval, nil
	}
	if approval.HeadSHA == "" {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval is missing a reviewed head SHA — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-missing-head", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-missing-head", nil
	}
	if approval.HeadSHA != headSHA {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval head changed since approval — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-head-changed", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-head-changed", nil
	}
	queuedBy := approval.QueuedBy
	if strings.EqualFold(author, queuedBy) {
		return AutoMergeSweepEvent{}, "self-merge-ban", nil
	}
	// SECURITY (audit F3): the self-merge ban above only proves queuer !=
	// author. It is defeated by a sockpuppet — a second account queues and
	// approves the first account's work — and on its own it lets ANY actor who
	// can get the merger-queue label applied merge anything. Re-verify the
	// merger tier here, at the point the merge actually happens, rather than
	// trusting the queue-time check in the dashboard handler.
	trusted, configured := c.isTrustedMerger(queuedBy)
	if !configured {
		return AutoMergeSweepEvent{}, autoMergeReasonNoMergerAuthz, nil
	}
	if !trusted {
		c.warn(autoMergeWarnUntrustedMerger, "owner", owner, "repo", repo, "pr", number,
			"queued_by", queuedBy, "author", author)
		return AutoMergeSweepEvent{}, autoMergeReasonUntrustedMerger, nil
	}

	mergeable := hgithub.MergeableFromState(pr.GetMergeableState(), pr.Mergeable)
	// On a queue-required base GitHub reports blocked until the PR merges
	// through the queue; the queue, not this gate, settles that state.
	if mergeable != hgithub.MergeableYes && !(nativeQueueGatedState(pr) && c.nativeQueueEnabled(ctx, owner+"/"+repo, pr)) {
		return AutoMergeSweepEvent{}, "not-mergeable", nil
	}
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, headSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Human-merge paths (#11038): a merger queuing the PR is not the same as
	// a person merging it, so the queued lane honors the list too.
	if fence := carriedFenceGate(displayRepo, pr, c.info); fence != "" {
		return AutoMergeSweepEvent{}, fence, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}

	// Approval desk (RFC #4000) for the trusted human merge-queue lane.
	// Consulted only after the legacy queue approval, trusted-merger,
	// mergeability, and green-check gates pass, so enabling the desk can record
	// or withhold this operation but cannot widen merge authority.
	if allow, deskReason := c.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{
		Kind:        hgithub.ApprovalDeskKindQueuedMerge,
		Repo:        displayRepo,
		Number:      number,
		Author:      author,
		Title:       pr.GetTitle(),
		Labels:      labels,
		ChecksGreen: true,
		HeadSHA:     headSHA,
	}); !allow {
		return AutoMergeSweepEvent{}, deskReason, nil
	}

	if reason, err := c.tryNativeQueue(ctx, pr, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: baseBranch, number: number, head: headSHA, state: pr.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, pr, headSHA, method)
	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, headSHA, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, mergeMessage, mergeOpts)
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if c.requiredChecksExpected(attempt, err) {
			return AutoMergeSweepEvent{}, autoMergeReasonChecksExpected, nil
		}
		if refused := c.refusedMerge(attempt, err); refused != "" {
			return AutoMergeSweepEvent{}, refused, nil
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Same audit obligation as the self-authored path above: pr_merged on
	// the trail is what makes this merge count as hive output. path=queue:
	// a person queued this PR; the sweep only carried the merge out.
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathQueue)
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: queuedBy,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Label:    label,
		Tier:     "queued",
	}
	c.info("automerge sweep merged PR", "repo", displayRepo, "pr", number, "queued_by", queuedBy, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) latestHiveQueueApproval(ctx context.Context, owner, repo string, number int) (hiveQueueApproval, bool, string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := hiveQueueApproval{}
	untrusted := false
	var seen []*gh.PullRequestReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return hiveQueueApproval{}, false, "", err
		}
		seen = append(seen, reviews...)
		for _, review := range reviews {
			if !strings.EqualFold(review.GetState(), "APPROVED") {
				continue
			}
			queuedBy := parseHiveQueueReview(review.GetBody())
			if queuedBy == "" {
				continue
			}
			if !c.isHiveAppReviewAuthor(review) {
				untrusted = true
				c.warn(autoMergeWarnUntrustedQueueApproval, "owner", owner, "repo", repo, "pr", number, "review_author", hgithub.SafeGetLogin(review.GetUser()), "claimed_queued_by", queuedBy, "expected_app_bot", c.transport.AppBotLogin())
				continue
			}
			latest = hiveQueueApproval{QueuedBy: queuedBy, HeadSHA: review.GetCommitID()}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.noteStageReviews(owner, repo, number, seen)
	if latest.QueuedBy != "" {
		return latest, true, "", nil
	}
	if untrusted {
		return latest, false, autoMergeReasonUntrustedQueueApproval, nil
	}
	return latest, false, "", nil
}

func (c *Engine) isHiveAppReviewAuthor(review *gh.PullRequestReview) bool {
	if c == nil || review == nil || strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return false
	}
	return strings.EqualFold(hgithub.SafeGetLogin(review.GetUser()), c.transport.AppBotLogin())
}

func (c *Engine) invalidateQueuedAutoMerge(ctx context.Context, owner, repo string, number int, label, body string) error {
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return fmt.Errorf("removing %s label: %w", label, err)
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("commenting on stale auto-merge approval: %w", err)
	}
	c.noteOwnStageComment(owner, repo, number)
	return nil
}

func parseHiveQueueReview(body string) string {
	matches := hiveQueueReviewRE.FindStringSubmatch(strings.TrimSpace(body))
	if len(matches) != 2 {
		return ""
	}

	return matches[1]
}

// commitGreen reports whether the head SHA is mergeable from a CI
// standpoint.
//
// Gating is REQUIRED-CHECKS-ONLY: commitGreen first asks which status
// contexts/check-run names are actually required for the target branch
// (requiredStatusCheckContexts). Any status or check-run whose context/name
// is NOT in that required set is skipped entirely, regardless of its state or
// conclusion — pending, failing, or cancelled non-required checks can never
// block self-merge. A required check still fully gates: pending blocks
// (return not-green, "pending" — the sweep must never squash a PR before its
// required CI has finished), and a failure/error/cancelled conclusion on a
// required check blocks too.
//
// Why required-only, not a hardcoded ignore-list: the previous approach
// (isMetaCheck/isIgnorableCICheck as an ALLOWLIST of names to ignore) is
// whack-a-mole against an open-ended set of non-required checks — #3611 added
// Playwright/Mobile Browser Tests/coverage-report/the chromium shard matrix,
// but "Detect untested files" (cancelled) still blocked #22471 and "Analyze
// (python)" (CodeQL, failure) still blocks #22450 because neither name was on
// the list. Every managed repo's ACTUAL required-checks set (e.g. console's
// main branch requires only "build-gate") is the one true source of what
// must be green; anything else is by definition non-required and must never
// wedge the queue.
//
// Where that required set comes from (see requiredStatusCheckContexts for
// the full precedence): branch protection first, then non-admin branch/rules
// fallbacks, then the operator-declared auto_merge.required_checks fallback.
// GitHub's protection APIs require administration:read on App tokens, but the
// branch payload exposes the same required status-check contexts to pushable
// installations.
//
// Unknown-source fallback: if the required-checks set cannot be determined by
// config, branch protection, the branch payload, or repository rulesets, the
// sweep no longer treats arbitrary red check-runs as required. It waits only
// while a check is still pending, then attempts the merge and lets GitHub's
// merge endpoint enforce the actual required checks server-side. A known-empty
// required set is not unknown: nothing is enforced server-side, so completed
// failing check-runs and statuses block locally.
func (c *Engine) commitGreen(ctx context.Context, owner, repo, branch, sha string) (bool, string, error) {
	return c.commitGreenForPR(ctx, owner, repo, branch, sha, 0, time.Time{}, nil)
}

func (c *Engine) commitGreenForPR(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache) (bool, string, error) {
	return c.commitGreenForPRSkipping(ctx, owner, repo, branch, sha, prNumber, headPushedAt, expectedChecks, nil)
}

func (c *Engine) commitGreenForPRSkipping(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache, skippedByDesign map[string]bool) (bool, string, error) {
	// The walk itself lives in hgithub.EvaluateCommitCI so the merge-request
	// watcher's positive-confirmation gate (#6173) evaluates a SHA with the
	// identical rules; only the required-set precedence is engine-specific.
	configRequired, configKnown := c.configRequiredChecksForRepo(owner + "/" + repo)
	required, requiredKnown, fromConfig, fallback, source := hgithub.RequiredStatusCheckContextsDetailedWithSource(ctx, c.gh, owner, repo, branch, configRequired, configKnown)
	actualRequired, actualKnown := required, requiredKnown && !fromConfig
	// A known-empty required set means the branch enforces nothing, so the
	// merge endpoint cannot be the source of truth: keep the positive-evidence
	// requirement but evaluate completed failures locally.
	positiveEvidenceGate := requiredKnown && len(required) == 0
	if positiveEvidenceGate {
		required, requiredKnown = nil, false
	}
	if prNumber > 0 {
		c.info("automerge required checks source", "repo", owner+"/"+repo, "pr", prNumber, "branch", branch, "source", source, "known", requiredKnown, "count", len(required))
		if !requiredKnown {
			c.info("automerge required checks unknown; relying on server-side enforcement", "repo", owner+"/"+repo, "pr", prNumber, "branch", branch, "source", source)
		}
	}
	if fallback && fromConfig {
		c.warnRequiredChecksFallback(owner, repo, branch)
	}
	st, err := hgithub.EvaluateCommitCI(ctx, c.gh, owner, repo, sha, hgithub.CommitCIOptions{
		PRNumber:                            prNumber,
		Required:                            required,
		RequiredKnown:                       requiredKnown,
		RequiredKnownFromConfig:             fromConfig,
		UnknownRequiredChecksServerEnforced: !requiredKnown && !positiveEvidenceGate,
		RequiredKnownEmpty:                  positiveEvidenceGate,
		RequireEvidence:                     !requiredKnown,
		SkippedByDesign:                     skippedByDesign,
		MinHeadAge:                          c.minHeadAge,
		HeadPushedAt:                        headPushedAt,
		Now:                                 c.now,
	})
	if err != nil {
		return false, st.Reason, err
	}
	if len(st.Reporting) > 0 {
		c.info("automerge CI reporting observation", "repo", owner+"/"+repo, "pr", prNumber,
			"head", sha, "observations", st.Reporting)
	}
	if prNumber > 0 && len(st.IgnoredMetaContexts) > 0 {
		c.info("automerge ignoring meta contexts on unprotected base", "pr", prNumber, "contexts", st.IgnoredMetaContexts)
	}
	if prNumber > 0 {
		c.noteStageRedChecks(owner, repo, prNumber, sha, st.FailingNonRequired)
		c.noteStageDCOGate(owner, repo, prNumber, sha, required, st.Observed)
	}
	if configKnown && actualKnown {
		c.warnRequiredChecksMismatch(owner, repo, configRequired, actualRequired, st.Observed)
	}
	if !st.Green && isFreshHeadOrMissingExpectedReason(st.Reason) && prNumber > 0 {
		c.info("automerge CI gate blocked PR", "repo", owner+"/"+repo, "pr", prNumber, "sha", sha, "reason", st.Reason)
	}
	if prNumber > 0 && !isFreshHeadOrMissingExpectedReason(st.Reason) {
		c.rememberEvaluatedHead(owner, repo, prNumber, sha)
	}
	reason := st.Reason
	if !st.Green && prNumber > 0 && classifyCIReason(reason) == ciReasonRed {
		// Red, but every failing check is also red on the base branch: the
		// merge is still blocked (we never merge on red), yet the recorded
		// skip reason names the base, not the head, so the stage engine and
		// the dashboard say whose fault it is (base_branch_red.go).
		if checks := stageBaseBranchRed(c.stagePolicy(), owner, repo, prNumber, sha); len(checks) > 0 {
			reason = baseBranchRedReason(checks)
		}
	}
	return st.Green, reason, nil
}

func isFreshHeadOrMissingExpectedReason(reason string) bool {
	return strings.HasPrefix(reason, "pending: head pushed ") || (strings.HasPrefix(reason, "pending: ") && strings.HasSuffix(reason, " has not started"))
}

func (c *Engine) warnRequiredChecksFallback(owner, repo, branch string) {
	if c == nil {
		return
	}
	key := strings.ToLower(owner + "/" + repo + "#" + branch)
	c.requiredChecksFallbackWarnedMu.Lock()
	if c.requiredChecksFallbackWarned[key] {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredChecksFallbackWarned[key] = true
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn("branch protection required checks unavailable; falling back to configured automerge required checks", "repo", owner+"/"+repo, "branch", branch)
}

func (c *Engine) warnRequiredChecksMismatch(owner, repo string, declared, actual, observed map[string]bool) {
	if c == nil {
		return
	}
	missing := mismatchedDeclaredChecks(declared, actual, observed)
	if len(missing) == 0 {
		return
	}
	key := strings.ToLower("mismatch:" + owner + "/" + repo)
	c.requiredChecksFallbackWarnedMu.Lock()
	if c.requiredChecksFallbackWarned[key] {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredChecksFallbackWarned[key] = true
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn("automerge required_checks override does not match repo protection", "repo", owner+"/"+repo, "declared", sortedSetKeys(declared), "actual", sortedSetKeys(actual), "using", "actual", "mismatch", missing)
}

func mismatchedDeclaredChecks(declared, actual, observed map[string]bool) []string {
	out := make([]string, 0)
	for name := range declared {
		if !actual[name] && !observed[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// requiredStatusCheckContexts returns the set of status-check contexts /
// check-run names that are actually required for branch, and whether that
// set could be determined at all. It is the single source of truth
// commitGreen gates on: membership in this set is what makes a check
// "required" (must be green) versus ignorable (any state/conclusion, never
// blocks).
//
// Precedence (first available source wins):
//  1. GitHub's branch-protection API (Repositories.GetRequiredStatusChecks).
//     Kept as a fallback in case the App ever does have admin-read scope, or
//     the branch is legitimately unprotected (gh.ErrBranchNotProtected — a
//     repo with zero required checks is a valid, common state, NOT an
//     error, so that case returns requiredKnown=true with an empty set).
//  2. Config: c.configRequiredChecks(), i.e. the operator-declared
//     auto_merge.required_checks list (config.AutoMergeConfig.RequiredCheckSet).
//  3. Neither available (no config list AND branch empty / API call failed
//     for a reason other than "not protected") → requiredKnown=false. The
//     caller must fall back to the OLD isMetaCheck/isIgnorableCICheck
//     allowlist rather than treating "we don't know the required set" as
//     "nothing is required" — see commitGreen's fail-closed comment.
func (c *Engine) requiredStatusCheckContexts(ctx context.Context, owner, repo, branch string) (map[string]bool, bool) {
	set, ok := c.configRequiredChecksForRepo(owner + "/" + repo)
	return hgithub.RequiredStatusCheckContexts(ctx, c.gh, owner, repo, branch, set, ok)
}

func labelNames(labels []*gh.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		if name := l.GetName(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return true
		}
	}
	return false
}

func isGitHubStatus(err error, status int) bool {
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == status
}

func requiredChecksExpectedMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	msg := ghErr.Message
	if msg == "" {
		msg = ghErr.Error()
	}
	return requiredChecksExpectedRE.MatchString(msg)
}

// mergeQueueRequiredMergeError reports the 405 a base branch whose ruleset
// requires a merge queue returns for a direct merge ("Repository rule
// violations found ... Changes must be made through the merge queue"). The
// REST merge can never succeed there, so the sweeps record a skip reason
// instead of a merge-failed error every pass (hivecommons/hive#14018).
func mergeQueueRequiredMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	return mergeQueueRequiredRE.MatchString(ghErr.Message) || mergeQueueRequiredRE.MatchString(ghErr.Error())
}

func mergeQueueBaseKey(owner, repo, base string) string {
	return strings.ToLower(owner+"/"+repo) + "#" + base
}

// noteMergeQueueBase records that base refused a direct merge as
// merge-queue-only.
func (c *Engine) noteMergeQueueBase(owner, repo, base string) {
	if c == nil || strings.TrimSpace(base) == "" {
		return
	}
	c.mergeQueueBasesMu.Lock()
	defer c.mergeQueueBasesMu.Unlock()
	if c.mergeQueueBases == nil {
		c.mergeQueueBases = make(map[string]time.Time)
	}
	c.mergeQueueBases[mergeQueueBaseKey(owner, repo, base)] = c.clock()
}

// mergeQueueBaseKnown reports whether base refused a direct merge as
// merge-queue-only within mergeQueueBaseRecheck.
func (c *Engine) mergeQueueBaseKnown(owner, repo, base string) bool {
	if c == nil || strings.TrimSpace(base) == "" {
		return false
	}
	key := mergeQueueBaseKey(owner, repo, base)
	c.mergeQueueBasesMu.Lock()
	defer c.mergeQueueBasesMu.Unlock()
	seen, ok := c.mergeQueueBases[key]
	if !ok {
		return false
	}
	if c.clock().Sub(seen) >= mergeQueueBaseRecheck {
		delete(c.mergeQueueBases, key)
		return false
	}
	return true
}

func mergeAPIErrorNotApplied(err error) error {
	if err == nil {
		return nil
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return err
	}
	switch ghErr.Response.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusForbidden:
		return effects.NotApplied(err)
	}
	return err
}

// executeMergeEffect runs the merge call through the mutation boundary and
// brackets it with a merge attempt (hivecommons/hive#15245) so the tracker
// shows "Merging" for the call's duration and a refusal's reason after.
func (c *Engine) executeMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int, head string, merge func(context.Context) (effects.Result, error)) (effects.Result, error) {
	slug := owner + "/" + repo
	hgithub.BeginMergeAttemptOnHead(slug, number, hgithub.PRAuditPathSweep, head)
	out, err := c.executeMergeEffectOnce(ctx, claim, owner, repo, number, merge)
	switch {
	case err != nil:
		hgithub.EndMergeAttempt(slug, number, false, err)
	case out.Provenance == "":
		hgithub.EndMergeAttempt(slug, number, false, errors.New("merge-not-applied"))
	default:
		hgithub.EndMergeAttempt(slug, number, true, nil)
	}
	return out, err
}

func (c *Engine) executeMergeEffectOnce(ctx context.Context, claim effects.Claim, owner, repo string, number int, merge func(context.Context) (effects.Result, error)) (effects.Result, error) {
	out, err := effects.Execute(ctx, c.mutation, claim, merge)
	if !errors.Is(err, effects.ErrNeedsReconciliation) {
		return out, err
	}
	reconciled, recErr := c.reconcileMergeEffect(ctx, claim, owner, repo, number)
	if recErr != nil {
		return out, fmt.Errorf("%w (reconciliation lookup failed: %v)", err, recErr)
	}
	c.info("reconciled unresolved automerge effect", "repo", owner+"/"+repo, "pr", number, "applied", reconciled.Applied)
	return effects.Execute(ctx, c.mutation, claim, merge)
}

func (c *Engine) reconcileOpenPRMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int) {
	if _, ok := c.mutation.(effects.Reconciler); !ok {
		return
	}
	if err := effects.Reconcile(ctx, c.mutation, claim, effects.ExternalState{Applied: false}); err == nil {
		c.info("reconciled open PR automerge effect as not applied", "repo", owner+"/"+repo, "pr", number)
	}
}

func (c *Engine) reconcileMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int) (effects.ExternalState, error) {
	if c == nil || c.gh == nil {
		return effects.ExternalState{}, hgithub.ErrNoGitHubClient
	}
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_reconcile_merge"), owner, repo, number)
	if err != nil {
		return effects.ExternalState{}, err
	}
	state := effects.ExternalState{Applied: pr.GetMerged(), Provenance: pr.GetMergeCommitSHA()}
	if !pr.GetMerged() {
		state.Provenance = ""
	}
	if err := effects.Reconcile(ctx, c.mutation, claim, state); err != nil {
		return state, err
	}
	return state, nil
}

func (c *Engine) warn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

// warnSelfAuthoredSkipOnce logs the skip at WARN once per repo#pr+reason+error
// and at Debug on later ticks, so a PR stuck on the same condition does not
// repeat the line every sweep.
func (c *Engine) warnSelfAuthoredSkipOnce(repo string, number int, reason string, err error) {
	if c == nil {
		return
	}
	key := fmt.Sprintf("%s#%d|%s|%v", repo, number, reason, err)
	c.selfSkipWarnedMu.Lock()
	if c.selfSkipWarned == nil || len(c.selfSkipWarned) > 4096 {
		c.selfSkipWarned = map[string]struct{}{}
	}
	_, seen := c.selfSkipWarned[key]
	c.selfSkipWarned[key] = struct{}{}
	c.selfSkipWarnedMu.Unlock()
	if seen {
		c.debug("self-authored automerge sweep skipped PR", "repo", repo, "pr", number, "reason", reason, "error", err)
		return
	}
	c.warn("self-authored automerge sweep skipped PR", "repo", repo, "pr", number, "reason", reason, "error", err)
}

func (c *Engine) info(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}

func (c *Engine) debug(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Debug(msg, args...)
	}
}

// mergeTrainPolicyOption folds the legacy MergeTrainEnabled switch into
// the policy func when no full policy is wired.
func mergeTrainPolicyOption(opts Options) func() MergeTrainPolicy {
	if opts.MergeTrain != nil || opts.MergeTrainEnabled == nil {
		return opts.MergeTrain
	}
	return func() MergeTrainPolicy {
		p := DefaultMergeTrainPolicy
		p.Enabled = opts.MergeTrainEnabled()
		return p
	}
}
