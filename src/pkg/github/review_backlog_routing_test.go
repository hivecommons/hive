package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

func TestReviewFindingFingerprintDeterministicAndNormalised(t *testing.T) {
	base := outputschema.Finding{Title: "Unchecked error", File: "pkg/a.go", Line: 10, Summary: "The error from Close is dropped."}
	fp := ReviewFindingFingerprint(base)
	if len(fp) != 64 {
		t.Fatalf("fingerprint %q is not a sha256 hex digest", fp)
	}
	if again := ReviewFindingFingerprint(base); again != fp {
		t.Fatalf("fingerprint is not deterministic: %q != %q", again, fp)
	}
	same := []outputschema.Finding{
		{Title: "  unchecked   ERROR ", File: "pkg/a.go", Line: 10, Summary: "The error from Close is dropped."},
		{Title: "Unchecked error", File: "./pkg/a.go", Line: 10, Summary: "the error\n\tfrom close   is dropped."},
		{Title: "Unchecked error", File: "pkg//a.go", Line: 99, Summary: "The error from Close is dropped."},
		{Title: "Unchecked error", File: "pkg\\a.go", Line: 10, Summary: "The error from Close is dropped.", Severity: outputschema.SeverityHigh},
	}
	for i, f := range same {
		if got := ReviewFindingFingerprint(f); got != fp {
			t.Errorf("variant %d: fingerprint %q, want %q", i, got, fp)
		}
	}
	different := []outputschema.Finding{
		{Title: "Other rule", File: "pkg/a.go", Line: 10, Summary: "The error from Close is dropped."},
		{Title: "Unchecked error", File: "pkg/b.go", Line: 10, Summary: "The error from Close is dropped."},
		{Title: "Unchecked error", File: "pkg/a.go", Line: 10, Summary: "The error from Open is dropped."},
	}
	for i, f := range different {
		if got := ReviewFindingFingerprint(f); got == fp {
			t.Errorf("different finding %d collided with the base fingerprint", i)
		}
	}
}

func TestReviewFindingBelowLine(t *testing.T) {
	cases := []struct {
		sev     outputschema.Severity
		blockAt string
		want    bool
	}{
		{outputschema.SeverityCritical, "P1", false},
		{outputschema.SeverityHigh, "P1", false},
		{outputschema.SeverityMedium, "P1", true},
		{outputschema.SeverityLow, "p1", true},
		{outputschema.SeverityInfo, "P1", true},
		{outputschema.SeverityMedium, "P2", false},
		{outputschema.SeverityLow, "P2", true},
		{outputschema.SeverityLow, "P3", false},
		{outputschema.SeverityLow, "", false},
		{outputschema.SeverityLow, "P9", false},
	}
	for _, tc := range cases {
		if got := ReviewFindingBelowLine(outputschema.Finding{Severity: tc.sev}, tc.blockAt); got != tc.want {
			t.Errorf("%s below %q = %v, want %v", tc.sev, tc.blockAt, got, tc.want)
		}
	}
}

func TestReviewBacklogTargetFallsBackToGitHubWithReviewerAgentLabel(t *testing.T) {
	c := &Client{}
	sink, labels := c.reviewBacklogTarget(ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: "jira", Labels: []string{"nit"}}})
	if sink.Destination() != config.ReviewBacklogGitHubIssue {
		t.Fatalf("destination = %q, want the GitHub issue fallback", sink.Destination())
	}
	if strings.Join(labels, ",") != "nit,agent/reviewer" {
		t.Fatalf("labels = %v, want [nit agent/reviewer]", labels)
	}
	fake := &fakeBacklogSink{dest: config.ReviewBacklogJira}
	sink, labels = c.reviewBacklogTarget(ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: "jira", Labels: []string{"nit"}}, Sink: fake})
	if sink != fake || strings.Join(labels, ",") != "nit" {
		t.Fatalf("configured sink: got %v %v, want the jira sink with [nit]", sink.Destination(), labels)
	}
	_, labels = c.reviewBacklogTarget(ReviewBacklogRouting{})
	if strings.Join(labels, ",") != "agent/reviewer" {
		t.Fatalf("default labels = %v, want [agent/reviewer]", labels)
	}
}

func TestReviewBacklogDailyCapAndPrune(t *testing.T) {
	if got := reviewBacklogDailyCap(ReviewBacklogRouting{}, false, 0); got != DefaultReviewBacklogIssueCap {
		t.Fatalf("unrouted cap = %d, want legacy default %d", got, DefaultReviewBacklogIssueCap)
	}
	if got := reviewBacklogDailyCap(ReviewBacklogRouting{}, false, 5); got != 5 {
		t.Fatalf("unrouted cap = %d, want legacy 5", got)
	}
	if got := reviewBacklogDailyCap(ReviewBacklogRouting{}, true, 5); got != config.DefaultReviewBacklogMaxPerPRPerDay {
		t.Fatalf("routed default cap = %d, want %d", got, config.DefaultReviewBacklogMaxPerPRPerDay)
	}
	if got := reviewBacklogDailyCap(ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{MaxPerPRPerDay: 4}}, true, 5); got != 4 {
		t.Fatalf("routed cap = %d, want 4", got)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	l := newReviewBacklogLedger()
	l.PRDailyCount["o/r#5@2026-10-01"] = 3
	l.PRDailyCount["o/r#5@2026-10-08"] = 2
	l.PRDailyCount[reviewBacklogDayKey("o/r#5", now)] = 1
	l.PRDailyCount["garbage"] = 1
	l.pruneDailyCounts(now)
	if len(l.PRDailyCount) != 2 || l.PRDailyCount["o/r#5@2026-10-08"] != 2 || l.PRDailyCount["o/r#5@2026-10-09"] != 1 {
		t.Fatalf("pruned counts = %v, want yesterday and today only", l.PRDailyCount)
	}
}

