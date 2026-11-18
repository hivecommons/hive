package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
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
	// Open serves the issue from the open listing (CreatedAt, Body) instead
	// of the closed one.
	Open      bool
	CreatedAt time.Time
	Body      string
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
	Base     string
	Author   string
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
	openLists    int
}

func newSharedCIRerunFake(t *testing.T, issues []sharedCIRerunIssue, prs []*sharedCIRerunPR) (*httptest.Server, *sharedCIRerunFake) {
	t.Helper()
	f := &sharedCIRerunFake{issues: issues, prs: map[int]*sharedCIRerunPR{}, posted: map[int][]string{}, commentLists: map[int]int{}}
	for _, pr := range prs {
		f.prs[pr.Number] = pr
	}
	commentsRE := regexp.MustCompile(`^/repos/org/repo/issues/(\d+)/comments$`)
	rerunRE := regexp.MustCompile(`^/repos/org/repo/actions/runs/(\d+)/rerun-failed-jobs$`)
	fullRerunRE := regexp.MustCompile(`^/repos/org/repo/actions/runs/(\d+)/rerun$`)
	updated := "2026-10-15T00:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/repos/org/repo/issues" && r.Method == http.MethodGet:
			if r.URL.Query().Get("state") == "open" {
				f.openLists++
				out := []map[string]any{}
				for _, is := range f.issues {
					if is.Open {
						out = append(out, map[string]any{"number": is.Number, "title": is.Title, "state": "open", "created_at": is.CreatedAt.Format(time.RFC3339), "body": is.Body})
					}
				}
				_ = json.NewEncoder(w).Encode(out)
				return
			}
			if r.URL.Query().Get("state") != "closed" || r.URL.Query().Get("since") == "" {
				t.Errorf("issue list query = %q, want state=closed with since", r.URL.RawQuery)
			}
			out := []map[string]any{}
			for _, is := range f.issues {
				if is.Open {
					continue
				}
				item := map[string]any{"number": is.Number, "title": is.Title, "state": "closed", "closed_at": is.ClosedAt.Format(time.RFC3339), "body": is.Body}
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
				base := pr.Base
				if base == "" {
					base = "main"
				}
				author := pr.Author
				if author == "" {
					author = "contributor"
				}
				out = append(out, map[string]any{"number": pr.Number, "state": "open", "updated_at": updated, "labels": labels, "head": map[string]any{"sha": pr.SHA}, "base": map[string]any{"ref": base}, "user": map[string]any{"login": author}})
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

// openOutageIncident is projectbluefin/utah#1336 in miniature: the scanner's
// open tracker for `contract`, filed while codeberg.org did not resolve,
// listing the PRs it found red.
func openOutageIncident(now time.Time, body string) sharedCIRerunIssue {
	return sharedCIRerunIssue{Number: 1336, Title: "[shared-ci] contract failing across org/repo", Open: true, CreatedAt: now.Add(-6 * time.Hour), Body: body}
}

// With the tracker still open, once `contract` has completed green on the
// base tip after the tracker was filed, the PRs it lists (no marker needed:
// a Renovate PR and a 072 PR included) and the PRs deferred to it get their
// failed jobs re-run once each. An unlisted, unmarked PR is left alone, and
// the tracker is never closed.
func TestSweepSharedCIReruns_OpenIncidentRecoveredOnBase(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{openOutageIncident(now, "**Summary:** codeberg.org does not resolve.\n\nAffected PRs: #2, #3, #9.\n")}
	prs := []*sharedCIRerunPR{
		{Number: 1, SHA: "aaaaaaaaaa", Comments: []string{"deferring\n<!-- hive-shared-ci-1336 -->"}, Runs: []sharedCIRerunRun{failedRun(11, 1)}},
		{Number: 2, SHA: "bbbbbbbbbb", Author: "renovate[bot]", Runs: []sharedCIRerunRun{failedRun(21, 1)}},
		{Number: 3, SHA: "cccccccccc", Labels: []string{"stage/072-returned-to-contributor"}, Runs: []sharedCIRerunRun{failedRun(31, 1)}},
		{Number: 4, SHA: "dddddddddd", Runs: []sharedCIRerunRun{failedRun(41, 1)}},
	}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	c.baseRed.now = sharedCIRerunNow
	c.baseRed.noteGreen(baseRedMemoryKey("org", "repo", "main"), map[string]time.Time{"contract": now.Add(-time.Hour)})

	opts := SharedCIRerunOptions{Now: sharedCIRerunNow}
	res, err := c.SweepSharedCIReruns(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, posted, _ := fake.snap()
	if !reflect.DeepEqual(reruns, []int64{11, 21, 31}) {
		t.Fatalf("reruns = %v, want PRs 1 (marker), 2 (listed, Renovate) and 3 (listed, 072)", reruns)
	}
	if res.Incidents != 1 || len(res.Rerun) != 3 {
		t.Fatalf("result = %+v", res)
	}
	for _, n := range []int{1, 2, 3} {
		if len(posted[n]) != 1 || !strings.Contains(posted[n][0], "<!-- hive-shared-ci-rerun-1336 -->") || !strings.Contains(posted[n][0], "still open; `contract` has passed on the base branch") {
			t.Fatalf("PR %d rerun comment = %q", n, posted[n])
		}
	}
	if len(posted[4]) != 0 {
		t.Fatalf("unlisted, unmarked PR 4 got %q", posted[4])
	}

	// Once per PR per incident: the next pass, with the checks still red
	// for a reason of their own, re-runs nothing.
	if res, err = c.SweepSharedCIReruns(context.Background(), opts); err != nil || len(res.Rerun) != 0 {
		t.Fatalf("second pass: result=%+v err=%v", res, err)
	}
	if reruns, _, _ = fake.snap(); len(reruns) != 3 {
		t.Fatalf("second pass re-ran again: %v", reruns)
	}
}

// No recovery, no rerun: the base tip's check has not passed since the
// tracker was filed (its last green run predates the outage), or it passed
// only on another base; with no green recorded at all the sweep does not
// even list the open incidents.
func TestSweepSharedCIReruns_OpenIncidentNotRecovered(t *testing.T) {
	now := sharedCIRerunNow()
	body := "Affected PRs: #1, #2.\n"
	for _, tc := range []struct {
		name      string
		greenKey  string
		greenAt   time.Time
		base      string
		wantLists int
	}{
		{name: "no green recorded", wantLists: 0},
		{name: "green before the tracker was filed", greenKey: "main", greenAt: now.Add(-8 * time.Hour), wantLists: 1},
		{name: "green on another base", greenKey: "main", greenAt: now.Add(-time.Hour), base: "v5", wantLists: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prs := []*sharedCIRerunPR{
				{Number: 1, SHA: "aaaaaaaaaa", Base: tc.base, Comments: []string{"<!-- hive-shared-ci-1336 -->"}, Runs: []sharedCIRerunRun{failedRun(11, 1)}},
				{Number: 2, SHA: "bbbbbbbbbb", Base: tc.base, Runs: []sharedCIRerunRun{failedRun(21, 1)}},
			}
			srv, fake := newSharedCIRerunFake(t, []sharedCIRerunIssue{openOutageIncident(now, body)}, prs)
			c := newTestClient(t, srv, "org", []string{"org/repo"})
			c.baseRed.now = sharedCIRerunNow
			if tc.greenKey != "" {
				c.baseRed.noteGreen(baseRedMemoryKey("org", "repo", tc.greenKey), map[string]time.Time{"contract": tc.greenAt})
			}
			if _, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow}); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			reruns, posted, _ := fake.snap()
			if len(reruns) != 0 || len(posted) != 0 {
				t.Fatalf("reruns=%v posted=%v, want nothing before the base recovers", reruns, posted)
			}
			fake.mu.Lock()
			lists := fake.openLists
			fake.mu.Unlock()
			if lists != tc.wantLists {
				t.Fatalf("open incident listings = %d, want %d", lists, tc.wantLists)
			}
		})
	}
}

