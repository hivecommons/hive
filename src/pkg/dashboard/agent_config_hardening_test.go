package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// agentConfigAuditDetail returns the detail of the newest audit entry for action.
func agentConfigAuditDetail(t *testing.T, s *Server, action string) string {
	t.Helper()
	for _, entry := range s.audit.Recent(50) {
		if entry.Action == action {
			return entry.Detail
		}
	}
	t.Fatalf("no %s audit entry", action)
	return ""
}

// #19788 phase 2: every agent config section writer holds agentConfigMu from
// its first read through the save, so a write waits for one already running.
func TestAgentConfigWriters_SerialisedByAgentConfigMu(t *testing.T) {
	writes := []struct {
		name string
		path string
		body any
	}{
		{"general", "/api/config/agent/scanner/general", map[string]any{"displayName": "Renamed"}},
		{"cadences", "/api/config/agent/scanner/cadences", map[string]any{"idle": 600}},
		{"models", "/api/config/agent/scanner/models", map[string]any{"model": "opus"}},
		{"channels", "/api/config/agent/scanner/channels", map[string]any{"channels": []any{}}},
		{"tools", "/api/config/agent/scanner/tools", map[string]any{}},
		{"connections", "/api/config/agent/scanner/connections", map[string]any{"connections": []any{}}},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			s, deps := apiServer(t)
			writableConfig(t, deps.Config)

			s.agentConfigMu.Lock()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- doPut(s, w.path, w.body) }()

			select {
			case rec := <-done:
				s.agentConfigMu.Unlock()
				t.Fatalf("write finished while agentConfigMu was held: %d %s", rec.Code, rec.Body.String())
			case <-time.After(100 * time.Millisecond):
			}
			s.agentConfigMu.Unlock()

			select {
			case rec := <-done:
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("write did not finish after agentConfigMu was released")
			}
		})
	}
}

func TestAgentConfigCadences_AuditRecordsBeforeAndAfter(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)

	rec := doPut(s, "/api/config/agent/scanner/cadences", map[string]any{"idle": 7200, "busy": 0})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	want := "section=cadences, busy=5m -> pause, idle=15m -> 2h"
	if got := agentConfigAuditDetail(t, s, "config_agent_cadences"); got != want {
		t.Fatalf("audit detail = %q, want %q", got, want)
	}
}

func TestAgentConfigGeneral_AuditRecordsChangedFieldsBeforeAndAfter(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"displayName": "Renamed", "enabled": false, "model": "sonnet"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := agentConfigAuditDetail(t, s, "config_agent_general")
	for _, want := range []string{"section=general", "enabled=true -> false", "displayName=(unset) -> Renamed"} {
		if !strings.Contains(got, want) {
			t.Errorf("audit detail %q is missing %q", got, want)
		}
	}
	// An unchanged field sent in the body is not recorded as a change.
	if strings.Contains(got, "model=") {
		t.Errorf("audit detail %q records the unchanged model", got)
	}
}

func TestValidateCadencesBody_NormalisesWithoutMutating(t *testing.T) {
	_, deps := apiServer(t)
	gov := &deps.Config.Governor
	before := snapshotAgentConfig(deps, "scanner")

	got, err := validateCadencesBody(gov, map[string]json.RawMessage{
		"idle": json.RawMessage(`7200`),
		"busy": json.RawMessage(`0`),
	})
	if err != nil {
		t.Fatalf("validateCadencesBody: %v", err)
	}
	if got["idle"] != "2h" || got["busy"] != "pause" {
		t.Fatalf("normalised cadences = %v, want idle=2h busy=pause", got)
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)

	if _, err := validateCadencesBody(gov, map[string]json.RawMessage{"idle": json.RawMessage(`600`), "nope": json.RawMessage(`600`)}); err == nil {
		t.Fatal("unknown mode accepted")
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)
}

// The models helper reports the backend and model the agent launches with,
// live overrides included, before and after the write.
func TestValidateModelsBody_ReturnsEffectiveValuesWithoutMutating(t *testing.T) {
	s, deps := apiServer(t)
	if err := deps.AgentMgr.SetBackendOverride("scanner", "copilot"); err != nil {
		t.Fatal(err)
	}
	if err := deps.AgentMgr.SetModelOverride("scanner", "gpt-4o-mini-2024-07-18"); err != nil {
		t.Fatal(err)
	}
	before := snapshotAgentConfig(deps, "scanner")

	u, err := s.validateModelsBody("scanner", deps.Config.Agents["scanner"], agentModelsBody{Model: "auto"})
	if err != nil {
		t.Fatalf("validateModelsBody: %v", err)
	}
	if u.PrevBackend != "copilot" || u.PrevModel != "gpt-4o-mini-2024-07-18" {
		t.Errorf("prev = (%q, %q), want the live overrides", u.PrevBackend, u.PrevModel)
	}
	if u.EffectiveBackend != "copilot" || u.EffectiveModel != "auto" {
		t.Errorf("effective = (%q, %q), want (copilot, auto)", u.EffectiveBackend, u.EffectiveModel)
	}
	if !u.ModelChanged || u.BackendChanged {
		t.Errorf("changed = model %v backend %v, want model only", u.ModelChanged, u.BackendChanged)
	}
	if u.Cfg.Model != "auto" || u.Cfg.ModelOwner != config.FieldOwnerOperator {
		t.Errorf("normalised config model = %q owner %q", u.Cfg.Model, u.Cfg.ModelOwner)
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)

	if _, err := s.validateModelsBody("scanner", deps.Config.Agents["scanner"], agentModelsBody{Backend: "no-such-backend"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)
}
