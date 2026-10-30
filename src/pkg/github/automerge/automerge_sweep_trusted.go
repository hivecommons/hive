package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func (c *Engine) trySweepSelfAuthoredPR(ctx context.Context, displayRepo, owner, repo string, number int, branchUpdateAllowed bool) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	c.noteStagePR(owner, repo, number, pr)
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	lane, laneOK := c.sweepLaneForAuthor(author)
	if !laneOK {
		// Neither the App's own PR nor a trusted bot's: this path never touches
		// other PRs, matching the human-queue sweep's untouched behavior for
		// PRs it does not own. Defense in depth — prefilterSelfAuthoredPR
		// already filtered on author, but a PR can change hands (rare, but
		// GitHub permits transferring PR authorship attribution in some flows)
		// between listing and evaluating it here.
		return AutoMergeSweepEvent{}, "not-app-authored", nil
	}
	selfLabels := labelNames(pr.Labels)
	// The refetched PR is the one that merges: a title or label edited
	// since listing re-applies the human-merge rule (hivecommons/hive#15442).
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), selfLabels); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	blocked := c.labelBlockReason(selfLabels, true)
	// A conflicting head — GitHub's mergeable_state or the needs-rebase
	// label a lane already put on it — is asked the superseded question
	// before the label block returns: a PR whose issue another merged PR
	// closed is closed, not left to wear needs-rebase (hivecommons/hive#15208).
	if lane == "self-authored" && (prMergeableStateConflicting(pr) || blocked == autoMergeReasonLabelNeedsRebase) {
		if closer, ok := c.transport.(supersededCloserTransport); ok {
			res, err := closer.CloseSupersededHivePRIfEligible(ctx, owner+"/"+repo, pr)
			if err != nil {
				c.warn("self-authored sweep: superseded check failed", "repo", displayRepo, "pr", number, "error", err)
			} else if res.Closed {
				c.info("self-authored sweep: conflicting PR closed as superseded", "repo", displayRepo, "pr", number, "closer", res.CloserPR, "issue", res.Issue, "reason", stagelabels.LaneReasonSupersededByMerge, "action", res.Reason)
				return AutoMergeSweepEvent{}, autoMergeReasonClosedSuperseded, nil
			}
		}
	}
	// A head GitHub reports as changing no files has nothing to merge: the
	// change is already on the base. The closer proves it with the compare
	// and closes the hive's own PR as superseded (hivecommons/hive#15499)
	// before a review cycle is spent asking a person to.
	if lane == "self-authored" && hgithub.PRChangedFilesClaimsEmpty(pr) {
		if closer, ok := c.transport.(emptyDiffCloserTransport); ok {
			verdict := c.contributorLaneVerdictFor(owner+"/"+repo, number, pr.GetHead().GetSHA())
			res, err := closer.CloseEmptyDiffHivePRIfEligible(ctx, owner+"/"+repo, pr, verdict.RedundantOf)
			if err != nil {
				c.warn("self-authored sweep: empty-diff check failed", "repo", displayRepo, "pr", number, "error", err)
			} else if res.Closed {
				c.info("self-authored sweep: empty-diff PR closed as superseded", "repo", displayRepo, "pr", number, "redundant_of", res.RedundantOf, "reason", stagelabels.LaneReasonSupersededByMerge, "action", res.Reason)
				return AutoMergeSweepEvent{}, autoMergeReasonClosedEmptyDiff, nil
			}
		}
	}
	// A needs-rebase label left on a head GitHub now reports merely behind
	// is stale: the gate still sees a mergeable PR (hivecommons/hive#15103).
	if blocked != "" && !staleNeedsRebaseLabel(blocked, pr) {
		return AutoMergeSweepEvent{}, blocked, nil
	}
	if c.transport.IsExemptLabels(selfLabels) {
		return AutoMergeSweepEvent{}, "exempt-label", nil
	}

	evaluatedHeadSHA := ""
	if pr.GetHead() != nil {
		evaluatedHeadSHA = pr.GetHead().GetSHA()
	}
	if evaluatedHeadSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
	verdict := c.contributorLaneVerdictFor(owner+"/"+repo, number, evaluatedHeadSHA)
	c.noteStageVerdict(owner, repo, number, verdict)
	priorApprover, _ := c.trustedHumanApprovalForRequiresHuman(ctx, owner, repo, number, evaluatedHeadSHA, verdict)
	priorChecked := verdictNeedsHuman(stageDetail{Verdict: verdict, VerdictKnown: true})
	// A trusted maintainer's changes_requested on this head outranks every
	// swarm verdict and the merge gates below (hivecommons/hive#13041): the
	// PR goes to 070 and the planner kicks the author agent with the review
	// text. Mirrors the trusted-author and contributor lanes, which also
	// refuse to merge over an outstanding changes_requested.
	if c.humanChangesRequestedForHead(ctx, owner, repo, number, evaluatedHeadSHA) {
		return AutoMergeSweepEvent{}, autoMergeReasonHumanChangesRequested, nil
	}
	method, methodReason := c.mergeMethodWithReason(ctx, owner, repo, pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(method, methodReason, evaluatedHeadSHA, lane),
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)

	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	// A native merge queue tests the merged result itself, and updating the
	// branch would push a new head that drops any queue entry — but the
	// current-base gate stays in force unless the operator explicitly set
	// auto_merge.native_merge_queue.skip_current_base_gate.
	queueSkipsBase := sync.OnceValue(func() bool { return c.nativeQueueSkipsCurrentBase(ctx, owner+"/"+repo, pr) })
	if strings.EqualFold(pr.GetMergeableState(), "behind") && !queueSkipsBase() && !c.requireCurrentBaseFor(owner+"/"+repo) {
		if !branchUpdateAllowed {
			// Keep the cause: still behind, retried next tick, not CI
			// state and not author work (hivecommons/hive#12607).
			return AutoMergeSweepEvent{}, autoMergeReasonBehindBudgetExhausted, nil
		}
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, evaluatedHeadSHA, "behind", c.isForkPR(pr))
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return AutoMergeSweepEvent{}, "conflicting", nil
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
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, evaluatedHeadSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, evaluatedHeadSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		if approveReason := c.detectActionRequiredWorkflowRuns(ctx, displayRepo, owner, repo, lane, pr, number, evaluatedHeadSHA, reason); approveReason != "" {
			return AutoMergeSweepEvent{}, approveReason, nil
		}
	}
	if !green {
		// A trusted bot's dependency-bump head never gets the contexts its
		// workflow skips by design (#12004); re-evaluate treating those as
		// satisfied. Every other required check must still be green.
		green, reason, err = c.recheckSkippedByDesign(ctx, displayRepo, owner, repo, baseBranch, pr, number, evaluatedHeadSHA, headPushedAt, reason)
		if err != nil {
			return AutoMergeSweepEvent{}, reason, err
		}
	}
	if !green {
		// A missing required check on a trusted-lane head may be a workflow
		// run GitHub is holding for approval; post /ok-to-test and surface it
		// distinctly rather than polling forever (automerge_held_runs.go).
		return AutoMergeSweepEvent{}, c.resolveMissingRequiredCheck(ctx, displayRepo, owner, repo, pr, number, evaluatedHeadSHA, reason, headPushedAt), nil
	}

	// Intent tier gate (#6258). The human merge lane never lets a PR reach
	// trySweepQueuedPR at a tier intent enforcement refuses (writeMergeEligible
	// drops it first); this path lists the App's PRs independently, so it
	// must ask the same question itself — via intent.EvaluateForAppSelfMerge,
	// the tier gate written for this path — or every green App PR merges
	// regardless of tier. Consulted after commitGreen so the extra files
	// fetch is only spent on PRs that are otherwise mergeable, and before the
	// approval desk so the desk still only sees requests policy permits.
	//
	// Human-merge paths (#11038) come first and do not depend on intent: the
	// App self-merge contract authorizes Tier 2/3 without a person, so a path
	// the operator reserved for a person must be refused before
	// EvaluateForAppSelfMerge is ever asked. A diff fence carried from an
	// adopted contributor PR (automerge_adoption_fence.go) is refused the
	// same way: the App PR carries the fenced diff, so a person merges it.
	if fence := carriedFenceGate(displayRepo, pr, c.info); fence != "" {
		return AutoMergeSweepEvent{}, fence, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	if intentReason, err := c.selfMergeIntentGate(ctx, displayRepo, owner, repo, pr, author, selfLabels); err != nil {
		return AutoMergeSweepEvent{}, intentReason, err
	} else if intentReason != "" {
		return AutoMergeSweepEvent{}, intentReason, nil
	}

	// Current-base gate (hivecommons/hive#13634). Consulted only once the
	// head is green and every policy gate above has passed: a red or pending
	// head is the author's to fix and must not be re-based under them, and a
	// head policy refuses would be updated for nothing. Sits before the desk
	// so the desk is only asked about a head that would merge this tick.
	if c.requireCurrentBaseFor(owner+"/"+repo) && !queueSkipsBase() {
		if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, lane, number, pr, evaluatedHeadSHA, priorApprover, priorChecked); reviewReason != "" {
			return AutoMergeSweepEvent{}, reviewReason, nil
		}
		if ev, reason, stop, err := c.currentBaseGate(ctx, displayRepo, owner, repo, number, baseBranch, evaluatedHeadSHA, branchUpdateAllowed, autoMergeReasonBehindBudgetExhausted, c.isForkPR(pr), stageLaneSelf); stop {
			return ev, reason, err
		}
	}

	// Approval desk (RFC #4000). Consulted AFTER the sweep's own eligibility
	// checks so the desk only ever sees requests the legacy gate already
	// permits — it can withhold a merge, never widen authority beyond what
	// SelfAuthoredAutoMergeAllowed already granted upstream. No-op when no hook
	// is installed, which is the default; see automerge_desk.go.
	if allow, deskReason := c.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{
		Kind:        hgithub.ApprovalDeskKindSelfMerge,
		Repo:        displayRepo,
		Number:      number,
		Author:      author,
		Title:       pr.GetTitle(),
		Labels:      labelNames(pr.Labels),
		ChecksGreen: true, // commitGreen returned green immediately above
		HeadSHA:     evaluatedHeadSHA,
	}); !allow {
		return AutoMergeSweepEvent{}, deskReason, nil
	}

	// Freshness-bounded read-through of the cached PR (hivecommons/hive#13377):
	// refuses on a stale or not-ready snapshot, a closed PR, or a moved head.
	if gateReason, err := c.forgestateMergeGate(ctx, owner, repo, number, evaluatedHeadSHA); gateReason != "" || err != nil {
		return AutoMergeSweepEvent{}, gateReason, err
	}
	// Re-verify the head SHA immediately before merging: a push landing
	// between the green-check above and the merge call below must never be
	// squashed without having gone through commitGreen itself.
	current, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	currentHeadSHA := ""
	if current.GetHead() != nil {
		currentHeadSHA = current.GetHead().GetSHA()
	}
	if currentHeadSHA == "" || currentHeadSHA != evaluatedHeadSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}
	// Review gate (#12592), on the re-verified head immediately before the
	// merge call: the swarm verdict for exactly the SHA about to merge.
	// Shadow (the default) records and merges; enforce holds.
	if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, lane, number, current, evaluatedHeadSHA, priorApprover, priorChecked); reviewReason != "" {
		return AutoMergeSweepEvent{}, reviewReason, nil
	}

	if hold := mergeTrainCandidateHold(ctx); hold != "" {
		return AutoMergeSweepEvent{}, hold, nil
	}

	// Queue-enabled bases use GitHub's native queue after the same gates.
	// Other bases retain the direct merge and remembered refusal handling.
	if reason, err := c.tryNativeQueue(ctx, current, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: baseBranch, number: number, head: evaluatedHeadSHA, state: current.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, current, evaluatedHeadSHA, method)
	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, evaluatedHeadSHA, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, mergeMessage, mergeOpts)
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if c.requiredChecksExpected(attempt, err) {
			if !branchUpdateAllowed {
				return AutoMergeSweepEvent{}, autoMergeReasonChecksExpected, nil
			}
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, evaluatedHeadSHA, "required-checks-expected", c.isForkPR(pr))
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
	// Audit the merge like MergePR does (pullrequest.go): the activity
	// collector counts pr_merged entries from this trail, and the /fleet L6
	// health verdict is judged on them. When the fleet's merges moved to this
	// sweep, the unaudited path made merging hives read as "no merge in Nd"
	// red on /fleet (observed live on kubestellar/console, 2026-08-26).
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: "", // no queuer in the self-authored path — the App merges its own PR
		HeadSHA:  evaluatedHeadSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     lane,
	}
	c.info("self-authored automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "lane", lane, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) releaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error) {
	if c == nil || c.transport == nil {
		return false, "", nil
	}
	transport, ok := c.transport.(levelHoldTransport)
	if !ok {
		return false, "", nil
	}
	return transport.ReleaseLevelHoldIfEligible(ctx, owner, repo, pr)
}

