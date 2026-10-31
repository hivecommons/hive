package github

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type MergeFailureAlertSink interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlert(id string)
}

type mergeFailureAlert struct {
	key     string
	message string
}

// mergeAlertEntry remembers what raised a system alert so the periodic
// revalidation can retire it once the blocking condition is gone.
type mergeAlertEntry struct {
	key    string
	number int
	head   string
}

// mergeAlertRevalidateInterval bounds how often the merge-request watcher
// re-checks live "merge blocked" alerts against GitHub. Package-level so
// tests can shorten it.
var mergeAlertRevalidateInterval = 5 * time.Minute

func (c *Client) SetMergeFailureAlertSink(sink MergeFailureAlertSink) {
	if c == nil {
		return
	}
	c.mergeAlertMu.Lock()
	defer c.mergeAlertMu.Unlock()
	c.mergeAlertSink = sink
	if c.mergeAlertIDsByRepo == nil {
		c.mergeAlertIDsByRepo = make(map[string]map[string]*mergeAlertEntry)
	}
}

func (c *Client) raiseMergeFailureAlert(repo string, number int, errMsg string, head ...string) {
	alert, ok := classifyMergeFailureForOperator(repo, errMsg)
	if !ok || c == nil {
		return
	}
	c.mergeAlertMu.Lock()
	defer c.mergeAlertMu.Unlock()
	sink := c.mergeAlertSink
	if sink == nil {
		return
	}
	id := mergeFailureAlertID(repo, alert.key)
	if c.mergeAlertIDsByRepo == nil {
		c.mergeAlertIDsByRepo = make(map[string]map[string]*mergeAlertEntry)
	}
	repoKey := strings.ToLower(strings.TrimSpace(repo))
	if c.mergeAlertIDsByRepo[repoKey] == nil {
		c.mergeAlertIDsByRepo[repoKey] = make(map[string]*mergeAlertEntry)
	}
	entry := &mergeAlertEntry{key: alert.key, number: number}
	if len(head) > 0 {
		entry.head = head[0]
	}
	c.mergeAlertIDsByRepo[repoKey][fmt.Sprintf("%s/%d", id, number)] = entry

	// Serialize sink updates with bookkeeping so an older observation cannot
	// clear a newly raised alert between deleting its entry and updating the sink.
	sink.AddSystemAlert(id, "error", alert.message)
}

func (c *Client) clearMergeFailureAlertsForPR(repo string, number int) {
	if c == nil {
		return
	}
	repoKey := strings.ToLower(strings.TrimSpace(repo))
	c.mergeAlertMu.Lock()
	defer c.mergeAlertMu.Unlock()
	for member, entry := range c.mergeAlertIDsByRepo[repoKey] {
		if entry.number == number {
			c.clearMergeFailureAlertEntryLocked(repoKey, member, entry)
		}
	}
}

