package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestParseCoveredByPRComments(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []int
	}{
		{name: "empty", body: "", want: nil},
		{name: "no match", body: "see #12 for context", want: nil},
		{name: "plain", body: "covered by PR #42", want: []int{42}},
		{name: "open pull request form", body: "This is Covered By open pull request #7.", want: []int{7}},
		{name: "multiple", body: "covered by PR #1\ncovered by pr #2", want: []int{1, 2}},
		{name: "zero is dropped", body: "covered by PR #0", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCoveredByPRComments(tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("parse(%q) = %v, want %v", tc.body, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parse(%q) = %v, want %v", tc.body, got, tc.want)
				}
			}
		})
	}
}

func TestStrconvAtoi(t *testing.T) {
	if _, err := strconvAtoi(""); err == nil {
		t.Fatal("empty string should error")
	}
	if _, err := strconvAtoi("12a"); err == nil {
		t.Fatal("non-digit should error")
	}
	if n, err := strconvAtoi("0042"); err != nil || n != 42 {
		t.Fatalf("strconvAtoi(0042) = %d, %v", n, err)
	}
}

func TestCloseOnMergeShortSHA(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "unknown",
		"   ":                "unknown",
		"abc":                "abc",
		"0123456789abcdef00": "0123456789ab",
	} {
		if got := closeOnMergeShortSHA(in); got != want {
			t.Fatalf("shortSHA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCloseOnMergeNilGuards(t *testing.T) {
	var nilClient *Client
	if got := nilClient.CloseOnMergeForLedger(context.Background(), nil, CloseOnMergeOptions{}); got != nil {
		t.Fatalf("nil client ledger = %v, want nil", got)
	}
	if got := nilClient.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{}); got != nil {
		t.Fatalf("nil client backfill = %v, want nil", got)
	}
	c := &Client{}
	if got := c.CloseOnMergeForLedger(context.Background(), NewClaimLedger(filepath.Join(t.TempDir(), "l.json"), nil), CloseOnMergeOptions{}); got != nil {
		t.Fatalf("client without API ledger = %v, want nil", got)
	}
	if got := c.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{}); got != nil {
		t.Fatalf("client without API backfill = %v, want nil", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	live := newTestClient(t, srv, "o", []string{"r"})
	if got := live.CloseOnMergeForLedger(context.Background(), nil, CloseOnMergeOptions{}); got != nil {
		t.Fatalf("nil ledger = %v, want nil", got)
	}
	if got, err := live.getPullRequestForCloseOnMerge(context.Background(), "o/r", 0); err == nil || got != nil {
		t.Fatalf("invalid PR ref = %v, %v; want error", got, err)
	}
}

func TestCloseOnMergeForLedgerFiltersAndCloses(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	var patched []int
	var prGets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/o/r/pulls/"):
			prGets = append(prGets, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/500") {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "Fixes #7", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/fix-7", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "body": "body", "labels": []map[string]string{}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			patched = append(patched, 7)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	ledger := NewClaimLedger(filepath.Join(t.TempDir(), "ledger.json"), nil)
	for _, claim := range []IssueClaim{
		// Strong merged claim: the only one that should be acted on.
		{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedPR: true},
		// Open PR claim: not merged, skipped before any API call.
		{Repo: "o/r", Issue: 8, PRNumber: 43, PRRepo: "o/r"},
		// Weak (Refs) merged claim: deliberately left alone.
		{Repo: "o/r", Issue: 9, PRNumber: 44, PRRepo: "o/r", MergedPR: true, Reference: true},
		// Merged claim whose PR cannot be read: logged and skipped.
		{Repo: "o/r", Issue: 10, PRNumber: 500, PRRepo: "o/r", MergedPR: true},
		// Merged claim missing a PR number: skipped.
		{Repo: "o/r", Issue: 11, PRNumber: 0, PRRepo: "o/r", MergedPR: true},
	} {
		if err := ledger.Record(claim); err != nil {
			t.Fatalf("record %+v: %v", claim, err)
		}
	}

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	c := newTestClient(t, srv, "o", []string{"r"})
	results := c.CloseOnMergeForLedger(context.Background(), ledger, CloseOnMergeOptions{
		Identity: HiveIdentity{AppLogin: "hive[bot]"},
		Logger:   logger,
	})
	if len(results) != 1 {
		t.Fatalf("results = %+v, want exactly one", results)
	}
	if results[0].Issue != 7 || results[0].PR != 42 || results[0].Action != CloseOnMergeClosed {
		t.Fatalf("result = %+v, want issue 7 closed by PR 42", results[0])
	}
	if results[0].MergeCommit != "abcdef0123456789" {
		t.Fatalf("merge commit = %q", results[0].MergeCommit)
	}
	if len(patched) != 1 {
		t.Fatalf("patched = %v, want one close", patched)
	}
	if len(prGets) != 2 {
		t.Fatalf("PR GETs = %v, want /pulls/42 and /pulls/500 only", prGets)
	}
	if !strings.Contains(logBuf.String(), "could not read merged PR") {
		t.Fatalf("unreadable PR was not logged: %s", logBuf.String())
	}
}

func TestCloseOnMergeDecisionSkipAndFailurePaths(t *testing.T) {
	type tc struct {
		name           string
		author         string
		trusted        TrustedPRAuthorFunc
		body           string
		head           string
		claimMetadata  bool
		issueState     string
		issueGetStatus int
		commentStatus  int
		patchStatus    int
		markerStatus   int
		wantAction     CloseOnMergeAction
		wantReason     string
		wantWrongRefs  bool
	}
	cases := []tc{
		{name: "untrusted author skipped", author: "stranger", body: "Fixes #7", claimMetadata: true, wantAction: CloseOnMergeSkipped, wantReason: "untrusted_author"},
		{name: "trusted author func admits external author", author: "friend", trusted: func(repo, login string) bool { return repo == "o/r" && login == "friend" }, body: "Fixes #7", claimMetadata: true, wantAction: CloseOnMergeClosed, wantReason: "closed_completed"},
		{name: "no claim metadata without closing keyword skipped", author: "hive[bot]", body: "unrelated", head: "scanner/other", claimMetadata: false, wantAction: CloseOnMergeSkipped, wantReason: "no_closing_or_claim_metadata"},
		{name: "refs-only claim flagged wrong refs but still closes", author: "hive[bot]", body: "Refs #7", head: "scanner/other", claimMetadata: true, wantAction: CloseOnMergeClosed, wantReason: "closed_completed", wantWrongRefs: true},
		{name: "issue read failure noops", author: "hive[bot]", body: "Fixes #7", claimMetadata: true, issueGetStatus: http.StatusInternalServerError, wantAction: CloseOnMergeNoop, wantReason: "issue_read_failed"},
		{name: "closed issue already done", author: "hive[bot]", body: "Fixes #7", claimMetadata: true, issueState: "closed", wantAction: CloseOnMergeAlreadyDone, wantReason: "issue_not_open"},
		{name: "close comment failure noops", author: "hive[bot]", body: "Fixes #7", claimMetadata: true, commentStatus: http.StatusForbidden, wantAction: CloseOnMergeNoop, wantReason: "close_comment_failed"},
		{name: "close patch failure noops", author: "hive[bot]", body: "Fixes #7", claimMetadata: true, patchStatus: http.StatusForbidden, wantAction: CloseOnMergeNoop, wantReason: "close_failed"},
		{name: "marker lookup failure treated as absent", author: "hive[bot]", body: "Fixes #7", claimMetadata: true, markerStatus: http.StatusInternalServerError, wantAction: CloseOnMergeClosed, wantReason: "closed_completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
					if tc.issueGetStatus != 0 {
						w.WriteHeader(tc.issueGetStatus)
						return
					}
					state := tc.issueState
					if state == "" {
						state = "open"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": state, "body": "body", "labels": []map[string]string{}})
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
					if tc.markerStatus != 0 {
						w.WriteHeader(tc.markerStatus)
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]string{})
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
					if tc.commentStatus != 0 {
						w.WriteHeader(tc.commentStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
				case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
					if tc.patchStatus != 0 {
						w.WriteHeader(tc.patchStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
				case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
					w.WriteHeader(http.StatusNotFound)
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			var logBuf strings.Builder
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))
			c := newTestClient(t, srv, "o", []string{"r"})
			head := tc.head
			if head == "" {
				head = "scanner/other"
			}
			pr := mergedTestPR(42, tc.author, head, tc.body)
			res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 7, pr, tc.claimMetadata, CloseOnMergeOptions{
				Identity:      HiveIdentity{AppLogin: "hive[bot]"},
				TrustedAuthor: tc.trusted,
				Logger:        logger,
			})
			if res.Action != tc.wantAction || res.Reason != tc.wantReason {
				t.Fatalf("result = %s/%s, want %s/%s", res.Action, res.Reason, tc.wantAction, tc.wantReason)
			}
			if res.WrongRefs != tc.wantWrongRefs {
				t.Fatalf("WrongRefs = %v, want %v", res.WrongRefs, tc.wantWrongRefs)
			}
			if tc.wantWrongRefs && !strings.Contains(logBuf.String(), "non-closing Refs") {
				t.Fatalf("wrong-refs warning not logged: %s", logBuf.String())
			}
			if tc.wantReason == "issue_read_failed" && !strings.Contains(logBuf.String(), "reading issue failed") {
				t.Fatalf("issue read failure not logged: %s", logBuf.String())
			}
		})
	}
}

func TestCloseOnMergeDecisionNilPRAndMissingIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 7, nil, true, CloseOnMergeOptions{})
	if res.Action != CloseOnMergeNoop || res.Reason != "missing_pr_or_issue" {
		t.Fatalf("nil PR result = %+v", res)
	}
	res = c.closeIssueForMergedPRClaim(context.Background(), "o/r", 0, mergedTestPR(42, "hive[bot]", "x", "Fixes #7"), true, CloseOnMergeOptions{})
	if res.Action != CloseOnMergeNoop || res.Reason != "missing_pr_or_issue" {
		t.Fatalf("zero issue result = %+v", res)
	}
}

func TestCloseOnMergeAwaitingCommentFailureNoops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "body": "body", "labels": []map[string]string{{"name": "needs-human"}}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			w.WriteHeader(http.StatusForbidden)
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	res := c.closeIssueForMergedPRClaim(context.Background(), "o/r", 7, mergedTestPR(42, "hive[bot]", "scanner/fix-7", "Fixes #7"), true, CloseOnMergeOptions{
		Identity: HiveIdentity{AppLogin: "hive[bot]"},
	})
	if res.Action != CloseOnMergeNoop || res.Reason != "awaiting_comment_failed" {
		t.Fatalf("result = %+v, want awaiting_comment_failed", res)
	}
}

