package github

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// DefaultQuestionAutoCloseMaxActions caps the closes/relabels/marks a single
// SweepAnsweredQuestions tick will perform across all repos, mirroring
// DefaultSupersessionSweepMaxActions and DefaultTaskListSweepMaxCloses. A
// runaway sweep must never spam close-and-comment across the fleet.
const DefaultQuestionAutoCloseMaxActions = 5

// questionAutoCloseLabel is the only label this sweep ever touches (#9584).
// Bugs, enhancements and anything without this label are never auto-closed,
// whatever else is true about the issue.
const questionAutoCloseLabel = "kind/question"

// questionAutoCloseMarker is the invisible HTML-comment tag the sweep stamps
// onto the newest hive-authored comment on a kind/question issue, mirroring
// supersessionSweepMarker and taskListSweepMarker. Its presence is what turns
// a plain hive reply into "the answer that starts the auto-close clock" —
// until it is there, the sweep has nothing to schedule against.
const questionAutoCloseMarker = "<!-- hive:question-autoclose -->"

// questionAutoCloseFooter is appended, once, right after the marker, to the
// newest hive comment on an open kind/question issue. It is the user-facing
// half of the acceptance criteria in #9584: the author has to be told, in the
// comment itself, exactly how to keep the issue open.
const questionAutoCloseFooter = "\n\n" + questionAutoCloseMarker + "\n" +
	"If this doesn't answer your question, react 👎 to this comment and the issue will stay open."

// questionAutoCloseWindowEnv lets an operator override the default 4-hour
// deadline from the issue without a code change, mirroring
// attributedClosedPRLookbackEnv's env-first, default-fallback shape.
const questionAutoCloseWindowEnv = "HIVE_QUESTION_AUTOCLOSE_HOURS"

// DefaultQuestionAutoCloseWindow is the deadline #9584 asks for absent an
// override: "a configurable window (HIVE_QUESTION_AUTOCLOSE_HOURS, default 4)".
const DefaultQuestionAutoCloseWindow = 4 * time.Hour