func TestGitHubIssueBacklogSinkAppendNeedsNumber(t *testing.T) {
	err := (&Client{}).ReviewBacklogIssueSink().AppendToItem(context.Background(), "o/r", ReviewBacklogRef{ID: "x"}, "hi")
	if err == nil {
		t.Fatal("append without an issue number must fail")
	}
}

type fakeBacklogAppend struct {
	ref     ReviewBacklogRef
	comment string
}

type fakeBacklogSink struct {
	dest    string
	created []ReviewBacklogItem
	appends []fakeBacklogAppend
	failAt  int // 1-based create that fails; 0 = never
	partial bool
	reuse   bool // every create reports an existing open twin (Reused)
	// nothing makes every create report nothing filed: an empty ref and
	// no error, as the github_issue sink does for a rejected twin.
	nothing bool
}

func (f *fakeBacklogSink) Destination() string { return f.dest }

func (f *fakeBacklogSink) CreateItem(_ context.Context, item ReviewBacklogItem) (ReviewBacklogRef, error) {
	f.created = append(f.created, item)
	if f.nothing {
		return ReviewBacklogRef{}, nil
	}
	n := len(f.created)
	ref := ReviewBacklogRef{ID: fmt.Sprintf("id-%d", n), Key: fmt.Sprintf("ENG-%d", n), URL: fmt.Sprintf("https://tracker.test/ENG-%d", n), Reused: f.reuse}
	if f.failAt == n {
		if f.partial {
			return ref, errors.New("set status failed")
		}
		return ReviewBacklogRef{}, errors.New("create failed")
	}
	return ref, nil
}

func (f *fakeBacklogSink) AppendToItem(_ context.Context, _ string, ref ReviewBacklogRef, comment string) error {
	f.appends = append(f.appends, fakeBacklogAppend{ref: ref, comment: comment})
	return nil
}

// withReviewBacklogState isolates the backlog ledger and dispatch state and
// authorises reviewer verdicts for o/r PRs 5 and 6.
func withReviewBacklogState(t *testing.T) {
	t.Helper()
	stateDir := t.TempDir()
	oldBacklog, oldLinks, oldReportDir := ReviewBacklogPath, ReviewLinksPath, review.DefaultReportDir
	ReviewBacklogPath = filepath.Join(stateDir, reviewBacklogFile)
	ReviewLinksPath = filepath.Join(stateDir, ReviewLinksFile)
	review.DefaultReportDir = filepath.Join(stateDir, "reports")
	t.Cleanup(func() {
		ReviewBacklogPath, ReviewLinksPath, review.DefaultReportDir = oldBacklog, oldLinks, oldReportDir
	})
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		{Repo: "o/r", Number: 5, HeadSHA: "abc", Perspective: review.PerspectiveCorrectness, Agent: "reviewer"},
		{Repo: "o/r", Number: 6, HeadSHA: "def", Perspective: review.PerspectiveCorrectness, Agent: "reviewer"},
	}})
}

func routingReport(t *testing.T, number int, head string, findings ...map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"lane":        "review-swarm",
		"kind":        "review",
		"perspective": "correctness",
		"verdict":     "changes_requested",
		"repo":        "o/r",
		"number":      number,
		"head_sha":    head,
		"summary":     "review with nits",
		"findings":    findings,
		"prs_opened":  []any{},
		"beads_filed": []any{},
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return string(raw)
}

func nit(title, sev, file string, line int, summary string) map[string]any {
	return map[string]any{"title": title, "severity": sev, "summary": summary, "file": file, "line": line, "review_scope": "in-scope"}
}

