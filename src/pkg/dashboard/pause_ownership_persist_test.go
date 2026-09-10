package dashboard

// Tests for the persistence branches of claimAgentPauseOwnership (api.go).
// pack_pause_ownership_test.go pins the #5706 sweep semantics and the happy
// path; these tests pin what happens when persisting the ownership marker
// FAILS — the operator must get a visible system alert warning that the
// resume will not survive a restart — and that a later successful claim
// clears a stale alert instead of leaving it up forever.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hasPauseOwnerSaveAlert reports whether the "agent-pause-owner-save-failed"
// system alert is currently raised on the server.
func hasPauseOwnerSaveAlert(t *testing.T, s *Server) bool {
	t.Helper()
	s.systemAlertsMu.RLock()
	defer s.systemAlertsMu.RUnlock()
	for _, a := range s.systemAlerts {
		if a.ID == "agent-pause-owner-save-failed" {
			return true
		}
	}
	return false
}

// TestClaimAgentPauseOwnership_UnknownAgentIsNoOp: a claim for an agent that
// is not in the config must change nothing and raise no alert — the guard
// clause, not the error path.
func TestClaimAgentPauseOwnership_UnknownAgentIsNoOp(t *testing.T) {
	s, deps := apiServer(t)

	before := len(deps.Config.Agents)
	s.claimAgentPauseOwnership("no-such-agent")

	if len(deps.Config.Agents) != before {
		t.Fatalf("claim for unknown agent mutated config.Agents (len %d -> %d)", before, len(deps.Config.Agents))
	}
	if hasPauseOwnerSaveAlert(t, s) {
		t.Fatal("claim for unknown agent raised the save-failed alert; the guard must return before persistence")
	}
}

// TestClaimAgentPauseOwnership_SaveConfigFailureRaisesAlert: when hive.yaml
// cannot be written, the operator must see the agent-pause-owner-save-failed
// alert — otherwise the resume silently lasts only until the next pod roll,
// which is exactly the #5706 failure mode the marker exists to prevent.
func TestClaimAgentPauseOwnership_SaveConfigFailureRaisesAlert(t *testing.T) {
	s, deps := apiServer(t)
	// A directory as SourcePath makes Config.Save fail on write.
	deps.Config.SourcePath = t.TempDir()
	// Save also writes the PVC runtime/overlay layers at fixed /data paths;
	// redirect them so the test never touches a live host's files. Save
	// reports success while ANY boot-durable layer is written, so the
	// runtime path must fail too (nonexistent parent dir) for the error
	// branch to fire.
	oldRuntime, oldOverlay := config.RuntimeConfigFile, config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(t.TempDir(), "no-such-dir", "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(t.TempDir(), "hive.yaml.dashboard")
	t.Cleanup(func() {
		config.RuntimeConfigFile = oldRuntime
		config.DashboardOverlayFile = oldOverlay
	})

	s.claimAgentPauseOwnership("scanner")

	if !hasPauseOwnerSaveAlert(t, s) {
		t.Fatal("hive.yaml save failed but no agent-pause-owner-save-failed alert was raised")
	}
	// The in-memory claim must still be recorded so THIS process honors it.
	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("in-memory ownership marker missing after failed persist")
	}
}

// TestClaimAgentPauseOwnership_OverlaySaveFailureRaisesAlert: for a MANAGED
// agent the overlay file replaces the hive.yaml entry on every config load,
// so a failed overlay write is a real durability loss and must alert even
// when the hive.yaml write succeeded (here: skipped via empty SourcePath).
func TestClaimAgentPauseOwnership_OverlaySaveFailureRaisesAlert(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.SourcePath = "" // saveConfig no-ops; isolate the overlay branch

	ac := deps.Config.Agents["scanner"]
	ac.Managed = true
	deps.Config.Agents["scanner"] = ac

	// A FILE as AgentsDir makes SaveAgentFile's MkdirAll fail.
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Config.Data.AgentsDir = notADir

	s.claimAgentPauseOwnership("scanner")

	if !hasPauseOwnerSaveAlert(t, s) {
		t.Fatal("agent overlay save failed but no agent-pause-owner-save-failed alert was raised")
	}
}

// TestClaimAgentPauseOwnership_UnmanagedAgentSkipsOverlay: an unmanaged agent
// has no overlay layer, so the claim must succeed without writing (or
// attempting to write) an overlay file, and must clear any stale alert.
func TestClaimAgentPauseOwnership_UnmanagedAgentSkipsOverlay(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.SourcePath = ""
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir

	s.AddSystemAlert("agent-pause-owner-save-failed", "error", "stale alert from an earlier failure")
	s.claimAgentPauseOwnership("scanner") // Managed is false in testDeps

	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("claim did not mark unmanaged agent operator-owned")
	}
	if _, err := os.Stat(filepath.Join(dir, "scanner.yaml")); !os.IsNotExist(err) {
		t.Fatalf("overlay file written for an UNMANAGED agent (stat err=%v); overlay writes are managed-only", err)
	}
	if hasPauseOwnerSaveAlert(t, s) {
		t.Fatal("successful claim did not clear the stale agent-pause-owner-save-failed alert")
	}
}

// TestClaimAgentPauseOwnership_SuccessPersistsOverlayAndClearsAlert: the full
// success path for a managed agent — marker lands in the overlay FILE and a
// stale failure alert is cleared.
func TestClaimAgentPauseOwnership_SuccessPersistsOverlayAndClearsAlert(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.SourcePath = ""
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir

	ac := deps.Config.Agents["scanner"]
	ac.Managed = true
	deps.Config.Agents["scanner"] = ac

	s.AddSystemAlert("agent-pause-owner-save-failed", "error", "stale alert from an earlier failure")
	s.claimAgentPauseOwnership("scanner")

	data, err := os.ReadFile(filepath.Join(dir, "scanner.yaml"))
	if err != nil {
		t.Fatalf("reading agent overlay: %v", err)
	}
	if !strings.Contains(string(data), "pause_owner: "+config.FieldOwnerOperator) {
		t.Fatalf("overlay does not persist the ownership marker:\n%s", data)
	}
	if hasPauseOwnerSaveAlert(t, s) {
		t.Fatal("successful claim did not clear the stale agent-pause-owner-save-failed alert")
	}
}
