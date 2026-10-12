package github

import (
	"context"
	"strings"
	"testing"
)

// A red PR is deferred as external only while every failing check carries a
// marker for the PR's current head (hivecommons/hive#11609). A push moves the
// head and makes the markers stale; a check that starts failing without a
// marker brings the PR back.
func TestEnrichCIStatus_ExternalCIDeferredOnlyAtMarkedHead(t *testing.T) {
	head := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	marker := func(sha, check string) string {
		return "Red upstream too.\n<!-- hive-ci-external head=" + sha + " check=" + check + " -->"
	}
	srv, _, _ := sharedCIServer(t, map[int][]string{
		1: {"triage", marker(head, "build")},
		2: {marker(other, "build")},
		3: {marker(head, "lint")},
		4: {"no marker"},
		5: {marker(strings.ToUpper(head), "build"), "<!-- hive-shared-ci-500 -->"},
	}, map[int]string{500: "open"})

	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: head},
		{Repo: "org/repo", Number: 2, HeadSHA: head},
		{Repo: "org/repo", Number: 3, HeadSHA: head},
		{Repo: "org/repo", Number: 4, HeadSHA: head},
		{Repo: "org/repo", Number: 5, HeadSHA: head},
	}
	c.EnrichCIStatus(context.Background(), prs)

	want := map[int]bool{1: true, 2: false, 3: false, 4: false, 5: true}
	for _, pr := range prs {
		if pr.CIStatus != "failure" {
			t.Fatalf("PR %d CIStatus = %q, want failure (fixture serves a red check)", pr.Number, pr.CIStatus)
		}
		if pr.ExternalCIDeferred != want[pr.Number] {
			t.Errorf("PR %d ExternalCIDeferred = %v, want %v", pr.Number, pr.ExternalCIDeferred, want[pr.Number])
		}
	}
	if prs[4].SharedCIIncident != 500 {
		t.Errorf("one comment listing must still resolve the shared-CI incident: got %d, want 500", prs[4].SharedCIIncident)
	}
}

func TestExternalCIDeferredNeedsEveryFailingCheck(t *testing.T) {
	head := strings.Repeat("c", 40)
	ogc := "<!-- hive-ci-external head=" + head + " check=Check%20ogc-lts%20%2844%29 -->"
	build := "<!-- hive-ci-external head=" + head + " check=build -->"
	cases := []struct {
		name    string
		texts   []string
		failing []string
		want    bool
	}{
		{"encoded name matches the check run name", []string{ogc}, []string{"Check ogc-lts (44)"}, true},
		{"one marker comment may carry several checks", []string{ogc + "\n" + build}, []string{"build", "Check ogc-lts (44)"}, true},
		{"an unmarked failing check keeps the PR listed", []string{ogc}, []string{"build", "Check ogc-lts (44)"}, false},
		{"no failing check names", []string{ogc}, nil, false},
		{"abbreviated head is not a match", []string{"<!-- hive-ci-external head=cccccccc check=build -->"}, []string{"build"}, false},
	}
	for _, tc := range cases {
		if got := externalCIDeferred(tc.texts, head, tc.failing); got != tc.want {
			t.Errorf("%s: externalCIDeferred = %v, want %v", tc.name, got, tc.want)
		}
	}
	if externalCIDeferred([]string{build}, "", []string{"build"}) {
		t.Error("a PR with no known head must never be deferred")
	}
}