// prCommentServer accepts PR summary comments and counts them per PR.
func prCommentServer(t *testing.T, bodies map[int][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var n int
		if r.Method == http.MethodPost {
			if _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d/comments", &n); err == nil {
				var payload struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				bodies[n] = append(bodies[n], payload.Body)
				_, _ = io.WriteString(w, `{"id":9}`)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReviewBacklogRoutesBelowLineFindingsWithDedupAndDailyCap(t *testing.T) {
	withReviewBacklogState(t)
	comments := map[int][]string{}
	srv := prCommentServer(t, comments)
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklog(func() (bool, int) { return true, 3 })
	sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, Labels: []string{"nit"}, MaxPerPRPerDay: 2},
			Sink:     sink,
		}
	})
	recs := captureAudit(c)
	ctx := context.Background()
	day1 := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

	pr5 := routingReport(t, 5, "abc",
		nit("unused variable", "medium", "pkg/a.go", 7, "x is assigned and never read"),
		nit("blocking bug", "high", "pkg/a.go", 9, "this blocks and must not be filed"),
		nit("typo in comment", "low", "pkg/b.go", 3, "receive should be receive"),
		nit("naming", "info", "pkg/c.go", 4, "prefer camelCase"),
		map[string]any{"title": "no evidence", "severity": "low", "summary": "uncited", "review_scope": "in-scope"},
	)
	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: pr5}, day1); err != nil {
		t.Fatalf("file PR 5: %v", err)
	}
	if len(sink.created) != 2 {
		t.Fatalf("created = %d, want 2 (daily cap) below-line items", len(sink.created))
	}
	first := sink.created[0]
	if first.Repo != "o/r" || first.Title != "[reviewer] Review backlog (correctness): unused variable" || strings.Join(first.Labels, ",") != "nit,blocked" {
		t.Fatalf("first item = %+v", first)
	}
	for _, want := range []string{
		"PR: #5 (https://github.com/o/r/pull/5)",
		"Severity: medium (P2)",
		"Finding: unused variable\nEvidence: `pkg/a.go:7`\nhttps://github.com/o/r/blob/abc/pkg/a.go#L7\n",
		"below the blocking line (P1)",
		"x is assigned and never read",
		"Fingerprint: `" + ReviewFindingFingerprint(outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Summary: "x is assigned and never read"}) + "`",
		"filed by Hive review backlog",
	} {
		if !strings.Contains(first.Body, want) {
			t.Errorf("item body missing %q:\n%s", want, first.Body)
		}
	}
	if strings.Index(first.Body, "Finding:") > strings.Index(first.Body, "Evidence:") || strings.Contains(first.Body, "(https://github.com/o/r/blob") {
		t.Errorf("body must put Finding before Evidence with an unparenthesised link:\n%s", first.Body)
	}
	for _, item := range sink.created {
		if strings.Contains(item.Title, "blocking bug") || strings.Contains(item.Title, "no evidence") {
			t.Errorf("filed a blocking or uncited finding: %q", item.Title)
		}
	}
	if len(comments[5]) != 1 || !strings.Contains(comments[5][0], "ENG-1") || !strings.Contains(comments[5][0], "ENG-2") {
		t.Fatalf("PR 5 summary comments = %v, want one listing ENG-1 and ENG-2", comments[5])
	}
	batch := assertTypedWrite(t, *recs, AuditActionReviewBacklogBatchRouted, "o/r", 5, 1)
	for _, want := range []string{"destination=linear", "filed=2", "updated=0", "capped=1"} {
		if !strings.Contains(batch.Detail, want) {
			t.Errorf("batch detail %q missing %q", batch.Detail, want)
		}
	}
	assertTypedWrite(t, *recs, AuditActionReviewBacklogIssueFiled, "", 0, 0)

	// PR 6 re-raises the first finding: reflowed text, other line, other head.
	pr6 := routingReport(t, 6, "def",
		nit("Unused  Variable", "medium", "./pkg/a.go", 42, "x is assigned\nand never read"),
	)
	req6 := ReviewRequest{Repo: "o/r", Number: 6, Agent: "reviewer", Report: pr6}
	for i := 0; i < 2; i++ {
		if err := c.fileOutOfScopeReviewBacklog(ctx, req6, day1); err != nil {
			t.Fatalf("file PR 6 pass %d: %v", i, err)
		}
	}
	if len(sink.created) != 2 {
		t.Fatalf("re-raise created a new item: %d items", len(sink.created))
	}
	if len(sink.appends) != 1 {
		t.Fatalf("appends = %d, want one update of the existing item", len(sink.appends))
	}
	up := sink.appends[0]
	if up.ref.ID != "id-1" || up.ref.Key != "ENG-1" {
		t.Fatalf("updated %+v, want ENG-1", up.ref)
	}
	for _, want := range []string{"PR #6 (https://github.com/o/r/pull/6)", "https://github.com/o/r/blob/def/pkg/a.go#L42", "filed by Hive review backlog", reviewBacklogReRaiseMarker} {
		if !strings.Contains(up.comment, want) {
			t.Errorf("re-raise comment missing %q:\n%s", want, up.comment)
		}
	}
	if len(comments[6]) != 0 {
		t.Fatalf("PR 6 got a summary comment without new items: %v", comments[6])
	}
	ledger, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := ledger.fingerprint("O/R", ReviewFindingFingerprint(outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Summary: "x is assigned and never read"}))
	if !ok || fmt.Sprint(rec.PRs) != "[5 6]" || rec.Destination != config.ReviewBacklogLinear || rec.ItemKey != "ENG-1" {
		t.Fatalf("fingerprint record = %+v ok=%v, want PRs [5 6] on linear ENG-1", rec, ok)
	}

	// Same day, PR 5 again: still capped. Next day: the capped finding files.
	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: pr5}, day1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 2 {
		t.Fatalf("same-day re-review created past the cap: %d", len(sink.created))
	}
	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: pr5}, day1.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 3 || !strings.Contains(sink.created[2].Title, "naming") {
		t.Fatalf("next day: created = %d, want the capped finding filed", len(sink.created))
	}
	if len(comments[5]) != 1 {
		t.Fatalf("PR 5 summary comments = %d, want still one", len(comments[5]))
	}
}

func TestReviewBacklogNoBlockingLineLeavesInScopeFindingsAlone(t *testing.T) {
	withReviewBacklogState(t)
	c := reviewTestClient(t, prCommentServer(t, map[int][]string{}).URL)
	sink := &fakeBacklogSink{dest: config.ReviewBacklogGitHubIssue}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting { return ReviewBacklogRouting{Sink: sink} })
	report := routingReport(t, 5, "abc", nit("nit", "low", "pkg/a.go", 1, "minor"))
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
		t.Fatal(err)
	}
	backlogBelow := false
	c.SetReviewBacklog(func() (bool, int) { return false, 3 })
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{Severity: config.ReviewSeverityConfig{BlockAt: "P1", BacklogBelow: &backlogBelow}, Sink: sink}
	})
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 0 {
		t.Fatalf("filed %d in-scope findings without an enabled blocking line", len(sink.created))
	}
}

func TestReviewBacklogCreateErrors(t *testing.T) {
	withReviewBacklogState(t)
	c := reviewTestClient(t, prCommentServer(t, map[int][]string{}).URL)
	sink := &fakeBacklogSink{dest: config.ReviewBacklogJira, failAt: 1}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogJira},
			Sink:     sink,
		}
	})
	req := ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: routingReport(t, 5, "abc", nit("nit", "low", "pkg/a.go", 1, "minor"))}
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), req, time.Now()); err == nil {
		t.Fatal("create failure must be returned")
	}
	// A failure after the item exists is returned but recorded, so the
	// retry does not file it twice.
	sink.failAt, sink.partial = 2, true
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), req, time.Now()); err == nil || !strings.Contains(err.Error(), "set status") {
		t.Fatalf("partial failure err = %v", err)
	}
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), req, time.Now()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(sink.created) != 2 {
		t.Fatalf("created = %d, want 2 (failed, then partial; no third on retry)", len(sink.created))
	}
}