func TestCloseOnMergeBackfillSkipPaths(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	// Issue 7: candidate label, linked via a "covered by PR #42" comment, PR
	// 42 unmerged -> noop pr_not_merged.
	// Issue 8: candidate label, timeline links PR 43 whose body neither
	// closes nor refs 8 and whose branch does not name it; the issue's
	// comments do not name it either -> skipped no_closing_or_claim_metadata.
	// Issue 9: no candidate label -> never looked at.
	// Issue 10: candidate, linked PR 500 cannot be read -> logged, skipped.
	// A second page of issues is requested and returns an error, which ends
	// the loop without failing the sweep.
	var issuePages int
	var logBuf strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			issuePages++
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Link", `<`+testServerBase(r)+`/repos/o/r/issues?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "state": "open", "labels": []map[string]string{{"name": "claimed"}}},
				{"number": 8, "state": "open", "labels": []map[string]string{{"name": LikelyDoneLabel}}},
				{"number": 9, "state": "open", "labels": []map[string]string{{"name": "bug"}}},
				{"number": 10, "state": "open", "labels": []map[string]string{{"name": CoveredByPRLabel}}},
				{"number": 11, "state": "open", "labels": []map[string]string{{"name": CoveredByPRLabel}}, "pull_request": map[string]any{"url": "u"}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "covered by PR #42"}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "Fixes #7", "merged": false, "state": "open",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/fix-7", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/8/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "no link here"}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/8/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"event": "labeled"},
				{"event": "cross-referenced", "source": map[string]any{"issue": map[string]any{"number": 99}}},
				{"event": "cross-referenced", "source": map[string]any{"issue": map[string]any{"number": 43, "pull_request": map[string]any{"url": "u"}}}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/43":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 43, "title": "fix", "body": "unrelated work", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/other", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/10/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "covered by PR #500"}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/10/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	results := c.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{
		Identity: HiveIdentity{AppLogin: "hive[bot]"},
		Logger:   slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	if issuePages != 2 {
		t.Fatalf("issue pages requested = %d, want 2", issuePages)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want two", results)
	}
	byIssue := map[int]CloseOnMergeResult{}
	for _, r := range results {
		byIssue[r.Issue] = r
	}
	if r := byIssue[7]; r.PR != 42 || r.Action != CloseOnMergeNoop || r.Reason != "pr_not_merged" {
		t.Fatalf("issue 7 result = %+v", r)
	}
	if r := byIssue[8]; r.PR != 43 || r.Action != CloseOnMergeSkipped || r.Reason != "no_closing_or_claim_metadata" {
		t.Fatalf("issue 8 result = %+v", r)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "reading linked PR failed") {
		t.Fatalf("unreadable linked PR not logged: %s", logs)
	}
	if !strings.Contains(logs, "listing open issues failed") {
		t.Fatalf("issue page failure not logged: %s", logs)
	}
}

func TestCloseOnMergeBackfillCommentLinkCountsAsClaim(t *testing.T) {
	defer resetPRDetailCacheForTest(nil, 0)()
	// PR 42 is merged by a trusted author but its body does not reference
	// issue 7 at all; the issue carries a "covered by PR #42" comment, which
	// is accepted as claim metadata and the issue is closed.
	var patched bool
	var commentListPages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "state": "open", "labels": []map[string]string{{"name": CoveredByPRLabel}}},
			})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			commentListPages++
			if r.URL.Query().Get("page") == "2" {
				_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "covered by PR #42"}})
				return
			}
			w.Header().Set("Link", `<`+testServerBase(r)+`/repos/o/r/issues/7/comments?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "first page, nothing relevant"}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "labels": []map[string]string{}})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix", "body": "unrelated", "merged": true,
				"merged_at": "2026-10-09T12:00:00Z", "merge_commit_sha": "abcdef0123456789",
				"user": map[string]string{"login": "hive[bot]"},
				"head": map[string]string{"ref": "scanner/other", "sha": "headsha"},
				"base": map[string]any{"repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			patched = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/labels/"):
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	results := c.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}})
	if len(results) != 1 || results[0].Action != CloseOnMergeClosed || results[0].PR != 42 {
		t.Fatalf("results = %+v, want issue closed by PR 42", results)
	}
	if !patched {
		t.Fatal("issue was not closed")
	}
	if commentListPages < 2 {
		t.Fatalf("comment pages fetched = %d, want pagination to page 2", commentListPages)
	}
}

func TestCloseOnMergeHasMarkerPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "GET" || r.URL.Path != "/repos/o/r/issues/7/comments" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "x\n" + CloseOnMergeMarker}})
			return
		}
		w.Header().Set("Link", `<`+testServerBase(r)+`/repos/o/r/issues/7/comments?page=2>; rel="next"`)
		_ = json.NewEncoder(w).Encode([]map[string]string{{"body": "no marker"}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	if !c.hasCloseOnMergeMarker(context.Background(), "o", "r", 7) {
		t.Fatal("marker on page 2 was not found")
	}
	// PR 0 never appears in a comment; both pages are walked and the miss is reported.
	if closeOnMergeIssueCommentsLinkPR(context.Background(), c, "o", "r", 7, 0) {
		t.Fatal("unexpected link for PR 0")
	}
}

func TestCloseOnMergeCachedPRUsable(t *testing.T) {
	if closeOnMergeCachedPRUsable(nil) {
		t.Fatal("nil PR should not be usable")
	}
	if closeOnMergeCachedPRUsable(&gh.PullRequest{Number: gh.Ptr(1), Body: gh.Ptr("  "), User: &gh.User{Login: gh.Ptr("x")}}) {
		t.Fatal("blank body should not be usable")
	}
	if closeOnMergeCachedPRUsable(&gh.PullRequest{Number: gh.Ptr(1), Body: gh.Ptr("b")}) {
		t.Fatal("missing author should not be usable")
	}
	if !closeOnMergeCachedPRUsable(mergedTestPR(1, "a", "h", "b")) {
		t.Fatal("full PR should be usable")
	}
}

func TestCloseOnMergeResolverNilAndCrossRepo(t *testing.T) {
	if closeOnMergePRClaimsIssue(nil, "o/r", "o/r", 7) || closeOnMergePRRefsIssue(nil, "o/r", "o/r", 7) {
		t.Fatal("nil PR must not claim or ref")
	}
	pr := mergedTestPR(42, "hive[bot]", "scanner/fix-7", "work")
	if closeOnMergePRClaimsIssue(pr, "o/r", "o/r", 0) || closeOnMergePRRefsIssue(pr, "o/r", "o/r", 0) {
		t.Fatal("issue 0 must not be claimed")
	}
	// Branch-name claims are only trusted within the same repository.
	if closeOnMergePRClaimsIssue(pr, "o/r", "o/other", 7) {
		t.Fatal("branch-name claim must not cross repositories")
	}
}

func TestCloseOnMergeComments(t *testing.T) {
	pr := mergedTestPR(42, "hive[bot]", "h", "b")
	closeBody := closeOnMergeCloseComment(pr)
	if !strings.HasPrefix(closeBody, CloseOnMergeMarker) || !strings.Contains(closeBody, "#42") || !strings.Contains(closeBody, "0123456789ab") {
		t.Fatalf("close comment = %q", closeBody)
	}
	awaiting := closeOnMergeAwaitingComment(pr)
	if !strings.Contains(awaiting, closeOnMergeAwaitingMarker) || !strings.Contains(awaiting, "#42") {
		t.Fatalf("awaiting comment = %q", awaiting)
	}
}

// testServerBase rebuilds the test server's base URL from the inbound request so
// Link headers point back at the same httptest server.
func testServerBase(r *http.Request) string {
	return "http://" + r.Host
}
