package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// issueParkingLabels are the labels that park an issue on a person and so
// make it the un-park sweep's business: `needs-human`, the park signal
// `stage/045-needs-decision` (hivecommons/hive#12683), the retired
// spellings — the pre-045 `stage/085-needs-decision`, `needs-decision`,
// `needs-direction` — until the stage reconciler has migrated them, and
// `blocked` (issueBlockedLabel). Only unparkRemovableLabels (plus the
// operator's configured needs_decision / needs_direction names) are ever
// cleared by a /hive command. `hold` is deliberately untouched because it has
// several independent sources (the ACMM level gate, the SHA hold, holdguard
// and a person), and clearing one of them from a comment would silently clear
// all of them.
var issueParkingLabels = []string{issueNeedsHumanLabel, NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel, issueBlockedLabel}

// unparkRemovableLabels are the park labels `/hive approve` / `/hive
// decision` (and label approval) always remove. `needs-human` is NOT among
// them: it goes through stageengine.DecisionLiftsNeedsHuman, which reads
// who applied it so the reply can attribute the cleared label. The rule
// lifts it for every applier: a maintainer's explicit command is a later,
// stronger signal than a label they added in prose earlier
// (hivecommons/docs#419), and the hive's own `needs-human` left in place
// re-derives stage/045 on the next reconcile so the un-park silently
// reverts (hivecommons/hive#13075, as on #12984). A maintainer who wants
// agents to stay off the issue after deciding says so with `hold`.
var unparkRemovableLabels = []string{NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel, issueBlockedLabel}

const maintainerCommandsDocURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/maintainer-commands.md"

// unparkCommandPrefix is the first token of every command this sweep answers.
const unparkCommandPrefix = "/hive"

// unparkNoticeMarker tags the "What to reply" comment the hive keeps on every
// parked issue, so it is edited in place rather than duplicated per cycle.
const unparkNoticeMarker = "<!-- hive:unpark-notice:9879 -->"

// unparkReplyMarkerPrefix tags the hive's answer to one specific comment. The
// comment ID is appended, which is what makes the sweep idempotent: a command
// whose answer is already on the timeline is never acted on twice.
const unparkReplyMarkerPrefix = "<!-- hive:unpark-reply:9879 id="

// unparkCommandScanWindow bounds how far back a command is honoured. A reply
// the hive could not see for a week is stale enough that the maintainer will
// have moved on, and the window also stops a first rollout from replaying
// years of old comments on every parked issue in the fleet.
const unparkCommandScanWindow = 7 * 24 * time.Hour

// DefaultIssueUnparkSweepMaxActions caps the issues one sweep writes to, in the
// same spirit as DefaultTaskListSweepMaxCloses: a misparse cannot rewrite a
// whole backlog's labels in a single tick.
const DefaultIssueUnparkSweepMaxActions = 10

// unparkPermissionLevels are the repository permissions that may unpark an
// issue. It is the same set the dashboard's repo-hold endpoint accepts: some
// contributor lanes run as ordinary user accounts that pass isHumanAuthor, and
// their pull-only tokens are what this check stops.
var unparkPermissionLevels = map[string]bool{"admin": true, "maintain": true, "write": true}

// unparkCommandKind is which command a comment carries.
type unparkCommandKind string

const (
	unparkCommandNone     unparkCommandKind = ""
	unparkCommandApprove  unparkCommandKind = "approve"
	unparkCommandDecision unparkCommandKind = "decision"
	unparkCommandHelp     unparkCommandKind = "help"
	// unparkCommandDone reports that the owner did a step only they could
	// do; the issue goes back to the queue to be verified and closed.
	unparkCommandDone unparkCommandKind = "done"

	// unparkCommandRefusedAIAuthor is a `/hive` command from the account named
	// in project.ai_author. Hive treats that account as itself, so it refuses
	// out loud instead of acting or staying silent.
	unparkCommandRefusedAIAuthor unparkCommandKind = "refused-ai-author"

	// unparkCommandFixed is `/fixed` (alias `/close`) on a parked issue: the
	// maintainer reports the thing the issue waited on was done, typically
	// out of band ("needs admin action outside the repo"), and the issue
	// closes as completed (hivecommons/dibs#459).
	unparkCommandFixed unparkCommandKind = "fixed"
)

// fixedCommandTokens are the first-line spellings of unparkCommandFixed,
// matching src/scripts/issue-confirm-fixed.sh.
var fixedCommandTokens = []string{"/fixed", "/close"}

// unparkCommand is a parsed `/hive …` or `/fixed` comment.
type unparkCommand struct {
	Kind     unparkCommandKind
	Decision string
	// Text is the whole decision as the person wrote it: the rest of the
	// command line plus every line after it, trimmed. Decision stays the
	// first line so the reply's `Decision:` round-trip is unchanged; the PR
	// path hands Text to the author agent verbatim (hivecommons/hive#14679).
	Text string
}

// Approval reports whether the decision explicitly approves the head: the
// first word is `approve`, `approved` or `lgtm` (trailing punctuation
// ignored) and the rest is retained as the approval rationale. A bare `yes`
// approves too, but `yes, but …` is a conditional answer and stays an
// instruction for the fix lane (hivecommons/hive#14816).
func (c unparkCommand) Approval() bool {
	text := strings.TrimSpace(c.DecisionText())
	words := strings.Fields(text)
	if len(words) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimRight(words[0], ".:,;!")) {
	case "approve", "approved", "lgtm":
		return true
	case "yes":
		return len(words) == 1
	}
	return false
}

// DecisionText is what the author agent should follow: the full comment
// body after the command when it has one, else the first-line decision.
func (c unparkCommand) DecisionText() string {
	if t := strings.TrimSpace(c.Text); t != "" {
		return t
	}
	return strings.TrimSpace(c.Decision)
}

// IssueUnparkEvent records one issue this sweep un-parked, for the audit log.
type IssueUnparkEvent struct {
	Repo       string
	Number     int
	Actor      string
	Decision   string
	CommentURL string
	ReleaseTag string
	ReleaseURL string
}

// IssueUnparkSweepOptions configures SweepIssueUnparkCommands.
type IssueUnparkSweepOptions struct {
	// PRApproveReply explains a `/hive approve` reply on a pull request using
	// the merge engine's policy: what the lane will do with it and why it
	// cannot use it (hivecommons/hive#13087). It must not merge or change
	// labels. nil leaves PRs to the 085 decision path alone.
	PRApproveReply func(context.Context, string, int, *gh.IssueComment) (string, error)
	// MaxActions caps the issues written to in one pass. Zero means
	// DefaultIssueUnparkSweepMaxActions.
	MaxActions int
	// Audit, when set, is called once per un-parked issue.
	Audit func(IssueUnparkEvent)
	// LabelDriven reports whether a watched repo opted in to label-driven
	// triage via project.repo_policies[].label_driven (hivecommons/hive#10537).
	// On such a repo the sweep posts no "What to reply" notice and never
	// removes a parking label; `/hive approve` and `/hive decision` get a
	// one-line reply pointing at the labels instead. nil means no repo opts in.
	LabelDriven func(repo string) bool
	// ReleaseOnApprove reports whether a repo opted in to the hub-owned
	// kind=release executor. nil keeps release owner steps manual.
	ReleaseOnApprove func(repo string) bool
	// ReleaseVersionSource returns an optional repository-relative text file
	// whose value must match the proposed release tag.
	ReleaseVersionSource func(repo string) string
	// LabelApproval resolves label-driven approval (hivecommons/hive#11313)
	// for a repo. It is consulted only for repos LabelDriven reports true,
	// so a repo that did not opt in never reaches it. nil falls back to the
	// resolver installed with SetLabelApprovalPolicy; with neither, the
	// label-driven sweep behaves exactly as before #11313.
	LabelApproval func(repo string) (LabelApprovalPolicy, bool)
	// LabelApprovalAudit, when set, is called once per acceptance acted on.
	LabelApprovalAudit func(IssueLabelApprovalEvent)
	// HumanChangeRequests receives `/hive decision <text>` left by a trusted
	// maintainer on a hive-authored PR parked at stage/085-needs-decision
	// or stage/070-fixing; the review planner turns instructions into a fix kick.
	// nil means the sweep answers the PR but cannot route the decision.
	HumanChangeRequests review.HumanChangeRequestStore
	// TrustedMerger is the automerge lane's merger trust test. A PR-side
	// `/hive decision` needs it on top of repo write access, the same bar
	// the self-authored lane sets for a human approval or changes_requested
	// review. nil (not configured) means no PR decision is acted on.
	TrustedMerger func(login string) bool
	// PRAudit, when set, is called once per PR decision routed to the fix lane.
	PRAudit func(PullRequestDecisionEvent)
	// ClosedAudit, when set, is called once per parked issue a maintainer's
	// `/fixed` closed (hivecommons/dibs#459).
	ClosedAudit func(IssueUnparkEvent)
}