// sweepLaneForAuthor reports which self-authored-sweep lane a PR author falls
// in: "self-authored" for the App's own login, "trusted-bot" for a login in
// the operator's trusted_bot_authors set, or ok=false for anyone else. The
// lane is recorded on the mutation claim and the merge log so audits can tell
// the two apart; both lanes pass through exactly the same eligibility gates.
func (c *Engine) sweepLaneForAuthor(author string) (string, bool) {
	author = strings.TrimSpace(author)
	if author == "" {
		return "", false
	}
	if c != nil && c.transport != nil && strings.EqualFold(author, c.transport.AppBotLogin()) {
		return "self-authored", true
	}
	if c == nil || c.trustedBotAuthors == nil {
		return "", false
	}
	if c.trustedBotAuthors()[strings.ToLower(author)] {
		return "trusted-bot", true
	}
	return "", false
}

// trustedBotLaneSkipReason is the trusted-author tier's skip reason for a PR
// it hands off to the self-authored sweep's "trusted-bot" lane.
const trustedBotLaneSkipReason = prstage.TrustedBotLaneSkipReason

// trustedAuthorReasonAppAuthored is the trusted-author tier's skip reason
// for the App's own PRs, which the self-authored lane owns. It is
// namespaced to this tier so the skip line and the stage observation never
// read as the contributor lane's.
const trustedAuthorReasonAppAuthored = prstage.TrustedAuthorReasonAppAuthored

