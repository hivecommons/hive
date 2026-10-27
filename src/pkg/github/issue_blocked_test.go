package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestBlockedOnMarkerRoundTrip(t *testing.T) {
	m := BlockedOnMarker("hivecommons/hive", 13092)
	if m != "<!-- hive:blocked-on pr=hivecommons/hive#13092 -->" {
		t.Fatalf("marker = %q", m)
	}
	repo, pr, ok := ParseBlockedOnMarker("intro\n" + m + "\nmore")
	if !ok || repo != "hivecommons/hive" || pr != 13092 {
		t.Fatalf("parse = %q %d %v", repo, pr, ok)
	}
	if _, _, ok := ParseBlockedOnMarker("<!-- hive:claim who=x -->"); ok {
		t.Fatal("claim marker parsed as blocked-on")
	}
	if got := blockedFilterReason("body " + m); got != "blocked on hivecommons/hive#13092" {
		t.Fatalf("filter reason = %q", got)
	}
	if got := blockedFilterReason("no marker"); got != "blocked" {
		t.Fatalf("bare filter reason = %q", got)
	}
	r, n, ok := parseBlockedOnReason("blocked on hivecommons/hive#13092")
	if !ok || r != "hivecommons/hive" || n != 13092 {
		t.Fatalf("parseBlockedOnReason = %q %d %v", r, n, ok)
	}
	if _, _, ok := parseBlockedOnReason("blocked"); ok {
		t.Fatal("bare reason parsed a PR")
	}
	if !IsBlockedIssueReason("blocked on o/r#1") || !IsBlockedIssueReason("blocked") || IsBlockedIssueReason("hold label") {
		t.Fatal("IsBlockedIssueReason")
	}
}

// hivecommons/hive#13093: a blocked issue derives no stage, whatever its
// bucket, and the legacy deriver and the engine adapter agree (the golden
// corpus carries the blocked label sets; this pins the live-shape input).
func TestIssueStageForBlockedDerivesNone(t *testing.T) {
	in := IssueStageInput{
		Repo:   "hivecommons/hive",
		Number: 13093,
		Labels: []string{"help wanted", "stage/010-actionable", "blocked"},
		Bucket: workBucketFiltered,
		Reason: "blocked on hivecommons/hive#13092",
	}
	stage, covered, reason := IssueStageFor(in, nil)
	if stage != stagelabels.None || covered || reason != "blocked on hivecommons/hive#13092" {
		t.Fatalf("IssueStageFor = %v %v %q", stage, covered, reason)
	}
	in.Bucket, in.Reason, in.ClaimKind = workBucketClaimed, "", "contributor"
	stage, _, reason = IssueStageFor(in, nil)
	if stage != stagelabels.None || reason != "blocked" {
		t.Fatalf("claimed+blocked = %v %q, want none/blocked", stage, reason)
	}
}

