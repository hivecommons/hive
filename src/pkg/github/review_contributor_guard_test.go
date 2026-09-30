package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contributorReviewServer serves one PR (GET) and accepts reviews (POST),
// recording the event and body the relay actually sent. prAuthor is the PR's
// author login; an empty one makes the PR unreadable (404), which is how a
// flaky API looks to the guard.
func contributorReviewServer(t *testing.T, prAuthor string, gotEvent, gotBody *string, prGets, reviews *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			var body struct {
				Event string `json:"event"`
				Body  string `json:"body"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			if gotEvent != nil {
				*gotEvent = body.Event
			}
			if gotBody != nil {
				*gotBody = body.Body
			}
			if reviews != nil {
				*reviews++
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":1,"html_url":"https://example.test/pr/5#review-1"}`)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			if prGets != nil {
				*prGets++
			}
			if prAuthor == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":5,"user":{"login":"`+prAuthor+`","type":"User"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// A contributor's PR can never be approved or have changes requested through
// the review relay: both become a COMMENT (hivecommons/hive#9590). This is the
// acceptance test for the guard — it asserts the event that reaches GitHub,
// not what the reviewer asked for.
func TestReviewRelay_ContributorPRIsCommentOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		event     string
		body      string
		wantBody  string
		wantState string
	}{
		{
			name:      "approve becomes comment",
			event:     "approve",
			wantBody:  contributorCommentOnlyFallbackBody,
			wantState: "commented",
		},
		{
			name:      "request_changes becomes comment",
			event:     "request_changes",
			body:      "please add a test",
			wantBody:  "please add a test",
			wantState: "commented",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotEvent, gotBody string
			reviews := 0
			srv := contributorReviewServer(t, "outside-contributor", &gotEvent, &gotBody, nil, &reviews)
			defer srv.Close()
			c := reviewTestClient(t, srv.URL)
			c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-bot"})
			dir := withReviewDir(t)
			var gotDetail string
			c.SetAttributionAudit(func(action, detail, agent string) { gotDetail = detail })

			reqPath, err := WriteReviewRequest(dir, ReviewRequest{
				Repo: "o/r", Number: 5, Event: tc.event, Body: tc.body, Agent: "reviewer",
			})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessReviewRequestsOnce(context.Background())

			if reviews != 1 {
				t.Fatalf("reviews submitted = %d, want 1", reviews)
			}
			if gotEvent != "COMMENT" {
				t.Errorf("api event = %q, want COMMENT on a contributor PR", gotEvent)
			}
			if !strings.Contains(gotBody, tc.wantBody) {
				t.Errorf("review body = %q, want it to contain %q", gotBody, tc.wantBody)
			}
			if !strings.Contains(gotDetail, "state="+tc.wantState) {
				t.Errorf("audit detail = %q, want state=%s", gotDetail, tc.wantState)
			}
			resp := readReviewResult(t, reqPath)
			if !resp.OK {
				t.Errorf("result should be ok: %+v", resp)
			}
			if !strings.Contains(resp.Note, "downgraded to COMMENT") {
				t.Errorf("result note = %q, want the downgrade explained", resp.Note)
			}
		})
	}
}

// The hive's own PRs are unaffected: approve still approves.
func TestReviewRelay_HiveAuthoredPRStillApproves(t *testing.T) {
	var gotEvent string
	srv := contributorReviewServer(t, "hive-bot", &gotEvent, nil, nil, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-bot"})
	dir := withReviewDir(t)

	if _, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 5, Event: "approve", Agent: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if gotEvent != "APPROVE" {
		t.Errorf("api event = %q, want APPROVE on a hive-authored PR", gotEvent)
	}
}

// A PR whose author cannot be read is treated as a contributor's: "we could
// not tell" must never resolve into approving someone else's work.
func TestReviewRelay_UnreadablePRAuthorIsCommentOnly(t *testing.T) {
	var gotEvent string
	srv := contributorReviewServer(t, "", &gotEvent, nil, nil, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-bot"})
	dir := withReviewDir(t)

	if _, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 5, Event: "approve", Agent: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if gotEvent != "COMMENT" {
		t.Errorf("api event = %q, want COMMENT when the author is unknown", gotEvent)
	}
}

// A COMMENT is already COMMENT-only, so the guard costs no extra API call on
// the path the review swarm actually takes.
func TestReviewRelay_CommentSkipsAuthorLookup(t *testing.T) {
	var gotEvent string
	prGets := 0
	srv := contributorReviewServer(t, "outside-contributor", &gotEvent, nil, &prGets, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.SetHiveIdentity(HiveIdentity{AIAuthor: "hive-bot"})
	dir := withReviewDir(t)

	if _, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 5, Event: "comment", Body: "one note", Agent: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if gotEvent != "COMMENT" {
		t.Errorf("api event = %q, want COMMENT", gotEvent)
	}
	if prGets != 0 {
		t.Errorf("PR fetched %d time(s) for a COMMENT; the guard should short-circuit", prGets)
	}
}

// A hive that has told us nothing about its own identity cannot tell its PRs
// from anyone else's, so the guard stays out of the way rather than
// downgrading every review it sees.
func TestReviewRelay_NoHiveIdentityLeavesEventAlone(t *testing.T) {
	var gotEvent string
	prGets := 0
	srv := contributorReviewServer(t, "outside-contributor", &gotEvent, nil, &prGets, nil)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	dir := withReviewDir(t)

	if _, err := WriteReviewRequest(dir, ReviewRequest{
		Repo: "o/r", Number: 5, Event: "approve", Agent: "reviewer",
	}); err != nil {
		t.Fatal(err)
	}
	c.ProcessReviewRequestsOnce(context.Background())

	if gotEvent != "APPROVE" {
		t.Errorf("api event = %q, want APPROVE with no identity configured", gotEvent)
	}
	if prGets != 0 {
		t.Errorf("PR fetched %d time(s) with the guard inactive", prGets)
	}
}
