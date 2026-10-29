package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// DefaultClosedIssueStageSweepWindow is how far back a first pass (or a pass
// after a long gap) looks for closed issues still wearing hive stage labels.
const DefaultClosedIssueStageSweepWindow = 48 * time.Hour

// DefaultClosedIssueStageSweepMaxActions caps the issues one pass writes to.
const DefaultClosedIssueStageSweepMaxActions = 20

// ClosedIssueStageSweepOptions configures SweepClosedIssueStageLabels.
type ClosedIssueStageSweepOptions struct {
	// Since bounds the listing to issues updated after it. Zero means
	// now minus DefaultClosedIssueStageSweepWindow.
	Since time.Time
	// MaxActions caps the issues written to in one pass. Zero means
	// DefaultClosedIssueStageSweepMaxActions.
	MaxActions int
	// Audit, when set, is called once per issue cleaned.
	Audit func(ClosedIssueStageEvent)
}

// ClosedIssueStageEvent records one closed issue whose hive labels were
// cleared, for the audit log.
type ClosedIssueStageEvent struct {
	Repo   string
	Number int
	// Stage lists the stage/* labels the issue carried; NeedsHuman is true
	// when the hive's own `needs-human` came off with them.
	Stage      []string
	NeedsHuman bool
}

// ClosedIssueStageSweepResult reports what one pass did.
type ClosedIssueStageSweepResult struct {
	// Seen counts closed issues examined.
	Seen int
	// Cleaned lists the issues whose labels were cleared.
	Cleaned []ClosedIssueStageEvent
	// Skipped counts issues whose clean-up failed in this pass.
	Skipped int
}

// SweepClosedIssueStageLabels takes the hive's park and stage labels off
// issues that closed while carrying them. The issue-stage reconciler only
// enumerates open issues, so an issue closed by a merged PR's `Fixes #N`, by
// a maintainer by hand, or by `/fixed` kept `stage/045-needs-decision` (and
// a hive-applied `needs-human`) and the dashboard tracker went on reading
// "Needs human decision" for it (hivecommons/homebrew-hive#127). Each hit
// is one Closed fact to the stage engine, whose writer removes every
// stage/* label; the hive's own `needs-human` (applied by one of its
// accounts, read from the timeline) comes off too, a person's stays, and
// `hold` is never touched. Pull requests are not this sweep's business.
func (c *Client) SweepClosedIssueStageLabels(ctx context.Context, opts ClosedIssueStageSweepOptions) (*ClosedIssueStageSweepResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultClosedIssueStageSweepMaxActions
	}
	since := opts.Since
	if since.IsZero() {
		since = time.Now().Add(-DefaultClosedIssueStageSweepWindow)
	}
	result := &ClosedIssueStageSweepResult{}
	// activeRepos, not getRepos: a paused repo keeps its labels untouched (#6203).
	for _, repo := range c.activeRepos() {
		if len(result.Cleaned) >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listClosedIssuesSince(ctx, owner, repoName, since)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Cleaned) >= maxActions {
				break
			}
			if issue == nil || issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "closed") {
				continue
			}
			labels := issueLabelNames(issue.Labels)
			stage := stagelabels.StageLabels(labels)
			needsHuman := hasIssueNeedsHumanLabel(labels)
			retired := closedIssueRetiredParkLabels(labels)
			if len(stage) == 0 && !needsHuman && len(retired) == 0 {
				continue
			}
			result.Seen++
			event, err := c.clearClosedIssueStageLabels(ctx, repo, owner, repoName, issue.GetNumber(), stage, retired, needsHuman)
			if err != nil {
				if c.logger != nil {
					c.logger.Warn("closed-issue stage sweep skipped issue", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
				continue
			}
			if event == nil {
				continue
			}
			result.Cleaned = append(result.Cleaned, *event)
			if opts.Audit != nil {
				opts.Audit(*event)
			}
		}
	}
	return result, nil
}

// listClosedIssuesSince is one page per 100 closed issues updated after
// since, newest activity first — the recent closes this sweep exists for.
func (c *Client) listClosedIssuesSince(ctx context.Context, owner, repo string, since time.Time) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Since:       since,
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, issues...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// closedIssueRetiredParkLabels are the non-stage park spellings the hive
// wrote itself and so owns on a closed issue.
func closedIssueRetiredParkLabels(labels []string) []string {
	var out []string
	for _, l := range unparkRemovableLabels {
		if !stagelabels.IsStageLabel(l) && labelPresent(labels, l) {
			out = append(out, l)
		}
	}
	return out
}

// clearClosedIssueStageLabels does the writes for one closed issue. The
// hive's own `needs-human` lifts; a person's or an unattributed one stays
// (the applier is read from the timeline, and not knowing is never
// permission to remove a person's label). It returns nil when nothing
// came off.
func (c *Client) clearClosedIssueStageLabels(ctx context.Context, displayRepo, owner, repo string, number int, stage, retired []string, needsHuman bool) (*ClosedIssueStageEvent, error) {
	event := &ClosedIssueStageEvent{Repo: displayRepo, Number: number, Stage: stage}
	if needsHuman {
		if out := c.needsHumanOutcome(ctx, owner, repo, number, nil); out.applier == stageengine.NeedsHumanApplierHive {
			if err := c.removeIssueLabel(ctx, owner, repo, number, issueNeedsHumanLabel); err != nil {
				return nil, err
			}
			event.NeedsHuman = true
		}
	}
	for _, label := range retired {
		if err := c.removeIssueLabel(ctx, owner, repo, number, label); err != nil {
			return nil, err
		}
	}
	if len(stage) > 0 {
		if err := c.StageFacts().Closed(ctx, c.IssueStageRef(owner+"/"+repo, number)); err != nil {
			return nil, err
		}
	}
	if len(stage) == 0 && len(retired) == 0 && !event.NeedsHuman {
		return nil, nil
	}
	if c.logger != nil {
		c.logger.Info("closed issue: hive stage labels cleared", "repo", displayRepo, "issue", number, "stage", stage, "needs_human", event.NeedsHuman)
	}
	return event, nil
}
