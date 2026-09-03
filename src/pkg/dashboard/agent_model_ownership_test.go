package dashboard

import (
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// enableScanner mirrors production, where every agent defaults to enabled
// (config.applyDefaults); the shared fixture leaves the field at its zero value.
func enableScanner(t *testing.T, srv *Server) {
	t.Helper()
	sc := srv.deps.Config.Agents["scanner"]
	sc.Enabled = true
	srv.deps.Config.Agents["scanner"] = sc
}

// TestApplyPackPreservesOperatorModel is the regression test for the reported
// bug: an operator sets a model in the Governor grid, and it reverts to the
// pack's model. ApplyPack runs on EVERY hive restart ("merging pack updates"),
// and it used to replace Model on diff unconditionally — so the operator's
// choice came back as the pack default every restart.
func TestApplyPackPreservesOperatorModel(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}

	packModel := srv.deps.Config.Agents["scanner"].Model
	if packModel == "" {
		t.Fatal("precondition: pack should have set a model")
	}

	// Operator picks a different model in the grid.
	const operatorModel = "some-other-model"
	srv.claimAgentFieldOwnership("scanner", operatorModel, "")

	// Restart: the pack is re-applied.
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3) re-apply: %v", err)
	}

	if got := srv.deps.Config.Agents["scanner"].Model; got != operatorModel {
		t.Errorf("operator model reverted: got %q, want %q (pack model %q)", got, operatorModel, packModel)
	}

	// And it must keep surviving repeated restarts — the user reported four.
	for i := 0; i < 4; i++ {
		if _, err := srv.ApplyPack(3); err != nil {
			t.Fatalf("ApplyPack(3) restart %d: %v", i, err)
		}
	}
	if got := srv.deps.Config.Agents["scanner"].Model; got != operatorModel {
		t.Errorf("operator model reverted after repeated restarts: got %q, want %q", got, operatorModel)
	}
}

// TestApplyPackPreservesOperatorBackend covers the grid's "method" column.
func TestApplyPackPreservesOperatorBackend(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}

	const operatorBackend = "claude"
	srv.claimAgentFieldOwnership("scanner", "", operatorBackend)

	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3) re-apply: %v", err)
	}

	if got := srv.deps.Config.Agents["scanner"].Backend; got != operatorBackend {
		t.Errorf("operator backend reverted: got %q, want %q", got, operatorBackend)
	}
}

// TestApplyPackStillReconcilesPackOwnedModel guards the other direction: a
// model the operator never touched must still follow the pack across a level
// change, otherwise agents keep behaving at their old level.
func TestApplyPackStillReconcilesPackOwnedModel(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}

	// Force a stale pack-owned value, as a previous level's pack would leave.
	sc := srv.deps.Config.Agents["scanner"]
	sc.Model = "stale-previous-level-model"
	sc.ModelOwner = config.FieldOwnerPack
	srv.deps.Config.Agents["scanner"] = sc

	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3) re-apply: %v", err)
	}

	if got := srv.deps.Config.Agents["scanner"].Model; got == "stale-previous-level-model" {
		t.Error("pack-owned model was not reconciled to the pack value")
	}
}

// TestClaimAgentFieldOwnershipMarksOwner verifies the ownership markers, which
// are what make the preservation above durable across a restart.
func TestClaimAgentFieldOwnershipMarksOwner(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)

	srv.claimAgentFieldOwnership("scanner", "m1", "b1")
	ac := srv.deps.Config.Agents["scanner"]

	if !ac.ModelIsOperatorOwned() {
		t.Errorf("ModelOwner = %q, want %q", ac.ModelOwner, config.FieldOwnerOperator)
	}
	if !ac.BackendIsOperatorOwned() {
		t.Errorf("BackendOwner = %q, want %q", ac.BackendOwner, config.FieldOwnerOperator)
	}
	if ac.Model != "m1" || ac.Backend != "b1" {
		t.Errorf("values not written: model=%q backend=%q", ac.Model, ac.Backend)
	}
}

