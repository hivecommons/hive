package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The incident pairing: the dashboard moved the agent to codex but left the
// bob launch_cmd behind, and the next boot failed config validation.
const staleBobLaunchCmd = "/usr/bin/bob --allow-all --model claude-sonnet-4-6"

func setScannerLaunchCmd(t *testing.T, srv *Server, backend, launchCmd string) {
	t.Helper()
	sc := srv.deps.Config.Agents["scanner"]
	sc.Enabled = true
	sc.Backend = backend
	sc.LaunchCmd = launchCmd
	srv.deps.Config.Agents["scanner"] = sc
}

func readScannerOverlay(t *testing.T, srv *Server) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(srv.deps.Config.Data.AgentsDir, "scanner.yaml"))
	if err != nil {
		t.Fatalf("reading scanner overlay: %v", err)
	}
	return string(raw)
}

// Gap 1: claimAgentFieldOwnership (the switch-backend persistence path) must
// not persist a backend next to a launch_cmd that launches a different CLI.
func TestClaimAgentFieldOwnership_ClearsContradictingLaunchCmd(t *testing.T) {
	srv := newFullServer(t)
	setScannerLaunchCmd(t, srv, "bob", staleBobLaunchCmd)

	srv.claimAgentFieldOwnership("scanner", "", "codex")

	sc := srv.deps.Config.Agents["scanner"]
	if sc.Backend != "codex" {
		t.Fatalf("backend = %q, want codex", sc.Backend)
	}
	if sc.LaunchCmd != "" {
		t.Errorf("launch_cmd = %q, want cleared on switch to codex", sc.LaunchCmd)
	}
	// Both persisted layers must agree with memory and pass the loader's gate.
	overlay := readScannerOverlay(t, srv)
	if strings.Contains(overlay, "/usr/bin/bob") {
		t.Errorf("overlay still carries launch_cmd:\n%s", overlay)
	}
	hiveYAML, err := os.ReadFile(srv.deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("reading hive.yaml: %v", err)
	}
	if strings.Contains(string(hiveYAML), "/usr/bin/bob") {
		t.Errorf("hive.yaml still carries the bob launch_cmd:\n%s", hiveYAML)
	}
	if err := srv.deps.Config.ValidateAgentLaunchCmds(); err != nil {
		t.Errorf("persisted config would fail the loader: %v", err)
	}
}

func TestClaimAgentFieldOwnership_KeepsMatchingLaunchCmd(t *testing.T) {
	srv := newFullServer(t)
	setScannerLaunchCmd(t, srv, "claude", staleBobLaunchCmd)

	srv.claimAgentFieldOwnership("scanner", "", "bob")

	sc := srv.deps.Config.Agents["scanner"]
	if sc.Backend != "bob" || sc.LaunchCmd != staleBobLaunchCmd {
		t.Errorf("agent = %+v, want backend bob with launch_cmd kept", sc)
	}
	if !strings.Contains(readScannerOverlay(t, srv), "/usr/bin/bob") {
		t.Error("overlay lost a launch_cmd that matched the new backend")
	}
}

// A model-only claim must never touch launch_cmd, even one that already
// contradicts the backend: the operator did not change the backend here.
func TestClaimAgentFieldOwnership_ModelOnlyLeavesLaunchCmd(t *testing.T) {
	srv := newFullServer(t)
	setScannerLaunchCmd(t, srv, "claude", "agent-launch.sh --backend claude")

	srv.claimAgentFieldOwnership("scanner", "opus", "")

	if sc := srv.deps.Config.Agents["scanner"]; sc.LaunchCmd != "agent-launch.sh --backend claude" || sc.Model != "opus" {
		t.Errorf("agent = %+v, want model opus and launch_cmd untouched", sc)
	}
}

// End to end through the handler the owner actually used.
func TestHandleSwitch_ClearsContradictingLaunchCmd(t *testing.T) {
	srv := newFullServer(t)
	setScannerLaunchCmd(t, srv, "bob", staleBobLaunchCmd)

	req := httptest.NewRequest(http.MethodPost, "/api/agents/scanner/switch/codex", nil)
	markOwnerRequest(req)
	req.SetPathValue("agent", "scanner")
	req.SetPathValue("backend", "codex")
	w := httptest.NewRecorder()
	srv.handleSwitch(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handleSwitch: %d %s", w.Code, w.Body.String())
	}
	sc := srv.deps.Config.Agents["scanner"]
	if sc.Backend != "codex" || sc.LaunchCmd != "" {
		t.Errorf("agent = %+v, want backend codex and launch_cmd cleared", sc)
	}
	if strings.Contains(readScannerOverlay(t, srv), "/usr/bin/bob") {
		t.Error("overlay still carries the stale launch_cmd after switch")
	}
}

