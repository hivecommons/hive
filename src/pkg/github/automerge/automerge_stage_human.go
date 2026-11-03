package automerge

// Human-stage routing (hivecommons/hive#12528). When a lane's opinion is
// "a human must act" the engine gathers what the lane did not evaluate —
// the reviewer verdict for the CURRENT head, a digest of the head's checks
// and DCO context, the sentinel / hold labels, the conflict reason and the
// contributor lane's soak — and stagelabels.RouteHuman turns the hand-off
// into 072 (author must fix), 050 (no verdict yet), 085 (needs a decision),
// 060 (gates still running) or 090 (ready for a human merge). Evidence is
// read only for human-routed PRs and cached per head.

import (
	"context"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/oktotest"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// humanChecksTTL bounds how often one head's checks are re-read. A new head
// is a cache miss, so a push is re-evaluated on the next pass.
const humanChecksTTL = 10 * time.Minute

// defaultSentinelLabel is the sentinel alert label when none is configured.
const defaultSentinelLabel = "sentinel-alert"

// humanChecksEntry is one cached head digest.
type humanChecksEntry struct {
	head    string
	at      time.Time
	summary hgithub.CommitChecksSummary
	err     bool
}

// CheckSummarizer reads the check digest for a head; tests swap it.
type CheckSummarizer func(ctx context.Context, owner, repo, sha string) (hgithub.CommitChecksSummary, error)

func (c *Engine) summarizeChecks(ctx context.Context, owner, repo, sha string) (hgithub.CommitChecksSummary, error) {
	if c.checkSummary != nil {
		return c.checkSummary(ctx, owner, repo, sha)
	}
	return hgithub.SummarizeCommitChecks(ctx, c.gh, owner, repo, sha)
}

// headChecks returns the (possibly cached) check digest for obs's head.
func (c *Engine) headChecks(ctx context.Context, tracker *StageTracker, key string, obs stageObservation, now time.Time) (hgithub.CommitChecksSummary, bool) {
	tracker.mu.Lock()
	entry, ok := tracker.checks[key]
	tracker.mu.Unlock()
	if ok && entry.head == obs.HeadSHA && now.Sub(entry.at) < humanChecksTTL {
		return entry.summary, !entry.err
	}
	summary, err := c.summarizeChecks(ctx, obs.Owner, obs.Repo, obs.HeadSHA)
	if err != nil {
		c.warn("stage labels: check digest failed", "repo", obs.Display, "pr", obs.Number, "error", err)
	}
	tracker.mu.Lock()
	tracker.checks[key] = humanChecksEntry{head: obs.HeadSHA, at: now, summary: summary, err: err != nil}
	tracker.mu.Unlock()
	return summary, err == nil
}

// humanFenceEntry is one head's contributor-lane diff fence ("" when the
// diff is clean).
type humanFenceEntry struct {
	head  string
	fence string
}

// headDiffFence returns the contributor lane's diff fence for obs's head
// (contributorLaneDiffReason: workflow / action files, tests removed), cached
// per head: a head's diff does not change, and a push is a new head. ok is
// false when it could not be read (transport error, incomplete file list, or
// the PR's head moved past obs); nothing is cached then, so the next pass
// tries again.
func (c *Engine) headDiffFence(ctx context.Context, tracker *StageTracker, key string, obs stageObservation) (string, bool) {
	tracker.mu.Lock()
	entry, ok := tracker.fences[key]
	tracker.mu.Unlock()
	if ok && entry.head == obs.HeadSHA {
		return entry.fence, true
	}
	fence, err := c.adoptionDiffRefusal(ctx, obs)
	if err != nil {
		c.warn("stage labels: diff fence read failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return "", false
	}
	if fence == contributorLaneReasonFilesEvidence || fence == adoptionReasonHeadChanged {
		return "", false
	}
	tracker.mu.Lock()
	if tracker.fences == nil {
		tracker.fences = map[string]humanFenceEntry{}
	}
	tracker.fences[key] = humanFenceEntry{head: obs.HeadSHA, fence: fence}
	tracker.mu.Unlock()
	return fence, true
}

// humanVerdict maps the reviewer verdict for the head to a VerdictState.
func humanVerdict(v ContributorLaneVerdict) stagelabels.VerdictState {
	v = normalizeContributorLaneVerdict(v)
	switch {
	case !v.Configured:
		return stagelabels.VerdictUnavailable
	case v.Approved:
		return stagelabels.VerdictApproved
	case verdictBlocks(stageDetail{Verdict: v, VerdictKnown: true}):
		return stagelabels.VerdictChangesRequested
	case verdictNeedsHuman(stageDetail{Verdict: v, VerdictKnown: true}):
		return stagelabels.VerdictRequiresHuman
	}
	return stagelabels.VerdictPending
}

// manualMergeReason reports whether a lane reason leaves no unattended way
// to merge: CI workflow / action files edited, tests removed, a human-merge
// path touched, or GitHub refusing the merge method the hive must use. An
// approval or `/hive approve` never overrides these, so the 085 comment says
// a person must merge (hivecommons/hive#13087).
func manualMergeReason(reason string) bool {
	switch reason {
	case contributorLaneReasonWorkflowPath, contributorLaneReasonTestsRemoved, autoMergeReasonHumanMergePath, autoMergeReasonMergeRefusedMethod:
		return true
	}
	return false
}

// humanConflictReasons are lane reasons that mean the head does not merge
// cleanly.
var humanConflictReasons = map[string]bool{"conflicting": true, "not-mergeable": true, "label:needs-rebase": true}

// humanConflict reports a head that does not merge cleanly. The stage reason
// is what classifyStage chose; for a hive or clanker PR a decision-requested
// verdict outranks the conflict (hivecommons/hive#14607). A person's own PR
// is different: the conflict is theirs to fix, so the lane's own reason
// (kept in LaneReason) still counts (hivecommons/hive#15202).
func humanConflict(obs stageObservation) bool {
	return humanConflictReasons[obs.Reason] || hasLabelFold(obs.Labels, "needs-rebase") ||
		obs.AuthorKind == authorkind.Human && humanConflictReasons[obs.LaneReason]
}

// supersededCheckTTL bounds how often the hive-authored superseded check
// re-reads a PR that stays parked on a person at the same head.
const supersededCheckTTL = humanChecksTTL

// supersededCheckWarranted says whether the route the pass is about to
// take is one a merged closer would answer: the reviewer swarm's own
// requires_human (not an owner's decision question, which stays theirs),
// or a conflicting head / needs-rebase label. Other hand-offs (trust,
// policy, a person's hold) are not the rule's to pre-empt.
func supersededCheckWarranted(verdict stageVerdict, obs stageObservation) bool {
	if humanConflictReasons[obs.LaneReason] || humanConflictReasons[verdict.Reason] {
		return true
	}
	if !verdict.Human || !obs.Detail.VerdictKnown {
		return false
	}
	v := obs.Detail.Verdict
	if v.DecisionRequested {
		// A decision question that names the PR already carrying this
		// change ("close as a no-op?") is the superseded question itself
		// (hivecommons/hive#15499) — by marker or in words
		// (hivecommons/hive#15647); any other decision stays a person's.
		_, claimed := alreadyLandedVerdict(v)
		return claimed
	}
	return strings.EqualFold(strings.TrimSpace(v.Verdict), "requires_human")
}

// closeSupersededBeforeHumanRoute runs the hive-authored superseded rule
// (hgithub.CloseSupersededHivePRIfEligible) on a hive-authored PR about to
// be routed to a person — a reviewer's requires_human, a conflicting head,
// a needs-rebase label. The merge that closed its issue (or declared it
// superseded) is the answer the person would give, so the PR is closed
// with the `superseded-by-merge` reason instead of parked at 085
// (hivecommons/hive#15208). It reports whether the PR was closed.
func (c *Engine) closeSupersededBeforeHumanRoute(ctx context.Context, tracker *StageTracker, key string, obs stageObservation, pass *stageengine.PRPassFacts, now time.Time) bool {
	hiveAuthored := (pass != nil && pass.AuthorIsApp) || obs.AuthorKind == authorkind.Hive
	// A person's hold is that person's decision; the rule keeps held PRs,
	// so it is not asked.
	if c.gh == nil || obs.Hold != nil {
		return false
	}
	// Another author's PR is asked only the already-landed question, and
	// only when the reviewer raised it (hivecommons/hive#15732): the
	// closer then proves the landing before closing. Trust changes nothing.
	landedClaimed := obs.Detail.VerdictKnown && alreadyLandedClaimed(obs.Detail.Verdict)
	if !hiveAuthored && !landedClaimed {
		return false
	}
	closer, ok := c.transport.(supersededCloserTransport)
	if !ok && hiveAuthored {
		return false
	}
	cacheKey := key + "@" + obs.HeadSHA
	tracker.mu.Lock()
	last, seen := tracker.superseded[cacheKey]
	tracker.mu.Unlock()
	if seen && now.Sub(last) < supersededCheckTTL {
		return false
	}
	tracker.mu.Lock()
	tracker.superseded[cacheKey] = now
	tracker.mu.Unlock()
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		c.warn("stage labels: superseded check could not read the PR", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false
	}
	if !hiveAuthored {
		return c.closeEmptyDiffBeforeHumanRoute(ctx, obs, pr, false, now)
	}
	res, err := closer.CloseSupersededHivePRIfEligible(ctx, obs.Owner+"/"+obs.Repo, pr)
	if err != nil {
		c.warn("stage labels: superseded check failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false
	}
	if !res.Closed {
		if c.closeEmptyDiffBeforeHumanRoute(ctx, obs, pr, true, now) {
			return true
		}
		c.debug("stage labels: hive PR kept", "repo", obs.Display, "pr", obs.Number, "verdict", res.Verdict, "reason", res.Reason)
		return false
	}
	c.info("stage labels: hive PR closed as superseded instead of a human decision",
		"repo", obs.Display, "pr", obs.Number, "reason", stagelabels.LaneReasonSupersededByMerge, "action", res.Reason,
		"closer", res.CloserPR, "issue", res.Issue, "lane_reason", obs.LaneReason)
	return true
}

// alreadyLandedVerdict reads a reviewer verdict for "this change is already
// on the base": landed is the PR it named (the `hive-redundant-of` marker,
// else "already landed via #N" in words), claimed is true when it says so
// at all, number or not (hivecommons/hive#15647).
func alreadyLandedVerdict(v ContributorLaneVerdict) (landed int, claimed bool) {
	return review.AlreadyLandedFromVerdict(v.RedundantOf, v.DecisionQuestion, v.HumanSummaries)
}

// alreadyLandedClaimed is alreadyLandedVerdict's claimed alone.
func alreadyLandedClaimed(v ContributorLaneVerdict) bool {
	_, claimed := alreadyLandedVerdict(v)
	return claimed
}

// alreadyLandedCloserTransport is the optional transport hook that closes
// a hive-authored PR on the reviewer's already-landed verdict
// (hgithub.Client.CloseAlreadyLandedHivePRIfEligible; hivecommons/hive#15647).
type alreadyLandedCloserTransport interface {
	CloseAlreadyLandedHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest, redundantOf int) (hgithub.HiveEmptyDiffClose, error)
}

// closeEmptyDiffBeforeHumanRoute runs the hive-authored empty-diff rule
// (hgithub.CloseEmptyDiffHivePRIfEligible) on a hive-authored PR about to
// be routed to a person: a head whose compare against the base lists no
// files has nothing to merge, so the hive closes it as superseded (by the
// PR the reviewer's `hive-redundant-of` marker named, when it did) instead
// of asking the owner to say so on every head (hivecommons/hive#15499). The
// diff is measured by the closer; the reviewer's text only names the
// superseding PR.
//
// When the verdict for this head says the change is already landed, the
// already-landed entry is taken instead (hivecommons/hive#15647): no
// freshness guard, files identical on the base tip count as no diff, and
// the close settles the adoption's lineage — the original the copy
// superseded is closed as superseded by the same landed PR, so the orphan
// recovery does not re-adopt a head the base already has. That entry is
// open to any author (hivecommons/hive#15732); the closer proves the
// landing. hiveAuthored is false for another author's PR, which the plain
// empty-diff rule never touches.
func (c *Engine) closeEmptyDiffBeforeHumanRoute(ctx context.Context, obs stageObservation, pr *gh.PullRequest, hiveAuthored bool, now time.Time) bool {
	redundantOf, claimed := 0, false
	if obs.Detail.VerdictKnown {
		redundantOf, claimed = alreadyLandedVerdict(obs.Detail.Verdict)
	}
	if claimed {
		if landedCloser, ok := c.transport.(alreadyLandedCloserTransport); ok {
			return c.closeAlreadyLandedBeforeHumanRoute(ctx, obs, pr, landedCloser, redundantOf, now)
		}
	}
	if !hiveAuthored {
		// The sweep's empty-diff rule is the hive's own; another author's
		// PR closes only on the already-landed entry above.
		return false
	}
	closer, ok := c.transport.(emptyDiffCloserTransport)
	if !ok {
		return false
	}
	res, err := closer.CloseEmptyDiffHivePRIfEligible(ctx, obs.Owner+"/"+obs.Repo, pr, redundantOf)
	if err != nil {
		c.warn("stage labels: empty-diff check failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false
	}
	if !res.Closed {
		if res.Empty {
			c.info("stage labels: empty-diff PR kept", "repo", obs.Display, "pr", obs.Number, "verdict", res.Verdict, "reason", res.Reason)
		}
		return false
	}
	c.info("stage labels: hive PR with an empty diff closed as superseded instead of a human decision",
		"repo", obs.Display, "pr", obs.Number, "reason", stagelabels.LaneReasonSupersededByMerge, "action", res.Reason,
		"redundant_of", res.RedundantOf, "lane_reason", obs.LaneReason)
	return true
}

// closeAlreadyLandedBeforeHumanRoute is the already-landed entry of the
// empty-diff rule (see closeEmptyDiffBeforeHumanRoute). It reports whether
// the PR was closed; a PR the closer kept (a real diff, a guard) falls
// through to the ordinary route.
func (c *Engine) closeAlreadyLandedBeforeHumanRoute(ctx context.Context, obs stageObservation, pr *gh.PullRequest, closer alreadyLandedCloserTransport, redundantOf int, now time.Time) bool {
	res, err := closer.CloseAlreadyLandedHivePRIfEligible(ctx, obs.Owner+"/"+obs.Repo, pr, redundantOf)
	if err != nil {
		c.warn("stage labels: already-landed check failed", "repo", obs.Display, "pr", obs.Number, "redundant_of", redundantOf, "error", err)
		return false
	}
	if !res.Closed {
		c.info("stage labels: already-landed verdict but the PR was kept", "repo", obs.Display, "pr", obs.Number, "redundant_of", redundantOf, "empty", res.Empty, "verdict", res.Verdict, "reason", res.Reason)
		return false
	}
	caption, _ := stagelabels.AlreadyLandedCaption(stagelabels.AlreadyLandedReason(res.RedundantOf))
	c.info("stage labels: PR closed as already landed instead of a human decision",
		"repo", obs.Display, "pr", obs.Number, "reason", stagelabels.AlreadyLandedAuthorReason(obs.AuthorKind), "action", res.Reason,
		"redundant_of", res.RedundantOf, "issues_closed", res.IssuesClosed, "supersedes", res.Supersedes, "caption", caption, "lane_reason", obs.LaneReason)
	c.settleAlreadyLandedLineage(ctx, obs, pr.GetNumber(), res.RedundantOf, res.Supersedes, now)
	return true
}

// settleAlreadyLandedLineage closes the originals an already-landed copy
// declared it supersedes (hivecommons/hive#15647): the adoption promised
// "this PR closes when that copy merges"; the copy did not merge because
// the base already had the change, which is the same outcome for the
// original. Each original whose record names this copy as its adoption is
// told, marked superseded on the store (so settleSuperseder does not read
// the closed copy as an orphan and re-adopt), and closed. A refused close
// (403/404 on a fork PR) leaves the note and the mark, as the merged-copy
// path does. The original's record is required: without it the hive does
// not know which head it adopted, and a PR that moved on since is not
// closed on the copy's word.
func (c *Engine) settleAlreadyLandedLineage(ctx context.Context, obs stageObservation, copy, landed int, originals []int, now time.Time) {
	if c.gh == nil || len(originals) == 0 {
		return
	}
	store := c.stageStoreOrShared()
	for _, number := range originals {
		if number <= 0 || number == copy {
			continue
		}
		key := stageKey(obs.Owner, obs.Repo, number)
		rec, ok := store.Get(key)
		if !ok || rec.AdoptedBy != copy {
			c.info("stage labels: already-landed copy names an original the store does not tie to it; left alone", "repo", obs.Display, "pr", number, "copy", copy, "recorded_adopted_by", rec.AdoptedBy)
			continue
		}
		if rec.Superseded() {
			continue
		}
		orig, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, number)
		if err != nil {
			c.warn("stage labels: already-landed lineage could not read the original", "repo", obs.Display, "pr", number, "copy", copy, "error", err)
			continue
		}
		if orig.GetMerged() || strings.EqualFold(orig.GetState(), "closed") {
			store.MarkSuperseded(key, copy, false, now)
			continue
		}
		if head := strings.TrimSpace(orig.GetHead().GetSHA()); head != "" && rec.AdoptedHead != "" && !strings.EqualFold(head, rec.AdoptedHead) {
			c.warn("stage labels: already-landed lineage: the original moved on since the adoption; left open", "repo", obs.Display, "pr", number, "copy", copy, "adopted_head", rec.AdoptedHead, "head", head)
			continue
		}
		body := alreadyLandedOriginalNote(copy, landed, hgithub.SafeGetLogin(orig.GetUser()), nonEmpty(orig.GetBase().GetRef(), obs.BaseRef))
		item := writeback.Item{Number: number, IsPR: true, Author: hgithub.SafeGetLogin(orig.GetUser()), Labels: labelNames(orig.Labels), CreatedAt: orig.GetCreatedAt().Time, SupersededBy: copy}
		if d := c.writebackAllowed(obs.Display, item, writeback.KindSupersessionClose, body); !d.Allowed {
			c.info("stage labels: already-landed lineage close gated", "repo", obs.Display, "pr", number, "copy", copy, "reason", d.Reason)
			continue
		}
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
			c.warn("stage labels: already-landed lineage note failed", "repo", obs.Display, "pr", number, "copy", copy, "error", err)
			continue
		}
		// The note is out: whatever the close below does, never post it
		// again, and never read the closed copy as an orphan.
		store.MarkSuperseded(key, copy, true, now)
		if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
			c.warn("stage labels: already-landed lineage: closing the original failed; left open with the note", "repo", obs.Display, "pr", number, "copy", copy, "error", err)
			continue
		}
		store.MarkSuperseded(key, copy, false, now)
		c.info("stage labels: adopted original closed; its copy was already landed", "repo", obs.Display, "pr", number, "copy", copy, "landed", landed, "reason", stagelabels.LaneReasonAlreadyLanded)
	}
}

