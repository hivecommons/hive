package github

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PRMergeState is a fresh (uncached) read of what the dashboard's
// rebase-then-merge follow-through needs to decide its next move: the head,
// GitHub's mergeability verdict, whether the hive can push to the head,
// and whether CI on the head is red.
type PRMergeState struct {
	State               string
	Merged              bool
	MergedAt            time.Time
	HeadSHA             string
	HeadRef             string
	BaseRef             string
	Author              string
	MergeableState      string
	FromFork            bool
	MaintainerCanModify bool
	// CIRed is true when a check or status on HeadSHA concluded red.
	CIRed bool
	// RedChecks names them.
	RedChecks []string
}

// NeedsRebase reports a head GitHub will not merge as it stands: behind
// its base or conflicting with it. "unknown" (GitHub still computing)
// counts as not-needing so a stale read never turns a plain merge into a
// rebase.
func (s PRMergeState) NeedsRebase() bool {
	switch strings.ToLower(strings.TrimSpace(s.MergeableState)) {
	case "dirty", "behind":
		return true
	}
	return false
}

// Conflicting is the dirty case: a plain update-branch will not do.
func (s PRMergeState) Conflicting() bool {
	return strings.EqualFold(strings.TrimSpace(s.MergeableState), "dirty")
}

// PushReachable reports whether the hive's token can push to the head.
func (s PRMergeState) PushReachable() bool {
	return !s.FromFork || s.MaintainerCanModify
}

// GetPRMergeState reads the PR fresh (no detail cache: the follow-through
// acts on what it sees) and, when the PR is open, folds in its head's
// check summary.
func (c *Client) GetPRMergeState(ctx context.Context, repo string, number int) (PRMergeState, error) {
	if c == nil || c.client == nil {
		return PRMergeState{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:rebase_merge_state"), owner, repoName, number)
	if err != nil {
		return PRMergeState{}, fmt.Errorf("reading PR %s#%d: %w", repo, number, err)
	}
	c.storePRDetail(owner+"/"+repoName, number, pr)
	st := PRMergeState{
		State: pr.GetState(), Merged: pr.GetMerged() || !pr.GetMergedAt().IsZero(),
		HeadSHA: pr.GetHead().GetSHA(), HeadRef: pr.GetHead().GetRef(), BaseRef: prBaseRef(pr),
		Author: pr.GetUser().GetLogin(), MergeableState: pr.GetMergeableState(),
		MaintainerCanModify: pr.GetMaintainerCanModify(),
	}
	if !pr.GetMergedAt().IsZero() {
		st.MergedAt = pr.GetMergedAt().Time
	}
	if head, base := pr.GetHead().GetRepo(), pr.GetBase().GetRepo(); head != nil && base != nil && head.GetFullName() != "" && base.GetFullName() != "" {
		st.FromFork = !strings.EqualFold(head.GetFullName(), base.GetFullName())
	}
	if strings.EqualFold(st.State, "open") && st.HeadSHA != "" {
		if sum, err := SummarizeCommitChecks(ctx, c.client, owner, repoName, st.HeadSHA); err == nil && len(sum.Red) > 0 {
			st.CIRed = true
			st.RedChecks = sum.Red
		}
	}
	return st, nil
}

// PRConflictCandidates names the files a PR changed that its base also
// changed since their merge base: the files a rebase will have to resolve.
// GitHub does not report conflicting paths, so this is the honest
// approximation from two compares (base...head for the PR's side,
// head...base for what moved under it). Errors are returned; an empty list
// means the compares agree nothing overlaps.
func (c *Client) PRConflictCandidates(ctx context.Context, repo, base, head string) ([]string, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	prFiles, err := c.compareFiles(ctx, owner, repoName, base, head)
	if err != nil {
		return nil, err
	}
	baseFiles, err := c.compareFiles(ctx, owner, repoName, head, base)
	if err != nil {
		return nil, err
	}
	var out []string
	for f := range prFiles {
		if baseFiles[f] {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (c *Client) compareFiles(ctx context.Context, owner, repo, base, head string) (map[string]bool, error) {
	out := map[string]bool{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		cmp, resp, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:rebase_merge_conflicts"), owner, repo, base, head, opts)
		if err != nil {
			return nil, fmt.Errorf("comparing %s/%s %s...%s: %w", owner, repo, base, head, err)
		}
		for _, f := range cmp.Files {
			if name := f.GetFilename(); name != "" {
				out[name] = true
			}
		}
		if resp == nil || resp.NextPage == 0 || len(cmp.Files) == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}