// TestClaimAgentFieldOwnershipPersistsAgentOverlay guards the authoritative
// layer for managed agents. Config loads replace the hive.yaml agent entry
// wholesale with this file, so updating only the base config silently loses
// both the operator's choice and its ownership marker on the next load.
func TestClaimAgentFieldOwnershipPersistsAgentOverlay(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)

	before := srv.deps.Config.Agents["scanner"]
	if err := config.SaveAgentFile(srv.deps.Config.Data.AgentsDir, "scanner", before); err != nil {
		t.Fatalf("seed stale agent overlay: %v", err)
	}

	srv.claimAgentFieldOwnership("scanner", "operator-model", "operator-backend")

	overlays, err := config.LoadAgentOverrides(srv.deps.Config.Data.AgentsDir)
	if err != nil {
		t.Fatalf("load agent overlays: %v", err)
	}
	got, ok := overlays["scanner"]
	if !ok {
		t.Fatal("scanner overlay was not persisted")
	}
	if got.Model != "operator-model" || got.Backend != "operator-backend" {
		t.Errorf("overlay values = model %q, backend %q; want operator-model, operator-backend", got.Model, got.Backend)
	}
	if !got.ModelIsOperatorOwned() || !got.BackendIsOperatorOwned() {
		t.Errorf("overlay owners = model %q, backend %q; want both %q", got.ModelOwner, got.BackendOwner, config.FieldOwnerOperator)
	}
}

// TestClaimAgentFieldOwnershipPartial checks that an empty argument leaves the
// other field (and its owner) untouched.
func TestClaimAgentFieldOwnershipPartial(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	before := srv.deps.Config.Agents["scanner"].Backend

	srv.claimAgentFieldOwnership("scanner", "only-model", "")
	ac := srv.deps.Config.Agents["scanner"]

	if ac.Backend != before {
		t.Errorf("backend changed: got %q, want %q", ac.Backend, before)
	}
	if ac.BackendOwner != "" {
		t.Errorf("BackendOwner claimed unexpectedly: %q", ac.BackendOwner)
	}
	if !ac.ModelIsOperatorOwned() {
		t.Error("ModelOwner not claimed")
	}
}

// TestPinClaimsOwnership verifies a pin is durable, not decorative: pinning a
// model must make the field operator-owned so the pack cannot reconcile it.
func TestPinClaimsOwnership(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}

	const pinned = "pinned-model"
	req := httptest.NewRequest("POST", "/api/agents/scanner/pin/model", nil)
	req.SetPathValue("agent", "scanner")
	req.SetPathValue("dimension", "model")
	w := httptest.NewRecorder()
	srv.handlePin(w, req)

	if w.Code != 200 {
		t.Fatalf("handlePin: %d %s", w.Code, w.Body.String())
	}
	// With no body the handler pins the CURRENT effective value; either way the
	// field must now be operator-owned so a pack apply cannot move it.
	if !srv.deps.Config.Agents["scanner"].ModelIsOperatorOwned() {
		t.Error("pin did not claim operator ownership — pin is decorative")
	}

	pinnedValue := srv.deps.Config.Agents["scanner"].Model
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack after pin: %v", err)
	}
	if got := srv.deps.Config.Agents["scanner"].Model; got != pinnedValue {
		t.Errorf("pinned model changed across pack apply: got %q, want %q", got, pinnedValue)
	}
	_ = pinned
}

// TestValidateModelForAgentAllowsUnverifiable ensures validation never blocks
// on a cold cache: an unverifiable model is allowed, not rejected.
func TestValidateModelForAgentAllowsUnverifiable(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	if err := srv.validateModelForAgent("scanner", "anything-at-all"); err != nil {
		t.Errorf("cold cache should not reject: %v", err)
	}
}

// TestValidateModelForAgentRejectsUnavailable is the legibility guarantee: a
// model the backend does not offer produces an explicit error instead of being
// stored and silently falling back at launch.
func TestValidateModelForAgentRejectsUnavailable(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)

	backend := srv.deps.Config.Agents["scanner"].Backend
	srv.cliModels.set(backend, cliModelResult{models: []string{"real-a", "real-b"}, fallback: false})

	err := srv.validateModelForAgent("scanner", "not-a-real-model")
	if err == nil {
		t.Fatal("expected an error for an unavailable model")
	}
	if !contains([]string{"real-a", "real-b"}, "real-a") {
		t.Fatal("fixture sanity")
	}
	t.Logf("legible error: %v", err)

	if err := srv.validateModelForAgent("scanner", "real-a"); err != nil {
		t.Errorf("available model rejected: %v", err)
	}
}

// TestValidateModelForAgentIgnoresFallbackList verifies the static fallback
// list is not used to reject, since it is a maintained guess.
func TestValidateModelForAgentIgnoresFallbackList(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)

	backend := srv.deps.Config.Agents["scanner"].Backend
	srv.cliModels.set(backend, cliModelResult{models: []string{"only-this"}, fallback: true})

	if err := srv.validateModelForAgent("scanner", "something-else"); err != nil {
		t.Errorf("fallback list must not reject: %v", err)
	}
}