// alreadyLandedOriginalNote is the close note on an adopted original whose
// hive copy was closed as already landed.
func alreadyLandedOriginalNote(copy, landed int, author, base string) string {
	var b strings.Builder
	b.WriteString(adoptionCloseMarker)
	if landed > 0 {
		b.WriteString("\n" + writeback.RedundantOfMarker + " #" + strconv.Itoa(landed) + " -->")
	}
	b.WriteString("\nClosing: #" + strconv.Itoa(copy) + " (the hive's adoption of this PR, with @" + author + "'s commits) was closed because the change is already on `" + base + "`")
	if landed > 0 {
		b.WriteString(" via #" + strconv.Itoa(landed))
	}
	b.WriteString(", so there is nothing left to merge. Thank you — reopen if that's wrong.")
	return b.String()
}

// humanEvidence gathers the Evidence for one human-routed observation.
func (c *Engine) humanEvidence(ctx context.Context, tracker *StageTracker, key string, obs stageObservation, cause stagelabels.HumanCause, now time.Time) stagelabels.Evidence {
	contributorVerdict := normalizeContributorLaneVerdict(c.contributorLaneVerdictFor(obs.Owner+"/"+obs.Repo, obs.Number, obs.HeadSHA))
	workflowManual := c.workflowFenceManual(obs.Owner, obs.Repo, obs.Number, obs.HeadSHA)
	e := stagelabels.Evidence{
		Head:             obs.HeadSHA,
		Verdict:          humanVerdict(contributorVerdict),
		Conflict:         humanConflict(obs),
		Holds:            stagelabels.Holds(obs.Labels),
		Hold:             obs.Hold,
		Cause:            cause,
		CauseReason:      decisionCauseFor(obs.Reason, workflowManual),
		DecisionQuestion: strings.TrimSpace(contributorVerdict.DecisionQuestion),
		DecisionFinding:  strings.TrimSpace(contributorVerdict.DecisionFinding),
		HumanSummaries:   append([]string(nil), contributorVerdict.HumanSummaries...),
		SelfAuthored:     obs.Lane == stageLaneSelf,
		AuthorKind:       obs.AuthorKind,
		ManualMerge:      manualMergeReason(obs.Reason),
		LaneReason:       obs.Reason,
		ManualMergeRule:  manualMergeRuleReasonFor(obs.Reason, workflowManual),
		// The 085 actions ask for the approval that releases the fence
		// instead of saying none helps (hivecommons/hive#14664).
		ApprovalReleasesFence: obs.Reason == contributorLaneReasonWorkflowPath && !workflowManual,
	}
	// A trusted merger's APPROVED review on exactly this head is the human
	// decision the lane noted (latestTrustedHumanApproval): it settles a
	// requires_human verdict and a manual-merge lane rule, so the route
	// must not keep asking for a decision already made (hivecommons/hive#14568).
	if approver := strings.TrimSpace(obs.Detail.HumanApprover); approver != "" && obs.HeadSHA != "" && strings.TrimSpace(obs.Detail.HumanApprovalHead) == obs.HeadSHA {
		e.HumanApprover = approver
	}
	// A trusted maintainer's CHANGES_REQUESTED review (or recorded
	// direction) on exactly this head is the human answer every park on it
	// was waiting for: the route is the author's fixing stage, not 085
	// (hivecommons/hive#15159).
	if requester := strings.TrimSpace(obs.Detail.HumanChangeRequester); requester != "" && obs.HeadSHA != "" && strings.TrimSpace(obs.Detail.HumanChangeRequestHead) == obs.HeadSHA {
		e.HumanChangeRequester = requester
	}
	if obs.Reason == autoMergeReasonMergeRefusedReview {
		// GitHub wants an approving review, and someone's change request
		// still stands: the ask is that reviewer's re-review, not an
		// approval over it (projectbluefin/utah-packages#633).
		if cause := mergeRefusedReviewChangeRequestCause(obs.HeadSHA, obs.Detail.StandingChangeRequests); cause != "" {
			e.CauseReason = cause
		}
	}
	if rec, ok := c.stageStoreOrShared().Get(key); ok && rec.AdoptedHead != "" && rec.AdoptDiffFence != "" && rec.AdoptDiffFence == obs.Reason && e.CauseReason != "" {
		// An adopted head whose diff fence is the blocker: the 085 note
		// says the hive repaired it and why it still will not merge it.
		e.CauseReason = "the hive adopted this PR to repair CI, but " + e.CauseReason
	}
	if !e.ManualMerge && obs.Lane == stageLaneContributor && obs.HeadSHA != "" {
		// The lane stops at the first thing that blocks a PR, and a
		// sentinel alert, a hold label or a requires_human verdict is
		// reached before the diff fence. A PR that edits workflow files
		// was then asked for an approval that can never lead to a merge,
		// and told it is manual-merge only once the first block cleared.
		// Name the fence from the start.
		// The fence's standing is this head's, as for a lane reason: an
		// owner's approval may release a workflow-path fence.
		if fence, ok := c.headDiffFence(ctx, tracker, key, obs); ok && manualMergeReason(fence) {
			e.ManualMerge = true
			e.ApprovalReleasesFence = fence == contributorLaneReasonWorkflowPath && !workflowManual
			if e.CauseReason == "" {
				e.CauseReason = decisionCauseFor(fence, workflowManual)
			}
		}
	}
	if strings.HasPrefix(obs.Reason, stageReasonForkGuard) {
		e.ForkGuard = append([]string(nil), obs.ForkGuardChecks...)
	}
	sentinel := c.sentinelLabelName()
	if sentinel == "" {
		sentinel = defaultSentinelLabel
	}
	e.Sentinel = hasLabelFold(obs.Labels, sentinel)
	summary, known := c.headChecks(ctx, tracker, key, obs, now)
	e.Checks = stagelabels.CheckSummary{Total: summary.Total, Green: summary.Green, Pending: summary.Pending, Red: summary.Red, Known: known}
	switch summary.DCO {
	case 1:
		e.DCO = stagelabels.DCOYes
	case -1:
		e.DCO = stagelabels.DCONo
	}
	// Nothing has reported on the head yet: it was just pushed (a repo
	// with no CI at all would not pass the lane's evidence gate either).
	e.HeadFresh = known && summary.Total == 0
	if soak := c.currentContributorLanePolicy().Soak; soak > 0 {
		e.Soak = soak
		if known && summary.Total > 0 && summary.Pending == 0 && len(summary.Red) == 0 {
			first := c.contributorSoak.observeGreen(contributorSoakKey(obs.Owner, obs.Repo, obs.Number, obs.HeadSHA), now)
			e.SoakElapsed = now.Sub(first)
		}
	}
	return e
}