// isTrustedBotLaneAuthor reports whether author falls in the "trusted-bot"
// lane of sweepLaneForAuthor.
func (c *Engine) isTrustedBotLaneAuthor(author string) bool {
	lane, ok := c.sweepLaneForAuthor(author)
	return ok && lane == "trusted-bot"
}

func trustedRegistryClanker(labels []string) bool {
	return hasLabelFold(labels, "role/trusted") && hasLabelFold(labels, "author/clanker")
}

func (c *Engine) sentinelLabelPresent(labels []string) bool {
	sentinel := c.sentinelLabelName()
	if sentinel == "" {
		sentinel = defaultSentinelLabel
	}
	return hasLabelFold(labels, sentinel)
}

// isHeld is the sweep's single hold predicate. It defers to the transport so
// the configured hold set (generic substrings plus the exact hive-scoped
// `hive-pause/<hive-id>` dashboard hold) gates merges exactly as it gates
// enumeration. A nil engine or transport fails closed to the generic set.
func (c *Engine) isHeld(labels []string) bool {
	if c == nil || c.transport == nil {
		return hgithub.HasHoldLabel(labels)
	}
	return c.transport.IsHeldLabels(labels)
}

func (c *Engine) sentinelLabelName() string {
	if c == nil || c.sentinelLabel == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.sentinelLabel()))
}

