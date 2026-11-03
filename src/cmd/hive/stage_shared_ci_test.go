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

func TestStageBaseBranchRedIndex(t *testing.T) {
	t.Cleanup(func() { stageSharedCIIndex.Store(nil) })
	act := &github.ActionableResult{}
	act.PRs.Items = []github.PullRequest{
		// Every failing check is red on the base: base-red.
		{Repo: "acme/widget", Number: 13270, HeadSHA: "aaa", FailingChecks: []string{"docker (hive)", "docker (hub)"}, BaseRedChecks: []string{"docker (hive)", "docker (hub)"}},
		// One PR-local failure on top of the base's: the author's.
		{Repo: "acme/widget", Number: 8, HeadSHA: "bbb", FailingChecks: []string{"docker (hive)", "lint"}, BaseRedChecks: []string{"docker (hive)"}},
	}
	publishStageSharedCIIncidents(act)

	if got := stageBaseBranchRed("acme/widget", 13270, "aaa"); len(got) != 2 || got[0] != "docker (hive)" {
		t.Fatalf("base-red PR = %v", got)
	}
	if got := stageBaseBranchRed("Acme/Widget", 13270, ""); len(got) != 2 {
		t.Fatalf("case/head-agnostic lookup = %v", got)
	}
	if got := stageBaseBranchRed("acme/widget", 13270, "new-head"); got != nil {
		t.Fatalf("new head must not inherit the old head's classification: %v", got)
	}
	if got := stageBaseBranchRed("acme/widget", 8, "bbb"); got != nil {
		t.Fatalf("mixed PR must stay author-facing: %v", got)
	}
	if got := stageSharedCIIncident("acme/widget", 13270, "aaa"); got != 0 {
		t.Fatalf("base-red without an incident reported incident %d", got)
	}
}
