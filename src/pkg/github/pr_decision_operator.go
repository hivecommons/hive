package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Operator decisions on a pull request parked at stage/085-needs-decision,
// made from the dashboard's activity monitor (hivecommons/hive#13479
// follow-up). Each mirrors what the hive honours when a maintainer does it
// on GitHub; none of them merges:
//
//   - approval posts the `/lgtm` and `/approve` commands as two comments —
//     the merge gate (Prow / tide, then the hive's own queue) decides from
//     there, exactly as when the maintainer types them. A person's approval
//     (PullRequestApprovalOptions) is also recorded against the head as the
//     answer to a requires_human verdict, lifts the `needs-human` park and
//     the sentinel alert, and is submitted to the stage engine — the
//     comments alone were read by no lane (#14821, #14822, crux#30 stayed
//     at stage/085 after the owner approved);
//   - a direction is the `/hive decision <text>` lane of
//     tryUnparkPullRequest: recorded against the head so the review
//     planner kicks the author agent, then acknowledged in a reply.

// ErrPullRequestNotFound is returned when the pull request does not exist.
var ErrPullRequestNotFound = errors.New("pull request not found")

// ErrPullRequestClosed is returned when a decision is pointed at a pull
// request that is no longer open.
var ErrPullRequestClosed = errors.New("pull request is closed")

// ErrPullRequestNotHiveAuthored is returned when a direction is pointed at
// a pull request the hive did not open: the hive does not push to it, so
// there is no author agent to carry the instruction.
var ErrPullRequestNotHiveAuthored = errors.New("pull request was not opened by the hive")

// ErrPullRequestHeadUnknown is returned when the PR's head SHA cannot be
// read, so a direction has nothing to be recorded against.
var ErrPullRequestHeadUnknown = errors.New("pull request head is unknown")

// PRApprovalCommands are the comment bodies an approval posts, in order.
var PRApprovalCommands = []string{"/lgtm", "/approve"}

// PullRequestApprovalEvent records one dashboard approval for the audit log.
type PullRequestApprovalEvent struct {
	Repo    string
	Number  int
	HeadSHA string
	Actor   string
	// Recorded is true when the approval was written to the human change
	// request store as the head-scoped answer the merge lanes and the stage
	// engine honour (PullRequestApprovalOptions.HumanChangeRequests).
	Recorded bool
	// NeedsHumanLifted is true when the PR's `needs-human` label was taken
	// off by this approval.
	NeedsHumanLifted bool
	// SentinelLabelCleared is true when the sentinel alert label was taken
	// off by this approval; SentinelAcknowledged when the review evidence
	// bundle for the head now records the person's acknowledgment of its
	// sentinel findings.
	SentinelLabelCleared bool
	SentinelAcknowledged bool
	// SentinelLabelOpen is true when the PR wore the sentinel alert label
	// and this approval could not take it off: the alert is still open and
	// a person must clear it on GitHub before the merge lanes touch the PR.
	SentinelLabelOpen bool
	// ProwManaged is true when the PR wears Prow's plugin labels (`approved`,
	// `size/*`, `do-not-merge/*`, `ok-to-test`), so the relayed `/approve`
	// came from the hive's account, which is not in OWNERS: Prow ignores it
	// ("NOT APPROVED"), and an OWNERS reviewer still has to approve there.
	ProwManaged bool
	// Stage is the stage label the engine derived when the approval was
	// submitted to it, or "" when the stage write was deferred to the sweep.
	Stage string
	// CommandsSkipped is set when the `/lgtm` + `/approve` comments were
	// not posted: PRApprovalCommandsSkippedProw when Prow rejected the
	// hive's own earlier commands on this PR (the bot is not in OWNERS),
	// so posting again would only draw the same "NOT APPROVED" reply. The
	// hive's own approval record is unaffected.
	CommandsSkipped string
	// ReportingError preserves partial progress: the native approval stands,
	// but comments, labels or stage reporting must be retried/reconciled.
	ReportingError string
}

// PRApprovalCommandsSkippedProw is the CommandsSkipped reason for a Prow
// repo where the hive bot is not an approver.
const PRApprovalCommandsSkippedProw = "prow-rejects-bot-commands"

// prowCommandRejections are the phrases Prow replies with when the hive
// bot's `/lgtm` or `/approve` carries no weight on the repo.
var prowCommandRejections = []string{"restricted to collaborators", "NOT APPROVED"}