func (c *Engine) labelBlockReason(labels []string, trusted bool) string {
	sentinelLabel := c.sentinelLabelName()
	holdLabels := labels
	if trusted && sentinelLabel != "" {
		holdLabels = make([]string, 0, len(labels))
		for _, label := range labels {
			if !strings.EqualFold(strings.TrimSpace(label), sentinelLabel) {
				holdLabels = append(holdLabels, label)
			}
		}
	}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		lower := strings.ToLower(trimmed)
		switch {
		case sentinelLabel != "" && strings.EqualFold(trimmed, sentinelLabel):
			if !trusted {
				return "sentinel"
			}
		case lower == "needs-rebase":
			return autoMergeReasonLabelNeedsRebase
		case lower == "needs-human":
			return "label:needs-human"
		case lower == "do-not-merge" || strings.HasPrefix(lower, "do-not-merge/"):
			return "label:" + lower
		}
	}
	if c.isHeld(holdLabels) {
		return "label:hold"
	}
	return ""
}

func (c *Engine) prefilterSelfAuthoredPR(pr *gh.PullRequest) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	if _, ok := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); !ok {
		return "not-app-authored"
	}
	labels := labelNames(pr.Labels)
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return reason
	}
	if blocked := c.labelBlockReason(labels, true); blocked != "" {
		return blocked
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	return ""
}

