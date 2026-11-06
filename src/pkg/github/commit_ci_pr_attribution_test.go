package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// projectbluefin/utah-packages#890 (in-repo branch danathar/supersede-878)
// and the fork PR #878 shared head 95b9d22. #878's fork Canary run failed
// (check suite for branch fix/bot-pr-required-check-handoff, no
// pull_requests, as GitHub reports fork runs); #890's own Canary had not
// started, and #890 went to 072 for "Failing checks: Canary".
func attributionServer(t *testing.T, runs []map[string]any, suiteStatus int, calls map[string]int) *httptest.Server {
	t.Helper()
	// Workflow runs behind each check suite, as GET actions/runs?check_suite_id=
	// returns them; suite 900 is another app's (no workflow run).
	workflowRuns := map[string]string{
		"878": `{"total_count":1,"workflow_runs":[{"id":42111306103,"head_branch":"fix/bot-pr-required-check-handoff","head_repository":{"full_name":"kylerankin/r"}}]}`,
		"890": `{"total_count":1,"workflow_runs":[{"id":42114117162,"head_branch":"danathar/supersede-878","head_repository":{"full_name":"o/r"}}]}`,
		"900": `{"total_count":0,"workflow_runs":[]}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls[r.URL.Path]++
		switch r.URL.Path {
		case "/repos/o/r/commits/95b9d22/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "statuses": []any{}})
		case "/repos/o/r/commits/95b9d22/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
		case "/repos/o/r/actions/runs":
			suite := r.URL.Query().Get("check_suite_id")
			calls["suite:"+suite]++
			if suite == "878" && suiteStatus != 0 {
				w.WriteHeader(suiteStatus)
				return
			}
			_, _ = w.Write([]byte(workflowRuns[suite]))
		case "/repos/o/r/check-suites/900":
			_, _ = w.Write([]byte(`{"id":900,"head_branch":"other-app-branch","head_sha":"95b9d22"}`))
		case "/repos/o/r/pulls/890":
			_, _ = w.Write([]byte(`{"number":890,"head":{"ref":"danathar/supersede-878","sha":"95b9d22","repo":{"full_name":"o/r"}}}`))
		case "/repos/o/r/pulls/891":
			// An in-repo copy pushed under the fork's branch name
			// (gh pr checkout 878 && git push origin HEAD).
			_, _ = w.Write([]byte(`{"number":891,"head":{"ref":"fix/bot-pr-required-check-handoff","sha":"95b9d22","repo":{"full_name":"o/r"}}}`))
		case "/repos/o/r/pulls/878":
			_, _ = w.Write([]byte(`{"number":878,"head":{"ref":"fix/bot-pr-required-check-handoff","sha":"95b9d22","repo":{"full_name":"kylerankin/r"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func checkRunJSON(id int64, name, status, conclusion string, suite int64, prs ...int) map[string]any {
	run := map[string]any{"id": id, "name": name, "status": status, "head_sha": "95b9d22", "app": map[string]any{"id": 15368}, "check_suite": map[string]any{"id": suite}, "started_at": "2026-11-05T21:17:39Z"}
	if conclusion != "" {
		run["conclusion"] = conclusion
	}
	list := []any{}
	for _, n := range prs {
		list = append(list, map[string]any{"number": n, "base": map[string]any{"ref": "main", "repo": map[string]any{"name": "r"}}})
	}
	run["pull_requests"] = list
	return run
}

func evaluateAttribution(t *testing.T, srv *httptest.Server, pr int) CommitCIState {
	t.Helper()
	client := gh.NewClient(nil)
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	st, err := EvaluateCommitCI(context.Background(), client, "o", "r", "95b9d22", CommitCIOptions{
		PRNumber:             pr,
		Required:             map[string]bool{"build": true},
		RequiredKnown:        true,
		FailOnAnyFailedCheck: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCheckRunsAttributedByPR(t *testing.T) {
	build := checkRunJSON(1, "build", "completed", "success", 890, 890)
	forkCanary := checkRunJSON(2, "Canary", "completed", "failure", 878)
	for _, tc := range []struct {
		name        string
		pr          int
		runs        []map[string]any
		suiteStatus int
		wantRed     bool
	}{
		{name: "#890: #878's fork Canary on the shared head is not #890's", pr: 890, runs: []map[string]any{build, forkCanary}, wantRed: false},
		{name: "#878: its own fork Canary still counts", pr: 878, runs: []map[string]any{build, forkCanary}, wantRed: true},
		{name: "#890: a run naming another PR only is not #890's", pr: 890, runs: []map[string]any{build, checkRunJSON(3, "Canary", "completed", "failure", 878, 878)}, wantRed: false},
		{name: "#890: its own red Canary counts", pr: 890, runs: []map[string]any{build, checkRunJSON(4, "Canary", "completed", "failure", 890, 890)}, wantRed: true},
		{name: "#890: a red run with no PR on its own branch counts (push run)", pr: 890, runs: []map[string]any{build, checkRunJSON(5, "Canary", "completed", "failure", 890)}, wantRed: true},
		{name: "#890: suite lookup fails, the run is kept", pr: 890, runs: []map[string]any{build, forkCanary}, suiteStatus: http.StatusInternalServerError, wantRed: true},
		{name: "#891: in-repo copy under the fork's branch name: the fork run is not #891's", pr: 891, runs: []map[string]any{build, forkCanary}, wantRed: false},
		{name: "#890: another app's suite on another branch is not #890's", pr: 890, runs: []map[string]any{build, checkRunJSON(7, "Canary", "completed", "failure", 900)}, wantRed: false},
		{name: "#891: another app's suite is judged on branch alone", pr: 891, runs: []map[string]any{build, checkRunJSON(8, "Canary", "completed", "failure", 900)}, wantRed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[string]int{}
			srv := attributionServer(t, tc.runs, tc.suiteStatus, calls)
			defer srv.Close()
			st := evaluateAttribution(t, srv, tc.pr)
			red := strings.HasPrefix(st.Reason, "check-failing:Canary") || len(st.FailingNonRequired) > 0
			if red != tc.wantRed {
				t.Fatalf("Canary red = %v (reason %q, failing %v), want %v", red, st.Reason, st.FailingNonRequired, tc.wantRed)
			}
		})
	}
}

// Without a PR number (base-branch or commit-only reads), nothing is
// filtered and no extra lookup is made; green runs never cost one either.
func TestCheckRunAttributionLookupsAreBounded(t *testing.T) {
	calls := map[string]int{}
	srv := attributionServer(t, []map[string]any{checkRunJSON(1, "build", "completed", "success", 890), checkRunJSON(2, "Canary", "completed", "failure", 878)}, 0, calls)
	defer srv.Close()
	if st := evaluateAttribution(t, srv, 0); !strings.HasPrefix(st.Reason, "check-failing:Canary") {
		t.Fatalf("commit-only read must keep every run: reason %q", st.Reason)
	}
	if calls["/repos/o/r/actions/runs"] != 0 || calls["/repos/o/r/pulls/890"] != 0 {
		t.Fatalf("lookups without a PR: %v", calls)
	}
	calls = map[string]int{}
	srv2 := attributionServer(t, []map[string]any{checkRunJSON(1, "build", "completed", "success", 890), checkRunJSON(6, "lint", "completed", "success", 878)}, 0, calls)
	defer srv2.Close()
	evaluateAttribution(t, srv2, 890)
	if calls["/repos/o/r/actions/runs"] != 0 || calls["/repos/o/r/pulls/890"] != 0 {
		t.Fatalf("green runs cost lookups: %v", calls)
	}
}

// Two red fork runs from one check suite cost one suite lookup and one PR read.
func TestCheckRunAttributionCachesSuite(t *testing.T) {
	calls := map[string]int{}
	srv := attributionServer(t, []map[string]any{
		checkRunJSON(1, "build", "completed", "success", 890, 890),
		checkRunJSON(2, "Canary", "completed", "failure", 878),
		checkRunJSON(3, "Canary-arm", "completed", "failure", 878),
	}, 0, calls)
	defer srv.Close()
	if st := evaluateAttribution(t, srv, 890); st.Reason != "" || len(st.FailingNonRequired) != 0 {
		t.Fatalf("reason %q failing %v, want #878's runs dropped", st.Reason, st.FailingNonRequired)
	}
	if calls["suite:878"] != 1 || calls["/repos/o/r/pulls/890"] != 1 {
		t.Fatalf("lookups = %v, want one suite read and one PR read", calls)
	}
}
