package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
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
	r, n, onIssue, ok := parseBlockedOnReason("blocked on hivecommons/hive#13092")
	if !ok || onIssue || r != "hivecommons/hive" || n != 13092 {
		t.Fatalf("parseBlockedOnReason = %q %d %v %v", r, n, onIssue, ok)
	}
	if _, _, _, ok := parseBlockedOnReason("blocked"); ok {
		t.Fatal("bare reason parsed a PR")
	}
	if !IsBlockedIssueReason("blocked on o/r#1") || !IsBlockedIssueReason("blocked") || IsBlockedIssueReason("hold label") {
		t.Fatal("IsBlockedIssueReason")
	}
}

// The issue form of the marker, `<!-- hive:blocked-on issue=owner/repo#N -->`
// (projectbluefin/utah#10, #855): it round-trips through the filtered reason
// as "blocked on issue owner/repo#N", never reads as a PR, and a malformed
// one is no marker at all.
func TestBlockedOnIssueMarker(t *testing.T) {
	const m = "<!-- hive:blocked-on issue=projectbluefin/utah-packages#42 -->"
	repo, n, ok := parseBlockedOnIssueMarker("still blocked on the factory\n" + m)
	if !ok || repo != "projectbluefin/utah-packages" || n != 42 {
		t.Fatalf("parseBlockedOnIssueMarker = %q %d %v", repo, n, ok)
	}
	if _, _, ok := ParseBlockedOnMarker(m); ok {
		t.Fatal("issue marker parsed as a PR marker")
	}
	if _, _, ok := parseBlockedOnIssueMarker(BlockedOnMarker("o/r", 5)); ok {
		t.Fatal("PR marker parsed as an issue marker")
	}
	reason := blockedFilterReason("body\n" + m)
	if reason != "blocked on issue projectbluefin/utah-packages#42" {
		t.Fatalf("filter reason = %q", reason)
	}
	if !IsBlockedIssueReason(reason) {
		t.Fatal("issue reason not recognised as blocked")
	}
	if r, n, onIssue, ok := parseBlockedOnReason(reason); !ok || !onIssue || r != "projectbluefin/utah-packages" || n != 42 {
		t.Fatalf("parseBlockedOnReason(issue) = %q %d %v %v", r, n, onIssue, ok)
	}
	if _, _, ok := ParseBlockedOnReason(reason); ok {
		t.Fatal("issue reason read as a blocking PR")
	}
	if got := blockedFilterReason(BlockedOnMarker("o/r", 5) + "\n" + m); got != "blocked on o/r#5" {
		t.Fatalf("PR marker must win over the issue form: %q", got)
	}
	for _, bad := range []string{
		"<!-- hive:blocked-on issue=factory -->",
		"<!-- hive:blocked-on issue=o/r -->",
		"<!-- hive:blocked-on issue=o/r#0 -->",
		"<!-- hive:blocked-on issue=#7 -->",
	} {
		if _, _, ok := parseBlockedOnIssueMarker(bad); ok {
			t.Errorf("malformed marker %q parsed", bad)
		}
		if got := blockedFilterReason(bad); got != "blocked" {
			t.Errorf("malformed marker %q filter reason = %q", bad, got)
		}
	}
}

