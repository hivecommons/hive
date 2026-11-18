package dashboard

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/buildcontains"
)

type fakeVersionHistory struct {
	compares map[string]string
	prs      map[int]buildcontains.PullRequest
}

func (f fakeVersionHistory) Compare(_ context.Context, base, head string) (string, bool, error) {
	st, ok := f.compares[base+"..."+head]
	return st, ok, nil
}

func (f fakeVersionHistory) PullRequest(_ context.Context, n int) (buildcontains.PullRequest, bool, error) {
	pr, ok := f.prs[n]
	return pr, ok, nil
}

func TestVersionContainsAnswer(t *testing.T) {
	hist := fakeVersionHistory{
		compares: map[string]string{
			"1a2b3c4d5e6f...bbbbbbb": "ahead",
			"1a2b3c4d5e6f...aaaaaaa": "behind",
		},
		prs: map[int]buildcontains.PullRequest{5: {Merged: true, MergeCommit: "1a2b3c4d5e6f"}},
	}
	ctx := context.Background()
	pr := buildcontains.Query{Kind: buildcontains.KindPR, PR: 5}

	if got := versionContainsAnswer(ctx, hist, pr, "bbbbbbb", "stable"); got.Contains != "yes" || got.Channel != "stable" {
		t.Fatalf("containing build must be yes: %+v", got)
	}
	if got := versionContainsAnswer(ctx, hist, pr, "aaaaaaa", "stable"); got.Contains != "no" {
		t.Fatalf("a PR outside the build's history must be no: %+v", got)
	}
	// A commit the history cannot resolve is unknown, never yes or no.
	if got := versionContainsAnswer(ctx, hist, pr, "ccccccc", "stable"); got.Contains != "unknown" || got.Reason == "" {
		t.Fatalf("unresolvable commit must be unknown: %+v", got)
	}
	// A hive that reports no usable commit is unknown.
	if got := versionContainsAnswer(ctx, hist, pr, "unknown", ""); got.Contains != "unknown" {
		t.Fatalf("unresolvable running commit must be unknown: %+v", got)
	}
}

func TestVersionContainsAnswerUnmergedPR(t *testing.T) {
	hist := fakeVersionHistory{prs: map[int]buildcontains.PullRequest{6: {Merged: false}}}
	got := versionContainsAnswer(context.Background(), hist, buildcontains.Query{Kind: buildcontains.KindPR, PR: 6}, "bbbbbbb", "")
	if got.Contains != "no" {
		t.Fatalf("an unmerged PR is in no build: %+v", got)
	}
}
