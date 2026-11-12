package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
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
		prBody     string
		marker     bool
		unmerged   bool
		external   bool
		mergedBy   string
		lookup     bool
		wantReason string
		wantAction CloseOnMergeAction
		wantPatch  bool
	}{
		{name: "plain open issue closes completed", prBody: "Fixes #7", wantAction: CloseOnMergeClosed, wantPatch: true},
		{name: "refs-only PR on a plain issue is partially addressed, not closed", wantAction: CloseOnMergeSkipped},
		{name: "external PR merged by someone else stays open", external: true, mergedBy: "maintainer", prBody: "Fixes #7", wantAction: CloseOnMergeSkipped, wantReason: "untrusted_author"},
		{name: "external PR missing merge actor stays open", external: true, lookup: true, prBody: "Fixes #7", wantAction: CloseOnMergeSkipped, wantReason: "untrusted_author"},
		{name: "external PR merged by hive closes", external: true, mergedBy: "hive[bot]", prBody: "Fixes #7", wantAction: CloseOnMergeClosed, wantPatch: true},
		{name: "external PR merge actor resolved from API", external: true, mergedBy: "hive[bot]", lookup: true, prBody: "Fixes #7", wantAction: CloseOnMergeClosed, wantPatch: true},
		{name: "hive merge still respects confirmation", external: true, mergedBy: "hive[bot]", labels: []string{"needs-human"}, prBody: "Fixes #7", wantAction: CloseOnMergeAwaiting},
		{name: "hive merge still requires merged PR", external: true, mergedBy: "hive[bot]", unmerged: true, wantAction: CloseOnMergeNoop},
		{name: "needs confirmation marker gets likely done comment", body: "body\n\nhive: needs-confirmation", labels: []string{"bug"}, prBody: "Fixes #7", wantAction: CloseOnMergeAwaiting},
		{name: "needs reporter gets likely done comment", labels: []string{"needs-reporter-confirmation"}, prBody: "Fixes #7", wantAction: CloseOnMergeAwaiting},
		{name: "needs human gets likely done comment", labels: []string{"needs-human"}, prBody: "Fixes #7", wantAction: CloseOnMergeAwaiting},
		{name: "epic gets likely done comment", labels: []string{"Epic"}, prBody: "Fixes #7", wantAction: CloseOnMergeAwaiting},
		{name: "refs-only PR is partially addressed, no awaiting comment", labels: []string{"needs-human"}, wantAction: CloseOnMergeSkipped},
		{name: "existing marker noops", marker: true, wantAction: CloseOnMergeAlreadyDone},
		{name: "unmerged PR noops", unmerged: true, wantAction: CloseOnMergeNoop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patched bool
			var posted []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42" && tc.lookup:
					pr := mergedTestPR(42, "contributor", "topic", "Fixes #7")
					pr.MergedBy = &gh.User{Login: gh.Ptr(tc.mergedBy)}
					_ = json.NewEncoder(w).Encode(pr)
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
			prBody := tc.prBody
			if prBody == "" {
				prBody = "Refs #7"
			}
			pr := mergedTestPR(42, "hive[bot]", "scanner/fix-7", prBody)
			pr.Base.Ref = gh.Ptr("v6")
			if tc.external {
				pr.User = &gh.User{Login: gh.Ptr("contributor")}
			}
			if !tc.lookup {
				pr.MergedBy = &gh.User{Login: gh.Ptr(tc.mergedBy)}
			}
			if tc.lookup {
				restore := resetPRDetailCacheForTest(time.Now, 0)
				defer restore()
				c.SetPRDetailTTLFunc(func() time.Duration { return time.Hour })
				c.storePRDetail("o/r", 42, pr)
			}
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
			if tc.wantAction == CloseOnMergeSkipped {
				wantReason := tc.wantReason
				if wantReason == "" {
					wantReason = "partially_addressed"
				}
				if res.Reason != wantReason || len(posted) != 0 {
					t.Fatalf("skipped result = %+v, posted = %#v, want %s without comments", res, posted, wantReason)
				}
			}
		})
	}
}

