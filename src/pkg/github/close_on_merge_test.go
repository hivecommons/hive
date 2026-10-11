package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func mergedTestPR(number int, author, head, body string) *gh.PullRequest {
	mergedAt := gh.Timestamp{Time: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	return &gh.PullRequest{
		Number:         gh.Ptr(number),
		Title:          gh.Ptr("fix"),
		Body:           gh.Ptr(body),
		User:           &gh.User{Login: gh.Ptr(author)},
		Merged:         gh.Ptr(true),
		MergedAt:       &mergedAt,
		MergeCommitSHA: gh.Ptr("0123456789abcdef"),
		Head:           &gh.PullRequestBranch{Ref: gh.Ptr(head), SHA: gh.Ptr("headsha")},
		Base:           &gh.PullRequestBranch{Repo: &gh.Repository{FullName: gh.Ptr("o/r")}},
	}
}

func TestCloseOnMergeResolver(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   *gh.PullRequest
		want bool
		refs bool
	}{
		{
			name: "closing keyword",
			pr:   mergedTestPR(42, "hive[bot]", "scanner/other", "Fixes #7"),
			want: true,
		},
		{
			name: "refs plus branch claim",
			pr:   mergedTestPR(42, "hive[bot]", "scanner/fix-7", "Refs #7"),
			want: true,
			refs: true,
		},
		{
			name: "refs alone is not close evidence",
			pr:   mergedTestPR(42, "hive[bot]", "scanner/other", "Refs #7"),
			want: false,
			refs: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closeOnMergePRClaimsIssue(tc.pr, "o/r", "o/r", 7); got != tc.want {
				t.Fatalf("claims issue = %v, want %v", got, tc.want)
			}
			if got := closeOnMergePRRefsIssue(tc.pr, "o/r", "o/r", 7); got != tc.refs {
				t.Fatalf("refs issue = %v, want %v", got, tc.refs)
			}
		})
	}
}

func TestCloseOnMergeDecisionClosesOrAwaits(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     []string
		body       string
		marker     bool
		unmerged   bool
		wantAction CloseOnMergeAction
		wantPatch  bool
	}{
		{name: "plain open issue closes completed", wantAction: CloseOnMergeClosed, wantPatch: true},
		{name: "needs confirmation marker gets likely done comment", body: "body\n\nhive: needs-confirmation", labels: []string{"bug"}, wantAction: CloseOnMergeAwaiting},
		{name: "needs reporter gets likely done comment", labels: []string{"needs-reporter-confirmation"}, wantAction: CloseOnMergeAwaiting},
		{name: "needs human gets likely done comment", labels: []string{"needs-human"}, wantAction: CloseOnMergeAwaiting},
		{name: "epic gets likely done comment", labels: []string{"Epic"}, wantAction: CloseOnMergeAwaiting},
		{name: "existing marker noops", marker: true, wantAction: CloseOnMergeAlreadyDone},
		{name: "unmerged PR noops", unmerged: true, wantAction: CloseOnMergeNoop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patched bool
			var posted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
					labels := make([]map[string]string, 0, len(tc.labels))
					for _, l := range tc.labels {
						labels = append(labels, map[string]string{"name": l})
					}
					body := tc.body
					if body == "" {
						body = "body"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "body": body, "user": map[string]string{"login": "human", "type": "User"}, "labels": labels})
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
					if tc.marker {
						_ = json.NewEncoder(w).Encode([]map[string]string{{"body": CloseOnMergeMarker}})
					} else {
						_ = json.NewEncoder(w).Encode([]map[string]string{})
					}
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
					_ = json.NewEncoder(w).Encode([]map[string]any{})
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
					var req map[string]string
					_ = json.NewDecoder(r.Body).Decode(&req)
					posted = append(posted, req["body"])
					_ = json.NewEncoder(w).Encode(map[string]any{"id": len(posted), "body": req["body"]})
				case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
					patched = true
					var req map[string]string
					_ = json.NewDecoder(r.Body).Decode(&req)
					if req["state"] != "closed" || req["state_reason"] != "completed" {
						t.Fatalf("patch = %+v, want closed/completed", req)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
				case (r.Method == "GET" || r.Method == "POST") && strings.Contains(r.URL.Path, "/labels"):
					_ = json.NewEncoder(w).Encode([]map[string]string{})
				case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/needs-reporter-confirmation"):
					w.WriteHeader(http.StatusNotFound)
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"r"})
			pr := mergedTestPR(42, "hive[bot]", "scanner/fix-7", "Refs #7")
			if tc.unmerged {
				pr.Merged = gh.Ptr(false)
				pr.MergedAt = nil
			}
			res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 7, pr, true, CloseOnMergeOptions{
				Identity: HiveIdentity{AppLogin: "hive[bot]"},
			})
			if res.Action != tc.wantAction {
				t.Fatalf("action = %s (%s), want %s", res.Action, res.Reason, tc.wantAction)
			}
			if patched != tc.wantPatch {
				t.Fatalf("patched = %v, want %v", patched, tc.wantPatch)
			}
			if tc.wantAction == CloseOnMergeClosed && (len(posted) != 1 || !strings.Contains(posted[0], "Reply /reopen")) {
				t.Fatalf("close comment = %#v", posted)
			}
			if tc.wantAction == CloseOnMergeAwaiting && (len(posted) != 1 || !strings.Contains(posted[0], "awaiting confirmation")) {
				t.Fatalf("awaiting comment = %#v", posted)
			}
		})
	}
}