// prowRejectedHiveCommands reports whether the Prow bot has replied to one of the hive's earlier dashboard approval commands on the
// PR with a Prow rejection (dibs#510: `/lgtm` → "changing LGTM is
// restricted to collaborators", `/approve` → "[APPROVALNOTIFIER] This PR is
// NOT APPROVED"). Prow quotes the comment it answers as
// "In response to [this](…#issuecomment-<id>)", so only replies bound to a
// hive command comment count. Only the Prow instance hivecommons runs
// (config.DefaultLabelDrivenTrustedBots) is believed: any other installed
// bot could quote the phrase and suppress the commands
// (hivecommons/hive#15263).
func (c *Client) prowRejectedHiveCommands(ctx context.Context, owner, name string, number int) (bool, error) {
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return false, err
	}
	own := map[int64]bool{}
	for _, comment := range comments {
		if comment == nil || !c.markerAuthorTrusted(comment.GetUser()) {
			continue
		}
		if _, _, _, ok := ParsePRApprovalCommandComment(comment.GetBody()); ok {
			own[comment.GetID()] = true
		}
	}
	if len(own) == 0 {
		return false, nil
	}
	for _, comment := range comments {
		if comment == nil || c.isHiveAccount(comment.GetUser()) {
			continue
		}
		login := strings.TrimSpace(comment.GetUser().GetLogin())
		prow := false
		for _, bot := range config.DefaultLabelDrivenTrustedBots {
			if strings.EqualFold(bot, login) {
				prow = true
				break
			}
		}
		if !prow {
			continue
		}
		body := comment.GetBody()
		rejected := false
		for _, phrase := range prowCommandRejections {
			if strings.Contains(body, phrase) {
				rejected = true
				break
			}
		}
		if !rejected {
			continue
		}
		for id := range own {
			if strings.Contains(body, fmt.Sprintf("#issuecomment-%d)", id)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// PullRequestApprovalOptions is what a person's Approve decision releases
// beyond posting the commands (hivecommons/hive: #14821, #14822, crux#30
// sat at stage/085 after the owner approved from the dashboard — the
// comments were read by nobody but the workflow-path fence). The zero value
// posts the commands only: a recommendation without release authority must
// not clear a person's gate.
type PullRequestApprovalOptions struct {
	// Expected identity binds an asynchronous decision to the facts it read.
	ExpectedHead       string
	ExpectedBodyDigest string
	ExpectedAuthor     string
	// Delegated decisions may not clear a newly arrived hold or sentinel.
	GuardSentinelLabel string
	VerifyActorWrite   bool
	// Recorded runs after the durable approval, before optional reporting.
	Recorded func(PullRequestApprovalEvent)
	// ReportingTimeout bounds lock admission and network reporting, not fsync.
	ReportingTimeout time.Duration
	// HumanChangeRequests, when set, records the approval against the head
	// as a bare `approve` decision — the fact withRecordedDecision and the
	// stage engine read as "the requires_human verdict on this head has
	// been answered", for every lane, whoever authored the PR.
	HumanChangeRequests review.HumanChangeRequestStore
	// LiftNeedsHuman takes `needs-human` off the PR. The hive's own park
	// (a supersession or adoption ask) is what the person just answered; a
	// person's label is lifted on the same reasoning as an issue decision
	// (stageengine.DecisionLiftsNeedsHuman).
	LiftNeedsHuman bool
	// SentinelLabel, when non-empty, is the alert label to take off: the
	// Decide dialog showed the sentinel reason and styled Approve as
	// dangerous, so the approval is the human review the alert asked for.
	// The acknowledgment is also recorded in the head's evidence bundle so
	// a bundle-only alert (no label) clears too.
	SentinelLabel string
	// Now stamps the acknowledgment; time.Now when nil.
	Now func() time.Time
}

// prApprovalCommentMarkerPrefix opens the hidden marker a dashboard approval
// comment carries so a later reader can bind the decision to the head it was
// made on (hivecommons/hive#14664).
const prApprovalCommentMarkerPrefix = "<!-- hive:pr-approval"

// prApprovalCommentSuffix is the attribution line of a dashboard approval.
const prApprovalCommentSuffix = " from the Hive dashboard; the merge gate decides from here."

func renderPRApprovalCommandComment(command, actor, head string) string {
	body := fmt.Sprintf("%s\n\nPosted by @%s%s", command, strings.TrimSpace(actor), prApprovalCommentSuffix)
	if head = strings.ToLower(strings.TrimSpace(head)); head != "" {
		body += "\n" + prApprovalCommentMarkerPrefix + " head=" + head + " by=" + strings.ToLower(strings.TrimSpace(actor)) + " -->"
	}
	return body
}

// ParsePRApprovalCommandComment reads a comment the dashboard posted on a
// maintainer's behalf (renderPRApprovalCommandComment). It returns the
// command (`/lgtm` or `/approve`), the actor the comment is attributed to,
// and the head SHA the marker bound it to ("" for comments posted before
// the marker existed). ok is false for any other comment.
func ParsePRApprovalCommandComment(body string) (command, actor, head string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var rest []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if command == "" {
			command = line
			continue
		}
		rest = append(rest, line)
	}
	if len(rest) == 0 {
		return "", "", "", false
	}
	known := false
	for _, want := range PRApprovalCommands {
		if command == want {
			known = true
		}
	}
	if !known {
		return "", "", "", false
	}
	attribution := rest[0]
	if !strings.HasPrefix(attribution, "Posted by @") || !strings.HasSuffix(attribution, prApprovalCommentSuffix) {
		return "", "", "", false
	}
	actor = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(attribution, "Posted by @"), prApprovalCommentSuffix))
	if actor == "" || strings.ContainsAny(actor, " \t") {
		return "", "", "", false
	}
	for _, line := range rest[1:] {
		if !strings.HasPrefix(line, prApprovalCommentMarkerPrefix) {
			continue
		}
		for _, field := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, prApprovalCommentMarkerPrefix), "-->")) {
			if v, found := strings.CutPrefix(field, "head="); found {
				head = strings.ToLower(strings.TrimSpace(v))
			}
		}
	}
	return command, actor, head, true
}