// manualMergeRuleReasonFor reports whether a lane reason is a rule no
// review or reply overrides: the hive never merges the head unattended, a
// person clicks merge. workflowManual supplies the workflow-path fence's
// standing: true when the hive will not merge the head itself even once
// approved (toggle off, or the App lacks `workflows: write` for this head);
// false when owner approval releases it (hivecommons/hive#14664).
func manualMergeRuleReasonFor(reason string, workflowManual bool) bool {
	switch reason {
	case contributorLaneReasonWorkflowPath:
		return workflowManual
	case contributorLaneReasonTestsRemoved, autoMergeReasonHumanMergePath, autoMergeReasonHumanMergeRequired:
		return true
	}
	return false
}

// workflowFenceManual reports whether a workflow-path PR at this head is
// one a person must merge: the owner-approval release is off, or the lane
// has learnt the App cannot merge the head (unauthorized merge-workflow-path).
func (c *Engine) workflowFenceManual(owner, repo string, number int, headSHA string) bool {
	if c == nil {
		return true
	}
	if !c.currentContributorLanePolicy().OwnerApprovalReleasesWorkflowFence {
		return true
	}
	return c.unauthorizedStep(owner, repo, number, headSHA) == unauthorizedStepMergeWorkflowPath
}

// routeHuman refines a lane's human hand-off into the stage the evidence
// supports. It returns the route (with the lane-vocabulary reason to
// record) and the evidence it was decided on, which the stage pass hands
// the engine as PRPassFacts.Route so DerivePR routes identically.
func (c *Engine) routeHuman(ctx context.Context, tracker *StageTracker, key string, obs stageObservation, stage stagelabels.Stage, now time.Time) (stagelabels.Route, stagelabels.Evidence) {
	cause := stagelabels.CauseTrust
	if stage == stagelabels.NeedsDecision {
		cause = stagelabels.CauseDecision
	}
	e := c.humanEvidence(ctx, tracker, key, obs, cause, now)
	return stagelabels.RouteHuman(e), e
}

