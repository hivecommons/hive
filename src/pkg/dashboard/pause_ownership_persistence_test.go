package dashboard

// Coverage for claimAgentPauseOwnership's persistence branches (api.go), which
// the #5706 tests in pack_pause_ownership_test.go do not reach: the nil-deps
// guard, the unknown-agent early return, the saveConfig failure alert, and the
// managed-agent overlay write in both its success (alert cleared) and failure
// (alert raised) shapes. These are the paths that decide whether an operator's
// resume survives the next ACMM pack apply, so a silent regression here
// re-opens the "agent re-paused on every boot" bug the marker exists to stop.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const pauseOwnerAlertID = "agent-pause-owner-save-failed"

// hasSystemAlert reports whether an alert with the given ID is currently
// raised on the server, safe under -race.
func hasSystemAlert(s *Server, id string) bool {
	s.systemAlertsMu.RLock()
	defer s.systemAlertsMu.RUnlock()
	for _, a := range s.systemAlerts {
		if a.ID == id {
			return true
		}
	}
	return false
}

// TestClaimAgentPauseOwnership_NilDepsIsSafe: the claim runs from handler
// paths that can exist before RegisterAPI wires dependencies; a bare server
// must no-op, not panic.
func TestClaimAgentPauseOwnership_NilDepsIsSafe(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.claimAgentPauseOwnership("scanner") // must not panic
}

// TestClaimAgentPauseOwnership_UnknownAgentIsNoOp: a name absent from the
// config must not be invented into it by the claim.
func TestClaimAgentPauseOwnership_UnknownAgentIsNoOp(t *testing.T) {
	s, deps := apiServer(t)
	s.claimAgentPauseOwnership("ghost")
	if _, ok := deps.Config.Agents["ghost"]; ok {
		t.Fatal("claimAgentPauseOwnership invented a config entry for an unknown agent")
	}
	if hasSystemAlert(s, pauseOwnerAlertID) {
		t.Fatal("no-op claim must not raise the save-failed alert")
	}
}

// TestClaimAgentPauseOwnership_SaveFailureRaisesAlert: when hive.yaml cannot
// be persisted, the operator must see the consequence — the resume may not
// survive a restart — as a system alert, and the in-memory marker must still
// be set so THIS boot's sweeps honor the resume.
func TestClaimAgentPauseOwnership_SaveFailureRaisesAlert(t *testing.T) {
	s, deps := apiServer(t)
	// A non-empty SourcePath makes saveConfig actually attempt the save; an
	// empty project.org makes the save guard refuse it deterministically,
	// with no writes anywhere on disk.
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.Project.Org = ""

	s.claimAgentPauseOwnership("scanner")

	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("in-memory ownership marker must be set even when persistence fails")
	}
	if !hasSystemAlert(s, pauseOwnerAlertID) {
		t.Fatalf("save failure must raise the %q alert so the operator knows the resume may not survive a restart", pauseOwnerAlertID)
	}
}

// TestClaimAgentPauseOwnership_ManagedAgentPersistsOverlay: for a managed
// agent the overlay file replaces the hive.yaml entry on every config load,
// so the marker must land in the overlay too — and a fully successful claim
// must clear any earlier save-failed alert.
func TestClaimAgentPauseOwnership_ManagedAgentPersistsOverlay(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir
	ac := deps.Config.Agents["scanner"]
	ac.Managed = true
	deps.Config.Agents["scanner"] = ac
	s.AddSystemAlert(pauseOwnerAlertID, "error", "stale alert from an earlier failure")

	s.claimAgentPauseOwnership("scanner")

	data, err := os.ReadFile(filepath.Join(dir, "scanner.yaml"))
	if err != nil {
		t.Fatalf("reading agent overlay: %v", err)
	}
	if !strings.Contains(string(data), "pause_owner: "+config.FieldOwnerOperator) {
		t.Fatalf("agent overlay does not persist the ownership marker:\n%s", data)
	}
	if hasSystemAlert(s, pauseOwnerAlertID) {
		t.Fatal("a successful claim must clear the save-failed alert")
	}
}

// TestClaimAgentPauseOwnership_OverlayWriteFailureRaisesAlert: an overlay
// write that fails leaves the marker vulnerable to the next config load, so
// it must be surfaced the same way as a hive.yaml save failure.
func TestClaimAgentPauseOwnership_OverlayWriteFailureRaisesAlert(t *testing.T) {
	s, deps := apiServer(t)
	// AgentsDir pointing at a regular FILE makes SaveAgentFile's MkdirAll
	// fail deterministically.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Config.Data.AgentsDir = blocker
	ac := deps.Config.Agents["scanner"]
	ac.Managed = true
	deps.Config.Agents["scanner"] = ac

	s.claimAgentPauseOwnership("scanner")

	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("in-memory ownership marker must be set even when the overlay write fails")
	}
	if !hasSystemAlert(s, pauseOwnerAlertID) {
		t.Fatalf("overlay write failure must raise the %q alert", pauseOwnerAlertID)
	}
}
