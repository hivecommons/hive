package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestHandlePacksListCurrentFlag verifies the list handler marks exactly one
// pack as current (the one matching the configured ACMM level).
func TestHandlePacksListCurrentFlag(t *testing.T) {
	srv := newFullServer(t) // level 2

	req := httptest.NewRequest("GET", "/api/packs", nil)
	w := httptest.NewRecorder()
	srv.handlePacksList(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var packs []struct {
		Level   int  `json:"level"`
		Current bool `json:"current"`
	}
	if err := json.NewDecoder(w.Body).Decode(&packs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(packs) == 0 {
		t.Fatal("expected at least one pack")
	}

	var currentCount int
	for _, p := range packs {
		if p.Current {
			currentCount++
			if p.Level != 2 {
				t.Errorf("current pack level = %d, want 2", p.Level)
			}
		}
	}
	if currentCount != 1 {
		t.Errorf("exactly 1 pack should be current, got %d", currentCount)
	}
}

// TestHandlePacksListIncludesAgents verifies the response includes the agents
// array and agent count for each pack.
func TestHandlePacksListIncludesAgents(t *testing.T) {
	srv := newFullServer(t)

	req := httptest.NewRequest("GET", "/api/packs", nil)
	w := httptest.NewRecorder()
	srv.handlePacksList(w, req)

	var packs []struct {
		Name       string `json:"name"`
		AgentCount int    `json:"agentCount"`
		Agents     []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&packs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, p := range packs {
		if p.AgentCount != len(p.Agents) {
			t.Errorf("pack %q: agentCount=%d but len(agents)=%d", p.Name, p.AgentCount, len(p.Agents))
		}
	}
}

// TestHandlePackApplyResponseBody validates the response structure from a
// successful pack apply.
func TestHandlePackApplyResponseBody(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	req := httptest.NewRequest("POST", "/api/packs/1/apply", nil)
	req.SetPathValue("level", "1")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["ok"] != true {
		t.Error("response missing ok=true")
	}
	if resp["status"] != "applied" {
		t.Errorf("status = %v, want applied", resp["status"])
	}
	if _, ok := resp["created"]; !ok {
		t.Error("response missing created field")
	}
	if _, ok := resp["tombstoned"]; !ok {
		t.Error("response missing tombstoned field")
	}
}

// TestHandlePackApplyInvalidLevelZero verifies that level 0 is rejected as
// invalid (packs are 1-indexed).
func TestHandlePackApplyInvalidLevelZero(t *testing.T) {
	srv := newFullServer(t)

	req := httptest.NewRequest("POST", "/api/packs/0/apply", nil)
	req.SetPathValue("level", "0")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code == 200 {
		t.Errorf("expected error for level 0, got 200")
	}
}

// TestApplyPackTombstonedAgents verifies that a deliberately removed agent is
// NOT re-created by ApplyPack, and is reported in the Tombstoned slice.
func TestApplyPackTombstonedAgents(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	// Mark scanner as removed so the pack will skip it.
	srv.deps.Config.RemovedAgents = append(srv.deps.Config.RemovedAgents, "scanner")
	// Remove from live agents so it would need to be re-created.
	delete(srv.deps.Config.Agents, "scanner")

	pack, err := config.ACMMPackByLevel(2)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(2): %v", err)
	}
	hasScannerInPack := false
	for _, a := range pack.Agents {
		if a.Name == "scanner" {
			hasScannerInPack = true
		}
	}
	if !hasScannerInPack {
		t.Skip("scanner not in L2 pack, cannot test tombstone")
	}

	result, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	found := false
	for _, name := range result.Tombstoned {
		if name == "scanner" {
			found = true
		}
	}
	if !found {
		t.Errorf("scanner not in tombstoned list: %v", result.Tombstoned)
	}
	if _, ok := srv.deps.Config.Agents["scanner"]; ok {
		t.Error("tombstoned scanner was re-created in config")
	}
}

// TestApplyPackPreservesOperatorModelOnReapply verifies that a model set by the
// operator (ModelOwner != pack) is not overwritten on re-apply of the same level.
func TestApplyPackPreservesOperatorModelOnReapply(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Model = "operator-chosen-model"
	scanner.ModelOwner = "operator"
	srv.deps.Config.Agents["scanner"] = scanner

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	got := srv.deps.Config.Agents["scanner"]
	if got.Model != "operator-chosen-model" {
		t.Errorf("operator model overwritten: got %q, want %q", got.Model, "operator-chosen-model")
	}
}

// TestApplyPackFillsEmptyBackend verifies that ApplyPack fills a missing
// backend from the pack definition but does not overwrite an existing one.
func TestApplyPackFillsEmptyBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	// Clear scanner backend so the pack can fill it.
	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = ""
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner

	pack, err := config.ACMMPackByLevel(2)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(2): %v", err)
	}
	var packBackend string
	for _, a := range pack.Agents {
		if a.Name == "scanner" {
			packBackend = a.Backend
		}
	}

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	got := srv.deps.Config.Agents["scanner"]
	if packBackend != "" && got.Backend != packBackend {
		t.Errorf("empty backend not filled: got %q, want %q", got.Backend, packBackend)
	}
}