func TestIssueClaimBlockReasonRefusesBlockedIssue(t *testing.T) {
	body := "finding\n" + BlockedOnMarker("hivecommons/hive", 13092)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/repos/o/r/issues/7" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "state": "open", "body": body,
				"labels": []map[string]string{{"name": "blocked"}, {"name": "help wanted"}},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	reason, err := c.IssueClaimBlockReason(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "blocked: hivecommons/hive#13092" {
		t.Fatalf("reason = %q", reason)
	}
}

func TestDefangMentions(t *testing.T) {
	cases := map[string]string{
		"@arteeh please pick this up":        "arteeh please pick this up",
		"Blocked on #13092 (cc @arteeh).":    "Blocked on #13092 (cc arteeh).",
		"mail me@example.com or `@keep`":     "mail me@example.com or `@keep`",
		"path /@scope/pkg and owner/@x stay": "path /@scope/pkg and owner/@x stay",
	}
	for in, want := range cases {
		if got := defangMentions(in); got != want {
			t.Errorf("defangMentions(%q) = %q, want %q", in, got, want)
		}
	}
	got := blockedNoteBody("o/r", 5, "Blocked on #5 @someone")
	if !strings.HasPrefix(got, BlockedOnMarker("o/r", 5)+"\n") || strings.Contains(got, "@someone") {
		t.Fatalf("blockedNoteBody = %q", got)
	}
	again := blockedNoteBody("o/r", 5, got)
	if strings.Count(again, "hive:blocked-on") != 1 {
		t.Fatalf("marker duplicated: %q", again)
	}
}

func TestBlockedBacklogLabels(t *testing.T) {
	got := blockedBacklogLabels([]string{"nit", "Help Wanted", "agent/reviewer"})
	if strings.Join(got, ",") != "nit,agent/reviewer,blocked" {
		t.Fatalf("labels = %v", got)
	}
	got = blockedBacklogLabels([]string{"blocked"})
	if strings.Join(got, ",") != "blocked" {
		t.Fatalf("labels = %v (blocked duplicated?)", got)
	}
}

// hivecommons/hive#13093: a finding whose file the PR introduces (absent on
// the base branch) is filed blocked — marker in the body, `blocked` label,
// no `help wanted` — as is a finding on an existing file changed by the PR.
func TestReviewBacklogFilesIntroducedAndTouchedItemsBlockedOnItsPR(t *testing.T) {
	withReviewBacklogState(t)
	var issues []struct {
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"open","base":{"ref":"v5"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/5/files":
			_, _ = io.WriteString(w, `[{"filename":"pkg/old.go","status":"modified"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/contents/pkg/new.go":
			if r.URL.Query().Get("ref") != "v5" {
				t.Errorf("contents probe ref = %q, want v5", r.URL.Query().Get("ref"))
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/contents/pkg/old.go":
			_, _ = io.WriteString(w, `{"type":"file","name":"old.go","path":"pkg/old.go"}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/labels":
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues":
			var payload struct {
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			issues = append(issues, payload)
			n := 100 + len(issues)
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.test/o/r/issues/%d"}`, n, n)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/5/comments":
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue, Labels: []string{"nit", "help wanted"}},
		}
	})
	recs := captureAudit(c)
	report := routingReport(t, 5, "abc",
		nit("only-in-pr", "low", "pkg/new.go", 3, "introduced by the PR"),
		nit("on-base", "low", "pkg/old.go", 4, "already there"))
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues = %d, want 2", len(issues))
	}
	// Introduced and touched files are parked on the reviewed PR; the note's
	// reason distinguishes a missing base file from a changed existing file.
	var missing, onBase int
	for _, is := range issues {
		if _, pr, ok := ParseBlockedOnMarker(is.Body); !ok || pr != 5 {
			t.Errorf("body lacks the blocked-on marker for PR 5: %q", is.Body)
		}
		if !strings.Contains(is.Body, "Blocked on PR #5") {
			t.Errorf("body = %q", is.Body)
		}
		if strings.Join(is.Labels, ",") != "nit,blocked,agent/reviewer" {
			t.Errorf("labels = %v, want nit,blocked,agent/reviewer (no help wanted)", is.Labels)
		}
		if strings.Contains(is.Body, "pkg/new.go") {
			missing++
			if !strings.Contains(is.Body, "does not exist on `v5`") {
				t.Errorf("file-missing body = %q", is.Body)
			}
		} else {
			onBase++
			if !strings.Contains(is.Body, "about code that PR is changing") {
				t.Errorf("on-base body = %q", is.Body)
			}
		}
	}
	if missing != 1 || onBase != 1 {
		t.Fatalf("missing=%d onBase=%d", missing, onBase)
	}
	var sawBlocked bool
	for _, r := range *recs {
		if r.Action == AuditActionReviewBacklogIssueFiled && strings.Contains(r.Detail, "blocked=true") {
			sawBlocked = true
		}
	}
	if !sawBlocked {
		t.Fatal("no review_backlog_issue_filed audit with blocked=true")
	}
}

