package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// The owner's admin_mcp.writes_enabled decides in both directions once set;
// while it is nil, HIVE_ADMIN_MCP_ENABLE_WRITES decides exactly as before;
// unset everywhere is off. Before the switch existed the endpoint read only
// the environment, so every "config" row below failed: a config true could not
// turn writes on, and a config false could not turn an env "1" off.
func TestAdminMCPWritesStatePrecedence(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name       string
		cfg        *bool
		env        string
		wantOn     bool
		wantSource string
	}{
		{"config true, env unset", &yes, "", true, adminMCPWritesSourceConfig},
		{"config true, env off", &yes, "0", true, adminMCPWritesSourceConfig},
		{"config true, env unrecognised", &yes, "nope", true, adminMCPWritesSourceConfig},
		{"config false, env on", &no, "1", false, adminMCPWritesSourceConfig},
		{"config false, env unset", &no, "", false, adminMCPWritesSourceConfig},
		{"config nil, env on", nil, "true", true, adminMCPWritesSourceEnv},
		{"config nil, env off", nil, "off", false, adminMCPWritesSourceEnv},
		{"config nil, env unrecognised", nil, "nope", false, adminMCPWritesSourceEnv},
		{"unset everywhere", nil, "", false, adminMCPWritesSourceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(adminMCPWritesEnvName, tc.env)
			s := &Server{deps: &Dependencies{Config: &config.Config{}}}
			s.deps.Config.AdminMCP.WritesEnabled = tc.cfg
			on, source := s.adminMCPWritesState()
			if on != tc.wantOn || source != tc.wantSource {
				t.Fatalf("adminMCPWritesState() = (%v, %q), want (%v, %q)", on, source, tc.wantOn, tc.wantSource)
			}
			if got := s.adminMCPWritesEnabled(); got != tc.wantOn {
				t.Fatalf("adminMCPWritesEnabled() = %v, want %v", got, tc.wantOn)
			}
		})
	}

	// A server with no config (no deps) keeps the environment behaviour.
	t.Setenv(adminMCPWritesEnvName, "yes")
	if on, source := (&Server{}).adminMCPWritesState(); !on || source != adminMCPWritesSourceEnv {
		t.Fatalf("no-config server = (%v, %q), want env on", on, source)
	}
}

func channelsPutAs(s *Server, user, role string, verifiedOwner bool, body any) *httptest.ResponseRecorder {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, channelsSettingsPath, &b)
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verifiedOwner {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func adminMCPWritesAuditRows(s *Server) []AuditEntry {
	return s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "config_admin_mcp_writes")
}

