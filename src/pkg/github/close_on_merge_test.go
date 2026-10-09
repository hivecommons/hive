package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
}
