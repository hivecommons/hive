package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The Settings dialog's Dashboard section now carries a real control for
// dashboard.sessions.enabled and the three limits; the dashboard settings API
// must read and write them, refuse negative limits and keep 0 as "default".
func TestDashboardSettingsAPISessionsRoundTrip(t *testing.T) {
	s := govServer(t)
	t.Setenv(config.EnvSessionsEnabled, "")

	get := doOwnerGet(s, "/api/config/dashboard")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
	}
	var payload struct {
		Sessions struct {
			Enabled      *bool `json:"enabled"`
			MaxSessions  *int  `json:"max_sessions"`
			EnvOverride  bool  `json:"env_override"`
			IdleTTLHours *int  `json:"idle_ttl_hours"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if payload.Sessions.Enabled == nil || *payload.Sessions.Enabled || payload.Sessions.EnvOverride {
		t.Fatalf("sessions default should be off with no env override: %s", get.Body.String())
	}

	put := doPut(s, "/api/config/dashboard", map[string]any{
		"sessions": map[string]any{"enabled": true, "max_sessions": 6, "max_per_user": 2, "idle_ttl_hours": 0},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}
	d := s.deps.Config.Dashboard.Sessions
	if !d.Enabled || d.MaxSessions != 6 || d.MaxPerUser != 2 || d.IdleTTLHours != 0 {
		t.Fatalf("sessions config not updated: %+v", d)
	}
	if !s.deps.Config.SessionsEnabled() {
		t.Fatal("saving the toggle did not enable sessions")
	}

	// An omitted field leaves the saved value alone.
	put = doPut(s, "/api/config/dashboard", map[string]any{"sessions": map[string]any{"max_per_user": 3}})
	if put.Code != http.StatusOK {
		t.Fatalf("partial PUT = %d: %s", put.Code, put.Body.String())
	}
	d = s.deps.Config.Dashboard.Sessions
	if !d.Enabled || d.MaxSessions != 6 || d.MaxPerUser != 3 {
		t.Fatalf("partial PUT clobbered sessions config: %+v", d)
	}

	bad := doPut(s, "/api/config/dashboard", map[string]any{"sessions": map[string]any{"max_sessions": -1}})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("negative limit => %d, want 400 (%s)", bad.Code, bad.Body.String())
	}
	if s.deps.Config.Dashboard.Sessions.MaxSessions != 6 {
		t.Fatal("rejected PUT mutated dashboard.sessions.max_sessions")
	}
}

// HIVE_SESSIONS_ENABLED decides over the saved toggle; the response says so,
// so the dialog can tell the operator why flipping the switch changes nothing.
func TestDashboardSettingsAPISessionsReportsEnvOverride(t *testing.T) {
	s := govServer(t)
	t.Setenv(config.EnvSessionsEnabled, "true")
	s.deps.Config.Dashboard.Sessions.Enabled = false

	get := doOwnerGet(s, "/api/config/dashboard")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
	}
	var payload struct {
		Sessions struct {
			Enabled     *bool `json:"enabled"`
			EnvOverride bool  `json:"env_override"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if payload.Sessions.Enabled == nil || *payload.Sessions.Enabled {
		t.Fatal("response must report the SAVED toggle, not the env-decided value")
	}
	if !payload.Sessions.EnvOverride {
		t.Fatalf("env_override not reported while %s is set: %s", config.EnvSessionsEnabled, get.Body.String())
	}
}
