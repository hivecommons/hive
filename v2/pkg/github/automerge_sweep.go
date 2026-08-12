package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const DefaultAutoMergeSweepMaxMerges = 3

// selfAuthoredAutoMergeSweepInterval is how often
// StartSelfAuthoredAutoMergeSweep re-scans the App's own open PRs. Matches
// the human-queue merge-request watcher's cadence (mergeRequestPollInterval):
// merges are latency-sensitive (an eligible PR should land quickly) but a
// tight loop risks GitHub secondary rate limits across every managed repo.
const selfAuthoredAutoMergeSweepInterval = 10 * time.Second

const (
	autoMergeReasonNoHiveQueueApproval        = "no-hive-queue-approval"
	autoMergeReasonNoAppBotLogin              = "no-app-bot-login"
	autoMergeReasonUntrustedQueueApproval     = "untrusted-hive-queue-approval"
	autoMergeWarnNoAppBotLogin                = "automerge sweep disabled: no GitHub App bot login configured"
	autoMergeWarnUntrustedQueueApproval       = "rejected untrusted Hive auto-merge queue approval"
	autoMergeNoAppBotLoginOperatorRemediation = "Hive has no usable GitHub App, so App-authorship cannot be verified and auto-merge is disabled"
)

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
// Hive App-authored queue approval proves the queuer is not the PR author.
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
	noAppBotLoginWarned := false

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
			if reason == autoMergeReasonNoAppBotLogin {
				if !noAppBotLoginWarned {
					c.warn(autoMergeWarnNoAppBotLogin, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoAppBotLoginOperatorRemediation)
					noAppBotLoginWarned = true
				}
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

// SweepSelfAuthoredAutoMerges merges the App's OWN open PRs directly on green
// CI, without a human "Approved ... for Hive auto-merge" queue review and
// without waiting on tide.
//
// Why this must exist as a SEPARATE path from SweepQueuedAutoMerges: Prow
// structurally forbids self-approval — tide requires lgtm+approved labels
// from a reviewer distinct from the PR author, and the author here is always
// the App itself. A human queuer can supply that for someone else's PR (the
// existing sweep above), but nobody can supply it for the App's own PR: the
// App cannot review its own work, and asking a human to rubber-stamp every
// App PR defeats the point of automation. So the App must self-merge
// directly over the GitHub REST API (squash), the same bypass-tide mechanism
// SweepQueuedAutoMerges already uses for tide-pending/unstable states (see
// mergeableFromState) — this path just skips the human-queue-approval lookup
// entirely rather than needing one.
//
// Every OTHER safety property is identical to the human queue: mergeability
// (mergeableFromState), green required checks (commitGreen), and a head-SHA
// re-check immediately before the merge call so a push landing between
// enumeration and merge can never be squashed unreviewed — mirrored below via
// re-fetching the PR right before calling Merge and comparing SHAs, the same
// pattern trySweepQueuedPR uses via the queue-approval's recorded HeadSHA.
// There is no queuedBy in this path, so the author==queuedBy self-merge-ban
// in trySweepQueuedPR simply does not apply — there is no queuer to compare
// against.
func (c *Client) SweepSelfAuthoredAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	result := &AutoMergeSweepResult{}
	if strings.TrimSpace(c.appBotLogin) == "" {
		// No usable App identity: there is no "self" to authenticate PRs as
		// App-authored, so this sweep has nothing safe to do. Warn once per
		// call (matching the human-queue sweep's per-call warn cadence) rather
		// than per-repo, since the cause is hive-wide, not per-repo.
		c.warn(autoMergeWarnNoAppBotLogin, "reason", autoMergeReasonNoAppBotLogin, "cause", autoMergeNoAppBotLoginOperatorRemediation)
		return result, nil
	}

	for _, repo := range c.getRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.splitRepo(repo)
		prs, err := c.listOpenAppAuthoredPullRequests(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			result.Seen++
			event, reason, err := c.trySweepSelfAuthoredPR(ctx, repo, owner, repoName, pr.GetNumber())
			if err != nil {
				c.warn("self-authored automerge sweep skipped PR", "repo", repo, "pr", pr.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("self-authored automerge sweep skipped PR", "repo", repo, "pr", pr.GetNumber(), "reason", reason)
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

// StartSelfAuthoredAutoMergeSweep runs a loop that periodically calls
// SweepSelfAuthoredAutoMerges. It returns immediately; the loop runs until ctx
// is cancelled. A nil client is a no-op. maxMerges is passed straight through
// to AutoMergeSweepOptions.MaxMerges (<=0 falls back to
// DefaultAutoMergeSweepMaxMerges there). Mirrors
// StartMergeRequestWatcher/StartPRRequestWatcher's own-ticker-goroutine
// pattern so all three App-identity-dependent watchers share one shape.
func (c *Client) StartSelfAuthoredAutoMergeSweep(ctx context.Context, maxMerges int) {
	if c == nil {
		return
	}
	go func() {
		t := time.NewTicker(selfAuthoredAutoMergeSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := c.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: maxMerges}); err != nil {
					c.warn("self-authored automerge sweep failed", "error", err)
				}
			}
		}
	}()
	c.info("self-authored automerge sweep started", "interval", selfAuthoredAutoMergeSweepInterval)
}

// listOpenAppAuthoredPullRequests returns every open, non-draft PR in owner/repo
// authored by the App bot login. Uses the PR list endpoint (not issue search)
// because the caller needs PullRequest objects (head SHA, mergeable state)
// for every candidate, not just issue metadata.
func (c *Client) listOpenAppAuthoredPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []*gh.PullRequest
	for {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open PRs for %s/%s: %w", owner, repo, err)
		}
		for _, pr := range prs {
			if pr.GetDraft() {
				continue
			}
			if !strings.EqualFold(safeGetLogin(pr.GetUser()), c.appBotLogin) {
				continue
			}
			out = append(out, pr)
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// trySweepSelfAuthoredPR evaluates and, if eligible, merges one App-authored
// PR. Re-fetches the PR immediately before merging to re-verify the head SHA
// against the one that was evaluated as green — the same
// evaluated-then-re-verified-at-merge-time safety property trySweepQueuedPR
// gets from the queue approval's recorded HeadSHA, just without a stored
// approval record to compare against (there is no queue step in this path).
func (c *Client) trySweepSelfAuthoredPR(ctx context.Context, displayRepo, owner, repo string, number int) (AutoMergeSweepEvent, string, error) {
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
	author := safeGetLogin(pr.GetUser())
	if !strings.EqualFold(author, c.appBotLogin) {
		// Not the App's own PR: this path never touches non-App-authored PRs,
		// matching the human-queue sweep's untouched behavior for PRs it does
		// not own. Defense in depth — listOpenAppAuthoredPullRequests already
		// filtered on author, but a PR can change hands (rare, but GitHub
		// permits transferring PR authorship attribution in some flows) between
		// listing and evaluating it here.
		return AutoMergeSweepEvent{}, "not-app-authored", nil
	}

	evaluatedHeadSHA := ""
	if pr.GetHead() != nil {
		evaluatedHeadSHA = pr.GetHead().GetSHA()
	}
	if evaluatedHeadSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}

	mergeable := mergeableFromState(pr.GetMergeableState(), pr.Mergeable)
	if mergeable != MergeableYes {
		return AutoMergeSweepEvent{}, "not-mergeable", nil
	}
	green, reason, err := c.commitGreen(ctx, owner, repo, evaluatedHeadSHA)
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Re-verify the head SHA immediately before merging: a push landing
	// between the green-check above and the merge call below must never be
	// squashed without having gone through commitGreen itself.
	current, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	currentHeadSHA := ""
	if current.GetHead() != nil {
		currentHeadSHA = current.GetHead().GetSHA()
	}
	if currentHeadSHA == "" || currentHeadSHA != evaluatedHeadSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}

	mergeResult, _, err := c.client.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
		SHA:         evaluatedHeadSHA,
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
		QueuedBy: "", // no queuer in the self-authored path — the App merges its own PR
		HeadSHA:  evaluatedHeadSHA,
		MergeSHA: mergeResult.GetSHA(),
	}
	c.info("self-authored automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "merge_sha", event.MergeSHA)
	return event, "", nil
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
	if strings.TrimSpace(c.appBotLogin) == "" {
		return AutoMergeSweepEvent{}, autoMergeReasonNoAppBotLogin, nil
	}
	approval, ok, reason, err := c.latestHiveQueueApproval(ctx, owner, repo, number)
	if err != nil {
		return AutoMergeSweepEvent{}, "queue-approval-check", err
	}
	if !ok {
		if reason != "" {
			return AutoMergeSweepEvent{}, reason, nil
		}
		return AutoMergeSweepEvent{}, autoMergeReasonNoHiveQueueApproval, nil
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

func (c *Client) latestHiveQueueApproval(ctx context.Context, owner, repo string, number int) (hiveQueueApproval, bool, string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := hiveQueueApproval{}
	untrusted := false
	for {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return hiveQueueApproval{}, false, "", err
		}
		for _, review := range reviews {
			if !strings.EqualFold(review.GetState(), "APPROVED") {
				continue
			}
			queuedBy := parseHiveQueueReview(review.GetBody())
			if queuedBy == "" {
				continue
			}
			if !c.isHiveAppReviewAuthor(review) {
				untrusted = true
				c.warn(autoMergeWarnUntrustedQueueApproval, "owner", owner, "repo", repo, "pr", number, "review_author", safeGetLogin(review.GetUser()), "claimed_queued_by", queuedBy, "expected_app_bot", c.appBotLogin)
				continue
			}
			latest = hiveQueueApproval{QueuedBy: queuedBy, HeadSHA: review.GetCommitID()}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if latest.QueuedBy != "" {
		return latest, true, "", nil
	}
	if untrusted {
		return latest, false, autoMergeReasonUntrustedQueueApproval, nil
	}
	return latest, false, "", nil
}

func (c *Client) isHiveAppReviewAuthor(review *gh.PullRequestReview) bool {
	if c == nil || review == nil || strings.TrimSpace(c.appBotLogin) == "" {
		return false
	}
	return strings.EqualFold(safeGetLogin(review.GetUser()), c.appBotLogin)
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