// trustCause renders the trust reason for the 090 comment.
func trustCause(reason string, role stagelabels.Role) string {
	tier := "`" + role.Label() + "`"
	if role == stagelabels.RoleNone {
		tier = "the author's trust tier"
	}
	switch reason {
	case contributorLaneReasonClaimTier:
		return tier + " (below require_tiers)"
	case contributorLaneReasonNoClaim, contributorLaneReasonNoLinkedIssue, contributorLaneReasonClaimUnverified,
		contributorLaneReasonClaimAuthor, contributorLaneReasonClaimOtherHive, contributorLaneReasonAuthorAssociation:
		return tier + " (no verified hive claim or registry tier)"
	case "untrusted-author-role", "author-permission-missing", "fork-non-member":
		return tier + " (no trusted-author role)"
	}
	return tier
}

// decisionCause renders a non-trust human-only lane reason as prose for the
// 085 comment ("" for reasons the labels already explain). The workflow-path
// fence reads as a manual-merge rule; decisionCauseFor refines it.
func decisionCause(reason string) string {
	return decisionCauseFor(reason, true)
}

// workflowPathCause is the 085 prose for the workflow-path fence: a rule a
// person merges around when workflowManual, otherwise a fence awaiting the
// owner's explicit approval (hivecommons/hive#14664).
func workflowPathCause(workflowManual bool) string {
	if workflowManual {
		return prstage.CauseWorkflowPathManual
	}
	return prstage.CauseWorkflowPathAwaitingApproval
}

