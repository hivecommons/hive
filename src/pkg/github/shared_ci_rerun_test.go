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
	// Open keeps the issue out of the closed list and serves it as open
	// from GET /issues/{n}.
	Open bool
}

func (is sharedCIRerunIssue) json() map[string]any {
	item := map[string]any{"number": is.Number, "title": is.Title, "state": "closed", "closed_at": is.ClosedAt.Format(time.RFC3339)}
	if is.Open {
		item["state"] = "open"
		delete(item, "closed_at")
	}
	if is.StateReason != "" {
		item["state_reason"] = is.StateReason
	}
	if is.IsPR {
		item["pull_request"] = map[string]any{"url": "x"}
	}
	return item
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
	fullReruns   []int64
	posted       map[int][]string
	commentLists map[int]int
	forbidRerun  bool
	forbidAfter  int
	issueGets    int
}

func newSharedCIRerunFake(t *testing.T, issues []sharedCIRerunIssue, prs []*sharedCIRerunPR) (*httptest.Server, *sharedCIRerunFake) {
	t.Helper()
	f := &sharedCIRerunFake{issues: issues, prs: map[int]*sharedCIRerunPR{}, posted: map[int][]string{}, commentLists: map[int]int{}}
	for _, pr := range prs {
		f.prs[pr.Number] = pr
	}
	commentsRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)/comments$`)
	issueRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)$`)
	rerunRE := regexp.MustCompile(`^/repos/org/repo/actions/runs/(\d+)/rerun-failed-jobs$`)
	fullRerunRE := regexp.MustCompile(`^/repos/org/repo/actions/runs/(\d+)/rerun$`)
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
				if is.Open {
					continue
				}
				out = append(out, is.json())
			}
			_ = json.NewEncoder(w).Encode(out)
		case issueRE.MatchString(r.URL.Path) && r.Method == http.MethodGet:
			n, _ := strconv.Atoi(issueRE.FindStringSubmatch(r.URL.Path)[1])
			f.issueGets++
			for _, is := range f.issues {
				if is.Number == n {
					_ = json.NewEncoder(w).Encode(is.json())
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
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
			if f.forbidRerun || (f.forbidAfter > 0 && len(f.reruns) > f.forbidAfter) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
				return
			}
			w.WriteHeader(http.StatusCreated)
		case fullRerunRE.MatchString(r.URL.Path) && r.Method == http.MethodPost:
			id, _ := strconv.ParseInt(fullRerunRE.FindStringSubmatch(r.URL.Path)[1], 10, 64)
			f.fullReruns = append(f.fullReruns, id)
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
		// Filed without the prefix, yet PR 4 was deferred to it: the marker,
		// not the title, says it is an incident (hivecommons/hive#17500).
		{Number: 501, Title: "lint failing across org/repo", ClosedAt: now.Add(-time.Hour)},
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
	if !reflect.DeepEqual(reruns, []int64{11, 41}) {
		t.Fatalf("reruns = %v, want [11 41] (PR 1's and PR 4's failed workflows)", reruns)
	}
	if res.Incidents != 2 || len(res.Rerun) != 2 || res.Rerun[0].Number != 1 || res.Rerun[1].Number != 4 {
		t.Fatalf("result = %+v", res)
	}
	if len(audited) != 2 || !reflect.DeepEqual(audited[0].Incidents, []int{500}) || !reflect.DeepEqual(audited[0].RunIDs, []int64{11}) || !reflect.DeepEqual(audited[1].Incidents, []int{501}) {
		t.Fatalf("audit = %+v", audited)
	}
	if len(posted[1]) != 1 || !strings.Contains(posted[1][0], "<!-- hive-shared-ci-rerun-500 -->") || !strings.Contains(posted[1][0], "#500") {
		t.Fatalf("PR 1 marker comment = %q", posted[1])
	}
	if len(posted[4]) != 1 || !strings.Contains(posted[4][0], "<!-- hive-shared-ci-lifted-501 -->") {
		t.Fatalf("PR 4 marker comment = %q", posted[4])
	}
	for _, n := range []int{2, 3, 5} {
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
	if len(reruns) != 2 || len(res.Rerun) != 0 {
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
	reruns, posted, lists := fake.snap()
	if res.Incidents != 0 || len(reruns) != 0 || len(posted[1]) != 0 || lists[1] != 1 {
		t.Fatalf("expected no re-run (one comment read to find the marker): result=%+v reruns=%v posted=%d lists=%d", res, reruns, len(posted[1]), lists[1])
	}
}

// Incident #17463 was filed as "mutation failing across hivecommons/hive"
// without the `[shared-ci]` prefix, yet the scanner stamped its marker on
// #17471; the title-prefix path never saw it close. An incident is now
// recognised by the marker a PR carries: the sweep fetches it by number,
// re-runs once when it is closed, and stays quiet while it is open — one
// issue GET per pass, no re-reading of the unchanged PR's comments.
func TestSweepSharedCIReruns_LiftsDeferralToAnIncidentFiledWithoutThePrefix(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{{Number: 17463, Title: "[scanner] mutation failing across org/repo", Open: true}}
	prs := []*sharedCIRerunPR{{
		Number: 1, SHA: "41dda120fa", Comments: []string{"deferring until that incident closes.\n\n<!-- hive-shared-ci-17463 -->"},
		Runs: []sharedCIRerunRun{{ID: 59, WorkflowID: 7, RunNumber: 59, Attempt: 1, Status: "completed", Conclusion: "cancelled"}},
	}}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	opts := SharedCIRerunOptions{Now: sharedCIRerunNow}

	res, err := c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep while open: %v", err)
	}
	_, posted, lists := fake.snap()
	if res.Incidents != 0 || len(res.Rerun) != 0 || len(posted[1]) != 0 || lists[1] != 1 || fake.issueGets != 1 {
		t.Fatalf("open incident: nothing re-run, one issue GET: result=%+v posted=%d lists=%d gets=%d", res, len(posted[1]), lists[1], fake.issueGets)
	}

	fake.mu.Lock()
	fake.issues[0].Open = false
	fake.issues[0].ClosedAt = now.Add(-30 * time.Minute)
	fake.mu.Unlock()
	res, err = c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep after close: %v", err)
	}
	_, posted, lists = fake.snap()
	fake.mu.Lock()
	full := append([]int64(nil), fake.fullReruns...)
	gets := fake.issueGets
	fake.mu.Unlock()
	if res.Incidents != 1 || !reflect.DeepEqual(full, []int64{59}) || len(posted[1]) != 1 || gets != 2 {
		t.Fatalf("closed incident lifts the deferral: result=%+v full=%v posted=%d gets=%d", res, full, len(posted[1]), gets)
	}
	if lists[1] != 2 {
		t.Fatalf("comments read once per decision, got %d", lists[1])
	}
	if !strings.Contains(posted[1][0], "<!-- hive-shared-ci-lifted-17463 -->") || !strings.Contains(posted[1][0], "Shared-CI incident #17463 closed") {
		t.Fatalf("lift comment: %q", posted[1][0])
	}

	res, err = c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	_, posted, _ = fake.snap()
	fake.mu.Lock()
	full = append([]int64(nil), fake.fullReruns...)
	fake.mu.Unlock()
	if len(res.Rerun) != 0 || len(full) != 1 || len(posted[1]) != 1 {
		t.Fatalf("third pass must lift nothing: result=%+v full=%v posted=%d", res, full, len(posted[1]))
	}
}

