package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// issueParkingLabels are the labels that park an issue on a person and so
// make it the un-park sweep's business: `needs-human`, the park signal
// `stage/045-needs-decision` (hivecommons/hive#12683), and the retired
// spellings — the pre-045 `stage/085-needs-decision`, `needs-decision`,
// `needs-direction` — until the stage reconciler has migrated them. Only unparkRemovableLabels are ever cleared by a /hive
// command. `hold` is deliberately untouched because it has several
// independent sources (the ACMM level gate, the SHA hold, holdguard and a
// person), and clearing one of them from a comment would silently clear all
// of them.
var issueParkingLabels = []string{issueNeedsHumanLabel, NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel}

// unparkRemovableLabels are the park labels `/hive approve` / `/hive
// decision` (and label approval) remove. `needs-human` is NOT among them:
// it says a human-held identity or consent is required for the work itself
// — a signal orthogonal to the decision state — so a decision never lifts
// it. The incident on hivecommons/hive#12680 / #12681 (2026-10-17) had the
// sweep clear it on `/hive decision … Agents stand down`, which dropped both
// issues straight back into the agent queue. Only a person removes it.
var unparkRemovableLabels = []string{NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel}

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

// unparkCommandKind is which of the three commands a comment carries.
type unparkCommandKind string

const (
	unparkCommandNone     unparkCommandKind = ""
	unparkCommandApprove  unparkCommandKind = "approve"
	unparkCommandDecision unparkCommandKind = "decision"
	unparkCommandHelp     unparkCommandKind = "help"
)

// unparkCommand is a parsed `/hive …` comment.
type unparkCommand struct {
	Kind     unparkCommandKind
	Decision string
}

// IssueUnparkEvent records one issue this sweep un-parked, for the audit log.
type IssueUnparkEvent struct {
	Repo       string
	Number     int
	Actor      string
	Decision   string
	CommentURL string
}

// IssueUnparkSweepOptions configures SweepIssueUnparkCommands.
type IssueUnparkSweepOptions struct {
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
	// LabelApproval resolves label-driven approval (hivecommons/hive#11313)
	// for a repo. It is consulted only for repos LabelDriven reports true,
	// so a repo that did not opt in never reaches it. nil falls back to the
	// resolver installed with SetLabelApprovalPolicy; with neither, the
	// label-driven sweep behaves exactly as before #11313.
	LabelApproval func(repo string) (LabelApprovalPolicy, bool)
	// LabelApprovalAudit, when set, is called once per acceptance acted on.
	LabelApprovalAudit func(IssueLabelApprovalEvent)
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
	// Skipped counts parked issues that carried no actionable command.
	Skipped int
	// Unparked lists the issues whose parking labels were cleared.
	Unparked []IssueUnparkEvent
	// Replies counts answers posted that did not un-park: `/hive help`, and
	// the hint left when a maintainer approves in prose.
	Replies int
	// LabelApproved lists the acceptances acted on via a configured approval
	// label on label-driven repos (hivecommons/hive#11313).
	LabelApproved []IssueLabelApprovalEvent
}

func (r *IssueUnparkSweepResult) actions() int {
	return len(r.Unparked) + r.Replies + len(r.LabelApproved)
}

