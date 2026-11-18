package hub

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/buildcontains"
)

type fakeContainsHistory struct {
	compares map[string]string
	prs      map[int]buildcontains.PullRequest
}

func (f fakeContainsHistory) Compare(_ context.Context, base, head string) (string, bool, error) {
	st, ok := f.compares[base+"..."+head]
	return st, ok, nil
}

func (f fakeContainsHistory) PullRequest(_ context.Context, n int) (buildcontains.PullRequest, bool, error) {
	pr, ok := f.prs[n]
	return pr, ok, nil
}

const (
	containsMerge = "1a2b3c4d5e6f"
	containsOld   = "aaaaaaa"
	containsNew   = "bbbbbbb"
	containsGone  = "ccccccc"
)

func containsFixture() (fakeContainsHistory, []buildContainsHive) {
	hist := fakeContainsHistory{
		compares: map[string]string{
			containsMerge + "..." + containsNew: "ahead",
			containsMerge + "..." + containsOld: "behind",
		},
		prs: map[int]buildcontains.PullRequest{
			10: {Merged: true, MergeCommit: containsMerge},
			11: {Merged: false},
		},
	}
	hives := []buildContainsHive{
		{ID: "h-new", Name: "alpha", GitHash: containsNew, ImageRef: "ghcr.io/hivecommons/hive:stable"},
		{ID: "h-old", ProjectName: "beta", GitHash: containsOld, TrackedChannel: "candidate"},
		{ID: "h-gone", GitHash: containsGone},
		{ID: "h-none"},
	}
	return hist, hives
}

func containsByID(resp BuildContainsResponse) map[string]BuildContainsHive {
	out := map[string]BuildContainsHive{}
	for _, h := range resp.Hives {
		out[h.ID] = h
	}
	return out
}

func TestBuildContainsAnswerPR(t *testing.T) {
	hist, hives := containsFixture()
	resp := buildContainsAnswer(context.Background(), hist, buildcontains.Query{Kind: buildcontains.KindPR, PR: 10}, hives)
	got := containsByID(resp)
	if got["h-new"].Contains != "yes" {
		t.Fatalf("build containing the merge commit must be yes: %+v", got["h-new"])
	}
	if got["h-old"].Contains != "no" {
		t.Fatalf("a PR outside the build's history must be no: %+v", got["h-old"])
	}
	if got["h-gone"].Contains != "unknown" || got["h-gone"].Reason == "" {
		t.Fatalf("an unresolvable commit must be unknown with a reason: %+v", got["h-gone"])
	}
	if got["h-none"].Contains != "unknown" {
		t.Fatalf("a hive reporting no commit must be unknown: %+v", got["h-none"])
	}
	if got["h-old"].Channel != "candidate" || got["h-old"].Name != "beta" {
		t.Fatalf("row labels wrong: %+v", got["h-old"])
	}
	if resp.Commit != containsMerge {
		t.Fatalf("response must name the tested commit, got %q", resp.Commit)
	}
}

func TestBuildContainsAnswerUnmergedPRIsNoForEveryHive(t *testing.T) {
	hist, hives := containsFixture()
	resp := buildContainsAnswer(context.Background(), hist, buildcontains.Query{Kind: buildcontains.KindPR, PR: 11}, hives)
	for _, h := range resp.Hives {
		if h.Commit == "" {
			if h.Contains != "unknown" {
				t.Fatalf("no running commit stays unknown: %+v", h)
			}
			continue
		}
		if h.Contains != "no" {
			t.Fatalf("unmerged PR must not be contained: %+v", h)
		}
	}
	if resp.Reason == "" {
		t.Fatal("a fleet-wide answer must carry its reason")
	}
}

func TestBuildContainsAnswerUnknownPR(t *testing.T) {
	hist, hives := containsFixture()
	resp := buildContainsAnswer(context.Background(), hist, buildcontains.Query{Kind: buildcontains.KindPR, PR: 999}, hives)
	for _, h := range resp.Hives {
		if h.Contains == "yes" || h.Contains == "no" {
			t.Fatalf("an unreadable PR must never yield yes or no: %+v", h)
		}
	}
}

func TestBuildContainsResponseShape(t *testing.T) {
	hist, hives := containsFixture()
	resp := buildContainsAnswer(context.Background(), hist, buildcontains.Query{Kind: buildcontains.KindCommit, Commit: containsMerge}, hives)
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"query", "commit", "hives"} {
		if _, ok := back[k]; !ok {
			t.Fatalf("response missing %q: %s", k, raw)
		}
	}
}