// PullRequestDecisionEvent records one `/hive decision` on a hive-authored
// PR the sweep routed to the fix lane (hivecommons/hive#13041).
type PullRequestDecisionEvent struct {
	Repo     string
	Number   int
	HeadSHA  string
	Actor    string
	Decision string
	// Mode is "changes_requested" (the decision carries an instruction)
	// or "approve" (an approval word followed by optional rationale).
	Mode       string
	CommentURL string
	// ToContributor is the owning contributor's login when the direction
	// was carried to their hive rather than the hive's own author agent (a
	// clanker-authored PR, hivecommons/hive#15159); "" for the hive's PRs.
	ToContributor string
	// ParkLabelsLifted counts the park labels (needs-human, the 085 stage
	// label) the decision took off in the same write.
	ParkLabelsLifted int
}

// IssueLabelApprovalEvent records one acceptance the sweep acted on, for the
// audit log (hivecommons/hive#11313).
type IssueLabelApprovalEvent struct {
	Repo   string
	Number int
	// Label is the acceptance label and Actor who added it.
	Label string
	Actor string
	// TrustReason says why Actor counted (role, org membership, bot allowlist).
	TrustReason string
	// Removed lists the hive park labels the acceptance cleared.
	Removed []string
	// Released is true when no park label remains.
	Released bool
	// Pending is true when a person's park label withheld the approval.
	Pending bool
}

// IssueUnparkSweepResult reports what one pass did.
type IssueUnparkSweepResult struct {
	// Seen counts parked issues examined.
	Seen int
	// Skipped counts parked issues that carried no actionable command, plus
	// issues and PRs whose comment read or reply failed in this pass.
	Skipped int
	// Unparked lists the issues whose parking labels were cleared.
	Unparked []IssueUnparkEvent
	// Replies counts answers posted that did not un-park: `/hive help`, the
	// hint left when a maintainer approves in prose, and PR approval replies.
	Replies int
	// LabelApproved lists the acceptances acted on via a configured approval
	// label on label-driven repos (hivecommons/hive#11313).
	LabelApproved []IssueLabelApprovalEvent
	// PRDecisions lists the `/hive decision` comments on hive-authored PRs
	// routed to the fix lane (hivecommons/hive#13041).
	PRDecisions []PullRequestDecisionEvent
	// Closed lists the parked issues a maintainer's `/fixed` closed as
	// completed (hivecommons/dibs#459); Decision carries the note after the
	// command, if any.
	Closed []IssueUnparkEvent
}

func (r *IssueUnparkSweepResult) actions() int {
	return len(r.Unparked) + r.Replies + len(r.LabelApproved) + len(r.PRDecisions) + len(r.Closed)
}

// SweepIssueUnparkCommands lets a maintainer re-queue a parked issue with a
// one-line reply instead of editing labels (hivecommons/hive#9879).
//
// On every parked issue (`needs-human`, `stage/045-needs-decision`, the
// retired pre-045 `stage/085-needs-decision`, `needs-decision` /
// `needs-direction`, or `blocked`) the sweep keeps
// a "What to reply" comment listing the commands, because the
// maintainer should never have to remember what to type. When an eligible
// person replies:
//
//   - `/hive approve` — go with the issue's own recommendation;
//   - `/hive decision <text>` — go ahead, following <text>;
//   - `/hive done` — the owner did a step only they could do; the issue
//     goes back to the queue to be verified and closed;
//   - `/hive help` — repeat the commands, change nothing;
//   - `/fixed` (alias `/close`) — the thing the issue waited on is done:
//     close it as completed and clear the hive's park labels
//     (hivecommons/dibs#459). Many parked issues wait on an admin action
//     outside the repository, where there is no PR to merge.
//
// The parking labels are the built-in names plus any the operator configured
// in project.issue_filter.hard_suppress_labels, the same set the dashboard's
// "your turn" decision reads; needs_spec is never answered here.
//
// Eligibility is deliberately narrow and every failing case is ignored in
// silence, so nobody can probe it: the commenter must be a person other than
// this hive (isHumanAuthor), must hold write, maintain or admin on the repo
// (checked live), the command must be on the comment's first line outside any
// quote or fence, and the comment must be recent and unedited — an edit to an
// old comment is not a new instruction.
func (c *Client) SweepIssueUnparkCommands(ctx context.Context, opts IssueUnparkSweepOptions) (*IssueUnparkSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultIssueUnparkSweepMaxActions
	}
	result := &IssueUnparkSweepResult{}
	if opts.HumanChangeRequests != nil {
		c.ReconcileSelfDecisionChangeRequests(opts.HumanChangeRequests)
	}
	cutoff := time.Now().Add(-unparkCommandScanWindow)

	for _, repo := range c.getRepos() {
		if result.actions() >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		if c.stageArchived.has(strings.ToLower(owner + "/" + repoName)) {
			continue
		}
		labelDriven := opts.LabelDriven != nil && opts.LabelDriven(repo)
		releasePolicy := unparkReleasePolicy{
			enabled: opts.ReleaseOnApprove != nil && opts.ReleaseOnApprove(repo),
		}
		if releasePolicy.enabled && opts.ReleaseVersionSource != nil {
			releasePolicy.versionSource = opts.ReleaseVersionSource(repo)
		}
		var policy LabelApprovalPolicy
		labelApproval := false
		if labelDriven {
			policy, labelApproval = c.resolveLabelApproval(opts, repo)
		}
		issues, err := c.listOpenIssuesForTaskListSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if result.actions() >= maxActions {
				break
			}
			if issue == nil {
				continue
			}
			labels := issueLabelNames(issue.Labels)
			if issue.IsPullRequest() {
				// `/hive approve` on any PR gets an acknowledgement or an
				// explanation of why the lane cannot use it
				// (hivecommons/hive#13087). It never removes labels, so no
				// parking label is required and the reply is not an action
				// against the issue path.
				if opts.PRApproveReply != nil && issue.GetComments() > 0 && !issue.GetUpdatedAt().Time.Before(cutoff) {
					acted, err := c.replyPRApproveCommand(ctx, repo, owner, repoName, issue, cutoff, opts.PRApproveReply)
					if err != nil {
						// One PR's failure must not stop un-parking for the rest of the pass.
						if c.logger != nil {
							c.logger.Warn("unpark sweep skipped PR approve reply", "repo", repo, "pr", issue.GetNumber(), "error", err)
						}
						result.Skipped++
						continue
					}
					if acted {
						result.Replies++
						continue
					}
				}
				// Otherwise PRs take one command only — `/hive decision`
				// while at 085 or 070 — and never a
				// notice.
				if !labelPresent(labels, PRNeedsDecisionStageLabel) && !labelPresent(labels, stagelabels.Fixing.Label()) {
					continue
				}
				result.Seen++
				acted, err := c.tryUnparkPullRequest(ctx, repo, owner, repoName, issue, cutoff, opts, result)
				if err != nil {
					if c.logger != nil {
						c.logger.Warn("unpark sweep skipped pull request", "repo", repo, "pr", issue.GetNumber(), "error", err)
					}
					result.Skipped++
					continue
				}
				if !acted {
					result.Skipped++
				}
				continue
			}
			parked := c.hasParkingLabel(labels)
			if !parked && !(labelApproval && hasLabelApprovalCandidate(labels, policy)) {
				continue
			}
			result.Seen++
			acted, err := c.tryUnparkIssue(ctx, repo, owner, repoName, issue, cutoff, labelDriven, unparkLabelApproval{policy: policy, enabled: labelApproval, audit: opts.LabelApprovalAudit}, releasePolicy, result, opts.Audit, opts.ClosedAudit)
			if err != nil {
				if c.noteArchivedWrite(strings.ToLower(owner+"/"+repoName), err) {
					if c.logger != nil {
						c.logger.Info("unpark sweep: repository is archived and read-only, skipping it until restart", "repo", repo)
					}
					result.Skipped++
					break
				}
				if c.logger != nil {
					c.logger.Warn("unpark sweep skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
				continue
			}
			if !acted {
				result.Skipped++
			}
		}
	}
	return result, nil
}

