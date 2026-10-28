package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestHiveUnblockRefreshesSnapshotsWithoutWebhook(t *testing.T) {
	s := newTestServer()
	status := minimalPayload()
	status.Repos = []FrontendRepo{{Name: "r", Full: "o/r", Issues: 1,
		ActionableIssues: []any{github.Issue{Repo: "o/r", Number: 7, State: "open", Labels: []string{"blocked"}}},
	}}
	s.status = status
	var paths []string
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/1":
			_, _ = w.Write([]byte(`{"number":1,"state":"closed","merged":true}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/o/r/issues/7/labels/blocked":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = w.Write([]byte(`{"id":1}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": "Unblocked", "state": "open",
				"labels": []map[string]string{{"name": "kind/bug"}}, "updated_at": time.Now().UTC().Format(time.RFC3339),
				"user": map[string]string{"login": "reporter"}})
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ghSrv.Close)
	client := github.NewClientForTest(ghSrv.URL, "o", []string{"r"}, s.logger)
	var handed []github.WebhookItemSnapshot
	s.deps = &Dependencies{GHClient: client, ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) {
		handed = append(handed, snaps...)
	}}
	client.SetItemRefreshHook(s.RefreshHiveItem)
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer

	res := github.ReconcileBlockedIssues(context.Background(), client, &github.ActionableResult{
		WorkDetailsByRepo: map[string]github.RepoWorkDetails{"o/r": {Issues: []github.RepoWorkIssue{
			{Repo: "o/r", Number: 7, Labels: []string{"blocked"}, Bucket: "filtered", Reason: "blocked on o/r#1"},
		}}},
	}, s.logger)
	if res.Unblocked != 1 || res.Errors != 0 {
		t.Fatalf("unblock = %+v", res)
	}
	if !s.stageTrackerCache.stale || s.webhookRefresh.pendingCount() != 1 || timers.count() != 1 {
		t.Fatal("unblock must invalidate the tracker and queue a targeted read without a webhook")
	}
	if len(handed) != 0 {
		t.Fatal("read must wait for the debounce window")
	}
	// Repeated local hints share the webhook refresher's one-read-per-item queue.
	s.RefreshHiveItem("o/r", 7, false)
	s.FlushWebhookTrackerRefresh()
	if len(handed) != 1 || handed[0].Repo != "o/r" || handed[0].Number != 7 || handed[0].IsPR ||
		!reflect.DeepEqual(handed[0].Labels, []string{"kind/bug"}) || handed[0].DeliveryID != "hive-write:item-refresh" {
		t.Fatalf("governor snapshot patches = %+v", handed)
	}
	got := s.status.Repos[0].ActionableIssues[0].(github.Issue)
	if got.Title != "Unblocked" || !reflect.DeepEqual(got.Labels, []string{"kind/bug"}) {
		t.Fatalf("published issue = %+v", got)
	}
	if !reflect.DeepEqual(status.Repos[0].ActionableIssues[0].(github.Issue).Labels, []string{"blocked"}) {
		t.Fatal("refresh mutated the previously published snapshot")
	}
	if want := []string{"GET /repos/o/r/issues/7/comments", "GET /repos/o/r/pulls/1", "DELETE /repos/o/r/issues/7/labels/blocked",
		"POST /repos/o/r/issues/7/comments", "GET /repos/o/r/issues/7"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("GitHub calls = %v, want %v (no issue list or webhook)", paths, want)
	}
}

func TestRefreshHiveItemValidationAndDisabledReads(t *testing.T) {
	var nilServer *Server
	nilServer.RefreshHiveItem("o/r", 7, false)
	s := newTestServer()
	s.RefreshHiveItem(" ", 7, false)
	s.RefreshHiveItem("o/r", 0, false)
	if s.stageTrackerGen != 0 || s.webhookRefresh != nil {
		t.Fatal("invalid item must not invalidate or queue a read")
	}
	disabled := false
	cfg := &config.Config{}
	cfg.Dashboard.StageTracker.WebhookRefresh = &disabled
	s.deps = &Dependencies{Config: cfg}
	s.RefreshHiveItem("o/r", 7, false)
	if !s.stageTrackerCache.stale || s.webhookRefresh != nil {
		t.Fatal("disabled targeted reads must still invalidate the tracker cache")
	}
}
