package worksource

// Tests for the GitHub Projects v2 reconcile cache (#13379): the GraphQL
// listing runs on the cadence, not per eval cycle; projects_v2_item
// deliveries patch one item; a failed re-list serves the last good listing.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testProjectNodeID = "PVT_board1"

// projectsStub answers the listing query from pages (one call per page,
// in order, wrapping around on later listings) and the node query from
// nodes[id]; it counts both.
type projectsStub struct {
	mu        sync.Mutex
	pages     [][]string // pages[listing][page]
	listings  int
	pageCalls int
	nodes     map[string]string
	nodeCalls []string
	nodeFail  int
	listFail  bool
}

func (st *projectsStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(body.Query, "node(id: $id)") {
			id, _ := body.Variables["id"].(string)
			st.nodeCalls = append(st.nodeCalls, id)
			if st.nodeFail != 0 {
				w.WriteHeader(st.nodeFail)
				return
			}
			node, ok := st.nodes[id]
			if !ok {
				node = "null"
			}
			fmt.Fprintf(w, `{"data":{"node":%s}}`, node)
			return
		}
		if st.listFail {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		cursor, _ := body.Variables["cursor"].(string)
		if cursor == "" {
			st.listings++
		}
		listing := st.pages[(st.listings-1)%len(st.pages)]
		page := 0
		if cursor != "" {
			fmt.Sscanf(cursor, "cursor-%d", &page)
		}
		if page >= len(listing) {
			t.Errorf("unexpected page %d of listing %d", page, st.listings)
			return
		}
		st.pageCalls++
		fmt.Fprint(w, listing[page])
	}
}

func boardPageJSON(hasNext bool, cursor string, items ...string) string {
	return fmt.Sprintf(`{"data":{"organization":{"projectV2":{"id":%q,"items":{
		"pageInfo":{"hasNextPage":%t,"endCursor":%q},
		"nodes":[%s]
	}}}}}`, testProjectNodeID, hasNext, cursor, strings.Join(items, ","))
}

// nodeJSON is itemJSON plus the project id the node query returns.
func nodeJSON(number int, title, status string, archived bool) string {
	item := itemJSON("ISSUE", number, title, status, "", "my-org/repo-a")
	item = strings.TrimSpace(item)
	return item[:len(item)-1] + fmt.Sprintf(`, "isArchived": %t, "project": {"id": %q}}`, archived, testProjectNodeID)
}

func itemEventJSON(action, nodeID, projectID string) []byte {
	return []byte(fmt.Sprintf(`{"action":%q,"projects_v2_item":{"id":1,"node_id":%q,"project_node_id":%q,"content_type":"Issue"},"organization":{"login":"my-org"}}`, action, nodeID, projectID))
}

func newCachedTestSource(t *testing.T, st *projectsStub, now *time.Time) (*githubProjectsSource, func()) {
	t.Helper()
	ResetGitHubProjectsCacheForTest()
	srv := httptest.NewServer(st.handler(t))
	src := newTestSource(srv.URL, nil).(*githubProjectsSource)
	src.nowFn = func() time.Time { return *now }
	return src, srv.Close
}

func titles(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.Title)
	}
	return out
}

