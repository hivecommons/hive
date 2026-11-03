package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// countingHandler is a slog.Handler that counts records per level.
type countingHandler struct {
	mu     sync.Mutex
	counts map[slog.Level]int
	msgs   []string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil {
		h.counts = map[slog.Level]int{}
	}
	h.counts[r.Level]++
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

// digestAnchorMux is digestCommentMux with a per-PATCH counter and a listed
// body that is served unchanged regardless of PATCHes, modelling the
// eventually-consistent comment list.
func digestAnchorMux(org, repo string, comments []map[string]any, patches map[int64]int, created *int) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(comments)
		case "POST":
			*created++
			json.NewEncoder(w).Encode(map[string]any{"id": 9999, "user": map[string]any{"login": "hive-app[bot]"}})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			var id int64
			fmt.Sscanf(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/comments/%%d", org, repo), &id)
			patches[id]++
			json.NewEncoder(w).Encode(map[string]any{"id": id, "user": map[string]any{"login": "hive-app[bot]"}})
		}
	})
	return mux
}

// A human-authored digest comment NEWER than the App's own (the
// torch-spyre/spyre-inference#406 shape in #16439): the App re-pins to its
// own older comment rather than creating a duplicate, and the foreign-anchor
// warning is logged once per issue per process with the action taken — later
// cycles demote it to Debug.
func TestPostAdvisoryDigest_ForeignAnchorRepinsToAppCommentWarnsOnce(t *testing.T) {
	org, repo := "torch-spyre", "spyre-inference"
	patches := map[int64]int{}
	created := 0
	server := httptest.NewServer(digestAnchorMux(org, repo, []map[string]any{
		{"id": 5061265231, "body": advisoryDigestPrefix + " — human", "user": map[string]any{"login": "joerunde", "type": "User"}},
		{"id": 4000, "body": advisoryDigestPrefix + " — ours (stale)", "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}},
	}, patches, &created))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	h := &countingHandler{}
	c.logger = slog.New(h)

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf("%s — findings v%d", advisoryDigestPrefix, i)
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, body); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if patches[4000] != 3 {
		t.Errorf("expected the App's own comment to be edited on each changed cycle, got %d PATCHes", patches[4000])
	}
	if patches[5061265231] != 0 || created != 0 {
		t.Errorf("wrong write path: patchedForeign=%d created=%d", patches[5061265231], created)
	}
	if got := h.counts[slog.LevelWarn]; got != 1 {
		t.Errorf("foreign-anchor warning logged %d times, want exactly once; msgs=%v", got, h.msgs)
	}
	if c.advisoryDigestAnchors["torch-spyre/spyre-inference#10"] != 4000 {
		t.Errorf("anchor not persisted: %v", c.advisoryDigestAnchors)
	}
}

// With no App-authored digest on the issue a fresh one is created, and the
// created comment becomes the persisted anchor.
func TestPostAdvisoryDigest_ForeignAnchorCreatesAndPersistsAnchor(t *testing.T) {
	org, repo := "torch-spyre", "spyre-inference"
	patches := map[int64]int{}
	created := 0
	server := httptest.NewServer(digestAnchorMux(org, repo, []map[string]any{
		{"id": 5061265231, "body": advisoryDigestPrefix + " — human", "user": map[string]any{"login": "joerunde", "type": "User"}},
	}, patches, &created))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	h := &countingHandler{}
	c.logger = slog.New(h)

	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+" — new"); err != nil {
		t.Fatalf("PostAdvisoryDigest: %v", err)
	}
	if created != 1 || len(patches) != 0 {
		t.Errorf("expected exactly one CREATE and no PATCH, got created=%d patches=%v", created, patches)
	}
	if c.advisoryDigestAnchors["torch-spyre/spyre-inference#10"] != 9999 {
		t.Errorf("created comment not persisted as anchor: %v", c.advisoryDigestAnchors)
	}
	if h.counts[slog.LevelWarn] != 1 {
		t.Errorf("want one Warn, got %d", h.counts[slog.LevelWarn])
	}
}

// Identical material content re-rendered with a fresh timestamp is not
// PATCHed again, even when the listed body still shows an older digest (the
// comment list lagging the last edit).
func TestPostAdvisoryDigest_UnchangedAnchorBodySkipsEdit(t *testing.T) {
	org, repo := "testorg", "testrepo"
	patches := map[int64]int{}
	created := 0
	server := httptest.NewServer(digestAnchorMux(org, repo, []map[string]any{
		{"id": 222, "body": advisoryDigestPrefix + " — older content", "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}},
	}, patches, &created))
	defer server.Close()

	c := newAppTestClient(t, server, org, repo, "hive-app[bot]")
	first := advisoryDigestPrefix + " — 2026-08-31 04:00 UTC\n**Findings:** 2\n- a\n- b"
	second := advisoryDigestPrefix + " — 2026-08-31 04:05 UTC\n**Findings:** 2\n- a\n- b"
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, second); err != nil {
		t.Fatalf("second: %v", err)
	}
	if patches[222] != 1 {
		t.Errorf("expected exactly one PATCH for identical material content, got %d", patches[222])
	}
	if created != 0 {
		t.Errorf("unexpected CREATE: %d", created)
	}
	third := advisoryDigestPrefix + " — 2026-08-31 04:10 UTC\n**Findings:** 3\n- a\n- b\n- c"
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, third); err != nil {
		t.Fatalf("third: %v", err)
	}
	if patches[222] != 2 {
		t.Errorf("changed material content must PATCH again, got %d", patches[222])
	}
}
