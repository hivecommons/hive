package dashboard

import (
	"net/http"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// TestKickStatusDeliveredResponseShape asserts the observable HTTP response shape
// when an asynchronous kick has completed delivery into the agent's pane.
func TestKickStatusDeliveredResponseShape(t *testing.T) {
	s, deps := apiServer(t)

	queuedAt := time.Date(2026, 9, 9, 14, 30, 0, 0, time.UTC)
	settledAt := time.Date(2026, 9, 9, 14, 30, 4, 500000000, time.UTC)

	deps.AgentMgr.RecordKickDispatchForTest(agent.KickDispatch{
		Agent:     "scanner",
		Phase:     agent.KickPhaseDelivered,
		QueuedAt:  queuedAt,
		SettledAt: settledAt,
	})

	rec := doGet(s, "/api/kick/scanner/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("kick status = %d, want 200", rec.Code)
	}

	body := decodeKickJSON(t, rec.Body.String())
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("expected ok=true, got %v", body["ok"])
	}
	if gotAgent, _ := body["agent"].(string); gotAgent != "scanner" {
		t.Errorf("agent = %q, want %q", gotAgent, "scanner")
	}
	if gotStatus, _ := body["status"].(string); gotStatus != kickStatusDelivered {
		t.Errorf("status = %q, want %q", gotStatus, kickStatusDelivered)
	}
	if pending, _ := body["pending"].(bool); pending {
		t.Errorf("delivered kick must not report pending: %v", body)
	}
	if gotQueued, _ := body["queuedAt"].(string); gotQueued != queuedAt.Format(time.RFC3339) {
		t.Errorf("queuedAt = %q, want %q", gotQueued, queuedAt.Format(time.RFC3339))
	}
	if gotSettled, _ := body["settledAt"].(string); gotSettled != settledAt.Format(time.RFC3339) {
		t.Errorf("settledAt = %q, want %q", gotSettled, settledAt.Format(time.RFC3339))
	}
	if _, has := body["error"]; has {
		t.Errorf("delivered kick must not have error field: %v", body)
	}
}

// TestKickStatusFailedResponseShape asserts the observable HTTP response shape
// when an asynchronous kick has permanently failed (e.g. timeout waiting for prompt).
func TestKickStatusFailedResponseShape(t *testing.T) {
	s, deps := apiServer(t)

	queuedAt := time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)
	settledAt := time.Date(2026, 9, 9, 15, 2, 0, 0, time.UTC)
	failMsg := "cli never reached prompt within 120s"

	deps.AgentMgr.RecordKickDispatchForTest(agent.KickDispatch{
		Agent:     "scanner",
		Phase:     agent.KickPhaseFailed,
		Error:     failMsg,
		QueuedAt:  queuedAt,
		SettledAt: settledAt,
	})

	rec := doGet(s, "/api/kick/scanner/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("kick status = %d, want 200", rec.Code)
	}

	body := decodeKickJSON(t, rec.Body.String())
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("expected ok=true, got %v", body["ok"])
	}
	if gotAgent, _ := body["agent"].(string); gotAgent != "scanner" {
		t.Errorf("agent = %q, want %q", gotAgent, "scanner")
	}
	if gotStatus, _ := body["status"].(string); gotStatus != kickStatusFailed {
		t.Errorf("status = %q, want %q", gotStatus, kickStatusFailed)
	}
	if pending, _ := body["pending"].(bool); pending {
		t.Errorf("failed kick must not report pending: %v", body)
	}
	if gotQueued, _ := body["queuedAt"].(string); gotQueued != queuedAt.Format(time.RFC3339) {
		t.Errorf("queuedAt = %q, want %q", gotQueued, queuedAt.Format(time.RFC3339))
	}
	if gotSettled, _ := body["settledAt"].(string); gotSettled != settledAt.Format(time.RFC3339) {
		t.Errorf("settledAt = %q, want %q", gotSettled, settledAt.Format(time.RFC3339))
	}
	if gotErr, _ := body["error"].(string); gotErr != failMsg {
		t.Errorf("error = %q, want %q", gotErr, failMsg)
	}
}

// TestKickStatusInFlightResponseShape asserts the observable HTTP response shape
// when an asynchronous kick is queued and currently in flight.
func TestKickStatusInFlightResponseShape(t *testing.T) {
	s, deps := apiServer(t)

	queuedAt := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)

	deps.AgentMgr.RecordKickDispatchForTest(agent.KickDispatch{
		Agent:    "scanner",
		Phase:    agent.KickPhasePending,
		QueuedAt: queuedAt,
	})

	rec := doGet(s, "/api/kick/scanner/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("kick status = %d, want 200", rec.Code)
	}

	body := decodeKickJSON(t, rec.Body.String())
	if ok, _ := body["ok"].(bool); !ok {
		t.Errorf("expected ok=true, got %v", body["ok"])
	}
	if gotAgent, _ := body["agent"].(string); gotAgent != "scanner" {
		t.Errorf("agent = %q, want %q", gotAgent, "scanner")
	}
	if gotStatus, _ := body["status"].(string); gotStatus != kickStatusInFlight {
		t.Errorf("status = %q, want %q", gotStatus, kickStatusInFlight)
	}
	if pending, _ := body["pending"].(bool); !pending {
		t.Errorf("in-flight kick must report pending=true: %v", body)
	}
	if gotQueued, _ := body["queuedAt"].(string); gotQueued != queuedAt.Format(time.RFC3339) {
		t.Errorf("queuedAt = %q, want %q", gotQueued, queuedAt.Format(time.RFC3339))
	}
	if _, has := body["settledAt"]; has {
		t.Errorf("in-flight kick must not have settledAt: %v", body)
	}
	if _, has := body["error"]; has {
		t.Errorf("in-flight kick must not have error field: %v", body)
	}
}

// TestKickStatusResolvesAgentAlias verifies that querying kick status with an
// agent ID resolves to the canonical agent name and retrieves its dispatch.
func TestKickStatusResolvesAgentAlias(t *testing.T) {
	srv := newFullServer(t)

	srv.deps.AgentMgr.RecordKickDispatchForTest(agent.KickDispatch{
		Agent:    "scanner",
		Phase:    agent.KickPhaseDelivered,
		QueuedAt: time.Now().UTC(),
	})

	rec := doGet(srv, "/api/kick/scan-001/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("kick status with agent ID = %d, want 200", rec.Code)
	}

	body := decodeKickJSON(t, rec.Body.String())
	if gotAgent, _ := body["agent"].(string); gotAgent != "scanner" {
		t.Errorf("resolved agent = %q, want %q", gotAgent, "scanner")
	}
	if gotStatus, _ := body["status"].(string); gotStatus != kickStatusDelivered {
		t.Errorf("status = %q, want %q", gotStatus, kickStatusDelivered)
	}
}
