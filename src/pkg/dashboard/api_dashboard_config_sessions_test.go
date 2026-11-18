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
	if payload.Sessions.Enabled == nil || !*payload.Sessions.Enabled || payload.Sessions.EnvOverride {
		t.Fatalf("sessions default should be on with no env override: %s", get.Body.String())
	}

	put := doPut(s, "/api/config/dashboard", map[string]any{"sessions": map[string]any{"enabled": false}})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT off = %d: %s", put.Code, put.Body.String())
	}
	if d := s.deps.Config.Dashboard.Sessions; d.Enabled == nil || *d.Enabled || s.deps.Config.SessionsEnabled() {
		t.Fatalf("saving the toggle off did not persist an explicit false: %+v", d)
	}

	put = doPut(s, "/api/config/dashboard", map[string]any{
		"sessions": map[string]any{"enabled": true, "max_sessions": 6, "max_per_user": 2, "idle_ttl_hours": 0},
	})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", put.Code, put.Body.String())
	}
	d := s.deps.Config.Dashboard.Sessions
	if !d.EnabledOrDefault() || d.MaxSessions != 6 || d.MaxPerUser != 2 || d.IdleTTLHours != 0 {
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
	if !d.EnabledOrDefault() || d.MaxSessions != 6 || d.MaxPerUser != 3 {
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
	off := false
	s.deps.Config.Dashboard.Sessions.Enabled = &off

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

// dashboard.sessions.autostart (the startup auto-answer) reads as on by
// default, saves an explicit false, and is left alone by a PUT that omits it.
func TestDashboardSettingsAPISessionsAutostartRoundTrip(t *testing.T) {
	s := govServer(t)
	t.Setenv(config.EnvSessionsEnabled, "")
	t.Setenv(config.EnvSessionsAutostart, "")
	saved := func() *bool {
		t.Helper()
		get := doOwnerGet(s, "/api/config/dashboard")
		if get.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", get.Code, get.Body.String())
		}
		var payload struct {
			Sessions struct {
				Autostart *bool `json:"autostart"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode GET: %v", err)
		}
		return payload.Sessions.Autostart
	}
	if v := saved(); v == nil || !*v {
		t.Fatal("autostart must read as on by default")
	}
	put := doPut(s, "/api/config/dashboard", map[string]any{"sessions": map[string]any{"autostart": false}})
	if put.Code != http.StatusOK {
		t.Fatalf("PUT off = %d: %s", put.Code, put.Body.String())
	}
	if d := s.deps.Config.Dashboard.Sessions; d.Autostart == nil || *d.Autostart || s.deps.Config.SessionsAutostart() {
		t.Fatalf("saving autostart off did not persist an explicit false: %+v", d)
	}
	if v := saved(); v == nil || *v {
		t.Fatal("GET does not report the saved false")
	}
	put = doPut(s, "/api/config/dashboard", map[string]any{"sessions": map[string]any{"max_per_user": 3}})
	if put.Code != http.StatusOK {
		t.Fatalf("partial PUT = %d: %s", put.Code, put.Body.String())
	}
	if s.deps.Config.SessionsAutostart() {
		t.Fatal("a PUT that omits autostart turned it back on")
	}
}