func TestCloseOnMergeBackfillClosesLinkedMergedPR(t *testing.T) {
	var patched bool
	var issueDetailGets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 7, "state": "open",
				"labels": []map[string]string{{"name": CoveredByPRLabel}},
			}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":  "cross-referenced",
				"source": map[string]any{"issue": map[string]any{"number": 42, "pull_request": map[string]any{"url": "u"}}},
			}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "Refs #7", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/fix-7", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			issueDetailGets++
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "labels": []map[string]string{}})
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			patched = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/needs-reporter-confirmation"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	results := c.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}})
	if len(results) == 0 || results[len(results)-1].Action != CloseOnMergeClosed {
		t.Fatalf("results = %+v, want close", results)
	}
	if !patched {
		t.Fatal("backfill did not close linked issue")
	}
	if issueDetailGets != 1 {
		t.Fatalf("backfill issue detail GETs = %d, want 1 from final close only", issueDetailGets)
	}
}

func TestCloseOnMergePRLookupUsesDetailCache(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	restore := resetPRDetailCacheForTest(func() time.Time { return now }, 0)
	defer restore()

	var pullGets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" || r.URL.Path != "/repos/o/r/pulls/42" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		pullGets++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 42, "title": "fix", "body": "Refs #7", "merged": true, "mergeable_state": "unknown",
			"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
			"user": map[string]string{"login": "hive[bot]"},
			"head": map[string]string{"ref": "scanner/fix-7", "sha": "headsha"},
			"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
		})
	}))
	defer srv.Close()

	c := newTestClient(t, srv, "o", []string{"r"})
	c.SetPRDetailTTLFunc(func() time.Duration { return time.Hour })
	for i := 0; i < 2; i++ {
		pr, err := c.getPullRequestForCloseOnMerge(context.Background(), "o/r", 42)
		if err != nil {
			t.Fatalf("iteration %d lookup failed: %v", i, err)
		}
		if !closeOnMergePRClaimsIssue(pr, "o/r", "o/r", 7) {
			t.Fatalf("iteration %d cached PR lost claim metadata: %+v", i, pr)
		}
	}
	if pullGets != 1 {
		t.Fatalf("pull detail GETs = %d, want 1", pullGets)
	}
}

// reopenFixture serves one open issue (o/r#7) whose comments grow as the
// sweep posts, and whose timeline is fixed per test (#11449).
type reopenFixture struct {
	t              *testing.T
	comments       []string
	timeline       []map[string]any
	timelineStatus int
	timelineGets   int
	posted         []string
	patched        bool
	pr             map[string]any
}

func reopenTimelineEvent(event, at, login, typ string) map[string]any {
	return map[string]any{"event": event, "created_at": at, "actor": map[string]string{"login": login, "type": typ}}
}

