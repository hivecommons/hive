package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Regression tests for #7503: an agent the ACMM pack does NOT manage, whose
// operator configured a mode BELOW the level default, must actually run at
// that mode. On the projectbluefin spoke `reviewer` was `mode: ADVISORY` in
// every config layer yet ran as ISSUES_AND_PRS — the L5 default — because the
// startup ApplyPack cleared Config.Mode for every agent in the process table
// (pack-managed or not) before SyncModeFiles wrote the enforcement files. The
// existing tests pinned the parse and the default table separately; the bug
// lived in the gap between them, so these assert the EFFECTIVE mode through
// the same gate AuthorizePROpen enforces.
//
// The original spoke agent was `reviewer`, which the L5/L6 packs have managed
// since #8023. These tests need an agent NO pack lists — that is the whole
// hypothesis — so they use a stand-in name instead. The skip below fires if
// some future pack adopts it too, which is the signal to pick another.
const nonPackAgentName = "librarian"

// seedAdvisoryNonPackAgent adds a non-pack agent configured ADVISORY to the
// server's config and process table, the way a per-agent overlay file lands it
// at boot.
func seedAdvisoryNonPackAgent(t *testing.T, srv *Server) {
	t.Helper()
	for _, name := range config.ACMMPackManagedAgentNames() {
		if name == nonPackAgentName {
			t.Skipf("an ACMM pack now manages %q; this regression needs an agent no pack lists", nonPackAgentName)
		}
	}
	agentCfg := config.AgentConfig{
		ID: nonPackAgentName, Role: nonPackAgentName, Backend: "claude", Model: "sonnet",
		DisplayName: "Librarian", Enabled: true, Mode: "ADVISORY",
		// Dashboard-created, so the visibility sweep leaves it running
		// (#5706) — the state the spoke's agent was actually in.
		PauseOwner: config.FieldOwnerOperator,
	}
	srv.deps.Config.Agents[nonPackAgentName] = agentCfg
	srv.deps.AgentMgr.AddAgent(nonPackAgentName, agentCfg)
}

func TestApplyPackKeepsOperatorModeOnNonPackAgent(t *testing.T) {
	srv := newFullServer(t)
	seedAdvisoryNonPackAgent(t, srv)

	// The startup path: "merging pack updates" at the persisted level.
	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	_, canOpenPR, _, ok := srv.deps.AgentMgr.AgentCapabilities(nonPackAgentName)
	if !ok {
		t.Fatal("librarian missing from the manager after ApplyPack")
	}
	if canOpenPR {
		t.Errorf("librarian configured ADVISORY can open PRs after ApplyPack(5): its Config.Mode was cleared and the L5 default (ISSUES_AND_PRS) took over (#7503)")
	}
	proc, err := srv.deps.AgentMgr.GetStatus(nonPackAgentName)
	if err != nil {
		t.Fatal(err)
	}
	if proc.Config.Mode != "ADVISORY" {
		t.Errorf("librarian process-table Mode = %q, want ADVISORY", proc.Config.Mode)
	}

	// Unchanged contract for PACK agents: their mode is still re-derived from
	// the level, so a stale lower-level mode does not stick.
	_, scannerCanOpenPR, _, ok := srv.deps.AgentMgr.AgentCapabilities("scanner")
	if !ok {
		t.Fatal("scanner missing from the manager after ApplyPack")
	}
	if !scannerCanOpenPR {
		t.Errorf("pack agent scanner should be push-capable at L5 after ApplyPack(5)")
	}
}

func TestSetLevelKeepsOperatorModeOnNonPackAgent(t *testing.T) {
	srv := newFullServer(t)
	seedAdvisoryNonPackAgent(t, srv)

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":5}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT /api/packs/level status = %d, body=%s", w.Code, w.Body.String())
	}

	// The persisted config is what the fsnotify reload re-reads; wiping the
	// operator's mode there would lose it on the next reload even if the
	// process table kept it.
	if got := srv.deps.Config.Agents[nonPackAgentName].Mode; got != "ADVISORY" {
		t.Errorf("persisted librarian Mode after level change = %q, want ADVISORY (#7503)", got)
	}
	_, canOpenPR, _, ok := srv.deps.AgentMgr.AgentCapabilities(nonPackAgentName)
	if !ok {
		t.Fatal("librarian missing from the manager after level change")
	}
	if canOpenPR {
		t.Errorf("librarian configured ADVISORY can open PRs after an explicit level change to 5 (#7503)")
	}
	// Pack agents still have their persisted mode cleared so the reload cannot
	// re-apply a stale pack mode — the reason the clear exists.
	if got := srv.deps.Config.Agents["scanner"].Mode; got != "" {
		pack, _ := config.ACMMPackByLevel(5)
		for _, pa := range pack.Agents {
			if pa.Name == "scanner" && pa.Mode != got {
				t.Errorf("pack agent scanner persisted Mode = %q after level change, want %q or empty", got, pa.Mode)
			}
		}
	}
}

// TestSetLevelDowngradeStillClearsHigherPackAgentMode pins why the clear is
// scoped to the UNION of every pack's roster rather than the target level's:
// on L5 → L2, strategist is not in the L2 pack but stays in the config with
// the L5 pack's ISSUES_AND_PRS. That is a stale pack mode and must still be
// cleared, or an operator resume would run it at L5 authority on an L2 hive.
// The operator-only librarian keeps its ADVISORY through the same downgrade.
func TestSetLevelDowngradeStillClearsHigherPackAgentMode(t *testing.T) {
	srv := newFullServer(t)
	seedAdvisoryNonPackAgent(t, srv)
	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}
	strategist, ok := srv.deps.Config.Agents["strategist"]
	if !ok {
		t.Skip("L5 pack no longer creates `strategist`")
	}
	if strategist.Mode == "" {
		t.Skip("L5 pack no longer seeds a mode for `strategist`")
	}

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":2}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT /api/packs/level status = %d, body=%s", w.Code, w.Body.String())
	}

	if got := srv.deps.Config.Agents["strategist"].Mode; got != "" {
		t.Errorf("strategist (L5 pack agent, not in L2) kept persisted Mode %q after downgrade; the stale pack mode must be cleared", got)
	}
	if got := srv.deps.Config.Agents[nonPackAgentName].Mode; got != "ADVISORY" {
		t.Errorf("operator-only librarian Mode after downgrade = %q, want ADVISORY", got)
	}
}