func (c *Engine) prefilterQueuedIssue(issue *gh.Issue, label string) string {
	if issue == nil || !issue.IsPullRequest() {
		return "not-pull-request"
	}
	labels := labelNames(issue.Labels)
	if !hasLabel(labels, label) {
		return "label-removed"
	}
	if reason := c.humanMergeRequiredReason(issue.GetTitle(), labels); reason != "" {
		return reason
	}
	if blocked := c.labelBlockReason(labels, false); blocked != "" {
		return blocked
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	return ""
}

func (c *Engine) prefilterTrustedAuthorPR(owner, repo string, pr *gh.PullRequest, policy TrustedAuthorPolicy) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	labels := labelNames(pr.Labels)
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return reason
	}
	if blocked := c.labelBlockReason(labels, true); blocked != "" && !staleNeedsRebaseLabel(blocked, pr) {
		return blocked
	}
	if excluded := policy.excludedLabel(labels); excluded != "" {
		if sentinelLabel := c.sentinelLabelName(); sentinelLabel != "" && strings.EqualFold(excluded, sentinelLabel) {
			excluded = ""
		}
		if excluded != "" {
			return "excluded-label:" + excluded
		}
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	// The prefilter has no caller context; the admission read only reaches
	// GitHub to carry a decision over a base refresh, and the lane repeats
	// the read with its own context before merging.
	if _, ok := c.trustedAuthorOrOperatorDecision(context.Background(), owner, repo, pr, policy); !ok {
		return "untrusted-author-role"
	}
	return ""
}

// trustedAuthorRoleOperatorApprovedPrefix is the Role recorded on a
// TrustedAuthorDecision when an operator's "Approve and let hive merge"
// admitted the head of a PR whose author holds no trusted-author role. The
// merge audit comment and log carry it, so the merge is attributed to the
// person who approved it, never to the author's trust.
const trustedAuthorRoleOperatorApprovedPrefix = "operator-approved:"

// trustedAuthorOrOperatorDecision is trustedAuthorDecision with the one
// exception the maintainer's last word allows: an untrusted author's PR
// whose current head an operator admitted (the dashboard's "Approve and
// let hive merge") is treated as trusted for this head, attributed to the
// operator. Any other untrusted author stays refused (ok=false).
func (c *Engine) trustedAuthorOrOperatorDecision(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy TrustedAuthorPolicy) (TrustedAuthorDecision, bool) {
	author := hgithub.SafeGetLogin(pr.GetUser())
	decision := c.trustedAuthorDecision(author, policy.RequireRole)
	if decision.Allowed {
		return decision, true
	}
	login := c.operatorAdmission(ctx, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA())
	if login == "" {
		return decision, false
	}
	return TrustedAuthorDecision{Allowed: true, Role: trustedAuthorRoleOperatorApprovedPrefix + login}, true
}

// operatorApprovedDecision reports whether a decision came from
// trustedAuthorOrOperatorDecision's operator-admission exception.
func operatorApprovedDecision(d TrustedAuthorDecision) bool {
	return strings.HasPrefix(d.Role, trustedAuthorRoleOperatorApprovedPrefix)
}

