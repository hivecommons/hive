package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/watchdog"
)

func TestWatchdogQueuedRefs(t *testing.T) {
	if refs := watchdogQueuedRefs(nil, "acme", 3); refs != nil {
		t.Fatalf("nil snapshot = %v, want nil", refs)
	}

	act := &github.ActionableResult{
		Issues: github.IssueResult{Items: []github.Issue{
			{Repo: "widgets", Number: 7, URL: "https://github.com/other/widgets/issues/7"},
			{Repo: "gadgets", Number: 9},
		}},
		PRs: github.PRResult{Items: []github.PullRequest{
			{Repo: "acme/widgets", Number: 12, URL: "https://ghe.example.com/acme/widgets/pull/12"},
			{Repo: "widgets", Number: 13},
		}},
	}
	got := watchdogQueuedRefs(act, "acme", 3)
	want := []watchdog.QueuedRef{
		{Ref: "other/widgets#7", Kind: "issue", URL: "https://github.com/other/widgets/issues/7"},
		{Ref: "acme/gadgets#9", Kind: "issue"},
		{Ref: "acme/widgets#12", Kind: "pr", URL: "https://ghe.example.com/acme/widgets/pull/12"},
	}
	if len(got) != len(want) {
		t.Fatalf("refs = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("refs[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestQueuedItemRefWithoutOrg(t *testing.T) {
	if got := queuedItemRef("", "widgets", 4, ""); got != "widgets#4" {
		t.Fatalf("queuedItemRef = %q, want widgets#4", got)
	}
	if got := queuedItemRef("acme", "widgets", 4, "https://github.com/acme/widgets/issues/5"); got != "acme/widgets#4" {
		t.Fatalf("mismatched URL number must fall back to repo, got %q", got)
	}
}
