package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// TestAttachReviewLinks_BareRepoNameMatchesLedger reproduces the shape the
// production snapshot actually has, which the original tests did not: the
// repo card carries the full "owner/repo" and each PR under it carries only
// the bare repository name. The ledger is keyed by the full name the relay
// recorded. Before the fix, 194 open PRs and a populated ledger produced zero
// review pills on the live dashboard.
func TestAttachReviewLinks_BareRepoNameMatchesLedger(t *testing.T) {
	reviewedAt := time.Date(2026, 9, 18, 15, 48, 23, 0, time.UTC)
	payload := &StatusPayload{Repos: []FrontendRepo{{
		Name: "common",
		Full: "projectbluefin/common",
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "common", Number: 1121}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "common", Number: 9999}},
		},
	}}}

	AttachReviewLinks(payload, map[string]github.ReviewLink{
		"projectbluefin/common#1121": {
			URL:   "https://github.com/projectbluefin/common/pull/1121#pullrequestreview-5249195393",
			State: "commented",
			Count: 1,
			At:    reviewedAt,
		},
	})

	reviewed, ok := payload.Repos[0].OpenPrs[0].(FrontendPR)
	if !ok {
		t.Fatalf("PR 1121 is not a FrontendPR")
	}
	if reviewed.ReviewURL == "" {
		t.Fatal("a PR whose ledger entry is keyed by the full owner/repo must receive its review link even though the snapshot PR carries only the bare repo name")
	}
	if reviewed.ReviewState != "commented" || reviewed.ReviewCount != 1 {
		t.Errorf("state/count not stamped: state=%q count=%d", reviewed.ReviewState, reviewed.ReviewCount)
	}
	if reviewed.ReviewedAt == nil || !reviewed.ReviewedAt.Equal(reviewedAt) {
		t.Errorf("reviewed_at not stamped: %v", reviewed.ReviewedAt)
	}

	// Absent means "no review recorded" and must stay pill-less.
	unreviewed, ok := payload.Repos[0].OpenPrs[1].(FrontendPR)
	if !ok {
		t.Fatalf("PR 9999 is not a FrontendPR")
	}
	if unreviewed.ReviewURL != "" {
		t.Errorf("PR with no ledger entry must not be stamped, got %q", unreviewed.ReviewURL)
	}
}