func (f *reopenFixture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 7, "state": "open",
				"labels": []map[string]string{{"name": LikelyDoneLabel}},
			}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "body": "body", "labels": []map[string]string{}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			out := make([]map[string]string, 0, len(f.comments))
			for _, b := range f.comments {
				out = append(out, map[string]string{"body": b})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
			f.timelineGets++
			if f.timelineStatus != 0 {
				w.WriteHeader(f.timelineStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(f.timeline)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.posted = append(f.posted, req["body"])
			f.comments = append(f.comments, req["body"])
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.posted), "body": req["body"]})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42":
			_ = json.NewEncoder(w).Encode(f.pr)
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			f.patched = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			f.t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func reopenFixturePR(body string) map[string]any {
	return map[string]any{
		"number": 42, "title": "fix", "body": body, "merged": true,
		"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
		"user": map[string]string{"login": "hive[bot]"},
		"head": map[string]string{"ref": "scanner/other", "sha": "headsha"},
		"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
	}
}

func assertReopenedMarkerComment(t *testing.T, posted []string, pr int) {
	t.Helper()
	if len(posted) != 1 {
		t.Fatalf("posted = %#v, want exactly one reopened-after-merge comment", posted)
	}
	if !strings.Contains(posted[0], closeOnMergeReopenedMarker(pr)) {
		t.Fatalf("comment %q lacks marker for PR %d", posted[0], pr)
	}
	if strings.Contains(posted[0], CloseOnMergeMarker) {
		t.Fatalf("comment %q carries the issue-wide close-on-merge marker", posted[0])
	}
}

func TestCloseOnMergeReopenedAfterMergeLedgerSkipsOnce(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := &reopenFixture{t: t, pr: reopenFixturePR("Fixes #7"), timeline: []map[string]any{
		reopenTimelineEvent("closed", "2026-10-09T12:00:02Z", "hive[bot]", "Bot"),
		reopenTimelineEvent("reopened", "2026-10-09T15:51:05Z", "maintainer", "User"),
	}}
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	if err := ledger.Record(IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedPR: true}); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, "o", []string{"r"})
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}
	for sweep := 1; sweep <= 2; sweep++ {
		results := c.CloseOnMergeForLedger(context.Background(), ledger, opts)
		if len(results) != 1 || results[0].Action != CloseOnMergeSkipped || results[0].Reason != "reopened_after_merge" {
			t.Fatalf("sweep %d results = %+v, want reopened_after_merge skip", sweep, results)
		}
	}
	if f.patched {
		t.Fatal("issue reopened after merge was closed")
	}
	if f.timelineGets != 1 {
		t.Fatalf("timeline GETs = %d, want 1 (second sweep stops at the per-PR marker)", f.timelineGets)
	}
	assertReopenedMarkerComment(t, f.posted, 42)
}

func TestCloseOnMergeReopenedAfterMergeBackfillSkipsOnce(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := &reopenFixture{pr: reopenFixturePR("Fixes #7"), timeline: []map[string]any{
		reopenTimelineEvent("reopened", "2026-10-09T15:51:05Z", "maintainer", "User"),
	}}
	f.t = t
	srv := f.server()
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	f.comments = []string{"covered by PR #42"}
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}
	for sweep := 1; sweep <= 2; sweep++ {
		results := c.CloseOnMergeBackfill(context.Background(), opts)
		if len(results) != 1 || results[0].PR != 42 || results[0].Action != CloseOnMergeSkipped || results[0].Reason != "reopened_after_merge" {
			t.Fatalf("sweep %d results = %+v, want reopened_after_merge skip", sweep, results)
		}
	}
	if f.patched {
		t.Fatal("issue reopened after merge was closed")
	}
	// The backfill's linked-PR discovery reads the timeline once per sweep;
	// the reopen check adds exactly one more read, on the first sweep only.
	if f.timelineGets != 3 {
		t.Fatalf("timeline GETs = %d, want 3 (2 link discovery + 1 reopen check)", f.timelineGets)
	}
	assertReopenedMarkerComment(t, f.posted, 42)
}