// The sweep lifts `blocked` once the tracked PR is merged (one note, marker
// included), strips `help wanted` while it is open, and leaves issues whose
// PR cannot be read alone.
func TestReconcileBlockedIssues(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/1":
			_, _ = io.WriteString(w, `{"number":1,"state":"closed","merged":true,"base":{"ref":"v5"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/2":
			_, _ = io.WriteString(w, `{"number":2,"state":"open","base":{"ref":"v5"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/3":
			_, _ = io.WriteString(w, `{"number":3,"state":"closed","merged":false}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/30/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"triage"},{"id":2,"body":"`+BlockedOnMarker("o/r", 3)+`\nBlocked on #3"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/60/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"blocked: awaiting the 24-hour checks"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/60/timeline":
			_, _ = io.WriteString(w, `[{"event":"labeled","created_at":"2026-10-20T10:00:00Z","label":{"name":"blocked"},"actor":{"login":"maintainer","type":"User"}}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/70/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"`+reviewBacklogLinkMarker+` also raised on PR #9"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/70/timeline":
			_, _ = io.WriteString(w, `[{"event":"labeled","created_at":"2026-10-20T10:00:00Z","label":{"name":"help wanted"},"actor":{"login":"maintainer","type":"User"}},{"event":"labeled","created_at":"2026-10-20T10:05:00Z","label":{"name":"blocked"},"actor":{"login":"`+testBot+`","type":"Bot"}}]`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = testBot
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{
		"o/r": {Issues: []RepoWorkIssue{
			{Repo: "o/r", Number: 10, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#1"},
			{Repo: "o/r", Number: 20, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#2"},
			{Repo: "o/r", Number: 30, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked"},
			{Repo: "o/r", Number: 40, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#404"},
			{Repo: "o/r", Number: 50, Labels: []string{"hold"}, Bucket: workBucketFiltered, Reason: "hold label"},
			// A relay's blocked verdict, no tracked PR (hivecommons/hive#13267):
			// nothing to lift, but `help wanted` still comes off.
			{Repo: "o/r", Number: 60, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked"},
			// A hive-applied `blocked` with no marker anywhere (a blocked
			// filing folded into an open twin, hivecommons/hive#13364) is a
			// defect: lifted.
			{Repo: "o/r", Number: 70, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked"},
		}},
	}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != 6 || res.Unblocked != 3 || res.Stripped != 2 || res.Errors != 1 {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"DELETE /repos/o/r/issues/10/labels/blocked",
		"POST /repos/o/r/issues/10/comments",
		"DELETE /repos/o/r/issues/20/labels/help wanted",
		"GET /repos/o/r/issues/30/comments",
		"DELETE /repos/o/r/issues/30/labels/blocked",
		"GET /repos/o/r/issues/60/comments",
		"GET /repos/o/r/issues/60/timeline",
		"DELETE /repos/o/r/issues/60/labels/help wanted",
		"GET /repos/o/r/issues/70/comments",
		"GET /repos/o/r/issues/70/timeline",
		"DELETE /repos/o/r/issues/70/labels/blocked",
		"POST /repos/o/r/issues/70/comments",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}
	for _, never := range []string{
		"DELETE /repos/o/r/issues/20/labels/blocked",
		"DELETE /repos/o/r/issues/40/",
		"/repos/o/r/issues/50",
		"DELETE /repos/o/r/issues/60/labels/blocked",
		"GET /repos/o/r/issues/30/timeline",
	} {
		if strings.Contains(joined, never) {
			t.Errorf("unexpected call %q in:\n%s", never, joined)
		}
	}
}

// The watcher's comment kind with blocked_on_pr posts the marker, strips
// mentions, adds `blocked` and removes `help wanted`.
func TestIssueRequestWatcherBlockedOnPRComment(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var commentBody string
	var added []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12":
			_, _ = io.WriteString(w, `{"number":12,"state":"open","labels":[{"name":"help wanted"}]}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"open"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/comments":
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			commentBody = payload.Body
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/labels":
			var payload []string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			added = append(added, payload...)
			mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/12/labels/"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = io.WriteString(w, `{"name":"blocked"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)
	reqPath, err := WriteIssueRequest(dir, IssueRequest{
		Kind: "comment", Repo: "o/r", Number: 12, Agent: "scanner", BlockedOnPR: 5,
		Body: "Blocked on #5 (not starting). @arteeh could you finish it?",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessIssueRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Fatalf("request not consumed (stat err=%v); calls=%v", err, calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(commentBody, "@arteeh") || !strings.Contains(commentBody, "arteeh could you") {
		t.Fatalf("comment body = %q", commentBody)
	}
	if _, pr, ok := ParseBlockedOnMarker(commentBody); !ok || pr != 5 {
		t.Fatalf("comment lacks marker: %q", commentBody)
	}
	if !issueHasLabel(added, BlockedLabel) {
		t.Fatalf("labels added = %v, want blocked", added)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "DELETE /repos/o/r/issues/12/labels/help wanted") {
		t.Fatalf("help wanted not removed: %v", calls)
	}
}

// hivecommons/hive#13310: a blocked issue whose body carries a recheck-after
// deadline (and no blocked-on marker) is filed "blocked until <RFC3339>" with
// the deadline on the row, and IssueStageFor echoes that reason.
func TestBlockedFilterReasonRecheckAfter(t *testing.T) {
	at := time.Date(2026, 10, 21, 21, 20, 0, 0, time.UTC)
	body := "soak checklist\n" + gfi.RecheckAfterMarker(at)
	if got := blockedFilterReason(body); got != "blocked until 2026-10-21T21:20:00Z" {
		t.Fatalf("filter reason = %q", got)
	}
	if got := blockedFilterReason(body + "\n" + BlockedOnMarker("o/r", 7)); got != "blocked on o/r#7" {
		t.Fatalf("blocked-on marker must win: %q", got)
	}
	if got, ops := blockedRecheck(body); !got.Equal(at) || ops {
		t.Fatalf("blockedRecheck = %v %v", got, ops)
	}
	if got, ops := blockedRecheck(body + "\n" + gfi.OpsTrackingMarker); !got.Equal(at) || !ops {
		t.Fatalf("blockedRecheck ops = %v %v", got, ops)
	}
	if got, ops := blockedRecheck("no markers"); !got.IsZero() || ops {
		t.Fatalf("blockedRecheck bare = %v %v", got, ops)
	}
	if !IsBlockedIssueReason("blocked until 2026-10-21T21:20:00Z") {
		t.Fatal("deadline reason not recognised as blocked")
	}
	if _, _, ok := parseBlockedOnReason("blocked until 2026-10-21T21:20:00Z"); ok {
		t.Fatal("deadline reason parsed as a tracked PR")
	}
	stage, covered, reason := IssueStageFor(IssueStageInput{
		Repo: "o/r", Number: 13267, Labels: []string{"blocked"},
		Bucket: workBucketFiltered, Reason: "blocked until 2026-10-21T21:20:00Z",
	}, nil)
	if stage != stagelabels.None || covered || reason != "blocked until 2026-10-21T21:20:00Z" {
		t.Fatalf("IssueStageFor = %v %v %q", stage, covered, reason)
	}
}

func TestEnumerateActionableRecordsBlockedRecheckAfter(t *testing.T) {
	const org, repo = "testorg", "testrepo"
	at := time.Date(2026, 10, 21, 21, 20, 0, 0, time.UTC)
	issues := []wireIssue{
		{Number: 1, Title: "soak", User: wireUser{"alice"}, Labels: []wireLabel{{Name: "blocked"}}, CreatedAt: hoursAgo(3),
			Body: "checklist\n" + gfi.OpsTrackingMarker + "\n" + gfi.RecheckAfterMarker(at)},
		{Number: 2, Title: "bare", User: wireUser{"bob"}, Labels: []wireLabel{{Name: "blocked"}}, CreatedAt: hoursAgo(2)},
	}
	server := httptest.NewServer(buildMux(t, org, repo, issues, []wirePR{}))
	t.Cleanup(server.Close)
	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	rows := map[int]RepoWorkIssue{}
	for _, row := range result.WorkDetailsByRepo[repo].Issues {
		rows[row.Number] = row
	}
	if r := rows[1]; r.Bucket != workBucketFiltered || r.Reason != "blocked until 2026-10-21T21:20:00Z" || !r.RecheckAfter.Equal(at) || !r.OpsTracking {
		t.Fatalf("deadline row = %+v", r)
	}
	if r := rows[2]; r.Bucket != workBucketFiltered || r.Reason != "blocked" || !r.RecheckAfter.IsZero() || r.OpsTracking {
		t.Fatalf("bare row = %+v", r)
	}
}

// The sweep lifts `blocked` from a row whose recheck-after deadline has
// passed (no tracked PR, no ops-tracking marker) with one unblocked note,
// leaves rows whose deadline is ahead, ops-tracking rows and rows that
// already carry the note for this deadline alone, and never posts twice.
func TestReconcileBlockedIssuesRecheckAfter(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	prev := blockedSweepNow
	blockedSweepNow = func() time.Time { return now }
	t.Cleanup(func() { blockedSweepNow = prev })
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	pastReason := "blocked until " + past.Format(time.RFC3339)

	var mu sync.Mutex
	var calls []string
	comments := map[string][]string{
		"/repos/o/r/issues/63/comments": {blockedDeadlineNote(past.Format(time.RFC3339))},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			out := []map[string]any{}
			for i, b := range comments[r.URL.Path] {
				out = append(out, map[string]any{"id": i + 1, "body": b})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			comments[r.URL.Path] = append(comments[r.URL.Path], payload.Body)
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{
		"o/r": {Issues: []RepoWorkIssue{
			{Repo: "o/r", Number: 60, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: pastReason, RecheckAfter: past},
			{Repo: "o/r", Number: 61, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked until " + future.Format(time.RFC3339), RecheckAfter: future},
			{Repo: "o/r", Number: 62, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: pastReason, RecheckAfter: past, OpsTracking: true},
			{Repo: "o/r", Number: 63, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: pastReason, RecheckAfter: past},
		}},
	}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != 4 || res.Unblocked != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v", res)
	}
	// A second cycle on the same snapshot (the label lift not yet seen by
	// the enumeration) finds the note and posts nothing more.
	res = ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Unblocked != 0 || res.Errors != 0 {
		t.Fatalf("second pass result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, "\n")
	if n := strings.Count(joined, "DELETE /repos/o/r/issues/60/labels/blocked"); n != 1 {
		t.Errorf("issue 60 unblocked %d times, want 1:\n%s", n, joined)
	}
	if n := strings.Count(joined, "POST /repos/o/r/issues/60/comments"); n != 1 {
		t.Errorf("issue 60 noted %d times, want 1:\n%s", n, joined)
	}
	note := comments["/repos/o/r/issues/60/comments"]
	if len(note) != 1 || !strings.HasPrefix(note[0], blockedUnblockedMarker+"\n") || !strings.Contains(note[0], "deadline "+past.Format(time.RFC3339)+" passed; re-triaging") {
		t.Errorf("issue 60 note = %q", note)
	}
	for _, n := range []int{61, 62, 63} {
		for _, never := range []string{fmt.Sprintf("DELETE /repos/o/r/issues/%d/", n), fmt.Sprintf("POST /repos/o/r/issues/%d/", n)} {
			if strings.Contains(joined, never) {
				t.Errorf("unexpected call %q in:\n%s", never, joined)
			}
		}
	}
	if got := len(comments["/repos/o/r/issues/63/comments"]); got != 1 {
		t.Errorf("issue 63 comments = %d, want the existing note only", got)
	}
}
func TestParseSupersededBy(t *testing.T) {
	tests := []struct {
		text string
		want int
		ok   bool
	}{
		{
			text: "<!-- hive:adopted by=hosted-available-oke-11-placeholder-r05x reason=contributor-offline mode=supersede superseded_by=13989 head=b94e9f0e1cc59cc12c71932db39d3824881b129e -->",
			want: 13989,
			ok:   true,
		},
		{
			text: "Superseded by #13989.",
			want: 13989,
			ok:   true,
		},
		{
			text: "Superseded by #90: re-authored by the hive App so the required pull_request checks can run.",
			want: 90,
			ok:   true,
		},
		{
			text: "Superseded by 14001",
			want: 14001,
			ok:   true,
		},
		{
			text: "<!-- hive:adopted by=x reason=y mode=in-place head=z -->",
			want: 0,
			ok:   false,
		},
		{
			text: "plain comment about #123",
			want: 0,
			ok:   false,
		},
	}
	for _, tt := range tests {
		got, ok := ParseSupersededBy(tt.text)
		if ok != tt.ok || got != tt.want {
			t.Errorf("ParseSupersededBy(%q) = (%d, %v), want (%d, %v)", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

// Issue 14194: when writing hive:blocked-on pr=X, if X is already merged or closed,
// the relation is NOT written, blocked label is NOT added, and notice is posted.
func TestIssueRequestWatcherBlockedOnPR_TargetAlreadySettled(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prState   string
		merged    bool
		wantState string
	}{
		{name: "merged PR", prState: "closed", merged: true, wantState: "merged"},
		{name: "closed unmerged PR", prState: "closed", merged: false, wantState: "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var commentBody string
			var labelsAdded []string
			var labelsRemoved []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/10":
					data, _ := json.Marshal(map[string]any{"number": 10, "state": tc.prState, "merged": tc.merged})
					_, _ = w.Write(data)
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12":
					_, _ = io.WriteString(w, `{"number":12,"state":"open","labels":[{"name":"help wanted"}]}`)
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/12/comments":
					_, _ = io.WriteString(w, `[]`)
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/comments":
					var payload struct {
						Body string `json:"body"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					mu.Lock()
					commentBody = payload.Body
					mu.Unlock()
					_, _ = io.WriteString(w, `{"id":9}`)
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/12/labels":
					var payload []string
					_ = json.NewDecoder(r.Body).Decode(&payload)
					mu.Lock()
					labelsAdded = append(labelsAdded, payload...)
					mu.Unlock()
					_, _ = io.WriteString(w, `[]`)
				case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/12/labels/"):
					mu.Lock()
					labelsRemoved = append(labelsRemoved, r.URL.Path)
					mu.Unlock()
					_, _ = io.WriteString(w, `[]`)
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
				}
			}))
			defer srv.Close()
			c := issueTestClient(t, srv.URL)
			dir := withIssueDir(t)
			reqPath, err := WriteIssueRequest(dir, IssueRequest{
				Kind: "comment", Repo: "o/r", Number: 12, Agent: "scanner", BlockedOnPR: 10,
				Body: "Parking until #10 lands.",
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessIssueRequestsOnce(context.Background())
			if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
				t.Fatalf("request not consumed")
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(commentBody, "hive:blocked-on") {
				t.Fatalf("comment must NOT contain blocked-on marker: %q", commentBody)
			}
			if !strings.Contains(commentBody, "Target PR o/r#10 is already "+tc.wantState+" — not parking as blocked.") {
				t.Fatalf("comment lacks notice: %q", commentBody)
			}
			if !strings.Contains(commentBody, "Verify this finding on current base branch; close this issue without a PR if it is already fixed") {
				t.Fatalf("comment lacks verify instruction: %q", commentBody)
			}
			if issueHasLabel(labelsAdded, BlockedLabel) {
				t.Fatalf("blocked label must not be added when target is settled: %v", labelsAdded)
			}
			if len(labelsRemoved) != 0 {
				t.Fatalf("help wanted must not be removed: %v", labelsRemoved)
			}
		})
	}
}

// Issue 14194: when blocker PR X is superseded by PR Y:
// - If Y is merged, unblock immediately naming Y (superseding X).
// - If Y is open, re-target the relation to Y.
func TestReconcileBlockedIssuesSuperseded(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var commentBodies = map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		// Blocker PR 100 closed unmerged, superseded by #101
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/100":
			_, _ = io.WriteString(w, `{"number":100,"state":"closed","merged":false}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/100/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"Superseded by #101."}]`)
		// Superseder PR 101 is still open
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/101":
			_, _ = io.WriteString(w, `{"number":101,"state":"open"}`)

		// Blocker PR 200 closed unmerged, superseded by #201
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/200":
			_, _ = io.WriteString(w, `{"number":200,"state":"closed","merged":false}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/200/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"<!-- hive:adopted by=x reason=offline mode=supersede superseded_by=201 head=abc -->"}]`)
		// Superseder PR 201 is closed and merged
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/201":
			_, _ = io.WriteString(w, `{"number":201,"state":"closed","merged":true}`)

		// Blocker PR 300 MERGED, but a comment still says "Superseded by #301"
		// (open). Merged means landed: the supersede text must be ignored.
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/300":
			_, _ = io.WriteString(w, `{"number":300,"state":"closed","merged":true}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/300/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"Superseded by #301."}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/301":
			_, _ = io.WriteString(w, `{"number":301,"state":"open"}`)

		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/1/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/2/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/3/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			mu.Lock()
			commentBodies[r.URL.Path] = append(commentBodies[r.URL.Path], payload.Body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = testBot
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{
		"o/r": {Issues: []RepoWorkIssue{
			// Issue 1: blocked on #100. #100 superseded by #101 (open) -> re-target to #101!
			{Repo: "o/r", Number: 1, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#100"},
			// Issue 2: blocked on #200. #200 superseded by #201 (merged) -> unblock!
			{Repo: "o/r", Number: 2, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#200"},
			// Issue 3: blocked on #300. #300 merged (supersede text ignored) -> unblock, not re-target!
			{Repo: "o/r", Number: 3, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#300"},
		}},
	}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != 3 || res.Retargeted != 1 || res.Unblocked != 2 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Items=3 Retargeted=1 Unblocked=2 Errors=0", res)
	}

	mu.Lock()
	defer mu.Unlock()
	// Issue 1 should have received a re-target comment pointing to #101
	c1 := commentBodies["/repos/o/r/issues/1/comments"]
	if len(c1) != 1 || !strings.Contains(c1[0], BlockedOnMarker("o/r", 101)) || !strings.Contains(c1[0], "Re-targeted: blocker #100 was superseded by #101.") {
		t.Fatalf("issue 1 comment = %v", c1)
	}

	// Issue 2 should have been unblocked naming #201 (superseding #200)
	c2 := commentBodies["/repos/o/r/issues/2/comments"]
	if len(c2) != 1 || !strings.Contains(c2[0], blockedUnblockedMarker) || !strings.Contains(c2[0], "o/r#201 (superseding #200) is no longer open") {
		t.Fatalf("issue 2 comment = %v", c2)
	}

	// Issue 3's blocker merged: unblocked, and never re-targeted to #301.
	c3 := commentBodies["/repos/o/r/issues/3/comments"]
	if len(c3) != 1 || !strings.Contains(c3[0], blockedUnblockedMarker) || strings.Contains(c3[0], BlockedOnMarker("o/r", 301)) {
		t.Fatalf("issue 3 comment = %v", c3)
	}
	for _, call := range calls {
		if call == "GET /repos/o/r/pulls/301" {
			t.Fatalf("merged blocker #300 must not consult superseder #301; calls = %v", calls)
		}
	}
}

// A re-target leaves the body naming the superseded blocker X and records Y
// only in a comment. On a later pass whose comment budget ran out before the
// issue, the sweep sees X from the body, re-detects the supersede, and must
// read the comments before posting: the newest marker already names Y, so
// nothing is re-posted and Retargeted stays 0.
func TestReconcileBlockedIssuesSupersededNotRepostedWhenBudgetExhausted(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		// Blocker PR 100 closed unmerged, superseded by #101 (still open).
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/100":
			_, _ = io.WriteString(w, `{"number":100,"state":"closed","merged":false}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/100/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"Superseded by #101."}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/101":
			_, _ = io.WriteString(w, `{"number":101,"state":"open"}`)
		// Issue 1 already carries the re-target comment from an earlier pass.
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/1/comments":
			_, _ = fmt.Fprintf(w, `[{"id":1,"body":%q}]`, BlockedOnMarker("o/r", 101)+"\nRe-targeted: blocker #100 was superseded by #101.")
		// Filler blocker PR 400 is open; filler issues have no comments.
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/400":
			_, _ = io.WriteString(w, `{"number":400,"state":"open"}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/") && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			mu.Lock()
			posts = append(posts, r.URL.Path)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = testBot
	// Exhaust the per-pass comment budget with filler issues on an open
	// blocker so issue 1 is reconciled from its body marker alone.
	var rows []RepoWorkIssue
	for i := 0; i < blockedSweepCommentLookups; i++ {
		rows = append(rows, RepoWorkIssue{Repo: "o/r", Number: 1000 + i, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#400"})
	}
	rows = append(rows, RepoWorkIssue{Repo: "o/r", Number: 1, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#100"})
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows}}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != blockedSweepCommentLookups+1 || res.Retargeted != 0 || res.Unblocked != 0 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Items=%d Retargeted=0 Unblocked=0 Errors=0", res, blockedSweepCommentLookups+1)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posts) != 0 {
		t.Fatalf("no comment may be posted when the newest marker already names the superseder; posts = %v", posts)
	}
	read := false
	for _, call := range calls {
		if call == "GET /repos/o/r/issues/1/comments" {
			read = true
		}
	}
	if !read {
		t.Fatalf("issue 1 comments must be read before re-targeting; calls = %v", calls)
	}
}

// Issue 14194: when an issue's body has an older blocker marker (#50, settled)
// but a comment has a newer blocker marker (#60, still open), the comment marker wins
// and the issue stays blocked.
func TestReconcileBlockedIssuesCommentMarkerOverridesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/50":
			_, _ = io.WriteString(w, `{"number":50,"state":"closed","merged":true}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/60":
			_, _ = io.WriteString(w, `{"number":60,"state":"open"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/5/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"`+BlockedOnMarker("o/r", 60)+`\nRe-blocked on #60"}]`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/5/labels/help wanted"):
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	result := &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{
		"o/r": {Issues: []RepoWorkIssue{
			{Repo: "o/r", Number: 5, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#50"},
		}},
	}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != 1 || res.Unblocked != 0 || res.Stripped != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Items=1 Unblocked=0 Stripped=1 Errors=0", res)
	}
}

// Issue 14194: SweepBlockedIssues lists open issues with `blocked` label directly
// and reconciles them.
func TestSweepBlockedIssues(t *testing.T) {
	var mu sync.Mutex
	var unblocked bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues" && strings.Contains(r.URL.RawQuery, "labels=blocked"):
			_, _ = io.WriteString(w, `[{"number":10,"state":"open","labels":[{"name":"blocked"}],"body":"`+BlockedOnMarker("o/r", 1)+`\nBlocked on #1"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/10/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/1":
			_, _ = io.WriteString(w, `{"number":1,"state":"closed","merged":true}`)
		case r.Method == "DELETE" && r.URL.Path == "/repos/o/r/issues/10/labels/blocked":
			mu.Lock()
			unblocked = true
			mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/10/comments":
			_, _ = io.WriteString(w, `{"id":1}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	res, err := c.SweepBlockedIssues(context.Background(), BlockedSweepOptions{Repos: []string{"o/r"}})
	if err != nil {
		t.Fatalf("SweepBlockedIssues failed: %v", err)
	}
	if res.Items != 1 || res.Unblocked != 1 || res.Errors != 0 {
		t.Fatalf("res = %+v, want Items=1 Unblocked=1 Errors=0", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if !unblocked {
		t.Fatal("blocked label was not removed from issue 10")
	}
}