func TestReviewBacklogGitHubIssueDestinationOverHTTP(t *testing.T) {
	withReviewBacklogState(t)
	var issueBodies []string
	var issueLabels [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_, _ = io.WriteString(w, `[]`)
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
			issueBodies = append(issueBodies, payload.Body)
			issueLabels = append(issueLabels, payload.Labels)
			n := 100 + len(issueBodies)
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
		// linear is not active, so the fallback GitHub issue sink is used.
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, Labels: []string{"nit"}},
		}
	})
	recs := captureAudit(c)
	report := routingReport(t, 5, "abc", nit("nit", "low", "pkg/a.go", 3, "minor"), nit("real", "medium", "pkg/a.go", 4, "blocks"))
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(issueBodies) != 1 {
		t.Fatalf("issues = %d, want 1 (the P3 nit only)", len(issueBodies))
	}
	if strings.Join(issueLabels[0], ",") != "nit,agent/reviewer,blocked" {
		t.Fatalf("labels = %v, want nit, blocked (#13833) and the agent/reviewer fallback label", issueLabels[0])
	}
	if !strings.Contains(issueBodies[0], "https://github.com/o/r/blob/abc/pkg/a.go#L3") || !strings.Contains(issueBodies[0], "filed by Hive review backlog") {
		t.Fatalf("issue body = %q", issueBodies[0])
	}
	rec := assertTypedWrite(t, *recs, AuditActionReviewBacklogIssueFiled, "o/r", 101, 1)
	if !strings.Contains(rec.Detail, "destination=github_issue") {
		t.Errorf("issue audit detail %q missing destination", rec.Detail)
	}
	assertTypedWrite(t, *recs, AuditActionReviewBacklogBatchRouted, "o/r", 5, 1)
}

type stateBacklogSink struct {
	fakeBacklogSink
	closed map[string]bool
	err    error // returned by every ItemOpen probe while non-nil
	probes int
}

func (s *stateBacklogSink) ItemOpen(_ context.Context, _ string, ref ReviewBacklogRef) (bool, error) {
	s.probes++
	if s.err != nil {
		return false, s.err
	}
	return !s.closed[ref.ID], nil
}

// A finding re-raised after its backlog item was closed (the ChairLift-pin
// shape in projectbluefin/common#1445) is re-filed, not appended to the
// closed item.
func TestReviewBacklogRefilesWhenLedgerItemClosed(t *testing.T) {
	withReviewBacklogState(t)
	srv := prCommentServer(t, map[int][]string{})
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklog(func() (bool, int) { return true, 3 })
	sink := &stateBacklogSink{fakeBacklogSink: fakeBacklogSink{dest: config.ReviewBacklogLinear}, closed: map[string]bool{}}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, MaxPerPRPerDay: 5},
			Sink:     sink,
		}
	})
	ctx := context.Background()
	day := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	finding := nit("ChairLift pin stale", "medium", "pkg/pin.go", 7, "ChairLift pin is out of date")

	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: routingReport(t, 5, "abc", finding)}, day); err != nil {
		t.Fatal(err)
	}
	sink.closed["id-1"] = true
	req6 := ReviewRequest{Repo: "o/r", Number: 6, Agent: "reviewer", Report: routingReport(t, 6, "def", finding)}
	if err := c.fileOutOfScopeReviewBacklog(ctx, req6, day); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 2 || len(sink.appends) != 0 {
		t.Fatalf("closed item: created=%d appends=%d, want re-file (2) and no append", len(sink.created), len(sink.appends))
	}
	ledger, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := ledger.fingerprint("o/r", ReviewFindingFingerprint(outputschema.Finding{Title: "ChairLift pin stale", File: "pkg/pin.go", Summary: "ChairLift pin is out of date"}))
	if !ok || rec.ItemID != "id-2" {
		t.Fatalf("ledger record = %+v ok=%v, want id-2", rec, ok)
	}
}

// The state probe is fail-safe and narrow: a lookup error keeps the old
// append path, and a re-raise from the PR that already filed the finding
// never probes (nothing to append, nothing to re-file).
func TestReviewBacklogStateProbeErrorAppendsAndSamePRSkipsProbe(t *testing.T) {
	withReviewBacklogState(t)
	srv := prCommentServer(t, map[int][]string{})
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklog(func() (bool, int) { return true, 3 })
	sink := &stateBacklogSink{fakeBacklogSink: fakeBacklogSink{dest: config.ReviewBacklogLinear}, closed: map[string]bool{}}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, MaxPerPRPerDay: 5},
			Sink:     sink,
		}
	})
	ctx := context.Background()
	day := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	finding := nit("ChairLift pin stale", "medium", "pkg/pin.go", 7, "ChairLift pin is out of date")
	req5 := ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: routingReport(t, 5, "abc", finding)}
	req6 := ReviewRequest{Repo: "o/r", Number: 6, Agent: "reviewer", Report: routingReport(t, 6, "def", finding)}

	if err := c.fileOutOfScopeReviewBacklog(ctx, req5, day); err != nil {
		t.Fatal(err)
	}
	if sink.probes != 0 {
		t.Fatalf("first filing probed %d times, want 0 (fingerprint unknown)", sink.probes)
	}

	sink.err = errors.New("tracker unavailable")
	if err := c.fileOutOfScopeReviewBacklog(ctx, req6, day); err != nil {
		t.Fatal(err)
	}
	if sink.probes != 1 || len(sink.created) != 1 || len(sink.appends) != 1 {
		t.Fatalf("probe error: probes=%d created=%d appends=%d, want 1/1/1 (append path kept)", sink.probes, len(sink.created), len(sink.appends))
	}

	sink.err = nil
	sink.closed["id-1"] = true
	if err := c.fileOutOfScopeReviewBacklog(ctx, req5, day.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if sink.probes != 1 || len(sink.created) != 1 || len(sink.appends) != 1 {
		t.Fatalf("same-PR re-raise: probes=%d created=%d appends=%d, want no probe, no create, no append", sink.probes, len(sink.created), len(sink.appends))
	}
}