// decisionCauseFor is decisionCause with the workflow-path fence's standing
// supplied (see workflowFenceManual).
func decisionCauseFor(reason string, workflowManual bool) string {
	switch {
	case reason == "held", reason == "label:hold", reason == "label:needs-human", reason == "exempt-label",
		strings.HasPrefix(reason, "excluded-label:"), reason == "sentinel", reason == contributorLaneReasonSentinelBlocked:
		return ""
	case reason == contributorLaneReasonWorkflowPath:
		return workflowPathCause(workflowManual)
	case reason == contributorLaneReasonTestsRemoved:
		return prstage.CauseTestsRemoved
	case strings.HasPrefix(reason, stageReasonForkGuard):
		// RouteHuman names the fork-guard checks from the evidence.
		return ""
	case reason == contributorLaneReasonNoReviewer, reason == stageReasonVerdictRequiresHuman, reason == stageReasonDecisionRequested:
		// RouteHuman renders the verdict itself from the evidence.
		return ""
	case reason == contributorLaneReasonReporterTrustHold:
		return prstage.CauseReporterTrustHold
	case reason == autoMergeReasonHumanMergePath, reason == autoMergeReasonHumanMergePathEvidence:
		return prstage.CauseHumanMergePath
	case reason == autoMergeReasonHumanMergeRequired:
		return prstage.CauseHumanMergeRequired
	case reason == autoMergeReasonIntentUnauthorized, reason == autoMergeReasonIntentEvidence:
		return prstage.CauseIntentUnauthorized
	case reason == autoMergeReasonWorkflowPermission:
		return "GitHub refused to bring the branch up to date: the base changed `.github/workflows/` files and the token the hive used lacks the Workflows permission, so no hive agent can push that update. A person must merge or rebase the base into the branch and push, or grant the App Workflows: write if the installation lacks it"
	case reason == autoMergeReasonForkWorkflowUpdate:
		return "GitHub refused to bring the branch up to date: the head branch is in a fork and the base changed `.github/workflows/` files, which the Hive App cannot push to a fork. The author, or a maintainer with push access when \"Allow edits by maintainers\" is on, must run `gh pr update-branch` on this PR or merge the base into the branch and push"
	case strings.HasPrefix(reason, workflowApprovalUntrustedPrefix):
		return prstage.CauseWorkflowApprovalUntrusted()
	case strings.HasPrefix(reason, workflowApprovalWorkflowMissingPrefix):
		return prstage.CauseWorkflowApprovalWorkflowMissing()
	case strings.HasPrefix(reason, unauthorizedReasonPrefix):
		return "the hive's GitHub App is not permitted to " + strings.ReplaceAll(strings.TrimPrefix(reason, unauthorizedReasonPrefix), "-", " ") + " (GitHub answered 403 Resource not accessible by integration); the `" + unauthorizedLabel + "` note above names the permission and the step a maintainer must take"
	case strings.HasPrefix(reason, workflowApprovalRequiredPrefix):
		return prstage.CauseWorkflowApprovalRequired()
	case reason == "approval-desk-withheld":
		return prstage.CauseApprovalDeskWithheld
	case strings.HasPrefix(reason, "linked-issue:"):
		if kind, issue, label, ok := prstage.ParseLinkedIssueReason(reason); ok {
			ref := "#" + strconv.Itoa(issue)
			if kind == linkedIssueReasonHoldPrefix {
				return "issue " + ref + ", which this PR closes, is on hold (`" + label + "`); remove `" + label + "` from " + ref + " to release this PR"
			}
			return "issue " + ref + ", which this PR closes, is parked for a decision (`" + label + "`); once it is decided, remove `" + label + "` from " + ref + " to release this PR"
		}
	case reason == autoMergeReasonMergeRefusedReview:
		return "GitHub refused the merge: branch protection requires an approving review from someone with write access, and the Hive App cannot give it. A GitHub **Approve** review on this head (or an App bypass in the branch rules) lets the hive merge it; the hive retries once GitHub reports the PR unblocked, or within 30 minutes"
	case reason == autoMergeReasonMergeRefusedMethod:
		return "GitHub refused the merge: the repository does not allow the merge method the hive chose for this PR. The hive does not switch methods on its own: a forward merge or a `" + stagelabels.MergeCommitLabel + "` PR must never be squashed. Merge it by hand with an allowed method, or allow the method in the repository settings and the hive retries within 30 minutes"
	case reason == "":
		return ""
	}
	return prstage.CauseLaneReported(reason)
}

