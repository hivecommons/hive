package hub

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// heartbeat.go — sendHeartbeat direct branches
// ============================================================

func TestSendHeartbeatNilPayload_Cov(t *testing.T) {
	if resp := sendHeartbeat(context.Background(), "http://x", func() *HeartbeatPayload { return nil }, slog.Default()); resp != nil {
		t.Error("nil payload should return nil response")
	}
}

func TestSendHeartbeatRequestBuildError_Cov(t *testing.T) {
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h1"} }
	// Control-char URL -> http.NewRequestWithContext fails.
	if resp := sendHeartbeat(context.Background(), "http://\x7f\x00", collect, slog.Default()); resp != nil {
		t.Error("bad URL should return nil response")
	}
}

func TestSendHeartbeatUnreachable_Cov(t *testing.T) {
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h1"} }
	if resp := sendHeartbeat(context.Background(), "http://127.0.0.1:1", collect, slog.Default()); resp != nil {
		t.Error("unreachable hub should return nil response")
	}
}

func TestSendHeartbeatRejected_Cov(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h1"} }
	if resp := sendHeartbeat(context.Background(), srv.URL, collect, slog.Default()); resp != nil {
		t.Error("rejected heartbeat should return nil response")
	}
}

func TestSendHeartbeatSuccessFiltersNames_Cov(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"hub_git_hash":"abc123","latest_sha":"def456"}`))
	}))
	defer srv.Close()

	collect := func() *HeartbeatPayload {
		return &HeartbeatPayload{
			HiveID: "h1",
			// One valid + one invalid entry so the name-filter loops run both branches.
			Leaderboard: []LeaderboardEntry{{GitHubUsername: "good"}, {GitHubUsername: "bad name!"}},
			Agents:      []AgentSummary{{Name: "agent1"}, {Name: "bad agent!"}},
		}
	}
	resp := sendHeartbeat(context.Background(), srv.URL, collect, slog.Default())
	if resp == nil || resp.HubGitHash != "abc123" {
		t.Errorf("expected decoded response, got %+v", resp)
	}
}
