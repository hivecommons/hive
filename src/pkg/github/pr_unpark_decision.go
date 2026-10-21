package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
)

// tryUnparkPullRequest answers the newest eligible `/hive …` command on a PR
// parked at stage/085-needs-decision (hivecommons/hive#13041). A PR is not
// an issue: there is no park label to clear and no "What to reply" notice,
// because GitHub already gives a maintainer the two levers — an Approve
// review merges the head, a changes-requested review sends it back. What
// the sweep adds is the text path:
//
//   - `/hive decision <text>` on a hive-authored PR is a changes_requested
//     review in comment form: the decision is recorded against the head and
//     the review planner kicks the author agent with <text> verbatim;
//   - `/hive decision` on a PR the hive did not open gets one reply naming
//     the review-side equivalents, since the hive does not push to it;
//   - `/hive approve` on a hive-authored PR also lifts a `needs-human` the
//     hive itself added and applies the reviewer hand-off transition
//     (`reviewer-passed` on, `reviewer-handoff` off), mirroring the issue
//     rule of hivecommons/hive#13075; a person's `needs-human` stays
//     (hivecommons/hive#13309);
//   - `/hive approve` and `/hive help` otherwise get a pointer.
//
// Eligibility is eligibleUnparkCommand's (human, repo write+, recent,
// unedited) plus the automerge lane's merger trust, so a text decision is
// never weaker than the review it stands in for. Prose gets no reply.
func (c *Client) tryUnparkPullRequest(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, opts IssueUnparkSweepOptions, result *IssueUnparkSweepResult) (bool, error) {
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	answered := c.answeredUnparkCommentIDs(comments)
	candidate, command, err := c.eligibleUnparkCommand(ctx, owner, repo, comments, answered, cutoff, false)
	if err != nil {
		return false, err
	}
	if candidate == nil || command.Kind == unparkCommandNone {
		return false, nil
	}
	actor := safeGetLogin(candidate.GetUser())
	if opts.TrustedMerger == nil || !opts.TrustedMerger(actor) {
		return false, nil
	}

	pr, resp, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("fetching pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	if strings.EqualFold(pr.GetState(), "closed") {
		return false, nil
	}
	head := ""
	if pr.GetHead() != nil {
		head = strings.TrimSpace(pr.GetHead().GetSHA())
	}
	selfAuthored := c.isHiveAccount(pr.GetUser()) || labelPresent(issueLabelNames(pr.Labels), authorkind.Hive.Label())

	reply := ""
	switch {
	case command.Kind == unparkCommandDecision && selfAuthored && head != "":
		req := review.HumanChangeRequest{
			Repo:     owner + "/" + repo,
			Number:   number,
			HeadSHA:  head,
			Source:   review.HumanChangeRequestSourceDecision,
			SourceID: candidate.GetID(),
			Actor:    actor,
			Body:     command.Decision,
			URL:      candidate.GetHTMLURL(),
		}
		if opts.HumanChangeRequests == nil {
			return false, fmt.Errorf("no human change request store; `/hive decision` on %s/%s#%d by @%s cannot be routed", owner, repo, number, actor)
		}
		// Record before replying: a reply without a record would tell the
		// maintainer the fix is coming when nothing is queued.
		if err := opts.HumanChangeRequests.Record(req); err != nil {
			return false, fmt.Errorf("recording `/hive decision` on %s/%s#%d: %w", owner, repo, number, err)
		}
		event := PullRequestDecisionEvent{Repo: displayRepo, Number: number, HeadSHA: head, Actor: actor, Decision: command.Decision, CommentURL: candidate.GetHTMLURL()}
		result.PRDecisions = append(result.PRDecisions, event)
		if opts.PRAudit != nil {
			opts.PRAudit(event)
		}
		reply = renderPRDecisionRecordedReply(actor, head)
	case command.Kind == unparkCommandDecision && selfAuthored:
		reply = renderPRDecisionNoHeadReply()
	case command.Kind == unparkCommandDecision:
		reply = renderPRDecisionNotHiveAuthoredReply(head)
	case command.Kind == unparkCommandApprove && selfAuthored:
		release, err := c.releaseHiveNeedsHumanOnPR(ctx, owner, repo, number, issueLabelNames(pr.Labels))
		if err != nil {
			return false, err
		}
		reply = renderPRApproveReply(head, release)
	case command.Kind == unparkCommandApprove:
		reply = renderPRApproveReply(head, prNeedsHumanRelease{present: c.hasNeedsHumanLabel(issueLabelNames(pr.Labels)), notHiveAuthored: true})
	default:
		reply = renderPRHelpReply(selfAuthored, head)
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), reply); err != nil {
		return false, err
	}
	if command.Kind != unparkCommandDecision || !selfAuthored || head == "" {
		result.Replies++
	}
	return true, nil
}