// mergeRefusedReviewChangeRequestCause is the 085 prose for GitHub's
// "approving review required" merge refusal when change requests still
// stand on the PR ("" when none does, and the plain approval ask applies).
// It names each reviewer, the commit they reviewed and what the hive has
// pushed since, and asks them to re-review or a maintainer to dismiss the
// review. On projectbluefin/utah-packages#633 the hive asked three times
// for an Approve review on a head it had reworked for @hanthor's change
// request, without ever naming it.
func mergeRefusedReviewChangeRequestCause(head string, requests []prstage.ChangeRequestRef) string {
	if len(requests) == 0 {
		return ""
	}
	head = strings.TrimSpace(head)
	var said, names []string
	for _, r := range requests {
		login := strings.TrimSpace(r.Login)
		if login == "" {
			continue
		}
		names = append(names, "@"+login)
		var b strings.Builder
		b.WriteString("@" + login + " requested changes")
		if !r.SubmittedAt.IsZero() {
			b.WriteString(" on " + r.SubmittedAt.UTC().Format("2006-01-02"))
		}
		commit := strings.TrimSpace(r.CommitID)
		switch {
		case commit != "" && commit == head:
			b.WriteString(" on this head")
		case commit != "":
			b.WriteString(" at `" + stagelabels.ShortSHA(commit) + "`")
		}
		if url := strings.TrimSpace(r.URL); url != "" {
			b.WriteString(" ([review](" + url + "))")
		}
		if commit != "" && head != "" && commit != head {
			b.WriteString(", and the hive has pushed since")
			if i := strings.Index(r.URL, "/pull/"); i > 0 {
				b.WriteString(" ([changes since that review](" + r.URL[:i] + "/compare/" + commit + "..." + head + "))")
			}
		}
		said = append(said, b.String())
	}
	if len(said) == 0 {
		return ""
	}
	return "GitHub refused the merge: branch protection requires an approving review from someone with write access, and the Hive App cannot give it. " +
		"A change request still stands on this PR: " + strings.Join(said, "; ") + ". " +
		"GitHub keeps a change request standing across pushes until its reviewer re-reviews or it is dismissed, so settle it before anyone approves: " +
		joinNames(names) + ", please re-review this head; if the request no longer applies, a maintainer can dismiss the review. " +
		"The hive retries once GitHub reports the PR unblocked, or within 30 minutes"
}