// githubIssueBacklogSink.ItemOpen: no number is an error (a non-GitHub ID
// landed in a GitHub ledger record), the issue state decides, and an API
// failure is returned rather than guessed.
func TestGitHubIssueBacklogSinkItemOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_, _ = io.WriteString(w, `{"number":7,"state":"closed"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/8":
			_, _ = io.WriteString(w, `{"number":8,"state":"open"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
		}
	}))
	defer srv.Close()
	sink := githubIssueBacklogSink{c: reviewTestClient(t, srv.URL)}
	ctx := context.Background()

	if _, err := sink.ItemOpen(ctx, "o/r", ReviewBacklogRef{ID: "ENG-1"}); err == nil || !strings.Contains(err.Error(), "no issue number") {
		t.Fatalf("ref without number: err = %v, want no-issue-number error", err)
	}
	if open, err := sink.ItemOpen(ctx, "o/r", ReviewBacklogRef{ID: "7", Number: 7}); err != nil || open {
		t.Fatalf("closed issue: open=%v err=%v, want false/nil", open, err)
	}
	if open, err := sink.ItemOpen(ctx, "o/r", ReviewBacklogRef{ID: "8", Number: 8}); err != nil || !open {
		t.Fatalf("open issue: open=%v err=%v, want true/nil", open, err)
	}
	if _, err := sink.ItemOpen(ctx, "o/r", ReviewBacklogRef{ID: "9", Number: 9}); err == nil {
		t.Fatal("API failure must be returned, not treated as a state")
	}
	if _, err := (githubIssueBacklogSink{c: &Client{}}).ItemOpen(ctx, "o/r", ReviewBacklogRef{ID: "8", Number: 8}); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: err = %v, want ErrNoGitHubClient", err)
	}
}

// The #11578 shape, narrowed by #13906: a review finding that cites the same
// file and nearby line, in the same category, as an open App-bot issue folds
// into that issue instead of becoming a second issue. The twin is not titled
// "Review backlog (…)", so it is reached as a file-set twin, not a canonical
// subject match. The ledger records the twin's number so later re-raises
// append there.
func TestReviewBacklogGitHubSinkFoldsIntoOpenAgentTwin(t *testing.T) {
	withReviewBacklogState(t)
	var created, labelled int
	var commentedOn []int
	var twinComments []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			if r.URL.Query().Get("state") != "open" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[{"number":41,"title":"[scanner] unchecked Close error in pkg/a.go","body":"Perspective: correctness\nEvidence: `+"`pkg/a.go:5`"+` drops the Close error","state":"open","html_url":"https://github.example/o/r/issues/41","user":{"login":"`+testBot+`"}}]`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/41/labels":
			labelled++
			_, _ = io.WriteString(w, `[{"name":"nit"}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var n int
			_, _ = fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d/comments", &n)
			commentedOn = append(commentedOn, n)
			if n == 41 {
				var payload struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				twinComments = append(twinComments, payload.Body)
			}
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			created++
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue, Labels: []string{"nit"}},
		}
	})
	finding := nit("nit", "low", "pkg/a.go", 3, "minor")
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: routingReport(t, 5, "abc", finding)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("created %d issues, want 0 (folded into the open agent twin)", created)
	}
	// The twin gets the consolidation comment carrying the finding, then the
	// reused-item link comment, which also carries the finding (#13604); the
	// PR gets its summary comment.
	if len(twinComments) != 2 || !strings.Contains(twinComments[0], "**Consolidated finding.**") || !strings.Contains(twinComments[0], "`pkg/a.go`") || !strings.HasPrefix(twinComments[1], reviewBacklogLinkMarker) {
		t.Fatalf("comments on #41 = %q (all comments %v), want consolidation then link", twinComments, commentedOn)
	}
	for _, want := range []string{"Additional finding (folded in", "Review backlog (correctness): nit", "`pkg/a.go:3`", "minor"} {
		if !strings.Contains(twinComments[1], want) {
			t.Errorf("link comment on #41 missing %q:\n%s", want, twinComments[1])
		}
	}
	if labelled != 1 {
		t.Fatalf("labelled = %d, want the backlog labels ensured on #41", labelled)
	}
	ledger, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := ledger.fingerprint("o/r", ReviewFindingFingerprint(outputschema.Finding{Title: "nit", File: "pkg/a.go", Summary: "minor"}))
	if !ok || rec.ItemNumber != 41 {
		t.Fatalf("ledger record = %+v ok=%v, want the twin #41", rec, ok)
	}
}