func (c *Engine) listQueuedPullRequestIssues(ctx context.Context, owner, repo, label string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing queued PRs for %s/%s: %w", owner, repo, err)
		}
		all = append(all, issues...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (c *Engine) trySweepTrustedAuthorPR(ctx context.Context, displayRepo, owner, repo string, number int, policy TrustedAuthorPolicy) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:trusted_author_automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	c.noteStagePR(owner, repo, number, pr)
	author := hgithub.SafeGetLogin(pr.GetUser())
	if lane, _ := c.sweepLaneForAuthor(author); lane == stageLaneSelf {
		return AutoMergeSweepEvent{}, trustedAuthorReasonAppAuthored, nil
	}
	if reason := c.prefilterTrustedAuthorPR(owner, repo, pr, policy); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	decision, ok := c.trustedAuthorOrOperatorDecision(ctx, owner, repo, pr, policy)
	if !ok {
		return AutoMergeSweepEvent{}, "untrusted-author-role", nil
	}
	if operatorApprovedDecision(decision) {
		c.info("trusted-author lane admitted by operator approval", "repo", displayRepo, "pr", number, "author", author, "admitted_by", strings.TrimPrefix(decision.Role, trustedAuthorRoleOperatorApprovedPrefix), "head", pr.GetHead().GetSHA())
	} else {
		c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
	}
	verdict := c.contributorLaneVerdictFor(owner+"/"+repo, number, pr.GetHead().GetSHA())
	c.noteStageVerdict(owner, repo, number, verdict)
	priorApprover, _ := c.trustedHumanApprovalForRequiresHuman(ctx, owner, repo, number, pr.GetHead().GetSHA(), verdict)
	priorChecked := verdictNeedsHuman(stageDetail{Verdict: verdict, VerdictKnown: true})
	permission := trustedAuthorPermission{Allowed: true, Level: "not-required"}
	if operatorApprovedDecision(decision) {
		permission.Level = "operator-approved"
	}
	if policy.RequireGitHubPermission && !operatorApprovedDecision(decision) {
		var err error
		permission, err = c.authorRepoPermission(ctx, owner, repo, author)
		if err != nil {
			return AutoMergeSweepEvent{}, "author-permission-check", err
		}
		if !permission.Allowed {
			return AutoMergeSweepEvent{}, "author-permission-missing", nil
		}
	}
	if c.isForkPR(pr) && !policy.RequireGitHubPermission && !operatorApprovedDecision(decision) {
		var err error
		permission, err = c.authorRepoPermission(ctx, owner, repo, author)
		if err != nil {
			return AutoMergeSweepEvent{}, "author-permission-check", err
		}
		if !permission.Allowed {
			return AutoMergeSweepEvent{}, "fork-non-member", nil
		}
	}
	if blocked, err := c.hasOutstandingChangesRequested(ctx, owner, repo, number); err != nil {
		return AutoMergeSweepEvent{}, "review-state-check", err
	} else if blocked {
		return AutoMergeSweepEvent{}, "changes-requested", nil
	}
	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	// See trySweepSelfAuthoredPR: the queue bypass is an explicit opt-in.
	queueSkipsBase := sync.OnceValue(func() bool { return c.nativeQueueSkipsCurrentBase(ctx, owner+"/"+repo, pr) })
	if mergeableState == "behind" && !queueSkipsBase() && !c.requireCurrentBaseFor(owner+"/"+repo) {
		headSHA := ""
		if pr.GetHead() != nil {
			headSHA = pr.GetHead().GetSHA()
		}
		if headSHA == "" {
			return AutoMergeSweepEvent{}, "missing-head-sha", nil
		}
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "behind", c.isForkPR(pr))
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return c.mergeForwardOrConflict(ctx, displayRepo, owner, repo, pr, number)
	}
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
		Inputs: mergeClaimInputs(method, methodReason, headSHA, "trusted-author"),
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
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
	if fence := carriedFenceGate(displayRepo, pr, c.info); fence != "" {
		return AutoMergeSweepEvent{}, fence, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	// Current-base gate (hivecommons/hive#13634): only a green head that
	// every gate above lets through is worth an update-branch commit.
	if c.requireCurrentBaseFor(owner+"/"+repo) && !queueSkipsBase() {
		if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, stageLaneTrusted, number, pr, headSHA, priorApprover, priorChecked); reviewReason != "" {
			return AutoMergeSweepEvent{}, reviewReason, nil
		}
		if ev, reason, stop, err := c.currentBaseGate(ctx, displayRepo, owner, repo, number, baseBranch, headSHA, true, "behind", c.isForkPR(pr), stageLaneTrusted); stop {
			return ev, reason, err
		}
	}
	if gateReason, err := c.forgestateMergeGate(ctx, owner, repo, number, headSHA); gateReason != "" || err != nil {
		return AutoMergeSweepEvent{}, gateReason, err
	}
	current, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:trusted_author_automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	currentHeadSHA := ""
	if current.GetHead() != nil {
		currentHeadSHA = current.GetHead().GetSHA()
	}
	if currentHeadSHA == "" || currentHeadSHA != headSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}
	// The swarm reviews human PRs too (hivecommons/hive#12974), so the same
	// review gate the self-authored lane runs sits in front of this merge:
	// a trusted author's role vouches for who they are, not for an
	// unreviewed head.
	if reviewReason := c.mergeReviewGate(ctx, displayRepo, owner, repo, stageLaneTrusted, number, current, headSHA, priorApprover, priorChecked); reviewReason != "" {
		return AutoMergeSweepEvent{}, reviewReason, nil
	}
	if hold := mergeTrainCandidateHold(ctx); hold != "" {
		return AutoMergeSweepEvent{}, hold, nil
	}
	if reason, err := c.tryNativeQueue(ctx, current, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: baseBranch, number: number, head: headSHA, state: current.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, current, headSHA, method)
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
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "required-checks-expected", c.isForkPR(pr))
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
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	if err := c.commentTrustedAuthorMerge(ctx, owner, repo, number, author, decision.Role, permission.Level, headSHA); err != nil {
		c.warn("trusted-author automerge audit comment failed", "repo", displayRepo, "pr", number, "error", err)
	}
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     "trusted-author",
	}
	c.info("trusted-author automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "role", decision.Role, "permission", permission.Level, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) authorRepoPermission(ctx context.Context, owner, repo, author string) (trustedAuthorPermission, error) {
	level, _, err := c.gh.Repositories.GetPermissionLevel(ctx, owner, repo, author)
	if err != nil {
		return trustedAuthorPermission{}, err
	}
	permission := strings.ToLower(strings.TrimSpace(level.GetPermission()))
	roleName := strings.ToLower(strings.TrimSpace(level.GetRoleName()))
	display := roleName
	if display == "" {
		display = permission
	}
	allowed := permission == "admin" || permission == "write" || roleName == "admin" || roleName == "maintain" || roleName == "write"
	return trustedAuthorPermission{Allowed: allowed, Level: display}, nil
}

