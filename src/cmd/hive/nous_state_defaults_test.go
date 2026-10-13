package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// loadNousState builds the dashboard's nous panel state from optional on-disk
// artifacts. Whatever is (or is not) on disk, the projected Status map must be
// internally consistent: the legacy and new key spellings must agree, the
// counts must match the loaded slices, and the phase must follow the ledger —
// a Status map that disagrees with the state it summarizes renders a lying
// panel.
func TestLoadNousStateStatusConsistency(t *testing.T) {
	state := loadNousState(nousstore.New(filepath.Join(t.TempDir(), "nous")), restoreTestLogger())

	if state.Mode != "observe" || state.Scope != "governor" {
		t.Errorf("mode/scope = %q/%q, want observe/governor", state.Mode, state.Scope)
	}

	wantPhase := "collecting"
	if len(state.Ledger) > 0 {
		wantPhase = "observing"
	}
	if state.Phase != wantPhase {
		t.Errorf("phase = %q with %d ledger iterations, want %q", state.Phase, len(state.Ledger), wantPhase)
	}

	// Legacy and new key spellings must never diverge.
	if state.Status["snapshots"] != state.Status["snapshotCount"] {
		t.Errorf("snapshots %v != snapshotCount %v", state.Status["snapshots"], state.Status["snapshotCount"])
	}
	if state.Status["principles"] != state.Status["principleCount"] {
		t.Errorf("principles %v != principleCount %v", state.Status["principles"], state.Status["principleCount"])
	}

	// Counts must reflect the loaded state, not a stale or hardcoded value.
	if got, ok := state.Status["iterations"].(int); !ok || got != len(state.Ledger) {
		t.Errorf("iterations = %v, want %d", state.Status["iterations"], len(state.Ledger))
	}
	if got, ok := state.Status["principles"].(int); !ok || got != len(state.Principles) {
		t.Errorf("principles = %v, want %d", state.Status["principles"], len(state.Principles))
	}

	// The Status map's own mode/scope/phase must mirror the struct fields.
	if state.Status["mode"] != state.Mode || state.Status["scope"] != state.Scope || state.Status["phase"] != state.Phase {
		t.Errorf("Status mode/scope/phase %v/%v/%v diverge from state %q/%q/%q",
			state.Status["mode"], state.Status["scope"], state.Status["phase"], state.Mode, state.Scope, state.Phase)
	}

	// The baseline target rides from the dashboard package's single constant.
	if state.Status["baseline_target"] != dashboard.NousBaselineTarget {
		t.Errorf("baseline_target = %v, want %d", state.Status["baseline_target"], dashboard.NousBaselineTarget)
	}

	// baseline_pct must be the shared, capped helper's output — never an
	// unbounded raw percentage (hivecommons/hive#10026).
	snapshotCount, ok := state.Status["snapshotCount"].(int)
	if !ok {
		t.Fatalf("snapshotCount = %v (%T), want int", state.Status["snapshotCount"], state.Status["snapshotCount"])
	}
	wantPct := dashboard.NousBaselinePct(snapshotCount)
	if got, ok := state.Status["baseline_pct"].(float64); !ok || got != wantPct {
		t.Errorf("baseline_pct = %v, want %v (dashboard.NousBaselinePct(%d))", state.Status["baseline_pct"], wantPct, snapshotCount)
	}
	if got := state.Status["baseline_pct"].(float64); got > 100 {
		t.Errorf("baseline_pct = %v, must never exceed 100", got)
	}
}

