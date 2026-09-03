package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Regression tests for #5632: ApplyPack reported created:1 on every
// steady-state apply because a pack agent the operator disabled is evicted
// from the agent manager by every config reload (ReconcileAgents tracks
// EnabledAgents only), and applyPack counted the futile re-registration as a
// creation. created>0 makes isFirstApplyOrExpansion true, which re-asserted
// the pack's governor cadences over the operator's on every apply.

// applyLevel2 seeds the test server with the L2 pack roster and returns the
// first apply's result. Skips the test when the embedded L2 pack no longer
// carries the agents/cadences these regressions rely on.
func applyLevel2(t *testing.T, srv *Server) *ApplyPackResult {
	t.Helper()
	pack, err := config.ACMMPackByLevel(2)
	if err != nil {
		t.Fatal(err)
	}
	if pack.Governor.Cadences["surge"]["scanner"] == "" {
		t.Skip("L2 pack no longer defines a surge cadence for scanner")
	}
	result, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("first ApplyPack(2): %v", err)
	}
	return result
}

func TestApplyPackFreshApplySeedsPackCadences(t *testing.T) {
	srv := newFullServer(t)
	result := applyLevel2(t, srv)

	if len(result.Created) == 0 {
		t.Fatalf("fresh apply created no agents: %+v", result)
	}
	pack, _ := config.ACMMPackByLevel(2)
	for mode, cadences := range pack.Governor.Cadences {
		for agent, want := range cadences {
			got := string(srv.deps.Config.Governor.Modes[mode].Cadences[agent])
			if got != want {
				t.Errorf("fresh apply: %s/%s cadence = %q, want pack value %q", mode, agent, got, want)
			}
		}
	}
}

func TestApplyPackSteadyStateReportsZeroCreatedAndPreservesOperatorCadences(t *testing.T) {
	srv := newFullServer(t)
	applyLevel2(t, srv)

	// Operator sets a cadence through the real handler, which must claim
	// ownership of the (mode, agent) entry.
	req := httptest.NewRequest("PUT", "/api/config/agent/scanner/cadences", strings.NewReader(`{"surge":"9h"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigCadences(w, req)
	if w.Code != 200 {
		t.Fatalf("cadence PUT status = %d, body=%s", w.Code, w.Body.String())
	}
	if !srv.deps.Config.Governor.CadenceIsOperatorOwned("surge", "scanner") {
		t.Fatal("cadence PUT did not claim operator ownership")
	}

	// Reproduce the live lifecycle that made the bug: the operator disables a
	// pack agent, and the next config reload rebuilds the manager's process
	// table from EnabledAgents(), evicting it (cmd/hive/main.go reload path).
	brainstorm := srv.deps.Config.Agents["brainstorm"]
	brainstorm.Enabled = false
	srv.deps.Config.Agents["brainstorm"] = brainstorm
	srv.deps.AgentMgr.ReconcileAgents(srv.deps.Config.EnabledAgents())

	// Steady-state apply (the startup "merging pack updates" path).
	result, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("steady-state ApplyPack(2): %v", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("steady-state apply reported created=%v, want none (#5632)", result.Created)
	}
	if got := string(srv.deps.Config.Governor.Modes["surge"].Cadences["scanner"]); got != "9h" {
		t.Errorf("operator cadence reverted: surge/scanner = %q, want 9h", got)
	}
	for _, change := range resultCadenceChanges(result) {
		if change.Mode == "surge" && change.Agent == "scanner" {
			t.Errorf("steady-state apply reported a change to the operator's cadence: %+v", change)
		}
	}
}

func TestApplyPackForceRespectsOperatorOwnedCadence(t *testing.T) {
	srv := newFullServer(t)
	applyLevel2(t, srv)

	mode := srv.deps.Config.Governor.Modes["surge"]
	mode.Cadences["scanner"] = "9h"
	srv.deps.Config.Governor.Modes["surge"] = mode
	srv.deps.Config.Governor.ClaimCadenceOwnership("surge", "scanner")

	// A non-owned cadence drifted; force must re-derive it from the pack while
	// leaving the operator's claim alone — same contract as model ownership.
	mode = srv.deps.Config.Governor.Modes["surge"]
	mode.Cadences["guide"] = "pause"
	srv.deps.Config.Governor.Modes["surge"] = mode

	if _, err := srv.ApplyPackForce(2); err != nil {
		t.Fatalf("ApplyPackForce(2): %v", err)
	}
	if got := string(srv.deps.Config.Governor.Modes["surge"].Cadences["scanner"]); got != "9h" {
		t.Errorf("forced apply reverted operator cadence: surge/scanner = %q, want 9h", got)
	}
	pack, _ := config.ACMMPackByLevel(2)
	if want := pack.Governor.Cadences["surge"]["guide"]; string(srv.deps.Config.Governor.Modes["surge"].Cadences["guide"]) != want {
		t.Errorf("forced apply did not re-derive pack-owned cadence: surge/guide = %q, want %q",
			srv.deps.Config.Governor.Modes["surge"].Cadences["guide"], want)
	}
}

func resultCadenceChanges(result *ApplyPackResult) []GovernorCadenceChange {
	if result == nil || result.GovernorChanges == nil {
		return nil
	}
	return result.GovernorChanges.Cadences
}
