package github

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// BackfillReviewBacklogSplits splits review-backlog hosts that closed before
// SweepReviewBacklogSplits started running (hivecommons/hive#13947). The
// sweep only lists issues updated inside its since window, so a host closed
// earlier and never touched again kept its folded findings closed with it.
// The backfill instead walks the ledger's host index, fetches each host
// directly and splits every closed one through the same per-host path, so
// the ledger's SplitKeys keep it idempotent with the sweep.
//
// Once a pass has visited every host without deferring one or failing to
// read or split one, the ledger's SplitBackfilled flag is set and every
// later call is a no-op. A pass cut short by opts.MaxActions (including a
// host left part-split when the budget ran out) or a transient error leaves the flag unset and the next pass retries; hosts already
// split are not split again. opts.Since is ignored.
func (c *Client) BackfillReviewBacklogSplits(ctx context.Context, opts ReviewBacklogSplitSweepOptions) (*ReviewBacklogSplitResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultReviewBacklogSplitMaxActions
	}
	done, hosts, err := loadReviewBacklogBackfillState()
	if err != nil {
		return nil, err
	}
	result := &ReviewBacklogSplitResult{}
	if done {
		return result, nil
	}
	complete := true
	sink, labels := c.reviewBacklogSplitTarget()
	for _, repo := range c.getRepos() {
		repoHosts := hosts[reviewBacklogRepoKey(repo)]
		if len(repoHosts) == 0 {
			continue
		}
		numbers := make([]int, 0, len(repoHosts))
		for n := range repoHosts {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		owner, repoName := c.splitRepo(repo)
		for _, n := range numbers {
			if len(result.Split) >= maxActions {
				result.Deferred++
				complete = false
				continue
			}
			issue, resp, err := c.client.Issues.Get(ctx, owner, repoName, n)
			if err != nil {
				if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
					// Deleted or transferred: nothing left to split here.
					continue
				}
				if c.logger != nil {
					c.logger.Warn("review backlog split backfill: could not read host", "repo", repo, "issue", n, "error", err)
				}
				result.Skipped++
				complete = false
				continue
			}
			if issue == nil || issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "closed") || issue.GetComments() == 0 {
				// Open hosts are left to the windowed sweep once they close.
				continue
			}
			ok, err := c.sweepReviewBacklogSplitHost(ctx, sink, labels, repo, owner, repoName, issue, maxActions, opts.Audit, result)
			if err != nil {
				return result, err
			}
			if !ok {
				complete = false
			}
		}
	}
	if !complete {
		if c.logger != nil {
			c.logger.Warn("review backlog split backfill incomplete, retrying next pass", "split", len(result.Split), "skipped", result.Skipped, "deferred_hosts", result.Deferred)
		}
		return result, nil
	}
	return result, markReviewBacklogSplitBackfilled()
}

// loadReviewBacklogBackfillState reads whether the backfill already ran and,
// if not, the host index it walks.
func loadReviewBacklogBackfillState() (bool, map[string]map[int]bool, error) {
	reviewBacklogLedgerMu.Lock()
	defer reviewBacklogLedgerMu.Unlock()
	l, err := loadReviewBacklog("")
	if err != nil {
		return false, nil, err
	}
	if l.SplitBackfilled {
		return true, nil, nil
	}
	return false, reviewBacklogBackfillHostIndex(l), nil
}

// reviewBacklogBackfillHostIndex is reviewBacklogHostIndex without the
// issues the ledger itself filed by splitting a host: those were created by
// the sweep or an earlier backfill pass, so they cannot predate the sweep
// window, and the windowed sweep handles them once they close. Leaving them
// in would make a budget-capped pass defer its own freshly split issues and
// never mark the backfill done. An issue is dropped only when every record
// pointing at it is a split destination, so a host that also holds findings
// of its own stays in the walk.
func reviewBacklogBackfillHostIndex(l *reviewBacklogLedger) map[string]map[int]bool {
	out := reviewBacklogHostIndex(l)
	splitOnly := map[string]map[int]bool{}
	for key, rec := range l.Items {
		if rec.IssueNumber <= 0 || (rec.Destination != "" && rec.Destination != config.ReviewBacklogGitHubIssue) {
			continue
		}
		repoKey := reviewBacklogRepoKey(rec.Repo)
		if splitOnly[repoKey] == nil {
			splitOnly[repoKey] = map[int]bool{}
		}
		isSplit := len(l.SplitKeys[key]) > 0 && !l.splitFrom(key, rec.IssueNumber)
		if seen, ok := splitOnly[repoKey][rec.IssueNumber]; ok {
			splitOnly[repoKey][rec.IssueNumber] = seen && isSplit
		} else {
			splitOnly[repoKey][rec.IssueNumber] = isSplit
		}
	}
	for repoKey, numbers := range splitOnly {
		for n, only := range numbers {
			if only {
				delete(out[repoKey], n)
			}
		}
	}
	return out
}

// markReviewBacklogSplitBackfilled sets the ledger's SplitBackfilled flag
// under the ledger lock, reloading first so a concurrent writer's update
// is kept.
func markReviewBacklogSplitBackfilled() error {
	reviewBacklogLedgerMu.Lock()
	defer reviewBacklogLedgerMu.Unlock()
	l, err := loadReviewBacklog("")
	if err != nil {
		return err
	}
	l.SplitBackfilled = true
	return l.save("", time.Now())
}