func TestCloseOnMergeBackfillClosesLinkedMergedPR(t *testing.T) {
	var patched bool
	var stageStripped []string
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
				"number": 42, "title": "fix", "body": "Fixes #7", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user":      map[string]string{"login": "contributor"},
				"merged_by": map[string]string{"login": "hive[bot]"},
				"head":      map[string]string{"ref": "scanner/fix-7", "sha": "headsha"},
				"base":      map[string]any{"ref": "v6", "repo": map[string]string{"full_name": "o/r"}},
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
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/stage/"):
			stageStripped = append(stageStripped, path.Base(r.URL.Path))
			_, _ = w.Write([]byte(`[]`))
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
	if len(stageStripped) != 1 || stageStripped[0] != path.Base(CoveredByPRLabel) {
		t.Fatalf("stage labels stripped on close = %v, want the covered-by-PR stage (#12436)", stageStripped)
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
	// RecheckAfter 0 would mean the default; a one-nanosecond window makes
	// every sweep re-verify so this test exercises the per-PR comment marker
	// rather than the ledger's settled marker.
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}, RecheckAfter: time.Nanosecond}
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

func TestCloseOnMergeSkipsClaimedPullRequestTarget(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/repos/o/r/issues/14730" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":       14730,
				"state":        "open",
				"pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/14730"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	pr := mergedTestPR(25, "hive[bot]", "fix", "the fix is made in o/r#14730")
	opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}
	res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 14730, pr, true, opts)
	if res.Action != CloseOnMergeSkipped || res.Reason != "target_is_pull_request" {
		t.Fatalf("result = %+v, want target_is_pull_request skip", res)
	}
	if len(writes) != 0 {
		t.Fatalf("unexpected write requests: %v", writes)
	}
}

// openSubIssuesFixture serves acme/widgets with one open issue, #7, whose
// payload carries (or lacks) GitHub's sub_issues_summary block in the
// single-issue read and in the open-issue listing alike. It records every
// write, so a test can prove close-on-merge wrote nothing at all.
type openSubIssuesFixture struct {
	t *testing.T
	// summary is #7's sub_issues_summary; nil leaves the block out.
	summary *IssueChildren
	labels  []string
	// comments is what GET /issues/7/comments answers, grown by posts.
	comments []string
	// writes is every non-GET request, "METHOD path".
	writes []string
	posted []string
	added  []string
	closed bool
	// issueGets counts GET /issues/7; listGets counts GET /issues.
	issueGets int
	listGets  int
	requests  int
}

func (f *openSubIssuesFixture) issue() map[string]any {
	labels := make([]map[string]string, 0, len(f.labels))
	for _, l := range f.labels {
		labels = append(labels, map[string]string{"name": l})
	}
	state := "open"
	if f.closed {
		state = "closed"
	}
	out := map[string]any{"number": 7, "state": state, "body": "body", "user": map[string]string{"login": "reporter", "type": "User"}, "labels": labels}
	if f.summary != nil {
		out["sub_issues_summary"] = f.summary
	}
	return out
}

func (f *openSubIssuesFixture) server() *httptest.Server {
	const repo = "/repos/acme/widgets"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.requests++
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		}
		switch {
		case r.Method == "GET" && r.URL.Path == repo+"/issues":
			f.listGets++
			out := []map[string]any{}
			if !f.closed {
				out = append(out, f.issue())
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "GET" && r.URL.Path == repo+"/issues/7":
			f.issueGets++
			_ = json.NewEncoder(w).Encode(f.issue())
		case r.Method == "GET" && r.URL.Path == repo+"/issues/7/comments":
			out := make([]map[string]string, 0, len(f.comments))
			for _, b := range f.comments {
				out = append(out, map[string]string{"body": b})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "GET" && r.URL.Path == repo+"/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && r.URL.Path == repo+"/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "Fixes #7", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/other", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "acme/widgets"}},
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/labels/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"name": strings.TrimPrefix(r.URL.Path, repo+"/labels/")})
		case r.Method == "POST" && r.URL.Path == repo+"/issues/7/comments":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.posted = append(f.posted, req["body"])
			f.comments = append(f.comments, req["body"])
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.posted), "body": req["body"]})
		case r.Method == "POST" && r.URL.Path == repo+"/issues/7/labels":
			var req []string
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.added = append(f.added, req...)
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == "PATCH" && r.URL.Path == repo+"/issues/7":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["state"] != "closed" || req["state_reason"] != "completed" {
				f.t.Errorf("patch = %+v, want closed/completed", req)
			}
			f.closed = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, repo+"/issues/7/labels/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// openSubIssuesPR is the merged PR the fixture serves as acme/widgets#42.
func openSubIssuesPR(body string) *gh.PullRequest {
	pr := mergedTestPR(42, "hive[bot]", "scanner/other", body)
	pr.Base.Repo.FullName = gh.Ptr("acme/widgets")
	return pr
}

