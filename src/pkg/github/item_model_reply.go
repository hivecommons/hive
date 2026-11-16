package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
)

// Model reply verbs outside the parked-issue path (hivecommons/hive#18357).
//
// A bound workflow model declares its reply verbs per state, and its state
// numbers are the stage numbers items carry (stageengine.DeriveForRepo), so
// a verb belongs to wherever the item currently sits — a PR at review, an
// issue being worked — not only to an issue parked on a person. The parked
// path (tryUnparkIssue) still owns parked issues, because a reply there also
// clears the hive's park labels; this path answers every other item whose
// current model state offers verbs, and changes only what the model maps the
// reply to.

// tryItemModelReply answers the newest eligible model reply on an item that
// is not on the parked-issue path: the label the model maps to the reply's
// transition (if any) is applied and the reply is answered in one comment.
// Nothing else is touched — no park label is cleared and no built-in
// command is consumed — so an unbound repository, the hive-default model or
// a state without verbs costs no API call and behaves exactly as before.
func (c *Client) tryItemModelReply(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, profile *workflowmodel.Profile, result *IssueUnparkSweepResult) (bool, error) {
	verbs := issueModelReplyVerbs(profile, issue)
	if len(verbs) == 0 {
		return false, nil
	}
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	candidate, command, err := c.eligibleModelReplyCommand(ctx, owner, repo, comments, c.answeredUnparkCommentIDs(comments), cutoff, verbs)
	if err != nil || candidate == nil {
		return false, err
	}
	labels := issueLabelNames(issue.Labels)
	eff, ok := profile.ReplyForNumber(stagelabels.Current(labels).Number(), command.Verb)
	if !ok {
		return false, nil
	}
	actor := safeGetLogin(candidate.GetUser())
	applied, err := c.applyModelReplyLabel(ctx, owner, repo, number, labels, eff)
	if err != nil {
		return false, err
	}
	decision := eff.Verb
	if t := strings.TrimSpace(command.DecisionText()); t != "" {
		decision += ": " + t
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, candidate.GetID(), renderItemModelReplyAccepted(actor, decision, eff, applied)); err != nil {
		return false, err
	}
	event := IssueUnparkEvent{Repo: displayRepo, Number: number, Actor: actor, Decision: decision, CommentURL: candidate.GetHTMLURL()}
	result.ModelReplies = append(result.ModelReplies, event)
	if c.logger != nil {
		c.logger.Info("workflow model reply recorded", "repo", displayRepo, "item", number, "pr", issue.IsPullRequest(), "actor", actor, "reply", eff.Verb, "to", eff.To, "label", applied)
	}
	return true, nil
}

// eligibleModelReplyCommand is eligibleUnparkCommand restricted to the
// model's reply verbs: only a `/hive <verb>` for the item's current state is
// a candidate, so built-in commands and prose assent off the parked path are
// left to the handlers that already own them. An answered model reply is a
// watermark — nothing older is replayed.
func (c *Client) eligibleModelReplyCommand(ctx context.Context, owner, repo string, comments []*gh.IssueComment, answered map[int64]bool, cutoff time.Time, verbs []string) (*gh.IssueComment, unparkCommand, error) {
	checked := 0
	for i := len(comments) - 1; i >= 0 && checked < unparkCandidateLimit; i-- {
		cm := comments[i]
		if cm == nil {
			continue
		}
		cmd, isReply := parseModelReplyCommand(cm.GetBody(), verbs)
		if answered[cm.GetID()] {
			if isReply {
				break
			}
			continue
		}
		if !isReply {
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
		if !c.isHumanAuthor(cm.GetUser()) || isHiveSelfDecisionComment(cm.GetBody()) {
			continue
		}
		checked++
		allowed, err := c.commenterMayUnpark(ctx, owner, repo, safeGetLogin(cm.GetUser()), false)
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

// applyModelReplyLabel adds the label the model maps to eff, unless the item
// already wears it, and returns the label applied ("" for none).
func (c *Client) applyModelReplyLabel(ctx context.Context, owner, repo string, number int, labels []string, eff workflowmodel.ReplyEffect) (string, error) {
	if eff.Label == "" || labelPresent(labels, eff.Label) {
		return "", nil
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{eff.Label}); err != nil { // writeback:solicited a maintainer answered the gate with this model verb (comment or dashboard)
		return "", fmt.Errorf("adding %s to %s/%s#%d: %w", eff.Label, owner, repo, number, err)
	}
	c.recordWriteAudit(AuditActionHiveLabelApplied, hiveWriteMeta(),
		WriteTarget{Repo: owner + "/" + repo, Number: number}, "label", eff.Label, "reply", eff.Verb)
	return eff.Label, nil
}

// renderItemModelReplyAccepted is the reply to a model verb off the parked
// path. It keeps the `Decision:` sentence shape of the parked reply but does
// not claim an un-park, since nothing was parked.
func renderItemModelReplyAccepted(actor, decision string, eff workflowmodel.ReplyEffect, applied string) string {
	return fmt.Sprintf("Recorded for @%s. Decision: %s.%s", actor, decision, modelReplyEffectSentence(eff, applied))
}

// modelReplyEffectSentence says what the model did with a reply.
func modelReplyEffectSentence(eff workflowmodel.ReplyEffect, applied string) string {
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
	return tail.String()
}