// revalidateMergeFailureAlerts retires "merge blocked" alerts whose cause has
// gone away without an App merge in that repo: the blocked PR is no longer
// open, (fork-run-approval) its head no longer has workflow runs awaiting
// maintainer approval — the operator approved them or relaxed the repo/org
// setting — or (app-permission) a check-runs read on its head succeeds again.
// Missing-check alerts require successful exact-current-head evidence.
// Errors leave the alert in place; only positive evidence clears it.
func (c *Client) revalidateMergeFailureAlerts(ctx context.Context, nowFn func() time.Time) {
	if c == nil || c.client == nil {
		return
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	c.mergeAlertMu.Lock()
	sink := c.mergeAlertSink
	if sink == nil || len(c.mergeAlertIDsByRepo) == 0 ||
		(!c.mergeAlertLastRevalidate.IsZero() && nowFn().Sub(c.mergeAlertLastRevalidate) < mergeAlertRevalidateInterval) {
		c.mergeAlertMu.Unlock()
		return
	}
	c.mergeAlertLastRevalidate = nowFn()
	type pending struct {
		repo, id string
		entry    *mergeAlertEntry
	}
	var work []pending
	for repo, ids := range c.mergeAlertIDsByRepo {
		for id, e := range ids {
			work = append(work, pending{repo: repo, id: id, entry: e})
		}
	}
	c.mergeAlertMu.Unlock()

	for _, w := range work {
		if ctx.Err() != nil {
			return
		}
		owner, name, ok := strings.Cut(w.repo, "/")
		if !ok || w.entry.number <= 0 {
			continue
		}
		pr, _, err := c.client.PullRequests.Get(ctx, owner, name, w.entry.number)
		if err != nil || pr == nil {
			continue
		}
		resolved := pr.GetState() == "closed"
		if pr.GetState() != "open" && !resolved {
			continue
		}
		if !resolved && strings.HasPrefix(w.entry.key, "missing-checks:") {
			resolved = c.missingMergeChecksResolved(ctx, owner, name, pr,
				splitCheckNames(strings.TrimPrefix(w.entry.key, "missing-checks:")))
		}
		if !resolved && w.entry.key == "app-permission" {
			// Positive evidence the App can act on the repo again: its
			// check-runs read on the blocked head succeeds. A real merge
			// refusal re-raises the alert on the next attempt.
			_, _, rerr := c.client.Checks.ListCheckRunsForRef(ctx, owner, name, pr.GetHead().GetSHA(), &gh.ListCheckRunsOptions{
				ListOptions: gh.ListOptions{PerPage: 1},
			})
			resolved = rerr == nil && pr.GetHead().GetSHA() != ""
		}
		if !resolved && w.entry.key == "fork-run-approval" {
			runs, _, rerr := c.client.Actions.ListRepositoryWorkflowRuns(ctx, owner, name, &gh.ListWorkflowRunsOptions{
				HeadSHA:     pr.GetHead().GetSHA(),
				Status:      "action_required",
				ListOptions: gh.ListOptions{PerPage: 1},
			})
			resolved = rerr == nil && runs != nil && runs.GetTotalCount() == 0
		}
		if !resolved {
			continue
		}
		c.mergeAlertMu.Lock()
		cleared := c.clearMergeFailureAlertEntryLocked(w.repo, w.id, w.entry)
		c.mergeAlertMu.Unlock()
		if cleared {
			c.logger.Info("merge alert cleared: blocking condition resolved",
				slog.String("repo", w.repo), slog.Int("number", w.entry.number),
				slog.String("key", w.entry.key), slog.String("origin_head", w.entry.head),
				slog.String("current_head", pr.GetHead().GetSHA()))
		}
	}
}

// The pointer is an observation token: a refusal raised during network reads
// replaces it and must survive the old observation.
func (c *Client) clearMergeFailureAlertEntryLocked(repoKey, member string, entry *mergeAlertEntry) bool {
	entries := c.mergeAlertIDsByRepo[repoKey]
	if entries[member] != entry {
		return false
	}
	delete(entries, member)
	for _, other := range entries {
		if other.key == entry.key {
			return true
		}
	}
	if len(entries) == 0 {
		delete(c.mergeAlertIDsByRepo, repoKey)
	}
	if c.mergeAlertSink != nil {
		c.mergeAlertSink.ClearSystemAlert(mergeFailureAlertID(repoKey, entry.key))
	}
	return true
}

func mergeFailureAlertID(repo, key string) string {
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(repo)) + "\x00" + key))
	return "merge-failure-" + hex.EncodeToString(sum[:8])
}