// openSubIssuesHeldLines counts the INFO lines that say an issue was left
// alone for its open sub-issues, and checks each names what it is about.
func openSubIssuesHeldLines(t *testing.T, logs string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "issue has open sub-issues") {
			continue
		}
		n++
		for _, want := range []string{"level=INFO", "issue=7", "pr=42", "open=", "total=3"} {
			if !strings.Contains(line, want) {
				t.Fatalf("held line lacks %q: %s", want, line)
			}
		}
	}
	return n
}

// The issue read by the function itself (no sweep listing). An issue with an
// open sub-issue gets no "Fixed by" comment, no awaiting comment, no
// hive/likely-done and no close; with every sub-issue closed, or none, it is
// commented on, labelled and closed exactly as before.
func TestCloseOnMergeLeavesIssueWithOpenSubIssuesAlone(t *testing.T) {
	closing := openSubIssuesPR("Fixes #7")
	for _, tc := range []struct {
		name       string
		summary    *IssueChildren
		labels     []string
		pr         *gh.PullRequest
		wantAction CloseOnMergeAction
		wantReason string
		wantPosted []string
		wantAdded  []string
		wantClosed bool
		wantHeld   bool
		// wantIssueGets is checked when non-zero.
		wantIssueGets int
	}{
		{name: "three sub-issues, none completed: nothing written", summary: &IssueChildren{Total: 3}, pr: closing,
			wantAction: CloseOnMergeSkipped, wantReason: "open_sub_issues", wantHeld: true, wantIssueGets: 1},
		{name: "one of three still open: nothing written", summary: &IssueChildren{Total: 3, Completed: 2}, pr: closing,
			wantAction: CloseOnMergeSkipped, wantReason: "open_sub_issues", wantHeld: true, wantIssueGets: 1},
		{name: "confirmation path, open sub-issues: no awaiting comment, no likely-done", summary: &IssueChildren{Total: 3}, labels: []string{"needs-human"}, pr: closing,
			wantAction: CloseOnMergeSkipped, wantReason: "open_sub_issues", wantHeld: true},
		{name: "every sub-issue completed: commented and closed as before", summary: &IssueChildren{Total: 3, Completed: 3}, pr: closing,
			wantAction: CloseOnMergeClosed, wantReason: "closed_completed", wantPosted: []string{closeOnMergeCloseComment(closing)}, wantClosed: true, wantIssueGets: 2},
		{name: "no sub-issues (empty summary): commented and closed as before", summary: &IssueChildren{}, pr: closing,
			wantAction: CloseOnMergeClosed, wantReason: "closed_completed", wantPosted: []string{closeOnMergeCloseComment(closing)}, wantClosed: true, wantIssueGets: 2},
		{name: "no summary block: commented and closed as before", pr: closing,
			wantAction: CloseOnMergeClosed, wantReason: "closed_completed", wantPosted: []string{closeOnMergeCloseComment(closing)}, wantClosed: true, wantIssueGets: 2},
		{name: "confirmation path, every sub-issue completed: awaiting comment and likely-done as before", summary: &IssueChildren{Total: 3, Completed: 3}, labels: []string{"needs-human"}, pr: closing,
			wantAction: CloseOnMergeAwaiting, wantReason: "needs_confirmation_or_human", wantPosted: []string{closeOnMergeAwaitingComment(closing)}, wantAdded: []string{LikelyDoneLabel}},
		{name: "confirmation path, no summary block: awaiting comment and likely-done as before", labels: []string{"needs-human"}, pr: closing,
			wantAction: CloseOnMergeAwaiting, wantReason: "needs_confirmation_or_human", wantPosted: []string{closeOnMergeAwaitingComment(closing)}, wantAdded: []string{LikelyDoneLabel}},
		{name: "refs-only PR on a parent is still partially addressed", summary: &IssueChildren{Total: 3}, pr: openSubIssuesPR("Refs #7"),
			wantAction: CloseOnMergeSkipped, wantReason: "partially_addressed", wantAdded: []string{PartiallyAddressedLabel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &openSubIssuesFixture{t: t, summary: tc.summary, labels: tc.labels}
			srv := f.server()
			defer srv.Close()
			var logBuf strings.Builder
			c := newTestClient(t, srv, "acme", []string{"widgets"})
			res := c.closeIssueForMergedPRClaim(context.Background(), "acme/widgets", 7, tc.pr, true, CloseOnMergeOptions{
				Identity: HiveIdentity{AppLogin: "hive[bot]"},
				Logger:   slog.New(slog.NewTextHandler(&logBuf, nil)),
			})
			if res.Action != tc.wantAction || res.Reason != tc.wantReason {
				t.Fatalf("result = %s/%s, want %s/%s", res.Action, res.Reason, tc.wantAction, tc.wantReason)
			}
			if strings.Join(f.posted, "\x00") != strings.Join(tc.wantPosted, "\x00") {
				t.Fatalf("posted = %q, want %q", f.posted, tc.wantPosted)
			}
			if strings.Join(f.added, ",") != strings.Join(tc.wantAdded, ",") {
				t.Fatalf("labels added = %v, want %v", f.added, tc.wantAdded)
			}
			if f.closed != tc.wantClosed {
				t.Fatalf("closed = %v, want %v", f.closed, tc.wantClosed)
			}
			if tc.wantHeld && len(f.writes) != 0 {
				t.Fatalf("an issue with open sub-issues was written to: %v", f.writes)
			}
			held := 0
			if tc.wantHeld {
				held = 1
			}
			if got := openSubIssuesHeldLines(t, logBuf.String()); got != held {
				t.Fatalf("held log lines = %d, want %d:\n%s", got, held, logBuf.String())
			}
			if tc.wantHeld && !res.Settles() {
				t.Fatalf("result %+v must settle the claim, like tracking_issue", res)
			}
			// The sub-issue summary rides on the read the function already
			// made: one GET when nothing is written, plus CloseIssue's own
			// pre-close read when the issue is closed.
			if tc.wantIssueGets != 0 && f.issueGets != tc.wantIssueGets {
				t.Fatalf("GET issues/7 = %d, want %d", f.issueGets, tc.wantIssueGets)
			}
		})
	}
}