// Only a verified owner may flip the switch through PUT /api/config/channels.
// A refused PUT changes nothing and writes no audit row; an accepted change
// is audited with the actor, the old effective value and its source, and the
// new value. Before this change the body had no adminMcp field, so the owner
// PUT left the config nil and the env "1" kept writes on.
func TestChannelsSettingsAdminMCPWritesSwitchIsOwnerOnlyAndAudited(t *testing.T) {
	t.Setenv(adminMCPWritesEnvName, "1")
	s, _ := apiServer(t)
	off := map[string]any{"adminMcp": map[string]any{"writesEnabled": false}}

	for _, tc := range []struct {
		name, role string
		verified   bool
	}{
		{"no role", "", false},
		{"read-only", config.RoleRead, false},
		{"read-write", config.RoleReadWrite, false},
		{"merger", config.RoleMerger, false},
		{"owner claimed but not verified", config.RoleOwner, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := channelsPutAs(s, "mallory", tc.role, tc.verified, off)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("PUT status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if s.deps.Config.AdminMCP.WritesEnabled != nil {
				t.Fatalf("refused PUT set admin_mcp.writes_enabled = %v", *s.deps.Config.AdminMCP.WritesEnabled)
			}
			if !s.adminMCPWritesEnabled() {
				t.Fatal("refused PUT turned writes off")
			}
			if rows := adminMCPWritesAuditRows(s); len(rows) != 0 {
				t.Fatalf("refused PUT was audited: %+v", rows)
			}
		})
	}

	payload, _ := getChannelsPayload(t, s)
	if mcp := channelsSection(t, payload, "adminMcp"); mcp["writesEnabled"] != true || mcp["writesSource"] != adminMCPWritesSourceEnv {
		t.Fatalf("before the switch is used, adminMcp = %#v, want on from env", mcp)
	}

	rec := channelsPutAs(s, "alice", config.RoleOwner, true, off)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner PUT status = %d: %s", rec.Code, rec.Body.String())
	}
	if v := s.deps.Config.AdminMCP.WritesEnabled; v == nil || *v {
		t.Fatalf("owner PUT stored %v, want false", v)
	}
	if s.adminMCPWritesEnabled() {
		t.Fatal("config false must override env 1")
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if mcp := channelsSection(t, resp, "adminMcp"); mcp["writesEnabled"] != false || mcp["writesSource"] != adminMCPWritesSourceConfig || mcp["envWritesOn"] != true {
		t.Fatalf("after the owner PUT, adminMcp = %#v", mcp)
	}
	rows := adminMCPWritesAuditRows(s)
	if len(rows) != 1 {
		t.Fatalf("want one audit row, got %+v", rows)
	}
	if rows[0].User != "alice" || rows[0].Detail != "old=on, old_source=env, new=off, new_source=config" {
		t.Fatalf("audit row = %+v", rows[0])
	}
	if !auditEntryIsSensitive(rows[0]) {
		t.Fatalf("config_admin_mcp_writes must be a sensitive audit action")
	}

	// Saving the same value again, or saving another surface, records no
	// further change and leaves the switch alone.
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, off); rec.Code != http.StatusOK {
		t.Fatalf("repeat PUT status = %d", rec.Code)
	}
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, map[string]any{"githubMentions": map[string]any{"enabled": false}}); rec.Code != http.StatusOK {
		t.Fatalf("other-surface PUT status = %d: %s", rec.Code, rec.Body.String())
	}
	if v := s.deps.Config.AdminMCP.WritesEnabled; v == nil || *v {
		t.Fatalf("an unrelated save changed the switch to %v", v)
	}
	if rows := adminMCPWritesAuditRows(s); len(rows) != 1 {
		t.Fatalf("an unchanged switch was audited again: %+v", rows)
	}

	// And back on.
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, map[string]any{"adminMcp": map[string]any{"writesEnabled": true}}); rec.Code != http.StatusOK {
		t.Fatalf("owner PUT on status = %d", rec.Code)
	}
	if !s.adminMCPWritesEnabled() {
		t.Fatal("owner could not turn writes back on")
	}
	if rows := adminMCPWritesAuditRows(s); len(rows) != 2 || rows[1].Detail != "old=off, old_source=config, new=on, new_source=config" {
		t.Fatalf("second change audit rows = %+v", rows)
	}
}

// A confirmation previewed while writes were on is refused at write_confirm
// once the owner turns the switch off, because the endpoint reads the switch
// on every request. The confirmation itself is left intact: with writes back
// on, the same id still executes, so the refusal came from the switch alone.
func TestAdminMCPConfirmRefusedAfterWritesSwitchTurnedOff(t *testing.T) {
	t.Setenv(adminMCPWritesEnvName, "")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = &Dependencies{Config: &config.Config{}}
	on, off := true, false
	s.deps.Config.AdminMCP.WritesEnabled = &on
	var writes int
	s.mux.HandleFunc("POST /api/repos/rescan", func(w http.ResponseWriter, r *http.Request) {
		writes++
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "started"})
	})
	owner := adminMCPAs(t, s.mux, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleOwner, ownerRoleVerifiedHeader: "true"})

	id := confirmationID(t, owner(rescanPreview))

	s.deps.Config.AdminMCP.WritesEnabled = &off
	if got := owner(rescanConfirm(id)); !strings.Contains(got, "admin MCP writes are disabled") {
		t.Fatalf("confirm after the switch was turned off = %s, want writes disabled", got)
	}
	if writes != 0 {
		t.Fatalf("a refused confirm still wrote %d time(s)", writes)
	}

	s.deps.Config.AdminMCP.WritesEnabled = &on
	if got := owner(rescanConfirm(id)); !strings.Contains(got, `"write_result"`) {
		t.Fatalf("confirm with writes back on = %s", got)
	}
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
}

