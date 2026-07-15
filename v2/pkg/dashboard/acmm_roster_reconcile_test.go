package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// packAgentNames returns the non-hidden agent names defined by a pack level.
func packAgentNames(t *testing.T, level int) []string {
	t.Helper()
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(%d): %v", level, err)
	}
	var names []string
	for _, a := range pack.Agents {
		if !a.Hidden {
			names = append(names, a.Name)
		}
	}
	return names
}

// TestApplyPackReconcilesFullRoster is the core regression test for the ACMM
// apply bug: setting a hive's level to N must bring the agent roster AND the
// governor cadences up to level N's pack. Upgrading from a lower level must ADD
// every agent the higher level introduces (architect + strategist at L5), each
// with its pack cadence in every governor mode.
func TestApplyPackReconcilesFullRoster(t *testing.T) {
	srv := newFullServer(t) // starts at L2 with only scanner in config

	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	// Every non-hidden agent the L5 pack defines must be in config.Agents.
	for _, name := range packAgentNames(t, 5) {
		if _, ok := srv.deps.Config.Agents[name]; !ok {
			t.Errorf("agent %q missing from config after L5 apply", name)
		}
	}
	// strategist is the specific agent that went missing on kellyaa.
	if _, ok := srv.deps.Config.Agents["strategist"]; !ok {
		t.Error("strategist missing from config.Agents after L5 apply")
	}

	// Cadences must also be reconciled: every governor mode must carry a
	// strategist cadence, otherwise a newly-added agent never gets scheduled.
	pack, err := config.ACMMPackByLevel(5)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(5): %v", err)
	}
	for modeName, packCadences := range pack.Governor.Cadences {
		mode, ok := srv.deps.Config.Governor.Modes[modeName]
		if !ok {
			t.Errorf("governor mode %q missing after L5 apply", modeName)
			continue
		}
		for agent, want := range packCadences {
			got, ok := mode.Cadences[agent]
			if !ok {
				t.Errorf("mode %q: cadence for %q missing after L5 apply", modeName, agent)
				continue
			}
			if got != want {
				t.Errorf("mode %q: cadence for %q = %q, want %q", modeName, agent, got, want)
			}
		}
	}
}

// TestApplyPackAllLevelsRosterMatchesPack verifies that applying any level N
// leaves config.Agents a superset of level N's non-hidden pack roster.
func TestApplyPackAllLevelsRosterMatchesPack(t *testing.T) {
	for level := 1; level <= 6; level++ {
		srv := newFullServer(t)
		if _, err := srv.ApplyPack(level); err != nil {
			t.Fatalf("ApplyPack(%d): %v", level, err)
		}
		for _, name := range packAgentNames(t, level) {
			if _, ok := srv.deps.Config.Agents[name]; !ok {
				t.Errorf("L%d: agent %q missing from config after apply", level, name)
			}
		}
	}
}

// TestApplyPackReconcilesOrphanAgent covers the orphan case: an agent already
// present in config as a bare/leftover entry (the leftover strategist that was
// running but had no proper config backing) must be reconciled with its pack
// fields and cadence when the level that includes it is applied — turning an
// orphan into a legitimately configured agent rather than leaving it stranded.
func TestApplyPackReconcilesOrphanAgent(t *testing.T) {
	srv := newFullServer(t)
	// Pre-seed a bare strategist entry (no backend/model/kick template).
	srv.deps.Config.Agents["strategist"] = config.AgentConfig{Role: "strategist"}

	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	sc := srv.deps.Config.Agents["strategist"]
	if sc.KickTemplate == "" {
		t.Error("orphan strategist did not get its pack kick_template after apply")
	}
	if sc.Mode == "" {
		t.Error("orphan strategist did not get its pack mode after apply")
	}
	for modeName := range map[string]struct{}{"surge": {}, "busy": {}, "quiet": {}, "idle": {}} {
		mode := srv.deps.Config.Governor.Modes[modeName]
		if _, ok := mode.Cadences["strategist"]; !ok {
			t.Errorf("mode %q: strategist cadence missing after orphan reconcile", modeName)
		}
	}
}

// TestSetLevelFailsWhenRosterReconcileFails locks in the fix for the decoupling
// bug: PUT /api/packs/level must NOT report success when the roster could not
// be reconciled to the requested level. Previously it wrote acmm_level, then
// only logged a warning if ApplyPack failed, returning HTTP 200 — leaving
// "level says N, roster says fewer" with no error surfaced.
func TestSetLevelFailsWhenRosterReconcileFails(t *testing.T) {
	srv := newFullServer(t)
	// Force ApplyPack to fail: no agents dir configured.
	srv.deps.Config.Data.AgentsDir = ""

	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":5}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handlePackSetLevel(w, req)

	if w.Code == 200 {
		t.Fatalf("expected non-200 when roster reconcile fails, got 200 body=%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reconciliation failed") {
		t.Errorf("expected reconciliation-failed error, got body=%s", w.Body.String())
	}
}

// TestApplyPackReconcilesStalePackFields is the regression test for the
// level-change drift bug: an agent carrying pack-behavior fields from a LOWER
// level (kick_template, mode, model, description) must be reconciled to the
// CURRENT level's pack when the level changes. Before the fix, kick_template
// and mode reconciled but model/description/role did not — so hives that
// climbed levels (kellyaa L5, ai-native L3, console L6) kept scanner/quality on
// their old advisory templates and sonnet model even though acmm_level moved up.
func TestApplyPackReconcilesStalePackFields(t *testing.T) {
	srv := newFullServer(t) // L2, scanner present

	// Seed the scanner with STALE fields as if left over from a lower level:
	// the L2/L3 advisory template + description, and the sonnet model.
	scanner := srv.deps.Config.Agents["scanner"]
	scanner.KickTemplate = "scanner-advisory.md"
	scanner.Mode = "ADVISORY"
	scanner.Model = "claude-sonnet-4-6"
	scanner.Description = "Scans code for issues and suggests fixes, but does NOT create PRs or issues."
	srv.deps.Config.Agents["scanner"] = scanner

	if _, err := srv.ApplyPack(5); err != nil {
		t.Fatalf("ApplyPack(5): %v", err)
	}

	// The L5 pack's scanner definition is the source of truth.
	pack, err := config.ACMMPackByLevel(5)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(5): %v", err)
	}
	var want config.PackAgent
	for _, pa := range pack.Agents {
		if pa.Name == "scanner" {
			want = pa
		}
	}
	if want.Name == "" {
		t.Fatal("scanner not found in L5 pack")
	}

	got := srv.deps.Config.Agents["scanner"]
	if got.KickTemplate != want.KickTemplate {
		t.Errorf("kick_template not reconciled: got %q want %q", got.KickTemplate, want.KickTemplate)
	}
	if got.Mode != want.Mode {
		t.Errorf("mode not reconciled: got %q want %q", got.Mode, want.Mode)
	}
	if want.Model != "" && got.Model != want.Model {
		t.Errorf("model not reconciled (the specific gap): got %q want %q", got.Model, want.Model)
	}
	if want.Description != "" && got.Description != want.Description {
		t.Errorf("description not reconciled: got %q want %q", got.Description, want.Description)
	}
	// The stale advisory template must be gone.
	if got.KickTemplate == "scanner-advisory.md" {
		t.Error("scanner still on stale advisory template after L5 apply")
	}
}