// prDecisionTarget is what one open-PR read tells a dashboard decision:
// the head, whether the hive opened the PR, and — when it did not —
// whether a contributor's hive did (author/clanker), so a direction has a
// hive to carry it to.
type prDecisionTarget struct {
	head         string
	selfAuthored bool
	clanker      bool
	author       string
	labels       []string
	bodyDigest   string
}

// SetAuthorKindLabels installs the live governor.labels.author resolver.
// Nil uses the default names, matching the stage reconciler.
func (c *Client) SetAuthorKindLabels(fn func() authorkind.Names) {
	if c != nil {
		c.authorKindLabels = fn
	}
}

func (c *Client) openPullRequestDecisionTarget(ctx context.Context, owner, name string, number int) (prDecisionTarget, error) {
	pr, resp, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return prDecisionTarget{}, fmt.Errorf("pull request %s/%s#%d: %w", owner, name, number, ErrPullRequestNotFound)
		}
		return prDecisionTarget{}, fmt.Errorf("fetching pull request %s/%s#%d: %w", owner, name, number, err)
	}
	if strings.EqualFold(pr.GetState(), "closed") {
		return prDecisionTarget{}, ErrPullRequestClosed
	}
	t := prDecisionTarget{author: strings.TrimSpace(safeGetLogin(pr.GetUser()))}
	t.bodyDigest = review.PullRequestBodyDigest(pr.GetTitle(), pr.GetBody())
	if pr.GetHead() != nil {
		t.head = strings.TrimSpace(pr.GetHead().GetSHA())
	}
	t.labels = issueLabelNames(pr.Labels)
	names := authorkind.Names{}
	if c.authorKindLabels != nil {
		names = c.authorKindLabels()
	}
	kind := authorkind.FromLabels(t.labels, names)
	t.selfAuthored = c.isHiveAccount(pr.GetUser()) || kind == authorkind.Hive
	if !t.selfAuthored {
		// Require the repository's classification stamp here. PR-body
		// markers are author-controlled hints for the sweep, not sufficient
		// evidence to accept a dashboard direction on a person's PR.
		t.clanker = kind == authorkind.Clanker
	}
	return t, nil
}

func (c *Client) operatorPRTarget(repo string, number int, actor, what string) (owner, name string, err error) {
	if c == nil || c.client == nil {
		return "", "", ErrNoGitHubClient
	}
	if strings.TrimSpace(actor) == "" {
		return "", "", fmt.Errorf("%s: actor is required", what)
	}
	owner, name = c.splitRepo(repo)
	if err := validateRepoRef(owner, name); err != nil {
		return "", "", fmt.Errorf("%s: %w", what, err)
	}
	if number <= 0 {
		return "", "", fmt.Errorf("%s: pull request number is required", what)
	}
	return owner, name, nil
}

