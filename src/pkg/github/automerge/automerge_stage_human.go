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
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/oktotest"
	"github.com/hivecommons/hive/pkg/stagelabels"
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

// humanConflictReasons are lane reasons that mean the head does not merge
// cleanly.
var humanConflictReasons = map[string]bool{"conflicting": true, "not-mergeable": true, "label:needs-rebase": true}

// humanEvidence gathers the Evidence for one human-routed observation.
func (c *Engine) humanEvidence(ctx context.Context, tracker *StageTracker, key string, obs stageObservation, cause stagelabels.HumanCause, now time.Time) stagelabels.Evidence {
	contributorVerdict := normalizeContributorLaneVerdict(c.contributorLaneVerdictFor(obs.Owner+"/"+obs.Repo, obs.Number, obs.HeadSHA))
	e := stagelabels.Evidence{
		Head:             obs.HeadSHA,
		Verdict:          humanVerdict(contributorVerdict),
		Conflict:         humanConflictReasons[obs.Reason] || hasLabelFold(obs.Labels, "needs-rebase"),
		Holds:            stagelabels.Holds(obs.Labels),
		Cause:            cause,
		CauseReason:      decisionCause(obs.Reason),
		DecisionQuestion: strings.TrimSpace(contributorVerdict.DecisionQuestion),
		HumanSummaries:   append([]string(nil), contributorVerdict.HumanSummaries...),
		SelfAuthored:     obs.Lane == stageLaneSelf,
		AuthorKind:       obs.AuthorKind,
	}
	if rec, ok := c.stageStoreOrShared().Get(key); ok && rec.AdoptedHead != "" && rec.AdoptDiffFence != "" && rec.AdoptDiffFence == obs.Reason && e.CauseReason != "" {
		// An adopted head whose diff fence is the blocker: the 085 note
		// says the hive repaired it and why it still will not merge it.
		e.CauseReason = "the hive adopted this PR to repair CI, but " + e.CauseReason
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
	route := stagelabels.RouteHuman(e)
	if route.Stage == stagelabels.NeedsDecision {
		route.Actions, route.ManualMerge = decisionActions(obs, e)
	}
	return route, e
}

// decisionActions names the operations that actually clear the observed
// cause (hivecommons/hive#13087). A review approval never overrides a
// manual-only path rule or label, and on this release line the lanes do not
// consume `/hive approve` on a PR: the release lever for `requires_human` is
// a trusted merger's GitHub Approve review on the current head.
func decisionActions(obs stageObservation, e stagelabels.Evidence) ([]string, bool) {
	switch obs.Reason {
	case contributorLaneReasonWorkflowPath, contributorLaneReasonTestsRemoved, autoMergeReasonHumanMergePath:
		return []string{"A maintainer must review and merge this PR manually; a GitHub Approve review or `/hive approve` reply cannot override this lane's manual-merge rule."}, true
	}
	var actions []string
	if e.Verdict == stagelabels.VerdictRequiresHuman {
		action := "After reading the verdict, a trusted merger can submit a GitHub **Approve** review for the current head and the lane clears the hold on its next pass; `/hive approve` replies on a PR are acknowledged but not consumed."
		if obs.Lane == stageLaneContributor {
			action += " An ordinary contributor cannot release their own PR."
		}
		actions = append(actions, action)
	}
	if e.Verdict == stagelabels.VerdictUnavailable {
		actions = append(actions, "Configure a reviewer and wait for its verdict, or have a maintainer review and merge manually; `/hive approve` cannot supply a missing swarm verdict.")
	}
	if e.Sentinel {
		actions = append(actions, "Resolve the sentinel alert and have a maintainer clear its label.")
	}
	for _, label := range e.Holds {
		actions = append(actions, "Resolve the reason for `"+label+"`, then have a maintainer remove that label; `/hive approve` does not remove it.")
	}
	if len(actions) == 0 {
		actions = append(actions, "A maintainer must resolve the lane condition named above or review and merge manually; `/hive approve` does not override that condition.")
	}
	return actions, false
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
// 085 comment ("" for reasons the labels already explain).
func decisionCause(reason string) string {
	switch {
	case reason == "held", reason == "label:hold", reason == "label:needs-human", reason == "exempt-label",
		strings.HasPrefix(reason, "excluded-label:"), reason == "sentinel", reason == contributorLaneReasonSentinelBlocked:
		return ""
	case reason == contributorLaneReasonWorkflowPath:
		return "the PR edits CI workflow / action files, which the lane never merges unattended"
	case reason == contributorLaneReasonTestsRemoved:
		return "the PR removes tests, which the lane never merges unattended"
	case reason == contributorLaneReasonNoReviewer, reason == stageReasonVerdictRequiresHuman, reason == stageReasonDecisionRequested:
		// RouteHuman renders the verdict itself from the evidence.
		return ""
	case reason == contributorLaneReasonReporterTrustHold:
		return "the reporter-trust hold is in place"
	case reason == autoMergeReasonHumanMergePath, reason == autoMergeReasonHumanMergePathEvidence:
		return "the PR touches a human-merge path (auto_merge.human_merge_paths)"
	case reason == autoMergeReasonIntentUnauthorized, reason == autoMergeReasonIntentEvidence:
		return "the merge intent could not be authorized"
	case strings.HasPrefix(reason, workflowApprovalWorkflowMissingPrefix):
		return "GitHub Actions is still waiting after Hive posted `" + workflowApprovalCommand + "`. Install the self-contained `" + oktotest.WorkflowPath + "` below (or render it with `hivectl ok-to-test-template`) so the repository GITHUB_TOKEN can approve trusted same-repo bot runs, or manually approve the runs from the PR checks banner."
	case strings.HasPrefix(reason, workflowApprovalRequiredPrefix):
		return "GitHub Actions is waiting for workflow approval. Hive posted `" + workflowApprovalCommand + "`; install `" + oktotest.WorkflowPath + "` (`hivectl ok-to-test-template`) if the runs do not start, or manually approve them from the PR checks banner."
	case reason == "approval-desk-withheld":
		return "the approval desk withheld this merge"
	case reason == "":
		return ""
	}
	return "the lane reported `" + reason + "`"
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
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: entry comment failed", "repo", obs.Display, "pr", obs.Number, "stage", route.Stage.String(), "error", err)
		return 0, err
	}
	store.MarkEntryCommentedCause(key, obs.HeadSHA, cause)
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
