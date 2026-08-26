package dashboard

import (
	"net/http"
	"strings"
	"testing"
)

// These tests pin caveman_mode enum validation on both agent write paths
// (#4531, flagged as a follow-up in #3897): the create handler must reject an
// invalid value with a 400 naming the allowed set instead of persisting it and
// letting the NEXT config load fail far from the request that caused it, and
// the update handler's existing gate must keep doing the same.

const cavemanModeErr = "caveman_mode must be one of: lite, full, ultra, wenyan (or empty to disable)"

func TestHandleAgentCreate_InvalidCavemanMode(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	for _, mode := range []string{"mega", "LITE", " full", "off", "true"} {
		rec := doPost(s, "/api/agents", map[string]interface{}{
			"name":  "caveman-bad",
			"agent": map[string]interface{}{"backend": "claude", "caveman_mode": mode},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("caveman_mode=%q: expected 400, got %d", mode, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), cavemanModeErr) {
			t.Errorf("caveman_mode=%q: error must name the allowed set, got %s", mode, rec.Body.String())
		}
		if _, ok := deps.Config.Agents["caveman-bad"]; ok {
			t.Fatalf("caveman_mode=%q: invalid agent must not be persisted", mode)
		}
	}
}

func TestHandleAgentCreate_ValidCavemanModes(t *testing.T) {
	for _, mode := range []string{"lite", "full", "ultra", "wenyan"} {
		s, deps := apiServer(t)
		deps.Config.Data.AgentsDir = t.TempDir()

		rec := doPost(s, "/api/agents", map[string]interface{}{
			"name":  "caveman-" + mode,
			"agent": map[string]interface{}{"backend": "claude", "caveman_mode": mode},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("caveman_mode=%q: expected 200, got %d: %s", mode, rec.Code, rec.Body.String())
		}
		if got := deps.Config.Agents["caveman-"+mode].CavemanMode; got != mode {
			t.Errorf("caveman_mode=%q: persisted %q", mode, got)
		}
	}
}

func TestHandleAgentCreate_CavemanModeAbsentOrEmpty(t *testing.T) {
	// "" (and absent, which unmarshals to "") means "caveman disabled" and must
	// stay accepted — it is the default for every agent created from the UI.
	for name, agent := range map[string]map[string]interface{}{
		"caveman-absent": {"backend": "claude"},
		"caveman-empty":  {"backend": "claude", "caveman_mode": ""},
	} {
		s, deps := apiServer(t)
		deps.Config.Data.AgentsDir = t.TempDir()

		rec := doPost(s, "/api/agents", map[string]interface{}{"name": name, "agent": agent})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", name, rec.Code, rec.Body.String())
		}
		if got := deps.Config.Agents[name].CavemanMode; got != "" {
			t.Errorf("%s: expected empty caveman_mode, got %q", name, got)
		}
	}
}

func TestAgentConfigGeneral_InvalidCavemanMode(t *testing.T) {
	s := acfgServer(t)
	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"cavemanMode": "mega"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), cavemanModeErr) {
		t.Errorf("error must name the allowed set, got %s", rec.Body.String())
	}
}

func TestAgentConfigGeneral_ValidCavemanModes(t *testing.T) {
	s := acfgServer(t)
	for _, mode := range []string{"lite", "full", "ultra", "wenyan", ""} {
		rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"cavemanMode": mode})
		if rec.Code != http.StatusOK {
			t.Fatalf("cavemanMode=%q: expected 200, got %d: %s", mode, rec.Code, rec.Body.String())
		}
		if got := s.deps.Config.Agents["scanner"].CavemanMode; got != mode {
			t.Errorf("cavemanMode=%q: persisted %q", mode, got)
		}
	}
}
