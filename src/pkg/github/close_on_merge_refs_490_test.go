package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// projectbluefin/utah-packages#490: PR #860 (branch hive/490-…) said "Refs
// #490" and the hive's coverage comment said it would not close the issue,
// yet the backfill sweep closed #490 when #860 merged. The backfill passes
// the configured repo name ("utah-packages"); the Refs check compared it
// with the parsed owner/repo and found no reference, so the guard let the
// close through. The client here is configured the same way: org "o",
// repo "r".
func runBackfill490(t *testing.T, prNum int, body string, labels []string) (CloseOnMergeResult, bool) {
	t.Helper()
	var mu sync.Mutex
	closed := false
	issueLabels := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		issueLabels = append(issueLabels, map[string]string{"name": l})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case r.Method == "GET" && p == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 7, "state": "open", "labels": append([]map[string]string{{"name": CoveredByPRLabel}}, issueLabels...)}})
		case r.Method == "GET" && p == "/repos/o/r/issues/7/timeline":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"event":  "cross-referenced",
				"source": map[string]any{"issue": map[string]any{"number": prNum, "pull_request": map[string]any{"url": "u"}}},
			}})
		case r.Method == "GET" && p == "/repos/o/r/issues/7/comments":
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == "GET" && p == "/repos/o/r/pulls/"+strconv.Itoa(prNum):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": prNum, "title": "parity batch", "body": body, "merged": true,
				"merged_at": "2026-11-01T10:25:46Z", "merge_commit_sha": "5bd573a1565f0000",
				"user":      map[string]string{"login": "hive[bot]"},
				"merged_by": map[string]string{"login": "hive[bot]"},
				"head":      map[string]string{"ref": "hive/7-dev-tools-parity", "sha": "headsha"},
				"base":      map[string]any{"ref": "main", "repo": map[string]string{"full_name": "o/r"}},
			})
		case r.Method == "GET" && p == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "labels": issueLabels})
		case r.Method == "PATCH" && p == "/repos/o/r/issues/7":
			mu.Lock()
			closed = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == "POST" && strings.HasSuffix(p, "/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == "POST" && strings.HasSuffix(p, "/labels"):
			_, _ = w.Write([]byte(`[{"name":"x"}]`))
		case r.Method == "GET" && strings.Contains(p, "/labels/"):
			_, _ = w.Write([]byte(`{"name":"x"}`))
		case r.Method == "DELETE":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	results := c.CloseOnMergeBackfill(context.Background(), CloseOnMergeOptions{Identity: HiveIdentity{AppLogin: "hive[bot]"}})
	if len(results) == 0 {
		t.Fatalf("no result")
	}
	mu.Lock()
	defer mu.Unlock()
	return results[len(results)-1], closed
}

func TestCloseOnMergeRefsNeverCloses490(t *testing.T) {
	const (
		closes   = "Closes #7: the last outputs are published."
		refs     = "Refs #7 (the accepted parity batch) — ten outputs remain unpublished."
		partOf   = "Part of #7: publishes five of the outputs."
		leaveOpn = "Publishes five outputs; #7 stays open for the rest."
		branch   = "Publishes the remaining outputs." // claimed only by the branch name
	)
	// The PR-detail cache is process-wide, so every case gets its own PR.
	for i, tc := range []struct {
		name   string
		body   string
		labels []string
		close  bool
		reason string
	}{
		{name: "Closes", body: closes, close: true, reason: "closed_completed"},
		{name: "Refs (#490)", body: refs, reason: "partially_addressed"},
		{name: "Part of", body: partOf, reason: "partially_addressed"},
		{name: "leave-open wording", body: leaveOpn, reason: "partially_addressed"},
		{name: "branch claim only", body: branch, close: true, reason: "closed_completed"},
		{name: "Closes + verified-open", body: closes, labels: []string{VerifiedOpenLabel}, close: true, reason: "closed_completed"},
		{name: "Refs + verified-open", body: refs, labels: []string{VerifiedOpenLabel}, reason: "partially_addressed"},
		{name: "branch claim + verified-open", body: branch, labels: []string{VerifiedOpenLabel}, reason: "issue_marked_open"},
		{name: "branch claim + partially-addressed", body: branch, labels: []string{PartiallyAddressedLabel}, reason: "issue_marked_open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, closed := runBackfill490(t, 4900+i, tc.body, tc.labels)
			if closed != tc.close || res.Reason != tc.reason {
				t.Fatalf("closed=%v reason=%q (action %q), want closed=%v reason=%q", closed, res.Reason, res.Action, tc.close, tc.reason)
			}
		})
	}
}

// The close path and the coverage comment read one decision.
func TestPRLeavesIssueOpen(t *testing.T) {
	for _, tc := range []struct {
		text, prRepo, issueRepo string
		want                    bool
	}{
		{"Refs #490", "o/r", "o/r", true},
		{"Part of #490", "o/r", "o/r", true},
		{"Refs o/r#490", "o/other", "o/r", true},
		{"This leaves #490 open.", "o/r", "o/r", true},
		{"This does not close #490.", "o/r", "o/r", false}, // GitHub reads "close #490" as a keyword
		{"Closes #490", "o/r", "o/r", false},
		{"Closes #490. Refs #491", "o/r", "o/r", false},
		{"Refs #491", "o/r", "o/r", false},
		{"Refs #490", "o/r", "o/x", false},
	} {
		if got := prLeavesIssueOpen(tc.text, tc.prRepo, tc.issueRepo, 490); got != tc.want {
			t.Errorf("prLeavesIssueOpen(%q, %s → %s) = %v, want %v", tc.text, tc.prRepo, tc.issueRepo, got, tc.want)
		}
	}
}
