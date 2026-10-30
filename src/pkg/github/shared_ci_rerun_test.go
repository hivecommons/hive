package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type sharedCIRerunIssue struct {
	Number      int
	Title       string
	StateReason string
	ClosedAt    time.Time
	IsPR        bool
}

type sharedCIRerunRun struct {
	ID         int64
	WorkflowID int64
	RunNumber  int
	Attempt    int
	Status     string
	Conclusion string
}

type sharedCIRerunPR struct {
	Number   int
	SHA      string
	Labels   []string
	Comments []string
	Runs     []sharedCIRerunRun
}

type sharedCIRerunFake struct {
	mu           sync.Mutex
	issues       []sharedCIRerunIssue
	prs          map[int]*sharedCIRerunPR
	reruns       []int64
	posted       map[int][]string
	commentLists map[int]int
	forbidRerun  bool
}

func newSharedCIRerunFake(t *testing.T, issues []sharedCIRerunIssue, prs []*sharedCIRerunPR) (*httptest.Server, *sharedCIRerunFake) {
	t.Helper()
	f := &sharedCIRerunFake{issues: issues, prs: map[int]*sharedCIRerunPR{}, posted: map[int][]string{}, commentLists: map[int]int{}}
	for _, pr := range prs {
		f.prs[pr.Number] = pr
	}
	commentsRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)/comments$`)
	rerunRE := regexp.MustCompile(`^/repos/org/repo/actions/runs/(\d+)/rerun-failed-jobs$`)
	updated := "2026-10-15T00:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/org/repo/issues" && r.Method == http.MethodGet:
			if r.URL.Query().Get("state") != "closed" || r.URL.Query().Get("since") == "" {
				t.Errorf("issue list query = %q, want state=closed with since", r.URL.RawQuery)
			}
			out := []map[string]any{}
			for _, is := range f.issues {
				item := map[string]any{"number": is.Number, "title": is.Title, "state": "closed", "closed_at": is.ClosedAt.Format(time.RFC3339)}
				if is.StateReason != "" {
					item["state_reason"] = is.StateReason
				}
				if is.IsPR {
					item["pull_request"] = map[string]any{"url": "x"}
				}
				out = append(out, item)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/repos/org/repo/pulls" && r.Method == http.MethodGet:
			out := []map[string]any{}
			for n := 1; n <= 100; n++ {
				pr, ok := f.prs[n]
				if !ok {
					continue
				}
				labels := []map[string]any{}
				for _, l := range pr.Labels {
					labels = append(labels, map[string]any{"name": l})
				}
				out = append(out, map[string]any{"number": pr.Number, "state": "open", "updated_at": updated, "labels": labels, "head": map[string]any{"sha": pr.SHA}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case commentsRE.MatchString(r.URL.Path):
			n, _ := strconv.Atoi(commentsRE.FindStringSubmatch(r.URL.Path)[1])
			pr := f.prs[n]
			if pr == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodPost {
				var body struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				pr.Comments = append(pr.Comments, body.Body)
				f.posted[n] = append(f.posted[n], body.Body)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": body.Body})
				return
			}
			f.commentLists[n]++
			out := []map[string]any{}
			for _, body := range pr.Comments {
				out = append(out, map[string]any{"body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/repos/org/repo/actions/runs" && r.Method == http.MethodGet:
			sha := r.URL.Query().Get("head_sha")
			runs := []map[string]any{}
			for _, pr := range f.prs {
				if pr.SHA != sha {
					continue
				}
				for _, run := range pr.Runs {
					runs = append(runs, map[string]any{
						"id": run.ID, "workflow_id": run.WorkflowID, "run_number": run.RunNumber, "run_attempt": run.Attempt,
						"status": run.Status, "conclusion": run.Conclusion, "head_sha": sha, "name": "CI",
						"html_url": "https://github.com/org/repo/actions/runs/" + strconv.FormatInt(run.ID, 10),
					})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
		case rerunRE.MatchString(r.URL.Path) && r.Method == http.MethodPost:
			id, _ := strconv.ParseInt(rerunRE.FindStringSubmatch(r.URL.Path)[1], 10, 64)
			f.reruns = append(f.reruns, id)
			if f.forbidRerun {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

// snap returns copies of the observations under the lock.
func (f *sharedCIRerunFake) snap() (reruns []int64, posted map[int][]string, lists map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reruns = append([]int64(nil), f.reruns...)
	posted = map[int][]string{}
	for k, v := range f.posted {
		posted[k] = append([]string(nil), v...)
	}
	lists = map[int]int{}
	for k, v := range f.commentLists {
		lists[k] = v
	}
	return reruns, posted, lists
}

func sharedCIRerunNow() time.Time { return time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) }

func failedRun(id, workflow int64) sharedCIRerunRun {
	return sharedCIRerunRun{ID: id, WorkflowID: workflow, RunNumber: 1, Attempt: 1, Status: "completed", Conclusion: "failure"}
}

func TestSweepSharedCIReruns_RerunsStampedPRsOncePerIncident(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{
		{Number: 500, Title: "[shared-ci] test (rest 1/5) failing across org/repo", ClosedAt: now.Add(-time.Hour)},
		{Number: 501, Title: "unrelated bug", ClosedAt: now.Add(-time.Hour)},
	}
	prs := []*sharedCIRerunPR{
		{Number: 1, SHA: "aaaaaaaaaa", Comments: []string{"deferring\n<!-- hive-shared-ci-500 -->"}, Runs: []sharedCIRerunRun{failedRun(11, 1), {ID: 12, WorkflowID: 2, RunNumber: 1, Attempt: 1, Status: "completed", Conclusion: "success"}}},
		{Number: 2, SHA: "bbbbbbbbbb", Comments: []string{"no marker here"}, Runs: []sharedCIRerunRun{failedRun(21, 1)}},
		{Number: 3, SHA: "cccccccccc", Comments: []string{"<!-- hive-shared-ci-500 -->", "<!-- hive-shared-ci-rerun-500 -->"}, Runs: []sharedCIRerunRun{failedRun(31, 1)}},
		{Number: 4, SHA: "dddddddddd", Comments: []string{"<!-- hive-shared-ci-501 -->"}, Runs: []sharedCIRerunRun{failedRun(41, 1)}},
		{Number: 5, SHA: "eeeeeeeeee", Labels: []string{"hold"}, Comments: []string{"<!-- hive-shared-ci-500 -->"}, Runs: []sharedCIRerunRun{failedRun(51, 1)}},
	}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})

	var audited []SharedCIRerunEvent
	opts := SharedCIRerunOptions{Now: sharedCIRerunNow, Audit: func(e SharedCIRerunEvent) { audited = append(audited, e) }}
	res, err := c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, posted, lists := fake.snap()
	if !reflect.DeepEqual(reruns, []int64{11}) {
		t.Fatalf("reruns = %v, want [11] (only PR 1's failed workflow)", reruns)
	}
	if res.Incidents != 1 || len(res.Rerun) != 1 || res.Rerun[0].Number != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(audited) != 1 || !reflect.DeepEqual(audited[0].Incidents, []int{500}) || !reflect.DeepEqual(audited[0].RunIDs, []int64{11}) {
		t.Fatalf("audit = %+v", audited)
	}
	if len(posted[1]) != 1 || !strings.Contains(posted[1][0], "<!-- hive-shared-ci-rerun-500 -->") || !strings.Contains(posted[1][0], "#500") {
		t.Fatalf("PR 1 marker comment = %q", posted[1])
	}
	for _, n := range []int{2, 3, 4, 5} {
		if len(posted[n]) != 0 {
			t.Fatalf("PR %d got a comment: %q", n, posted[n])
		}
	}
	if lists[5] != 0 {
		t.Fatalf("held PR 5 comments were listed %d times", lists[5])
	}

	listsBefore := lists[2]
	res, err = c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	reruns, _, lists = fake.snap()
	if len(reruns) != 1 || len(res.Rerun) != 0 {
		t.Fatalf("second pass re-ran again: reruns=%v result=%+v", reruns, res)
	}
	if lists[2] != listsBefore {
		t.Fatalf("unchanged PR 2 comments re-listed on second pass")
	}
}

func TestSweepSharedCIReruns_IgnoresNotPlannedStaleAndPRIncidents(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{
		{Number: 600, Title: "[shared-ci] build failing across org/repo", StateReason: "not_planned", ClosedAt: now.Add(-time.Hour)},
		{Number: 601, Title: "[shared-ci] lint failing across org/repo", ClosedAt: now.Add(-10 * 24 * time.Hour)},
		{Number: 602, Title: "[shared-ci] fix the thing", ClosedAt: now.Add(-time.Hour), IsPR: true},
	}
	prs := []*sharedCIRerunPR{
		{Number: 1, SHA: "aaaaaaaaaa", Comments: []string{"<!-- hive-shared-ci-600 -->", "<!-- hive-shared-ci-601 -->", "<!-- hive-shared-ci-602 -->"}, Runs: []sharedCIRerunRun{failedRun(11, 1)}},
	}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	res, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, _, lists := fake.snap()
	if res.Incidents != 0 || len(reruns) != 0 || lists[1] != 0 {
		t.Fatalf("expected no work: result=%+v reruns=%v lists=%d", res, reruns, lists[1])
	}
}

func TestSweepSharedCIReruns_SkipsWorkflowWhoseNewestRunPassed(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{{Number: 700, Title: "[shared-ci] test failing across org/repo", ClosedAt: now.Add(-time.Hour)}}
	prs := []*sharedCIRerunPR{{
		Number: 1, SHA: "aaaaaaaaaa", Comments: []string{"<!-- hive-shared-ci-700 -->"},
		Runs: []sharedCIRerunRun{
			failedRun(11, 1),
			{ID: 12, WorkflowID: 1, RunNumber: 1, Attempt: 2, Status: "completed", Conclusion: "success"},
			{ID: 13, WorkflowID: 2, RunNumber: 3, Attempt: 1, Status: "in_progress"},
		},
	}}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	res, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, posted, _ := fake.snap()
	if len(reruns) != 0 || len(res.Rerun) != 0 || len(posted[1]) != 0 {
		t.Fatalf("expected no rerun: reruns=%v result=%+v posted=%q", reruns, res, posted[1])
	}
}

func TestPendingSharedCIReruns(t *testing.T) {
	closed := map[int]bool{10: true, 11: true}
	cases := []struct {
		name  string
		texts []string
		want  []int
	}{
		{"no marker", []string{"hello"}, nil},
		{"closed incident pending", []string{"<!-- hive-shared-ci-10 -->"}, []int{10}},
		{"already rerun", []string{"<!-- hive-shared-ci-10 -->", "<!-- hive-shared-ci-rerun-10 -->"}, nil},
		{"newest marker still open", []string{"<!-- hive-shared-ci-10 -->", "<!-- hive-shared-ci-12 -->"}, nil},
		{"two closed incidents", []string{"<!-- hive-shared-ci-11 -->", "<!-- hive-shared-ci-10 -->"}, []int{10, 11}},
		{"rerun marker alone is not a deferral", []string{"<!-- hive-shared-ci-rerun-10 -->"}, nil},
	}
	for _, tc := range cases {
		if got := pendingSharedCIReruns(tc.texts, closed); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSweepSharedCIReruns_ForbiddenStopsAndIsRememberedPerRepo(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{{Number: 800, Title: "[shared-ci] test failing across org/repo", ClosedAt: now.Add(-time.Hour)}}
	marker := []string{"<!-- hive-shared-ci-800 -->"}
	prs := []*sharedCIRerunPR{
		{Number: 1, SHA: "aaaaaaaaaa", Comments: marker, Runs: []sharedCIRerunRun{failedRun(11, 1), failedRun(12, 2), failedRun(13, 3)}},
		{Number: 2, SHA: "bbbbbbbbbb", Comments: marker, Runs: []sharedCIRerunRun{failedRun(21, 1), failedRun(22, 2)}},
	}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	fake.forbidRerun = true
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	res, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, posted, _ := fake.snap()
	if len(reruns) != 1 {
		t.Fatalf("reruns = %v, want exactly one POST across both PRs", reruns)
	}
	if len(res.Rerun) != 0 || len(posted[1]) != 0 || len(posted[2]) != 0 {
		t.Fatalf("expected no rerun result or marker comment: result=%+v posted=%v", res, posted)
	}
}
