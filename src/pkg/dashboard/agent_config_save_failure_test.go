package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// agentConfigState is the part of the live config the agent config handlers
// write, deep-copied so a later in-place mutation cannot hide from the compare.
type agentConfigState struct {
	agent    config.AgentConfig
	sandbox  *config.AgentSandboxOverride
	cadences map[string]config.Cadence
	owned    map[string]bool
}

func snapshotAgentConfig(deps *Dependencies, name string) agentConfigState {
	st := agentConfigState{
		agent:    deps.Config.Agents[name],
		cadences: map[string]config.Cadence{},
		owned:    map[string]bool{},
	}
	if sb := st.agent.Sandbox; sb != nil {
		cp := *sb
		st.sandbox = &cp
	}
	for mode, mc := range deps.Config.Governor.Modes {
		if c, ok := mc.Cadences[name]; ok {
			st.cadences[mode] = c
		}
		st.owned[mode] = deps.Config.Governor.CadenceIsOperatorOwned(mode, name)
	}
	return st
}

func assertAgentConfigUnchanged(t *testing.T, deps *Dependencies, name string, before agentConfigState) {
	t.Helper()
	if after := snapshotAgentConfig(deps, name); !reflect.DeepEqual(after, before) {
		t.Fatalf("in-memory config changed by a refused write:\n before %+v\n after  %+v", before, after)
	}
}

// unwritableConfig makes every layer Config.Save writes fail: the source file,
// the runtime copy and the dashboard overlay all sit under a missing directory.
func unwritableConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "missing")
	cfg.SourcePath = filepath.Join(missing, "hive.yaml")
	oldRuntime, oldOverlay := config.RuntimeConfigFile, config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(missing, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(missing, "dashboard-overlay.yaml")
	t.Cleanup(func() { config.RuntimeConfigFile, config.DashboardOverlayFile = oldRuntime, oldOverlay })
}

// #19847: the general, cadences and models handlers answered 200 when the
// config save failed, leaving the change in memory only until the next
// restart. A failed save must be an error and must leave memory unchanged.
func TestAgentConfigWrites_FailedSaveIsNotSuccess(t *testing.T) {
	handlers := []struct {
		name string
		path string
		body any
	}{
		{"general", "/api/config/agent/scanner/general", map[string]any{"displayName": "Renamed", "enabled": false}},
		{"cadences", "/api/config/agent/scanner/cadences", map[string]any{"idle": 600, "busy": "45m"}},
		{"models", "/api/config/agent/scanner/models", map[string]any{"model": "opus"}},
	}
	failures := []struct {
		name  string
		setup func(*testing.T, *config.Config)
		want  int
	}{
		{"no source path", func(*testing.T, *config.Config) {}, http.StatusConflict},
		{"write fails", unwritableConfig, http.StatusInternalServerError},
	}
	for _, h := range handlers {
		for _, f := range failures {
			t.Run(h.name+"/"+f.name, func(t *testing.T) {
				s, deps := apiServer(t)
				f.setup(t, deps.Config)
				before := snapshotAgentConfig(deps, "scanner")

				rec := doPut(s, h.path, h.body)
				if rec.Code != f.want {
					t.Fatalf("status = %d, want %d: %s", rec.Code, f.want, rec.Body.String())
				}
				assertAgentConfigUnchanged(t, deps, "scanner", before)
			})
		}
	}
}

func TestAgentConfigCadences_UnknownModeRefused(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	before := snapshotAgentConfig(deps, "scanner")

	rec := doPut(s, "/api/config/agent/scanner/cadences", map[string]any{"idle": 600, "nonexistent_mode": 900})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)
}

// A bad entry anywhere in the body must not leave the valid modes applied. The
// bad entry is a schedule object missing its timezone, which cadence validation
// rejects (a bare unparseable string is accepted as an interval).
func TestAgentConfigCadences_InvalidModeLeavesOthersUnchanged(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	before := snapshotAgentConfig(deps, "scanner")

	rec := doPut(s, "/api/config/agent/scanner/cadences", map[string]any{"idle": 600, "quiet": 300, "busy": map[string]any{"times": []string{"09:00"}}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)
}

// The general handler rejected a later field only after writing the substrate
// edit through the Sandbox pointer it shares with the live config.
func TestAgentConfigGeneral_RejectedSaveLeavesSharedSandboxUnchanged(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)
	scanner := deps.Config.Agents["scanner"]
	scanner.Sandbox = &config.AgentSandboxOverride{
		Substrate: &config.SandboxSubstrateConfig{Endpoint: "https://substrate.example"},
	}
	deps.Config.Agents["scanner"] = scanner
	before := snapshotAgentConfig(deps, "scanner")

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{
		"sandboxSubstrate": map[string]any{"endpoint": "https://other.example"},
		"cavemanMode":      "mega",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	assertAgentConfigUnchanged(t, deps, "scanner", before)
	if got := deps.Config.Agents["scanner"].Sandbox.Substrate.Endpoint; got != "https://substrate.example" {
		t.Fatalf("live substrate endpoint = %q, want the original", got)
	}
}

// The success path still persists: the change reaches the file and memory.
func TestAgentConfigWrites_SaveSucceeds(t *testing.T) {
	s, deps := apiServer(t)
	writableConfig(t, deps.Config)

	if rec := doPut(s, "/api/config/agent/scanner/cadences", map[string]any{"idle": 7200}); rec.Code != http.StatusOK {
		t.Fatalf("cadences: status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Governor.Modes["idle"].Cadences["scanner"]; got != "2h" {
		t.Errorf("idle cadence = %q, want 2h", got)
	}
	if !deps.Config.Governor.CadenceIsOperatorOwned("idle", "scanner") {
		t.Error("cadence save did not claim operator ownership")
	}
	saved, err := os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	if !strings.Contains(string(saved), "scanner: 2h") {
		t.Errorf("saved config does not carry the new idle cadence:\n%s", saved)
	}
}
