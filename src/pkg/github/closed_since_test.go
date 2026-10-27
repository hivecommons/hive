package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Every issue / PR delivery hands the receiver the payload's own state for
// the item it embeds (hivecommons/hive#14159, #14064): the stage tracker
// drops a row whose payload says closed on whatever action carried it.
func TestHandleWebhookEventCarriesItemStateHint(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    *WebhookItemHint
	}{
		{"14159 unlabeled after close (not_planned)", "issues",
			`{"action":"unlabeled","label":{"name":"stage/015-triage-needed"},"issue":{"number":14159,"state":"closed","state_reason":"not_planned","closed_at":"2026-10-25T11:56:36Z","updated_at":"2026-10-25T12:00:41Z"},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 14159, State: "closed", UpdatedAt: time.Date(2026, 10, 25, 12, 0, 41, 0, time.UTC)}},
		{"14064 unlabeled after close (completed)", "issues",
			`{"action":"unlabeled","label":{"name":"stage/045-needs-decision"},"issue":{"number":14064,"state":"closed","state_reason":"completed","updated_at":"2026-10-25T12:00:57Z"},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 14064, State: "closed", UpdatedAt: time.Date(2026, 10, 25, 12, 0, 57, 0, time.UTC)}},
		{"comment on closed issue", "issue_comment",
			`{"action":"created","issue":{"number":14159,"state":"closed","updated_at":"2026-10-25T11:56:43Z"},"comment":{"body":"x"},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 14159, State: "closed", UpdatedAt: time.Date(2026, 10, 25, 11, 56, 43, 0, time.UTC)}},
		{"open issue labeled", "issues",
			`{"action":"labeled","issue":{"number":9,"state":"open"},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 9, State: "open"}},
		{"PR via issues feed", "issues",
			`{"action":"closed","issue":{"number":9,"state":"closed","pull_request":{"url":"x"}},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 9, IsPR: true, State: "closed"}},
		{"merged PR", "pull_request",
			`{"action":"closed","pull_request":{"number":12,"state":"closed","merged":true,"merged_at":"2026-10-25T10:00:00Z","updated_at":"2026-10-25T10:00:01Z","head":{"sha":"abc"}},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 12, IsPR: true, State: "closed", Merged: true, UpdatedAt: time.Date(2026, 10, 25, 10, 0, 1, 0, time.UTC)}},
		{"review on open PR", "pull_request_review",
			`{"action":"submitted","pull_request":{"number":12,"state":"open"},"repository":{"full_name":"org/repo"}}`,
			&WebhookItemHint{Number: 12, IsPR: true, State: "open"}},
		{"check_run names no item", "check_run",
			`{"action":"completed","check_run":{"head_sha":"abc"},"repository":{"full_name":"org/repo"}}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := setupWebhookCacheTest(t)
			res, err := c.HandleWebhookEvent(tc.event, []byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(res.Item, tc.want) {
				t.Fatalf("item hint = %+v, want %+v", res.Item, tc.want)
			}
			wantClosed := tc.want != nil && (tc.want.Merged || tc.want.State == "closed")
			if res.Item.Closed() != wantClosed {
				t.Fatalf("Closed() = %v, want %v", res.Item.Closed(), wantClosed)
			}
		})
	}
	var nilHint *WebhookItemHint
	if nilHint.Closed() {
		t.Fatal("nil hint is not closed")
	}
}

// ListClosedSince: one list call per page over state=closed&since, both
// kinds, newest first, closed snapshots only.
func TestListClosedSince(t *testing.T) {
	// ListClosedSince refuses while the process-wide REST budget is critical
	// or the rate-limit caution window is engaged; isolate from state another
	// test left behind.
	resetRESTAccountingForTest(t, time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC))
	ResetRateLimitPacingForTest()
	t.Cleanup(ResetRateLimitPacingForTest)
	var (
		pathsMu sync.Mutex
		paths   []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.String())
		pathsMu.Unlock()
		q := r.URL.Query()
		if q.Get("state") != "closed" || q.Get("since") != "2026-10-25T11:50:00Z" || q.Get("sort") != "updated" || q.Get("direction") != "desc" || q.Get("per_page") != "100" {
			t.Errorf("query = %v", q)
		}
		switch q.Get("page") {
		case "1":
			w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 14064, "title": "[DCO] post-merge trailer failures on v5", "state": "closed", "state_reason": "completed", "updated_at": "2026-10-25T12:00:57Z",
					"user": map[string]any{"login": "clubanderson"}, "labels": []map[string]any{{"name": "help wanted"}, {"name": "ci-dco"}}, "html_url": "https://github.com/acme/console/issues/14064"},
				{"number": 14159, "title": "✨ feature: evaluate Microsoft NVX", "state": "closed", "state_reason": "not_planned", "updated_at": "2026-10-25T12:00:41Z",
					"user": map[string]any{"login": "clubanderson"}, "labels": []map[string]any{{"name": "needs-triage"}}},
				{"number": 14100, "title": "merged pr", "state": "closed", "updated_at": "2026-10-25T11:58:00Z", "pull_request": map[string]any{"url": "x"}},
				{"number": 14101, "title": "lies", "state": "open", "updated_at": "2026-10-25T11:57:00Z"},
			})
		case "2":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 14050, "title": "older", "state": "closed", "updated_at": "2026-10-25T11:51:00Z"}})
		default:
			t.Errorf("unexpected page %q", q.Get("page"))
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "acme", []string{"console"}, nil)
	since := time.Date(2026, 10, 25, 11, 50, 0, 0, time.UTC)
	got, err := c.ListClosedSince(context.Background(), "acme/console", since, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("list calls = %v, want two pages", paths)
	}
	var numbers []int
	for _, s := range got {
		if s.Open() || s.Repo != "acme/console" {
			t.Fatalf("snapshot %+v must be closed and keyed to the repo", s)
		}
		numbers = append(numbers, s.Number)
	}
	if !reflect.DeepEqual(numbers, []int{14064, 14159, 14100, 14050}) {
		t.Fatalf("numbers = %v", numbers)
	}
	if !got[2].IsPR || got[0].IsPR || got[0].Title == "" || len(got[0].Labels) != 2 || !got[0].UpdatedAt.Equal(time.Date(2026, 10, 25, 12, 0, 57, 0, time.UTC)) {
		t.Fatalf("snapshots = %+v", got)
	}

	pathsMu.Lock()
	paths = nil
	pathsMu.Unlock()
	if _, err := c.ListClosedSince(context.Background(), "acme/console", since, 1); err != nil || len(paths) != 1 {
		t.Fatalf("maxPages=1: err=%v calls=%v", err, paths)
	}
	if _, err := (*Client)(nil).ListClosedSince(context.Background(), "acme/console", since, 1); err == nil {
		t.Fatal("nil client must error")
	}
}
