package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

// These tests pin the server-authoritative pause/resume contract: a no-op
// transition (pausing an already-paused agent, resuming a running one) must
// return ok:true with changed:false and the authoritative state, so a client
// with a stale belief can correct itself instead of silently re-pausing an
// agent the operator was trying to start.

func decodeToggleBody(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("response is not JSON: %v (body=%s)", err, body)
	}
	return m
}

func TestHandlePause_RealTransitionReportsChangedTrue(t *testing.T) {
	s, _ := apiServer(t)
	rec := doOwnerPost(s, "/api/pause/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	m := decodeToggleBody(t, rec.Body.Bytes())
	if m["ok"] != true {
		t.Errorf("ok = %v, want true", m["ok"])
	}
	if m["changed"] != true {
		t.Errorf("changed = %v, want true on a real pause transition", m["changed"])
	}
	if m["state"] != "paused" {
		t.Errorf("state = %v, want %q", m["state"], "paused")
	}
}

func TestHandlePause_NoopReturnsChangedFalse(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doOwnerPost(s, "/api/pause/scanner", nil); rec.Code != http.StatusOK {
		t.Fatalf("first pause status = %d, want 200", rec.Code)
	}
	rec := doOwnerPost(s, "/api/pause/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second pause status = %d, want 200 (ok:true is part of the contract)", rec.Code)
	}
	m := decodeToggleBody(t, rec.Body.Bytes())
	if m["ok"] != true {
		t.Errorf("ok = %v, want true even on a no-op", m["ok"])
	}
	if m["changed"] != false {
		t.Errorf("changed = %v, want false when pausing an already-paused agent", m["changed"])
	}
	if m["state"] != "paused" {
		t.Errorf("state = %v, want %q", m["state"], "paused")
	}
}

func TestHandleResume_NoopReturnsChangedFalse(t *testing.T) {
	s, _ := apiServer(t)
	// scanner starts un-paused: resuming it is a no-op.
	rec := doOwnerPost(s, "/api/resume/scanner", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no-op resume must not touch tmux)", rec.Code)
	}
	m := decodeToggleBody(t, rec.Body.Bytes())
	if m["ok"] != true {
		t.Errorf("ok = %v, want true even on a no-op", m["ok"])
	}
	if m["changed"] != false {
		t.Errorf("changed = %v, want false when resuming a non-paused agent", m["changed"])
	}
	if m["state"] != "running" {
		t.Errorf("state = %v, want %q", m["state"], "running")
	}
}

func TestHandleResume_RealTransitionReportsChangedTrue(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doOwnerPost(s, "/api/pause/scanner", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200", rec.Code)
	}
	rec := doOwnerPost(s, "/api/resume/scanner", nil)
	// A real resume relaunches the agent session; in environments without a
	// working tmux this legitimately fails with 400 (same tolerance as
	// TestHandleResume). The contract under test is: when it DOES succeed,
	// it must report changed:true with the authoritative state.
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 200 or 400", rec.Code)
	}
	if rec.Code == http.StatusOK {
		m := decodeToggleBody(t, rec.Body.Bytes())
		if m["changed"] != true {
			t.Errorf("changed = %v, want true on a real resume transition", m["changed"])
		}
		if m["state"] != "running" {
			t.Errorf("state = %v, want %q", m["state"], "running")
		}
	}
}

func TestHandleAgentState_ReflectsAuthoritativePause(t *testing.T) {
	s, _ := apiServer(t)

	rec := doGet(s, "/api/agent-state/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	m := decodeToggleBody(t, rec.Body.Bytes())
	if m["paused"] != false || m["state"] != "running" {
		t.Errorf("fresh agent: paused = %v, state = %v; want false/running", m["paused"], m["state"])
	}

	if rec := doOwnerPost(s, "/api/pause/scanner", nil); rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200", rec.Code)
	}

	rec = doGet(s, "/api/agent-state/scanner")
	if rec.Code != http.StatusOK {
		t.Fatalf("status after pause = %d, want 200", rec.Code)
	}
	m = decodeToggleBody(t, rec.Body.Bytes())
	if m["paused"] != true || m["state"] != "paused" {
		t.Errorf("paused agent: paused = %v, state = %v; want true/paused", m["paused"], m["state"])
	}
}

func TestHandleAgentState_UnknownAgent404(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agent-state/nonexistent")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
