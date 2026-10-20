package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type relayHoldActivity struct {
	login    string
	userType string
	state    string // review state; empty for a comment
	body     string
	at       time.Time
}

// newRelayHoldFront serves o/r#42's reviews and comments and proxies
// everything else to backend.
func newRelayHoldFront(t *testing.T, backend string, reviews, comments []relayHoldActivity, reviewStatus int) *httptest.Server {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	user := func(a relayHoldActivity) map[string]string {
		typ := a.userType
		if typ == "" {
			typ = "User"
		}
		return map[string]string{"login": a.login, "type": typ}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/42/reviews":
			if reviewStatus != 0 {
				w.WriteHeader(reviewStatus)
				return
			}
			out := []map[string]any{}
			for _, a := range reviews {
				out = append(out, map[string]any{"user": user(a), "state": a.state, "submitted_at": a.at.Format(time.RFC3339)})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42/comments":
			out := []map[string]any{}
			for _, a := range comments {
				out = append(out, map[string]any{"user": user(a), "body": a.body, "created_at": a.at.Format(time.RFC3339)})
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
}

// hivecommons/hive#13085: the merge-request relay refuses a PR the review
// swarm holds for a person until a trusted merger releases it, the hold the
// self-merge sweep has applied since #12374.
func TestMergeRequestWatcher_ReviewVerdictHold13085(t *testing.T) {
	verdictAt := time.Date(2026, 10, 19, 22, 52, 0, 0, time.UTC)
	after := verdictAt.Add(time.Hour)
	before := verdictAt.Add(-time.Hour)
	held := ReviewVerdictHold{Held: true, Verdict: "requires_human", HeadSHA: "2c19cd5aaaaaaa", RecordedAt: verdictAt}
	for _, tc := range []struct {
		name         string
		hold         ReviewVerdictHold
		reviews      []relayHoldActivity
		comments     []relayHoldActivity
		reviewStatus int
		wantMerged   bool
		wantDenied   bool
	}{
		{name: "no hold merges", hold: ReviewVerdictHold{}, wantMerged: true},
		{name: "held with no release is refused", hold: held, wantDenied: true},
		{name: "owner /hive approve after the verdict releases", hold: held, wantMerged: true,
			comments: []relayHoldActivity{{login: "owner", body: "/hive approve\nMerge once CI is green.", at: after}}},
		{name: "trusted merger approving review after the verdict releases", hold: held, wantMerged: true,
			reviews: []relayHoldActivity{{login: "owner", state: "APPROVED", at: after}}},
		{name: "owner /hive approve before the verdict does not release", hold: held, wantDenied: true,
			comments: []relayHoldActivity{{login: "owner", body: "/hive approve", at: before}}},
		{name: "untrusted contributor /hive approve does not release", hold: held, wantDenied: true,
			comments: []relayHoldActivity{{login: "contributor", body: "/hive approve", at: after}}},
		{name: "bot /hive approve does not release", hold: held, wantDenied: true,
			comments: []relayHoldActivity{{login: "owner[bot]", userType: "Bot", body: "/hive approve", at: after}}},
		{name: "unreadable reviews fail closed without denying", hold: held, reviewStatus: http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newMergeMockServer(t, 0, &merges)
			defer api.Close()
			front := newRelayHoldFront(t, api.URL, tc.reviews, tc.comments, tc.reviewStatus)
			defer front.Close()
			c := testMergeClient(t, front.URL)
			c.SetMergeReviewVerdictGate(func(repo string, number int) ReviewVerdictHold {
				if repo == "o/r" && number == 42 {
					return tc.hold
				}
				return ReviewVerdictHold{}
			}, func(login string) bool { return login == "owner" })

			dir := t.TempDir()
			mergeRequestDirForTest = dir
			defer func() { mergeRequestDirForTest = "" }()
			reqPath, err := WriteMergeRequest(dir, MergeRequest{Repo: "o/r", Number: 42, Method: "squash", Agent: "scanner"})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessMergeRequestsOnce(context.Background())

			if got := merges == 1; got != tc.wantMerged {
				t.Fatalf("merged = %v (merges = %d), want %v", got, merges, tc.wantMerged)
			}
			resp := readMergeResult(t, reqPath)
			if resp.OK != tc.wantMerged {
				t.Fatalf("result ok = %v, want %v (%+v)", resp.OK, tc.wantMerged, resp)
			}
			_, deniedErr := os.Stat(reqPath + ".denied")
			if denied := deniedErr == nil; denied != tc.wantDenied {
				t.Fatalf("denied = %v, want %v (%+v)", denied, tc.wantDenied, resp)
			}
			if tc.wantDenied && !strings.Contains(resp.Error, "review verdict requires_human") {
				t.Fatalf("refusal = %q, want it to name the review verdict", resp.Error)
			}
			if tc.wantDenied && !strings.Contains(resp.Error, ReviewHoldReleaseCommand) {
				t.Fatalf("refusal = %q, want it to name %q", resp.Error, ReviewHoldReleaseCommand)
			}
		})
	}
}
