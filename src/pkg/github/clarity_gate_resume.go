package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ClarityGateMarker tags the clarity gate's one question comment
// (runs.triage.clarity_gate, hivecommons/hive#13577). The scheduler posts it;
// SweepClarityGateResume reads it back to find the issues the gate parked.
const ClarityGateMarker = "<!-- hive:clarity-gate -->"

// DefaultClarityGateResumeMaxActions caps the issues one resume pass
// un-parks, in the same spirit as DefaultIssueUnparkSweepMaxActions.
const DefaultClarityGateResumeMaxActions = 10

// ClarityGateResumeOptions configures SweepClarityGateResume.
type ClarityGateResumeOptions struct {
	// Label is the park label the gate adds: the first entry of
	// project.issue_filter.hard_suppress_labels.needs_spec. Empty means
	// `needs-spec`.
	Label string
	// MaxActions caps the issues un-parked in one pass. Zero means
	// DefaultClarityGateResumeMaxActions.
	MaxActions int
	// Audit, when set, is called once per resumed issue.
	Audit func(ClarityGateResumeEvent)
}

// ClarityGateResumeEvent records one issue the sweep un-parked.
type ClarityGateResumeEvent struct {
	Repo   string
	Number int
	// Actor answered the gate's questions: the issue author or a maintainer.
	Actor      string
	Label      string
	CommentURL string
}

// ClarityGateResumeResult reports what one pass did.
type ClarityGateResumeResult struct {
	// Seen counts open issues carrying the park label.
	Seen int
	// Resumed lists the issues whose park label was lifted.
	Resumed []ClarityGateResumeEvent
	// Skipped counts issues whose reads or writes failed in this pass.
	Skipped int
}

// SweepClarityGateResume lifts the clarity gate's park once somebody answers
// its questions (hivecommons/hive#13588). An open issue is resumed only when
// all of these hold:
//
//   - it carries the park label and a ClarityGateMarker comment written by
//     one of this hive's own accounts (a pasted marker does not count);
//   - the newest `labeled` event for the park label is by one of the hive's
//     own accounts, so a `needs-spec` a person added — with or without an
//     older gate comment on the issue — is never lifted;
//   - after both, the issue author, or a person other than the hive with
//     write, maintain or admin on the repo (checked live), commented.
//
// The label comes off and the stage engine records an Unparked fact by the
// answering account, so the issue re-derives stage/010-actionable and
// re-enters the queue. The gate's marker stays, so the scheduler does not
// ask again. A hive that cannot name its own accounts lifts nothing.
func (c *Client) SweepClarityGateResume(ctx context.Context, opts ClarityGateResumeOptions) (*ClarityGateResumeResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		label = issueNeedsSpecLabel
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultClarityGateResumeMaxActions
	}
	result := &ClarityGateResumeResult{}
	if !c.knowsOwnAccounts() {
		return result, nil
	}
	for _, repo := range c.getRepos() {
		if len(result.Resumed) >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenIssuesWithLabel(ctx, owner, repoName, label)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Resumed) >= maxActions {
				break
			}
			if issue == nil || issue.IsPullRequest() || !labelPresent(issueLabelNames(issue.Labels), label) {
				continue
			}
			result.Seen++
			event, err := c.tryClarityGateResume(ctx, repo, owner, repoName, issue, label)
			if err != nil {
				if c.logger != nil {
					c.logger.Warn("clarity gate resume skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
				continue
			}
			if event == nil {
				continue
			}
			result.Resumed = append(result.Resumed, *event)
			if opts.Audit != nil {
				opts.Audit(*event)
			}
		}
	}
	return result, nil
}