func TestGitHubProjectsListsOnReconcileCadenceNotPerCycle(t *testing.T) {
	st := &projectsStub{pages: [][]string{
		{
			boardPageJSON(true, "cursor-1", itemJSON("ISSUE", 1, "one", "Todo", "", "my-org/repo-a")),
			boardPageJSON(false, "", itemJSON("ISSUE", 2, "two", "Todo", "", "my-org/repo-a")),
		},
		{boardPageJSON(false, "", itemJSON("ISSUE", 3, "three", "Todo", "", "my-org/repo-a"))},
	}}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	src, closeSrv := newCachedTestSource(t, st, &now)
	defer closeSrv()

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if got := titles(issues); strings.Join(got, ",") != "one,two" {
		t.Fatalf("issues=%v", got)
	}
	if st.listings != 1 || st.pageCalls != 2 {
		t.Fatalf("first cycle: listings=%d pages=%d", st.listings, st.pageCalls)
	}

	// Later eval cycles inside the cadence, each with a fresh source (as
	// FromConfig builds one per cycle): no GraphQL call.
	for i := 0; i < 3; i++ {
		now = now.Add(2 * time.Minute)
		again := newTestSource(src.cfg.BaseURL, nil).(*githubProjectsSource)
		again.nowFn = func() time.Time { return now }
		issues, err = again.ListIssues(context.Background())
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		if got := titles(issues); strings.Join(got, ",") != "one,two" {
			t.Fatalf("cycle %d issues=%v", i, got)
		}
	}
	if st.listings != 1 || st.pageCalls != 2 {
		t.Fatalf("cycles within the cadence must not list: listings=%d pages=%d", st.listings, st.pageCalls)
	}

	// Past the cadence the full listing runs again and overwrites.
	now = now.Add(githubProjectsReconcileInterval)
	issues, err = src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.listings != 2 || strings.Join(titles(issues), ",") != "three" {
		t.Fatalf("reconcile: listings=%d issues=%v", st.listings, titles(issues))
	}
}

