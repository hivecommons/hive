package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
)

func newReviewMockServer(t *testing.T, reviewed *int, lastEvent *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews") {
			if reviewed != nil {
				*reviewed++
			}
			if lastEvent != nil {
				var body struct {
					Event string `json:"event"`
				}
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				*lastEvent = body.Event
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"state":"APPROVED"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func reviewTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	c := testClient(t, srvURL)
	c.reviewAuthz = func(agent string, uid int) error { return nil }
	return c
}

func withReviewDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := reviewRequestDirForTest
	reviewRequestDirForTest = dir
	t.Cleanup(func() { reviewRequestDirForTest = old })
	return dir
}

func TestReviewRequestWatcherSetsQueueMode(t *testing.T) {
	for _, tt := range []struct {
		name     string
		preexist bool
	}{
		{name: "fresh"},
		{name: "upgrade existing", preexist: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := withReviewDir(t)
			if tt.preexist {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("pre-create review queue: %v", err)
				}
				if err := os.Chmod(dir, 0o755); err != nil {
					t.Fatalf("pre-create chmod: %v", err)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			done := reviewTestClient(t, "http://127.0.0.1:0").StartReviewRequestWatcher(ctx, nil, nil)
			cancel()
			<-done

			assertRequestDirMode(t, "review", dir)
		})
	}
}

// End-to-end: an approve review is submitted, consumed, audited as
// agent_pr_reviewed with state=approved.
func TestReviewRequestWatcher_ApprovesAndAudits(t *testing.T) {
	reviewed := 0
	var apiEvent string
	srv := newReviewMockServer(t, &reviewed, &apiEvent)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	var gotAction, gotDetail string
	c.SetAttributionAudit(func(action, detail, agent string) { gotAction, gotDetail = action, detail })

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 5, Event: "approve", Agent: "reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if reviewed != 1 {
		t.Fatalf("expected 1 review submitted, got %d", reviewed)
	}
	if apiEvent != "APPROVE" {
		t.Errorf("api event = %q, want APPROVE", apiEvent)
	}
	if gotAction != AuditActionPRReviewed {
		t.Errorf("audit action = %q, want %q", gotAction, AuditActionPRReviewed)
	}
	if !strings.Contains(gotDetail, "state=approved") || !strings.Contains(gotDetail, "number=5") {
		t.Errorf("audit detail wrong: %q", gotDetail)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("review request should be consumed on success")
	}
}

// request_changes and comment map to the right API verb + state.
func TestReviewRequestWatcher_EventMapping(t *testing.T) {
	cases := []struct{ event, apiEvent, state string }{
		{"request_changes", "REQUEST_CHANGES", "changes_requested"},
		{"comment", "COMMENT", "commented"},
	}
	for _, tc := range cases {
		reviewed := 0
		var apiEvent string
		srv := newReviewMockServer(t, &reviewed, &apiEvent)
		c := reviewTestClient(t, srv.URL)
		dir := withReviewDir(t)
		var gotDetail string
		c.SetAttributionAudit(func(action, detail, agent string) { gotDetail = detail })
		if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 9, Event: tc.event, Body: "please fix", Agent: "reviewer"}); err != nil {
			t.Fatal(err)
		}
		c.ProcessReviewRequestsOnce(context.Background())
		srv.Close()
		if apiEvent != tc.apiEvent {
			t.Errorf("%s: api event = %q, want %q", tc.event, apiEvent, tc.apiEvent)
		}
		if !strings.Contains(gotDetail, "state="+tc.state) {
			t.Errorf("%s: detail missing state=%s: %q", tc.event, tc.state, gotDetail)
		}
	}
}

// request_changes/comment without a body is malformed (GitHub rejects it).
func TestReviewRequestWatcher_RequiresBodyForNonApprove(t *testing.T) {
	c := reviewTestClient(t, "http://127.0.0.1:0")
	dir := withReviewDir(t)
	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 9, Event: "comment", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if _, err := os.Stat(reqPath + ".bad"); err != nil {
		t.Errorf("body-less comment review should be quarantined .bad")
	}
}

// A nil authorizer denies (fail closed).
func TestReviewRequestWatcher_AuthzFailsClosed(t *testing.T) {
	reviewed := 0
	srv := newReviewMockServer(t, &reviewed, nil)
	defer srv.Close()
	c := testClient(t, srv.URL) // testClient sets prAuthz but NOT reviewAuthz → nil
	dir := withReviewDir(t)
	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "approve", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if reviewed != 0 {
		t.Errorf("nil authorizer must deny; got %d reviews", reviewed)
	}
	if _, err := os.Stat(reqPath + ".denied"); err != nil {
		t.Errorf("denied review should be quarantined .denied")
	}
}