func TestNormalizeSharedCIIncidentIssue(t *testing.T) {
	cases := []struct {
		title      string
		labels     []string
		wantTitle  string
		wantLabels []string
	}{
		{"mutation failing across hivecommons/hive", []string{"help wanted"}, "[shared-ci] mutation failing across hivecommons/hive", []string{"help wanted", "shared-ci"}},
		{"[scanner] mutation failing across hivecommons/hive", nil, "[shared-ci] mutation failing across hivecommons/hive", []string{"shared-ci"}},
		{"[shared-ci] lint failing across org/repo", []string{"shared-ci"}, "[shared-ci] lint failing across org/repo", []string{"shared-ci"}},
		{"[Flaky] e2e failing across org/repo", nil, "[shared-ci] [Flaky] e2e failing across org/repo", []string{"shared-ci"}},
		{"Baseline broken on main", []string{"shared-ci"}, "[shared-ci] Baseline broken on main", []string{"shared-ci"}},
		{"[scanner] Review backlog (security): 3 PRs", []string{"origin/scanner"}, "[scanner] Review backlog (security): 3 PRs", []string{"origin/scanner"}},
		{"tests are failing across the board", nil, "tests are failing across the board", nil},
	}
	for _, tc := range cases {
		gotTitle, gotLabels := normalizeSharedCIIncidentIssue(tc.title, tc.labels)
		if gotTitle != tc.wantTitle || !reflect.DeepEqual(gotLabels, tc.wantLabels) {
			t.Errorf("normalize(%q, %v) = %q, %v; want %q, %v", tc.title, tc.labels, gotTitle, gotLabels, tc.wantTitle, tc.wantLabels)
		}
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

// hivecommons/hive#17500: a job cut off by its timeout-minutes is recorded
// as a cancelled run, not a failed one; hive#17471 sat "Deferred" after
// incident #17463 closed because the sweep saw nothing red. A cancelled
// newest run is red: the whole workflow is re-run once, the PR is told the
// deferral lifted, and the next pass does nothing.
func TestSweepSharedCIReruns_LiftsDeferralWhenTheRedRunWasCancelled(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{{Number: 17463, Title: "[shared-ci] mutation failing across org/repo", ClosedAt: now.Add(-time.Hour)}}
	prs := []*sharedCIRerunPR{{
		Number: 1, SHA: "41dda120fa", Comments: []string{"tracked in #17463; deferring until that incident closes.\n\n<!-- hive-shared-ci-17463 -->"},
		Runs: []sharedCIRerunRun{{ID: 59, WorkflowID: 7, RunNumber: 59, Attempt: 1, Status: "completed", Conclusion: "cancelled"}},
	}}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	opts := SharedCIRerunOptions{Now: sharedCIRerunNow}
	res, err := c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, posted, _ := fake.snap()
	fake.mu.Lock()
	full := append([]int64(nil), fake.fullReruns...)
	fake.mu.Unlock()
	if len(reruns) != 0 || !reflect.DeepEqual(full, []int64{59}) || len(res.Rerun) != 1 {
		t.Fatalf("a cancelled run is re-run whole: failed-jobs=%v full=%v result=%+v", reruns, full, res)
	}
	if len(posted[1]) != 1 {
		t.Fatalf("one lift comment, got %q", posted[1])
	}
	for _, want := range []string{"<!-- hive-shared-ci-lifted-17463 -->", "<!-- hive-shared-ci-rerun-17463 -->", "Shared-CI incident #17463 closed — re-running `CI`"} {
		if !strings.Contains(posted[1][0], want) {
			t.Fatalf("lift comment misses %q: %q", want, posted[1][0])
		}
	}

	res, err = c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	_, posted, _ = fake.snap()
	fake.mu.Lock()
	full = append([]int64(nil), fake.fullReruns...)
	fake.mu.Unlock()
	if len(full) != 1 || len(posted[1]) != 1 || len(res.Rerun) != 0 {
		t.Fatalf("second pass must lift nothing: full=%v posted=%d result=%+v", full, len(posted[1]), res)
	}

	// The memo is per process; a fresh client sees the lifted marker and stays quiet too.
	c2 := newTestClient(t, srv, "org", []string{"org/repo"})
	if res, err = c2.SweepSharedCIReruns(context.Background(), opts); err != nil || len(res.Rerun) != 0 {
		t.Fatalf("fresh client re-lifted: err=%v result=%+v", err, res)
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
		{"already lifted", []string{"<!-- hive-shared-ci-10 -->", "<!-- hive-shared-ci-lifted-10 -->"}, nil},
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
	if block, ok := c.SharedCIRerunBlocked("org/repo", 1, "aaaaaaaaaa"); !ok || block.RunID == 0 {
		t.Fatalf("permission refusal was not recorded: %+v %v", block, ok)
	}
	if _, ok := c.SharedCIRerunBlocked("org/repo", 1, "new-head"); ok {
		t.Fatal("old-head refusal applied to a new head")
	}
	if _, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow}); err != nil {
		t.Fatal(err)
	}
	reruns, _, _ = fake.snap()
	if len(reruns) != 1 {
		t.Fatalf("cooldown allowed another POST: %v", reruns)
	}

	fake.mu.Lock()
	fake.forbidRerun = false
	fake.mu.Unlock()
	c.sharedCIRerunMu.Lock()
	c.sharedCIRerunForbidden["org/repo"] = time.Now().Add(-sharedCIRerunForbiddenTTL - time.Second)
	c.sharedCIRerunMu.Unlock()
	res, err = c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow})
	if err != nil || len(res.Rerun) != 2 {
		t.Fatalf("unchanged PRs did not recover after permission change: %+v %v", res, err)
	}
	if _, ok := c.SharedCIRerunBlocked("org/repo", 1, "aaaaaaaaaa"); ok {
		t.Fatal("successful recovery retained permission block")
	}
}

