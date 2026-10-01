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

// issueNeedsDecisionLabel is the second parking label a /hive command clears.
// `needs-human` and `needs-decision` are the only labels this sweep removes:
// `hold` is deliberately untouched because it has several independent sources
// (the ACMM level gate, the SHA hold, holdguard and a person), and clearing one
// of them from a comment would silently clear all of them.
const issueNeedsDecisionLabel = "needs-decision"

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
}

// SweepIssueUnparkCommands lets a maintainer re-queue a parked issue with a
// one-line reply instead of editing labels (hivecommons/hive#9879).
//
// On every parked issue (`needs-human` or `needs-decision`) the sweep keeps a
// "What to reply" comment listing the commands, because the maintainer should
// never have to remember what to type. When an eligible person replies:
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
		if len(result.Unparked)+result.Replies >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenIssuesForTaskListSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Unparked)+result.Replies >= maxActions {
				break
			}
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			if !hasIssueParkingLabel(issueLabelNames(issue.Labels)) {
				continue
			}
			result.Seen++
			acted, err := c.tryUnparkIssue(ctx, repo, owner, repoName, issue, cutoff, result, opts.Audit)
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

// tryUnparkIssue handles one parked issue: keep its "What to reply" comment
// current, then answer the newest eligible command on it.
func (c *Client) tryUnparkIssue(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) (bool, error) {
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if err := c.ensureUnparkNoticeComment(ctx, owner, repo, number, issue.GetBody(), comments); err != nil {
		return false, err
	}

	answered := answeredUnparkCommentIDs(comments)
	candidate, command, err := c.eligibleUnparkCommand(ctx, owner, repo, comments, answered, cutoff)
	if err != nil {
		return false, err
	}
	if candidate == nil {
		return false, nil
	}
	actor := safeGetLogin(candidate.GetUser())

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

	if err := c.clearIssueParkingLabels(ctx, owner, repo, number, issueLabelNames(issue.Labels)); err != nil {
		return false, err
	}
	decision := strings.TrimSpace(command.Decision)
	if decision == "" {
		decision = "go with the recommendation"
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderUnparkAcceptedReply(actor, decision)); err != nil {
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

// hasIssueParkingLabel reports whether the issue is parked on a person.
func hasIssueParkingLabel(labels []string) bool {
	if hasIssueNeedsHumanLabel(labels) {
		return true
	}
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), issueNeedsDecisionLabel) {
			return true
		}
	}
	return false
}

// clearIssueParkingLabels removes the two parking labels the issue actually
// carries and adds the approval label, so the resulting PR is not held for
// #5117 and the issue ranks as an agreed direction. A label that has already
// gone is not an error.
func (c *Client) clearIssueParkingLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	for _, label := range []string{issueNeedsHumanLabel, issueNeedsDecisionLabel} {
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
func (c *Client) commenterMayUnpark(ctx context.Context, owner, repo, login string) (bool, error) {
	if strings.TrimSpace(login) == "" {
		return false, nil
	}
	level, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return false, fmt.Errorf("permission lookup for %s on %s/%s: %w", login, owner, repo, err)
	}
	return unparkPermissionLevels[strings.ToLower(strings.TrimSpace(level.GetPermission()))], nil
}

// answeredUnparkCommentIDs collects the comment IDs the hive has already
// answered, read back from its own replies' markers.
func answeredUnparkCommentIDs(comments []*gh.IssueComment) map[int64]bool {
	answered := map[int64]bool{}
	for _, cm := range comments {
		if cm == nil {
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
func (c *Client) eligibleUnparkCommand(ctx context.Context, owner, repo string, comments []*gh.IssueComment, answered map[int64]bool, cutoff time.Time) (*gh.IssueComment, unparkCommand, error) {
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
		allowed, err := c.commenterMayUnpark(ctx, owner, repo, safeGetLogin(cm.GetUser()))
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

func renderUnparkAcceptedReply(actor, decision string) string {
	return fmt.Sprintf("Un-parked by @%s. Decision: %s\n\n`needs-human` and `needs-decision` are cleared and `%s` is applied; any `hold` label is left exactly as it was.",
		actor, decision, HumanAckLabel)
}

func renderUnparkHelpReply(issueBody string) string {
	return "**What to reply**\n" + unparkCommandBullets(issueBody)
}

// unparkNoticeBody is the "What to reply" comment kept on every parked issue.
func unparkNoticeBody(issueBody string) string {
	return unparkNoticeMarker + "\n**What to reply**\n" + unparkCommandBullets(issueBody) +
		"\n_Only a maintainer with write access can run these; everything else is ignored._"
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