// TestApplyPackDoesNotOverwriteExistingBackend verifies that an existing
// backend is preserved even when the pack defines a different one.
func TestApplyPackDoesNotOverwriteExistingBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = "user-chosen-backend"
	srv.deps.Config.Agents["scanner"] = scanner

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}

	got := srv.deps.Config.Agents["scanner"]
	if got.Backend != "user-chosen-backend" {
		t.Errorf("existing backend overwritten: got %q, want %q", got.Backend, "user-chosen-backend")
	}
}

// scannerPackBackend returns the level-2 pack's backend for the scanner agent.
func scannerPackBackend(t *testing.T) string {
	t.Helper()
	pack, err := config.ACMMPackByLevel(2)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(2): %v", err)
	}
	for _, a := range pack.Agents {
		if a.Name == "scanner" {
			return a.Backend
		}
	}
	t.Fatal("scanner not in level-2 pack")
	return ""
}

// assertStampedOnly checks an agent whose only change was a persisted
// backend_owner stamp: skipped, not updated, and the stamp is in the overlay.
func assertStampedOnly(t *testing.T, srv *Server, res *ApplyPackResult, wantOwner string) {
	t.Helper()
	for _, n := range res.Updated {
		if n == "scanner" {
			t.Errorf("scanner in Updated for an ownership-only change: %v", res.Updated)
		}
	}
	skipped := false
	for _, n := range res.Skipped {
		skipped = skipped || n == "scanner"
	}
	if !skipped {
		t.Errorf("scanner missing from Skipped: %v", res.Skipped)
	}
	overlays, err := config.LoadAgentOverrides(srv.deps.Config.Data.AgentsDir)
	if err != nil {
		t.Fatalf("load agent overlays: %v", err)
	}
	if got := overlays["scanner"].BackendOwner; got != wantOwner {
		t.Errorf("persisted backend owner = %q, want %q", got, wantOwner)
	}
}

// TestApplyPackAdoptsUnownedBackendMatchingPack verifies an unowned backend
// equal to the pack default is adopted as pack-owned and is not reported as
// preserved (#13221).
func TestApplyPackAdoptsUnownedBackendMatchingPack(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	packBackend := scannerPackBackend(t)
	// Reconcile the pack-behavior fields first so the second apply below
	// has nothing left to change but the ownership stamp.
	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2) seed: %v", err)
	}

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = packBackend
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner

	res, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	got := srv.deps.Config.Agents["scanner"]
	if got.Backend != packBackend || got.BackendOwner != config.FieldOwnerPack {
		t.Errorf("got backend %q owner %q, want %q owner %q", got.Backend, got.BackendOwner, packBackend, config.FieldOwnerPack)
	}
	for _, n := range res.Preserved {
		if n == "scanner" {
			t.Error("scanner reported preserved for a pack-matching backend")
		}
	}
	assertStampedOnly(t, srv, res, config.FieldOwnerPack)
}

// TestApplyPackPreservesUnownedBackendDifferingFromPack verifies a hand-set
// backend with no owner stamp is kept, stamped operator-owned and reported as
// preserved rather than updated (#13221).
func TestApplyPackPreservesUnownedBackendDifferingFromPack(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	packBackend := scannerPackBackend(t)
	// Reconcile the pack-behavior fields first so the second apply below
	// has nothing left to change but the ownership stamp.
	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2) seed: %v", err)
	}

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = "claude"
	if packBackend == "claude" {
		scanner.Backend = "copilot"
	}
	handSet := scanner.Backend
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner

	res, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	got := srv.deps.Config.Agents["scanner"]
	if got.Backend != handSet {
		t.Errorf("hand-set backend replaced: got %q, want %q", got.Backend, handSet)
	}
	if got.BackendOwner != config.FieldOwnerOperator {
		t.Errorf("backend owner = %q, want %q", got.BackendOwner, config.FieldOwnerOperator)
	}
	found := false
	for _, n := range res.Preserved {
		found = found || n == "scanner"
	}
	if !found {
		t.Errorf("scanner missing from Preserved: %v", res.Preserved)
	}
	assertStampedOnly(t, srv, res, config.FieldOwnerOperator)
}

