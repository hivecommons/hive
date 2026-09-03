package dashboard

// Tests for #5706: the ACMM pack visibility sweep must not pause agents whose
// pause/run state the operator owns. Before the fix, syncAgentVisibility —
// which runs inside ApplyPack on EVERY startup ("ACMM pack applied on
// startup") — paused every non-pack agent with reason "agent not in pack
// level N", including reviewer-role agents the operator had explicitly
// created via POST /api/agents and resumed via POST /api/resume/{agent}. The
// operator's run-state silently reverted on every pod roll. Same
// pack-clobbers-operator-config family as #5632; the fix mirrors the
// ModelOwner/BackendOwner (#5558) and cadence-ownership (#5668) markers.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// syncLevel is a level whose pack roster contains neither "scanner" nor any
// agent added by these tests (level 1 defines guide + brainstorm only), so
// every test agent is a NON-pack agent from the sweep's point of view.
const syncLevel = 1

// TestSyncAgentVisibility_OperatorOwnedNonPackAgentLeftRunning is the
// red-before-fix test for the field report: an operator-created agent
// ("adjudicator", pause_owner: operator) survives the pack sweep running,
// while an unowned non-pack agent is still paused as designed.
func TestSyncAgentVisibility_OperatorOwnedNonPackAgentLeftRunning(t *testing.T) {
	s, deps := apiServer(t)

	owned := config.AgentConfig{
		Backend:    "claude",
		Role:       "reviewer",
		Enabled:    true,
		Managed:    true,
		PauseOwner: config.FieldOwnerOperator,
	}
	deps.Config.Agents["adjudicator"] = owned
	deps.AgentMgr.AddAgent("adjudicator", owned)

	paused, _ := s.syncAgentVisibility(syncLevel)

	for _, name := range paused {
		if name == "adjudicator" {
			t.Fatalf("pack sweep paused operator-owned agent %q (paused=%v) — the operator's run-state must survive a pack apply", name, paused)
		}
	}
	if deps.AgentMgr.IsPaused("adjudicator") {
		t.Fatal("adjudicator is paused after the sweep; an operator-owned non-pack agent must be left running")
	}

	// Control: "scanner" carries no ownership marker and is not in the level's
	// pack, so the sweep must still pause it — the fix is a targeted skip, not
	// a disabling of pack reconciliation.
	if !deps.AgentMgr.IsPaused("scanner") {
		t.Fatal("scanner (no pause ownership) was not paused; the sweep must still reconcile unowned non-pack agents")
	}
}

// TestSyncAgentVisibility_PackOwnedAgentStillPaused pins the other half of the
// contract: an agent whose pause state is pack-owned (or unowned) still gets
// paused when the level's roster does not include it.
func TestSyncAgentVisibility_PackOwnedAgentStillPaused(t *testing.T) {
	s, deps := apiServer(t)

	packMade := config.AgentConfig{
		Backend:    "claude",
		Enabled:    true,
		Managed:    true,
		PauseOwner: config.FieldOwnerPack,
	}
	deps.Config.Agents["pack-made"] = packMade
	deps.AgentMgr.AddAgent("pack-made", packMade)

	_, _ = s.syncAgentVisibility(syncLevel)

	if !deps.AgentMgr.IsPaused("pack-made") {
		t.Fatal("pack-owned non-pack agent was not paused; only OPERATOR ownership exempts an agent from the sweep")
	}
	if !deps.AgentMgr.IsPaused("scanner") {
		t.Fatal("unowned non-pack agent was not paused; pack reconciliation must keep working for pack-managed agents")
	}
}

// TestHandleAgentCreate_ClaimsPauseOwnership is the field scenario end to end:
// creating an agent via POST /api/agents stamps operator pause ownership,
// persists it to the agent overlay file, and a subsequent pack sweep (the
// startup path) leaves the agent running instead of pausing it as
// "not in pack level N".
func TestHandleAgentCreate_ClaimsPauseOwnership(t *testing.T) {
	s, deps := apiServer(t)
	dir := t.TempDir()
	deps.Config.Data.AgentsDir = dir

	rec := doPost(s, "/api/agents", map[string]interface{}{
		"name":  "adjudicator",
		"agent": map[string]interface{}{"backend": "claude", "role": "reviewer"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	ac, ok := deps.Config.Agents["adjudicator"]
	if !ok {
		t.Fatal("adjudicator missing from config after create")
	}
	if !ac.PauseIsOperatorOwned() {
		t.Fatalf("pause_owner = %q after dashboard-API create, want %q", ac.PauseOwner, config.FieldOwnerOperator)
	}

	// The marker must be in the overlay FILE, not only in memory: the overlay
	// replaces the hive.yaml entry on every config load, so an in-memory-only
	// claim would not survive the very restart whose pack apply it guards.
	data, err := os.ReadFile(filepath.Join(dir, "adjudicator.yaml"))
	if err != nil {
		t.Fatalf("reading agent overlay: %v", err)
	}
	if !strings.Contains(string(data), "pause_owner: "+config.FieldOwnerOperator) {
		t.Fatalf("agent overlay does not persist the ownership marker:\n%s", data)
	}

	// The startup pack apply must now leave the agent running.
	paused, _ := s.syncAgentVisibility(syncLevel)
	if deps.AgentMgr.IsPaused("adjudicator") {
		t.Fatalf("pack sweep paused the freshly created agent (paused=%v); this is the #5706 boot regression", paused)
	}
}

// TestClaimAgentPauseOwnership_StaysResumedAcrossBoots is the resume-claims-
// ownership round trip: once the operator's resume has claimed ownership, the
// sweep leaves the agent running on this boot AND the next one.
func TestClaimAgentPauseOwnership_StaysResumedAcrossBoots(t *testing.T) {
	s, deps := apiServer(t)

	if deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("precondition: scanner must start with no pause ownership")
	}
	s.claimAgentPauseOwnership("scanner")
	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("claimAgentPauseOwnership did not mark scanner operator-owned")
	}

	// Two sweeps = two boots. Before the fix the FIRST one already re-paused.
	for boot := 1; boot <= 2; boot++ {
		_, _ = s.syncAgentVisibility(syncLevel)
		if deps.AgentMgr.IsPaused("scanner") {
			t.Fatalf("boot %d: pack sweep re-paused an agent the operator resumed; the claim must hold across restarts", boot)
		}
	}

	// The claim is idempotent — a second resume must not lose it.
	s.claimAgentPauseOwnership("scanner")
	if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
		t.Fatal("repeat claim dropped the ownership marker")
	}
}

// TestHandleResume_ClaimsPauseOwnership drives the claim through the real
// endpoint: an operator resume of a pack-paused agent stamps operator
// ownership. Resume relaunches the agent session, which can legitimately fail
// without a working tmux (same tolerance as TestHandleResume_RealTransition...),
// so the ownership assertion applies only when the resume succeeded.
func TestHandleResume_ClaimsPauseOwnership(t *testing.T) {
	s, deps := apiServer(t)

	if err := deps.AgentMgr.Pause("scanner", "acmm-pack", "agent not in pack level 1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	rec := doOwnerPost(s, "/api/resume/scanner", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("resume status = %d, want 200 or 400", rec.Code)
	}
	if rec.Code == http.StatusOK {
		if !deps.Config.Agents["scanner"].PauseIsOperatorOwned() {
			t.Fatal("operator resume did not claim pause ownership; the resume would silently last only until the next pod roll")
		}
	}
}
