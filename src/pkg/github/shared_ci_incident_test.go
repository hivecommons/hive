package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestNewestSharedCIMarker(t *testing.T) {
	cases := []struct {
		name  string
		texts []string
		want  int
	}{
		{"none", []string{"", "plain comment", "<!-- hive-finding: abc -->"}, 0},
		{"earlier marker kept when later comments have none", []string{"<!-- hive-shared-ci-42 -->", "later"}, 42},
		{"newest comment wins", []string{"", "<!-- hive-shared-ci-10 -->", "x", "<!-- hive-shared-ci-12 -->"}, 12},
		{"last marker in one comment wins", []string{"<!-- hive-shared-ci-7 --> then <!-- hive-shared-ci-9 -->"}, 9},
		{"malformed markers ignored", []string{"<!-- hive-shared-ci-0 -->", "<!-- hive-shared-ci-#5 -->", "<!--hive-shared-ci-5-->"}, 0},
	}
	for _, tc := range cases {
		if got := newestSharedCIMarker(tc.texts); got != tc.want {
			t.Errorf("%s: newestSharedCIMarker = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// sharedCIServer serves a red check run for every PR, each PR's comments from
// comments, and incident issues whose state comes from incidentState. It
// counts issue GETs per incident.
func sharedCIServer(t *testing.T, comments map[int][]string, incidentState map[int]string) (*httptest.Server, map[int]int, *sync.Mutex) {
	t.Helper()
	incidents := map[int]map[string]any{}
	for n, state := range incidentState {
		incidents[n] = map[string]any{"state": state}
	}
	return sharedCIServerIssues(t, comments, incidents)
}

// sharedCIServerIssues is sharedCIServer with each incident issue's full
// payload (state, closed_at, state_reason).
func sharedCIServerIssues(t *testing.T, comments map[int][]string, incidentIssues map[int]map[string]any) (*httptest.Server, map[int]int, *sync.Mutex) {
	t.Helper()
	commentsRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)/comments$`)
	issueRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)$`)
	var mu sync.Mutex
	issueGets := map[int]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m := commentsRE.FindStringSubmatch(r.URL.Path); m != nil {
			n, _ := strconv.Atoi(m[1])
			out := []map[string]any{}
			for _, body := range comments[n] {
				out = append(out, map[string]any{"body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if m := issueRE.FindStringSubmatch(r.URL.Path); m != nil {
			n, _ := strconv.Atoi(m[1])
			fields, ok := incidentIssues[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			mu.Lock()
			issueGets[n]++
			mu.Unlock()
			out := map[string]any{"number": n}
			for k, v := range fields {
				out[k] = v
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		if regexp.MustCompile(`/commits/[^/]+/check-runs$`).MatchString(r.URL.Path) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs": []map[string]any{
					{"id": 1, "name": "build", "status": "completed", "conclusion": "failure"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, issueGets, &mu
}

// A red PR whose newest shared-CI marker names a still-open incident is
// deferred; a closed incident, an older marker superseded by a newer one,
// or no marker at all leaves the PR in the repair lists (hivecommons/hive#10528).
func TestEnrichCIStatus_SharedCIIncidentOnlyWhileIncidentOpen(t *testing.T) {
	srv, issueGets, mu := sharedCIServer(t, map[int][]string{
		1: {"triage", "Deferred to #500.\n<!-- hive-shared-ci-500 -->"},
		2: {"Deferred to #600.\n<!-- hive-shared-ci-600 -->"},
		3: {"<!-- hive-shared-ci-500 -->", "Re-deferred to #600.\n<!-- hive-shared-ci-600 -->"},
		4: {"no marker"},
		5: {"<!-- hive-shared-ci-500 -->"},
	}, map[int]string{500: "open", 600: "closed"})

	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: "a1"},
		{Repo: "org/repo", Number: 2, HeadSHA: "a2"},
		{Repo: "org/repo", Number: 3, HeadSHA: "a3"},
		{Repo: "org/repo", Number: 4, HeadSHA: "a4"},
		{Repo: "org/repo", Number: 5, HeadSHA: "a5"},
	}
	c.EnrichCIStatus(context.Background(), prs)

	want := map[int]int{1: 500, 2: 0, 3: 0, 4: 0, 5: 500}
	for _, pr := range prs {
		if pr.CIStatus != "failure" {
			t.Fatalf("PR %d CIStatus = %q, want failure (fixture serves a red check)", pr.Number, pr.CIStatus)
		}
		if pr.SharedCIIncident != want[pr.Number] {
			t.Errorf("PR %d SharedCIIncident = %d, want %d", pr.Number, pr.SharedCIIncident, want[pr.Number])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if issueGets[500] != 1 || issueGets[600] != 1 {
		t.Errorf("incident state must be fetched once per incident per pass, got %v", issueGets)
	}
}

// An incident closed as completed moments ago keeps its PRs deferred until
// the rerun sweep has re-run them (its rerun marker): until then the head is
// red only because the failed jobs have not run again on the repaired base,
// and handing it back to its author then posted a "will not merge until the
// head changes" nudge (hivecommons/hive#17003). An old close, a not-planned
// close (no re-run comes) or a re-run already stamped releases the PR.
func TestEnrichCIStatus_SharedCIIncidentHeldUntilRerunAfterClose(t *testing.T) {
	recent := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	srv, issueGets, mu := sharedCIServerIssues(t, map[int][]string{
		1: {"<!-- hive-shared-ci-700 -->"},
		2: {"<!-- hive-shared-ci-700 -->", "Re-ran the failed jobs.\n<!-- hive-shared-ci-rerun-700 -->"},
		3: {"<!-- hive-shared-ci-701 -->"},
		4: {"<!-- hive-shared-ci-702 -->"},
		5: {"<!-- hive-shared-ci-700 -->", "<!-- hive-shared-ci-rerun-650 -->"},
	}, map[int]map[string]any{
		700: {"state": "closed", "state_reason": "completed", "closed_at": recent},
		701: {"state": "closed", "state_reason": "completed", "closed_at": old},
		702: {"state": "closed", "state_reason": "not_planned", "closed_at": recent},
	})
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: "a1"},
		{Repo: "org/repo", Number: 2, HeadSHA: "a2"},
		{Repo: "org/repo", Number: 3, HeadSHA: "a3"},
		{Repo: "org/repo", Number: 4, HeadSHA: "a4"},
		{Repo: "org/repo", Number: 5, HeadSHA: "a5"},
	}
	c.EnrichCIStatus(context.Background(), prs)
	want := map[int]int{1: 700, 2: 0, 3: 0, 4: 0, 5: 700}
	for _, pr := range prs {
		if pr.SharedCIIncident != want[pr.Number] {
			t.Errorf("PR %d SharedCIIncident = %d, want %d", pr.Number, pr.SharedCIIncident, want[pr.Number])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if issueGets[700] != 1 {
		t.Errorf("incident 700 fetched %d times in one pass, want 1", issueGets[700])
	}
}

// A lookup failure must never hide a PR: an unknown incident (404) leaves
// it listed.
func TestEnrichCIStatus_SharedCIIncidentLookupFailureKeepsPRListed(t *testing.T) {
	srv, _, _ := sharedCIServer(t, map[int][]string{
		1: {"<!-- hive-shared-ci-777 -->"},
	}, map[int]string{})
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "a1"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].SharedCIIncident != 0 {
		t.Errorf("SharedCIIncident = %d, want 0 when the incident state cannot be read", prs[0].SharedCIIncident)
	}
}

// Green PRs are never looked up: the marker only matters for a red PR.
func TestEnrichCIStatus_GreenPRSkipsSharedCILookup(t *testing.T) {
	var commentCalls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case regexp.MustCompile(`/check-runs$`).MatchString(r.URL.Path):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs":  []map[string]any{{"name": "build", "status": "completed", "conclusion": "success"}},
			})
		case regexp.MustCompile(`/comments$`).MatchString(r.URL.Path):
			mu.Lock()
			commentCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode([]map[string]any{{"body": "<!-- hive-shared-ci-1 -->"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "a1"}}
	c.EnrichCIStatus(context.Background(), prs)
	mu.Lock()
	defer mu.Unlock()
	if commentCalls != 0 || prs[0].SharedCIIncident != 0 {
		t.Errorf("green PR: comment calls = %d, SharedCIIncident = %d; want 0, 0", commentCalls, prs[0].SharedCIIncident)
	}
}