// TestApplyPackMovesUnownedDeniedBackend verifies a hand-set backend with no
// owner stamp that the backends policy denies is moved to the pack backend,
// stamped pack-owned and counted as updated, keeping the #11310 deny intact.
func TestApplyPackMovesUnownedDeniedBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	packBackend := scannerPackBackend(t)

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = "claude"
	if packBackend == "claude" {
		scanner.Backend = "copilot"
	}
	handSet := scanner.Backend
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner
	srv.deps.Config.Backends.Deny = []string{handSet}

	res, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	got := srv.deps.Config.Agents["scanner"]
	if strings.EqualFold(got.Backend, handSet) || got.BackendOwner != config.FieldOwnerPack {
		t.Errorf("got backend %q owner %q, want a non-%q backend owned by %q", got.Backend, got.BackendOwner, handSet, config.FieldOwnerPack)
	}
	updated := false
	for _, n := range res.Updated {
		updated = updated || n == "scanner"
	}
	if !updated {
		t.Errorf("scanner missing from Updated: %v", res.Updated)
	}
	for _, n := range res.Preserved {
		if n == "scanner" {
			t.Error("scanner reported preserved for a denied backend")
		}
	}
}

// TestApplyPackMovesUnownedDeniedPackBackend verifies an unowned backend equal
// to the pack's own raw backend is pack-written, so when the policy denies it
// it is moved to the fallback rather than kept as an operator choice.
func TestApplyPackMovesUnownedDeniedPackBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	packBackend := scannerPackBackend(t)

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = packBackend
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner
	srv.deps.Config.Backends.Deny = []string{packBackend}

	res, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	got := srv.deps.Config.Agents["scanner"]
	if strings.EqualFold(got.Backend, packBackend) || got.BackendOwner != config.FieldOwnerPack {
		t.Errorf("got backend %q owner %q, want a non-%q backend owned by %q", got.Backend, got.BackendOwner, packBackend, config.FieldOwnerPack)
	}
	for _, n := range res.Preserved {
		if n == "scanner" {
			t.Error("scanner reported preserved for the pack's own denied backend")
		}
	}
}

// TestApplyPackAdoptsUnownedBackendCaseInsensitive verifies a hand-typed
// backend differing from the pack only by case is adopted as pack-owned and
// not reported as preserved.
func TestApplyPackAdoptsUnownedBackendCaseInsensitive(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	packBackend := scannerPackBackend(t)

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = strings.ToUpper(packBackend[:1]) + packBackend[1:]
	scanner.BackendOwner = ""
	srv.deps.Config.Agents["scanner"] = scanner

	res, err := srv.ApplyPack(2)
	if err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	if got := srv.deps.Config.Agents["scanner"]; got.BackendOwner != config.FieldOwnerPack {
		t.Errorf("backend owner = %q, want %q", got.BackendOwner, config.FieldOwnerPack)
	}
	for _, n := range res.Preserved {
		if n == "scanner" {
			t.Error("scanner reported preserved for a case-variant of the pack backend")
		}
	}
}

// TestApplyPackReconcilesPackOwnedBackendOffDeniedBackend verifies a
// pack-owned backend is still reconciled when the backends policy denies it.
func TestApplyPackReconcilesPackOwnedBackendOffDeniedBackend(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	scanner := srv.deps.Config.Agents["scanner"]
	scanner.Backend = "claude"
	scanner.BackendOwner = config.FieldOwnerPack
	srv.deps.Config.Agents["scanner"] = scanner
	srv.deps.Config.Backends.Deny = []string{"claude"}

	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	if got := srv.deps.Config.Agents["scanner"]; got.Backend == "claude" {
		t.Errorf("pack-owned denied backend not reconciled: %q", got.Backend)
	}
}

// TestHandlePackSetLevelZero verifies that level 0 is rejected.
func TestHandlePackSetLevelZero(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":0}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for level 0", w.Code)
	}
}

// TestHandlePackSetLevelNegative verifies that a negative level is rejected.
func TestHandlePackSetLevelNegative(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":-1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for negative level", w.Code)
	}
}

// TestDetectACMMLevelDefault verifies the fallback when no level is configured.
func TestDetectACMMLevelDefault(t *testing.T) {
	got := detectACMMLevel(&config.Config{})
	if got != 1 {
		t.Errorf("detectACMMLevel(nil ACMMLevel) = %d, want 1", got)
	}
}

// TestDetectACMMLevelExplicit verifies that a set level is returned as-is.
func TestDetectACMMLevelExplicit(t *testing.T) {
	level := 4
	got := detectACMMLevel(&config.Config{ACMMLevel: &level})
	if got != 4 {
		t.Errorf("detectACMMLevel(4) = %d, want 4", got)
	}
}

// TestApplyPackNoAgentsDir verifies that ApplyPack fails cleanly when
// agents_dir is not configured.
func TestApplyPackNoAgentsDir(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = ""
	// Remove existing agents so the pack tries to create new ones.
	srv.deps.Config.Agents = map[string]config.AgentConfig{}

	_, err := srv.ApplyPack(2)
	if err == nil {
		t.Error("expected error when agents_dir is empty, got nil")
	}
	if !strings.Contains(err.Error(), "agents_dir") {
		t.Errorf("error = %q, want mention of agents_dir", err)
	}
}