func classifyMergeFailureForOperator(repo, errMsg string) (mergeFailureAlert, bool) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		repo = "this repo"
	}
	msg := strings.TrimSpace(errMsg)
	lower := strings.ToLower(msg)
	missing := missingRequiredChecks(msg)

	switch {
	case isMergeFailureRateLimited(lower), isConflictMergeBlocker(msg), isCIGateReadFailure(lower):
		return mergeFailureAlert{}, false
	case strings.Contains(lower, "fork pr workflow runs are awaiting maintainer approval"):
		return mergeFailureAlert{
			key:     "fork-run-approval",
			message: fmt.Sprintf("Merge blocked for %s: fork PR workflow runs are awaiting maintainer approval. Approve the runs manually or relax the repo setting at https://github.com/%s/settings/actions (Approval for running fork pull request workflows).", repo, repo),
		}, true
	case strings.Contains(lower, "same-repo pr workflow runs are awaiting maintainer approval"):
		// Same key as the fork case: revalidation clears both by the same
		// evidence (no action_required runs left on the head).
		return mergeFailureAlert{
			key:     "fork-run-approval",
			message: fmt.Sprintf("Merge blocked for %s: workflow runs on a same-repo PR opened by a bot token are awaiting maintainer approval (GitHub applies the fork-PR approval policy to GITHUB_TOKEN-opened PRs). Approve the runs manually, have the opener dispatch the required workflows on the branch, or relax the repo setting at https://github.com/%s/settings/actions (Approval for running fork pull request workflows).", repo, repo),
		}, true
	case strings.Contains(lower, "resource not accessible by integration") ||
		(strings.Contains(lower, "403") && (strings.Contains(lower, "contents") || strings.Contains(lower, "pull request") || strings.Contains(lower, "pull_requests"))) ||
		strings.Contains(lower, "contents:write") || strings.Contains(lower, "contents write") ||
		strings.Contains(lower, "pull_requests:write") || strings.Contains(lower, "pull requests write"):
		return mergeFailureAlert{
			key:     "app-permission",
			message: fmt.Sprintf("Merge blocked for %s: the GitHub App lacks repository write permission. Grant the hivecommons App Contents & Pull requests write on %s, or install the App on the repo. Trigger: %s", repo, repo, mergeFailureTrigger(msg)),
		}, true
	case strings.Contains(lower, "approving review") ||
		strings.Contains(lower, "required review") ||
		strings.Contains(lower, "review is required") ||
		strings.Contains(lower, "changes have been requested") ||
		(strings.Contains(lower, "ruleset") && strings.Contains(lower, "review")):
		return mergeFailureAlert{
			key:     "review-required",
			message: fmt.Sprintf("Merge blocked for %s: branch protection or a ruleset requires reviews/approvals the App cannot provide. Add the App to the bypass list or require-review exemption, or approve the PR.", repo),
		}, true
	case strings.Contains(lower, "was never created"):
		return mergeFailureAlert{
			key:     "never-created-checks",
			message: fmt.Sprintf("Merge blocked for %s: a required check was never created on the PR head and nothing is running, so the hive will not keep polling it. The PR author's token likely cannot trigger workflows; re-trigger CI or merge it manually.", repo),
		}, true
	case len(missing) > 0:
		return mergeFailureAlert{
			key:     "missing-checks:" + strings.Join(missing, ","),
			message: fmt.Sprintf("Merge blocked for %s: required status check(s) never reported: %s. Make the workflow report those checks on PR heads, or update branch protection / auto_merge.required_checks.", repo, strings.Join(missing, ", ")),
		}, true
	case strings.Contains(lower, "merge method") ||
		strings.Contains(lower, "squash commits are not allowed") ||
		strings.Contains(lower, "merge commits are not allowed") ||
		strings.Contains(lower, "rebase commits are not allowed") ||
		strings.Contains(lower, "not allowed to merge using"):
		return mergeFailureAlert{
			key:     "merge-method",
			message: fmt.Sprintf("Merge blocked for %s: the requested merge method is not allowed. Enable squash/merge on the repository or set the hive merge method to one the repo allows.", repo),
		}, true
	case strings.Contains(lower, "403") || strings.Contains(lower, "permission") || strings.Contains(lower, "must have admin rights"):
		return mergeFailureAlert{
			key:     "app-permission",
			message: fmt.Sprintf("Merge blocked for %s: the GitHub App appears to lack permission to merge. Grant the hivecommons App Contents & Pull requests write on %s, or install the App on the repo. Trigger: %s", repo, repo, mergeFailureTrigger(msg)),
		}, true
	}
	return mergeFailureAlert{}, false
}

func isMergeFailureRateLimited(lower string) bool {
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "secondary rate") ||
		strings.Contains(lower, "status code: 429") ||
		strings.Contains(lower, "http 429")
}

// isCIGateReadFailure reports a merge-request CI-gate error caused by a failed
// GitHub read (check-runs, statuses, workflow runs, the PR itself): an HTTP
// 5xx, or a 403 / "Resource not accessible by integration". The gate never
// writes, so such a failure says nothing about the App's merge permission; it
// is a transient, unverified verdict the retry path handles (#11924).
func isCIGateReadFailure(lower string) bool {
	if !strings.HasPrefix(lower, "ci gate:") {
		return false
	}
	return strings.Contains(lower, "resource not accessible by integration") ||
		ciGateReadHTTPErrRE.MatchString(lower)
}

// mergeFailureTrigger bounds the raw error quoted in an operator alert so a
// false positive stays diagnosable from the dashboard.
func mergeFailureTrigger(msg string) string {
	const maxTriggerLen = 300
	if len(msg) > maxTriggerLen {
		return msg[:maxTriggerLen] + "…"
	}
	return msg
}

var (
	ciGateReadHTTPErrRE   = regexp.MustCompile(`\bget https?://\S+: (?:403|5\d\d)\b`)
	missingChecksColonRE  = regexp.MustCompile(`(?i)required check\(s\) not yet reported[^:]*:\s*([^()]+)`)
	missingChecksQuotedRE = regexp.MustCompile(`(?i)required status checks? ["']([^"']+)["'] (?:is|are) expected`)
)

func missingRequiredChecks(msg string) []string {
	for _, re := range []*regexp.Regexp{missingChecksColonRE, missingChecksQuotedRE} {
		m := re.FindStringSubmatch(msg)
		if len(m) < 2 {
			continue
		}
		return splitCheckNames(m[1])
	}
	return nil
}

func splitCheckNames(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\t' })
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool)
	for _, part := range parts {
		part = strings.TrimSpace(strings.Trim(part, `"'`))
		if part == "" {
			continue
		}
		key := strings.ToLower(part)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, part)
	}
	return out
}