// RecordOperatorPullRequestApproval posts the `/lgtm` and `/approve`
// commands on an open pull request on behalf of actor, each attributed to
// them. It does not review, label or merge: whatever gates merging reads
// the commands exactly as it would had the maintainer typed them. The
// caller is responsible for having verified actor's write access.
func (c *Client) RecordOperatorPullRequestApproval(ctx context.Context, repo string, number int, actor string) (PullRequestApprovalEvent, error) {
	return c.RecordOperatorPullRequestApprovalWith(ctx, repo, number, actor, PullRequestApprovalOptions{})
}

// RecordOperatorPullRequestApprovalWith is RecordOperatorPullRequestApproval
// plus what opts asks the person's approval to release: the validated native
// approval is recorded against the head BEFORE optional external reporting.
// Park labels and sentinel acknowledgment are reconciled, then the stage
// engine is given the decision and the command comments are reported.
// Reporting is bounded; its failures do not undo a recorded approval.
// Failed label/stage writes remain logged and reflected in the event, and
// transport failures appear in ReportingError for the caller's audit/receipt.
func (c *Client) RecordOperatorPullRequestApprovalWith(ctx context.Context, repo string, number int, actor string, opts PullRequestApprovalOptions) (PullRequestApprovalEvent, error) {
	owner, name, err := c.operatorPRTarget(repo, number, actor, "RecordOperatorPullRequestApproval")
	if err != nil {
		return PullRequestApprovalEvent{}, err
	}
	actor = strings.TrimSpace(actor)
	target, err := c.openPullRequestDecisionTarget(ctx, owner, name, number)
	if err != nil {
		return PullRequestApprovalEvent{}, err
	}
	head, selfAuthored, labels := target.head, target.selfAuthored, target.labels
	if opts.ExpectedHead != "" && head != opts.ExpectedHead ||
		opts.ExpectedBodyDigest != "" && target.bodyDigest != opts.ExpectedBodyDigest ||
		opts.ExpectedAuthor != "" && !strings.EqualFold(target.author, opts.ExpectedAuthor) {
		return PullRequestApprovalEvent{}, errors.New("approval target changed since the decision; gather and validate again")
	}
	if opts.VerifyActorWrite {
		if labelPresent(labels, "hold") || opts.GuardSentinelLabel != "" && labelPresent(labels, opts.GuardSentinelLabel) {
			return PullRequestApprovalEvent{}, errors.New("approval target gained a hold or sentinel; delegate must not release it")
		}
		permission, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, name, actor)
		if err != nil {
			return PullRequestApprovalEvent{}, fmt.Errorf("revalidating approval actor: %w", err)
		}
		switch strings.ToLower(permission.GetPermission()) {
		case "admin", "maintain", "write":
		default:
			return PullRequestApprovalEvent{}, errors.New("approval actor no longer has write access")
		}
	}
	full := owner + "/" + name
	event := PullRequestApprovalEvent{Repo: full, Number: number, HeadSHA: head, Actor: actor, ProwManaged: prowManagedLabels(labels)}
	if opts.HumanChangeRequests != nil {
		if head == "" {
			return event, ErrPullRequestHeadUnknown
		}
		req := review.HumanChangeRequest{
			Repo: full, Number: number, HeadSHA: head, BodyDigest: target.bodyDigest,
			Source: review.HumanChangeRequestSourceDecision, SourceID: operatorDecisionMarkerID,
			Actor: actor, Body: prDecisionApproveBody, Kind: review.HumanChangeRequestKindApprove,
		}
		start := time.Now()
		if err := opts.HumanChangeRequests.Record(req); err != nil {
			return event, fmt.Errorf("recording dashboard approval on %s#%d: %w", full, number, err)
		}
		event.Recorded = true
		if c.logger != nil {
			c.logger.Info("native operator approval recorded; external reporting pending", "repo", full, "pr", number, "head", head, "actor", actor, "record_duration", time.Since(start))
		}
		if opts.Recorded != nil {
			opts.Recorded(event)
		}
	}
	timeout := opts.ReportingTimeout
	if timeout <= 0 {
		timeout = defaultApprovalReportingTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reportStarted := time.Now()
	reportErr := c.reportOperatorPullRequestApproval(ctx, owner, name, number, actor, head, selfAuthored, labels, opts, &event)
	if c.logger != nil {
		c.logger.Info("operator approval external reporting finished", "repo", full, "pr", number, "head", head, "approval_recorded", event.Recorded, "report_duration", time.Since(reportStarted), "error", reportErr)
	}
	if reportErr != nil {
		if !event.Recorded {
			return event, reportErr
		}
		event.ReportingError = reportErr.Error()
		c.warnOperatorApproval("native approval stands; external reporting incomplete", full, number, head, reportErr)
	}
	return event, nil
}

