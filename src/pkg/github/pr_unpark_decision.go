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
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
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
		return c.reconcileAnsweredPRDecision(ctx, displayRepo, owner, repo, number, comments, opts)
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
	if closer, closeRequested := supersessionCloseDecision(command); closeRequested {
		reply, handled, err := c.closeSupersededPRDecision(ctx, displayRepo, owner, repo, pr, actor, closer)
		if err != nil {
			return false, err
		}
		if handled {
			if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), reply); err != nil {
				return false, err
			}
			result.Replies++
			return true, nil
		}
	}
	switch {
	case command.Kind == unparkCommandDecision && selfAuthored && head != "":
		mode := prDecisionModeFor(command)
		req := review.HumanChangeRequest{
			Repo:     owner + "/" + repo,
			Number:   number,
			HeadSHA:  head,
			Source:   review.HumanChangeRequestSourceDecision,
			SourceID: candidate.GetID(),
			Actor:    actor,
			Body:     command.DecisionText(),
			URL:      candidate.GetHTMLURL(),
			Kind:     mode.kind(),
		}
		if opts.HumanChangeRequests == nil {
			return false, fmt.Errorf("no human change request store; `/hive decision` on %s/%s#%d by @%s cannot be routed", owner, repo, number, actor)
		}
		// Record before replying: a reply without a record would tell the
		// maintainer the fix is coming when nothing is queued.
		if err := opts.HumanChangeRequests.Record(req); err != nil {
			return false, fmt.Errorf("recording `/hive decision` on %s/%s#%d: %w", owner, repo, number, err)
		}
		event := PullRequestDecisionEvent{Repo: displayRepo, Number: number, HeadSHA: head, Actor: actor, Decision: req.Body, Mode: string(mode), CommentURL: candidate.GetHTMLURL()}
		result.PRDecisions = append(result.PRDecisions, event)
		if opts.PRAudit != nil {
			opts.PRAudit(event)
		}
		stage := c.applyPRDecisionStage(ctx, owner, repo, number, head, actor, mode, selfAuthored)
		c.logPRDecision(displayRepo, number, head, actor, mode, stage, false)
		reply = renderPRDecisionReply(actor, head, mode)
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

// prDecisionMode is what a `/hive decision` on a PR asks for.
type prDecisionMode string

const (
	// prDecisionChangesRequested: the decision carries an instruction; the
	// PR goes to fixing and the author agent follows it verbatim.
	prDecisionChangesRequested prDecisionMode = "changes_requested"
	// prDecisionApprove: a bare `/hive decision approve`; the owner answered
	// the swarm's question with "yes" and the merge path takes over.
	prDecisionApprove prDecisionMode = "approve"
)

func (m prDecisionMode) kind() string {
	if m == prDecisionApprove {
		return review.HumanChangeRequestKindApprove
	}
	return review.HumanChangeRequestKindChanges
}

func prDecisionModeFor(cmd unparkCommand) prDecisionMode {
	if cmd.BareApproval() {
		return prDecisionApprove
	}
	return prDecisionChangesRequested
}

// prDecisionHeadMarker tags the reply with the exact head the decision was
// recorded for, so a restart that lost the store can rebuild the record
// from the comment thread alone (reconcileAnsweredPRDecision).
const prDecisionHeadMarker = "<!-- hive:decision-head "

func prDecisionHeadMarkerLine(head string, mode prDecisionMode) string {
	return fmt.Sprintf("%s%s mode=%s -->", prDecisionHeadMarker, strings.TrimSpace(head), mode)
}

// parsePRDecisionHeadMarker reads the head and mode a recorded-decision reply
// was posted for. Replies from before the marker existed carry only the
// 7-character head in prose ("head `3b4a2b5`"); those are matched by prefix
// against the live head, with the mode read from the prose.
func parsePRDecisionHeadMarker(reply, liveHead string) (prDecisionMode, bool) {
	liveHead = strings.TrimSpace(liveHead)
	if liveHead == "" {
		return "", false
	}
	if idx := strings.Index(reply, prDecisionHeadMarker); idx >= 0 {
		rest := reply[idx+len(prDecisionHeadMarker):]
		if end := strings.Index(rest, "-->"); end >= 0 {
			rest = rest[:end]
		}
		fields := strings.Fields(rest)
		if len(fields) < 1 || !strings.EqualFold(fields[0], liveHead) {
			return "", false
		}
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "mode=") {
				if m := prDecisionMode(strings.TrimPrefix(f, "mode=")); m == prDecisionApprove {
					return prDecisionApprove, true
				}
			}
		}
		return prDecisionChangesRequested, true
	}
	if !strings.Contains(reply, "Decision recorded by @") && !strings.Contains(reply, "Approval recorded by @") {
		return "", false
	}
	short := liveHead
	if len(short) > 7 {
		short = short[:7]
	}
	if !strings.Contains(reply, "head `"+short+"`") {
		return "", false
	}
	if strings.Contains(reply, "Approval recorded by @") {
		return prDecisionApprove, true
	}
	return prDecisionChangesRequested, true
}