func prHeadRef(head string) string {
	head = strings.TrimSpace(head)
	if head == "" {
		return "the current head"
	}
	if len(head) > 7 {
		head = head[:7]
	}
	return "head `" + head + "`"
}

func renderPRDecisionRecordedReply(actor, head string) string {
	return fmt.Sprintf("Decision recorded by @%s for %s. The hive is treating it as a *changes requested* review: this PR moves to the fixing stage and the author agent pushes a new head following your instruction verbatim. The review swarm then re-reviews that head; if it approves, the PR comes back for your merge approval.",
		actor, prHeadRef(head))
}

func renderPRDecisionNoHeadReply() string {
	return "I could not read this PR's head commit, so the decision was not routed. Leave a *changes requested* review with the same text and the hive will pick it up on its next pass."
}

func renderPRDecisionNotHiveAuthoredReply(head string) string {
	return fmt.Sprintf("`/hive decision` routes a fix to the hive's own author agent, and the hive did not open this PR, so it will not push to it. The PR-side equivalents are reviews: **Approve** %s to let it merge as-is, or leave a **changes requested** review and the author is nudged with your text. See <%s>.",
		prHeadRef(head), maintainerCommandsDocURL)
}

// prReviewerHandoffLabel mirrors scheduler.ReviewerHandoffLabel (that
// package imports this one): the adjudicator's HAND-OFF verdict label, which
// a maintainer's `/hive approve` answers.
const prReviewerHandoffLabel = "reviewer-handoff"

// prNeedsHumanRelease is what a trusted `/hive approve` did about
// `needs-human` on a pull request (hivecommons/hive#13309).
type prNeedsHumanRelease struct {
	present         bool
	notHiveAuthored bool
	outcome         unparkNeedsHumanOutcome
	added, removed  []string
}

// releaseHiveNeedsHumanOnPR lifts a `needs-human` the hive itself put on a
// hive-authored PR when a trusted merger answers it with `/hive approve`, and
// applies the transition the adjudicator's hand-off prescribes:
// `reviewer-passed` on, `reviewer-handoff` off. It is the PR side of the
// issue rule (stageengine.DecisionLiftsNeedsHuman, hivecommons/hive#13075):
// a person's `needs-human`, or one whose applier the timeline cannot name,
// stays and no label is touched (hivecommons/hive#12680). `hold`-class
// labels, CI, soak and verdict gates are not touched either. reviewer-passed
// is added and reviewer-handoff removed before needs-human goes, so a failed
// write leaves needs-human in place and the next pass retries the whole
// transition.
func (c *Client) releaseHiveNeedsHumanOnPR(ctx context.Context, owner, repo string, number int, labels []string) (prNeedsHumanRelease, error) {
	var out prNeedsHumanRelease
	var needsHuman []string
	for _, label := range labels {
		if c.hasNeedsHumanLabel([]string{label}) {
			needsHuman = append(needsHuman, label)
		}
	}
	if len(needsHuman) == 0 {
		return out, nil
	}
	out.present = true
	out.outcome = c.needsHumanOutcome(ctx, owner, repo, number, nil)
	if !out.outcome.lift {
		return out, nil
	}
	if !labelPresent(labels, escalation.ReviewerPassedLabel) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{escalation.ReviewerPassedLabel}); err != nil {
			return out, fmt.Errorf("adding %s to %s/%s#%d: %w", escalation.ReviewerPassedLabel, owner, repo, number, err)
		}
		c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
			WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", escalation.ReviewerPassedLabel)
		out.added = append(out.added, escalation.ReviewerPassedLabel)
	}
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), prReviewerHandoffLabel) {
			needsHuman = append([]string{label}, needsHuman...)
		}
	}
	for _, label := range needsHuman {
		if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
			return out, err
		}
		out.removed = append(out.removed, label)
	}
	return out, nil
}