// resolveLabelApproval picks the label-approval resolver: the sweep option
// first, then the client-wide one. Only called for label-driven repos.
func (c *Client) resolveLabelApproval(opts IssueUnparkSweepOptions, repo string) (LabelApprovalPolicy, bool) {
	if opts.LabelApproval != nil {
		p, ok := opts.LabelApproval(repo)
		if !ok || !p.Enabled() {
			return LabelApprovalPolicy{}, false
		}
		return p, true
	}
	return c.labelApprovalPolicyFor(repo)
}

// hasLabelApprovalCandidate reports whether an unparked issue still has
// something for the label path to do: an acceptance label without the
// approval label (R3: approval is given whether or not the issue is parked).
func hasLabelApprovalCandidate(labels []string, policy LabelApprovalPolicy) bool {
	if labelPresent(labels, HumanAckLabel) {
		return false
	}
	for _, l := range labels {
		if policy.hasLabel(l) {
			return true
		}
	}
	return false
}

// unparkLabelApproval carries the per-repo label-approval settings into
// tryUnparkIssue. enabled is false for every repo that did not opt in.
type unparkLabelApproval struct {
	policy  LabelApprovalPolicy
	enabled bool
	audit   func(IssueLabelApprovalEvent)
}

// issueOwnerStepProposed reports whether the hive ever parked the issue on an
// owner step: a hive-authored body marker or a hive comment marker. Any such
// issue keeps its cache entry current, so a typed release proposed in a
// comment and later superseded by a manual step (releaseStep == nil, no body
// marker) clears the stale "Release <tag>" the dashboard would otherwise
// keep offering until restart.
func (c *Client) issueOwnerStepProposed(issue *gh.Issue, comments []*gh.IssueComment) bool {
	if c.isHiveAccount(issue.GetUser()) && hasOwnerStepMarker(issue.GetBody()) {
		return true
	}
	for _, cm := range comments {
		if cm != nil && c.isHiveAccount(cm.GetUser()) && hasOwnerStepMarker(cm.GetBody()) {
			return true
		}
	}
	return false
}

// unparkTextsProposeOwnerStep reports whether the issue body or any comment
// carries an owner-step marker, whoever authored it: `/hive approve` there
// accepts the proposed step and keeps its legacy meaning (the menu credits
// the marker only on hive-authored text; this check only decides whether
// approve still needs a recommendation).
func unparkTextsProposeOwnerStep(issue *gh.Issue, comments []*gh.IssueComment) bool {
	if hasOwnerStepMarker(issue.GetBody()) {
		return true
	}
	for _, cm := range comments {
		if cm != nil && hasOwnerStepMarker(cm.GetBody()) {
			return true
		}
	}
	return false
}

// tryUnparkIssue handles one parked issue: keep its "What to reply" comment
// current, then answer the newest eligible command on it. On a label-driven
// repo there is no notice; without label approval configured there is no
// label change either: commands get a pointer to the labels and prose assent
// gets no hint. With label approval (hivecommons/hive#11313) a trusted
// acceptance label is acted on first, and `/hive approve` / `/hive decision`
// remove only the park labels the hive added itself.
func (c *Client) tryUnparkIssue(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, labelDriven bool, la unparkLabelApproval, releasePolicy unparkReleasePolicy, result *IssueUnparkSweepResult, audit, closedAudit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	menu := c.unparkMenuFor(issue.GetBody(), issue.GetUser(), comments)
	if menu.releaseStep != nil || c.issueOwnerStepProposed(issue, comments) {
		c.rememberIssueOwnerStep(displayRepo, number, menu.releaseStep)
	}
	if !labelDriven {
		if err := c.ensureUnparkNoticeComment(ctx, owner, repo, number, issue.GetBody(), issue.GetUser(), comments, releasePolicy.enabled); err != nil {
			return false, err
		}
	}

	if la.enabled {
		acted, err := c.tryLabelApproval(ctx, displayRepo, owner, repo, issue, comments, la, result)
		if err != nil || acted {
			return acted, err
		}
	}
	if !c.hasParkingLabel(issueLabelNames(issue.Labels)) {
		return false, nil
	}

	answered := c.answeredUnparkCommentIDs(comments)
	candidate, command, err := c.eligibleUnparkCommand(ctx, owner, repo, comments, answered, cutoff, labelDriven && la.enabled)
	if err != nil {
		return false, err
	}
	if candidate == nil {
		return false, nil
	}
	actor := safeGetLogin(candidate.GetUser())

	if command.Kind == unparkCommandRefusedAIAuthor {
		if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkAIAuthorRefusal(actor)); err != nil {
			return false, err
		}
		result.Replies++
		if c.logger != nil {
			c.logger.Warn("unpark sweep: /hive command ignored, commenter is project.ai_author",
				"repo", displayRepo, "issue", number, "login", actor)
		}
		return true, nil
	}

	if command.Kind == unparkCommandFixed {
		return c.closeParkedIssueAsFixed(ctx, displayRepo, owner, repo, issue, candidate, command, labelDriven, result, closedAudit)
	}

	// `/hive approve` records what the menu says it accepts, and is refused
	// when there is nothing to accept. A typed release proposal and the
	// label-driven pointer reply (no label approval) answer it themselves.
	// Approve needs no recommendation on a `blocked` park, where it means
	// "the blocker is cleared" (hivecommons/hive#12376), or on an owner
	// step, where it accepts the step the hive proposed.
	if command.Kind == unparkCommandApprove && !(releasePolicy.enabled && menu.releaseStep != nil) && (!labelDriven || la.enabled) &&
		!menu.ownerStep && menu.releaseStep == nil && !unparkTextsProposeOwnerStep(issue, comments) && !labelPresent(issueLabelNames(issue.Labels), issueBlockedLabel) {
		decision, ok := menu.approveDecision()
		if !ok {
			if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), unparkApproveNothingReply); err != nil {
				return false, err
			}
			result.Replies++
			return true, nil
		}
		command.Decision = decision
	}

	if labelDriven {
		parking := c.parkingLabelsPresent(issueLabelNames(issue.Labels))
		body := renderUnparkLabelDrivenReply(parking)
		switch command.Kind {
		case unparkCommandNone:
			return false, nil
		case unparkCommandHelp:
			body = renderUnparkLabelDrivenReply(parking) + "\n\n" + maintainerCommandReference()
		case unparkCommandApprove, unparkCommandDecision, unparkCommandDone:
			if la.enabled {
				return c.unparkLabelDrivenCommand(ctx, displayRepo, owner, repo, issue, candidate, command, result, audit)
			}
		}
		if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), body); err != nil {
			return false, err
		}
		result.Replies++
		return true, nil
	}

	switch command.Kind {
	case unparkCommandHelp:
		if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkHelpForPolicy(menu, releasePolicy.enabled)); err != nil {
			return false, err
		}
		result.Replies++
		return true, nil
	case unparkCommandNone:
		// Prose assent, no command. Say what to type; do not act on it.
		if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), unparkProseHintReply); err != nil {
			return false, err
		}
		result.Replies++
		return true, nil
	}

	if releasePolicy.enabled && command.Kind == unparkCommandApprove && menu.releaseStep != nil {
		return c.approveIssueRelease(ctx, displayRepo, owner, repo, issue, candidate, actor, menu.releaseStep, releasePolicy, result, audit)
	}

	labels := issueLabelNames(issue.Labels)
	needsHuman := unparkNeedsHumanOutcome{}
	hasNeedsHuman := c.hasNeedsHumanLabel(labels)
	if hasNeedsHuman {
		needsHuman = c.needsHumanOutcome(ctx, owner, repo, number, nil)
	}
	removed, err := c.clearIssueParkingLabels(ctx, owner, repo, number, labels, actor, needsHuman.lift)
	if err != nil {
		return false, err
	}
	decision := unparkCommandDecisionText(command)
	reply := renderUnparkAcceptedReplyFiltered(actor, decision, removed, hasNeedsHuman, needsHuman, c.unparkIssueFilterNote(labels, removed))
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), reply); err != nil {
		return false, err
	}
	if menu.releaseStep != nil {
		c.rememberIssueOwnerStep(displayRepo, number, nil)
	}
	event := IssueUnparkEvent{
		Repo:       displayRepo,
		Number:     number,
		Actor:      actor,
		Decision:   decision,
		CommentURL: candidate.GetHTMLURL(),
	}
	result.Unparked = append(result.Unparked, event)
	if audit != nil {
		audit(event)
	}
	if c.logger != nil {
		c.logger.Info("issue un-parked by maintainer reply",
			"repo", displayRepo, "issue", number, "actor", actor, "decision", decision)
	}
	return true, nil
}

