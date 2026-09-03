package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
)

// --- handleBudgetHistory ---

// A hive that has never seen a window roll and has no live status must still
// answer with an empty windows array (never null) and no "current" key.
func TestBudgetHistory_EmptyHistoryNoStatus(t *testing.T) {
	s, _ := apiServer(t)

	rec := doGet(s, "/api/budget/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	windows, ok := body["windows"].([]interface{})
	if !ok {
		t.Fatalf("windows should be an array, got %T (%v)", body["windows"], body["windows"])
	}
	if len(windows) != 0 {
		t.Errorf("windows = %v, want empty", windows)
	}
	if _, ok := body["current"]; ok {
		t.Errorf("current should be absent when no status has been published, got %v", body["current"])
	}
}

// With a live status carrying an open window, the report includes a "current"
// block with the open window's spend and both bounds, alongside the closed rows.
func TestBudgetHistory_CurrentWindowFromStatus(t *testing.T) {
	s, _ := apiServer(t)

	s.SeedBudgetWindowHistory([]collect.BudgetWindowEntry{
		{WindowStart: 1000, WindowEnd: 2000, Limit: 500, Used: 500, PctUsed: 100, Exhausted: true},
	})
	s.statusMu.Lock()
	s.status = &StatusPayload{Budget: FrontendBudget{
		WeeklyBudget:   1_000_000,
		Used:           250_000,
		PctUsed:        25,
		Exhausted:      false,
		WindowStartsAt: "2026-08-24T00:00:00Z",
		WindowEndsAt:   "2026-08-31T00:00:00Z",
	}}
	s.statusMu.Unlock()

	rec := doGet(s, "/api/budget/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)

	windows, ok := body["windows"].([]interface{})
	if !ok || len(windows) != 1 {
		t.Fatalf("windows = %v, want 1 closed row", body["windows"])
	}
	current, ok := body["current"].(map[string]interface{})
	if !ok {
		t.Fatalf("current missing or wrong type: %v", body["current"])
	}
	if got := current["limit"].(float64); got != 1_000_000 {
		t.Errorf("current.limit = %v, want 1000000", got)
	}
	if got := current["used"].(float64); got != 250_000 {
		t.Errorf("current.used = %v, want 250000", got)
	}
	if got := current["pctUsed"].(float64); got != 25 {
		t.Errorf("current.pctUsed = %v, want 25", got)
	}
	if got := current["exhausted"].(bool); got {
		t.Errorf("current.exhausted = true, want false")
	}
	if got := current["windowStart"]; got != "2026-08-24T00:00:00Z" {
		t.Errorf("current.windowStart = %v", got)
	}
	if got := current["windowEnd"]; got != "2026-08-31T00:00:00Z" {
		t.Errorf("current.windowEnd = %v", got)
	}
}

// When no weekly limit is set the status carries empty window bounds; the
// current block must omit windowStart/windowEnd rather than emit empty strings.
func TestBudgetHistory_CurrentWindowOmitsEmptyBounds(t *testing.T) {
	s, _ := apiServer(t)

	s.statusMu.Lock()
	s.status = &StatusPayload{Budget: FrontendBudget{Exhausted: true}}
	s.statusMu.Unlock()

	rec := doGet(s, "/api/budget/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decodeJSON(t, rec)
	current, ok := body["current"].(map[string]interface{})
	if !ok {
		t.Fatalf("current missing: %v", body)
	}
	if _, ok := current["windowStart"]; ok {
		t.Errorf("windowStart should be omitted when unset, got %v", current["windowStart"])
	}
	if _, ok := current["windowEnd"]; ok {
		t.Errorf("windowEnd should be omitted when unset, got %v", current["windowEnd"])
	}
	if got := current["exhausted"].(bool); !got {
		t.Errorf("current.exhausted = false, want true")
	}
}

// --- document/knowledge handler 503 gates ---

// Every knowledge-backed endpoint must refuse with 503 when the server has no
// dependencies at all (ensureKnowledge false), instead of dereferencing nil.
func TestKnowledgeHandlers_NilDepsServiceUnavailable(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewServer(0, logger) // no RegisterAPI: s.deps stays nil

	handlers := map[string]http.HandlerFunc{
		"documents list":    s.handleDocumentsList,
		"documents import":  s.handleDocumentsImport,
		"document get":      s.handleDocumentGet,
		"document delete":   s.handleDocumentDelete,
		"document reimport": s.handleDocumentReimport,
		"cleanup orphans":   s.handleCleanupOrphans,
		"context7 search":   s.handleContext7Search,
	}
	for name, h := range handlers {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", name, rec.Code)
		}
	}
}