func renderPRApproveReply(head string, release prNeedsHumanRelease) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Noted. On a pull request a trusted merger's `/hive approve` reply posted after the swarm's `requires_human` verdict releases that hold on %s on the hive's next pass, exactly as a GitHub **Approve** review on that head does (the owner's own PR included, where GitHub refuses the self-review). It does not bypass CI, soak, `hold`-class labels, a `changes_requested` / `reject` verdict or a lane that only merges by hand: if this PR is parked for one of those, the `%s` comment above names what clears it.",
		prHeadRef(head), PRNeedsDecisionStageLabel)
	switch {
	case !release.present:
		fmt.Fprintf(&b, " No `%s` label blocks this PR.", issueNeedsHumanLabel)
	case release.notHiveAuthored:
		fmt.Fprintf(&b, " `%s` still blocks this PR: `/hive approve` lifts it only on a PR the hive opened, so a person must remove it.", issueNeedsHumanLabel)
	case release.outcome.lift:
		fmt.Fprintf(&b, " `%s` cleared: Hive added it and your approval is the person's answer it asked for", issueNeedsHumanLabel)
		if len(release.added) > 0 {
			fmt.Fprintf(&b, "; %s applied", quoteLabelsProse(release.added))
		}
		if len(release.removed) > 0 {
			fmt.Fprintf(&b, "; %s removed", quoteLabelsProse(release.removed))
		}
		b.WriteString(". No `needs-human` label still blocks this PR; the lane re-evaluates it on its next pass and it still waits for green CI and soak.")
	case release.outcome.applier == stageengine.NeedsHumanApplierPerson:
		fmt.Fprintf(&b, " `%s` still blocks this PR (added by @%s): a command never lifts a person's hold, so the PR stays parked until a person removes that label.", issueNeedsHumanLabel, release.outcome.actor)
	default:
		fmt.Fprintf(&b, " `%s` still blocks this PR (Hive could not read who added it): it stays parked until a person removes that label.", issueNeedsHumanLabel)
	}
	fmt.Fprintf(&b, " See <%s>.", maintainerCommandsDocURL)
	return b.String()
}

func renderPRHelpReply(selfAuthored bool, head string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On a pull request parked at `%s`:\n\n", PRNeedsDecisionStageLabel)
	fmt.Fprintf(&b, "- **Approve** %s (GitHub review), or reply `/hive approve` after the swarm's `requires_human` verdict, to merge it as-is; a trusted merger's release only, and never past CI, `hold`-class labels or a `changes_requested` verdict.\n", prHeadRef(head))
	if selfAuthored {
		fmt.Fprintf(&b, "- On this hive-authored PR, `/hive approve` from a trusted merger also lifts a `%s` the hive added (a person's stays), adding `%s` and removing `%s`.\n", issueNeedsHumanLabel, escalation.ReviewerPassedLabel, prReviewerHandoffLabel)
		b.WriteString("- Leave a **changes requested** review, or reply `/hive decision <what to change>`, and the hive pushes the fix to this PR.\n")
	} else {
		b.WriteString("- Leave a **changes requested** review and the author is nudged with your text; the hive does not push to a PR it did not open.\n")
	}
	fmt.Fprintf(&b, "\nSee <%s>.", maintainerCommandsDocURL)
	return b.String()
}