// The ledger sweep with one eligible claim reads the issue itself. A parent
// with open sub-issues is settled without a write; while settled it costs no
// request; re-verified past the recheck window it is still left alone; and
// once its sub-issues are all closed and it is still open, the next
// re-verification comments and closes as close-on-merge always did.
func TestCloseOnMergeLedgerSweepSettlesIssueWithOpenSubIssues(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	f := &openSubIssuesFixture{t: t, summary: &IssueChildren{Total: 3}}
	srv := f.server()
	defer srv.Close()
	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	if err := ledger.Record(IssueClaim{Repo: "acme/widgets", Issue: 7, PRNumber: 42, PRRepo: "acme/widgets", MergedPR: true}); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	now := time.Now()
	var logBuf strings.Builder
	opts := CloseOnMergeOptions{
		Identity: HiveIdentity{AppLogin: "hive[bot]"},
		Logger:   slog.New(slog.NewTextHandler(&logBuf, nil)),
		Now:      func() time.Time { return now },
	}
	wantHeld := func(pass string, s CloseOnMergeSweep) {
		t.Helper()
		if len(s.Results) != 1 || s.Results[0].Action != CloseOnMergeSkipped || s.Results[0].Reason != "open_sub_issues" || s.Marked != 1 {
			t.Fatalf("%s = %+v, want one settled open_sub_issues skip", pass, s)
		}
		if len(f.writes) != 0 {
			t.Fatalf("%s wrote to an issue with open sub-issues: %v", pass, f.writes)
		}
	}

	wantHeld("first sweep", c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts))
	if f.listGets != 0 || f.issueGets != 1 {
		t.Fatalf("first sweep: list GETs = %d, issue GETs = %d, want 0 and 1 (the read v6 already made)", f.listGets, f.issueGets)
	}
	if got := openSubIssuesHeldLines(t, logBuf.String()); got != 1 {
		t.Fatalf("held log lines = %d, want 1:\n%s", got, logBuf.String())
	}

	// Inside the recheck window the settled claim costs nothing.
	requests := f.requests
	now = now.Add(time.Hour)
	if s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts); len(s.Results) != 0 || s.SkippedSettled != 1 {
		t.Fatalf("settled sweep = %+v, want the claim skipped", s)
	}
	if f.requests != requests {
		t.Fatalf("settled sweep made %d request(s)", f.requests-requests)
	}

	// Past the window it is verified again, and still left alone.
	now = now.Add(DefaultCloseOnMergeRecheck)
	wantHeld("recheck sweep", c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts))

	// The sub-issues close; the parent is still open at the next recheck.
	f.summary = &IssueChildren{Total: 3, Completed: 3}
	now = now.Add(2 * DefaultCloseOnMergeRecheck)
	s := c.CloseOnMergeLedgerSweep(context.Background(), ledger, opts)
	if len(s.Results) != 1 || s.Results[0].Action != CloseOnMergeClosed || s.Results[0].Reason != "closed_completed" {
		t.Fatalf("sweep after the sub-issues closed = %+v, want the issue closed", s)
	}
	if !f.closed || len(f.posted) != 1 || !strings.Contains(f.posted[0], CloseOnMergeMarker) || !strings.Contains(f.posted[0], "Fixed by #42") {
		t.Fatalf("closed = %v, posted = %q, want one Fixed-by comment and the close", f.closed, f.posted)
	}
}