const defaultApprovalReportingTimeout = 15 * time.Second

func (c *Client) reportOperatorPullRequestApproval(ctx context.Context, owner, name string, number int, actor, head string, selfAuthored bool, labels []string, opts PullRequestApprovalOptions, event *PullRequestApprovalEvent) error {
	full := owner + "/" + name
	if opts.LiftNeedsHuman {
		event.NeedsHumanLifted = slices.Contains(c.liftPRDecisionParkLabels(ctx, full, number, head, labels), issueNeedsHumanLabel)
	}
	if label := strings.TrimSpace(opts.SentinelLabel); label != "" {
		if labelPresent(labels, label) {
			if err := c.RemoveLabel(ctx, full, number, label); err != nil {
				c.warnOperatorApproval("sentinel label not cleared", full, number, head, err)
				event.SentinelLabelOpen = true
			} else {
				event.SentinelLabelCleared = true
			}
		}
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		event.SentinelAcknowledged = c.RecordReviewEvidenceSentinelAck(full, number, head, actor, now())
	}
	if event.Recorded {
		event.Stage = c.applyPRDecisionStage(ctx, owner, name, number, head, actor, prDecisionApprove, selfAuthored)
		c.logPRDecision(full, number, head, actor, prDecisionApprove, event.Stage, false)
	}
	prowRejected, err := c.prowRejectedHiveCommands(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("reading comments of %s/%s#%d before posting approval commands: %w", owner, name, number, err)
	}
	if prowRejected {
		event.CommandsSkipped = PRApprovalCommandsSkippedProw
		if c.logger != nil {
			c.logger.Info("pull request approval commands not posted: Prow rejected the hive's earlier commands on this PR (bot is not an approver); the hive's own approval record stands", "repo", event.Repo, "pr", number, "actor", actor, "head", head)
		}
	} else {
		for _, command := range PRApprovalCommands {
			if err := c.CreateIssueComment(ctx, full, number, renderPRApprovalCommandComment(command, actor, head)); err != nil {
				return fmt.Errorf("posting %s on %s/%s#%d: %w", command, owner, name, number, err)
			}
		}
		if c.logger != nil {
			c.logger.Info("pull request approval commands posted by dashboard decision", "repo", event.Repo, "pr", number, "actor", actor, "head", head)
		}
	}
	return ctx.Err()
}

// prowManagedLabels reports whether labels carry Prow's plugin markers:
// the approve plugin's `approved`, size's `size/*`, the `do-not-merge/*`
// family and trigger's `ok-to-test` / `needs-ok-to-test`.
func prowManagedLabels(labels []string) bool {
	for _, l := range labels {
		l = strings.ToLower(strings.TrimSpace(l))
		switch {
		case l == "approved", l == "ok-to-test", l == "needs-ok-to-test",
			strings.HasPrefix(l, "size/"), strings.HasPrefix(l, "do-not-merge/"), strings.HasPrefix(l, "tide/"):
			return true
		}
	}
	return false
}

// prDecisionApproveBody is the decision text a dashboard approval records,
// the bare form `/hive decision approve` carries.
const prDecisionApproveBody = "approve"

func (c *Client) warnOperatorApproval(what, repo string, number int, head string, err error) {
	if c.logger != nil {
		c.logger.Warn("dashboard approval: "+what, "repo", repo, "pr", number, "head", head, "error", err)
	}
}