func TestSharedCIRerunManualRecoveryClearsBlockDuringCooldown(t *testing.T) {
	now := sharedCIRerunNow()
	pr := &sharedCIRerunPR{Number: 1, SHA: "aaa", Comments: []string{"<!-- hive-shared-ci-800 -->"}, Runs: []sharedCIRerunRun{failedRun(11, 1)}}
	srv, fake := newSharedCIRerunFake(t, []sharedCIRerunIssue{{Number: 800, Title: "[shared-ci] incident", ClosedAt: now.Add(-time.Hour)}}, []*sharedCIRerunPR{pr})
	fake.forbidRerun = true
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	opts := SharedCIRerunOptions{Now: sharedCIRerunNow}
	if _, err := c.SweepSharedCIReruns(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	pr.Runs[0].Conclusion = "success"
	fake.mu.Unlock()
	opts.Now = func() time.Time { return now.Add(DefaultSharedCIRerunLookback + time.Hour) }
	if _, err := c.SweepSharedCIReruns(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.SharedCIRerunBlocked("org/repo", 1, "aaa"); ok {
		t.Fatal("manual recovery must clear the block without a PR edit or TTL expiry")
	}
	reruns, posted, _ := fake.snap()
	if len(reruns) != 1 || len(posted[1]) != 0 {
		t.Fatalf("manual recovery retried or stamped an automated success: %v %v", reruns, posted)
	}
}

func TestSharedCIRerunPartialPermissionFailureDoesNotFinishIncident(t *testing.T) {
	now := sharedCIRerunNow()
	pr := &sharedCIRerunPR{Number: 1, SHA: "aaa", Comments: []string{"<!-- hive-shared-ci-800 -->"}, Runs: []sharedCIRerunRun{failedRun(11, 1), failedRun(12, 2)}}
	srv, fake := newSharedCIRerunFake(t, []sharedCIRerunIssue{{Number: 800, Title: "[shared-ci] incident", ClosedAt: now.Add(-time.Hour)}}, []*sharedCIRerunPR{pr})
	fake.forbidAfter = 1
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	res, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow})
	if err != nil || len(res.Rerun) != 1 || len(res.Rerun[0].RunIDs) != 1 {
		t.Fatalf("partial recovery audit = %+v, %v", res, err)
	}
	_, posted, _ := fake.snap()
	if len(posted[1]) != 0 {
		t.Fatalf("partial recovery stamped incident complete: %v", posted)
	}
	if _, ok := c.SharedCIRerunBlocked("org/repo", 1, "aaa"); !ok {
		t.Fatal("partial recovery hid outstanding permission block")
	}
}