func (c *Engine) isForkPR(pr *gh.PullRequest) bool {
	if pr == nil || pr.GetHead() == nil || pr.GetBase() == nil {
		return false
	}
	headRepo := ""
	if pr.GetHead().GetRepo() != nil {
		headRepo = pr.GetHead().GetRepo().GetFullName()
	}
	baseRepo := ""
	if pr.GetBase().GetRepo() != nil {
		baseRepo = pr.GetBase().GetRepo().GetFullName()
	}
	return headRepo != "" && baseRepo != "" && !strings.EqualFold(headRepo, baseRepo)
}

func (c *Engine) updateBranchAndSkip(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, reason string, fork bool) (AutoMergeSweepEvent, string, error) {
	if err := c.executeUpdateBranchEffect(ctx, owner, repo, number, headSHA, reason); err != nil {
		if wait := preparationWaitReason(err); wait != "" {
			return AutoMergeSweepEvent{}, wait, nil
		}
		if c.requireCurrentBaseFor(owner+"/"+repo) && hgithub.IsUpdateBranchStaleHead(err) {
			return AutoMergeSweepEvent{}, basePreparationPrefix + "snapshot-changed", nil
		}
		switch classifyUpdateBranchError(err, fork) {
		case updateBranchUpToDate:
			return AutoMergeSweepEvent{}, "check-pending", nil
		case updateBranchForkNotEditable:
			c.info("automerge update-branch skipped: fork head not editable by the app", "repo", displayRepo, "pr", number, "reason", reason)
			return AutoMergeSweepEvent{}, autoMergeReasonForkNotEditable, nil
		case updateBranchConflict:
			return AutoMergeSweepEvent{}, "conflicting", nil
		}
		return AutoMergeSweepEvent{}, "update-branch", err
	}
	c.info("automerge update-branch", "repo", displayRepo, "pr", number, "reason", reason)
	if c.requireCurrentBaseFor(owner + "/" + repo) {
		return AutoMergeSweepEvent{Repo: displayRepo, Number: number, HeadSHA: headSHA, BranchUpdated: true}, basePreparationReady, nil
	}
	return AutoMergeSweepEvent{Repo: displayRepo, Number: number, HeadSHA: headSHA, BranchUpdated: true}, "check-pending", nil
}

const autoMergeReasonForkNotEditable = prstage.AutoMergeReasonForkNotEditable

type updateBranchFailure int

const (
	updateBranchOther updateBranchFailure = iota
	updateBranchConflict
	updateBranchUpToDate
	updateBranchForkNotEditable
)