// hivecommons/hive#13604: a finding the sink folds into an existing open
// twin must land there in full — title, file, summary, PR — under the
// hive:review-backlog-link marker, and the PR summary must say the item was
// folded rather than imply the twin carries this finding's title. Repeating
// the same finding key appends nothing; the same fingerprint from another PR
// still takes the short re-raise path.
func TestReviewBacklogFoldedTwinKeepsFullFinding(t *testing.T) {
	withReviewBacklogState(t)
	comments := map[int][]string{}
	srv := prCommentServer(t, comments)
	c := reviewTestClient(t, srv.URL)
	c.SetReviewBacklog(func() (bool, int) { return true, 3 })
	sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear, reuse: true}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, Labels: []string{"nit"}, MaxPerPRPerDay: 5},
			Sink:     sink,
		}
	})
	ctx := context.Background()
	day1 := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	finding := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is assigned and never read"}
	pr5 := routingReport(t, 5, "abc", nit(finding.Title, "medium", finding.File, finding.Line, finding.Summary))
	req5 := ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: pr5}
	for i := 0; i < 2; i++ {
		if err := c.fileOutOfScopeReviewBacklog(ctx, req5, day1); err != nil {
			t.Fatalf("file PR 5 pass %d: %v", i, err)
		}
	}
	if len(sink.created) != 1 || len(sink.appends) != 1 {
		t.Fatalf("created=%d appends=%d, want one create folded into a twin and one comment (second pass is a no-op)", len(sink.created), len(sink.appends))
	}
	got := sink.appends[0]
	if got.ref.Key != "ENG-1" || !got.ref.Reused {
		t.Fatalf("append ref = %+v, want the reused ENG-1", got.ref)
	}
	key := reviewBacklogKey("o/r", 5, review.PerspectiveCorrectness, finding)
	fp := ReviewFindingFingerprint(finding)
	if !strings.HasPrefix(got.comment, reviewBacklogLinkMarker+key+" -->\n") {
		t.Fatalf("folded comment does not start with the link marker for the finding key:\n%s", got.comment)
	}
	for _, want := range []string{
		"**Additional finding (folded in — same finding area and category).**",
		"it is a correctness finding at the same symbol or lines of `pkg/a.go`",
		"PR #5 (https://github.com/o/r/pull/5)",
		"### [reviewer] Review backlog (correctness): unused variable",
		"- [ ] unused variable (`pkg/a.go:7`)",
		"Perspective: correctness",
		"Severity: medium (P2)",
		"Finding: unused variable\nEvidence: `pkg/a.go:7`\nhttps://github.com/o/r/blob/abc/pkg/a.go#L7\n",
		"x is assigned and never read",
		"Fingerprint: `" + fp + "`",
		reviewBacklogFooter,
	} {
		if !strings.Contains(got.comment, want) {
			t.Errorf("folded comment missing %q:\n%s", want, got.comment)
		}
	}
	if strings.Contains(got.comment, "Linked again") {
		t.Errorf("folded comment still uses the bare link line:\n%s", got.comment)
	}
	if len(comments[5]) != 1 || !strings.Contains(comments[5][0], "- ENG-1 (folded into an existing issue) — [reviewer] Review backlog (correctness): unused variable (https://tracker.test/ENG-1)") {
		t.Fatalf("PR 5 summary comments = %q, want one line saying ENG-1 was folded", comments[5])
	}

	// Another PR raising the same fingerprint takes the re-raise path as
	// before: no second create, one short re-raise comment on the twin.
	pr6 := routingReport(t, 6, "def", nit("Unused  Variable", "medium", "./pkg/a.go", 42, "x is assigned\nand never read"))
	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 6, Agent: "reviewer", Report: pr6}, day1); err != nil {
		t.Fatalf("file PR 6: %v", err)
	}
	if len(sink.created) != 1 || len(sink.appends) != 2 {
		t.Fatalf("after re-raise created=%d appends=%d, want 1 and 2", len(sink.created), len(sink.appends))
	}
	reraise := sink.appends[1].comment
	if !strings.HasPrefix(reraise, reviewBacklogReRaiseMarker+fp+" -->\n") || strings.Contains(reraise, "Additional finding") || strings.Contains(reraise, "- [ ]") {
		t.Fatalf("re-raise comment changed shape:\n%s", reraise)
	}
}

