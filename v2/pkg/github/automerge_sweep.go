package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const DefaultAutoMergeSweepMaxMerges = 3

var hiveQueueReviewRE = regexp.MustCompile(`(?i)^Approved by @([A-Za-z0-9-]+) for Hive auto-merge on green CI\.`)

type AutoMergeSweepOptions struct {
	MaxMerges int
	Audit     func(AutoMergeSweepEvent)
}

type AutoMergeSweepEvent struct {
	Repo     string
	Number   int
	Author   string
	QueuedBy string
	HeadSHA  string
	MergeSHA string
	Label    string
}

type AutoMergeSweepResult struct {
	Merged  []AutoMergeSweepEvent
	Seen    int
	Skipped int
}

type hiveQueueApproval struct {
	QueuedBy string
	HeadSHA  string
}

// SweepQueuedAutoMerges consumes the configured Hive merger-queue label. It
// only squashes open, labelled, non-draft PRs in managed repos after GitHub
// reports them mergeable, commit statuses/check-runs are green, and the latest
// Hive queue approval body proves the queuer is not the PR author.
func (c *Client) SweepQueuedAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	label := c.AutoMergeLabel()
	result := &AutoMergeSweepResult{}

	for _, repo := range c.getRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listQueuedPullRequestIssues(ctx, owner, repoName, label)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Merged) >= maxMerges {
				break
			}
			if issue == nil || !issue.IsPullRequest() {
				continue
			}
			result.Seen++
			event, reason, err := c.trySweepQueuedPR(ctx, repo, owner, repoName, issue.GetNumber(), label)
			if err != nil {
				c.warn("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				result.Skipped++
				continue
			}
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

func (c *Client) listQueuedPullRequestIssues(ctx context.Context, owner, repo, label string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing queued PRs for %s/%s: %w", owner, repo, err)
		}
		all = append(all, issues...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (c *Client) trySweepQueuedPR(ctx context.Context, displayRepo, owner, repo string, number int, label string) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	if !hasLabel(extractPRLabels(pr.Labels), label) {
		return AutoMergeSweepEvent{}, "label-removed", nil
	}

	author := safeGetLogin(pr.GetUser())
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	approval, ok, err := c.latestHiveQueueApproval(ctx, owner, repo, number)
	if err != nil {
		return AutoMergeSweepEvent{}, "queue-approval-check", err
	}
	if !ok {
		return AutoMergeSweepEvent{}, "no-hive-queue-approval", nil
	}
	if approval.HeadSHA == "" {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval is missing a reviewed head SHA — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-missing-head", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-missing-head", nil
	}
	if approval.HeadSHA != headSHA {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval head changed since approval — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-head-changed", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-head-changed", nil
	}
	queuedBy := approval.QueuedBy
	if strings.EqualFold(author, queuedBy) {
		return AutoMergeSweepEvent{}, "self-merge-ban", nil
	}

	mergeable := mergeableFromState(pr.GetMergeableState(), pr.Mergeable)
	if mergeable != MergeableYes {
		return AutoMergeSweepEvent{}, "not-mergeable", nil
	}
	green, reason, err := c.commitGreen(ctx, owner, repo, headSHA)
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	mergeResult, _, err := c.client.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
		SHA:         headSHA,
		MergeMethod: "squash",
	})
	if err != nil {
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: queuedBy,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Label:    label,
	}
	c.info("automerge sweep merged PR", "repo", displayRepo, "pr", number, "queued_by", queuedBy, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Client) latestHiveQueueApproval(ctx context.Context, owner, repo string, number int) (hiveQueueApproval, bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := hiveQueueApproval{}
	for {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return hiveQueueApproval{}, false, err
		}
		for _, review := range reviews {
			if !strings.EqualFold(review.GetState(), "APPROVED") {
				continue
			}
			if queuedBy := parseHiveQueueReview(review.GetBody()); queuedBy != "" {
				latest = hiveQueueApproval{QueuedBy: queuedBy, HeadSHA: review.GetCommitID()}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, latest.QueuedBy != "", nil
}

func (c *Client) invalidateQueuedAutoMerge(ctx context.Context, owner, repo string, number int, label, body string) error {
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return fmt.Errorf("removing %s label: %w", label, err)
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("commenting on stale auto-merge approval: %w", err)
	}
	return nil
}

func parseHiveQueueReview(body string) string {
	matches := hiveQueueReviewRE.FindStringSubmatch(strings.TrimSpace(body))
	if len(matches) != 2 {
		return ""
	}
	return matches[1]
}

// commitGreen reports whether every non-meta commit status and check run on
// the head SHA has actually succeeded. Pending is NOT green: the sweep runs
// every minute and a queue action usually lands seconds after the PR opens,
// so treating in-flight CI as green would squash the PR before its first CI
// run finishes — mergeable_state cannot catch that, because "unstable"
// (non-required checks outstanding) deliberately maps to MergeableYes and
// managed repos generally have no branch protection making any check
// required. Meta gates (tide, netlify previews, ...) are excluded on both
// surfaces — tide in particular posts a commit status that stays "pending"
// forever on Prow-managed repos and must never wedge the queue.
func (c *Client) commitGreen(ctx context.Context, owner, repo, sha string) (bool, string, error) {
	statusOpts := &gh.ListOptions{PerPage: 100}
	for {
		status, resp, err := c.client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			return false, "status-check", err
		}
		for _, s := range status.Statuses {
			if isMetaCheck(s.GetContext()) {
				continue
			}
			switch s.GetState() {
			case "success":
			case "pending":
				return false, "status-pending", nil
			default: // "failure", "error"
				return false, "status-" + s.GetState(), nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		checkRuns, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return false, "check-runs", err
		}
		for _, cr := range checkRuns.CheckRuns {
			if isMetaCheck(cr.GetName()) {
				continue
			}
			if cr.GetStatus() != "completed" {
				return false, "check-pending", nil
			}
			switch cr.GetConclusion() {
			case "success", "neutral", "skipped":
			default:
				return false, "check-" + cr.GetConclusion(), nil
			}
		}
		if resp.NextPage == 0 {
			return true, "", nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return true
		}
	}
	return false
}

func isGitHubStatus(err error, status int) bool {
	ghErr, ok := err.(*gh.ErrorResponse)
	return ok && ghErr.Response != nil && ghErr.Response.StatusCode == status
}

func (c *Client) warn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

func (c *Client) info(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}
