package github

import (
	"context"
	"fmt"
	"strings"
)

// WorkflowItemSnapshot is the head-level view of one issue or pull request
// the workflow simulate API (RFC hivecommons/hive#17620 phase 3c) fills
// its Facts from: what one issues GET and, for a PR, one pulls GET plus
// the head's check summary say. Nothing the hive derives (lanes, claims,
// buckets) is here; the simulate response marks those unknown.
type WorkflowItemSnapshot struct {
	Kind   string // "issue" | "pr"
	Labels []string
	Open   bool
	Merged bool
	Draft  bool
	Author string
	// PR-only; zero on an issue.
	HeadSHA string
	BaseRef string
	// MergeCommitSHA and DefaultBranch are set on a merged PR: the commit
	// the merge put on the base branch and the repository's default
	// branch, which the post-merge run observer attributes runs against
	// (pipeline.status, hivecommons/hive#18356).
	MergeCommitSHA string
	DefaultBranch  string
	// Conflicting is set when GitHub reports the head as not mergeable
	// (mergeable_state dirty). ConflictKnown is false while GitHub is still
	// computing mergeability.
	Conflicting, ConflictKnown bool
	// Checks is the head's gating-check digest; ChecksKnown is false when
	// the head could not be read.
	Checks      CommitChecksSummary
	ChecksKnown bool
}

// WorkflowItemSnapshot reads repo#number read-only for the simulate API.
func (c *Client) WorkflowItemSnapshot(ctx context.Context, repo string, number int) (WorkflowItemSnapshot, error) {
	if c == nil || c.client == nil {
		return WorkflowItemSnapshot{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:workflow_simulate"), owner, repoName, number)
	if err != nil {
		return WorkflowItemSnapshot{}, fmt.Errorf("reading %s#%d: %w", repo, number, err)
	}
	snap := WorkflowItemSnapshot{Kind: "issue", Open: strings.EqualFold(issue.GetState(), "open"), Author: safeGetLogin(issue.GetUser())}
	for _, l := range issue.Labels {
		if name := l.GetName(); name != "" {
			snap.Labels = append(snap.Labels, name)
		}
	}
	if !issue.IsPullRequest() {
		return snap, nil
	}
	snap.Kind = "pr"
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:workflow_simulate"), owner, repoName, number)
	if err != nil {
		return WorkflowItemSnapshot{}, fmt.Errorf("reading PR %s#%d: %w", repo, number, err)
	}
	c.storePRDetail(owner+"/"+repoName, number, pr)
	snap.Open = strings.EqualFold(pr.GetState(), "open")
	snap.Merged = pr.GetMerged() || !pr.GetMergedAt().IsZero()
	snap.Draft = pr.GetDraft()
	snap.HeadSHA = pr.GetHead().GetSHA()
	snap.BaseRef = prBaseRef(pr)
	if snap.Merged {
		snap.MergeCommitSHA = pr.GetMergeCommitSHA()
		snap.DefaultBranch = pr.GetBase().GetRepo().GetDefaultBranch()
	}
	if ms := pr.GetMergeableState(); ms != "" && ms != "unknown" {
		snap.ConflictKnown = true
		snap.Conflicting = ms == "dirty"
	}
	if snap.Open && snap.HeadSHA != "" {
		if sum, err := SummarizeCommitChecks(ctx, c.client, owner, repoName, snap.HeadSHA); err == nil {
			snap.Checks, snap.ChecksKnown = sum, true
		}
	}
	return snap, nil
}
