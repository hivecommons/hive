package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIssueBackfillExcludesPRsAndCountsOlderCloses(t *testing.T) {
	since := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 0, 14)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("state") != "all" {
			t.Errorf("unexpected request %s", r.URL)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 1, "created_at": "2026-10-06T00:00:00Z", "closed_at": nil, "body": "", "user": map[string]string{"login": "person", "type": "User"}},
			{"number": 2, "created_at": "2026-10-07T00:00:00Z", "closed_at": nil, "pull_request": map[string]string{"url": "pr"}, "user": map[string]string{"login": "person", "type": "User"}},
			{"number": 3, "created_at": "2026-09-01T00:00:00Z", "closed_at": "2026-10-08T00:00:00Z", "body": "", "user": map[string]string{"login": "bot[bot]", "type": "Bot"}},
			{"number": 4, "created_at": "2026-10-09T00:00:00Z", "closed_at": nil, "body": "", "user": map[string]string{"login": "hive-agent", "type": "User"}},
		})
	}))
	defer server.Close()
	fetcher := newBackfillFetcher(server.URL, "")
	issues, err := fetcher.listIssues(context.Background(), "o/r", until)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 3 {
		t.Fatalf("issues = %+v", issues)
	}
	weeks := issueWeeks(issues, since, until, []string{"hive-agent"})
	if len(weeks) != 2 || weeks[0].Opened["human"] != 1 || weeks[0].Opened["agent"] != 1 || weeks[0].Closed != 1 || weeks[1].Opened["human"] != 0 {
		t.Fatalf("weeks = %+v", weeks)
	}
}
