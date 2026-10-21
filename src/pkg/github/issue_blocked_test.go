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
// no `help wanted` — while a finding on a file the base already has files
// as before.
func TestReviewBacklogFilesBlockedWhenFileMissingOnBase(t *testing.T) {
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
	var blocked, plain int
	for _, is := range issues {
		if strings.Contains(is.Body, "pkg/new.go") {
			blocked++
			if _, pr, ok := ParseBlockedOnMarker(is.Body); !ok || pr != 5 {
				t.Errorf("blocked body lacks marker: %q", is.Body)
			}
			if !strings.Contains(is.Body, "Blocked on PR #5") || !strings.Contains(is.Body, "does not exist on `v5`") {
				t.Errorf("blocked body = %q", is.Body)
			}
			if strings.Join(is.Labels, ",") != "nit,blocked,agent/reviewer" {
				t.Errorf("blocked labels = %v, want nit,blocked,agent/reviewer (no help wanted)", is.Labels)
			}
		} else {
			plain++
			if _, _, ok := ParseBlockedOnMarker(is.Body); ok {
				t.Errorf("base-file finding carries marker: %q", is.Body)
			}
			if strings.Join(is.Labels, ",") != "nit,help wanted,agent/reviewer" {
				t.Errorf("plain labels = %v", is.Labels)
			}
		}
	}
	if blocked != 1 || plain != 1 {
		t.Fatalf("blocked=%d plain=%d", blocked, plain)
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
