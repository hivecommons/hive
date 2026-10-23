package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/priorart"
)

// PriorArtGateMarker tags the prior-art gate's one park comment
// (runs.triage.prior_art, hivecommons/hive#13575). The scheduler posts it;
// SweepPriorArtGateResume reads it back to find the issues the gate parked.
const PriorArtGateMarker = priorart.Marker

// DefaultPriorArtGateResumeMaxActions caps the issues one resume pass acts
// on, in the same spirit as DefaultClarityGateResumeMaxActions.
const DefaultPriorArtGateResumeMaxActions = 10

// PriorArtGateResumeOptions configures SweepPriorArtGateResume.
type PriorArtGateResumeOptions struct {
	// MaxActions caps the issues acted on in one pass. Zero means
	// DefaultPriorArtGateResumeMaxActions.
	MaxActions int
	// Audit, when set, is called once per issue acted on.
	Audit func(PriorArtGateResumeEvent)
	// BotCloseTTL closes a clanker-filed park (another hive, a contributor
	// CLI, an external bot) as already existing once this long has passed
	// since the gate asked with no reply. Zero disables auto-close. A
	// human-filed park is never auto-closed.
	BotCloseTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// PriorArtGateResumeEvent records one issue the sweep acted on.
type PriorArtGateResumeEvent struct {
	Repo   string
	Number int
	// Actor replied to the gate: the issue author or a maintainer.
	Actor string
	// Decision is what the reply said: proceed (also a plain reply) or exists.
	Decision priorart.Decision
	// Delta is the `proceed` text recorded into the issue body.
	Delta      string
	CommentURL string
	Closed     bool
	// AutoClosed marks a clanker-filed park closed by the TTL, not a reply.
	AutoClosed bool
}

// PriorArtGateResumeResult reports what one pass did.
type PriorArtGateResumeResult struct {
	// Seen counts open issues carrying stage/017-possible-existing.
	Seen int
	// Resumed lists the issues un-parked (proceed or plain reply).
	Resumed []PriorArtGateResumeEvent
	// Closed lists the issues closed as already existing, by a reply or by
	// the clanker TTL (AutoClosed).
	Closed []PriorArtGateResumeEvent
	// Skipped counts issues whose reads or writes failed in this pass.
	Skipped int
}

// SweepPriorArtGateResume answers the prior-art gate's park once somebody
// replies (hivecommons/hive#13575). An open issue is acted on only when all
// of these hold, mirroring SweepClarityGateResume:
//
//   - it carries stage/017-possible-existing and a PriorArtGateMarker
//     comment written by one of this hive's own accounts;
//   - the newest `labeled` event for the stage label is by one of the
//     hive's own accounts, so a 017 a person added by hand is never lifted
//     here (they take it off by hand too);
//   - after both, the issue author, or a person other than the hive with
//     write, maintain or admin on the repo (checked live), commented, and
//     that comment has not been answered yet (unpark-reply marker).
//
// `/hive decision exists` closes the issue as not planned with the
// `already-exists` label and a reply pointing at the findings. Anything
// else — `/hive decision proceed — <delta>` or a plain reply — lifts the
// stage label, appends a "Scope vs existing" section recording the delta
// to the issue body, and records an Unparked fact so the issue re-derives
// stage/010-actionable and re-enters the queue. The gate's marker stays, so
// the scheduler does not ask again. A hive that cannot name its own
// accounts acts on nothing.
func (c *Client) SweepPriorArtGateResume(ctx context.Context, opts PriorArtGateResumeOptions) (*PriorArtGateResumeResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultPriorArtGateResumeMaxActions
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	result := &PriorArtGateResumeResult{}
	if !c.knowsOwnAccounts() {
		return result, nil
	}
	acted := func() int { return len(result.Resumed) + len(result.Closed) }
	for _, repo := range c.getRepos() {
		if acted() >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenIssuesWithLabel(ctx, owner, repoName, PossiblyExistsStageLabel)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if acted() >= maxActions {
				break
			}
			if issue == nil || issue.IsPullRequest() || !labelPresent(issueLabelNames(issue.Labels), PossiblyExistsStageLabel) {
				continue
			}
			result.Seen++
			event, err := c.tryPriorArtGateResume(ctx, repo, owner, repoName, issue, opts.BotCloseTTL, now())
			if err != nil {
				if c.logger != nil {
					c.logger.Warn("prior-art gate resume skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
				continue
			}
			if event == nil {
				continue
			}
			if event.Closed {
				result.Closed = append(result.Closed, *event)
			} else {
				result.Resumed = append(result.Resumed, *event)
			}
			if opts.Audit != nil {
				opts.Audit(*event)
			}
		}
	}
	return result, nil
}

// tryPriorArtGateResume handles one parked issue. It returns nil when the
// issue is not the gate's or nobody eligible has replied yet.
func (c *Client) tryPriorArtGateResume(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, botCloseTTL time.Duration, now time.Time) (*PriorArtGateResumeEvent, error) {
	number := issue.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	gate, ok := c.priorArtGateComment(comments)
	if !ok {
		return nil, nil
	}
	asked := gate.GetCreatedAt().Time
	replied := c.clarityGateHasLaterHumanComment(comments, asked)
	clanker := c.priorArtAuthorKind(issue) == priorart.AuthorClanker
	ttlDue := botCloseTTL > 0 && clanker && now.Sub(asked) >= botCloseTTL
	if !replied && !ttlDue {
		return nil, nil
	}
	events, err := c.listIssueLabelEvents(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	parked, ok := lastLabeledEvent(events, PossiblyExistsStageLabel)
	if !ok || !c.isHiveAccount(parked.Actor) {
		return nil, nil
	}
	if parked.At.After(asked) {
		asked = parked.At
	}
	if !replied {
		// A clanker that never answered: close after the TTL, measured
		// from the later of the ask and the park.
		if now.Sub(asked) < botCloseTTL {
			return nil, nil
		}
		event := &PriorArtGateResumeEvent{Repo: displayRepo, Number: number, Actor: safeGetLogin(issue.GetUser()), Decision: priorart.DecisionExists, CommentURL: gate.GetHTMLURL(), AutoClosed: true}
		return c.priorArtGateClose(ctx, displayRepo, owner, repo, number, gate, nil, event)
	}
	answered := c.answeredUnparkCommentIDs(comments)
	answer, err := c.priorArtGateAnswer(ctx, owner, repo, safeGetLogin(issue.GetUser()), comments, asked, answered)
	if err != nil || answer == nil {
		return nil, err
	}
	actor := safeGetLogin(answer.GetUser())
	reply := priorart.ParseReply(answer.GetBody())
	event := &PriorArtGateResumeEvent{Repo: displayRepo, Number: number, Actor: actor, Decision: reply.Decision, Delta: reply.Delta, CommentURL: answer.GetHTMLURL()}

	if reply.Decision == priorart.DecisionExists {
		return c.priorArtGateClose(ctx, displayRepo, owner, repo, number, gate, answer, event)
	}
	if err := c.RemoveLabel(ctx, displayRepo, number, PossiblyExistsStageLabel); err != nil {
		return nil, err
	}
	if reply.Decision == priorart.DecisionProceed {
		newBody := priorart.AppendScopeSection(issue.GetBody(), reply.Delta, gate.GetHTMLURL(), actor)
		if newBody != issue.GetBody() {
			if _, _, err := c.client.Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{Body: gh.Ptr(logscrub.ScrubString(newBody))}); err != nil {
				return nil, fmt.Errorf("recording scope on %s#%d: %w", displayRepo, number, err)
			}
		}
	}
	if err := c.StageFacts().Unparked(ctx, c.IssueStageRef(owner+"/"+repo, number), actor); err != nil {
		return nil, err
	}
	var body string
	switch reply.Decision {
	case priorart.DecisionProceed:
		body = fmt.Sprintf("Un-parked by @%s: the gap is recorded under **%s** in the issue body and the issue is back in the queue.", actor, strings.TrimPrefix(priorart.ScopeHeading, "## "))
	default:
		body = fmt.Sprintf("Un-parked by @%s's reply; the issue is back in the queue. The claiming agent should state what the existing work lacks before building.", actor)
	}
	if err := c.postUnparkReply(ctx, owner, repo, number, answer.GetID(), body); err != nil {
		return nil, err
	}
	if c.logger != nil {
		c.logger.Info("prior-art gate resumed issue after a reply", "repo", displayRepo, "issue", number, "actor", actor, "decision", reply.Decision)
	}
	return event, nil
}

// priorArtGateClose answers `/hive decision exists`: label, close as not
// planned, reply with the pointer. The 017 label comes off first so the
// Closed fact never sees an issue both closed and parked.
func (c *Client) priorArtGateClose(ctx context.Context, displayRepo, owner, repo string, number int, gate, answer *gh.IssueComment, event *PriorArtGateResumeEvent) (*PriorArtGateResumeEvent, error) {
	if err := c.AddLabels(ctx, displayRepo, number, []string{AlreadyExistsLabel}); err != nil {
		return nil, err
	}
	if err := c.RemoveLabel(ctx, displayRepo, number, PossiblyExistsStageLabel); err != nil {
		return nil, err
	}
	if err := c.CloseIssue(ctx, displayRepo, number, IssueCloseOptions{StateReason: "not_planned"}); err != nil {
		return nil, err
	}
	var body string
	if event.AutoClosed {
		body = fmt.Sprintf("%s\nClosed as already available: the author (@%s, an automated account) did not reply to the [findings](%s) above in time. Reopen with `/hive decision proceed — <what is missing>` if something there does not cover it.", priorart.AutoCloseMarker, event.Actor, gate.GetHTMLURL())
		if err := c.CreateIssueComment(ctx, displayRepo, number, body); err != nil {
			return nil, err
		}
	} else {
		body = fmt.Sprintf("Closed by @%s as already available — see the [findings](%s) above. Reopen with `/hive decision proceed — <what is missing>` if something there does not cover it.", event.Actor, gate.GetHTMLURL())
		if err := c.postUnparkReply(ctx, owner, repo, number, answer.GetID(), body); err != nil {
			return nil, err
		}
	}
	if c.logger != nil {
		c.logger.Info("prior-art gate closed issue as already existing", "repo", displayRepo, "issue", number, "actor", event.Actor, "auto", event.AutoClosed)
	}
	event.Closed = true
	return event, nil
}

// priorArtAuthorKind classifies the issue's author for the gate: the
// `author/*` label when present, else the hive's own-account and bot
// heuristics (priorart.ClassifyAuthor).
func (c *Client) priorArtAuthorKind(issue *gh.Issue) priorart.AuthorKind {
	user := issue.GetUser()
	return priorart.ClassifyAuthor(priorart.Authorship{
		Login:         safeGetLogin(user),
		IsHuman:       c.isHumanAuthor(user),
		IsHiveAccount: c.isHiveAccount(user),
		Labels:        issueLabelNames(issue.Labels),
	})
}

// priorArtGateComment returns the gate's park comment: the oldest
// PriorArtGateMarker comment written by one of the hive's own accounts.
func (c *Client) priorArtGateComment(comments []*gh.IssueComment) (*gh.IssueComment, bool) {
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), PriorArtGateMarker) || !c.isHiveAccount(cm.GetUser()) {
			continue
		}
		return cm, true
	}
	return nil, false
}

// priorArtGateAnswer is clarityGateAnswer that also skips comments the hive
// has already answered, so a reply never acts twice across restarts.
func (c *Client) priorArtGateAnswer(ctx context.Context, owner, repo, author string, comments []*gh.IssueComment, after time.Time, answered map[int64]bool) (*gh.IssueComment, error) {
	checked := 0
	for i := len(comments) - 1; i >= 0; i-- {
		cm := comments[i]
		if cm == nil || !cm.GetCreatedAt().Time.After(after) || !c.clarityGatePerson(cm.GetUser()) || answered[cm.GetID()] {
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