// tryLabelApproval acts on a trusted acceptance label (hivecommons/hive#11313):
// it adds the approval label, removes the park labels the hive itself added
// before the acceptance, and on a parked issue posts one comment saying what
// changed and what still blocks. Each acceptance is acted on once: the hive's
// own `approved-direction` addition after the acceptance, or its own marker
// comment, is the record. It returns true when it wrote anything.
func (c *Client) tryLabelApproval(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, comments []*gh.IssueComment, la unparkLabelApproval, result *IssueUnparkSweepResult) (bool, error) {
	number := issue.GetNumber()
	labels := issueLabelNames(issue.Labels)
	f, err := c.evaluateLabelApproval(ctx, owner, repo, issue, la.policy)
	if err != nil {
		return false, err
	}
	if !f.Accepted {
		return false, nil
	}
	parked := c.hasParkingLabel(labels)
	if f.Acted || c.labelApprovalAlreadyCommented(comments, f.AcceptanceID) {
		return false, nil
	}
	event := IssueLabelApprovalEvent{Repo: displayRepo, Number: number, Label: f.Label, Actor: f.Actor, TrustReason: f.TrustReason}
	if f.Blocked() {
		// A person's park label keeps the issue out of the queue: no approval,
		// one comment, and nothing more until that label is gone (R13).
		if err := c.postLabelApprovalReply(ctx, owner, repo, number, f.AcceptanceID, renderLabelApprovalPendingReply(f)); err != nil {
			return false, err
		}
		event.Pending = true
		c.recordLabelApproval(event, la, result)
		return true, nil
	}
	if !parked && labelPresent(labels, HumanAckLabel) {
		return false, nil
	}
	for _, label := range f.removableParkLabels() {
		if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
			return false, err
		}
		event.Removed = append(event.Removed, label)
	}
	if !labelPresent(labels, HumanAckLabel) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanAckLabel}); err != nil {
			return false, fmt.Errorf("adding %s to %s/%s#%d: %w", HumanAckLabel, owner, repo, number, err)
		}
		c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
			WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", HumanAckLabel)
	}
	event.Released = f.Released()
	if parked {
		// R8: approval alone, on an issue that was not parked, gets no comment.
		if err := c.postLabelApprovalReply(ctx, owner, repo, number, f.AcceptanceID, renderLabelApprovalReply(f, event.Removed)); err != nil {
			return false, err
		}
	}
	c.recordLabelApproval(event, la, result)
	if c.logger != nil {
		c.logger.Info("issue approved by label", "repo", displayRepo, "issue", number, "label", f.Label, "actor", f.Actor, "trust", f.TrustReason, "removed", strings.Join(event.Removed, ","), "released", event.Released)
	}
	return true, nil
}

func (c *Client) recordLabelApproval(event IssueLabelApprovalEvent, la unparkLabelApproval, result *IssueUnparkSweepResult) {
	result.LabelApproved = append(result.LabelApproved, event)
	if la.audit != nil {
		la.audit(event)
	}
}

func (c *Client) postLabelApprovalReply(ctx context.Context, owner, repo string, number int, eventID int64, body string) error {
	marked := labelApprovalMarker(eventID) + "\n" + body
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(marked)}); err != nil {
		return fmt.Errorf("replying to label approval on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// unparkLabelDrivenCommand answers `/hive approve` / `/hive decision` on a
// label-driven repo with label approval configured (R5): it removes only the
// park labels the hive itself added, adds the approval label unless an
// external needs-human remains, and says which labels it left alone and who
// added them.
func (c *Client) unparkLabelDrivenCommand(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, candidate *gh.IssueComment, command unparkCommand, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	labels := issueLabelNames(issue.Labels)
	events, err := c.listIssueLabelEvents(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	var removed []string
	var kept []string
	var stageLabels []string
	acknowledge := true
	for _, label := range labels {
		if !c.isParkingLabel(label) {
			continue
		}
		ev, ok := lastLabeledEvent(events, label)
		if c.hasNeedsHumanLabel([]string{label}) {
			// A label-driven repo's lifecycle bot owns `needs-human`
			// (hivecommons/hive#10537 R5): only the hive's own lifts here.
			outcome := c.needsHumanOutcome(ctx, owner, repo, number, events)
			if outcome.lift && outcome.applier == stageengine.NeedsHumanApplierHive {
				if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
					return false, err
				}
				removed = append(removed, label)
				continue
			}
			// A retained lifecycle park still needs a person. Adding an ack
			// would contradict it and the stage engine would clear the ack.
			acknowledge = false
			note := fmt.Sprintf("`%s`", label)
			if ok {
				note = labelAttribution{Label: label, Actor: safeGetLogin(ev.Actor)}.String()
			}
			kept = append(kept, note)
			continue
		}
		if !ok || !c.isHiveAccount(ev.Actor) {
			kept = append(kept, labelAttribution{Label: label, Actor: safeGetLogin(ev.Actor)}.String())
			continue
		}
		if stagelabels.IsStageLabel(label) {
			// The stage park label is the stage engine's to lift (below).
			stageLabels = append(stageLabels, label)
			continue
		}
		if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
			return false, err
		}
		removed = append(removed, label)
	}
	// The engine re-derives the stage from the labels left behind, so a kept
	// park label (a person's or a lifecycle bot's `needs-human`) re-parks the
	// issue at the same stage (hivecommons/hive#12715).
	stayingStage := unparkStageThatStays(displayRepo, number, labels, removed, stageLabels)
	if acknowledge && !labelPresent(labels, HumanAckLabel) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanAckLabel}); err != nil {
			return false, fmt.Errorf("adding %s to %s/%s#%d: %w", HumanAckLabel, owner, repo, number, err)
		}
		c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
			WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", HumanAckLabel)
	}
	actor := safeGetLogin(candidate.GetUser())
	if err := c.StageFacts().Unparked(ctx, c.IssueStageRef(owner+"/"+repo, number), actor); err != nil {
		return false, err
	}
	// Only a stage label the issue no longer carries once the engine has
	// run is reported as removed (projectbluefin/website#926).
	var lifting []string
	for _, label := range stageLabels {
		if !strings.EqualFold(label, stayingStage) {
			lifting = append(lifting, label)
		}
	}
	gone, lingering := c.unparkStageLabelsLifted(ctx, owner, repo, number, lifting)
	removed = append(removed, gone...)
	decision := unparkCommandDecisionText(command)
	reply := renderUnparkLabelDrivenAcceptedReply(actor, decision, removed, kept, stayingStage, lingering, acknowledge)
	if note := c.unparkIssueFilterNote(labels, removed); note != "" {
		reply += "\n\n" + note
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), reply); err != nil {
		return false, err
	}
	event := IssueUnparkEvent{Repo: displayRepo, Number: number, Actor: actor, Decision: decision, CommentURL: candidate.GetHTMLURL()}
	result.Unparked = append(result.Unparked, event)
	if audit != nil {
		audit(event)
	}
	return true, nil
}

// unparkStageThatStays returns the stage label among stageLabels that the
// issue still derives once the un-park has removed removed and applied
// approved-direction, or "" when the park lifts.
func unparkStageThatStays(displayRepo string, number int, labels, removed, stageLabels []string) string {
	if len(stageLabels) == 0 {
		return ""
	}
	after := []string{HumanAckLabel}
	for _, label := range labels {
		if !labelPresent(removed, label) && !labelPresent(stageLabels, label) {
			after = append(after, label)
		}
	}
	stage, _, _ := IssueStageFor(IssueStageInput{Repo: displayRepo, Number: number, Labels: after}, nil)
	if stage == stagelabels.None {
		return ""
	}
	for _, label := range stageLabels {
		if strings.EqualFold(label, stage.Label()) {
			return label
		}
	}
	return ""
}

