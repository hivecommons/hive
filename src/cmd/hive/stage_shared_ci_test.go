package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestStageSharedCIIncidentIndex(t *testing.T) {
	t.Cleanup(func() { stageSharedCIIndex.Store(nil) })
	act := &github.ActionableResult{}
	act.PRs.Items = []github.PullRequest{
		{Repo: "acme/widget", Number: 7, HeadSHA: "aaa", SharedCIIncident: 4242},
		{Repo: "acme/widget", Number: 8, HeadSHA: "bbb"},
	}
	act.PRs.Held = []github.PullRequest{{Repo: "gadget", Number: 9, HeadSHA: "ccc", SharedCIIncident: 51}}
	publishStageSharedCIIncidents(act)

	cases := []struct {
		repo   string
		number int
		head   string
		want   int
	}{
		{"acme/widget", 7, "aaa", 4242},
		{"Acme/Widget", 7, "", 4242},
		{"acme/widget", 7, "new-head", 0},
		{"acme/widget", 8, "bbb", 0},
		{"acme/gadget", 9, "ccc", 51},
		{"acme/widget", 99, "aaa", 0},
	}
	for _, tc := range cases {
		if got := stageSharedCIIncident(tc.repo, tc.number, tc.head); got != tc.want {
			t.Fatalf("stageSharedCIIncident(%q, %d, %q) = %d, want %d", tc.repo, tc.number, tc.head, got, tc.want)
		}
	}

	publishStageSharedCIIncidents(nil)
	if got := stageSharedCIIncident("acme/widget", 7, "aaa"); got != 0 {
		t.Fatalf("after an empty snapshot = %d, want 0", got)
	}
}
