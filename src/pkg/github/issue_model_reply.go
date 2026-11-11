package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	"github.com/hivecommons/hive/pkg/workflowmodel/hivedefault"
)

// unparkCommandModelReply is `/hive <verb>` where <verb> is one of the
// replies the repository's bound workflow model declares for the item's
// current state (hivecommons/hive#17620, RFC phase 4). It is only ever
// produced when a model is bound; on an unbound repository the same comment
// is an unknown command exactly as before.
const unparkCommandModelReply unparkCommandKind = "model-reply"

// modelReplyVerbs returns the verbs a bound model offers for an item wearing
// labels, in declaration order; nil for the hive-default model, a profile
// that is not bound, or a state that declares no replies. The state is the
// built-in stage number the item currently carries — the model's state
// numbers are its stage numbers (stageengine.DeriveForRepo).
func modelReplyVerbs(profile *workflowmodel.Profile, labels []string) []string {
	if profile == nil || profile.Name == hivedefault.Name {
		return nil
	}
	return profile.RepliesForNumber(stagelabels.Current(labels).Number())
}

// parseUnparkCommandFor is parseUnparkCommand extended with a model's reply
// verbs. On a bound repository the model owns the vocabulary for its state:
// the longest verb whose words open the command line (case, `_`/`-`,
// spacing and trailing punctuation folded by workflowmodel.NormalizeReply)
// is the reply and what follows is the decision text; only then do the
// built-in commands apply. An empty verb list is exactly parseUnparkCommand,
// so an unbound repository parses every comment as before.
func parseUnparkCommandFor(body string, verbs []string) (unparkCommand, bool) {
	if cmd, ok := parseModelReplyCommand(body, verbs); ok {
		return cmd, true
	}
	return parseUnparkCommand(body)
}

func parseModelReplyCommand(body string, verbs []string) (unparkCommand, bool) {
	if len(verbs) == 0 {
		return unparkCommand{}, false
	}
	line := firstNonEmptyLine(body)
	if line == "" || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
		return unparkCommand{}, false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[0], unparkCommandPrefix) {
		return unparkCommand{}, false
	}
	rest := fields[1:]
	best, bestWords := "", 0
	for _, verb := range verbs {
		want := workflowmodel.NormalizeReply(verb)
		if want == "" {
			continue
		}
		for k := 1; k <= len(rest) && k <= modelReplyMaxWords; k++ {
			got := workflowmodel.NormalizeReply(strings.TrimRight(strings.Join(rest[:k], " "), ".:,;!"))
			if got == want && k > bestWords {
				best, bestWords = verb, k
			}
		}
	}
	if best == "" {
		return unparkCommand{}, false
	}
	decision := strings.TrimSpace(strings.Join(rest[bestWords:], " "))
	return unparkCommand{Kind: unparkCommandModelReply, Verb: best, Decision: decision, Text: decisionText(body, decision)}, true
}

// modelReplyMaxWords bounds how many words of a command line are tried
// against a verb; model replies are short imperatives.
const modelReplyMaxWords = 6

// applyIssueModelReply records a model reply on a parked issue: the park
// labels the hive added are cleared as for `/hive approve`, the label the
// model maps to the reply's transition (if any) is applied, and the reply is
// answered in one comment. source names where the reply came from for the
// comment ("" for a comment command). labels are the issue's labels as read.
func (c *Client) applyIssueModelReply(ctx context.Context, owner, repo string, number int, labels []string, actor string, eff workflowmodel.ReplyEffect, text string, commentID int64, source string) (IssueUnparkEvent, error) {
	needsHuman := unparkNeedsHumanOutcome{}
	if hasIssueNeedsHumanLabel(labels) {
		needsHuman = c.needsHumanOutcome(ctx, owner, repo, number, nil)
	}
	removed, err := c.clearIssueParkingLabels(ctx, owner, repo, number, labels, actor, needsHuman.lift)
	if err != nil {
		return IssueUnparkEvent{}, err
	}
	applied := ""
	if eff.Label != "" && !labelPresent(labels, eff.Label) {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{eff.Label}); err != nil { // writeback:solicited a maintainer answered the gate with this model verb (comment or dashboard)
			return IssueUnparkEvent{}, fmt.Errorf("adding %s to %s/%s#%d: %w", eff.Label, owner, repo, number, err)
		}
		c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
			WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", eff.Label, "reply", eff.Verb)
		applied = eff.Label
	}
	decision := eff.Verb
	if t := strings.TrimSpace(text); t != "" {
		decision += ": " + t
	}
	body := renderModelReplyAccepted(actor, decision, eff, applied, removed, hasIssueNeedsHumanLabel(labels), needsHuman)
	if source != "" {
		body += "\n\n_Recorded from " + source + "._"
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, commentID, body); err != nil {
		return IssueUnparkEvent{}, err
	}
	event := IssueUnparkEvent{Repo: owner + "/" + repo, Number: number, Actor: actor, Decision: decision}
	if c.logger != nil {
		c.logger.Info("issue un-parked by model reply", "repo", event.Repo, "issue", number, "actor", actor, "reply", eff.Verb, "to", eff.To, "label", applied)
	}
	return event, nil
}