func renderUnparkLabelDrivenAcceptedReply(actor, decision string, removed, kept []string, stayingStage string, lingering []string, acknowledge bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nApproved by @%s. Decision: %s\n\n", unparkAcceptedMarker, actor, decision)
	if acknowledge {
		fmt.Fprintf(&b, "`%s` is applied", HumanAckLabel)
	} else {
		fmt.Fprintf(&b, "`%s` is not applied while `needs-human` remains", HumanAckLabel)
	}
	if len(removed) > 0 {
		fmt.Fprintf(&b, " and Hive's own %s removed", quoteLabels(removed))
	}
	b.WriteString(".")
	if len(kept) > 0 {
		fmt.Fprintf(&b, " Left in place: %s.", strings.Join(kept, ", "))
	}
	if stayingStage != "" {
		fmt.Fprintf(&b, " `%s` stays: a label left on the issue still parks it for a person, so agents will not pick it up until that label is removed.", stayingStage)
	}
	if len(lingering) > 0 {
		fmt.Fprintf(&b, " %s is still on the issue: Hive's stage writer did not take it off, so the issue still reads as parked until it is removed.", quoteLabelsProse(lingering))
	}
	b.WriteString(" Any `hold` label is left exactly as it was.")
	return b.String()
}

// unparkStageLabelsLifted re-reads the issue once the stage engine has run
// on an un-park and splits stageLabels, the stage park labels the issue
// wore, into the ones that came off (gone) and the ones still on it
// (still). A failed read reports neither, so no reply claims anything
// about them.
func (c *Client) unparkStageLabelsLifted(ctx context.Context, owner, repo string, number int, stageLabels []string) (gone, still []string) {
	if len(stageLabels) == 0 {
		return nil, nil
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("un-park: could not re-read the issue's stage labels", "repo", owner+"/"+repo, "issue", number, "error", err)
		}
		return nil, nil
	}
	now := extractLabels(issue.Labels)
	for _, label := range stageLabels {
		if labelPresent(now, label) {
			still = append(still, label)
		} else {
			gone = append(gone, label)
		}
	}
	return gone, still
}

// removeIssueLabel removes one label; a label that has already gone is not an
// error.
func (c *Client) removeIssueLabel(ctx context.Context, owner, repo string, number int, label string) error {
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return nil
		}
		return fmt.Errorf("removing %s from %s/%s#%d: %w", label, owner, repo, number, err)
	}
	return nil
}

func isIssueParkingLabel(label string) bool {
	for _, parking := range issueParkingLabels {
		if strings.EqualFold(strings.TrimSpace(label), parking) {
			return true
		}
	}
	return false
}

// isUnparkKeptLabel reports whether a park label is decided by
// stageengine.DecisionLiftsNeedsHuman rather than always removed:
// `needs-human` is, every other park label is removable outright.
func isUnparkKeptLabel(label string) bool {
	return isIssueParkingLabel(label) && !isUnparkRemovableLabel(label)
}

// unparkNeedsHumanOutcome is what an un-park decided about `needs-human`:
// whether to lift it, who applied it, and the login the reply attributes it
// to when it stays.
type unparkNeedsHumanOutcome struct {
	lift    bool
	applier stageengine.NeedsHumanApplier
	actor   string
}

// needsHumanOutcome classifies who put `needs-human` on the issue — the
// reply attributes the cleared label — and applies
// stageengine.DecisionLiftsNeedsHuman. events, when nil, is read from the
// issue's timeline; a timeline that cannot be read is logged and classified
// unknown.
func (c *Client) needsHumanOutcome(ctx context.Context, owner, repo string, number int, events []issueLabelEvent) unparkNeedsHumanOutcome {
	if events == nil {
		var err error
		if events, err = c.listIssueLabelEvents(ctx, owner, repo, number); err != nil {
			if c.logger != nil {
				c.logger.Warn("unpark: needs-human attribution unavailable", "repo", owner+"/"+repo, "issue", number, "error", err)
			}
			events = nil
		}
	}
	out := unparkNeedsHumanOutcome{applier: stageengine.NeedsHumanApplierUnknown}
	ev, ok := lastLabeledEvent(events, issueNeedsHumanLabel)
	if !ok {
		// The operator may have renamed the needs_human bucket
		// (issue_park_labels.go); the newest add of any such label counts.
		for i := len(events) - 1; i >= 0 && !ok; i-- {
			if events[i].Event == "labeled" && c.hasNeedsHumanLabel([]string{events[i].Label}) {
				ev, ok = events[i], true
			}
		}
	}
	if ok {
		out.actor = safeGetLogin(ev.Actor)
		if c.isHiveAccount(ev.Actor) {
			out.applier = stageengine.NeedsHumanApplierHive
		} else if out.actor != "" {
			out.applier = stageengine.NeedsHumanApplierPerson
		}
	}
	out.lift = stageengine.DecisionLiftsNeedsHuman(out.applier)
	return out
}

func isUnparkRemovableLabel(label string) bool {
	for _, removable := range unparkRemovableLabels {
		if strings.EqualFold(strings.TrimSpace(label), removable) {
			return true
		}
	}
	return false
}

// hasIssueParkingLabel reports whether the issue is parked on a person.
func hasIssueParkingLabel(labels []string) bool {
	if hasIssueNeedsHumanLabel(labels) {
		return true
	}
	for _, label := range labels {
		if isIssueParkingLabel(label) {
			return true
		}
	}
	return false
}

// clearIssueParkingLabels removes the removable (non-stage) parking labels
// the issue actually carries, under the built-in or the configured
// hard_suppress_labels names — and `needs-human` (built-in or the configured
// needs_human bucket) when liftNeedsHuman says the maintainer's command
// answers it (stageengine.DecisionLiftsNeedsHuman, hivecommons/hive#13075,
// hivecommons/docs#419) — adds the approval label, so the resulting PR is
// not held for #5117 and the issue ranks as an agreed direction, and tells
// the stage engine the park lifted (an Unparked fact by actor): the engine
// re-reads the issue, takes the stage/045 park label off and re-derives the
// stage. A kept `needs-human` would leave the issue hard-suppressed and at
// stage/045 after the decision is recorded. A label that has already gone
// is not an error. It returns the labels that came off, spelled as the
// issue carried them — its own removals, then the stage park labels a
// re-read after the engine ran no longer finds — so the reply can name
// exactly what changed.
func (c *Client) clearIssueParkingLabels(ctx context.Context, owner, repo string, number int, labels []string, actor string, liftNeedsHuman bool) ([]string, error) {
	var removed, stageLabels []string
	for _, label := range c.parkingLabelsPresent(labels) {
		if stagelabels.IsStageLabel(label) {
			stageLabels = append(stageLabels, label)
			continue
		}
		if c.hasNeedsHumanLabel([]string{label}) && !liftNeedsHuman {
			continue
		}
		if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil {
			if githubStatusError(err, http.StatusNotFound) {
				continue
			}
			return removed, fmt.Errorf("removing %s from %s/%s#%d: %w", label, owner, repo, number, err)
		}
		removed = append(removed, label)
	}
	if !labelPresent(labels, HumanAckLabel) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanAckLabel}); err != nil {
			return removed, fmt.Errorf("adding %s to %s/%s#%d: %w", HumanAckLabel, owner, repo, number, err)
		}
	}
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
		WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", HumanAckLabel)
	if err := c.StageFacts().Unparked(ctx, c.IssueStageRef(owner+"/"+repo, number), actor); err != nil {
		return removed, err
	}
	gone, _ := c.unparkStageLabelsLifted(ctx, owner, repo, number, stageLabels)
	return append(removed, gone...), nil
}

func labelPresent(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}

