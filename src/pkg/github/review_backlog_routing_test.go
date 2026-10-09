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

func TestReviewBacklogTargetFallsBackToGitHubWithFromReviewLabel(t *testing.T) {
	c := &Client{}
	sink, labels := c.reviewBacklogTarget(ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: "jira", Labels: []string{"nit"}}})
	if sink.Destination() != config.ReviewBacklogGitHubIssue {
		t.Fatalf("destination = %q, want the GitHub issue fallback", sink.Destination())
	}
	if strings.Join(labels, ",") != "nit,from-review" {
		t.Fatalf("labels = %v, want [nit from-review]", labels)
	}
	fake := &fakeBacklogSink{dest: config.ReviewBacklogJira}
	sink, labels = c.reviewBacklogTarget(ReviewBacklogRouting{Backlog: config.ReviewBacklogConfig{Destination: "jira", Labels: []string{"nit"}}, Sink: fake})
	if sink != fake || strings.Join(labels, ",") != "nit" {
		t.Fatalf("configured sink: got %v %v, want the jira sink with [nit]", sink.Destination(), labels)
	}
	_, labels = c.reviewBacklogTarget(ReviewBacklogRouting{})
	if strings.Join(labels, ",") != "from-review" {
		t.Fatalf("default labels = %v, want [from-review]", labels)
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
}

func (f *fakeBacklogSink) Destination() string { return f.dest }

func (f *fakeBacklogSink) CreateItem(_ context.Context, item ReviewBacklogItem) (ReviewBacklogRef, error) {
	f.created = append(f.created, item)
	n := len(f.created)
	ref := ReviewBacklogRef{ID: fmt.Sprintf("id-%d", n), Key: fmt.Sprintf("ENG-%d", n), URL: fmt.Sprintf("https://tracker.test/ENG-%d", n)}
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
	if first.Repo != "o/r" || first.Title != "Review backlog (correctness): unused variable" || strings.Join(first.Labels, ",") != "nit" {
		t.Fatalf("first item = %+v", first)
	}
	for _, want := range []string{
		"PR: #5 (https://github.com/o/r/pull/5)",
		"Severity: medium (P2)",
		"Evidence: `pkg/a.go:7` (https://github.com/o/r/blob/abc/pkg/a.go#L7)",
		"below the blocking line (P1)",
		"x is assigned and never read",
		"Fingerprint: `" + ReviewFindingFingerprint(outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Summary: "x is assigned and never read"}) + "`",
		"filed by Hive review backlog",
	} {
		if !strings.Contains(first.Body, want) {
			t.Errorf("item body missing %q:\n%s", want, first.Body)
		}
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
	if strings.Join(issueLabels[0], ",") != "nit,from-review" {
		t.Fatalf("labels = %v, want nit plus the from-review fallback label", issueLabels[0])
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
