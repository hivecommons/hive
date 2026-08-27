package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// #4041 dashboard-side provenance: pausing via the API must record WHO acted,
// and the agent payload the frontend renders must carry the full WHO/WHY/WHEN
// so a paused fleet explains itself.

func doOwnerPostAs(s *Server, path, user string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	markOwnerRequest(req)
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

// Positive control: a pause via the API records the authenticated actor on
// the agent state itself, not only in the audit log.
func TestHandlePause_RecordsActingUser(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doOwnerPostAs(s, "/api/pause/scanner", "bketelsen"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	proc, err := deps.AgentMgr.GetStatus("scanner")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if proc.PausedBy != "bketelsen" {
		t.Errorf("PausedBy = %q, want %q (actor lost on the API pause path)", proc.PausedBy, "bketelsen")
	}
	if proc.PausedTrigger != "dashboard-api" {
		t.Errorf("PausedTrigger = %q, want %q", proc.PausedTrigger, "dashboard-api")
	}
}

// Without an auth proxy the acting user resolves to "local" — same rule the
// audit log has always used, so the two surfaces can never disagree.
func TestHandlePause_NoUserHeaderRecordsLocal(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doOwnerPostAs(s, "/api/pause/scanner", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	proc, _ := deps.AgentMgr.GetStatus("scanner")
	if proc.PausedBy != "local" {
		t.Errorf("PausedBy = %q, want %q", proc.PausedBy, "local")
	}
}

// The agent payload carries the full provenance for the frontend to render.
func TestAgentPayload_CarriesPauseProvenance(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doOwnerPostAs(s, "/api/pause/scanner", "bketelsen"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	payload := BuildAgentOnlyStatus(deps.Governor.GetState(), deps.AgentMgr.AllStatuses(), deps.Config)
	var found bool
	for _, a := range payload.Agents {
		if a.Name != "scanner" {
			continue
		}
		found = true
		if !a.Paused {
			t.Fatal("scanner not paused in payload")
		}
		if a.PausedBy != "bketelsen" {
			t.Errorf("payload PausedBy = %q, want %q", a.PausedBy, "bketelsen")
		}
		if a.PausedTrigger != "dashboard-api" {
			t.Errorf("payload PausedTrigger = %q, want %q", a.PausedTrigger, "dashboard-api")
		}
		if a.PausedReason != "manual pause" {
			t.Errorf("payload PausedReason = %q, want %q", a.PausedReason, "manual pause")
		}
		if a.PausedAt == "" {
			t.Error("payload PausedAt empty")
		}
	}
	if !found {
		t.Fatal("scanner missing from payload")
	}
	_ = s
}

// Resuming clears the provenance from the payload — a running agent must not
// wear a stale "paused by ..." explanation.
func TestAgentPayload_ProvenanceClearedOnResume(t *testing.T) {
	s, deps := apiServer(t)
	if rec := doOwnerPostAs(s, "/api/pause/scanner", "bketelsen"); rec.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200", rec.Code)
	}
	// A real resume may fail without a live tmux (same tolerance as
	// TestHandleResume_RealTransitionReportsChangedTrue); provenance clearing
	// happens before the relaunch half either way.
	_ = doOwnerPostAs(s, "/api/resume/scanner", "bketelsen")
	proc, _ := deps.AgentMgr.GetStatus("scanner")
	if proc.Paused {
		t.Skip("resume did not complete in this environment")
	}
	if proc.PausedBy != "" || proc.PausedTrigger != "" {
		t.Errorf("provenance not cleared on resume: by=%q trigger=%q", proc.PausedBy, proc.PausedTrigger)
	}
	_ = s
}
