package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/notify"
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

	if state.Mode != "observe" || state.Scope != dashboard.NousScopeRepo {
		t.Errorf("mode/scope = %q/%q, want observe/repo", state.Mode, state.Scope)
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

func TestLoadNousStateLoadsHermeticArtifacts(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.LedgerPath(), []byte(`{"iterations":[{"id":"one"},{"id":"two"}]}`), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	if err := os.WriteFile(layout.PrinciplesPath(), []byte(`{"principles":[{"id":"p1","statement":"Prefer seams","confidence":"high","category":"testing"}]}`), 0o600); err != nil {
		t.Fatalf("write principles: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layout.SnapshotDir(), "snapshot.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	state := loadNousStateFrom(layout, "", restoreTestLogger())
	if state.GovernorDir != layout.GovernorDir() {
		t.Fatalf("GovernorDir = %q, want %q so the lab can tell whether the experiment loop runs", state.GovernorDir, layout.GovernorDir())
	}
	if state.Phase != "observing" || len(state.Ledger) != 2 {
		t.Fatalf("phase/ledger = %q/%d, want observing/2", state.Phase, len(state.Ledger))
	}
	// Rows without type/status load with the schema defaults (#11697).
	for _, e := range state.Ledger {
		if e[nousstore.KeyType] != nousstore.DefaultLedgerType || e[nousstore.KeyStatus] != nousstore.DefaultLedgerStatus {
			t.Fatalf("legacy row not defaulted: %v", e)
		}
	}
	if len(state.Principles) != 1 || state.Principles[0].ID != "p1" || state.Principles[0].Confidence != nousstore.ConfidenceHigh {
		t.Fatalf("principles = %+v, want parsed p1 with confidence", state.Principles)
	}
	if p := state.Principles[0]; p.Text != "Prefer seams" || p.Source != "testing" || p.Type != nousstore.DefaultPrincipleType || p.Status != nousstore.DefaultPrincipleStatus {
		t.Fatalf("principle = %+v, want text/source kept and type/status defaulted", p)
	}
	if state.Status["snapshotCount"] != 1 || state.Status["iterations"] != 2 || state.Status["principles"] != 1 {
		t.Fatalf("status counts = %+v, want one snapshot, two iterations, one principle", state.Status)
	}
}

// Live ledger/principles (#11697): a round the agent writes after boot is
// visible on the next SyncKnowledge — what every /api/nous read performs —
// with no restart, and the Status counts and phase follow. Change detection
// is driven by the file's mtime, so the test sets it explicitly instead of
// sleeping.
func TestLoadNousStateServesKnowledgeLive(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	state := loadNousStateFrom(layout, "", restoreTestLogger())
	if len(state.Ledger) != 0 || state.Phase != "collecting" || state.KnowledgeStore == nil {
		t.Fatalf("fresh state = %d/%q store=%v", len(state.Ledger), state.Phase, state.KnowledgeStore)
	}

	writeAt := func(path, data string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	writeAt(layout.LedgerPath(), `{"iterations":[{"iteration":0,"family":"baseline","candidate_id":"baseline"},{"iteration":1,"family":"routing","h_main_result":"CONFIRMED"}]}`, t0)
	writeAt(layout.PrinciplesPath(), `{"principles":[{"id":"p1","statement":"s","confidence":"medium","category":"meta","status":"updated"}]}`, t0)

	if err := state.SyncKnowledge(); err != nil {
		t.Fatal(err)
	}
	if len(state.Ledger) != 2 || state.Ledger[0][nousstore.KeyType] != nousstore.LedgerTypeBaseline || state.Ledger[1][nousstore.KeyOutcome] != "confirmed" {
		t.Fatalf("live ledger = %v", state.Ledger)
	}
	if len(state.Principles) != 1 || state.Principles[0].Type != nousstore.PrincipleTypeMeta || state.Principles[0].Status != nousstore.PrincipleStatusUpdated || state.Principles[0].Confidence != nousstore.ConfidenceMedium {
		t.Fatalf("live principles = %+v", state.Principles)
	}
	if state.Phase != "observing" || state.Status["phase"] != "observing" || state.Status["iterations"] != 2 || state.Status["principleCount"] != 1 {
		t.Fatalf("status did not follow: phase=%q %+v", state.Phase, state.Status)
	}

	// A half-written ledger keeps the last good copy and is not an error
	// that reaches the panel as empty data.
	writeAt(layout.LedgerPath(), `{"iterations":[`, t0.Add(time.Minute))
	if err := state.SyncKnowledge(); err == nil || len(state.Ledger) != 2 {
		t.Fatalf("corrupt ledger: err=%v len=%d", err, len(state.Ledger))
	}
	// Off-schema rows are logged, defaulted, and still served.
	writeAt(layout.LedgerPath(), `{"iterations":[{"type":"nous_kick"}]}`, t0.Add(2*time.Minute))
	if err := state.SyncKnowledge(); err != nil || len(state.Ledger) != 1 || state.Ledger[0][nousstore.KeyType] != nousstore.DefaultLedgerType {
		t.Fatalf("off-schema: err=%v ledger=%v", err, state.Ledger)
	}
}

// The dashboard mirrors nousstore's schema constants without importing the
// package (the same arrangement as the gate decisions); this pins the two
// spellings together so neither side can drift.
func TestNousSchemaConstantsAgreeAcrossPackages(t *testing.T) {
	for name, pair := range map[string][2]string{
		"key type":               {dashboard.NousKeyType, nousstore.KeyType},
		"key status":             {dashboard.NousKeyStatus, nousstore.KeyStatus},
		"key outcome":            {dashboard.NousKeyOutcome, nousstore.KeyOutcome},
		"ledger baseline":        {dashboard.NousLedgerTypeBaseline, nousstore.LedgerTypeBaseline},
		"ledger experiment":      {dashboard.NousLedgerTypeExperiment, nousstore.LedgerTypeExperiment},
		"ledger dry_run":         {dashboard.NousLedgerTypeDryRun, nousstore.LedgerTypeDryRun},
		"ledger analysis":        {dashboard.NousLedgerTypeAnalysis, nousstore.LedgerTypeAnalysis},
		"ledger shadow_analysis": {dashboard.NousLedgerTypeShadowAnalysis, nousstore.LedgerTypeShadowAnalysis},
		"status pending":         {dashboard.NousLedgerStatusPending, nousstore.LedgerStatusPending},
		"status active":          {dashboard.NousLedgerStatusActive, nousstore.LedgerStatusActive},
		"status completed":       {dashboard.NousLedgerStatusCompleted, nousstore.LedgerStatusCompleted},
		"status aborted":         {dashboard.NousLedgerStatusAborted, nousstore.LedgerStatusAborted},
		"principle domain":       {dashboard.NousPrincipleTypeDomain, nousstore.PrincipleTypeDomain},
		"principle meta":         {dashboard.NousPrincipleTypeMeta, nousstore.PrincipleTypeMeta},
		"principle active":       {dashboard.NousPrincipleStatusActive, nousstore.PrincipleStatusActive},
		"principle updated":      {dashboard.NousPrincipleStatusUpdated, nousstore.PrincipleStatusUpdated},
		"principle pruned":       {dashboard.NousPrincipleStatusPruned, nousstore.PrincipleStatusPruned},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: dashboard %q != nousstore %q", name, pair[0], pair[1])
		}
	}
	for _, v := range []string{dashboard.NousLedgerTypeBaseline, dashboard.NousLedgerTypeExperiment, dashboard.NousLedgerTypeDryRun, dashboard.NousLedgerTypeAnalysis, dashboard.NousLedgerTypeShadowAnalysis} {
		if !nousstore.ValidLedgerType(v) {
			t.Errorf("dashboard ledger type %q not valid in nousstore", v)
		}
	}
	for _, v := range []string{dashboard.NousLedgerStatusPending, dashboard.NousLedgerStatusActive, dashboard.NousLedgerStatusCompleted, dashboard.NousLedgerStatusAborted} {
		if !nousstore.ValidLedgerStatus(v) {
			t.Errorf("dashboard ledger status %q not valid in nousstore", v)
		}
	}
	if !nousstore.ValidPrincipleType(dashboard.NousPrincipleTypeDomain) || !nousstore.ValidPrincipleType(dashboard.NousPrincipleTypeMeta) {
		t.Error("dashboard principle types not valid in nousstore")
	}
	for _, v := range []string{dashboard.NousPrincipleStatusActive, dashboard.NousPrincipleStatusUpdated, dashboard.NousPrincipleStatusPruned} {
		if !nousstore.ValidPrincipleStatus(v) {
			t.Errorf("dashboard principle status %q not valid in nousstore", v)
		}
	}
}

func TestNousPrinciplesForPanelMapsFields(t *testing.T) {
	if got := nousPrinciplesForPanel(nil); got != nil {
		t.Fatalf("nil in, %v out", got)
	}
	got := nousPrinciplesForPanel([]nousstore.Principle{{ID: "p", Statement: "s", Confidence: 0.4, Category: "meta", Type: "meta", Status: "pruned"}})
	want := dashboard.NousPrinciple{ID: "p", Text: "s", Confidence: 0.4, Source: "meta", Type: "meta", Status: "pruned"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	wireNousKnowledge(nil, nousstore.New(t.TempDir()), "", restoreTestLogger())
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
	// The lab is repo-only (#12207): the default scope is "repo".
	if state.Mode != "observe" || state.Scope != "repo" {
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

type fakeGateNotifier struct{ sent []string }

func (f *fakeGateNotifier) Send(title, message string, _ notify.Priority) {
	f.sent = append(f.sent, title+": "+message)
}

// The gate protocol end to end through the real on-disk store (#11696):
// the agent publishes <root>/gates/pending.json, the hive mirrors it, pings
// once, and the owner's decision lands in decision.json + history.jsonl.
func TestWireNousGateRoundTripsThroughStore(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	fixed := time.Date(2026, 10, 13, 17, 0, 0, 0, time.UTC)
	notifier := &fakeGateNotifier{}
	state := &dashboard.NousState{Status: map[string]interface{}{}}
	wireNousGateWith(state, layout, notifier, restoreTestLogger(), func() time.Time { return fixed })
	if state.GateStore == nil || state.GateNotify == nil || state.Now == nil {
		t.Fatalf("wiring incomplete: %+v", state)
	}

	// Agent side.
	if err := layout.WriteGatePending(nousstore.GateRequest{ID: "design-1", Gate: "design", Question: "run it?", Scope: "repo"}, fixed); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := state.SyncGate(); err != nil {
			t.Fatal(err)
		}
	}
	if state.GatePending["id"] != "design-1" || state.GatePending["question"] != "run it?" {
		t.Fatalf("mirrored pending = %v", state.GatePending)
	}
	if len(notifier.sent) != 1 || !strings.Contains(notifier.sent[0], "design gate (repo scope)") {
		t.Fatalf("pings = %q", notifier.sent)
	}
	if layout.LastNotifiedGate() != "design-1" {
		t.Fatalf("marker = %q", layout.LastNotifiedGate())
	}

	// Owner side.
	d, err := state.RecordGateDecision("", dashboard.NousGateApprove, "ship", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if d.GateID != "design-1" || !d.DecidedAt.Equal(fixed) {
		t.Fatalf("decision = %+v", d)
	}
	stored, ok, err := layout.LoadGateDecision()
	if err != nil || !ok || stored.GateID != "design-1" || stored.Decision != "approve" || stored.DecidedBy != "alice" || !stored.DecidedAt.Equal(fixed) {
		t.Fatalf("stored decision = %+v ok=%v err=%v", stored, ok, err)
	}
	if _, ok, _ := layout.LoadGatePending(); ok {
		t.Fatal("pending.json must be gone after the decision")
	}
	hist, _ := layout.GateHistory()
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}

	// A "restart" over the same store does not ping again for an unanswered gate.
	if err := layout.WriteGatePending(nousstore.GateRequest{ID: "findings-1", Question: "promote?"}, fixed); err != nil {
		t.Fatal(err)
	}
	fresh := &dashboard.NousState{Status: map[string]interface{}{}}
	wireNousGateWith(fresh, layout, notifier, restoreTestLogger(), func() time.Time { return fixed })
	_ = fresh.SyncGate()
	again := &dashboard.NousState{Status: map[string]interface{}{}}
	wireNousGateWith(again, layout, notifier, restoreTestLogger(), func() time.Time { return fixed })
	_ = again.SyncGate()
	if len(notifier.sent) != 2 {
		t.Fatalf("restart re-pinged: %q", notifier.sent)
	}

	// Production wrapper: nil notifier and nil state are safe.
	wireNousGate(nil, layout, nil, restoreTestLogger())
	bare := &dashboard.NousState{}
	wireNousGate(bare, layout, nil, restoreTestLogger())
	if bare.GateStore == nil || bare.GateNotify != nil {
		t.Fatalf("nil notifier wiring = %+v", bare)
	}
	withNotifier := &dashboard.NousState{}
	wireNousGate(withNotifier, layout, notify.New(config.NotificationsConfig{}, restoreTestLogger()), restoreTestLogger())
	if withNotifier.GateNotify == nil {
		t.Fatal("real notifier must enable pings")
	}
	withNotifier.GateNotify("t", "m") // no channels configured: a no-op send
}