// "Use environment default" (writesFollowEnv) resets the switch to nil, so
// HIVE_ADMIN_MCP_ENABLE_WRITES (or the default, off) decides again. Only the
// owner may reset; a reset on an already-nil switch is a no-op with no audit
// row; sending it together with writesEnabled is a 400 that changes nothing.
// Before this change the body had no writesFollowEnv field, so a reset was
// silently ignored and the stored value kept deciding.
func TestChannelsSettingsAdminMCPWritesResetFollowsEnv(t *testing.T) {
	t.Setenv(adminMCPWritesEnvName, "1")
	s, _ := apiServer(t)
	reset := map[string]any{"adminMcp": map[string]any{"writesFollowEnv": true}}

	// Already nil: accepted, nothing changes, nothing audited.
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, reset); rec.Code != http.StatusOK {
		t.Fatalf("reset on nil status = %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.AdminMCP.WritesEnabled != nil {
		t.Fatal("reset on nil stored a value")
	}
	if rows := adminMCPWritesAuditRows(s); len(rows) != 0 {
		t.Fatalf("reset on an already-nil switch was audited: %+v", rows)
	}

	off := false
	s.deps.Config.AdminMCP.WritesEnabled = &off

	// Both fields at once is a 400 and leaves the switch alone.
	both := map[string]any{"adminMcp": map[string]any{"writesFollowEnv": true, "writesEnabled": true}}
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, both); rec.Code != http.StatusBadRequest {
		t.Fatalf("both fields status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if v := s.deps.Config.AdminMCP.WritesEnabled; v == nil || *v {
		t.Fatalf("a rejected PUT changed the switch to %v", v)
	}

	// A non-owner reset is refused.
	if rec := channelsPutAs(s, "mallory", config.RoleReadWrite, false, reset); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner reset status = %d, want 403", rec.Code)
	}
	if v := s.deps.Config.AdminMCP.WritesEnabled; v == nil || *v {
		t.Fatalf("a refused reset changed the switch to %v", v)
	}
	if rows := adminMCPWritesAuditRows(s); len(rows) != 0 {
		t.Fatalf("refused or rejected PUTs were audited: %+v", rows)
	}

	// The owner's reset returns to the env value (on) and is audited.
	rec := channelsPutAs(s, "alice", config.RoleOwner, true, reset)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner reset status = %d: %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.AdminMCP.WritesEnabled != nil {
		t.Fatalf("owner reset left %v stored", *s.deps.Config.AdminMCP.WritesEnabled)
	}
	if on, source := s.adminMCPWritesState(); !on || source != adminMCPWritesSourceEnv {
		t.Fatalf("after reset state = (%v, %q), want env on", on, source)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if mcp := channelsSection(t, resp, "adminMcp"); mcp["writesEnabled"] != true || mcp["writesSource"] != adminMCPWritesSourceEnv {
		t.Fatalf("after reset adminMcp = %#v", mcp)
	}
	rows := adminMCPWritesAuditRows(s)
	if len(rows) != 1 || rows[0].User != "alice" || rows[0].Detail != "old=off, old_source=config, new=on, new_source=env" {
		t.Fatalf("reset audit rows = %+v", rows)
	}

	// With no env, a reset falls back to the default, off.
	t.Setenv(adminMCPWritesEnvName, "")
	yes := true
	s.deps.Config.AdminMCP.WritesEnabled = &yes
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, reset); rec.Code != http.StatusOK {
		t.Fatalf("second reset status = %d", rec.Code)
	}
	if on, source := s.adminMCPWritesState(); on || source != adminMCPWritesSourceDefault {
		t.Fatalf("reset with no env = (%v, %q), want default off", on, source)
	}
	if rows := adminMCPWritesAuditRows(s); len(rows) != 2 || rows[1].Detail != "old=on, old_source=config, new=off, new_source=default" {
		t.Fatalf("second reset audit rows = %+v", rows)
	}
}

// A reset that turns writes off (config true, no env) makes write_confirm
// refuse a confirmation previewed while the switch was on.
func TestAdminMCPConfirmRefusedAfterResetTurnsWritesOff(t *testing.T) {
	t.Setenv(adminMCPWritesEnvName, "")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	s, _ := apiServer(t)
	on := true
	s.deps.Config.AdminMCP.WritesEnabled = &on
	owner := adminMCPAs(t, s.mux, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleOwner, ownerRoleVerifiedHeader: "true"})

	id := confirmationID(t, owner(rescanPreview))
	if rec := channelsPutAs(s, "alice", config.RoleOwner, true, map[string]any{"adminMcp": map[string]any{"writesFollowEnv": true}}); rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := owner(rescanConfirm(id)); !strings.Contains(got, "admin MCP writes are disabled") {
		t.Fatalf("confirm after the reset = %s, want writes disabled", got)
	}
}