// newReviewFailingMockServer fails the first `fails` review POSTs with a 500,
// then succeeds.
func newReviewFailingMockServer(t *testing.T, reviewed *int, fails *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews") {
			if *fails > 0 {
				*fails--
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if reviewed != nil {
				*reviewed++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"state":"APPROVED"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// Transient failure: retries are suppressed inside the backoff window and the
// request succeeds after it elapses — never an every-tick retry loop.
func TestReviewRequestWatcher_RetriesWithBackoff(t *testing.T) {
	reviewed, fails := 0, 1
	srv := newReviewFailingMockServer(t, &reviewed, &fails)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 7, Event: "approve", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c.processReviewRequests(context.Background(), clock)
	if reviewed != 0 {
		t.Fatalf("first attempt should have failed")
	}
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("request must survive a transient failure: %v", err)
	}
	c.processReviewRequests(context.Background(), clock)
	if reviewed != 0 {
		t.Fatalf("retry must be suppressed inside the backoff window")
	}
	now = now.Add(requestRetryBase + time.Second)
	c.processReviewRequests(context.Background(), clock)
	if reviewed != 1 {
		t.Fatalf("expected review on post-backoff retry, got %d", reviewed)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("request should be consumed after eventual success")
	}
}

// Give-up horizon: a review request still failing past requestRetryMaxAge is
// quarantined as .failed and never retried again.
func TestReviewRequestWatcher_QuarantinesAfterMaxAge(t *testing.T) {
	reviewed, fails := 0, 1000000
	srv := newReviewFailingMockServer(t, &reviewed, &fails)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 7, Event: "approve", Agent: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }
	c.processReviewRequests(context.Background(), clock)
	now = now.Add(requestRetryMaxAge + time.Hour)
	c.processReviewRequests(context.Background(), clock)

	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed quarantine: %v", err)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("original request should be renamed away")
	}
}

// Two request files with byte-identical bodies for the same PR are two
// reviews (the reviewer's "no findings" text repeats across heads), while a
// retry of one file is a replay. The mutation journal must see the first as
// distinct and the second as already applied — and neither may leave the
// request retrying (hivecommons/hive: chairlift#182 sat in backoff for hours
// behind "operation effect is already applied").
func TestReviewRequestWatcher_IdenticalBodyOnNewRequestIsNotAReplay(t *testing.T) {
	reviewed := 0
	var apiEvent string
	srv := newReviewMockServer(t, &reviewed, &apiEvent)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	ledger, err := mutation.OpenLedger(filepath.Join(t.TempDir(), "claims.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := mutation.OpenJournal(filepath.Join(t.TempDir(), "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetMutationBoundary(&mutation.Boundary{Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: "enforce"}, Holder: "hive"})

	req := ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Body: "**Reviewed** — no findings"}
	for i := 0; i < 2; i++ {
		if _, err := WriteReviewRequest(dir, req); err != nil {
			t.Fatal(err)
		}
		c.ProcessReviewRequestsOnce(context.Background())
	}
	if reviewed != 2 {
		t.Fatalf("expected both requests to post, got %d reviews", reviewed)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range left {
		if !strings.HasSuffix(f, ".result.json") {
			t.Fatalf("request left behind (still retrying): %s", filepath.Base(f))
		}
	}
}

// review.confidence_score appends the derived score to the posted comment,
// above the attribution trailer, computed from the verdict file that rides
// with the request (hivecommons/hive#8182). Off, the body is untouched; a
// request with no parseable verdict gets no line rather than a guess.
func TestReviewRequestWatcher_ConfidenceLine(t *testing.T) {
	var posted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews") {
			var body struct {
				Body string `json:"body"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			posted = body.Body
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"state":"COMMENTED"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)
	enabled := false
	c.SetConfidenceScore(func() bool { return enabled })

	report := validVerdictJSON(t, "o/r", 5, "correctness", "changes_requested")
	post := func(rep string) string {
		posted = ""
		if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Body: "**correctness** — off by one", Report: rep}); err != nil {
			t.Fatal(err)
		}
		c.ProcessReviewRequestsOnce(context.Background())
		return posted
	}

	if got := post(report); strings.Contains(got, "Confidence:") {
		t.Fatalf("disabled must not add a score, got %q", got)
	}
	enabled = true
	got := post(report)
	want := "**Confidence: 3/5** (needs attention) — 1 perspective requested changes"
	if !strings.Contains(got, want) {
		t.Fatalf("enabled body = %q, want it to contain %q", got, want)
	}
	if strings.Index(got, "Confidence:") > strings.Index(got, AttributionTrailerPrefix) && strings.Contains(got, AttributionTrailerPrefix) {
		t.Fatalf("confidence line must precede the attribution trailer: %q", got)
	}
	if got := post(""); strings.Contains(got, "Confidence:") {
		t.Fatalf("no verdict must mean no score, got %q", got)
	}
}

// A verdict that fails validation must refuse the WHOLE request before the
// comment is posted. Posting first and dropping the verdict afterwards left the
// PR unrecorded, so the next kick handed it back and the same comment landed
// again — actions#548 collected six duplicate notices in two hours.
func TestReviewRequestWatcher_MalformedVerdictRefusesWholeRequest(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews") {
			posts++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"state":"COMMENTED"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	// The shape the cadence reviewer actually produced on a live hive.
	freeform := `{"repo":"o/r","pr":5,"verdict":"requires_human","summary":"duplicate of #6"}`
	path, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Body: "This appears to duplicate #6", Report: freeform})
	if err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if posts != 0 {
		t.Fatalf("a malformed verdict must post nothing, posted %d reviews", posts)
	}
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatalf("malformed request must be quarantined as .bad: %v", err)
	}
	raw, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("result file: %v", err)
	}
	var resp ReviewResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Fatalf("result must report failure, got %+v", resp)
	}
	for _, want := range []string{"nothing posted", "lane: is required", `"lane":"review-swarm"`} {
		if !strings.Contains(resp.Error, want) {
			t.Errorf("error %q must contain %q so the agent can fix the JSON from the result alone", resp.Error, want)
		}
	}

	// A well-formed verdict on the same PR still goes through.
	if _, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "comment", Agent: "reviewer", Body: "This appears to duplicate #6", Report: validVerdictJSON(t, "o/r", 5, "intent-alignment", "requires_human")}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())
	if posts != 1 {
		t.Fatalf("valid verdict must post exactly once, posted %d", posts)
	}
}