// SweepIssueUnparkCommands lets a maintainer re-queue a parked issue with a
// one-line reply instead of editing labels (hivecommons/hive#9879).
//
// On every parked issue (`needs-human` or `stage/045-needs-decision`) the
// sweep keeps a "What to reply" comment listing the commands, because the
// maintainer should never have to remember what to type. When an eligible
// person replies:
//
//   - `/hive approve` — go with the issue's own recommendation;
//   - `/hive decision <text>` — go ahead, following <text>;
//   - `/hive help` — repeat the commands, change nothing.
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
	cutoff := time.Now().Add(-unparkCommandScanWindow)

	for _, repo := range c.getRepos() {
		if result.actions() >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		labelDriven := opts.LabelDriven != nil && opts.LabelDriven(repo)
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
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			labels := issueLabelNames(issue.Labels)
			parked := hasIssueParkingLabel(labels)
			if !parked && !(labelApproval && hasLabelApprovalCandidate(labels, policy)) {
				continue
			}
			result.Seen++
			acted, err := c.tryUnparkIssue(ctx, repo, owner, repoName, issue, cutoff, labelDriven, unparkLabelApproval{policy: policy, enabled: labelApproval, audit: opts.LabelApprovalAudit}, result, opts.Audit)
			if err != nil {
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

// tryUnparkIssue handles one parked issue: keep its "What to reply" comment
// current, then answer the newest eligible command on it. On a label-driven
// repo there is no notice; without label approval configured there is no
// label change either: commands get a pointer to the labels and prose assent
// gets no hint. With label approval (hivecommons/hive#11313) a trusted
// acceptance label is acted on first, and `/hive approve` / `/hive decision`
// remove only the park labels the hive added itself.
func (c *Client) tryUnparkIssue(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, labelDriven bool, la unparkLabelApproval, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if !labelDriven {
		if err := c.ensureUnparkNoticeComment(ctx, owner, repo, number, issue.GetBody(), comments); err != nil {
			return false, err
		}
	}

	if la.enabled {
		acted, err := c.tryLabelApproval(ctx, displayRepo, owner, repo, issue, comments, la, result)
		if err != nil || acted {
			return acted, err
		}
	}
	if !hasIssueParkingLabel(issueLabelNames(issue.Labels)) {
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

	if labelDriven {
		body := unparkLabelDrivenReply
		switch command.Kind {
		case unparkCommandNone:
			return false, nil
		case unparkCommandHelp:
			body = unparkLabelDrivenReply + "\n\n" + maintainerCommandReference()
		case unparkCommandApprove, unparkCommandDecision:
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
		if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkHelpReply(issue.GetBody())); err != nil {
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

	labels := issueLabelNames(issue.Labels)
	if err := c.clearIssueParkingLabels(ctx, owner, repo, number, labels); err != nil {
		return false, err
	}
	decision := strings.TrimSpace(command.Decision)
	if decision == "" {
		decision = "go with the recommendation"
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkAcceptedReply(actor, decision, hasIssueNeedsHumanLabel(labels))); err != nil {
		return false, err
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
	parked := hasIssueParkingLabel(labels)
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
	for _, label := range f.HiveParkLabels {
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
// park labels the hive itself added, adds the approval label, and says which
// labels it left alone and who added them.
func (c *Client) unparkLabelDrivenCommand(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, candidate *gh.IssueComment, command unparkCommand, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	labels := issueLabelNames(issue.Labels)
	events, err := c.listIssueLabelEvents(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	var removed []string
	var kept []string
	for _, label := range labels {
		if !isIssueParkingLabel(label) {
			continue
		}
		ev, ok := lastLabeledEvent(events, label)
		if isUnparkKeptLabel(label) {
			// needs-human stays whoever added it: a decision never lifts
			// a hold for a person (hivecommons/hive#12680).
			note := fmt.Sprintf("`%s`", label)
			if ok {
				note = labelAttribution{Label: label, Actor: safeGetLogin(ev.Actor)}.String()
			}
			kept = append(kept, note+" — a decision never lifts it")
			continue
		}
		if !ok || !c.isHiveAccount(ev.Actor) {
			kept = append(kept, labelAttribution{Label: label, Actor: safeGetLogin(ev.Actor)}.String())
			continue
		}
		if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
			return false, err
		}
		removed = append(removed, label)
	}
	if !labelPresent(labels, HumanAckLabel) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanAckLabel}); err != nil {
			return false, fmt.Errorf("adding %s to %s/%s#%d: %w", HumanAckLabel, owner, repo, number, err)
		}
		c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
			WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", HumanAckLabel)
	}
	actor := safeGetLogin(candidate.GetUser())
	decision := strings.TrimSpace(command.Decision)
	if decision == "" {
		decision = "go with the recommendation"
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkLabelDrivenAcceptedReply(actor, decision, removed, kept)); err != nil {
		return false, err
	}
	event := IssueUnparkEvent{Repo: displayRepo, Number: number, Actor: actor, Decision: decision, CommentURL: candidate.GetHTMLURL()}
	result.Unparked = append(result.Unparked, event)
	if audit != nil {
		audit(event)
	}
	return true, nil
}

func renderUnparkLabelDrivenAcceptedReply(actor, decision string, removed, kept []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approved by @%s. Decision: %s\n\n`%s` is applied", actor, decision, HumanAckLabel)
	if len(removed) > 0 {
		fmt.Fprintf(&b, " and Hive's own %s removed", quoteLabels(removed))
	}
	b.WriteString(".")
	if len(kept) > 0 {
		fmt.Fprintf(&b, " Left in place: %s.", strings.Join(kept, ", "))
	}
	b.WriteString(" Any `hold` label is left exactly as it was.")
	return b.String()
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

// isUnparkKeptLabel reports whether a park label survives an un-park:
// `needs-human` does, every other park label is removable.
func isUnparkKeptLabel(label string) bool {
	return isIssueParkingLabel(label) && !isUnparkRemovableLabel(label)
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

// clearIssueParkingLabels removes the removable parking labels the issue
// actually carries and adds the approval label, so the resulting PR is not
// held for #5117 and the issue ranks as an agreed direction. `needs-human`
// stays (unparkRemovableLabels), so an issue carrying it remains
// hard-suppressed and at stage/045 after the decision is recorded. A label
// that has already gone is not an error.
func (c *Client) clearIssueParkingLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	for _, label := range unparkRemovableLabels {
		if !labelPresent(labels, label) {
			continue
		}
		if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil {
			if githubStatusError(err, http.StatusNotFound) {
				continue
			}
			return fmt.Errorf("removing %s from %s/%s#%d: %w", label, owner, repo, number, err)
		}
	}
	if !labelPresent(labels, HumanAckLabel) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{HumanAckLabel}); err != nil {
			return fmt.Errorf("adding %s to %s/%s#%d: %w", HumanAckLabel, owner, repo, number, err)
		}
	}
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
		WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", HumanAckLabel)
	return nil
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
func (c *Client) eligibleUnparkCommand(ctx context.Context, owner, repo string, comments []*gh.IssueComment, answered map[int64]bool, cutoff time.Time, allowTriage bool) (*gh.IssueComment, unparkCommand, error) {
	checked := 0
	for i := len(comments) - 1; i >= 0 && checked < unparkCandidateLimit; i-- {
		cm := comments[i]
		if cm == nil || answered[cm.GetID()] {
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

// parseUnparkCommand reads a `/hive …` command off the first line of a comment.
// The first line only, and never inside a quote or a fence, so quoting someone
// else's command cannot execute it.
func parseUnparkCommand(body string) (unparkCommand, bool) {
	line := firstNonEmptyLine(body)
	if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
		return unparkCommand{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[0], unparkCommandPrefix) {
		return unparkCommand{}, false
	}
	switch strings.ToLower(fields[1]) {
	case string(unparkCommandApprove):
		return unparkCommand{Kind: unparkCommandApprove}, true
	case string(unparkCommandHelp):
		return unparkCommand{Kind: unparkCommandHelp}, true
	case string(unparkCommandDecision):
		decision := strings.TrimSpace(strings.Join(fields[2:], " "))
		if decision == "" {
			return unparkCommand{}, false
		}
		return unparkCommand{Kind: unparkCommandDecision, Decision: decision}, true
	}
	return unparkCommand{}, false
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
	if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, unparkCommandPrefix) {
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

// unparkLabelDrivenReply answers `/hive approve`, `/hive decision` and
// `/hive help` on a repo with project.repo_policies[].label_driven set.
const unparkLabelDrivenReply = "This repository is label-driven: Hive leaves `needs-human` and `stage/045-needs-decision` to maintainers, so accept or un-park this issue with the repository's triage labels instead of `/hive approve` / `/hive decision`. No labels were changed."

func renderUnparkAcceptedReply(actor, decision string, needsHuman bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Un-parked by @%s. Decision: %s\n\n`%s` is cleared when present and `%s` is applied; any `hold` label is left exactly as it was.",
		actor, decision, NeedsDecisionStageLabel, HumanAckLabel)
	if needsHuman {
		fmt.Fprintf(&b, " `%s` stays: this issue is held for a person and remains out of the agent queue until a person removes that label.", issueNeedsHumanLabel)
	}
	return b.String()
}

func renderUnparkHelpReply(issueBody string) string {
	return "**Maintainer commands**\n" + unparkCommandBullets(issueBody) + "\n" + maintainerCommandReference()
}

// unparkNoticeBody is the "What to reply" comment kept on every parked issue.
func unparkNoticeBody(issueBody string) string {
	return unparkNoticeMarker + "\n**What to reply**\n" + unparkCommandBullets(issueBody) +
		"\nIf this issue is waiting only for reporter confirmation after a merged fix, the reporter or a maintainer can reply `/fixed` instead.\n\n" +
		fmt.Sprintf("_Only a maintainer with write access can un-park; everything else is ignored. Full command reference: %s._", maintainerCommandsDocURL)
}

// unparkCommandBullets lists the commands, filled in with the options this
// issue offers when its body presents lettered ones.
func unparkCommandBullets(issueBody string) string {
	var b strings.Builder
	options := unparkOptionLabels(issueBody)
	if len(options) > 0 {
		fmt.Fprintf(&b, "- `/hive approve`: go with the recommendation (option %s)\n", options[0])
		for _, opt := range options[1:] {
			fmt.Fprintf(&b, "- `/hive decision %s`: go with option %s\n", opt, opt)
		}
	} else {
		fmt.Fprintln(&b, "- `/hive approve`: go with the recommendation")
		fmt.Fprintln(&b, "- `/hive decision <your instructions>`: go ahead, following your instructions")
	}
	fmt.Fprintln(&b, "- `/hive help`: show these again")
	return b.String()
}

func maintainerCommandReference() string {
	return strings.Join([]string{
		"Other commands:",
		"- `/fixed`: close an open issue after the reporter or a maintainer confirms the fix.",
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

func unparkOptionLetter(line string) string {
	trimmed := strings.TrimLeft(strings.TrimSpace(line), "-*# \t")
	trimmed = strings.TrimPrefix(trimmed, "**")
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "option ") {
		return ""
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
// reading the bottom of the issue always sees the current commands.
func (c *Client) ensureUnparkNoticeComment(ctx context.Context, owner, repo string, number int, issueBody string, comments []*gh.IssueComment) error {
	desired := unparkNoticeBody(issueBody)
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