// joinNames renders "a", "a and b", "a, b and c".
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// postHumanEntry writes the 085 / 090 entry comment once per stage visit,
// and again when a parked 085 PR's decision cause changes on the same head
// to one this visit has not explained yet: the maintainer who cleared the
// sentinel alert the comment named must be told the workflow-path fence is
// what now holds the PR, not left reading a stale cause. Each distinct
// cause is posted once per head. Returns the comments posted.
func (c *Engine) postHumanEntry(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, role stagelabels.Role, route stagelabels.Route) (int, error) {
	rec, ok := store.Get(key)
	if !ok || rec.Stage != route.Stage {
		return 0, nil
	}
	cause := stagelabels.EntryCause(route.Reasons)
	decisionKey := ""
	if route.Stage == stagelabels.NeedsDecision && obs.Detail.VerdictKnown && obs.Detail.Verdict.DecisionRequested {
		decisionKey = writeback.DecisionKey(obs.Detail.Verdict.RedundantOf, obs.Detail.Verdict.DecisionQuestion)
	}
	if rec.EntryCommentRepeatsDecision(decisionKey) {
		// Same question, new head (hivecommons/hive#15499): the person
		// has it already; the tracker shows the current wording.
		store.MarkEntryCommentedCause(key, obs.HeadSHA, cause)
		return 0, nil
	}
	if rec.EntryCommented {
		if route.Stage != stagelabels.NeedsDecision || obs.HeadSHA == "" || cause == "" || rec.EntryCommentedHead != obs.HeadSHA {
			return 0, nil
		}
		if rec.EntryCommentExplains(obs.HeadSHA, cause) {
			// Also seeds the cause on records from before causes were kept.
			store.MarkEntryCommentedCause(key, obs.HeadSHA, cause)
			return 0, nil
		}
	} else if obs.HeadSHA != "" && rec.EntryCommentExplains(obs.HeadSHA, cause) {
		// Same stage, head and cause already explained on an earlier visit.
		store.MarkEntryCommentedCause(key, obs.HeadSHA, cause)
		return 0, nil
	}
	var body string
	switch route.Stage {
	case stagelabels.ReadyForHumanMerge:
		body = stagelabels.ReadyComment(route, trustCause(obs.Reason, role))
	case stagelabels.NeedsDecision:
		body = stagelabels.DecisionComment(route) + c.workflowApprovalSnippet(obs.Reason)
	default:
		return 0, nil
	}
	if obs.Reason == autoMergeReasonHumanMergeRequired {
		// The "waiting for a person" note on a breaking-titled or
		// owner-flagged PR is an unsolicited write (hivecommons/hive#15442):
		// it goes through the write-back gate like every other hive
		// comment, and a refusal leaves the record unmarked so the next
		// pass asks again.
		if d := c.writebackAllowed(obs.Display, writebackItemForObservation(obs), writeback.KindNeedsDecisionComment, body); !d.Allowed {
			c.info("stage labels: entry comment withheld by the write-back gate", "repo", obs.Display, "pr", obs.Number, "stage", route.Stage.String(), "reason", d.Reason)
			return 0, nil
		}
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: entry comment failed", "repo", obs.Display, "pr", obs.Number, "stage", route.Stage.String(), "error", err)
		return 0, err
	}
	store.MarkEntryCommentedCause(key, obs.HeadSHA, cause)
	store.MarkEntryCommentedDecisionKey(key, decisionKey)
	c.info("stage labels: entry comment posted", "repo", obs.Display, "pr", obs.Number, "stage", route.Stage.String(), "reasons", route.Reasons)
	return 1, nil
}

// workflowApprovalSnippet appends the self-contained ok-to-test workflow,
// rendered for this hive's own App login, to the 085 entry when held runs
// stayed `action_required` past the grace period (the repository most likely
// lacks the workflow). It contains no `uses:` reference so it installs under
// restrictive Actions policies and on GHES.
func (c *Engine) workflowApprovalSnippet(reason string) string {
	if !strings.HasPrefix(reason, workflowApprovalWorkflowMissingPrefix) {
		return ""
	}
	login := ""
	if c != nil && c.transport != nil {
		login = strings.TrimSpace(c.transport.AppBotLogin())
	}
	workflow := oktotest.Workflow(oktotest.Options{AppLogins: login})
	var b strings.Builder
	b.WriteString("\n\n<details><summary>Install <code>")
	b.WriteString(oktotest.WorkflowPath)
	b.WriteString("</code> (self-contained; commit it to the default branch)</summary>\n\n```yaml\n")
	b.WriteString(strings.TrimRight(workflow, "\n"))
	b.WriteString("\n```\n\n</details>")
	return b.String()
}
