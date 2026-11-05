package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The report lists #490 (closed on a Refs-only PR) and an issue closed on a
// branch claim while marked verified-open; not one a closing PR closed (even
// when marked verified-open), one a person closed or only quoted the hive's
// note on, a PR, or one closed before the window.
func TestCloseOnMergeWrongClosesReport(t *testing.T) {
	closedAt := "2026-11-01T12:22:26Z"
	marker := func(pr string) []map[string]any {
		return []map[string]any{{"user": map[string]string{"login": "hive[bot]"}, "body": CloseOnMergeMarker + "\nFixed by #" + pr + " (merged 5bd573a1565f). Closing. Reply /reopen if the problem persists."}}
	}
	pr := func(n int, body string) map[string]any {
		return map[string]any{"number": n, "title": "t", "body": body, "merged": true, "merged_at": "2026-11-01T10:25:46Z",
			"user": map[string]string{"login": "hive[bot]"}, "head": map[string]string{"ref": "hive/x"}, "base": map[string]any{"repo": map[string]string{"full_name": "o/r"}}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/repos/o/r/issues":
			if r.URL.Query().Get("state") != "closed" || r.URL.Query().Get("since") == "" {
				t.Errorf("list query = %s", r.URL.RawQuery)
			}
			_ = enc.Encode([]map[string]any{
				{"number": 490, "state": "closed", "closed_at": closedAt, "title": "parity batch"},
				{"number": 491, "state": "closed", "closed_at": closedAt, "labels": []map[string]string{{"name": VerifiedOpenLabel}}},
				{"number": 492, "state": "closed", "closed_at": closedAt},
				{"number": 493, "state": "closed", "closed_at": closedAt},
				{"number": 494, "state": "closed", "closed_at": "2026-09-01T00:00:00Z"},
				{"number": 495, "state": "closed", "closed_at": closedAt, "pull_request": map[string]any{"url": "u"}},
				{"number": 496, "state": "closed", "closed_at": closedAt, "labels": []map[string]string{{"name": VerifiedOpenLabel}}},
				{"number": 497, "state": "closed", "closed_at": closedAt},
			})
		case "/repos/o/r/issues/490/comments":
			_ = enc.Encode(marker("860"))
		case "/repos/o/r/issues/491/comments":
			_ = enc.Encode(marker("861"))
		case "/repos/o/r/issues/492/comments":
			_ = enc.Encode(marker("862"))
		case "/repos/o/r/issues/493/comments":
			_ = enc.Encode([]map[string]any{{"user": map[string]string{"login": "maintainer"}, "body": "done"}})
		case "/repos/o/r/issues/496/comments":
			_ = enc.Encode(marker("863"))
		case "/repos/o/r/issues/497/comments":
			// A person quoting the hive's close note is not the hive closing it.
			_ = enc.Encode([]map[string]any{{"user": map[string]string{"login": "someone"}, "body": CloseOnMergeMarker + "\nFixed by #864 (merged 5bd573a1565f)."}})
		case "/repos/o/r/pulls/864":
			_ = enc.Encode(pr(864, "Refs #497"))
		case "/repos/o/r/pulls/863":
			_ = enc.Encode(pr(863, "Closes #496"))
		case "/repos/o/r/pulls/860":
			_ = enc.Encode(pr(860, "Refs #490 — ten outputs remain unpublished."))
		case "/repos/o/r/pulls/861":
			_ = enc.Encode(pr(861, "Publishes the rest."))
		case "/repos/o/r/pulls/862":
			_ = enc.Encode(pr(862, "Closes #492"))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	c.SetAppBotLogin("hive[bot]")
	audit, err := c.CloseOnMergeWrongCloses(context.Background(), CloseOnMergeAuditOptions{Since: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	rows := audit.Items
	if audit.Truncated || audit.Inspected != 6 {
		t.Fatalf("inspected=%d truncated=%v, want 6 closed issues in the window and no truncation", audit.Inspected, audit.Truncated)
	}
	if len(rows) != 2 || rows[0].Issue != 490 || rows[0].PR != 860 || rows[0].Reason != "pr_leaves_issue_open" || rows[0].Repo != "o/r" ||
		rows[1].Issue != 491 || rows[1].Reason != "issue_marked_open" {
		t.Fatalf("rows = %+v, want #490 (pr_leaves_issue_open) and #491 (issue_marked_open)", rows)
	}
}

// MaxIssues stops the scan: only that many closed issues cost a comments
// read, and the result says it was truncated.
func TestCloseOnMergeWrongClosesCap(t *testing.T) {
	commentReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/o/r/issues":
			var issues []map[string]any
			for n := 1; n <= 5; n++ {
				issues = append(issues, map[string]any{"number": n, "state": "closed", "closed_at": "2026-11-01T12:00:00Z"})
			}
			_ = json.NewEncoder(w).Encode(issues)
		case strings.HasSuffix(r.URL.Path, "/comments"):
			commentReads++
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"r"})
	audit, err := c.CloseOnMergeWrongCloses(context.Background(), CloseOnMergeAuditOptions{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), MaxIssues: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !audit.Truncated || audit.Inspected != 2 || commentReads != 2 {
		t.Fatalf("truncated=%v inspected=%d comment reads=%d, want true/2/2", audit.Truncated, audit.Inspected, commentReads)
	}
}
