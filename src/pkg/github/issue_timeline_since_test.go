package github

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIssueTimelineSinceFetchesAndFiltersEvents(t *testing.T) {
	since := time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	newer := since.Add(time.Minute)
	older := since.Add(-time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/issues/7/timeline" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"event": "labeled", "created_at": older.Format(time.RFC3339), "label": map[string]any{"name": "old"}},
			{"event": "cross-referenced", "created_at": newer.Format(time.RFC3339), "source": map[string]any{"issue": map[string]any{"number": 12, "pull_request": map[string]any{"url": "u"}}}, "actor": map[string]any{"login": "hive[bot]", "type": "Bot"}},
			{"event": "referenced", "created_at": newer.Add(time.Minute).Format(time.RFC3339), "commit_id": "abc123", "user": map[string]any{"login": "alice", "type": "User"}},
		})
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.Default())
	got, err := c.IssueTimelineSince(t.Context(), "acme/widgets", 7, since)
	if err != nil {
		t.Fatalf("IssueTimelineSince() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %#v, want 2", got)
	}
	if got[0].Event != "cross-referenced" || got[0].SourcePR != 12 || !got[0].ActorIsBot {
		t.Fatalf("cross-reference event = %#v", got[0])
	}
	if got[1].Event != "referenced" || got[1].CommitID != "abc123" || got[1].Actor != "alice" || got[1].ActorIsBot {
		t.Fatalf("referenced event = %#v", got[1])
	}
}

func TestIssueTimelineSinceNilClient(t *testing.T) {
	var c *Client
	if _, err := c.IssueTimelineSince(t.Context(), "acme/widgets", 1, time.Now()); err != ErrNoGitHubClient {
		t.Fatalf("IssueTimelineSince nil error = %v", err)
	}
}
