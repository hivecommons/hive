package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestGroupPRsByRepoExcludesHeldPRs(t *testing.T) {
	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		{Repo: "common", Number: 1},
		{Repo: "bluefin", Number: 2},
		{Repo: "common", Number: 3},
	}
	actionable.PRs.StaleDrafts = []github.PullRequest{
		{Repo: "common", Number: 4, Draft: true},
	}
	// A held PR is a maintainer's explicit "not now". Recommending it would
	// be the fastest way to lose the reader's trust in the whole digest.
	actionable.PRs.Held = []github.PullRequest{
		{Repo: "common", Number: 99},
	}

	got := groupPRsByRepo(actionable)

	if len(got["common"]) != 3 {
		t.Fatalf("common: want 3 PRs (2 actionable + 1 stale draft), got %d", len(got["common"]))
	}
	if len(got["bluefin"]) != 1 {
		t.Fatalf("bluefin: want 1 PR, got %d", len(got["bluefin"]))
	}
	for _, pr := range got["common"] {
		if pr.Number == 99 {
			t.Fatal("held PR 99 leaked into the recommendations digest")
		}
	}
}

func TestGroupPRsByRepoSkipsPRsWithNoRepo(t *testing.T) {
	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		{Repo: "", Number: 1},
		{Repo: "common", Number: 2},
	}

	got := groupPRsByRepo(actionable)

	if _, ok := got[""]; ok {
		t.Fatal("a PR with no repo produced an empty-string repo group, which would post to nowhere")
	}
	if len(got) != 1 {
		t.Fatalf("want 1 repo group, got %d", len(got))
	}
}
