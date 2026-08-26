package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