// A closed incident's tracker list counts too: a listed PR no fix lane ever
// stamped is re-run after the close, like a stamped one.
func TestSweepSharedCIReruns_ClosedIncidentRerunsListedPRs(t *testing.T) {
	now := sharedCIRerunNow()
	issues := []sharedCIRerunIssue{{Number: 500, Title: "[shared-ci] lint failing across org/repo", ClosedAt: now.Add(-time.Hour),
		Body: RenderSharedCIIssueBody(SharedCICluster{Repo: "org/repo", Check: "lint", PRs: []int{1}})}}
	prs := []*sharedCIRerunPR{
		{Number: 1, SHA: "aaaaaaaaaa", Author: "dependabot[bot]", Runs: []sharedCIRerunRun{failedRun(11, 1)}},
		{Number: 2, SHA: "bbbbbbbbbb", Runs: []sharedCIRerunRun{failedRun(21, 1)}},
	}
	srv, fake := newSharedCIRerunFake(t, issues, prs)
	c := newTestClient(t, srv, "org", []string{"org/repo"})
	if _, err := c.SweepSharedCIReruns(context.Background(), SharedCIRerunOptions{Now: sharedCIRerunNow}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	reruns, posted, _ := fake.snap()
	if !reflect.DeepEqual(reruns, []int64{11}) || len(posted[1]) != 1 || !strings.Contains(posted[1][0], "Shared-CI incident #500 closed") {
		t.Fatalf("reruns=%v posted=%v, want listed PR 1 only", reruns, posted)
	}
}

func TestSharedCITrackerListedPRs(t *testing.T) {
	hive := RenderSharedCIIssueBody(SharedCICluster{Repo: "o/r", Check: "contract", PRs: []int{1274, 1295}})
	scanner := "Check: `contract` on projectbluefin/utah.\n\nAffected PRs: #1274, #1295, #1296.\n\nRelated: open PR #1191 (retry transient Codeberg submodule fetch failures)."
	bold := "**Affected PRs:** #7, #8"
	for _, tc := range []struct {
		name, body string
		want       []int
	}{
		{name: "hive tracker", body: hive, want: []int{1274, 1295}},
		{name: "scanner tracker", body: scanner, want: []int{1274, 1295, 1296}},
		{name: "bold label", body: bold, want: []int{7, 8}},
		{name: "no list", body: "Related: open PR #1191", want: nil},
	} {
		got := sharedCITrackerListedPRs(tc.body)
		var nums []int
		for n := range got {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		if !reflect.DeepEqual(nums, tc.want) {
			t.Errorf("%s: listed = %v, want %v", tc.name, nums, tc.want)
		}
	}
}

func TestPendingSharedCIRerunsListed(t *testing.T) {
	over := map[int]bool{1336: true}
	if got := pendingSharedCIReruns(nil, over, 1336); !reflect.DeepEqual(got, []int{1336}) {
		t.Fatalf("listed, unmarked = %v", got)
	}
	if got := pendingSharedCIReruns([]string{"<!-- hive-shared-ci-rerun-1336 -->"}, over, 1336); got != nil {
		t.Fatalf("listed, already re-run = %v", got)
	}
	if got := pendingSharedCIReruns([]string{"<!-- hive-shared-ci-900 -->"}, over, 1336); got != nil {
		t.Fatalf("listed but deferred to another incident still open = %v", got)
	}
	if got := pendingSharedCIReruns(nil, map[int]bool{}, 1336); got != nil {
		t.Fatalf("listed by an incident that is not over = %v", got)
	}
}