func TestReviewBacklogSummaryCommentMarksFoldedItems(t *testing.T) {
	got := reviewBacklogSummaryComment([]reviewBacklogFiled{
		{number: 7, key: "#7", url: "https://github.example/o/r/issues/7", title: "Review backlog (correctness): a"},
		{number: 9, key: "#9", url: "https://github.example/o/r/issues/9", title: "Review backlog (correctness): b", folded: true},
		{key: "ENG-3", title: "Review backlog (security): c", folded: true},
	}, 10)
	for _, want := range []string{
		"- #7 — Review backlog (correctness): a (https://github.example/o/r/issues/7)\n",
		"- #9 (folded into an existing issue) — Review backlog (correctness): b (https://github.example/o/r/issues/9)\n",
		"- ENG-3 (folded into an existing issue) — Review backlog (security): c\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

// hivecommons/hive#13364: a finding whose evidence file exists only on the
// reviewed PR is filed `blocked`, but when it folds into an already-open
// same-titled twin the body (and its blocked-on marker) is discarded. The
// twin's own state governs: `blocked` is not applied to it.
func TestReviewBacklogGitHubSinkReusedTwinDoesNotInheritBlocked(t *testing.T) {
	withReviewBacklogState(t)
	var created int
	var twinLabels [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			if r.URL.Query().Get("state") != "open" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, `[{"number":41,"title":"Review backlog (correctness): nit","body":"pkg/new.go:3 earlier evidence, on the base branch","state":"open","html_url":"https://github.example/o/r/issues/41","labels":[{"name":"nit"},{"name":"help wanted"}],"user":{"login":"`+testBot+`"}}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/5":
			_, _ = io.WriteString(w, `{"number":5,"state":"open","base":{"ref":"v5"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/contents/pkg/new.go":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/41/labels":
			var payload []string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			twinLabels = append(twinLabels, payload)
			_, _ = io.WriteString(w, `[{"name":"nit"}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, `{"id":1}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			created++
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue, Labels: []string{"nit", "help wanted"}},
		}
	})
	recs := captureAudit(c)
	finding := nit("nit", "low", "pkg/new.go", 3, "introduced by the PR")
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: routingReport(t, 5, "abc", finding)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("created %d issues, want 0 (folded into the open twin)", created)
	}
	if len(twinLabels) != 1 {
		t.Fatalf("label calls on #41 = %v, want exactly one", twinLabels)
	}
	for _, l := range twinLabels[0] {
		if strings.EqualFold(l, BlockedLabel) {
			t.Fatalf("reused twin #41 got labels %v; `blocked` must not propagate to an open twin", twinLabels[0])
		}
	}
	if strings.Join(twinLabels[0], ",") != "nit" {
		t.Fatalf("reused twin labels = %v, want nit", twinLabels[0])
	}
	var sawReusedBlocked bool
	for _, r := range *recs {
		if r.Action == AuditActionReviewBacklogIssueFiled && strings.Contains(r.Detail, "reused=true") && strings.Contains(r.Detail, "blocked=true") {
			sawReusedBlocked = true
		}
	}
	if !sawReusedBlocked {
		t.Fatal("no review_backlog_issue_filed audit with reused=true blocked=true: the finding was not filed blocked, so the test exercised nothing")
	}
}

// hivecommons/hive#13906: an open issue that merely cites the same file is
// not the same finding. Without a matching category and a nearby line or
// shared symbol the finding is filed as its own issue, cross-linked to the
// same-file issue, and nothing is appended to that issue.
func TestReviewBacklogGitHubSinkFilesSeparatelyForSameFileDifferentFinding(t *testing.T) {
	cases := []struct {
		name  string
		title string
		body  string
	}{
		{"agent issue without a category", "[scanner] unchecked error in pkg/a.go", "pkg/a.go:3 drops the Close error"},
		{"different category, same line", "Review backlog (security): token logged", "Perspective: security\nEvidence: `pkg/a.go:3`"},
		{"same category, distant line", "Review backlog (correctness): stale cache", "Perspective: correctness\nEvidence: `pkg/a.go:300`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withReviewBacklogState(t)
			var created int
			comments := map[int][]string{}
			twin, err := json.Marshal([]map[string]any{{
				"number": 41, "title": tc.title, "body": tc.body, "state": "open",
				"html_url": "https://github.example/o/r/issues/41", "user": map[string]any{"login": testBot},
			}})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
					if r.URL.Query().Get("state") != "open" {
						_, _ = io.WriteString(w, `[]`)
						return
					}
					_, _ = w.Write(twin)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
					_, _ = io.WriteString(w, `{"name":"x"}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
					var n int
					_, _ = fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d/comments", &n)
					var payload struct {
						Body string `json:"body"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					comments[n] = append(comments[n], payload.Body)
					_, _ = io.WriteString(w, `{"id":1}`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
					created++
					_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
				}
			}))
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			c.appBotLogin = testBot
			c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
				return ReviewBacklogRouting{
					Severity: config.ReviewSeverityConfig{BlockAt: "P2"},
					Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogGitHubIssue, Labels: []string{"nit"}},
				}
			})
			finding := nit("nit", "low", "pkg/a.go", 3, "minor")
			if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: routingReport(t, 5, "abc", finding)}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if created != 1 {
				t.Fatalf("created %d issues, want 1 (a same-file issue is not the same finding)", created)
			}
			if len(comments[41]) != 0 {
				t.Fatalf("comments on same-file issue #41 = %q, want none", comments[41])
			}
			if len(comments[99]) != 1 || !strings.HasPrefix(comments[99][0], reviewBacklogSameFileMarker+"41 -->\n") || !strings.Contains(comments[99][0], "Same file as #41: both cite `pkg/a.go`") {
				t.Fatalf("comments on new issue #99 = %q, want one same-file cross-link to #41", comments[99])
			}
			if len(comments[5]) != 1 || strings.Contains(comments[5][0], "folded into an existing issue") {
				t.Fatalf("PR 5 summary comments = %q, want one summary with no folded item", comments[5])
			}
			ledger, err := loadReviewBacklog("")
			if err != nil {
				t.Fatal(err)
			}
			rec, ok := ledger.fingerprint("o/r", ReviewFindingFingerprint(outputschema.Finding{Title: "nit", File: "pkg/a.go", Summary: "minor"}))
			if !ok || rec.ItemNumber != 99 {
				t.Fatalf("ledger record = %+v ok=%v, want the new issue #99", rec, ok)
			}
		})
	}
}

