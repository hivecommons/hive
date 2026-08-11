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

// hiveQueueApproval separates the two identities an auto-merge approval
// carries, because they answer different questions and have different trust.
//
//   - QueuedBy is the GitHub account that SUBMITTED the review. GitHub
//     attributes it, a caller cannot choose it, and it is the only one of the
//     two that may carry merge authority.
//   - ClaimedBy is the name written in the review BODY. On the hosted path the
//     hive posts the approval as its App bot on a human's behalf, so this is
//     the only record of which human asked. It is caller-controlled text and
//     must never gate anything — it is used solely to keep the self-merge ban
//     working, where treating it as untrusted is safe: the worst a forged
//     value can do is ban a merge that would otherwise be allowed.
type hiveQueueApproval struct {
	QueuedBy  string
	ClaimedBy string
	HeadSHA   string
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
	// The self-merge ban must catch BOTH shapes of approval: a human approving
	// directly (author == the submitting actor) and the hosted path, where the
	// hive posts as its App bot on a human's behalf so the only trace of that
	// human is the claimed name in the body. Checking the actor alone would
	// silently retire the ban on the hosted path, since a bot login never
	// equals a human author. ClaimedBy is untrusted input, but only ever
	// widens the ban — a forged value can block a merge, never permit one.
	if strings.EqualFold(author, queuedBy) || strings.EqualFold(author, approval.ClaimedBy) {
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
			// SECURITY (audit F3, CWE-863): the merge authority is the actor who
			// SUBMITTED the review, never a name parsed out of its body.
			//
			// This used to read queuedBy from parseHiveQueueReview(review.Body).
			// Contributor tokens carry pull-request write permission, so any
			// contributor could approve their own PR with a body reading
			// "Approved by @trusted-maintainer ..." and merge under that name.
			// The self-merge ban made it worse rather than better: it compares
			// the PR author against the CLAIMED name, so naming someone else
			// both forged the authority and slipped the ban.
			//
			// The body marker is still REQUIRED — it is what distinguishes an
			// ordinary code-review approval from an explicit request to queue
			// for auto-merge, and an approving reviewer must opt in deliberately.
			// It just no longer decides WHO did it.
			if marker := parseHiveQueueReview(review.GetBody()); marker != "" {
				actor := safeGetLogin(review.GetUser())
				if actor == "" {
					// No identifiable actor (deleted account, or an API shape we
					// do not recognise) — fail closed rather than fall back to
					// the body, which is exactly the trust we are removing.
					continue
				}
				latest = hiveQueueApproval{
					QueuedBy:  actor,
					ClaimedBy: marker,
					HeadSHA:   review.GetCommitID(),
				}
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
