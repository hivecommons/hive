package hub

import (
	"strings"
	"testing"
)

// ============================================================
// server.go — handleHeartbeat project-config delivery + SaaS error-clear
// ============================================================

func TestHandleHeartbeatDeliversProjectConfig(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// A CLAIMED hosted hive with a complete project (org+repos+primary+acmm>0).
	saveSaaSHive(&SaaSHive{
		ID: "hosted-claimed", Owner: "alice", Status: "running",
		Org: "acme", Repos: []string{"repo"}, PrimaryRepo: "repo", ACMMLevel: 3,
	})

	// The spoke heartbeats reporting a DIFFERENT (stale placeholder) project, so
	// the hub delivers the real project config.
	body := `{"hive_id":"hosted-claimed","org":"placeholder","primary_repo":"other","acmm_level":0}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "acme") {
		t.Errorf("expected delivered project config with org=acme, got %s", rec.Body.String())
	}
}

func TestHandleHeartbeatClearsSaaSError(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	// A hosted hive stuck in "error" that is now heartbeating -> status cleared.
	saveSaaSHive(&SaaSHive{ID: "hosted-err", Owner: "alice", Status: "error", Error: "boom"})
	// A matching registry entry so the found-path (error-clear) runs.
	s.registry.Hives = []RegistryEntry{{ID: "hosted-err", Owner: "alice"}}

	rec := postHeartbeat(t, s, `{"hive_id":"hosted-err","primary_repo":"r"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := loadSaaSHive("hosted-err"); got == nil || got.Status == "error" {
		t.Errorf("expected error status cleared, got %+v", got)
	}
}
