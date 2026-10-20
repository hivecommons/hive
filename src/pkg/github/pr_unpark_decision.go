package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/review"
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
//   - `/hive approve` and `/hive help` get the same pointer.
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
	case command.Kind == unparkCommandApprove:
		reply = renderPRApproveReply(head)
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

func renderPRApproveReply(head string) string {
	return fmt.Sprintf("Noted. On a pull request a trusted merger's `/hive approve` reply posted after the swarm's `requires_human` verdict releases that hold on %s on the hive's next pass, exactly as a GitHub **Approve** review on that head does (the owner's own PR included, where GitHub refuses the self-review). It does not bypass CI, `hold`-class labels, a `changes_requested` / `reject` verdict or a lane that only merges by hand: if this PR is parked for one of those, the `%s` comment above names what clears it. See <%s>.",
		prHeadRef(head), PRNeedsDecisionStageLabel, maintainerCommandsDocURL)
}

func renderPRHelpReply(selfAuthored bool, head string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On a pull request parked at `%s`:\n\n", PRNeedsDecisionStageLabel)
	fmt.Fprintf(&b, "- **Approve** %s (GitHub review), or reply `/hive approve` after the swarm's `requires_human` verdict, to merge it as-is; a trusted merger's release only, and never past CI, `hold`-class labels or a `changes_requested` verdict.\n", prHeadRef(head))
	if selfAuthored {
		b.WriteString("- Leave a **changes requested** review, or reply `/hive decision <what to change>`, and the hive pushes the fix to this PR.\n")
	} else {
		b.WriteString("- Leave a **changes requested** review and the author is nudged with your text; the hive does not push to a PR it did not open.\n")
	}
	fmt.Fprintf(&b, "\nSee <%s>.", maintainerCommandsDocURL)
	return b.String()
}