// tryClarityGateResume handles one issue carrying the park label. It returns
// nil when the issue is not the gate's or nobody eligible has answered yet.
func (c *Client) tryClarityGateResume(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, label string) (*ClarityGateResumeEvent, error) {
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	asked, ok := c.clarityGateMarkerAt(comments)
	if !ok || !c.clarityGateHasLaterHumanComment(comments, asked) {
		return nil, nil
	}
	events, err := c.listIssueLabelEvents(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	parked, ok := lastLabeledEvent(events, label)
	if !ok || !c.isHiveAccount(parked.Actor) {
		return nil, nil
	}
	if parked.At.After(asked) {
		asked = parked.At
	}
	answer, err := c.clarityGateAnswer(ctx, owner, repo, safeGetLogin(issue.GetUser()), comments, asked)
	if err != nil || answer == nil {
		return nil, err
	}
	if err := c.RemoveLabel(ctx, displayRepo, number, label); err != nil {
		return nil, err
	}
	actor := safeGetLogin(answer.GetUser())
	if err := c.StageFacts().Unparked(ctx, c.IssueStageRef(owner+"/"+repo, number), actor); err != nil {
		return nil, err
	}
	if c.logger != nil {
		c.logger.Info("clarity gate resumed issue after a reply", "repo", displayRepo, "issue", number, "actor", actor, "label", label)
	}
	return &ClarityGateResumeEvent{Repo: displayRepo, Number: number, Actor: actor, Label: label, CommentURL: answer.GetHTMLURL()}, nil
}

// clarityGateMarkerAt returns when the hive posted the gate's question: the
// oldest ClarityGateMarker comment written by one of its own accounts.
func (c *Client) clarityGateMarkerAt(comments []*gh.IssueComment) (time.Time, bool) {
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), ClarityGateMarker) || !c.isHiveAccount(cm.GetUser()) {
			continue
		}
		return cm.GetCreatedAt().Time, true
	}
	return time.Time{}, false
}

// clarityGateHasLaterHumanComment is the free pre-check before the timeline
// and permission reads: has any person commented after the question?
func (c *Client) clarityGateHasLaterHumanComment(comments []*gh.IssueComment, after time.Time) bool {
	for _, cm := range comments {
		if cm != nil && cm.GetCreatedAt().Time.After(after) && c.clarityGatePerson(cm.GetUser()) {
			return true
		}
	}
	return false
}

// clarityGateAnswer returns the newest comment after the question from the
// issue author, or from a person with write access or above. Comments from
// anyone else are skipped in silence; at most unparkCandidateLimit
// permission lookups are spent per issue.
func (c *Client) clarityGateAnswer(ctx context.Context, owner, repo, author string, comments []*gh.IssueComment, after time.Time) (*gh.IssueComment, error) {
	checked := 0
	for i := len(comments) - 1; i >= 0; i-- {
		cm := comments[i]
		if cm == nil || !cm.GetCreatedAt().Time.After(after) || !c.clarityGatePerson(cm.GetUser()) {
			continue
		}
		login := safeGetLogin(cm.GetUser())
		if author != "" && strings.EqualFold(login, author) {
			return cm, nil
		}
		if checked >= unparkCandidateLimit {
			continue
		}
		checked++
		allowed, err := c.commenterMayUnpark(ctx, owner, repo, login, false)
		if err != nil {
			return nil, err
		}
		if allowed {
			return cm, nil
		}
	}
	return nil, nil
}

// clarityGatePerson reports whether a commenter is a person and not one of
// this hive's accounts (isHumanAuthor alone misses a PAT hive's resolved
// login and additional bot logins).
func (c *Client) clarityGatePerson(user *gh.User) bool {
	return c.isHumanAuthor(user) && !c.isHiveAccount(user)
}

// IssueLabelEverApplied reports whether label was ever added to the issue,
// present or not, from its timeline. The scheduler's clarity gate asks this
// when it finds its own question comment on an issue that does not carry the
// park label: a label that never landed means the park write failed before
// a restart and is retried; one that landed and came off means the issue
// was un-parked and runs (hivecommons/hive#13577).
func (c *Client) IssueLabelEverApplied(ctx context.Context, repo string, number int, label string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	events, err := c.listIssueLabelEvents(ctx, owner, name, number)
	if err != nil {
		return false, err
	}
	_, ok := lastLabeledEvent(events, label)
	return ok, nil
}