// commenterMayUnpark checks the commenter's live repository permission. A
// lookup failure is reported to the caller rather than being read as "allowed":
// the asymmetry is the same one the self-authorization gate uses — not knowing
// is never permission.
//
// Both the legacy `permission` field and `role_name` are read: `role_name`
// is where GitHub reports `maintain` and `triage` (the legacy field collapses
// them to "write" and "read"), while the legacy field still covers custom
// org roles. allowTriage additionally admits the triage role; it is true only
// on label-driven repos with label approval (hivecommons/hive#11313), where
// the people who accept with labels are the people who answer Hive.
func (c *Client) commenterMayUnpark(ctx context.Context, owner, repo, login string, allowTriage bool) (bool, error) {
	if strings.TrimSpace(login) == "" {
		return false, nil
	}
	level, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return false, fmt.Errorf("permission lookup for %s on %s/%s: %w", login, owner, repo, err)
	}
	role := strings.ToLower(strings.TrimSpace(level.GetRoleName()))
	if unparkPermissionLevels[strings.ToLower(strings.TrimSpace(level.GetPermission()))] || unparkPermissionLevels[role] {
		return true, nil
	}
	return allowTriage && role == "triage", nil
}

// answeredUnparkCommentIDs collects the comment IDs the hive has already
// answered, read back from its own replies' markers. Only markers in comments
// the hive itself wrote count (hivecommons/hive#11313): a pasted marker must
// not suppress an answer. A hive that cannot name its own accounts keeps the
// previous any-author rule.
func (c *Client) answeredUnparkCommentIDs(comments []*gh.IssueComment) map[int64]bool {
	answered := map[int64]bool{}
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
			answered[id] = true
		}
	}
	return answered
}

// unparkCandidateLimit bounds the permission lookups one issue can cost in a
// single pass. A maintainer's instruction is always among the last few
// comments; anything older has had cycles to be answered already.
const unparkCandidateLimit = 5

// eligibleUnparkCommand returns the newest unanswered comment that both wants
// an answer — a `/hive` command, or prose assent that deserves the hint — and
// comes from someone allowed to give one. Comments from people without write
// access do not shadow an older command from someone who has it; they are
// skipped in silence so the gate cannot be probed.
//
// An answered `/hive approve` / `/hive decision` / `/fixed` is a watermark:
// every command posted before it is superseded and never replayed, even
// after the issue is parked again (hivecommons/hive#14344). Otherwise a
// retracted older decision would be acted on once the newer one is answered.
func (c *Client) eligibleUnparkCommand(ctx context.Context, owner, repo string, comments []*gh.IssueComment, answered map[int64]bool, cutoff time.Time, allowTriage bool) (*gh.IssueComment, unparkCommand, error) {
	checked := 0
	for i := len(comments) - 1; i >= 0 && checked < unparkCandidateLimit; i-- {
		cm := comments[i]
		if cm == nil {
			continue
		}
		if answered[cm.GetID()] {
			if isUnparkWatermarkCommand(cm.GetBody()) {
				break
			}
			continue
		}
		created := cm.GetCreatedAt().Time
		if created.Before(cutoff) {
			continue
		}
		// An edit to an existing comment is not a new instruction.
		if updated := cm.GetUpdatedAt().Time; updated.After(created) {
			continue
		}
		cmd, isCommand := parseUnparkCommand(cm.GetBody())
		if !isCommand && !isUnparkProseAssent(cm.GetBody()) {
			continue
		}
		if !c.isHumanAuthor(cm.GetUser()) {
			if isCommand && c.isAIAuthorUser(cm.GetUser()) {
				return cm, unparkCommand{Kind: unparkCommandRefusedAIAuthor}, nil
			}
			continue
		}
		if isHiveSelfDecisionComment(cm.GetBody()) {
			continue
		}
		checked++
		allowed, err := c.commenterMayUnpark(ctx, owner, repo, safeGetLogin(cm.GetUser()), allowTriage)
		if err != nil {
			return nil, unparkCommand{}, err
		}
		if !allowed {
			continue
		}
		return cm, cmd, nil
	}
	return nil, unparkCommand{}, nil
}

// isAIAuthorUser reports whether isHumanAuthor rejected the user only because
// the login is the configured project.ai_author: a user account, not a bot and
// not the App's own bot login.
func (c *Client) isAIAuthorUser(user *gh.User) bool {
	if c == nil {
		return false
	}
	login := strings.TrimSpace(user.GetLogin())
	if login == "" {
		return false
	}
	if strings.EqualFold(user.GetType(), "Bot") || strings.HasSuffix(login, "[bot]") {
		return false
	}
	if c.appBotLogin != "" && strings.EqualFold(login, c.appBotLogin) {
		return false
	}
	return c.getHiveIdentity().Matches(login)
}

// renderUnparkAIAuthorRefusal explains why a `/hive` command from the
// project.ai_author account was not acted on.
func renderUnparkAIAuthorRefusal(login string) string {
	return "@" + login + " is configured as this hive's `project.ai_author`, so Hive treats comments from it as its own and will not act on `/hive` commands from it. " +
		"To un-park this issue, remove the parking label by hand, or set `project.ai_author` to an account that is not a maintainer (clear it to author as the GitHub App bot)."
}

// isUnparkWatermarkCommand reports whether an answered comment carried a
// command that acted on the park state, so nothing older may be replayed.
func isUnparkWatermarkCommand(body string) bool {
	cmd, ok := parseUnparkCommand(body)
	if !ok {
		return false
	}
	switch cmd.Kind {
	case unparkCommandApprove, unparkCommandDecision, unparkCommandFixed:
		return true
	}
	return false
}

// parseUnparkCommand reads a `/hive …` command — or `/fixed` / `/close`
// (unparkCommandFixed) — off the first line of a comment. The first line
// only, and never inside a quote or a fence, so quoting someone else's
// command cannot execute it.
func parseUnparkCommand(body string) (unparkCommand, bool) {
	line := firstNonEmptyLine(body)
	if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
		return unparkCommand{}, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return unparkCommand{}, false
	}
	for _, token := range fixedCommandTokens {
		if strings.EqualFold(fields[0], token) {
			return unparkCommand{Kind: unparkCommandFixed, Decision: strings.TrimSpace(strings.Join(fields[1:], " "))}, true
		}
	}
	if len(fields) < 2 || !strings.EqualFold(fields[0], unparkCommandPrefix) {
		return unparkCommand{}, false
	}
	switch strings.ToLower(fields[1]) {
	case string(unparkCommandApprove):
		return unparkCommand{Kind: unparkCommandApprove}, true
	case string(unparkCommandHelp):
		return unparkCommand{Kind: unparkCommandHelp}, true
	case string(unparkCommandDone):
		return unparkCommand{Kind: unparkCommandDone}, true
	case string(unparkCommandDecision):
		decision := strings.TrimSpace(strings.Join(fields[2:], " "))
		if decision == "" {
			return unparkCommand{}, false
		}
		return unparkCommand{Kind: unparkCommandDecision, Decision: decision, Text: decisionText(body, decision)}, true
	}
	return unparkCommand{}, false
}

// decisionText joins the first-line decision with every line that follows
// the command line, so numbered instructions under `/hive decision approve`
// reach the author agent instead of being dropped (hivecommons/hive#14679).
func decisionText(body, firstLine string) string {
	lines := strings.Split(body, "\n")
	rest := make([]string, 0, len(lines))
	seen := false
	for _, line := range lines {
		line = strings.TrimRight(strings.TrimSuffix(line, "\r"), " \t")
		if !seen {
			if strings.TrimSpace(line) == "" {
				continue
			}
			seen = true
			continue
		}
		rest = append(rest, line)
	}
	tail := strings.TrimSpace(strings.Join(rest, "\n"))
	if tail == "" {
		return firstLine
	}
	return strings.TrimSpace(firstLine + "\n\n" + tail)
}

// firstNonEmptyLine returns the comment's first line that has content,
// trimmed of surrounding whitespace.
func firstNonEmptyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line != "" {
			return line
		}
	}
	return ""
}

// unparkProsePhrases are the ways a maintainer says yes without a command.
var unparkProsePhrases = []string{
	"approved", "approve this", "go ahead", "go with", "lgtm",
	"sounds good", "yes do it", "please proceed", "ship it",
}