// The Governor grid's PUT .../models path also sets Backend; it must reconcile
// the same way and must not leave a contradiction on disk.
func TestHandleAgentConfigModels_BackendChangeClearsContradictingLaunchCmd(t *testing.T) {
	srv := newFullServer(t)
	setScannerLaunchCmd(t, srv, "bob", staleBobLaunchCmd)

	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/scanner/models", strings.NewReader(`{"backend":"codex"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	srv.handleAgentConfigModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("handleAgentConfigModels: %d %s", w.Code, w.Body.String())
	}
	sc := srv.deps.Config.Agents["scanner"]
	if sc.Backend != "codex" || sc.LaunchCmd != "" {
		t.Errorf("agent = %+v, want backend codex and launch_cmd cleared", sc)
	}
	if strings.Contains(readScannerOverlay(t, srv), "/usr/bin/bob") {
		t.Error("overlay still carries the stale launch_cmd")
	}
	for _, a := range systemAlertsOf(srv) {
		if a.ID == "agent-field-save-failed" {
			t.Errorf("unexpected save-failed alert: %+v", a)
		}
	}
}

// Gap 2: a contradiction that somehow reaches the save path is refused by the
// config layer rather than written. The dashboard surfaces that as an alert.
func TestClaimAgentFieldOwnership_SaveRefusalRaisesAlert(t *testing.T) {
	srv := newFullServer(t)
	// A second agent already contradicting itself makes the whole-file save
	// fail regardless of what the claim does to scanner.
	srv.deps.Config.Agents["reviewer"] = config.AgentConfig{Backend: "codex", LaunchCmd: staleBobLaunchCmd, Enabled: true}
	setScannerLaunchCmd(t, srv, "claude", "")

	srv.claimAgentFieldOwnership("scanner", "", "bob")

	if _, err := os.Stat(srv.deps.Config.SourcePath); !os.IsNotExist(err) {
		t.Errorf("hive.yaml was written despite the contradiction (stat err = %v)", err)
	}
	var found bool
	for _, a := range systemAlertsOf(srv) {
		if a.ID == "agent-field-save-failed" && strings.Contains(a.Message, "contradicts launch_cmd") {
			found = true
		}
	}
	if !found {
		t.Errorf("no agent-field-save-failed alert naming the contradiction; alerts = %+v", systemAlertsOf(srv))
	}
}

// Gap 3: a launch_cmd the loader dropped at boot must be visible to the
// operator as a dashboard banner, one per agent.
func TestSurfaceLaunchCmdDegradations(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.LaunchCmdDegradations = []config.LaunchCmdDegradation{
		{Agent: "brainstorm", Backend: "codex", LaunchCmd: staleBobLaunchCmd, Error: "x"},
		{Agent: "scanner", Backend: "codex", LaunchCmd: staleBobLaunchCmd, Error: "y"},
	}

	srv.surfaceLaunchCmdDegradations()

	alerts := systemAlertsOf(srv)
	var got []string
	for _, a := range alerts {
		if strings.HasPrefix(a.ID, launchCmdDegradedAlertPrefix) {
			got = append(got, a.ID)
			if a.Severity != "error" || !strings.Contains(a.Message, staleBobLaunchCmd) || !strings.Contains(a.Message, `backend "codex"`) {
				t.Errorf("alert %+v lacks severity/launch_cmd/backend detail", a)
			}
		}
	}
	if len(got) != 2 || got[0] != launchCmdDegradedAlertPrefix+"brainstorm" || got[1] != launchCmdDegradedAlertPrefix+"scanner" {
		t.Errorf("degradation alerts = %v, want one per agent", got)
	}

	// No degradations, no alerts.
	srv2 := newFullServer(t)
	srv2.surfaceLaunchCmdDegradations()
	for _, a := range systemAlertsOf(srv2) {
		if strings.HasPrefix(a.ID, launchCmdDegradedAlertPrefix) {
			t.Errorf("unexpected degradation alert %+v on a clean config", a)
		}
	}
}

// Guards against the surfacing call being dropped from the server wiring.
func TestSurfaceLaunchCmdDegradations_NilSafe(t *testing.T) {
	(&Server{}).surfaceLaunchCmdDegradations()
	(&Server{deps: &Dependencies{}}).surfaceLaunchCmdDegradations()
}

func systemAlertsOf(s *Server) []SystemAlert {
	s.systemAlertsMu.Lock()
	defer s.systemAlertsMu.Unlock()
	return append([]SystemAlert(nil), s.systemAlerts...)
}