func TestLoadNousStateFromPathsLoadsHermeticArtifacts(t *testing.T) {
	governorDir := t.TempDir()
	snapshotDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(governorDir, "ledger.json"), []byte(`{"iterations":[{"id":"one"},{"id":"two"}]}`), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	if err := os.WriteFile(filepath.Join(governorDir, "principles.json"), []byte(`{"principles":[{"id":"p1","statement":"Prefer seams","confidence":"high","category":"testing"}]}`), 0o600); err != nil {
		t.Fatalf("write principles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, "snapshot.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	state := loadNousStateFromPaths(restoreTestLogger(), governorDir, snapshotDir)
	if state.Phase != "observing" || len(state.Ledger) != 2 {
		t.Fatalf("phase/ledger = %q/%d, want observing/2", state.Phase, len(state.Ledger))
	}
	if len(state.Principles) != 1 || state.Principles[0].ID != "p1" || state.Principles[0].Confidence != 0.9 {
		t.Fatalf("principles = %+v, want parsed p1 with confidence", state.Principles)
	}
	if state.Status["snapshotCount"] != 1 || state.Status["iterations"] != 2 || state.Status["principles"] != 1 {
		t.Fatalf("status counts = %+v, want one snapshot, two iterations, one principle", state.Status)
	}
}

// Durable layout (#11694): ensureNousDirs creates <root>/governor and
// <root>/snapshots and copies legacy /var/run-style governor state forward;
// loadNousState then reads the durable copy and re-applies the persisted
// panel configuration so mode/scope/config survive a restart.
func TestEnsureNousDirsMigratesLegacyAndLoadRestoresPanel(t *testing.T) {
	base := t.TempDir()
	layout := nousstore.New(filepath.Join(base, "nous"))
	legacy := filepath.Join(base, "legacy-governor")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, nousstore.LedgerFile), []byte(`{"iterations":[{"id":"one"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := restoreTestLogger()
	ensureNousDirsFrom(layout, legacy, logger)

	for _, dir := range []string{layout.GovernorDir(), layout.SnapshotDir()} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("%s not created: %v", dir, err)
		}
	}
	if _, err := os.Stat(layout.LedgerPath()); err != nil {
		t.Fatalf("legacy ledger not migrated into durable root: %v", err)
	}

	// Persisted panel config from a previous run.
	if err := layout.SavePanelConfig(nousstore.PanelConfig{
		Mode:   "suggest",
		Scope:  "both",
		Config: map[string]interface{}{"goals": map[string]interface{}{"governor": "lower MTTR"}},
	}, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	state := loadNousStateFrom(layout, legacy, logger)
	if len(state.Ledger) != 1 || state.Phase != "observing" {
		t.Fatalf("ledger/phase = %d/%q, want 1/observing from the durable copy", len(state.Ledger), state.Phase)
	}
	if state.Mode != "suggest" || state.Scope != "both" {
		t.Fatalf("mode/scope = %q/%q, want restored suggest/both", state.Mode, state.Scope)
	}
	if state.Status["mode"] != "suggest" || state.Status["scope"] != "both" {
		t.Fatalf("status mirrors = %v/%v", state.Status["mode"], state.Status["scope"])
	}
	if goals, _ := state.Config["goals"].(map[string]interface{}); goals["governor"] != "lower MTTR" {
		t.Fatalf("config not restored: %+v", state.Config)
	}
}

// When the durable root is empty but the legacy dir has state (migration
// could not write, say), the loader falls back to reading the legacy dir.
func TestLoadNousStateFallsBackToLegacyDir(t *testing.T) {
	base := t.TempDir()
	layout := nousstore.New(filepath.Join(base, "nous"))
	legacy := filepath.Join(base, "legacy-governor")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, nousstore.PrinciplesFile), []byte(`{"principles":[{"id":"p1","statement":"s","confidence":"low","category":"c"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := loadNousStateFrom(layout, legacy, restoreTestLogger())
	if len(state.Principles) != 1 || state.Principles[0].ID != "p1" {
		t.Fatalf("principles = %+v, want p1 from legacy fallback", state.Principles)
	}
}

// A corrupt panel-config file is logged and ignored; defaults still load.
func TestLoadNousStateIgnoresCorruptPanelConfig(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	if err := os.MkdirAll(layout.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.PanelConfigPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := loadNousStateFrom(layout, "", restoreTestLogger())
	if state.Mode != "observe" || state.Scope != "governor" {
		t.Fatalf("mode/scope = %q/%q, want defaults", state.Mode, state.Scope)
	}
}

// ensureNousDirs must never panic or fail the boot when the root is
// unwritable; it logs and moves on.
func TestEnsureNousDirsToleratesUnwritableRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, nousstore.LedgerFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureNousDirsFrom(nousstore.New(file), legacy, restoreTestLogger())
}

// The production wrappers bind the legacy constant; exercised on temp roots
// so nothing touches /var/run or /data.
func TestNousProductionWrappersUseLegacyConstant(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	ensureNousDirs(layout, restoreTestLogger())
	state := loadNousState(layout, restoreTestLogger())
	if state == nil || state.Mode != "observe" {
		t.Fatalf("state = %+v", state)
	}
	if legacyNousGovernorDir != nousstore.LegacyGovernorDir {
		t.Fatalf("legacy dir = %q", legacyNousGovernorDir)
	}
}

// The persister written at boot and the loader must agree: what the
// dashboard hands to PersistPanel comes back via loadNousStateFrom after a
// "restart", stamped with the injected clock.
func TestNousPanelPersisterRoundTripsThroughLoad(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	fixed := time.Date(2026, 10, 13, 17, 0, 0, 0, time.UTC)
	persist := nousPanelPersister(layout, func() time.Time { return fixed })

	err := persist(dashboard.NousPanelSnapshot{
		Mode:   "suggest",
		Scope:  "both",
		Config: map[string]interface{}{"goals": map[string]interface{}{"governor": "reduce MTTR"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pc, ok, err := layout.LoadPanelConfig()
	if err != nil || !ok || !pc.SavedAt.Equal(fixed) {
		t.Fatalf("persisted: ok=%v err=%v saved_at=%v", ok, err, pc.SavedAt)
	}

	state := loadNousStateFrom(layout, "", restoreTestLogger())
	if state.Mode != "suggest" || state.Scope != "both" {
		t.Fatalf("after restart mode/scope = %q/%q", state.Mode, state.Scope)
	}
	if goals, _ := state.Config["goals"].(map[string]interface{}); goals["governor"] != "reduce MTTR" {
		t.Fatalf("after restart goals = %+v", state.Config)
	}
	if state.Status["mode"] != "suggest" {
		t.Fatalf("status mirror = %+v", state.Status)
	}
}
