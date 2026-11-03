package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/holdguard"
	"github.com/hivecommons/hive/pkg/review"
)

// githubWebFlowLogin is the committer GitHub stamps on a commit it makes
// server-side: update-branch, the merges API, createCommitOnBranch. A commit
// pushed with git names whoever pushed it instead.
const githubWebFlowLogin = "web-flow"

// ReviewDeltaDiff fetches the compare between a PR's previously reviewed head
// and its current head for the review planner's delta re-review
// (review.delta_rereview). Both are immutable SHAs, so the answer cannot
// describe a newer push. One page of files is read: GitHub caps a compare's
// file list at githubCompareFileLimit per page, and a delta that large is not
// a candidate for the trivial skip anyway, so a full page is reported as
// unknown rather than paged. Any failure is also unknown, never an error: the
// planner then sends the delta instruction without a file list.
//
// When the commits between the heads are all the hive's own merges of the
// base (reviewBaseSyncParents), it also reads the PR's own files and the
// base's side of the merge, so the planner can carry an approve across the
// hive's update-branch (DeltaDiff.BaseSync,
// projectbluefin/fsdk-containers#482). That is two more compares, only for
// such a head. baseRef is the branch the PR targets; "" skips that step.
func (c *Client) ReviewDeltaDiff(ctx context.Context, repo, baseRef, previousHead, head string) review.DeltaDiff {
	out := review.DeltaDiff{PreviousHead: strings.TrimSpace(previousHead)}
	head = strings.TrimSpace(head)
	if c == nil || c.client == nil || out.PreviousHead == "" || head == "" || review.SameHeadSHA(out.PreviousHead, head) {
		return out
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	restCtx := WithRESTCaller(ctx, "hive:review_delta")
	comparison, allCommits, ok := c.reviewCompare(restCtx, owner, name, out.PreviousHead, head, githubCompareFileLimit)
	if !ok || !allCommits {
		return out
	}
	out.Known = true
	for _, f := range comparison.Files {
		out.Files = append(out.Files, review.DeltaFile{Path: f.GetFilename(), Additions: f.GetAdditions(), Deletions: f.GetDeletions()})
	}
	baseRef = strings.TrimSpace(baseRef)
	if len(out.Files) == 0 || baseRef == "" {
		return out
	}
	baseParents, ok := reviewBaseSyncParents(comparison, out.PreviousHead, head, c.AppBotLogin())
	if !ok {
		return out
	}
	merged := baseParents[len(baseParents)-1]
	// The PR's own files, and the proof the last merged parent is on the
	// base: the base branch's merge base with the head is that parent exactly
	// when the base contains it (it is an ancestor of the head, and the head
	// holds no newer base commit). Three-dot, like the PR's Files tab. The
	// file list is complete on the first page whatever the commit paging, so
	// one commit per page is enough.
	prSide, _, ok := c.reviewCompare(restCtx, owner, name, baseRef, head, 1)
	if !ok || !review.SameHeadSHA(prSide.GetMergeBaseCommit().GetSHA(), merged) {
		return out
	}
	// The base's side: what it changed since the reviewed head's merge base.
	// Earlier merged parents (more than one hive merge since the review) must
	// be among its commits, i.e. on the base below the last one, so only
	// then is the commit list read in full.
	earlier := baseParents[:len(baseParents)-1]
	perPage := 1
	if len(earlier) > 0 {
		perPage = githubCompareFileLimit
	}
	baseSide, baseAll, ok := c.reviewCompare(restCtx, owner, name, out.PreviousHead, merged, perPage)
	if !ok || (len(earlier) > 0 && !(baseAll && compareHasCommits(baseSide, earlier))) {
		return out
	}
	out.BaseSync = true
	out.PRFiles = compareFilePaths(prSide)
	out.BaseFiles = compareFilePaths(baseSide)
	return out
}

// reviewBaseSyncParents reports whether every commit from previousHead to
// head is a merge of the base the hive App asked GitHub for, and returns each
// merge's second (base) parent in order. Each commit must be a two-parent
// merge whose first parent is the commit before it (holdguard.IsHygieneMerge,
// the shape the hold guard already adopts as hygiene, hivecommons/hive#14535),
// authored by the App and committed by GitHub itself (web-flow):
// update-branch and the merges API make exactly that, and refuse a
// conflicting merge rather than resolve it. A merge pushed with git, by a
// person, another bot or an agent (agents may commit under the App-linked
// identity, hivecommons/hive#9478), names its pusher as committer and never
// counts. Proving the base parents are on the base is the caller's job.
func reviewBaseSyncParents(comparison *gh.CommitsComparison, previousHead, head, appLogin string) ([]string, bool) {
	appLogin = strings.TrimSpace(appLogin)
	if comparison == nil || appLogin == "" || len(comparison.Commits) == 0 || comparison.GetTotalCommits() != len(comparison.Commits) {
		return nil, false
	}
	var parents []string
	prev := previousHead
	for _, rc := range comparison.Commits {
		if !strings.EqualFold(rc.GetAuthor().GetLogin(), appLogin) || rc.GetCommitter().GetLogin() != githubWebFlowLogin {
			return nil, false
		}
		commit := holdguard.Commit{SHA: rc.GetSHA()}
		for _, p := range rc.Parents {
			commit.Parents = append(commit.Parents, p.GetSHA())
		}
		// The reviewed head may be recorded abbreviated (review.SameHeadSHA);
		// the merge names it in full.
		if len(commit.Parents) > 0 && review.SameHeadSHA(commit.Parents[0], prev) {
			prev = commit.Parents[0]
		}
		baseParent := ""
		record := func(sha string) bool {
			baseParent = sha
			return true
		}
		if !holdguard.IsHygieneMerge(commit, prev, record) {
			return nil, false
		}
		parents = append(parents, baseParent)
		prev = commit.SHA
	}
	if !review.SameHeadSHA(prev, head) {
		return nil, false
	}
	return parents, true
}

// reviewCompare is one compare page whose file list is complete: GitHub
// returns every changed file on the first page, up to githubCompareFileLimit,
// so a full list there is reported as unknown (ok false). allCommits is true
// when that page also holds every commit of the comparison.
func (c *Client) reviewCompare(ctx context.Context, owner, name, base, head string, perPage int) (comparison *gh.CommitsComparison, allCommits, ok bool) {
	comparison, resp, err := c.client.Repositories.CompareCommits(ctx, owner, name, base, head, &gh.ListOptions{PerPage: perPage})
	if err != nil || comparison == nil || len(comparison.Files) >= githubCompareFileLimit {
		return nil, false, false
	}
	allCommits = (resp == nil || resp.NextPage == 0) && comparison.GetTotalCommits() <= len(comparison.Commits)
	return comparison, allCommits, true
}

func compareFilePaths(comparison *gh.CommitsComparison) []string {
	paths := make([]string, 0, len(comparison.Files))
	for _, f := range comparison.Files {
		if p := strings.TrimSpace(f.GetFilename()); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// compareHasCommits reports whether every sha is among the compare's commits.
func compareHasCommits(comparison *gh.CommitsComparison, shas []string) bool {
	have := map[string]bool{}
	for _, rc := range comparison.Commits {
		have[rc.GetSHA()] = true
	}
	for _, sha := range shas {
		if !have[sha] {
			return false
		}
	}
	return true
}