// QuestionAutoCloseWindow resolves the configured auto-close deadline. It
// accepts a plain integer number of hours (the form the issue names) or any
// Go duration string, and falls back to DefaultQuestionAutoCloseWindow for an
// unset, empty or unparsable value — never a zero or negative window.
func QuestionAutoCloseWindow() time.Duration {
	raw := strings.TrimSpace(os.Getenv(questionAutoCloseWindowEnv))
	if raw == "" {
		return DefaultQuestionAutoCloseWindow
	}
	if hours, err := strconv.Atoi(raw); err == nil && hours > 0 {
		return time.Duration(hours) * time.Hour
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return DefaultQuestionAutoCloseWindow
}

// QuestionAutoCloseOptions configures one SweepAnsweredQuestions tick.
type QuestionAutoCloseOptions struct {
	MaxActions int
	// Window overrides QuestionAutoCloseWindow() for this tick, mainly so
	// tests can use a synthetic deadline instead of real env state.
	Window time.Duration
	Audit  func(QuestionAutoCloseEvent)
}

// QuestionAutoCloseEvent describes one sweep action for the audit sink.
// Action is one of "marked" (the sweep stamped the hive's answer and started
// the clock), "relabeled" (the author objected, so the issue was handed to a
// human instead of closed) or "closed".
type QuestionAutoCloseEvent struct {
	Repo   string
	Number int
	Author string
	Action string
}

// QuestionAutoCloseResult tallies one SweepAnsweredQuestions tick.
type QuestionAutoCloseResult struct {
	Marked    []QuestionAutoCloseEvent
	Relabeled []QuestionAutoCloseEvent
	Closed    []QuestionAutoCloseEvent
	Seen      int
	Skipped   int
}

func (r *QuestionAutoCloseResult) actions() int {
	return len(r.Marked) + len(r.Relabeled) + len(r.Closed)
}

// SweepAnsweredQuestions implements #9584: a kind/question issue whose newest
// comment is the hive's own answer gets that comment stamped with a marker
// and a downvote-to-reopen footer, then — absent a 👎 from the issue's author
// or any further comment from anyone — is closed as "completed" once the
// configured window has elapsed since the answer was posted.
//
// Deliberately stateless: every decision is re-derived from GitHub's own
// state (the label, the comment list, the reactions on the marked comment)
// on every tick, so the schedule needs no separate store and is automatically
// durable across restarts — a hive that reboots mid-window simply re-reads
// the same marked comment's timestamp next tick and picks up where it left
// off (#9584 acceptance: "the schedule survives a restart").
//
// A reply from anyone after the marked comment — including the author simply
// asking a follow-up instead of reacting — cancels the schedule for good: the
// hive's marked comment is no longer the newest comment, so the sweep has
// nothing to act on until the hive answers again and gets a fresh marker.
func (c *Client) SweepAnsweredQuestions(ctx context.Context, opts QuestionAutoCloseOptions) (*QuestionAutoCloseResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultQuestionAutoCloseMaxActions
	}
	window := opts.Window
	if window <= 0 {
		window = QuestionAutoCloseWindow()
	}
	result := &QuestionAutoCloseResult{}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	now := time.Now()

	for _, repo := range c.getRepos() {
		if result.actions() >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenQuestionIssues(ctx, owner, repoName)
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
			result.Seen++
			event, action, err := c.trySweepAnsweredQuestion(ctx, repo, owner, repoName, issue, identity, now, window)
			if err != nil {
				if c.logger != nil {
					c.logger.Warn("question autoclose sweep skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
				continue
			}
			switch action {
			case "marked":
				result.Marked = append(result.Marked, event)
			case "relabeled":
				result.Relabeled = append(result.Relabeled, event)
			case "closed":
				result.Closed = append(result.Closed, event)
			default:
				result.Skipped++
				continue
			}
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

func (c *Client) listOpenQuestionIssues(ctx context.Context, owner, repo string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{questionAutoCloseLabel},
		ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
	}
	var all []*gh.Issue
	for page := 0; page < claimSearchMaxPages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing %s issues for %s/%s: %w", questionAutoCloseLabel, owner, repo, err)
		}
		all = append(all, issues...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return all, nil
}

func (c *Client) trySweepAnsweredQuestion(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, identity HiveIdentity, now time.Time, window time.Duration) (QuestionAutoCloseEvent, string, error) {
	labels := issueLabelNames(issue.Labels)
	if c.isHeld(labels) || c.isExempt(labels) {
		return QuestionAutoCloseEvent{}, "", nil
	}

	comments, err := c.listAllIssueComments(ctx, owner, repo, issue.GetNumber())
	if err != nil {
		return QuestionAutoCloseEvent{}, "", err
	}
	if len(comments) == 0 {
		// Nothing has answered this question yet; there is no clock to start.
		return QuestionAutoCloseEvent{}, "", nil
	}
	last := comments[len(comments)-1]
	if last == nil || !identity.Matches(safeGetLogin(last.GetUser())) {
		// The newest comment is not the hive's: either nobody has answered
		// yet, or someone replied after the hive did. Either way the
		// schedule is not running.
		return QuestionAutoCloseEvent{}, "", nil
	}

	event := QuestionAutoCloseEvent{
		Repo:   displayRepo,
		Number: issue.GetNumber(),
		Author: safeGetLogin(issue.GetUser()),
	}

	if !strings.Contains(last.GetBody(), questionAutoCloseMarker) {
		body := last.GetBody() + questionAutoCloseFooter
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, last.GetID(), &gh.IssueComment{Body: &body}); err != nil {
			return event, "", fmt.Errorf("marking answer comment on %s#%d: %w", displayRepo, issue.GetNumber(), err)
		}
		event.Action = "marked"
		return event, "marked", nil
	}

	objected, err := c.commentHasAuthorObjection(ctx, owner, repo, last, event.Author)
	if err != nil {
		return event, "", err
	}
	if objected {
		if hasLabel(labels, issueNeedsHumanLabel) {
			// Already handed to a human on a prior tick; nothing new to do.
			return QuestionAutoCloseEvent{}, "", nil
		}
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, issue.GetNumber(), []string{issueNeedsHumanLabel}); err != nil {
			return event, "", fmt.Errorf("labeling objected question %s#%d: %w", displayRepo, issue.GetNumber(), err)
		}
		event.Action = "relabeled"
		return event, "relabeled", nil
	}

	deadline := last.GetCreatedAt().Time.Add(window)
	if now.Before(deadline) {
		return QuestionAutoCloseEvent{}, "", nil
	}

	closedReason := "completed"
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, issue.GetNumber(), &gh.IssueRequest{
		State:       gh.Ptr("closed"),
		StateReason: &closedReason,
	}); err != nil {
		return event, "", fmt.Errorf("closing answered question %s#%d: %w", displayRepo, issue.GetNumber(), err)
	}
	event.Action = "closed"
	return event, "closed", nil
}

// listAllIssueComments pages through every comment on an issue, oldest first
// (GitHub's default order), so the caller can treat the last element as "the
// newest comment" the way trySweepAnsweredQuestion does.
func (c *Client) listAllIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
	}
	var all []*gh.IssueComment
	for page := 0; page < claimSearchMaxPages; page++ {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		all = append(all, comments...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return all, nil
}

// commentHasAuthorObjection reports whether the issue's author reacted 👎
// ("-1") to the hive's marked answer comment. It skips the reactions API call
// entirely when the comment's own summary count says there are no -1
// reactions at all, which is the common case and saves an extra request on
// every tick for every already-answered, not-yet-objected-to question.
func (c *Client) commentHasAuthorObjection(ctx context.Context, owner, repo string, comment *gh.IssueComment, author string) (bool, error) {
	if comment.GetReactions().GetMinusOne() == 0 {
		return false, nil
	}
	opts := &gh.ListReactionOptions{
		Content:     "-1",
		ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
	}
	for page := 0; page < claimSearchMaxPages; page++ {
		reactions, resp, err := c.client.Reactions.ListIssueCommentReactions(ctx, owner, repo, comment.GetID(), opts)
		if err != nil {
			return false, fmt.Errorf("listing reactions on comment %d in %s/%s: %w", comment.GetID(), owner, repo, err)
		}
		for _, r := range reactions {
			if r == nil {
				continue
			}
			if strings.EqualFold(safeGetLogin(r.User), author) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return false, nil
}

// hasLabel reports whether labels contains name, case-insensitively.
func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}
