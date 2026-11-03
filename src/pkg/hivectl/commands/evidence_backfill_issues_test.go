package commands

import (
	"context"
	"encoding/json"
	"fmt"
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
			{"number": 1, "created_at": "2026-10-06T00:00:00Z", "updated_at": "2026-10-06T00:00:00Z", "closed_at": nil, "body": "", "user": map[string]string{"login": "person", "type": "User"}},
			{"number": 2, "created_at": "2026-10-07T00:00:00Z", "updated_at": "2026-10-07T00:00:00Z", "closed_at": nil, "pull_request": map[string]string{"url": "pr"}, "user": map[string]string{"login": "person", "type": "User"}},
			{"number": 3, "created_at": "2026-09-01T00:00:00Z", "updated_at": "2026-10-08T00:00:00Z", "closed_at": "2026-10-08T00:00:00Z", "body": "", "user": map[string]string{"login": "bot[bot]", "type": "Bot"}},
			{"number": 4, "created_at": "2026-10-09T00:00:00Z", "updated_at": "2026-10-09T00:00:00Z", "closed_at": nil, "body": "", "user": map[string]string{"login": "hive-agent", "type": "User"}},
		})
	}))
	defer server.Close()
	fetcher := newBackfillFetcher(server.URL, "")
	issues, err := fetcher.listIssues(context.Background(), "o/r", since, until)
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

func TestListIssuesSendsSinceAndStopsAtOlderPage(t *testing.T) {
	since := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 0, 14)
	var sinces, sorts []string
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sinces = append(sinces, q.Get("since"))
		sorts = append(sorts, q.Get("sort"))
		pages = append(pages, q.Get("page"))
		var batch []map[string]any
		for i := 0; i < evidenceBackfillPerPage; i++ {
			n := 1000 - len(pages)*evidenceBackfillPerPage - i
			updated, closed := "2026-10-10T00:00:00Z", any(nil)
			switch q.Get("page") {
			case "1":
				if i == 0 {
					closed = "2026-10-08T00:00:00Z"
				}
			default:
				updated = "2026-10-01T00:00:00Z"
			}
			batch = append(batch, map[string]any{"number": n, "created_at": "2026-09-01T00:00:00Z", "updated_at": updated, "closed_at": closed, "body": "",
				"user": map[string]string{"login": fmt.Sprintf("u%d", n), "type": "User"}})
		}
		_ = json.NewEncoder(w).Encode(batch)
	}))
	defer server.Close()
	fetcher := newBackfillFetcher(server.URL, "")
	issues, err := fetcher.listIssues(context.Background(), "o/r", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("requested pages %v, want 2 (stop at the first page entirely older than since)", pages)
	}
	for i := range sinces {
		if sinces[i] != "2026-10-05T00:00:00Z" || sorts[i] != "updated" {
			t.Fatalf("request %d since=%q sort=%q", i, sinces[i], sorts[i])
		}
	}
	if len(issues) != evidenceBackfillPerPage {
		t.Fatalf("got %d issues, want only the in-range first page", len(issues))
	}
	if issues[0].ClosedAt == nil || !issues[0].ClosedAt.Equal(time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("in-range older close missing: %+v", issues[0])
	}
}
