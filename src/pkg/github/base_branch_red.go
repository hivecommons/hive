package github

import (
	"context"
	"path"
	"slices"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Base-branch red: a failing check on a PR head that is not the PR's fault.
//
// Rule. A failing check on a PR head is BASE RED when a check of the same
// name is also red on the latest run of the PR's base branch tip (the
// `base_ref` the list payload carries), UNLESS the PR itself touches a path
// that plausibly owns CI — `.github/workflows/`, a Dockerfile, or the Go
// module manifest (baseRedCIOwningPath). A PR whose EVERY failing check is
// base red (PullRequest.AllFailuresBaseRed) has nothing its author can fix:
// the stage engine keeps it out of 070/071/072, the fix loop neither kicks
// the fixer nor counts the pass toward the escalation budget, and the
// automerge lane records `base-branch-red:<check>` as the reason it did not
// merge. A red PR already deferred to an open `[shared-ci]` incident
// (SharedCIIncident) is handled by the older marker path and needs no base
// lookup.
//
// Observed 2026-10-20 on hivecommons/hive: v5's own Docker build was red
// (Debian ca-certificates dropped withdrawn roots), so every PR rebased on
// v5 showed the same six red checks. A one-file CSS PR was moved to 071
// "waiting on the author", and a hive PR burned three fix attempts against
// the red baseline and escalated to needs-human.
//
// Error side. Every lookup failure leaves BaseRedChecks empty: the PR is
// then routed exactly as before this rule existed (author-facing), which is
// the conservative direction — a wrongly "base red" PR would sit unrepaired.

// baseRedMaxFilePages bounds the PR file listing (100 files per page). A PR
// larger than this is treated as possibly owning CI (not base red).
const baseRedMaxFilePages = 3

// baseRedCIOwningPath reports a changed path that plausibly owns a CI
// check: a workflow, a Dockerfile, or the Go module manifest. A PR touching
// one may have broken the very check that is also red on the base, so its
// red is never attributed to the base.
func baseRedCIOwningPath(p string) bool {
	p = strings.TrimPrefix(strings.ReplaceAll(p, "\\", "/"), "./")
	if strings.HasPrefix(p, ".github/workflows/") {
		return true
	}
	base := path.Base(p)
	if strings.HasSuffix(base, ".md") {
		return false
	}
	if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".Dockerfile") || strings.HasSuffix(base, ".dockerfile") {
		return true
	}
	return base == "go.mod" || base == "go.sum"
}

// TouchesCIOwningPath reports whether any changed path owns CI (see
// baseRedCIOwningPath).
func TouchesCIOwningPath(files []string) bool {
	for _, f := range files {
		if baseRedCIOwningPath(f) {
			return true
		}
	}
	return false
}

// BaseRedChecksFor applies the rule to one PR: the failing checks whose
// name is also red on the base, sorted, or nil when the PR touches a
// CI-owning path or nothing overlaps.
func BaseRedChecksFor(failing []string, baseRed map[string]bool, files []string) []string {
	if len(failing) == 0 || len(baseRed) == 0 || TouchesCIOwningPath(files) {
		return nil
	}
	var out []string
	for _, name := range failing {
		if baseRed[name] && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// AllFailuresBaseRed reports a red PR whose EVERY failing check is also red
// on its base branch (and that touches no CI-owning path): the red is the
// base branch's, not this PR's.
func (p PullRequest) AllFailuresBaseRed() bool {
	if len(p.FailingChecks) == 0 || len(p.BaseRedChecks) == 0 {
		return false
	}
	for _, name := range p.FailingChecks {
		if !slices.Contains(p.BaseRedChecks, name) {
			return false
		}
	}
	return true
}

// redCheckNamesForRef returns the names of the latest check runs on ref
// that concluded red (checkConclusionIsRed, meta checks excluded).
func (c *Client) redCheckNamesForRef(ctx context.Context, owner, repo, ref string) (map[string]bool, error) {
	ctx = WithRESTCaller(ctx, "hive:base_branch_red")
	var all []*gh.CheckRun
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, runs.CheckRuns...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	red := map[string]bool{}
	for _, cr := range latestCheckRunsByNameAndApp(all) {
		name := cr.GetName()
		if name == "" || isMetaCheck(name) || cr.GetStatus() != "completed" {
			continue
		}
		if checkConclusionIsRed(cr.GetConclusion()) {
			red[name] = true
		}
	}
	return red, nil
}

// prChangedPaths lists the PR's changed file paths, up to
// baseRedMaxFilePages pages. ok is false when the listing failed or was
// truncated — the caller then declines to call the PR base red.
func (c *Client) prChangedPaths(ctx context.Context, owner, repo string, number int) (files []string, ok bool) {
	ctx = WithRESTCaller(ctx, "hive:base_branch_red_files")
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < baseRedMaxFilePages; page++ {
		list, resp, err := c.client.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, false
		}
		for _, f := range list {
			files = append(files, f.GetFilename())
			if prev := f.GetPreviousFilename(); prev != "" {
				files = append(files, prev)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return files, true
		}
		opts.Page = resp.NextPage
	}
	return files, false
}

// enrichBaseBranchRed stamps BaseRedChecks on every red PR in prs whose
// failing checks are also red on its base branch tip. One check-run
// listing per distinct repo+base per pass; one file listing per red PR
// whose failing checks overlap the base's.
func (c *Client) enrichBaseBranchRed(ctx context.Context, prs []PullRequest) {
	type baseKey struct{ owner, repo, ref string }
	baseRed := map[baseKey]map[string]bool{}
	for i := range prs {
		pr := &prs[i]
		pr.BaseRedChecks = nil
		if pr.CIStatus != "failure" || len(pr.FailingChecks) == 0 || pr.BaseRef == "" {
			continue
		}
		owner, repoName := c.splitRepo(pr.Repo)
		key := baseKey{owner, repoName, pr.BaseRef}
		red, known := baseRed[key]
		if !known {
			var err error
			red, err = c.redCheckNamesForRef(ctx, owner, repoName, pr.BaseRef)
			if err != nil {
				c.logger.Debug("base branch check runs unavailable; red PRs stay author-facing", "repo", pr.Repo, "base", pr.BaseRef, "error", err)
				red = nil
			}
			baseRed[key] = red
		}
		if len(BaseRedChecksFor(pr.FailingChecks, red, nil)) == 0 {
			continue
		}
		files, ok := c.prChangedPaths(ctx, owner, repoName, pr.Number)
		if !ok {
			c.logger.Debug("PR file listing unavailable or truncated; red stays author-facing", "repo", pr.Repo, "pr", pr.Number)
			continue
		}
		pr.ChangedPaths = files
		pr.BaseRedChecks = BaseRedChecksFor(pr.FailingChecks, red, files)
	}
}
