package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func externalMarker(head, check string, run int64) string {
	return "Red upstream too.\n<!-- hive-ci-external head=" + head + " check=" + check + " run=" + strconv.FormatInt(run, 10) + " -->"
}

// A red PR is deferred as external only while every failing check run
// carries a marker for the PR's current head and that run
// (hivecommons/hive#11609). sharedCIServer serves one failing run, id 1.
func TestEnrichCIStatus_ExternalCIDeferredOnlyAtMarkedHeadAndRun(t *testing.T) {
	head := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	srv, _, _ := sharedCIServer(t, map[int][]string{
		1: {"triage", externalMarker(head, "build", 1)},
		2: {externalMarker(other, "build", 1)},
		3: {externalMarker(head, "build", 2)},
		4: {"no marker"},
		5: {externalMarker(strings.ToUpper(head), "build", 1), "<!-- hive-shared-ci-500 -->"},
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

// Red → green → red at one head: the marker stamped for the first failing run
// must not hide the second failure, which is a new run (hivecommons/hive#11609).
func TestEnrichCIStatus_ExternalCIDeferralEndsWhenTheCheckFailsAgain(t *testing.T) {
	head := strings.Repeat("d", 40)
	var mu sync.Mutex
	run := map[string]any{"id": 101, "name": "build", "status": "completed", "conclusion": "failure"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case regexp.MustCompile(`/check-runs$`).MatchString(r.URL.Path):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]any{run}})
		case regexp.MustCompile(`/issues/1/comments$`).MatchString(r.URL.Path):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"body": externalMarker(head, "build", 101)}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	pass := func(id int, conclusion string) PullRequest {
		mu.Lock()
		run = map[string]any{"id": id, "name": "build", "status": "completed", "conclusion": conclusion}
		mu.Unlock()
		prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: head}}
		c.EnrichCIStatus(context.Background(), prs)
		return prs[0]
	}

	if pr := pass(101, "failure"); !pr.ExternalCIDeferred {
		t.Fatalf("the marked failing run must be deferred: %+v", pr)
	}
	if pr := pass(102, "success"); pr.CIStatus != "success" || pr.ExternalCIDeferred {
		t.Fatalf("a green check is not deferred: CIStatus=%q deferred=%v", pr.CIStatus, pr.ExternalCIDeferred)
	}
	if pr := pass(103, "failure"); pr.CIStatus != "failure" || pr.ExternalCIDeferred {
		t.Errorf("a new failing run at the same head must be listed again: CIStatus=%q deferred=%v", pr.CIStatus, pr.ExternalCIDeferred)
	}
}

func TestExternalCIDeferredNeedsEveryFailingRun(t *testing.T) {
	head := strings.Repeat("c", 40)
	ogc := externalMarker(head, "Check%20ogc-lts%20%2844%29", 7)
	build := externalMarker(head, "build", 8)
	cases := []struct {
		name    string
		texts   []string
		failing []int64
		want    bool
	}{
		{"marked run", []string{ogc}, []int64{7}, true},
		{"one comment may carry several markers", []string{ogc + "\n" + build}, []int64{8, 7}, true},
		{"an unmarked failing run keeps the PR listed", []string{ogc}, []int64{7, 8}, false},
		{"no failing run ids", []string{ogc}, nil, false},
		{"marker without a run id is not a match", []string{"<!-- hive-ci-external head=" + head + " check=build -->"}, []int64{8}, false},
		{"abbreviated head is not a match", []string{"<!-- hive-ci-external head=cccccccc check=build run=8 -->"}, []int64{8}, false},
	}
	for _, tc := range cases {
		if got := externalCIDeferred(tc.texts, head, tc.failing); got != tc.want {
			t.Errorf("%s: externalCIDeferred = %v, want %v", tc.name, got, tc.want)
		}
	}
	if externalCIDeferred([]string{build}, "", []int64{8}) {
		t.Error("a PR with no known head must never be deferred")
	}
}

// hivecommons/hive#12977: a transient-CI triage marker is pinned to the head
// and failing run exactly like the external marker, and the two never stand
// in for each other.
func TestTransientCITriagedNeedsEveryFailingRun(t *testing.T) {
	head := strings.Repeat("d", 40)
	transient := func(check string, run int64) string {
		return "Not the bump: the cache was not signed yet.\n<!-- hive-ci-transient head=" + head + " check=" + check + " run=" + strconv.FormatInt(run, 10) + " -->"
	}
	build := transient("Build%20PR%20Image%20%28No%20Push%29", 9)
	cases := []struct {
		name    string
		texts   []string
		failing []int64
		want    bool
	}{
		{"marked run", []string{build}, []int64{9}, true},
		{"an unmarked failing run keeps the PR untriaged", []string{build}, []int64{9, 10}, false},
		{"a re-run is a new run id", []string{build}, []int64{11}, false},
		{"no failing run ids", []string{build}, nil, false},
		{"marker for another head is stale", []string{strings.Replace(build, head, strings.Repeat("e", 40), 1)}, []int64{9}, false},
		{"an external marker is not a transient triage", []string{externalMarker(head, "build", 9)}, []int64{9}, false},
	}
	for _, tc := range cases {
		if got := transientCITriaged(tc.texts, head, tc.failing); got != tc.want {
			t.Errorf("%s: transientCITriaged = %v, want %v", tc.name, got, tc.want)
		}
	}
	if externalCIDeferred([]string{build}, head, []int64{9}) {
		t.Error("a transient marker must not defer the PR as failing upstream")
	}
	if transientCITriaged([]string{build}, "", []int64{9}) {
		t.Error("a PR with no known head must never be triaged")
	}
}

// The batched GraphQL path must carry each check run's real id: the marker is
// pinned to it, and a positional stand-in would never match.
func TestGraphQLCheckRunsCarryDatabaseID(t *testing.T) {
	n := decodeGQLPRNode(t, gqlPRNodeWithContextsJSON(1, "sha-1",
		`{"__typename":"CheckRun","databaseId":987654321,"name":"build","status":"COMPLETED","conclusion":"FAILURE"},`+
			`{"__typename":"StatusContext","context":"DCO","state":"FAILURE"}`))
	checks, ok := graphQLCheckRuns(n)
	if !ok || len(checks) != 2 {
		t.Fatalf("graphQLCheckRuns = %v, %v", checks, ok)
	}
	if checks[0].GetID() != 987654321 {
		t.Errorf("check run id = %d, want the databaseId 987654321", checks[0].GetID())
	}
	if checks[1].GetID() != 2 {
		t.Errorf("status context id = %d, want positional 2 (a status has no run id)", checks[1].GetID())
	}
}
