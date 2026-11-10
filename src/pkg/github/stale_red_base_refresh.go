package github

import (
	"context"
	"fmt"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Stale-red base refresh: a red PR whose base has since fixed the failure.
//
// A PR goes red on a check that is broken on its base branch too (a flaky
// test, a stale baseline). Someone fixes the base. The PR's CI never runs
// again: a re-run reuses the old merge ref, so only a new head picks up the
// fix. On projectbluefin/actions#544 `pytest` was red from a baseline pin
// that main fixed in #734 at 05:00Z on 2026-11-10; the PR sat red and
// behind until a maintainer pressed "Update branch" at 11:33Z, and pytest
// then passed. The automerge lanes have the same repair for PRs the hive
// adopted (automerge_adoption_recovery.go), but nothing ran it for any
// other PR, or for any PR in a repository with auto-merge off.
//
// The signal is deliberately simple: the PR is behind its base, and every
// check that failed on its head has completed green on the current base
// tip. No guess is made about which files a check depends on. The caller
// (cmd/hive) picks the candidates, caps the updates and remembers each
// (head, base tip) it settled, so a PR is updated at most once per base tip.

// StaleRedBaseState is a base branch tip and the latest completed check runs
// on it.
type StaleRedBaseState struct {
	// SHA is the base branch tip the check runs were read for.
	SHA string
	// Completed names every check with a completed latest run on SHA;
	// Red names those whose latest run concluded red.
	Completed map[string]bool
	Red       map[string]bool
}

// StaleRedBaseVerdict is what the base tip says about a PR's failing checks.
type StaleRedBaseVerdict string

const (
	// StaleRedBaseClears: every failing check is green on the base tip.
	StaleRedBaseClears StaleRedBaseVerdict = "clears"
	// StaleRedBaseRed: a failing check is red on the base tip too; a
	// refresh would only pull in the same failure.
	StaleRedBaseRed StaleRedBaseVerdict = "base-red"
	// StaleRedBaseUnknown: a failing check has no completed run on the base
	// tip (still running, or a PR-only check the base never runs). Nothing
	// proves the base fixed it, so no refresh.
	StaleRedBaseUnknown StaleRedBaseVerdict = "base-unknown"
)

// StaleRedBaseVerdictFor judges a PR's failing checks against the base tip.
// It also returns the first check (sorted) that kept the verdict from
// clearing, or "" when it clears.
func StaleRedBaseVerdictFor(failing []string, base StaleRedBaseState) (StaleRedBaseVerdict, string) {
	names := append([]string(nil), failing...)
	sort.Strings(names)
	if len(names) == 0 {
		return StaleRedBaseUnknown, ""
	}
	for _, name := range names {
		if base.Red[name] {
			return StaleRedBaseRed, name
		}
	}
	for _, name := range names {
		if !base.Completed[name] {
			return StaleRedBaseUnknown, name
		}
	}
	return StaleRedBaseClears, ""
}

// StaleRedBaseStateFor reads the tip of baseRef in repo and the latest
// completed check run per name on it (meta checks excluded, as the CI
// rollup excludes them). exists is false when the branch is gone.
func (c *Client) StaleRedBaseStateFor(ctx context.Context, repo, baseRef string) (StaleRedBaseState, bool, error) {
	if c == nil || c.client == nil {
		return StaleRedBaseState{}, false, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:stale_red_base_refresh")
	sha, exists, err := c.BranchHead(ctx, repo, baseRef)
	if err != nil || !exists {
		return StaleRedBaseState{}, exists, err
	}
	owner, name := c.splitRepo(repo)
	var all []*gh.CheckRun
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, name, sha, opts)
		if err != nil {
			return StaleRedBaseState{}, true, fmt.Errorf("listing check runs on %s/%s@%s: %w", owner, name, shortSHA(sha), err)
		}
		if runs != nil {
			all = append(all, runs.CheckRuns...)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	st := StaleRedBaseState{SHA: sha, Completed: map[string]bool{}, Red: map[string]bool{}}
	for _, cr := range latestCheckRunsByNameAndApp(all) {
		n := cr.GetName()
		if n == "" || isMetaCheck(n) || cr.GetStatus() != "completed" {
			continue
		}
		st.Completed[n] = true
		if checkConclusionIsRed(cr.GetConclusion()) {
			st.Red[n] = true
		}
	}
	return st, true, nil
}

// StaleRedRefreshOutcome is what RefreshStaleRedPR did for one PR.
type StaleRedRefreshOutcome struct {
	// Updated means GitHub accepted the update-branch request.
	Updated bool
	// Settled means this (head, base tip) needs no further attempt: the
	// branch was updated, is already current, or the write-back gate or
	// GitHub refused in a way the same token would meet again. Unsettled
	// means a later pass may try again (the head moved under the request).
	Settled bool
	// BehindBy is how many base commits the head lacked.
	BehindBy int
	// Skipped names why nothing was updated ("" when Updated).
	Skipped string
}

// RefreshStaleRedPR merges the base tip into pr's branch when the head is
// behind base.SHA. The caller has already established that every failing
// check on the head is green on base.SHA (StaleRedBaseVerdictFor). The
// update is pinned to the evaluated head, so a push that landed after
// enumeration is never merged onto unseen. It is an unsolicited write on a
// PR the hive may not own, so the write-back gate (KindBaseRefresh) decides
// first. A repository whose base refresh belongs to the automerge
// preparation front (auto_merge.require_current_base) is left to it.
func (c *Client) RefreshStaleRedPR(ctx context.Context, pr PullRequest, base StaleRedBaseState) (StaleRedRefreshOutcome, error) {
	if c == nil || c.client == nil {
		return StaleRedRefreshOutcome{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(pr.Repo)
	repo := owner + "/" + name
	if strings.TrimSpace(pr.HeadSHA) == "" || strings.TrimSpace(base.SHA) == "" {
		return StaleRedRefreshOutcome{Settled: true, Skipped: "head-or-base-unknown"}, nil
	}
	if c.baseRefreshScheduled(repo) {
		return StaleRedRefreshOutcome{Settled: true, Skipped: "automerge-owns-base-refresh"}, nil
	}
	preview := fmt.Sprintf("merge %s@%s into %s#%d at %s (failing checks green on the base tip)", repo, shortSHA(base.SHA), repo, pr.Number, shortSHA(pr.HeadSHA))
	if d := c.WritebackAllowed(repo, c.WritebackItemForPR(pr), writeback.KindBaseRefresh, preview); !d.Allowed {
		return StaleRedRefreshOutcome{Settled: true, Skipped: d.Reason}, nil
	}
	cmp, _, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:stale_red_base_refresh"), owner, name, base.SHA, pr.HeadSHA, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return StaleRedRefreshOutcome{}, fmt.Errorf("comparing %s@%s with %s#%d head %s: %w", repo, shortSHA(base.SHA), repo, pr.Number, shortSHA(pr.HeadSHA), err)
	}
	out := StaleRedRefreshOutcome{BehindBy: cmp.GetBehindBy()}
	if out.BehindBy <= 0 {
		out.Settled, out.Skipped = true, "current"
		return out, nil
	}
	if err := c.UpdateBranchExpectedHead(ctx, repo, pr.Number, pr.HeadSHA, base.SHA); err != nil { // writeback:gated — WritebackAllowed(KindBaseRefresh) consulted above
		if IsUpdateBranchStaleHead(err) {
			out.Skipped = "head-moved"
			return out, nil
		}
		switch ClassifyBranchUpdateError(err, pr.FromFork) {
		case BranchUpdateUpToDate:
			out.Settled, out.Skipped = true, "current"
		case BranchUpdateConflict:
			out.Settled, out.Skipped = true, "conflict"
		case BranchUpdateForkNotEditable:
			out.Settled, out.Skipped = true, "fork-not-editable"
		case BranchUpdateForkWorkflowUpdate:
			out.Settled, out.Skipped = true, "fork-workflow-update"
		case BranchUpdateWorkflowPermission:
			out.Settled, out.Skipped = true, "workflow-permission"
		default:
			return out, err
		}
		return out, nil
	}
	out.Updated, out.Settled = true, true
	return out, nil
}