// The ledger sweep with two eligible claims in a repo, and the backfill,
// hand the function an issue from their open-issue listing. The sub-issue
// summary on that listing row is what holds the parent: no issue read is
// added, and with every sub-issue closed the listed issue is closed with
// only CloseIssue's own read, as before.
func TestCloseOnMergeListedIssueWithOpenSubIssues(t *testing.T) {
	for _, tc := range []struct {
		name          string
		summary       *IssueChildren
		wantAction    CloseOnMergeAction
		wantReason    string
		wantClosed    bool
		wantIssueGets int
	}{
		{name: "open sub-issues", summary: &IssueChildren{Total: 3}, wantAction: CloseOnMergeSkipped, wantReason: "open_sub_issues"},
		{name: "every sub-issue completed", summary: &IssueChildren{Total: 3, Completed: 3}, wantAction: CloseOnMergeClosed, wantReason: "closed_completed", wantClosed: true, wantIssueGets: 1},
		{name: "no summary block", wantAction: CloseOnMergeClosed, wantReason: "closed_completed", wantClosed: true, wantIssueGets: 1},
	} {
		check := func(t *testing.T, f *openSubIssuesFixture, results []CloseOnMergeResult) {
			t.Helper()
			if len(results) != 1 || results[0].Issue != 7 || results[0].PR != 42 || results[0].Action != tc.wantAction || results[0].Reason != tc.wantReason {
				t.Fatalf("results = %+v, want #7 for PR 42 %s/%s", results, tc.wantAction, tc.wantReason)
			}
			if f.closed != tc.wantClosed {
				t.Fatalf("closed = %v, want %v", f.closed, tc.wantClosed)
			}
			if !tc.wantClosed && len(f.writes) != 0 {
				t.Fatalf("an issue with open sub-issues was written to: %v", f.writes)
			}
			if tc.wantClosed && (len(f.posted) != 1 || !strings.Contains(f.posted[0], "Fixed by #42")) {
				t.Fatalf("posted = %q, want the one Fixed-by comment", f.posted)
			}
			if f.listGets != 1 || f.issueGets != tc.wantIssueGets {
				t.Fatalf("list GETs = %d, issue GETs = %d, want 1 and %d", f.listGets, f.issueGets, tc.wantIssueGets)
			}
		}
		t.Run("ledger sweep/"+tc.name, func(t *testing.T) {
			defer resetPRDetailCacheForTest(nil, 0)()
			f := &openSubIssuesFixture{t: t, summary: tc.summary}
			srv := f.server()
			defer srv.Close()
			ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
			// Two eligible claims make the sweep list the repo's open issues
			// once; #8 is not in the listing, so only #7 is worked.
			for issue, pr := range map[int]int{7: 42, 8: 43} {
				if err := ledger.Record(IssueClaim{Repo: "acme/widgets", Issue: issue, PRNumber: pr, PRRepo: "acme/widgets", MergedPR: true}); err != nil {
					t.Fatal(err)
				}
			}
			c := newTestClient(t, srv, "acme", []string{"widgets"})
			check(t, f, c.CloseOnMergeForLedger(context.Background(), ledger, CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}))
		})
		t.Run("backfill/"+tc.name, func(t *testing.T) {
			defer resetPRDetailCacheForTest(nil, 0)()
			f := &openSubIssuesFixture{t: t, summary: tc.summary, labels: []string{LikelyDoneLabel}, comments: []string{"covered by PR #42"}}
			srv := f.server()
			defer srv.Close()
			c := newTestClient(t, srv, "acme", []string{"widgets"})
			opts := CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}}
			check(t, f, c.CloseOnMergeBackfill(context.Background(), opts))
			if tc.wantClosed {
				return
			}
			// A second backfill pass writes nothing either.
			f.listGets = 0
			check(t, f, c.CloseOnMergeBackfill(context.Background(), opts))
		})
	}
}