// projectbluefin/utah#10 and #855: a `blocked` set for an issue dependency
// was lifted as marker-less and re-verified "still blocked" nine times. A
// block whose marker names an open issue stays (only `help wanted` comes
// off, whoever applied the label); a closed one lifts with one note; a
// malformed issue marker falls back to the marker-less lift; a PR marker
// still reads the PR.
func TestReconcileBlockedIssuesBlockedOnIssue(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var notes []string
	hiveLabeled := `[{"event":"labeled","created_at":"2026-10-22T10:00:00Z","label":{"name":"blocked"},"actor":{"login":"` + testBot + `","type":"Bot"}}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/100":
			_, _ = io.WriteString(w, `{"number":100,"state":"open"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/pkgs/issues/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"closed","closed_at":"2026-10-25T10:00:00Z"}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/2":
			_, _ = io.WriteString(w, `{"number":2,"state":"open","base":{"ref":"v5"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/20/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"triage"},{"id":2,"body":"<!-- hive:blocked-on issue=o/pkgs#5 -->\nBlocked on o/pkgs#5"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/30/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"<!-- hive:blocked-on issue=factory -->\nstill blocked on factory"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/40/comments":
			_, _ = io.WriteString(w, `[{"id":1,"body":"<!-- hive:blocked-on issue=o/r#100 -->\nstill blocked on factory"}]`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/timeline"):
			_, _ = io.WriteString(w, hiveLabeled)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/30":
			_, _ = io.WriteString(w, `{"number":30,"state":"open","title":"blocked on factory","labels":[{"name":"blocked"}]}`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notes = append(notes, r.URL.Path+" "+body.Body)
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
			// Body marker names an open issue: stays blocked.
			{Repo: "o/r", Number: 10, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked on issue o/r#100"},
			// Comment marker names a closed issue in another repo: lifted.
			{Repo: "o/r", Number: 20, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked"},
			// Malformed issue marker, hive-applied label: read as marker-less,
			// which on v6 becomes a needs-human ask rather than a plain lift.
			{Repo: "o/r", Number: 30, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked"},
			// The utah#10 shape: hive-applied label, comment marker on an
			// open issue: stays blocked, never read as marker-less.
			{Repo: "o/r", Number: 40, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked"},
			// PR marker unchanged: the PR is read, not the issue.
			{Repo: "o/r", Number: 50, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#2"},
		}},
	}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != 5 || res.Unblocked != 1 || res.AskedHuman != 1 || res.Stripped != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"GET /repos/o/r/issues/100",
		"DELETE /repos/o/r/issues/10/labels/help wanted",
		"GET /repos/o/pkgs/issues/5",
		"DELETE /repos/o/r/issues/20/labels/blocked",
		"GET /repos/o/r/issues/30/timeline",
		"POST /repos/o/r/issues/30/labels",
		"DELETE /repos/o/r/issues/30/labels/blocked",
		"GET /repos/o/r/pulls/2",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}
	for _, never := range []string{
		"DELETE /repos/o/r/issues/10/labels/blocked",
		"GET /repos/o/r/issues/10/timeline",
		"GET /repos/o/r/issues/20/timeline",
		"DELETE /repos/o/r/issues/40/labels/blocked",
		"GET /repos/o/r/issues/40/timeline",
		"DELETE /repos/o/r/issues/50/labels/blocked",
		"GET /repos/o/r/issues/2\n",
		"GET /repos/o/r/pulls/100",
	} {
		if strings.Contains(joined+"\n", never) {
			t.Errorf("unexpected call %q in:\n%s", never, joined)
		}
	}
	notesJoined := strings.Join(notes, "\n")
	if !strings.Contains(notesJoined, "/repos/o/r/issues/20/comments "+blockedUnblockedMarker+"\nUnblocked: o/pkgs#5 is closed.") {
		t.Errorf("issue-closed note missing: %q", notesJoined)
	}
	if !strings.Contains(notesJoined, "/repos/o/r/issues/30/comments "+blockedMarkerlessAskNote) {
		t.Errorf("marker-less note missing for the malformed marker: %q", notesJoined)
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
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue, Labels: []string{"nit", "help wanted"}, MinSeverity: "P3"},
		}
	})
	recs := captureAudit(c)
	// Two passes: findings from one pass that share a perspective collapse
	// into one item (hivecommons/hive#17065), and this test wants each
	// finding's own blocked note.
	for _, finding := range []map[string]any{
		nit("only-in-pr", "low", "pkg/new.go", 3, "introduced by the PR"),
		nit("on-base", "low", "pkg/old.go", 4, "already there"),
	} {
		report := routingReport(t, 5, "abc", finding)
		if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
			t.Fatal(err)
		}
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
	notes := map[string]string{}
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
		case r.Method == "GET" && (r.URL.Path == "/repos/o/r/issues/80/comments" || r.URL.Path == "/repos/o/r/issues/90/comments"):
			_, _ = io.WriteString(w, `[{"id":1,"body":"Blocked: the upstream API this needs is not released yet."}]`)
		case r.Method == "GET" && (r.URL.Path == "/repos/o/r/issues/80/timeline" || r.URL.Path == "/repos/o/r/issues/90/timeline"):
			_, _ = io.WriteString(w, `[{"event":"labeled","created_at":"2026-10-20T10:05:00Z","label":{"name":"blocked"},"actor":{"login":"`+testBot+`","type":"Bot"}}]`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/80/labels":
			_, _ = io.WriteString(w, `[{"name":"needs-human"}]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments"):
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notes[r.URL.Path] = body.Body
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
			{Repo: "o/r", Number: 10, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#1"},
			{Repo: "o/r", Number: 20, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#2"},
			{Repo: "o/r", Number: 30, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked"},
			{Repo: "o/r", Number: 40, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on o/r#404"},
			{Repo: "o/r", Number: 50, Labels: []string{"hold"}, Bucket: workBucketFiltered, Reason: "hold label"},
			// A relay's blocked verdict, no tracked PR (hivecommons/hive#13267):
			// nothing to lift, but `help wanted` still comes off.
			{Repo: "o/r", Number: 60, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked"},
			// A hive-applied `blocked` with no marker anywhere on a
			// review-backlog item (a blocked filing folded into an open twin,
			// hivecommons/hive#13364) is a defect: lifted.
			{Repo: "o/r", Number: 70, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked", ReviewBacklog: true},
			// A contributor relay's blocked verdict that named nothing: the
			// task token makes the labeled actor the hive's own bot. It is
			// not lifted (the next relay would re-block it) but turned into
			// a needs-human ask, and loses `help wanted`.
			{Repo: "o/r", Number: 80, Labels: []string{"blocked", "help wanted"}, Bucket: workBucketFiltered, Reason: "blocked"},
			// A review-backlog item GitHub still reports an open "blocked
			// by" dependency on (#9839) stays blocked.
			{Repo: "o/r", Number: 90, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked", ReviewBacklog: true, OpenBlockedBy: 1},
		}},
	}}
	res := ReconcileBlockedIssues(context.Background(), c, result, nil)
	if res.Items != 8 || res.Unblocked != 3 || res.AskedHuman != 1 || res.Stripped != 3 || res.Errors != 1 {
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
		"DELETE /repos/o/r/issues/60/labels/help wanted",
		"GET /repos/o/r/issues/70/comments",
		"GET /repos/o/r/issues/70/timeline",
		"DELETE /repos/o/r/issues/70/labels/blocked",
		"POST /repos/o/r/issues/70/comments",
		"GET /repos/o/r/issues/80/comments",
		"GET /repos/o/r/issues/80/timeline",
		"POST /repos/o/r/issues/80/labels",
		"DELETE /repos/o/r/issues/80/labels/blocked",
		"POST /repos/o/r/issues/80/comments",
		"DELETE /repos/o/r/issues/80/labels/help wanted",
		"GET /repos/o/r/issues/60/timeline",
		"GET /repos/o/r/issues/90/comments",
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
		// A person's marker-less block is read (to learn who applied it)
		// but never converted; an open native blocker spends no read.
		"POST /repos/o/r/issues/60/labels",
		"GET /repos/o/r/issues/90/timeline",
		"DELETE /repos/o/r/issues/90/labels/blocked",
		"POST /repos/o/r/issues/90/comments",
	} {
		if strings.Contains(joined, never) {
			t.Errorf("unexpected call %q in:\n%s", never, joined)
		}
	}
	// The unblocked notes promise a re-triage, not the agent queue: an
	// issue project.issue_filter refuses never reaches the queue.
	for _, issue := range []string{"10", "30", "70"} {
		note := notes["/repos/o/r/issues/"+issue+"/comments"]
		if !strings.HasPrefix(note, blockedUnblockedMarker+"\n") || !strings.Contains(note, "re-triaging") || strings.Contains(note, "actionable") {
			t.Errorf("issue %s unblocked note = %q, want the marker and a re-triage, no actionable promise", issue, note)
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
	if _, _, _, ok := parseBlockedOnReason("blocked until 2026-10-21T21:20:00Z"); ok {
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
		// liftBlockedMarkerless reads both off the row: the review-backlog
		// footer and GitHub's open "blocked by" count.
		{Number: 3, Title: "backlog", User: wireUser{"hive-app[bot]"}, Labels: []wireLabel{{Name: "blocked"}}, CreatedAt: hoursAgo(1),
			Body: "Finding: x\n\n" + reviewBacklogFooter + "\n", IssueDependenciesSummary: &wireDepsSummary{BlockedBy: 2, TotalBlockedBy: 2}},
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
	if r := rows[2]; r.Bucket != workBucketFiltered || r.Reason != "blocked" || !r.RecheckAfter.IsZero() || r.OpsTracking || r.ReviewBacklog || r.OpenBlockedBy != 0 {
		t.Fatalf("bare row = %+v", r)
	}
	if r := rows[3]; r.Bucket != workBucketFiltered || r.Reason != "blocked" || !r.ReviewBacklog || r.OpenBlockedBy != 2 {
		t.Fatalf("backlog row = %+v", r)
	}
	// A blocked-on marker the sweep read from #2's comments names the
	// row's blocker on the next enumeration (projectbluefin/finpilot#624).
	noteBlockedOnSeen(repo, 2, "other/actions", 697)
	t.Cleanup(func() { forgetBlockedOnSeen(repo, 2) })
	result, err = c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	for _, row := range result.WorkDetailsByRepo[repo].Issues {
		if row.Number == 2 && row.Reason != "blocked on other/actions#697" {
			t.Fatalf("row reason after a comment marker = %q", row.Reason)
		}
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

// A contributor relay's marker-less `blocked` carries the hive's own bot as
// the labeled actor (the hub-minted task token). Lifting it put the issue
// back in the ready band and the next relay re-blocked it, daily
// (projectbluefin/utah#771); keeping it hid the issue from every inbox
// (projectbluefin/testsuite#908). The sweep turns it into a needs-human ask,
// once. Decision table:
//
//	#70 review-backlog, hive-applied, no marker  → lifted (hivecommons/hive#13364)
//	#80 relay verdict, hive-applied, no marker   → needs-human ask
//	#85 a person applied it, no marker           → left alone
//	#90 open native "blocked by" link (backlog)  → left alone
//	#92 open native "blocked by" link (relay)    → left alone
//	#95 blocked-on marker in a comment (open PR) → left alone; the row names the PR
func TestSweepBlockedIssuesAsksHumanOnRelayMarkerlessBlock(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	gone := map[int]bool{}
	backlogBody := "Filed from an out-of-scope finding in the Hive review of PR #9.\n\n" + reviewBacklogFooter + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues" && strings.Contains(r.URL.RawQuery, "labels=blocked"):
			items := []map[string]any{
				{"number": 85, "state": "open", "labels": []any{map[string]any{"name": "blocked"}}, "body": "Waiting on the vendor."},
				{"number": 90, "state": "open", "labels": []any{map[string]any{"name": "blocked"}}, "body": backlogBody, "issue_dependencies_summary": map[string]any{"blocked_by": 1, "total_blocked_by": 1}},
				{"number": 95, "state": "open", "labels": []any{map[string]any{"name": "blocked"}}, "body": "Needs the shared workflow fix."},
				{"number": 92, "state": "open", "labels": []any{map[string]any{"name": "blocked"}}, "body": "Waits on the upstream issue.", "issue_dependencies_summary": map[string]any{"blocked_by": 1, "total_blocked_by": 1}},
			}
			mu.Lock()
			if !gone[70] {
				items = append(items, map[string]any{"number": 70, "state": "open", "labels": []any{map[string]any{"name": "blocked"}}, "body": backlogBody})
			}
			if !gone[80] {
				items = append(items, map[string]any{"number": 80, "state": "open", "labels": []any{map[string]any{"name": "blocked"}}, "body": "Upgrade the client once the upstream API ships."})
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(items)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/95/comments":
			_, _ = io.WriteString(w, `[{"id":3,"body":"`+BlockedOnMarker("other/actions", 697)+`\nblocked on the shared fix"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/other/actions/pulls/697":
			_, _ = io.WriteString(w, `{"number":697,"state":"open"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `[{"id":1,"body":"---\n🐝 **Hive Agent**: `+"`contributor`"+`"}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/85/timeline":
			_, _ = io.WriteString(w, `[{"event":"labeled","created_at":"2026-10-27T20:38:49Z","label":{"name":"blocked"},"actor":{"login":"castrojo","type":"User"}}]`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/timeline"):
			_, _ = io.WriteString(w, `[{"event":"labeled","created_at":"2026-10-29T04:17:25Z","label":{"name":"blocked"},"actor":{"login":"`+testBot+`","type":"Bot"}}]`)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/80/labels":
			_, _ = io.WriteString(w, `[{"name":"needs-human"}]`)
		case r.Method == "DELETE" && (r.URL.Path == "/repos/o/r/issues/70/labels/blocked" || r.URL.Path == "/repos/o/r/issues/80/labels/blocked"):
			n, _ := strconv.Atoi(strings.Split(r.URL.Path, "/")[5])
			mu.Lock()
			gone[n] = true
			mu.Unlock()
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && (r.URL.Path == "/repos/o/r/issues/70/comments" || r.URL.Path == "/repos/o/r/issues/80/comments"):
			_, _ = io.WriteString(w, `{"id":9}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = testBot
	for pass := 1; pass <= 3; pass++ {
		res, err := c.SweepBlockedIssues(context.Background(), BlockedSweepOptions{Repos: []string{"o/r"}})
		if err != nil {
			t.Fatalf("pass %d: SweepBlockedIssues failed: %v", pass, err)
		}
		want := 0
		if pass == 1 {
			want = 1
		}
		if res.Unblocked != want || res.AskedHuman != want || res.Errors != 0 {
			t.Fatalf("pass %d: res = %+v, want Unblocked=%d AskedHuman=%d Errors=0", pass, res, want, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"DELETE /repos/o/r/issues/70/labels/blocked",
		"POST /repos/o/r/issues/80/labels",
		"DELETE /repos/o/r/issues/80/labels/blocked",
		"POST /repos/o/r/issues/80/comments",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}
	if strings.Index(joined, "POST /repos/o/r/issues/80/labels") > strings.Index(joined, "DELETE /repos/o/r/issues/80/labels/blocked") {
		t.Errorf("needs-human must land before blocked comes off, or a failed add leaves the issue in the queue:\n%s", joined)
	}
	for _, never := range []string{
		"DELETE /repos/o/r/issues/85/labels/blocked", "POST /repos/o/r/issues/85/labels",
		"DELETE /repos/o/r/issues/90/labels/blocked", "POST /repos/o/r/issues/90/labels", "GET /repos/o/r/issues/90/timeline",
		"DELETE /repos/o/r/issues/95/labels/blocked", "POST /repos/o/r/issues/95/labels", "GET /repos/o/r/issues/95/timeline",
		"DELETE /repos/o/r/issues/92/labels/blocked", "POST /repos/o/r/issues/92/labels", "GET /repos/o/r/issues/92/timeline",
	} {
		if strings.Contains(joined, never) {
			t.Errorf("unexpected call %q in:\n%s", never, joined)
		}
	}
	if got := blockedFilterReasonFor("o/r", 95, "Needs the shared workflow fix."); got != "blocked on other/actions#697" {
		t.Errorf("row reason for a comment marker = %q, want blocked on other/actions#697", got)
	}
	if got := blockedFilterReasonFor("o/r", 85, "Waiting on the vendor."); got != "blocked" {
		t.Errorf("row reason with no marker = %q, want the bare label", got)
	}
}

// The note says what was missing and both ways out.
func TestBlockedMarkerlessAskNote(t *testing.T) {
	for _, want := range []string{blockedUnblockedMarker, "named no issue or pull request", "`needs-human`", "hive:blocked-on pr=owner/repo#N", "blocked by"} {
		if !strings.Contains(blockedMarkerlessAskNote, want) {
			t.Errorf("note lacks %q:\n%s", want, blockedMarkerlessAskNote)
		}
	}
}

// unblockIssue forgets the comment marker it remembered.
func TestUnblockForgetsCommentMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	noteBlockedOnSeen("o/r", 7, "o/r", 3)
	if got := blockedFilterReasonFor("O/R", 7, ""); got != "blocked on o/r#3" {
		t.Fatalf("reason = %q", got)
	}
	if err := c.unblockIssue(context.Background(), "o/r", 7); err != nil {
		t.Fatal(err)
	}
	if got := blockedFilterReasonFor("o/r", 7, ""); got != "blocked" {
		t.Fatalf("reason after unblock = %q, want the bare label", got)
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

// A writeback-gate refusal (a burst hold, a duplicate within same_item_ttl,
// a paused repo) leaves the block as it is for a later sweep and says so in
// the log instead of falling through silently.
func TestSweepBlockedAskHeldByGateLogs(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues" && strings.Contains(r.URL.RawQuery, "labels=blocked"):
			_, _ = io.WriteString(w, `[{"number":80,"state":"open","labels":[{"name":"blocked"}],"body":"Upgrade once upstream ships."}]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/80":
			_, _ = io.WriteString(w, `{"number":80,"state":"open","user":{"login":"`+testBot+`"},"labels":[{"name":"blocked"}]}`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/80/comments":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/80/timeline":
			_, _ = io.WriteString(w, `[{"event":"labeled","created_at":"2026-10-29T04:17:25Z","label":{"name":"blocked"},"actor":{"login":"`+testBot+`","type":"Bot"}}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = testBot
	var logs bytes.Buffer
	c.logger = slog.New(slog.NewTextHandler(&logs, nil))
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "owner", Reason: "freeze"}, true
		},
	}, ""))
	res, err := c.SweepBlockedIssues(context.Background(), BlockedSweepOptions{Repos: []string{"o/r"}})
	if err != nil {
		t.Fatalf("SweepBlockedIssues: %v", err)
	}
	if res.AskedHuman != 0 || res.Unblocked != 0 {
		t.Fatalf("res = %+v, want nothing converted while the gate refuses", res)
	}
	mu.Lock()
	joined := strings.Join(calls, "\n")
	mu.Unlock()
	for _, never := range []string{"POST /repos/o/r/issues/80/labels", "DELETE /repos/o/r/issues/80/labels/blocked", "POST /repos/o/r/issues/80/comments"} {
		if strings.Contains(joined, never) {
			t.Errorf("unexpected call %q while the gate refuses:\n%s", never, joined)
		}
	}
	if out := logs.String(); !strings.Contains(out, "needs-human ask held by writeback gate") || !strings.Contains(out, "issue=80") {
		t.Fatalf("gate refusal not logged:\n%s", out)
	}
}

func TestBlockedOnIssueTarget(t *testing.T) {
	cases := []struct {
		name, body, reason string
		repo               string
		n                  int
		ok                 bool
	}{
		{"marker", "x <!-- hive:blocked-on issue=acme/api#9 -->", "blocked", "acme/api", 9, true},
		{"reason", "", "blocked on issue acme/api#9", "acme/api", 9, true},
		{"pr marker is not an issue", BlockedOnMarker("acme/api", 9), "blocked", "", 0, false},
		{"pr reason is not an issue", "", "blocked on acme/api#9", "", 0, false},
		{"bare", "", "blocked", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, n, ok := BlockedOnIssueTarget(tc.body, tc.reason)
			if repo != tc.repo || n != tc.n || ok != tc.ok {
				t.Fatalf("got %q %d %v", repo, n, ok)
			}
		})
	}
}

// blockedSuccessorFake serves closed blocker issues, their comments, the
// duplicate-of GraphQL lookup and the blocked issues' own comments, and
// records every call and write. A comment posted on an issue is served back
// on the next read, as GitHub would.
type blockedSuccessorFake struct {
	mu sync.Mutex
	// issues maps "owner/repo#N" to the issue's JSON.
	issues map[string]string
	// comments maps "owner/repo#N" to the JSON objects of its comments.
	comments map[string][]string
	// duplicateOf maps a node id to the JSON of the canonical issue.
	duplicateOf map[string]string
	// moved maps "owner/repo#N" to the path GitHub redirects it to.
	moved map[string]string
	// commentsStatus maps "owner/repo#N" to the HTTP status its comment
	// listing answers with instead of the comments.
	commentsStatus map[string]int
	// graphQLStatus, when set, is the HTTP status the GraphQL endpoint
	// answers with; graphQLReply, when set, is the body it answers with
	// instead of the duplicate-of data.
	graphQLStatus int
	graphQLReply  string
	calls         []string
	graphQL       int
	posts         map[string][]string
	removed       []string
}

func newBlockedSuccessorFake() *blockedSuccessorFake {
	return &blockedSuccessorFake{issues: map[string]string{}, comments: map[string][]string{}, duplicateOf: map[string]string{}, moved: map[string]string{}, commentsStatus: map[string]int{}, posts: map[string][]string{}}
}

func (f *blockedSuccessorFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
			var req struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.graphQL++
			if f.graphQLStatus != 0 {
				w.WriteHeader(f.graphQLStatus)
				_, _ = io.WriteString(w, `{"message":"unavailable"}`)
				return
			}
			if f.graphQLReply != "" {
				_, _ = io.WriteString(w, f.graphQLReply)
				return
			}
			canonical, ok := f.duplicateOf[req.Variables.ID]
			if !ok {
				canonical = "null"
			}
			_, _ = io.WriteString(w, `{"data":{"node":{"duplicateOf":`+canonical+`}}}`)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) < 4 || parts[2] != "issues" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		ref := parts[0] + "/" + parts[1] + "#" + parts[3]
		switch {
		case len(parts) == 4 && r.Method == http.MethodGet:
			if to, ok := f.moved[ref]; ok {
				w.Header().Set("Location", to)
				w.WriteHeader(http.StatusMovedPermanently)
				return
			}
			issue, ok := f.issues[ref]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Not Found"}`)
				return
			}
			_, _ = io.WriteString(w, issue)
		case len(parts) == 5 && parts[4] == "comments" && r.Method == http.MethodGet:
			if status := f.commentsStatus[ref]; status != 0 {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"message":"unavailable"}`)
				return
			}
			_, _ = io.WriteString(w, "["+strings.Join(f.comments[ref], ",")+"]")
		case len(parts) == 5 && parts[4] == "comments" && r.Method == http.MethodPost:
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.posts[ref] = append(f.posts[ref], payload.Body)
			f.comments[ref] = append(f.comments[ref], fmt.Sprintf(`{"id":%d,"body":%q,"user":{"login":%q}}`, 900+len(f.calls), payload.Body, testBot))
			_, _ = io.WriteString(w, `{"id":9}`)
		case r.Method == http.MethodDelete && len(parts) == 6 && parts[4] == "labels":
			f.removed = append(f.removed, ref+" "+parts[5])
			_, _ = io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// called reports whether the fake saw the exact "METHOD /path" call.
func (f *blockedSuccessorFake) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func blockedSuccessorRow(number int, blocker string) RepoWorkIssue {
	return RepoWorkIssue{Repo: "o/r", Number: number, Labels: []string{"blocked"}, Bucket: workBucketFiltered, Reason: "blocked on issue " + blocker}
}

// An issue blocked on another issue used to be unblocked the moment that
// issue closed. Closed as a duplicate of an open issue (GitHub's own closure,
// here across repositories), or replaced by one a trusted "Superseded by"
// names, the block now moves to the open issue with one re-target comment. A
// closed canonical issue, a plain closure, and a stranger's "Superseded by"
// unblock exactly as before. A transferred blocker is not closed: GitHub
// redirects its old URL, the client follows it, and the block stays.
func TestReconcileBlockedIssuesFollowsIssueSuccessor(t *testing.T) {
	f := newBlockedSuccessorFake()
	// #1: closed as a duplicate of an open issue in another repository.
	f.issues["o/r#100"] = `{"number":100,"state":"closed","state_reason":"duplicate","node_id":"N100","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	f.duplicateOf["N100"] = `{"number":7,"repository":{"nameWithOwner":"o/other"}}`
	f.issues["o/other#7"] = `{"number":7,"state":"open"}`
	// #2: closed as a duplicate of an issue that is closed too.
	f.issues["o/r#200"] = `{"number":200,"state":"closed","state_reason":"duplicate","node_id":"N200","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	f.duplicateOf["N200"] = `{"number":201,"repository":{"nameWithOwner":"o/r"}}`
	f.issues["o/r#201"] = `{"number":201,"state":"closed","state_reason":"completed","closed_at":"2026-11-09T10:00:00Z","user":{"login":"reporter"}}`
	// #3: closed, and a maintainer said what replaced it.
	f.issues["o/r#300"] = `{"number":300,"state":"closed","state_reason":"not_planned","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	f.comments["o/r#300"] = []string{`{"id":1,"body":"Superseded by #301.","user":{"login":"maintainer"},"author_association":"MEMBER"}`}
	f.issues["o/r#301"] = `{"number":301,"state":"open"}`
	// #4: closed as completed, nothing names a successor.
	f.issues["o/r#400"] = `{"number":400,"state":"closed","state_reason":"completed","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	// #5: closed; only a stranger says what replaced it.
	f.issues["o/r#500"] = `{"number":500,"state":"closed","state_reason":"completed","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	f.comments["o/r#500"] = []string{`{"id":1,"body":"Superseded by #501.","user":{"login":"mallory"},"author_association":"NONE"}`}
	f.issues["o/r#501"] = `{"number":501,"state":"open"}`
	// #6: a chain: duplicate of a closed issue whose author named an open one.
	f.issues["o/r#600"] = `{"number":600,"state":"closed","state_reason":"duplicate","node_id":"N600","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	f.duplicateOf["N600"] = `{"number":601,"repository":{"nameWithOwner":"o/r"}}`
	f.issues["o/r#601"] = `{"number":601,"state":"closed","state_reason":"completed","closed_at":"2026-11-09T10:00:00Z","user":{"login":"reporter"}}`
	f.comments["o/r#601"] = []string{`{"id":1,"body":"Superseded by #602.","user":{"login":"reporter"},"author_association":"NONE"}`}
	f.issues["o/r#602"] = `{"number":602,"state":"open"}`
	// #7: transferred to another repository, where it is still open.
	f.moved["o/r#700"] = "/repos/o/moved/issues/9"
	f.issues["o/moved#9"] = `{"number":9,"state":"open","repository_url":"https://api.github.com/repos/o/moved"}`

	c := testClient(t, f.server(t).URL)
	c.appBotLogin = testBot
	t.Cleanup(func() {
		for _, n := range []int{1, 3, 6} {
			forgetBlockedOnSeen("o/r", n)
		}
	})
	rows := []RepoWorkIssue{
		blockedSuccessorRow(1, "o/r#100"), blockedSuccessorRow(2, "o/r#200"), blockedSuccessorRow(3, "o/r#300"),
		blockedSuccessorRow(4, "o/r#400"), blockedSuccessorRow(5, "o/r#500"), blockedSuccessorRow(6, "o/r#600"),
		blockedSuccessorRow(7, "o/r#700"),
	}
	res := ReconcileBlockedIssues(context.Background(), c, &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows}}}, nil)
	if res.Items != 7 || res.Retargeted != 3 || res.Unblocked != 3 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Items=7 Retargeted=3 Unblocked=3 Errors=0", res)
	}

	f.mu.Lock()
	posts, removed, graphQL := f.posts, strings.Join(f.removed, "\n"), f.graphQL
	f.mu.Unlock()
	retargets := map[string]string{
		"o/r#1": "<!-- hive:blocked-on issue=o/other#7 -->\nRe-targeted: blocker o/r#100 is a duplicate of o/other#7, which is still open.",
		"o/r#3": "<!-- hive:blocked-on issue=o/r#301 -->\nRe-targeted: blocker o/r#300 was replaced by o/r#301, which is still open.",
		"o/r#6": "<!-- hive:blocked-on issue=o/r#602 -->\nRe-targeted: blocker o/r#600 was replaced by o/r#602, which is still open.",
	}
	for ref, want := range retargets {
		if got := posts[ref]; len(got) != 1 || got[0] != want {
			t.Errorf("%s comments = %q, want exactly %q", ref, got, want)
		}
		if strings.Contains(removed, ref+" blocked") {
			t.Errorf("%s must stay blocked; removed labels:\n%s", ref, removed)
		}
	}
	unblocks := map[string]string{"o/r#2": "o/r#200", "o/r#4": "o/r#400", "o/r#5": "o/r#500"}
	for ref, blocker := range unblocks {
		want := blockedUnblockedMarker + "\nUnblocked: " + blocker + " is closed. This issue is actionable again."
		if got := posts[ref]; len(got) != 1 || got[0] != want {
			t.Errorf("%s comments = %q, want exactly the note it always had, %q", ref, got, want)
		}
		if !strings.Contains(removed, ref+" blocked") {
			t.Errorf("%s must be unblocked as before; removed labels:\n%s", ref, removed)
		}
	}
	if len(posts["o/r#7"]) != 0 || strings.Contains(removed, "o/r#7 ") {
		t.Errorf("a transferred blocker that is open in its new repository must keep the block: posts=%q removed:\n%s", posts["o/r#7"], removed)
	}
	if graphQL != 3 {
		t.Errorf("the duplicate-of lookup runs once per duplicate closure (3), ran %d times", graphQL)
	}
	if f.called("GET /repos/o/r/issues/501") {
		t.Error("a stranger's successor must not even be read")
	}
	if !f.called("GET /repos/o/moved/issues/9") {
		t.Error("the transferred blocker's redirect must be followed to its new repository")
	}

	// A later pass: the newest marker names the open successor, so nothing
	// is written again.
	still := []RepoWorkIssue{blockedSuccessorRow(1, "o/r#100"), blockedSuccessorRow(3, "o/r#300"), blockedSuccessorRow(6, "o/r#600"), blockedSuccessorRow(7, "o/r#700")}
	res = ReconcileBlockedIssues(context.Background(), c, &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: still}}}, nil)
	if res.Retargeted != 0 || res.Unblocked != 0 || res.Errors != 0 {
		t.Fatalf("second pass = %+v, want nothing re-targeted or unblocked", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ref := range []string{"o/r#1", "o/r#3", "o/r#6"} {
		if len(f.posts[ref]) != 1 {
			t.Errorf("%s got a second comment on the later pass: %q", ref, f.posts[ref])
		}
	}
	if f.graphQL != graphQL {
		t.Errorf("a re-targeted issue must not look its old blocker up again: %d GraphQL calls, was %d", f.graphQL, graphQL)
	}
}

// The re-target lives in a comment while the body still names the closed
// blocker. On a pass whose comment budget ran out before the issue, the
// sweep sees the old blocker from the body and finds its successor again; it
// must read the comments before posting, see the newest marker already
// names the successor, and write nothing.
func TestReconcileBlockedIssuesSuccessorNotRepostedWhenBudgetExhausted(t *testing.T) {
	f := newBlockedSuccessorFake()
	f.issues["o/r#100"] = `{"number":100,"state":"closed","state_reason":"duplicate","node_id":"N100","closed_at":"2026-11-10T10:00:00Z"}`
	f.duplicateOf["N100"] = `{"number":101,"repository":{"nameWithOwner":"o/r"}}`
	f.issues["o/r#101"] = `{"number":101,"state":"open"}`
	f.issues["o/r#400"] = `{"number":400,"state":"open"}`
	f.comments["o/r#1"] = []string{fmt.Sprintf(`{"id":1,"body":%q,"user":{"login":%q}}`, blockedSuccessorIssueRetargetNote(blockedSuccessorIssue{repo: "o/r", number: 101, duplicate: true}, "o/r#100"), testBot)}

	c := testClient(t, f.server(t).URL)
	c.appBotLogin = testBot
	var rows []RepoWorkIssue
	for i := 0; i < blockedSweepCommentLookups; i++ {
		rows = append(rows, blockedSuccessorRow(1000+i, "o/r#400"))
	}
	rows = append(rows, blockedSuccessorRow(1, "o/r#100"))
	res := ReconcileBlockedIssues(context.Background(), c, &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows}}}, nil)
	if res.Items != blockedSweepCommentLookups+1 || res.Retargeted != 0 || res.Unblocked != 0 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Items=%d Retargeted=0 Unblocked=0 Errors=0", res, blockedSweepCommentLookups+1)
	}
	if !f.called("GET /repos/o/r/issues/1/comments") {
		t.Fatal("issue 1's comments must be read before re-targeting")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) != 0 || len(f.removed) != 0 {
		t.Fatalf("nothing may be written when the newest marker already names the successor: posts=%v removed=%v", f.posts, f.removed)
	}
}

// With the write-back gate refusing every comment in the repository, the
// hive may not leave the re-target note, so the closed blocker unblocks the
// issue exactly as before rather than holding it on a blocker nothing names.
func TestReconcileBlockedIssuesSuccessorUnblocksAsBeforeWhenPolicyRefusesTheNote(t *testing.T) {
	f := newBlockedSuccessorFake()
	f.issues["o/r#100"] = `{"number":100,"state":"closed","state_reason":"duplicate","node_id":"N100","closed_at":"2026-11-10T10:00:00Z"}`
	f.duplicateOf["N100"] = `{"number":101,"repository":{"nameWithOwner":"o/r"}}`
	f.issues["o/r#101"] = `{"number":101,"state":"open"}`
	f.issues["o/r#1"] = `{"number":1,"state":"open","labels":[{"name":"blocked"}],"user":{"login":"reporter"},"created_at":"2026-11-01T00:00:00Z"}`

	c := testClient(t, f.server(t).URL)
	c.appBotLogin = testBot
	c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: writeback.RepoModeOff}}}, writeback.Deps{}, ""))
	rows := []RepoWorkIssue{blockedSuccessorRow(1, "o/r#100")}
	res := ReconcileBlockedIssues(context.Background(), c, &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows}}}, nil)
	if res.Retargeted != 0 || res.Unblocked != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v, want Retargeted=0 Unblocked=1 Errors=0", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := blockedUnblockedMarker + "\nUnblocked: o/r#100 is closed. This issue is actionable again."
	if got := f.posts["o/r#1"]; len(got) != 1 || got[0] != want {
		t.Fatalf("comments = %q, want exactly the note it always had, %q", got, want)
	}
}

// A statement only counts from someone trusted to make it, and the blocked
// sweeps' own comments are bookkeeping about what an item waits on, never a
// statement that the item was replaced.
func TestBlockedSuccessorTrustAndStatements(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	c.appBotLogin = testBot
	comment := func(login, association string) *gh.IssueComment {
		return &gh.IssueComment{Body: gh.Ptr("Superseded by #9."), User: &gh.User{Login: gh.Ptr(login)}, AuthorAssociation: gh.Ptr(association)}
	}
	for _, tt := range []struct {
		name   string
		cm     *gh.IssueComment
		author string
		want   bool
	}{
		{"the hive's own bot", comment(testBot, "NONE"), "reporter", true},
		{"the closed item's author", comment("Reporter", "NONE"), "reporter", true},
		{"owner", comment("someone", "OWNER"), "reporter", true},
		{"member", comment("someone", "member"), "reporter", true},
		{"collaborator", comment("someone", "COLLABORATOR"), "reporter", true},
		{"contributor", comment("someone", "CONTRIBUTOR"), "reporter", false},
		{"first-time contributor", comment("someone", "FIRST_TIME_CONTRIBUTOR"), "reporter", false},
		{"no association", comment("someone", "NONE"), "reporter", false},
		{"no association and no known author", comment("someone", "NONE"), "", false},
		{"no comment", nil, "reporter", false},
	} {
		if got := c.blockedSuccessorTrustedComment(tt.cm, tt.author); got != tt.want {
			t.Errorf("%s: trusted = %v, want %v", tt.name, got, tt.want)
		}
	}
	for _, tt := range []struct {
		text string
		want int
	}{
		{"Superseded by #12.", 12},
		{"<!-- hive:adopted by=hive mode=supersede superseded_by=14 head=abc -->", 14},
		{BlockedOnMarker("o/r", 12) + "\nRe-targeted: blocker #11 was superseded by #12.", 0},
		{blockedOnIssueMarker("o/r", 12) + "\nRe-targeted: blocker o/r#11 was replaced by o/r#12, which is still open.", 0},
		{blockedUnblockedMarker + "\nUnblocked: o/r#12 (superseding #11) is no longer open. Superseded by #12.", 0},
		{"No successor here.", 0},
	} {
		got, ok := blockedSuccessorStatement(tt.text)
		if got != tt.want || ok != (tt.want != 0) {
			t.Errorf("blockedSuccessorStatement(%q) = (%d, %v), want %d", tt.text, got, ok, tt.want)
		}
	}
	if repo, n, ok := parseBlockedOnIssueMarker(blockedOnIssueMarker("o/other", 7)); !ok || repo != "o/other" || n != 7 {
		t.Errorf("blockedOnIssueMarker does not round-trip: %q %d %v", repo, n, ok)
	}
}

// A read of the successor lookup that GitHub definitely refuses means "no
// successor": the issue is unblocked on the first pass, exactly as before. A
// GraphQL errors array that says rate limit is not a refusal: the block is
// kept and the next pass looks again.
func TestReconcileBlockedIssuesSuccessorLookupRefusalAndRateLimit(t *testing.T) {
	const duplicate = `{"number":%d,"state":"closed","state_reason":"duplicate","node_id":"N%d","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	cases := []struct {
		name        string
		issue       int
		blocker     int
		setup       func(f *blockedSuccessorFake)
		wantUnblock bool
	}{
		{name: "GraphQL answers an errors array of type FORBIDDEN", issue: 11, blocker: 110, wantUnblock: true, setup: func(f *blockedSuccessorFake) {
			f.issues["o/r#110"] = fmt.Sprintf(duplicate, 110, 110)
			f.graphQLReply = `{"data":{"node":null},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`
		}},
		{name: "the closed blocker's comment list answers 404", issue: 13, blocker: 130, wantUnblock: true, setup: func(f *blockedSuccessorFake) {
			f.issues["o/r#130"] = `{"number":130,"state":"closed","state_reason":"completed","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
			f.commentsStatus["o/r#130"] = http.StatusNotFound
		}},
		{name: "GraphQL answers an errors array that says rate limit", issue: 14, blocker: 140, wantUnblock: false, setup: func(f *blockedSuccessorFake) {
			f.issues["o/r#140"] = fmt.Sprintf(duplicate, 140, 140)
			f.graphQLReply = `{"data":{"node":null},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation."}]}`
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blockedSuccessorClock(t)
			f := newBlockedSuccessorFake()
			tc.setup(f)
			c := testClient(t, f.server(t).URL)
			c.appBotLogin = testBot
			blocker := "o/r#" + strconv.Itoa(tc.blocker)
			ref := "o/r#" + strconv.Itoa(tc.issue)
			rows := []RepoWorkIssue{blockedSuccessorRow(tc.issue, blocker)}
			res := ReconcileBlockedIssues(context.Background(), c, &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows}}}, nil)
			f.mu.Lock()
			defer f.mu.Unlock()
			if !tc.wantUnblock {
				if res.Unblocked != 0 || res.Retargeted != 0 || res.Errors != 1 || len(f.posts) != 0 || len(f.removed) != 0 {
					t.Fatalf("a rate limit must keep the block and write nothing: %+v posts=%v removed=%v", res, f.posts, f.removed)
				}
				if !blockedSuccessorLookupRecorded("o/r", tc.issue, blocker) {
					t.Fatal("the failing lookup must go on record so the grace can run out")
				}
				return
			}
			want := blockedUnblockedMarker + "\nUnblocked: " + blocker + " is closed. This issue is actionable again."
			if res.Unblocked != 1 || res.Retargeted != 0 || res.Errors != 0 {
				t.Fatalf("a refused lookup must unblock on the first pass: %+v", res)
			}
			if got := f.posts[ref]; len(got) != 1 || got[0] != want {
				t.Fatalf("comments = %q, want exactly the note it always had, %q", got, want)
			}
			if blockedSuccessorLookupRecorded("o/r", tc.issue, blocker) {
				t.Fatal("a refused lookup must leave nothing on record")
			}
		})
	}
}

// The duplicate-of lookup fails for a reason that may pass (HTTP 502): the
// block is kept, but only within the grace. Once the lookup has failed for
// longer, the issue is unblocked with the note it always had and the give-up
// is logged once.
func TestReconcileBlockedIssuesSuccessorLookupFailureIsBounded(t *testing.T) {
	advance := blockedSuccessorClock(t)
	f := newBlockedSuccessorFake()
	f.issues["o/r#120"] = `{"number":120,"state":"closed","state_reason":"duplicate","node_id":"N120","closed_at":"2026-11-10T10:00:00Z","user":{"login":"reporter"}}`
	f.graphQLStatus = http.StatusBadGateway
	c := testClient(t, f.server(t).URL)
	c.appBotLogin = testBot
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rows := []RepoWorkIssue{blockedSuccessorRow(12, "o/r#120")}
	sweep := func() BlockedIssueResult {
		return ReconcileBlockedIssues(context.Background(), c, &ActionableResult{WorkDetailsByRepo: map[string]RepoWorkDetails{"o/r": {Issues: rows}}}, logger)
	}

	for pass, wait := range []time.Duration{0, 30 * time.Minute} {
		advance(wait)
		res := sweep()
		f.mu.Lock()
		wrote := len(f.posts) + len(f.removed)
		f.mu.Unlock()
		if res.Errors != 1 || res.Unblocked != 0 || res.Retargeted != 0 || wrote != 0 {
			t.Fatalf("pass %d inside the grace must keep the block and write nothing: %+v wrote=%d", pass+1, res, wrote)
		}
	}
	if !blockedSuccessorLookupRecorded("o/r", 12, "o/r#120") || strings.Contains(logs.String(), blockedSuccessorGaveUpLog) {
		t.Fatalf("inside the grace the failure is on record and nothing is given up: %s", logs.String())
	}

	advance(blockedSuccessorLookupGrace)
	res := sweep()
	if res.Unblocked != 1 || res.Retargeted != 0 {
		t.Fatalf("past the grace the issue must be unblocked as before: %+v", res)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	want := blockedUnblockedMarker + "\nUnblocked: o/r#120 is closed. This issue is actionable again."
	if got := f.posts["o/r#12"]; len(got) != 1 || got[0] != want {
		t.Fatalf("comments = %q, want exactly the note it always had, %q", got, want)
	}
	if got := strings.Count(logs.String(), blockedSuccessorGaveUpLog); got != 1 {
		t.Fatalf("the give-up must be logged once, got %d in:\n%s", got, logs.String())
	}
	if blockedSuccessorLookupRecorded("o/r", 12, "o/r#120") {
		t.Fatal("the record must go with the released block")
	}
}

// What counts as GitHub definitely refusing a read (release at once) and
// what counts as a failure that may pass (keep the hold within the grace).
func TestBlockedSuccessorRefused(t *testing.T) {
	rest := func(status int) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status, Request: httptest.NewRequest(http.MethodGet, "/repos/o/r/issues/1/comments", nil)}}
	}
	graphQL := func(typ, msg string) error {
		return &graphQLErrors{Errors: []graphQLError{{Type: typ, Message: msg}}}
	}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"REST 404", rest(http.StatusNotFound), true},
		{"REST 404, wrapped", fmt.Errorf("listing comments: %w", rest(http.StatusNotFound)), true},
		{"REST 410", rest(http.StatusGone), true},
		{"REST 403", rest(http.StatusForbidden), true},
		{"REST 500", rest(http.StatusInternalServerError), false},
		{"REST 502", rest(http.StatusBadGateway), false},
		{"primary rate limit", &gh.RateLimitError{}, false},
		{"secondary rate limit", &gh.AbuseRateLimitError{}, false},
		{"deadline", context.DeadlineExceeded, false},
		{"invalid repository reference", &invalidRepoRefError{msg: "bad"}, true},
		{"GraphQL FORBIDDEN", graphQL("FORBIDDEN", "Resource not accessible by integration"), true},
		{"GraphQL NOT_FOUND", graphQL("NOT_FOUND", "Could not resolve to a node"), true},
		{"GraphQL untyped error", graphQL("", "Something went wrong while executing your query"), true},
		{"GraphQL RATE_LIMITED", graphQL("RATE_LIMITED", "API rate limit exceeded"), false},
		{"GraphQL rate limit in the message only", graphQL("", "You have exceeded a secondary rate limit"), false},
		{"GraphQL refusal beside a rate limit", &graphQLErrors{Errors: []graphQLError{{Type: "FORBIDDEN"}, {Type: "RATE_LIMITED"}}}, false},
	} {
		if got := blockedSuccessorRefused(tt.err); got != tt.want {
			t.Errorf("%s: refused = %v, want %v", tt.name, got, tt.want)
		}
	}
}