// renderModelReplyAccepted is the reply to a model verb. It leads with the
// same sentence as a built-in decision so the `Decision:` round-trip the
// decision history reads is unchanged, then says what the model did with it.
func renderModelReplyAccepted(actor, decision string, eff workflowmodel.ReplyEffect, applied string, removed []string, needsHuman bool, outcome unparkNeedsHumanOutcome) string {
	b := renderUnparkAcceptedReplyKeeping(actor, decision, removed, needsHuman, outcome)
	var tail strings.Builder
	fmt.Fprintf(&tail, "\n\nWorkflow reply `%s`", eff.Verb)
	switch {
	case eff.To != "" && applied != "":
		fmt.Fprintf(&tail, " moves the item from `%s` to `%s`; `%s` applied.", eff.From, eff.To, applied)
	case eff.To != "":
		fmt.Fprintf(&tail, " moves the item from `%s` to `%s`; the model maps no label to that state, so the reply is recorded here only.", eff.From, eff.To)
	default:
		tail.WriteString(" is recorded; the model declares no transition for it.")
	}
	return b + tail.String()
}

// RecordOperatorIssueModelReply is RecordOperatorIssueDecision for a model
// reply chosen in the dashboard (hivecommons/hive#17620): verb must be one
// of the replies profile declares for the issue's current state, else
// ErrUnknownModelReply. profile nil or hive-default is the same error — the
// dashboard offers model verbs only for bound repositories.
func (c *Client) RecordOperatorIssueModelReply(ctx context.Context, repo string, number int, actor, verb, text string, profile *workflowmodel.Profile) (IssueUnparkEvent, error) {
	if c == nil || c.client == nil {
		return IssueUnparkEvent{}, ErrNoGitHubClient
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return IssueUnparkEvent{}, fmt.Errorf("RecordOperatorIssueModelReply: actor is required")
	}
	owner, name := c.splitRepo(repo)
	if err := validateRepoRef(owner, name); err != nil {
		return IssueUnparkEvent{}, fmt.Errorf("RecordOperatorIssueModelReply: %w", err)
	}
	if number <= 0 {
		return IssueUnparkEvent{}, fmt.Errorf("RecordOperatorIssueModelReply: issue number is required")
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return IssueUnparkEvent{}, fmt.Errorf("reading %s#%d before reply: %w", repo, number, err)
	}
	if issue.IsPullRequest() {
		return IssueUnparkEvent{}, ErrDecisionTargetIsPR
	}
	labels := issueLabelNames(issue.Labels)
	eff, ok := modelReplyEffect(profile, labels, verb)
	if !ok {
		return IssueUnparkEvent{}, ErrUnknownModelReply
	}
	if !hasIssueParkingLabel(labels) {
		return IssueUnparkEvent{}, ErrIssueNotParked
	}
	return c.applyIssueModelReply(ctx, owner, name, number, labels, actor, eff, text, operatorDecisionMarkerID, "the hive dashboard")
}

// modelReplyEffect resolves verb against the replies the model offers for
// the item's current state.
func modelReplyEffect(profile *workflowmodel.Profile, labels []string, verb string) (workflowmodel.ReplyEffect, bool) {
	if len(modelReplyVerbs(profile, labels)) == 0 {
		return workflowmodel.ReplyEffect{}, false
	}
	return profile.ReplyForNumber(stagelabels.Current(labels).Number(), verb)
}

// ErrUnknownModelReply is returned when a reply verb is not one the bound
// model declares for the item's current state.
var ErrUnknownModelReply = fmt.Errorf("reply is not one the workflow model offers for this item")

// issueModelReplyVerbs is modelReplyVerbs for a listed issue.
func issueModelReplyVerbs(profile *workflowmodel.Profile, issue *gh.Issue) []string {
	if profile == nil || issue == nil {
		return nil
	}
	return modelReplyVerbs(profile, issueLabelNames(issue.Labels))
}