func TestGitHubProjectsItemWebhookPatchesCache(t *testing.T) {
	st := &projectsStub{
		pages: [][]string{{boardPageJSON(false, "",
			itemJSON("ISSUE", 1, "one", "Todo", "", "my-org/repo-a"),
			itemJSON("ISSUE", 2, "two", "Todo", "", "my-org/repo-a"),
		)}},
		nodes: map[string]string{
			"item-2": nodeJSON(2, "two renamed", "In Progress", false),
			"item-3": nodeJSON(3, "three", "Todo", false),
			"item-4": nodeJSON(4, "four", "Todo", true),
		},
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	src, closeSrv := newCachedTestSource(t, st, &now)
	defer closeSrv()
	ctx := context.Background()

	// Before the first listing a delivery has no board to patch.
	patch, err := ApplyProjectsV2ItemEvent(ctx, itemEventJSON("edited", "item-2", testProjectNodeID))
	if err != nil || patch.Matched || len(st.nodeCalls) != 0 {
		t.Fatalf("unlisted board: patch=%+v err=%v nodeCalls=%v", patch, err, st.nodeCalls)
	}
	if _, err := src.ListIssues(ctx); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	// edited: one node read, item replaced in place.
	patch, err = ApplyProjectsV2ItemEvent(ctx, itemEventJSON("edited", "item-2", testProjectNodeID))
	if err != nil || !patch.Matched || !patch.Upserted {
		t.Fatalf("edited: patch=%+v err=%v", patch, err)
	}
	issues, _ := src.ListIssues(ctx)
	if got := titles(issues); strings.Join(got, ",") != "one,two renamed" {
		t.Fatalf("after edit issues=%v", got)
	}
	if issues[1].Number != 2 || st.listings != 1 || len(st.nodeCalls) != 1 || st.nodeCalls[0] != "item-2" {
		t.Fatalf("edit must be one node read and no re-list: listings=%d nodeCalls=%v", st.listings, st.nodeCalls)
	}

	// created: appended.
	if patch, err = ApplyProjectsV2ItemEvent(ctx, itemEventJSON("created", "item-3", testProjectNodeID)); err != nil || !patch.Upserted {
		t.Fatalf("created: patch=%+v err=%v", patch, err)
	}
	// deleted: removed without a read.
	reads := len(st.nodeCalls)
	if patch, err = ApplyProjectsV2ItemEvent(ctx, itemEventJSON("deleted", "item-1", testProjectNodeID)); err != nil || !patch.Removed {
		t.Fatalf("deleted: patch=%+v err=%v", patch, err)
	}
	if len(st.nodeCalls) != reads {
		t.Fatalf("deleted must not read: %v", st.nodeCalls)
	}
	// archived: re-read says archived, removed.
	if patch, err = ApplyProjectsV2ItemEvent(ctx, itemEventJSON("created", "item-4", testProjectNodeID)); err != nil || !patch.Matched || patch.Upserted {
		t.Fatalf("archived create: patch=%+v err=%v", patch, err)
	}
	// reordered on a gone node: removed/unchanged, no error.
	if patch, err = ApplyProjectsV2ItemEvent(ctx, itemEventJSON("reordered", "item-9", testProjectNodeID)); err != nil || !patch.Matched || !patch.Unchanged {
		t.Fatalf("gone node: patch=%+v err=%v", patch, err)
	}
	issues, _ = src.ListIssues(ctx)
	if got := titles(issues); strings.Join(got, ",") != "two renamed,three" {
		t.Fatalf("after patches issues=%v", got)
	}
	if st.listings != 1 {
		t.Fatalf("patches must never re-list: listings=%d", st.listings)
	}

	// Another board's item is not ours.
	patch, err = ApplyProjectsV2ItemEvent(ctx, itemEventJSON("edited", "item-2", "PVT_other"))
	if err != nil || patch.Matched {
		t.Fatalf("other board: patch=%+v err=%v", patch, err)
	}
	// Bad payloads are errors, not patches.
	if _, err := ApplyProjectsV2ItemEvent(ctx, []byte(`{"action":"exploded","projects_v2_item":{"node_id":"x","project_node_id":"y"}}`)); err == nil {
		t.Fatal("unknown action should error")
	}
	if _, err := ApplyProjectsV2ItemEvent(ctx, []byte(`not json`)); err == nil {
		t.Fatal("bad json should error")
	}
	if _, err := ApplyProjectsV2ItemEvent(ctx, []byte(`{"action":"edited"}`)); err == nil {
		t.Fatal("payload without item should error")
	}
	// A failed re-read surfaces as an error and leaves the item as it was.
	st.mu.Lock()
	st.nodeFail = http.StatusBadGateway
	st.mu.Unlock()
	if _, err := ApplyProjectsV2ItemEvent(ctx, itemEventJSON("edited", "item-2", testProjectNodeID)); err == nil {
		t.Fatal("failed node read should error")
	}
	issues, _ = src.ListIssues(ctx)
	if got := titles(issues); strings.Join(got, ",") != "two renamed,three" {
		t.Fatalf("failed patch must not change the cache: %v", got)
	}
}

func TestGitHubProjectsFailedReconcileServesLastGood(t *testing.T) {
	st := &projectsStub{pages: [][]string{{boardPageJSON(false, "", itemJSON("ISSUE", 1, "one", "Todo", "", "my-org/repo-a"))}}}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	src, closeSrv := newCachedTestSource(t, st, &now)
	defer closeSrv()
	ctx := context.Background()
	if _, err := src.ListIssues(ctx); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	st.mu.Lock()
	st.listFail = true
	st.mu.Unlock()
	now = now.Add(githubProjectsReconcileInterval + time.Minute)
	issues, err := src.ListIssues(ctx)
	if err != nil || strings.Join(titles(issues), ",") != "one" {
		t.Fatalf("failed reconcile should serve last good: err=%v issues=%v", err, titles(issues))
	}
	if !src.cacheEntry().stale {
		t.Fatal("entry should be marked stale")
	}
	now = now.Add(githubProjectsLastGoodMaxAge)
	if _, err := src.ListIssues(ctx); err == nil {
		t.Fatal("last good past its max age should surface the error")
	}
	st.mu.Lock()
	st.listFail = false
	st.mu.Unlock()
	if _, err := src.ListIssues(ctx); err != nil || src.cacheEntry().stale {
		t.Fatalf("recovery: err=%v stale=%v", err, src.cacheEntry().stale)
	}
}