// isUnparkProseAssent reports whether a comment reads as assent but carries no
// command. It decides only whether to print a hint; it never un-parks anything.
func isUnparkProseAssent(body string) bool {
	line := strings.ToLower(firstNonEmptyLine(body))
	// Any slash command is a command, parsed or not, never prose.
	if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "/") {
		return false
	}
	for _, phrase := range unparkProsePhrases {
		if strings.Contains(line, phrase) {
			return true
		}
	}
	return false
}

const unparkProseHintReply = "To start work, reply `/hive approve` (or `/hive decision A`). A plain reply acknowledges the issue but leaves it parked."

// renderUnparkLabelDrivenReply answers `/hive approve`, `/hive decision`,
// `/hive done` and `/hive help` on a repo with
// project.repo_policies[].label_driven set. It names the parking labels the
// issue carries, built-in or renamed, so the operator reads the labels their
// own repository uses.
func renderUnparkLabelDrivenReply(parking []string) string {
	if len(parking) == 0 {
		parking = []string{issueNeedsHumanLabel, NeedsDecisionStageLabel}
	}
	return fmt.Sprintf("This repository is label-driven: Hive leaves %s to maintainers, so accept or un-park this issue with the repository's triage labels instead of `/hive approve` / `/hive decision` / `/hive done`. No labels were changed.", quoteLabelsProse(parking))
}

// quoteLabelsProse renders labels as "`a`, `b` and `c`" so the reply reads as
// a sentence rather than a list.
func quoteLabelsProse(labels []string) string {
	if len(labels) <= 1 {
		return quoteLabels(labels)
	}
	return quoteLabels(labels[:len(labels)-1]) + " and `" + labels[len(labels)-1] + "`"
}

// renderUnparkAcceptedReply records an accepted command. It names the labels
// actually removed (built-in or the operator's renamed ones) rather than the
// built-in names, and carries unparkAcceptedMarker so the menu knows an owner
// step was answered.
func renderUnparkAcceptedReply(actor, decision string, removed []string) string {
	return renderUnparkAcceptedReplyKeeping(actor, decision, removed, false, unparkNeedsHumanOutcome{})
}

// renderUnparkAcceptedReplyKeeping is renderUnparkAcceptedReply plus, when
// needsHuman is set, what happened to `needs-human`: it lifts when Hive
// itself asked for a person (hivecommons/hive#13075) and stays, with the
// applier named, when a person added it or the timeline could not say
// (hivecommons/hive#12680).
func renderUnparkAcceptedReplyKeeping(actor, decision string, removed []string, needsHuman bool, outcome unparkNeedsHumanOutcome) string {
	return renderUnparkAcceptedReplyFiltered(actor, decision, removed, needsHuman, outcome, "")
}

// renderUnparkAcceptedReplyFiltered is renderUnparkAcceptedReplyKeeping plus
// filterNote (unparkIssueFilterNote): when project.issue_filter still refuses
// the issue, a lifted `needs-human` does not send it to the agent queue, and
// the reply says what will.
func renderUnparkAcceptedReplyFiltered(actor, decision string, removed []string, needsHuman bool, outcome unparkNeedsHumanOutcome, filterNote string) string {
	queued := "so the issue returns to the agent queue"
	if filterNote != "" {
		queued = "so it no longer holds the issue"
	}
	cleared := "No parking label was left to remove"
	if len(removed) > 0 {
		cleared = quoteLabels(removed) + " removed"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nUn-parked by @%s. Decision: %s\n\n%s and `%s` is applied; any `hold` label is left exactly as it was.",
		unparkAcceptedMarker, actor, decision, cleared, HumanAckLabel)
	switch {
	case !needsHuman:
	case !outcome.lift && outcome.applier == stageengine.NeedsHumanApplierPerson:
		fmt.Fprintf(&b, " `%s` stays (added by @%s): this issue is held for a person and remains out of the agent queue until a person removes that label.", issueNeedsHumanLabel, outcome.actor)
	case !outcome.lift:
		fmt.Fprintf(&b, " `%s` stays (Hive could not read who added it): this issue is held for a person and remains out of the agent queue until a person removes that label.", issueNeedsHumanLabel)
	case outcome.applier == stageengine.NeedsHumanApplierPerson:
		fmt.Fprintf(&b, " `%s` cleared (added by @%s): your command is the human input it waited for, %s; add `hold` to keep agents off it.", issueNeedsHumanLabel, outcome.actor, queued)
	default:
		fmt.Fprintf(&b, " `%s` cleared: your command is the human input it waited for, %s; add `hold` to keep agents off it.", issueNeedsHumanLabel, queued)
	}
	if filterNote != "" {
		b.WriteString("\n\n" + filterNote)
	}
	return b.String()
}

// unparkIssueFilterNote is the reply paragraph for an un-park that leaves
// the issue refused by project.issue_filter (require_labels). Approval
// lifts the park, but only a required label admits the issue to agent
// work, and the hive does not add one on a comment's say-so; without this
// the reply read as "accepted" while the issue stayed out of the queue.
// labels are the issue's labels before the un-park and removed the ones it
// took off; `approved-direction` is counted as applied. "" when the filter
// admits the result.
func (c *Client) unparkIssueFilterNote(labels, removed []string) string {
	after := []string{HumanAckLabel}
	for _, l := range labels {
		if !labelPresent(removed, l) {
			after = append(after, l)
		}
	}
	filter := c.getIssueFilter()
	if filter.Admits(after) {
		return ""
	}
	var required []string
	if notice, ok := filter.(IssueFilterNoticeConfig); ok {
		required = notice.RequireLabelsForNotice()
	}
	const lead = "This issue is still outside the agent queue: `project.issue_filter` admits "
	switch len(required) {
	case 0:
		return lead + "only issues carrying one of its required labels. A maintainer adding one admits it."
	case 1:
		return lead + "only issues labelled " + quoteLabels(required) + ". A maintainer adding it admits this one."
	default:
		return lead + "only issues carrying one of " + quoteLabels(required) + ". A maintainer adding one admits this one."
	}
}

func renderUnparkHelpReply(issueBody string) string {
	return renderUnparkHelpFor(unparkMenuFromBody(issueBody))
}

func renderUnparkHelpFor(menu unparkMenu) string {
	return renderUnparkHelpForPolicy(menu, false)
}

func renderUnparkHelpForPolicy(menu unparkMenu, releaseOnApprove bool) string {
	return "**Maintainer commands**\n" + menu.sourceLine() + menu.bulletsFor(releaseOnApprove) + "\n" + maintainerCommandReference()
}

// unparkNoticeBody is the "What to reply" comment kept on every parked issue.
func unparkNoticeBody(issueBody string) string {
	return renderUnparkNotice(unparkMenuFromBody(issueBody))
}

// renderUnparkNotice renders the default-off "What to reply" comment.
func renderUnparkNotice(menu unparkMenu) string {
	return renderUnparkNoticeForPolicy(menu, false)
}

func renderUnparkNoticeForPolicy(menu unparkMenu, releaseOnApprove bool) string {
	return unparkNoticeMarker + "\n**What to reply**\n" + menu.sourceLine() + menu.bulletsFor(releaseOnApprove) +
		"\nIf what this issue waited on has been done (a merged fix, or an action taken outside this repository), a maintainer can reply `/fixed` to close it as completed; Hive clears its park labels.\n\n" +
		fmt.Sprintf("_Only a maintainer with write access can un-park; everything else is ignored. Full command reference: %s._", maintainerCommandsDocURL)
}

func maintainerCommandReference() string {
	return strings.Join([]string{
		"Other commands:",
		"- `/fixed`: close an open issue after the reporter or a maintainer confirms the fix; on a parked issue a maintainer's `/fixed` also clears Hive's park labels.",
		"- `/reopen`: reopen a closed issue; limited to the reporter or write/maintain/admin.",
		"- `/help-wanted`: add the `help wanted` label.",
		"- `/good-first-issue`: add the `good first issue` label.",
		"- `/hacktober-fest`: add the `hacktober-fest` label.",
		"- `/kind <kind>`: validate or request a kind label.",
		"- `/area <area>`: validate or request an area label.",
		"- `/assign`: assign yourself where the repository's assignment automation supports it.",
		"- `/unassign`: remove your assignment where the repository's assignment automation supports it.",
		fmt.Sprintf("\nFull reference: %s", maintainerCommandsDocURL),
	}, "\n")
}