// reconcileAnsweredPRDecision is the restart path (hivecommons/hive#14679).
// The reply marker says the newest `/hive decision` on this PR was already
// acknowledged, so it is never answered twice — but the acknowledgement is
// only the promise; the record that moves the PR and kicks the fix lives in
// the change-request store. If that record is missing for the live head
// (lost data dir, a crash between record and persist, an older build that
// recorded a different head) the decision is re-recorded from the thread,
// silently, and the stage re-applied. A decision answered for an older head
// is left alone: the author already pushed past it.
func (c *Client) reconcileAnsweredPRDecision(ctx context.Context, displayRepo, owner, repo string, number int, comments []*gh.IssueComment, opts IssueUnparkSweepOptions) (bool, error) {
	if opts.HumanChangeRequests == nil || opts.TrustedMerger == nil {
		return false, nil
	}
	replies := c.unparkRepliesByCommentID(comments)
	var decision *gh.IssueComment
	var cmd unparkCommand
	for i := len(comments) - 1; i >= 0; i-- {
		cm := comments[i]
		if cm == nil || replies[cm.GetID()] == nil || !c.isHumanAuthor(cm.GetUser()) {
			continue
		}
		parsed, ok := parseUnparkCommand(cm.GetBody())
		if !ok {
			continue
		}
		if parsed.Kind != unparkCommandDecision {
			if isUnparkWatermarkCommand(cm.GetBody()) {
				return false, nil
			}
			continue
		}
		if !opts.TrustedMerger(safeGetLogin(cm.GetUser())) {
			continue
		}
		decision, cmd = cm, parsed
		break
	}
	if decision == nil {
		return false, nil
	}
	// Already recorded with the text as it reads now: nothing to repair. A
	// record whose body differs (an older build kept only the first line,
	// hivecommons/hive#14628) is re-recorded below so the planner and the
	// lane see the instruction the person actually gave.
	if store, ok := opts.HumanChangeRequests.(interface {
		Lookup(repo string, number int, source string, sourceID int64) (review.HumanChangeRequest, bool)
	}); ok {
		if existing, found := store.Lookup(owner+"/"+repo, number, review.HumanChangeRequestSourceDecision, decision.GetID()); found && existing.Body == cmd.DecisionText() {
			return false, nil
		}
	}
	pr, resp, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("fetching pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	if strings.EqualFold(pr.GetState(), "closed") || pr.GetHead() == nil {
		return false, nil
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" {
		return false, nil
	}
	mode, ok := parsePRDecisionHeadMarker(replies[decision.GetID()].GetBody(), head)
	if !ok {
		return false, nil
	}
	if existing, found := opts.HumanChangeRequests.For(owner+"/"+repo, number, head); found && existing.Source == review.HumanChangeRequestSourceDecision && existing.SourceID == decision.GetID() && existing.Body == cmd.DecisionText() && existing.Kind == mode.kind() {
		return false, nil
	}
	selfAuthored := c.isHiveAccount(pr.GetUser()) || labelPresent(issueLabelNames(pr.Labels), authorkind.Hive.Label())
	if !selfAuthored {
		return false, nil
	}
	actor := safeGetLogin(decision.GetUser())
	req := review.HumanChangeRequest{
		Repo:       owner + "/" + repo,
		Number:     number,
		HeadSHA:    head,
		Source:     review.HumanChangeRequestSourceDecision,
		SourceID:   decision.GetID(),
		Actor:      actor,
		Body:       cmd.DecisionText(),
		URL:        decision.GetHTMLURL(),
		Kind:       mode.kind(),
		RecordedAt: replies[decision.GetID()].GetCreatedAt().Time,
	}
	if err := opts.HumanChangeRequests.Record(req); err != nil {
		return false, fmt.Errorf("re-recording `/hive decision` on %s/%s#%d: %w", owner, repo, number, err)
	}
	stage := c.applyPRDecisionStage(ctx, owner, repo, number, head, actor, mode, selfAuthored)
	c.logPRDecision(displayRepo, number, head, actor, mode, stage, true)
	return true, nil
}

