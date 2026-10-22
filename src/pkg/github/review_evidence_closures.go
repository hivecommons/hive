package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

const clarityGateEvidenceMarker = "<!-- hive:clarity-gate -->"

// Capture only confirmed closures, never bare references or already-closed
// issues. A failed issue fetch is unknown and is left out of both cohorts.
func (c *Client) captureReviewEvidenceClosures(ctx context.Context, b *evidence.Bundle, pr *gh.PullRequest) {
	if c.client == nil || pr == nil || pr.GetMergedAt().IsZero() {
		return
	}
	seen := map[string]bool{}
	for _, ref := range ParseClaimedIssues(pr.GetTitle()+"\n"+pr.GetBody(), b.Repo) {
		owner, name := c.splitRepo(ref.Repo)
		repo := owner + "/" + name
		key := evidence.PRKey(repo, ref.Issue)
		if seen[key] {
			continue
		}
		seen[key] = true
		issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:review_evidence_closures"), owner, name, ref.Issue)
		if err != nil {
			continue
		}
		at := issue.GetClosedAt().Time
		// GitHub closes keyword-linked issues at merge. Do not attribute an old
		// manual closure (or a much later one) to this PR.
		if issue.IsPullRequest() || issue.GetState() != "closed" || at.IsZero() || at.Before(pr.GetMergedAt().Time) || at.After(pr.GetMergedAt().Add(time.Minute)) {
			continue
		}
		gated := strings.Contains(issue.GetBody(), clarityGateEvidenceMarker)
		if !gated {
			// The gate asks its question in a marker comment, not in the issue body.
			// A failed comment listing is unknown, never an ungated observation.
			gated, err = hasMarkerComment(ctx, c.client, owner, name, ref.Issue, clarityGateEvidenceMarker)
			if err != nil {
				continue
			}
		}
		b.IssueClosures = append(b.IssueClosures, evidence.IssueClosure{Repo: repo, Issue: ref.Issue, At: at.UTC(), ClarityGate: gated})
	}
}