// unparkOptionLabels finds the lettered options an issue offers, e.g. a line
// starting "Option B:" or "- **C.**". Only A-F, in the order they appear, and
// each one once.
func unparkOptionLabels(issueBody string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(issueBody, "\n") {
		letter := unparkOptionLetter(line)
		if letter == "" || seen[letter] {
			continue
		}
		seen[letter] = true
		out = append(out, letter)
	}
	return out
}

// unparkOptionTexts maps each option letter to the one-line summary on the
// line that first defines it. Letters with no summary are left out.
func unparkOptionTexts(text string) map[string]string {
	out := map[string]string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		letter := unparkOptionLetter(line)
		if letter == "" || seen[letter] {
			continue
		}
		seen[letter] = true
		if summary := unparkOptionSummary(line); summary != "" {
			out[letter] = summary
		}
	}
	return out
}

const unparkOptionSummaryMax = 160

// unparkHTMLCommentPattern matches an HTML comment, including an unclosed
// one running to the end of the line, so a body cannot smuggle a hive marker
// into the notice.
var unparkHTMLCommentPattern = regexp.MustCompile(`<!--.*?(-->|$)`)

// unparkOptionSummary returns the text after the option marker on line,
// whitespace-collapsed and cut to unparkOptionSummaryMax runes. The issue body
// is untrusted: HTML comments are removed so no hive marker can be laundered
// into the hive-authored notice, and @mentions are defanged so quoting the
// option notifies nobody.
//
// Invariant: the byte offsets below mirror unparkOptionLetter and
// unparkBoldOptionLetter, which have already validated the line's shape by
// the time the guard on the first line passes. "Option X" lines yield a rest
// starting at the single-byte letter; bold lines are "**X" after trimming
// list markers, so b[2:] is the letter and rest[1:] skips it. Change those
// parsers and this slicing together.
func unparkOptionSummary(line string) string {
	if unparkOptionLetter(line) == "" {
		return ""
	}
	t := strings.TrimLeft(strings.TrimSpace(line), "-*# \t")
	t = strings.TrimPrefix(t, "**")
	var rest string
	if strings.HasPrefix(strings.ToLower(t), "option ") {
		rest = strings.TrimSpace(t[len("option "):])
	} else {
		b := strings.TrimLeft(strings.TrimSpace(line), "-+ \t")
		rest = b[2:]
	}
	rest = rest[1:]
	rest = strings.TrimLeftFunc(rest, func(r rune) bool { return strings.ContainsRune(" \t*.:)—–-", r) })
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSpace(strings.TrimSuffix(rest, "**"))
	rest = unparkHTMLCommentPattern.ReplaceAllString(rest, " ")
	rest = defangMentions(rest)
	rest = strings.Join(strings.Fields(rest), " ")
	if r := []rune(rest); len(r) > unparkOptionSummaryMax {
		rest = strings.TrimSpace(string(r[:unparkOptionSummaryMax-2])) + " …"
	}
	return rest
}

func unparkOptionLetter(line string) string {
	trimmed := strings.TrimLeft(strings.TrimSpace(line), "-*# \t")
	trimmed = strings.TrimPrefix(trimmed, "**")
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "option ") {
		return unparkBoldOptionLetter(line)
	}
	rest := strings.TrimSpace(trimmed[len("option "):])
	if rest == "" {
		return ""
	}
	letter := strings.ToUpper(rest[:1])
	if letter < "A" || letter > "F" {
		return ""
	}
	if len(rest) > 1 && !strings.ContainsAny(rest[1:2], ".:) *—-") {
		return ""
	}
	return letter
}

// ensureUnparkNoticeComment posts the "What to reply" block once per parked
// issue and edits it in place when the issue's options change, so a maintainer
// reading the bottom of the issue always sees the current commands. Options
// and owner-only steps the hive posted in comments count as well as the body.
func (c *Client) ensureUnparkNoticeComment(ctx context.Context, owner, repo string, number int, issueBody string, issueAuthor *gh.User, comments []*gh.IssueComment, releaseOnApprove bool) error {
	desired := renderUnparkNoticeForPolicy(c.unparkMenuFor(issueBody, issueAuthor, comments), releaseOnApprove)
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), unparkNoticeMarker) {
			continue
		}
		if cm.GetBody() == desired {
			return nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(desired)}); err != nil {
			return fmt.Errorf("editing unpark notice on %s/%s#%d: %w", owner, repo, number, err)
		}
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(desired)}); err != nil {
		return fmt.Errorf("creating unpark notice on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// postUnparkReply answers one command comment exactly once; the marker carries
// the answered comment's ID so a later cycle recognises its own work.
func (c *Client) postUnparkReply(ctx context.Context, owner, repo string, number int, commentID int64, body string) error {
	marked := fmt.Sprintf("%s%d -->\n%s", unparkReplyMarkerPrefix, commentID, body)
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(marked)}); err != nil {
		return fmt.Errorf("replying to unpark command on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// closeParkedIssueAsFixed answers `/fixed` (alias `/close`) from a
// maintainer with write access on a parked issue (hivecommons/dibs#459):
// the hive's own non-stage park labels come off (`needs-human` and the
// retired decision spellings; on a label-driven repo only the stage label,
// which is the hive's whichever way the repo triages), the issue closes as
// completed, the Closed fact has the stage engine take every stage/* label
// off, and one marked reply says what happened. `hold` is never touched.
// The reporter-confirmation gate is overridden explicitly: a maintainer
// confirming the fix is the confirmation it asks for.
func (c *Client) closeParkedIssueAsFixed(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, candidate *gh.IssueComment, command unparkCommand, labelDriven bool, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	actor := safeGetLogin(candidate.GetUser())
	labels := issueLabelNames(issue.Labels)
	var removed []string
	// needs-human lifts whoever applied it (stageengine.DecisionLiftsNeedsHuman),
	// except on a label-driven repo, whose lifecycle bot owns it along with
	// the retired park spellings: there only the hive's own comes off.
	for _, label := range c.parkingLabelsPresent(labels) {
		if stagelabels.IsStageLabel(label) {
			continue
		}
		if labelDriven {
			if !c.hasNeedsHumanLabel([]string{label}) || c.needsHumanOutcome(ctx, owner, repo, number, nil).applier != stageengine.NeedsHumanApplierHive {
				continue
			}
		}
		if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
			return false, err
		}
		removed = append(removed, label)
	}
	for _, label := range labels {
		if stagelabels.IsStageLabel(strings.ToLower(strings.TrimSpace(label))) {
			removed = append(removed, label)
		}
	}
	note := strings.TrimSpace(command.Decision)
	override := fmt.Sprintf("maintainer @%s confirmed the fix with /fixed", actor)
	if err := c.CloseIssue(ctx, owner+"/"+repo, number, IssueCloseOptions{OverrideReason: override, SuppressOverrideComment: true, StateReason: IssueStateReasonCompleted}); err != nil {
		return false, err
	}
	if err := c.StageFacts().Closed(ctx, c.IssueStageRef(owner+"/"+repo, number)); err != nil {
		if c.logger != nil {
			c.logger.Warn("unpark: stage labels not cleared after /fixed close", "repo", displayRepo, "issue", number, "error", err)
		}
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkFixedReply(actor, note, removed)); err != nil {
		return false, err
	}
	event := IssueUnparkEvent{Repo: displayRepo, Number: number, Actor: actor, Decision: note, CommentURL: candidate.GetHTMLURL()}
	result.Closed = append(result.Closed, event)
	if audit != nil {
		audit(event)
	}
	if c.logger != nil {
		c.logger.Info("parked issue closed by maintainer /fixed", "repo", displayRepo, "issue", number, "actor", actor, "note", note)
	}
	return true, nil
}

func renderUnparkFixedReply(actor, note string, removed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Closed as completed: @%s confirmed with `/fixed`", actor)
	if note != "" {
		fmt.Fprintf(&b, " — %s", note)
	}
	b.WriteString(".")
	if len(removed) > 0 {
		fmt.Fprintf(&b, " Hive's park labels %s are cleared", quoteLabels(removed))
	} else {
		b.WriteString(" Hive's park labels are cleared")
	}
	b.WriteString("; any `hold` label is left exactly as it was.\n\nIf this was closed by mistake, the reporter or a maintainer can comment `/reopen`.")
	return b.String()
}