// unparkRepliesByCommentID maps each answered command's comment ID to the
// hive's reply that answered it.
func (c *Client) unparkRepliesByCommentID(comments []*gh.IssueComment) map[int64]*gh.IssueComment {
	out := map[int64]*gh.IssueComment{}
	for _, cm := range comments {
		if cm == nil || !c.markerAuthorTrusted(cm.GetUser()) {
			continue
		}
		body := cm.GetBody()
		idx := strings.Index(body, unparkReplyMarkerPrefix)
		if idx < 0 {
			continue
		}
		rest := body[idx+len(unparkReplyMarkerPrefix):]
		end := strings.Index(rest, " ")
		if end < 0 {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(rest[:end], "%d", &id); err == nil && id > 0 {
			out[id] = cm
		}
	}
	return out
}

// applyPRDecisionStage submits the decision to the stage engine so the park
// lifts now rather than on the next sweep. A failed write is not fatal: the
// record stands in the store, and the lanes derive the same stage from it
// on their next pass. It returns the stage written, or "" when none was.
func (c *Client) applyPRDecisionStage(ctx context.Context, owner, repo string, number int, head, actor string, mode prDecisionMode, selfAuthored bool) string {
	sink := c.StageFacts()
	if sink == nil {
		return ""
	}
	tr, err := sink.PRDecided(ctx, c.PRStageRef(owner+"/"+repo, number), stageengine.PRDecisionFact{
		Head:         head,
		By:           actor,
		Instruction:  mode == prDecisionChangesRequested,
		SelfAuthored: selfAuthored,
	})
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("decision: stage write deferred to next sweep", "repo", owner+"/"+repo, "pr", number, "head", head, "error", err)
		}
		return ""
	}
	if tr.To == stagelabels.None {
		return ""
	}
	return tr.To.Label()
}

func (c *Client) logPRDecision(displayRepo string, number int, head, actor string, mode prDecisionMode, stage string, reconciled bool) {
	if c.logger == nil {
		return
	}
	stageText := stage
	if stageText == "" {
		stageText = "next-sweep"
	}
	c.logger.Info(fmt.Sprintf("decision: unpark pr=%d head=%s mode=%s by=%s → stage=%s", number, prShortHead(head), mode, actor, stageText),
		"repo", displayRepo, "pr", number, "head", head, "mode", string(mode), "by", actor, "stage", stage, "reconciled", reconciled)
}

func prShortHead(head string) string {
	head = strings.TrimSpace(head)
	if len(head) > 7 {
		return head[:7]
	}
	return head
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

func renderPRDecisionReply(actor, head string, mode prDecisionMode) string {
	if mode == prDecisionApprove {
		return renderPRDecisionApprovedReply(actor, head)
	}
	return renderPRDecisionRecordedReply(actor, head)
}

func renderPRDecisionRecordedReply(actor, head string) string {
	return fmt.Sprintf("%s\nDecision recorded by @%s for %s. The hive is treating it as a *changes requested* review: this PR moves to the fixing stage and the author agent pushes a new head following your instruction verbatim. The review swarm then re-reviews that head; if it approves, the PR comes back for your merge approval.",
		prDecisionHeadMarkerLine(head, prDecisionChangesRequested), actor, prHeadRef(head))
}

func renderPRDecisionApprovedReply(actor, head string) string {
	return fmt.Sprintf("%s\nApproval recorded by @%s for %s. The hive is treating it as your answer to the review swarm's question: nothing is sent back to the author, and the PR follows the merge path on the next pass (merged when the merge gates pass, or handed to you at the ready-for-merge stage where a person has to press the button).",
		prDecisionHeadMarkerLine(head, prDecisionApprove), actor, prHeadRef(head))
}

func renderPRDecisionNoHeadReply() string {
	return "I could not read this PR's head commit, so the decision was not routed. Leave a *changes requested* review with the same text and the hive will pick it up on its next pass."
}

func renderPRDecisionNotHiveAuthoredReply(head string) string {
	return fmt.Sprintf("`/hive decision` routes a fix to the hive's own author agent, and the hive did not open this PR, so it will not push to it. The PR-side equivalents are reviews: **Approve** %s to let it merge as-is, or leave a **changes requested** review and the author is nudged with your text. See <%s>.",
		prHeadRef(head), maintainerCommandsDocURL)
}

func renderPRApproveReply(head string) string {
	return fmt.Sprintf("On a pull request the approval is the GitHub review: **Approve** %s and the hive merges it on its next pass (`/hive approve` is for parked issues). See <%s>.",
		prHeadRef(head), maintainerCommandsDocURL)
}

func renderPRHelpReply(selfAuthored bool, head string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "On a pull request parked at `%s`:\n\n", PRNeedsDecisionStageLabel)
	fmt.Fprintf(&b, "- **Approve** %s (GitHub review) to merge it as-is.\n", prHeadRef(head))
	if selfAuthored {
		b.WriteString("- Leave a **changes requested** review, or reply `/hive decision <what to change>`, and the hive pushes the fix to this PR.\n")
	} else {
		b.WriteString("- Leave a **changes requested** review and the author is nudged with your text; the hive does not push to a PR it did not open.\n")
	}
	b.WriteString("- For a superseded PR, reply `/hive decision reject` or `/hive decision close superseded by #N` to close it.\n")
	fmt.Fprintf(&b, "\nSee <%s>.", maintainerCommandsDocURL)
	return b.String()
}
