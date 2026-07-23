package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHeartbeatStoresTokenTotal verifies that a heartbeat carrying tokens_24h
// is stored on the registry entry's TotalTokens24h, so heartbeat-only hives
// (reached via heartbeat, not hub-kubectl) show real token consumption on the
// My Hives token column.
func TestHeartbeatStoresTokenTotal(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.hubSecret = ""

	const wantTokens = int64(1234567)
	payload := `{
		"hive_id":"kellyaa",
		"org":"testorg",
		"primary_repo":"repo",
		"tokens_24h":1234567
	}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHeartbeat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var got int64 = -1
	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "kellyaa" {
			got = h.TotalTokens24h
		}
	}
	srv.mu.RUnlock()

	if got != wantTokens {
		t.Fatalf("TotalTokens24h = %d, want %d", got, wantTokens)
	}
}

// TestHeartbeatTokenTotalClamped verifies the token total is clamped to a sane
// maximum so a corrupt/hostile spoke cannot inject an absurd value.
func TestHeartbeatTokenTotalClamped(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.hubSecret = ""

	payload := `{
		"hive_id":"clamphive",
		"org":"testorg",
		"primary_repo":"repo",
		"tokens_24h":999999999999
	}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHeartbeat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var got int64 = -1
	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "clamphive" {
			got = h.TotalTokens24h
		}
	}
	srv.mu.RUnlock()

	// clampInt64(payload.Tokens24h, 0, 100_000_000_000) — 100B ceiling is a
	// sanity guard against a corrupt/hostile spoke; real hives legitimately
	// exceed 100M/day so the cap must be far above realistic usage.
	const maxTokens = int64(100_000_000_000)
	if got != maxTokens {
		t.Fatalf("clamped TotalTokens24h = %d, want %d", got, maxTokens)
	}
}
