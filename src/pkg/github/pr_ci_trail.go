package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PR CI recovery trail (hivecommons/hive#12365 follow-up): the evidence
// card's claim-vs-verdict panel grades how a PR that was CI-red when the
// agent claimed done RECOVERED, not the red itself — on a hive that lets CI
// arbitrate, red at claim is the design. The trail is every head the PR
// carried from the claim head on, with what CI says about each now, so the
// panel can count pushes to green and time to green.

// PRCIHead is one PR head in the trail: when it was committed and the CI
// state of its checks now (green, red, pending, or none when no gating
// check exists for it).
type PRCIHead struct {
	SHA   string    `json:"sha"`
	At    time.Time `json:"at"`
	State string    `json:"state"`
}

// PRCITrail is a PR's heads from the claim head onward plus its outcome.
type PRCITrail struct {
	State    string     `json:"state"` // open, closed or merged
	MergedAt time.Time  `json:"merged_at,omitzero"`
	ClosedAt time.Time  `json:"closed_at,omitzero"`
	Heads    []PRCIHead `json:"heads"`
	// Truncated is true when the PR had more heads after the claim than
	// prCITrailMaxHeads and the oldest were skipped (the trail still ends at
	// the current head).
	Truncated bool `json:"truncated,omitempty"`
}

const (
	prCITrailMaxHeads = 6
	prCITrailPerPage  = 100
	prCITrailMaxPages = 3

	PRCIHeadGreen   = "green"
	PRCIHeadRed     = "red"
	PRCIHeadPending = "pending"
	PRCIHeadNone    = "none"
)

// PRCIRecoveryTrail lists the PR's commits, keeps the claim head and every
// commit after it (the newest prCITrailMaxHeads when there are more), and
// summarises CI for each. When claimHead is not among the PR's commits
// (force-pushed away), the trail is the newest heads. Sheds under REST
// budget pressure like the other dashboard-only lookups.
func (c *Client) PRCIRecoveryTrail(ctx context.Context, repo string, number int, claimHead string) (PRCITrail, error) {
	var out PRCITrail
	if c == nil || c.client == nil {
		return out, ErrNoGitHubClient
	}
	if c.shouldShedMTTRIssueFetches() {
		return out, fmt.Errorf("github REST budget below reserve %d; skipping CI trail for %s#%d", lowValueRESTBudgetFloor, repo, number)
	}
	owner, name := c.splitRepo(repo)
	st, err := c.GetPRState(ctx, repo, number)
	if err != nil {
		return out, err
	}
	out.State = strings.ToLower(st.State)
	if !st.MergedAt.IsZero() {
		out.State = "merged"
		out.MergedAt = st.MergedAt.UTC()
	}
	if !st.ClosedAt.IsZero() {
		out.ClosedAt = st.ClosedAt.UTC()
	}
	opts := &gh.ListOptions{PerPage: prCITrailPerPage}
	var commits []*gh.RepositoryCommit
	for page := 0; page < prCITrailMaxPages; page++ {
		got, resp, err := c.client.PullRequests.ListCommits(ctx, owner, name, number, opts)
		if err != nil {
			return out, fmt.Errorf("listing commits for %s#%d: %w", repo, number, err)
		}
		commits = append(commits, got...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	start := 0
	for i, cm := range commits {
		if strings.EqualFold(cm.GetSHA(), claimHead) {
			start = i
			break
		}
	}
	heads := commits[start:]
	if len(heads) > prCITrailMaxHeads {
		heads = heads[len(heads)-prCITrailMaxHeads:]
		out.Truncated = true
	}
	for _, cm := range heads {
		h := PRCIHead{SHA: cm.GetSHA(), At: cm.GetCommit().GetCommitter().GetDate().Time.UTC()}
		sum, err := SummarizeCommitChecks(ctx, c.client, owner, name, cm.GetSHA())
		if err != nil {
			return out, fmt.Errorf("checks for %s@%s: %w", repo, cm.GetSHA(), err)
		}
		h.State = prCIHeadState(sum)
		out.Heads = append(out.Heads, h)
	}
	return out, nil
}

func prCIHeadState(s CommitChecksSummary) string {
	switch {
	case len(s.Red) > 0:
		return PRCIHeadRed
	case s.Pending > 0:
		return PRCIHeadPending
	case s.Green > 0:
		return PRCIHeadGreen
	}
	return PRCIHeadNone
}