// RecordOperatorPullRequestDecision records a maintainer's direction on an
// open pull request made through the dashboard — the same effect as that
// maintainer commenting `/hive decision <text>` and the un-park sweep
// answering it: the text is stored against the head as a human change
// request and one reply naming the actor is posted. On a PR the hive
// opened the review planner kicks the hive's own author agent with the
// text verbatim. On a contributor's clanker PR (author/clanker) the
// direction is carried to that contributor's hive
// instead: it stands as a changes-requested review on the head, the PR
// goes to 072 returned-to-contributor and the contributor nudge delivers
// the text (hivecommons/hive#15159). A PR a person opened has no hive to
// carry it: ErrPullRequestNotHiveAuthored, leave a review instead. The
// caller is responsible for having verified actor's write access.
func (c *Client) RecordOperatorPullRequestDecision(ctx context.Context, repo string, number int, actor, decision string, store review.HumanChangeRequestStore) (PullRequestDecisionEvent, error) {
	owner, name, err := c.operatorPRTarget(repo, number, actor, "RecordOperatorPullRequestDecision")
	if err != nil {
		return PullRequestDecisionEvent{}, err
	}
	actor = strings.TrimSpace(actor)
	decision = strings.TrimSpace(decision)
	if decision == "" {
		return PullRequestDecisionEvent{}, fmt.Errorf("RecordOperatorPullRequestDecision: decision text is required")
	}
	if store == nil {
		return PullRequestDecisionEvent{}, fmt.Errorf("RecordOperatorPullRequestDecision: no human change request store")
	}
	target, err := c.openPullRequestDecisionTarget(ctx, owner, name, number)
	if err != nil {
		return PullRequestDecisionEvent{}, err
	}
	head, selfAuthored, labels := target.head, target.selfAuthored, target.labels
	if !selfAuthored && !target.clanker {
		return PullRequestDecisionEvent{}, ErrPullRequestNotHiveAuthored
	}
	if head == "" {
		return PullRequestDecisionEvent{}, ErrPullRequestHeadUnknown
	}
	mode := prDecisionModeFor(unparkCommand{Kind: unparkCommandDecision, Decision: decision, Text: decision})
	if !selfAuthored && mode == prDecisionApprove {
		// A bare "approve" is not a direction a contributor's hive can act
		// on; the dashboard's Approve action is the release for that.
		return PullRequestDecisionEvent{}, ErrPullRequestNotHiveAuthored
	}
	req := review.HumanChangeRequest{
		Repo:     owner + "/" + name,
		Number:   number,
		HeadSHA:  head,
		Source:   review.HumanChangeRequestSourceDecision,
		SourceID: operatorDecisionMarkerID,
		Actor:    actor,
		Body:     decision,
		Kind:     mode.kind(),
	}
	// Record before replying: a reply without a record would promise a fix
	// nothing is queued to make.
	if err := store.Record(req); err != nil {
		return PullRequestDecisionEvent{}, fmt.Errorf("recording dashboard decision on %s/%s#%d: %w", owner, name, number, err)
	}
	event := PullRequestDecisionEvent{Repo: owner + "/" + name, Number: number, HeadSHA: head, Actor: actor, Decision: decision, Mode: string(mode), ToContributor: target.author}
	if selfAuthored {
		event.ToContributor = ""
	}
	// The decision answers the park: the labels that asked for it come off
	// in the same write, or the next label-derived pass re-parks the row
	// the person just un-parked.
	event.ParkLabelsLifted = len(c.liftPRDecisionParkLabels(ctx, event.Repo, number, head, labels))
	stage := c.applyPRDecisionStage(ctx, owner, name, number, head, actor, mode, selfAuthored)
	reply := renderPRDecisionReply(actor, head, mode)
	if !selfAuthored {
		reply = renderPRDecisionToContributorHiveReply(actor, head, target.author, decision)
	}
	body := reply + "\n\n_Recorded from the hive dashboard._"
	if err := c.postUnparkReply(ctx, owner, name, number, operatorDecisionMarkerID, body); err != nil {
		return PullRequestDecisionEvent{}, err
	}
	c.logPRDecision(event.Repo, number, head, actor, mode, stage, false)
	return event, nil
}

// prDecisionParkLabels are the labels a recorded decision on a pull
// request answers: the needs-human ask and the 085 needs-decision stage
// label (current and legacy). `hold` is never among them.
var prDecisionParkLabels = []string{issueNeedsHumanLabel, stagelabels.NeedsDecision.Label(), NeedsDecisionStageLabel}

// liftPRDecisionParkLabels removes every prDecisionParkLabels entry present
// in labels from repo#number and returns the ones it removed. A failed
// removal is logged, not returned: the decision record stands and the
// stage engine's park-lift derives the same answer.
func (c *Client) liftPRDecisionParkLabels(ctx context.Context, repo string, number int, head string, labels []string) []string {
	var lifted []string
	for _, label := range prDecisionParkLabels {
		if !labelPresent(labels, label) {
			continue
		}
		if err := c.RemoveLabel(ctx, repo, number, label); err != nil {
			c.warnOperatorApproval(label+" not lifted", repo, number, head, err)
			continue
		}
		lifted = append(lifted, label)
	}
	return lifted
}