// classifyUpdateBranchError separates the update-branch failures GitHub
// reports with overlapping statuses: 422 covers conflicts and an already
// current branch, while a 422 naming maintainer edits (or a 403 on a fork PR)
// means the app cannot write to the fork head. A 403 on a same-repo branch is
// a plain permission or rate-limit failure. An unrecognised 422 stays a conflict.
func classifyUpdateBranchError(err error, fork bool) updateBranchFailure {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return updateBranchOther
	}
	msg := strings.ToLower(ghErr.Message)
	switch ghErr.Response.StatusCode {
	case http.StatusForbidden:
		if fork {
			return updateBranchForkNotEditable
		}
		// TODO(hivecommons/hive#14589): a same-repo 403 "Resource not
		// accessible by integration" means the App lacks contents: write
		// and should mark the lane unauthorized (step: update-branch);
		// a rate-limit 403 must not. Wire through markUnauthorized once
		// the two are told apart here (isResourceNotAccessible).
	case http.StatusUnprocessableEntity:
		switch {
		case strings.Contains(msg, "maintainer") || strings.Contains(msg, "permission") || strings.Contains(msg, "not authorized"):
			return updateBranchForkNotEditable
		case strings.Contains(msg, "merge conflict"):
			return updateBranchConflict
		case strings.Contains(msg, "up to date") || strings.Contains(msg, "up-to-date") || strings.Contains(msg, "no new commits"):
			return updateBranchUpToDate
		}
		return updateBranchConflict
	}
	return updateBranchOther
}

func (c *Engine) executeUpdateBranchEffect(ctx context.Context, owner, repo string, number int, headSHA, reason string) error {
	if c.requireCurrentBaseFor(owner + "/" + repo) {
		if err := c.claimBasePreparationUpdate(ctx, owner, repo, number, headSHA); err != nil {
			return err
		}
	}
	inputs := map[string]string{"expected_head_sha": headSHA, "reason": reason}
	observe := func(ctx context.Context) (effects.ExternalState, error) {
		pr, _, getErr := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_reconcile_branch_update"), owner, repo, number)
		if getErr != nil {
			return effects.ExternalState{}, getErr
		}
		return hgithub.BranchUpdateState(pr, headSHA, owner+"/"+repo+"#"+strconv.Itoa(number)), nil
	}
	_, err := effects.ExecuteReconciling(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchUpdate,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: inputs,
	}, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
		var opts *gh.PullRequestBranchUpdateOptions
		if strings.TrimSpace(headSHA) != "" {
			opts = &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: gh.Ptr(headSHA)}
		}
		_, _, apiErr := c.gh.PullRequests.UpdateBranch(ctx, owner, repo, number, opts)
		var accepted *gh.AcceptedError
		if errors.As(apiErr, &accepted) {
			apiErr = nil
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: owner + "/" + repo + "#" + strconv.Itoa(number)}, nil
	})
	var rejected *gh.ErrorResponse
	if c.requireCurrentBaseFor(owner+"/"+repo) && errors.As(err, &rejected) && rejected.Response != nil &&
		(rejected.Response.StatusCode == http.StatusForbidden || rejected.Response.StatusCode == http.StatusUnprocessableEntity) {
		c.forgetRejectedPreparationUpdate(owner, repo, number, headSHA)
	}
	return err
}

func (c *Engine) hasOutstandingChangesRequested(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := map[string]string{}
	var seen []*gh.PullRequestReview
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return false, err
		}
		seen = append(seen, reviews...)
		for _, review := range reviews {
			login := strings.ToLower(strings.TrimSpace(hgithub.SafeGetLogin(review.GetUser())))
			if login == "" {
				continue
			}
			state := strings.ToUpper(strings.TrimSpace(review.GetState()))
			if state == "APPROVED" || state == "CHANGES_REQUESTED" || state == "DISMISSED" {
				latest[login] = state
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.noteStageReviews(owner, repo, number, seen)
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return true, nil
		}
	}
	return false, nil
}

func (c *Engine) commentTrustedAuthorMerge(ctx context.Context, owner, repo string, number int, author, role, permission, sha string) error {
	body := fmt.Sprintf("merged by Hive trusted-author auto-merge: author @%s holds role %s and GitHub permission %s on repo; CI green at %s.", author, role, permission, sha)
	if login := strings.TrimPrefix(role, trustedAuthorRoleOperatorApprovedPrefix); login != role {
		body = fmt.Sprintf("merged by Hive on behalf of @%s, who approved this head (%s) with \"Approve and let hive merge\": author @%s holds no trusted-author role; CI green at %s.", login, stagelabels.OperatorApprovalAdmission(login), author, sha)
	}
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}
