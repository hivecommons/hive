package dashboard

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
)

func TestUpdateAgentProcessConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		agent   string
		enabled bool
		present bool
		wantErr bool
	}{
		{name: "disabled absent", agent: "strategist"},
		{name: "ACMM gated absent", agent: "telemetry", enabled: true},
		{name: "active unexpectedly absent", agent: "strategist", enabled: true, wantErr: true},
		{name: "disabled existing copy", agent: "strategist", present: true},
		{name: "ACMM gated existing copy", agent: "telemetry", enabled: true, present: true},
		{name: "active existing copy", agent: "strategist", enabled: true, present: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, deps := apiServer(t)
			runtime := cruxtest.NewRecorder()
			runtime.ACMM = 1
			deps.AgentMgr = runtime
			if tt.present {
				runtime.Procs[tt.agent] = &crux.Process{Name: tt.agent}
			}
			cfg := config.AgentConfig{Enabled: tt.enabled, DisplayName: "Updated name"}
			if err := s.updateAgentProcessConfig(tt.agent, cfg); (err != nil) != tt.wantErr {
				t.Fatalf("updateAgentProcessConfig() = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.present {
				if got := runtime.Procs[tt.agent].Config.DisplayName; got != cfg.DisplayName {
					t.Errorf("runtime display name = %q, want %q", got, cfg.DisplayName)
				}
			} else if _, exists := runtime.Procs[tt.agent]; exists {
				t.Error("editing config created an unwanted runtime entry")
			}
		})
	}
}

// The reported strategist error comes from saving a config-only agent after
// reconciliation has deliberately left it out of the enabled runtime roster.
func TestGeneralConfigDisabledAgentDoesNotWarnAboutMissingProcess(t *testing.T) {
	s, deps := apiServer(t)
	deps.AgentMgr = cruxtest.NewRecorder()
	deps.Config.Agents["strategist"] = config.AgentConfig{Backend: "claude", Enabled: false}
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))

	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/strategist/general", strings.NewReader(`{"display_name":"Strategy"}`))
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["strategist"].DisplayName; got != "Strategy" {
		t.Errorf("saved display name = %q, want Strategy", got)
	}
	if strings.Contains(logs.String(), "failed to sync agent config to process") {
		t.Fatalf("expected no runtime sync warning for a disabled config-only agent: %s", logs.String())
	}
}
