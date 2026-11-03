package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousengine"
	"github.com/hivecommons/hive/pkg/nousstore"
)

func writeNousVerdictFile(t *testing.T, layout nousstore.Layout, rec map[string]any) {
	t.Helper()
	dir := layout.GovernorDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"deployment_recommendation": rec})
	if err := os.WriteFile(filepath.Join(dir, nousengine.MetaFindingsFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// wireNousVerdict serves the governor campaign's meta_findings.json as the
// panel verdict and the durable promotions file as its promotions; nothing
// is promoted by wiring alone.
func TestWireNousVerdictServesEngineVerdictAndPromotions(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}}}
	state := &dashboard.NousState{}

	wireNousVerdict(state, cfg, layout, func(string) string { return "" }, restoreTestLogger())
	if state.VerdictStore == nil || state.PromotionStore == nil {
		t.Fatalf("stores not wired: %+v", state)
	}
	if state.Verdict != nil || len(state.Promotions) != 0 {
		t.Fatalf("fresh root must have no verdict or promotions: %+v", state)
	}

	writeNousVerdictFile(t, layout, map[string]any{
		"recommendation": "deploy_with_caveats", "score": 0.9, "baseline": 0.8,
		"citations": []string{"ledger#3"}, "caveats": []string{"two rounds"}, "summary": "ok",
	})
	if err := os.WriteFile(filepath.Join(layout.GovernorDir(), nousReportFile), []byte("# report"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.SyncVerdict(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	v := state.Verdict
	if v == nil || v.Campaign != nousstore.GovernorSubdir || v.Recommendation != dashboard.NousVerdictWithCaveats || !v.MeasuredAgainstBaseline || v.TargetRepo != "acme/widgets" {
		t.Fatalf("verdict = %+v", v)
	}
	if v.Report != filepath.Join(layout.GovernorDir(), nousReportFile) || len(v.Citations) != 1 || len(v.Caveats) != 1 || v.Summary != "ok" {
		t.Fatalf("verdict details = %+v", v)
	}

	// Promotions round-trip through nousstore's history file.
	when := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	if err := state.PromotionStore.RecordPromotion(dashboard.NousPromotion{
		Campaign: "governor", Target: dashboard.NousPromoteTargetIssue, Recommendation: v.Recommendation,
		PromotedBy: "alice", PromotedAt: when, Repo: "acme/widgets", Number: 9, URL: "https://x/9",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := state.SyncVerdict(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(state.Promotions) != 1 || state.Promotions[0].Number != 9 || state.Promotions[0].PromotedBy != "alice" || !state.Promotions[0].PromotedAt.Equal(when) {
		t.Fatalf("promotions = %+v", state.Promotions)
	}

	// A verdict that is not measured against a baseline keeps its flag; a
	// corrupt file is an error that leaves the last good verdict in place.
	writeNousVerdictFile(t, layout, map[string]any{"recommendation": "deploy", "score": 0.5})
	if err := state.SyncVerdict(); err != nil || state.Verdict == nil || state.Verdict.MeasuredAgainstBaseline {
		t.Fatalf("unmeasured: err=%v verdict=%+v", err, state.Verdict)
	}
	if err := os.WriteFile(filepath.Join(layout.GovernorDir(), nousengine.MetaFindingsFile), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.SyncVerdict(); err == nil || state.Verdict == nil {
		t.Fatalf("corrupt: err=%v verdict=%+v", err, state.Verdict)
	}
	if err := os.Remove(filepath.Join(layout.GovernorDir(), nousengine.MetaFindingsFile)); err != nil {
		t.Fatal(err)
	}
	if err := state.SyncVerdict(); err != nil || state.Verdict != nil {
		t.Fatalf("removed: err=%v verdict=%+v", err, state.Verdict)
	}

	// Unreadable promotions surface as a sync error.
	if err := os.WriteFile(layout.PromotionHistoryPath(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.SyncVerdict(); err != nil {
		t.Fatalf("a malformed line is skipped, not fatal: %v", err)
	}
	if err := os.Remove(layout.PromotionHistoryPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.PromotionHistoryPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.SyncVerdict(); err == nil {
		t.Fatalf("directory in place of the history file must be an error")
	}
}

func TestWireNousVerdictNilStateAndDefaults(t *testing.T) {
	wireNousVerdict(nil, nil, nousstore.New(t.TempDir()), os.Getenv, nil)
	state := &dashboard.NousState{}
	wireNousVerdict(state, nil, nousstore.New(t.TempDir()), os.Getenv, nil)
	if state.VerdictStore == nil {
		t.Fatalf("nil config still wires the default engine")
	}
	if got := nousDefaultTargetRepo(&config.Config{Project: config.ProjectConfig{Org: "o", Repos: []string{"a", "b"}}}); got != "" {
		t.Fatalf("several repos must not pick one: %q", got)
	}
	if got := nousDefaultTargetRepo(nil); got != "" {
		t.Fatalf("nil cfg = %q", got)
	}
	store := nousVerdictStore{engine: mustNousEngine(t), campaign: nousengine.Campaign{ID: "c1"}}
	if _, ok, err := store.LoadVerdict(); ok || err != nil {
		t.Fatalf("missing campaign: ok=%v err=%v", ok, err)
	}
}

func mustNousEngine(t *testing.T) nousengine.Engine {
	t.Helper()
	e, err := nousengine.New("", nousengine.Settings{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The dashboard mirrors nousstore's promotion targets and nousengine's
// verdict vocabulary without importing either; pin them together.
func TestNousPromoteConstantsAgreeAcrossPackages(t *testing.T) {
	for name, pair := range map[string][2]string{
		"target issue":    {dashboard.NousPromoteTargetIssue, nousstore.PromoteTargetIssue},
		"target run":      {dashboard.NousPromoteTargetSpektacularRun, nousstore.PromoteTargetSpektacularRun},
		"verdict deploy":  {dashboard.NousVerdictDeploy, nousengine.VerdictDeploy},
		"verdict caveats": {dashboard.NousVerdictWithCaveats, nousengine.VerdictWithCaveats},
		"verdict fallbk":  {dashboard.NousVerdictFallBack, nousengine.VerdictFallBack},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: dashboard %q != %q", name, pair[0], pair[1])
		}
	}
	for _, target := range []string{dashboard.NousPromoteTargetIssue, dashboard.NousPromoteTargetSpektacularRun} {
		if !nousstore.ValidPromoteTarget(target) || !dashboard.ValidNousPromoteTarget(target) {
			t.Errorf("target %q not valid on both sides", target)
		}
	}
}