func TestCloseOnMergeReopenDecision(t *testing.T) {
	mergedAt := func(pr *gh.PullRequest, at string) *gh.PullRequest {
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		pr.MergedAt = &gh.Timestamp{Time: ts}
		return pr
	}
	for _, tc := range []struct {
		name           string
		pr             *gh.PullRequest
		comments       []string
		timeline       []map[string]any
		timelineStatus int
		wantAction     CloseOnMergeAction
		wantReason     string
		wantTimeline   int
		wantPosted     int
		wantMarker     bool
	}{
		{
			name:         "reopened before merge still closes",
			pr:           mergedTestPR(42, "hive[bot]", "scanner/other", "Fixes #7"),
			timeline:     []map[string]any{reopenTimelineEvent("reopened", "2026-10-09T11:00:00Z", "maintainer", "User")},
			wantAction:   CloseOnMergeClosed,
			wantReason:   "closed_completed",
			wantTimeline: 1,
			wantPosted:   1,
		},
		{
			name:         "reopen by github-actions bot counts",
			pr:           mergedTestPR(42, "hive[bot]", "scanner/other", "Fixes #7"),
			timeline:     []map[string]any{reopenTimelineEvent("reopened", "2026-10-09T13:00:00Z", "github-actions[bot]", "Bot")},
			wantAction:   CloseOnMergeSkipped,
			wantReason:   "reopened_after_merge",
			wantTimeline: 1,
			wantPosted:   1,
			wantMarker:   true,
		},
		{
			name:         "reopen by the hive itself is ignored",
			pr:           mergedTestPR(42, "hive[bot]", "scanner/other", "Fixes #7"),
			timeline:     []map[string]any{reopenTimelineEvent("reopened", "2026-10-09T13:00:00Z", "hive[bot]", "Bot")},
			wantAction:   CloseOnMergeClosed,
			wantReason:   "closed_completed",
			wantTimeline: 1,
			wantPosted:   1,
		},
		{
			name:         "existing per-PR marker skips without a timeline read",
			pr:           mergedTestPR(42, "hive[bot]", "scanner/other", "Fixes #7"),
			comments:     []string{closeOnMergeReopenedMarker(42)},
			wantAction:   CloseOnMergeSkipped,
			wantReason:   "reopened_after_merge",
			wantTimeline: 0,
			wantPosted:   0,
		},
		{
			name:         "different PR merged after the reopen still closes despite the marker",
			pr:           mergedAt(mergedTestPR(43, "hive[bot]", "scanner/other", "Fixes #7"), "2026-10-10T12:00:00Z"),
			comments:     []string{closeOnMergeReopenedComment(mergedTestPR(42, "hive[bot]", "x", ""))},
			timeline:     []map[string]any{reopenTimelineEvent("reopened", "2026-10-09T15:51:05Z", "maintainer", "User")},
			wantAction:   CloseOnMergeClosed,
			wantReason:   "closed_completed",
			wantTimeline: 1,
			wantPosted:   1,
		},
		{
			name:           "timeline read error skips without posting the marker",
			pr:             mergedTestPR(42, "hive[bot]", "scanner/other", "Fixes #7"),
			timelineStatus: http.StatusInternalServerError,
			wantAction:     CloseOnMergeSkipped,
			wantReason:     "timeline_read_failed",
			wantTimeline:   1,
			wantPosted:     0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &reopenFixture{t: t, comments: tc.comments, timeline: tc.timeline, timelineStatus: tc.timelineStatus}
			if f.timeline == nil {
				f.timeline = []map[string]any{}
			}
			srv := f.server()
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"r"})
			res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 7, tc.pr, true, CloseOnMergeOptions{
				Identity: HiveIdentity{AppLogin: "hive[bot]"},
			})
			if res.Action != tc.wantAction || res.Reason != tc.wantReason {
				t.Fatalf("result = %s/%s, want %s/%s", res.Action, res.Reason, tc.wantAction, tc.wantReason)
			}
			if f.patched != (tc.wantAction == CloseOnMergeClosed) {
				t.Fatalf("patched = %v for action %s", f.patched, res.Action)
			}
			if f.timelineGets != tc.wantTimeline {
				t.Fatalf("timeline GETs = %d, want %d", f.timelineGets, tc.wantTimeline)
			}
			if len(f.posted) != tc.wantPosted {
				t.Fatalf("posted = %#v, want %d comments", f.posted, tc.wantPosted)
			}
			if tc.wantMarker {
				assertReopenedMarkerComment(t, f.posted, tc.pr.GetNumber())
			}
		})
	}
}