// The #13906 evidence shapes: findings folded into #13823 and #13629 only
// shared a file with the host issue.
func TestReviewBacklogFoldMatches(t *testing.T) {
	stagelabelsHost := "Review backlog (correctness): Current is now case-insensitive while Owned/Plan remain case-sensitive"
	stagelabelsBody := "Perspective: correctness\nSeverity: info (P3)\nEvidence: `src/pkg/stagelabels/stagelabels.go:832` (https://github.com/o/r/blob/abc/src/pkg/stagelabels/stagelabels.go#L832)\nFinding: Current is now case-insensitive"
	cases := []struct {
		name        string
		perspective review.Perspective
		finding     outputschema.Finding
		title, body string
		want        bool
	}{
		{
			name:        "same file, distant line, no shared symbol",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "Backports IssueCoveredByPR/CoversIssue to v5 at v6's insertion point", File: "src/pkg/stagelabels/stagelabels.go", Line: 872},
			title:       stagelabelsHost, body: stagelabelsBody,
			want: false,
		},
		{
			name:        "same file, nearby line, same category",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "Current drops the trim on label names", File: "./src/pkg/stagelabels/stagelabels.go", Line: 838},
			title:       stagelabelsHost, body: stagelabelsBody,
			want: true,
		},
		{
			name:        "same file, nearby line, different category",
			perspective: review.PerspectiveSecurity,
			finding:     outputschema.Finding{Title: "label input is not bounded", File: "src/pkg/stagelabels/stagelabels.go", Line: 832},
			title:       stagelabelsHost, body: stagelabelsBody,
			want: false,
		},
		{
			name:        "shared symbol, distant line, same category",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "contributorProfileFromRegistrationToken ignores token_max_age", File: "src/pkg/dashboard/contribute_profiles.go", Line: 482},
			title:       "Review backlog (correctness): contributorProfileFromRegistrationToken is now unused",
			body:        "Perspective: correctness\nEvidence: `src/pkg/dashboard/contribute_profiles.go:120`",
			want:        true,
		},
		{
			name:        "backticked symbol",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "`Current` misses empty labels", File: "src/pkg/stagelabels/stagelabels.go", Line: 900},
			title:       "Review backlog (correctness): `Current` is case-insensitive", body: stagelabelsBody,
			want: true,
		},
		{
			name:        "shared file name is not a shared symbol",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "review_evidence.go leaks a temp file", File: "src/pkg/github/review_evidence.go", Line: 400},
			title:       "Review backlog (correctness): review_evidence.go drops a GitHub error",
			body:        "Perspective: correctness\nEvidence: `src/pkg/github/review_evidence.go:12`",
			want:        false,
		},
		{
			name:        "agent-filed issue without a category",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "nit", File: "pkg/a.go", Line: 3},
			title:       "[scanner] unchecked error in pkg/a.go", body: "pkg/a.go:3 drops the Close error",
			want: false,
		},
		{
			name:        "category from the Perspective line",
			perspective: review.PerspectiveCorrectness,
			finding:     outputschema.Finding{Title: "nit", File: "pkg/a.go", Line: 3},
			title:       "[scanner] unchecked error", body: "Perspective: Correctness\nEvidence: `pkg/a.go:13`",
			want: true,
		},
		{
			name:    "no perspective never folds",
			finding: outputschema.Finding{Title: "nit", File: "pkg/a.go", Line: 3},
			title:   "Review backlog (): nit", body: "Evidence: `pkg/a.go:3`",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewBacklogFoldMatches(tc.perspective, tc.finding, tc.title, tc.body); got != tc.want {
				t.Fatalf("reviewBacklogFoldMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReusedTwinLabels(t *testing.T) {
	got := reusedTwinLabels([]string{"nit", "Blocked", "agent/reviewer"})
	if strings.Join(got, ",") != "nit,agent/reviewer" {
		t.Fatalf("reusedTwinLabels = %v", got)
	}
	if got := reusedTwinLabels(nil); len(got) != 0 {
		t.Fatalf("reusedTwinLabels(nil) = %v", got)
	}
}

// The split sweep files folds out of a not_planned host through this sink,
// and that host is itself a recently rejected App-bot issue over the same
// file set (a legacy fold cited exactly the host's files). With SplitFrom
// naming the host, createIssue's rejected-twin gate ignores it and the
// split is filed. Without it — or when some other rejected issue matches —
// the gate returns the closed issue's number with RejectedTwin set, and the
// sink must report nothing filed rather than hand back a closed issue as
// the new item, or the sweep would cross-link the host to itself and record
// the split as done.
func TestReviewBacklogGitHubSinkRejectedTwin(t *testing.T) {
	cases := []struct {
		name      string
		splitFrom int
		rejected  []int // closed not_planned App-bot issues over pkg/a.go
		wantFiled bool
	}{
		{name: "host is the only rejected twin, ignored", splitFrom: 42, rejected: []int{42}, wantFiled: true},
		{name: "fresh filing with a rejected twin", splitFrom: 0, rejected: []int{42}, wantFiled: false},
		{name: "another rejected twin besides the host", splitFrom: 42, rejected: []int{42, 41}, wantFiled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withReviewBacklogState(t)
			var created, labelled, commented int
			recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
					if r.URL.Query().Get("state") != "closed" {
						_, _ = io.WriteString(w, `[]`)
						return
					}
					var items []string
					for _, n := range tc.rejected {
						items = append(items, fmt.Sprintf(`{"number":%d,"title":"[scanner] unchecked Close error in pkg/a.go","body":"Evidence: `+"`pkg/a.go:5`"+` drops the Close error","state":"closed","state_reason":"not_planned","closed_at":%q,"updated_at":%q,"html_url":"https://github.example/o/r/issues/%d","user":{"login":%q}}`, n, recent, recent, n, testBot))
					}
					_, _ = io.WriteString(w, "["+strings.Join(items, ",")+"]")
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
					_, _ = io.WriteString(w, `{"name":"x"}`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
					labelled++
					_, _ = io.WriteString(w, `[{"name":"nit"}]`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
					commented++
					_, _ = io.WriteString(w, `{"id":1}`)
				case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
					created++
					_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
				default:
					w.WriteHeader(http.StatusNotFound)
					_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
				}
			}))
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			c.appBotLogin = testBot
			sink := githubIssueBacklogSink{c: c}
			finding := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
			ref, err := sink.CreateItem(context.Background(), ReviewBacklogItem{
				Repo:        "o/r",
				Title:       "Review backlog (correctness): unused variable",
				Body:        "Split from #42 (host closed)\n\n`pkg/a.go:7` x is never read",
				Labels:      []string{"nit"},
				Perspective: review.PerspectiveCorrectness,
				Finding:     finding,
				SplitFrom:   tc.splitFrom,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantFiled {
				if ref.Number != 99 || ref.ID != "99" || ref.Reused {
					t.Fatalf("ref = %+v, want the new issue #99", ref)
				}
				if created != 1 {
					t.Fatalf("created=%d, want 1", created)
				}
				return
			}
			if ref.ID != "" || ref.Number != 0 || ref.Reused {
				t.Fatalf("ref = %+v, want empty: a rejected twin is not a backlog item", ref)
			}
			if created != 0 || labelled != 0 || commented != 0 {
				t.Fatalf("created=%d labelled=%d commented=%d, want no writes", created, labelled, commented)
			}
		})
	}
}
