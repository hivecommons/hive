package nousstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nousLedgerTemplate is templates/ledger.json from the pinned Nous commit:
// the row shape the orchestrator writes, with no type/status at all.
const nousLedgerTemplate = `{"iterations":[{"iteration":0,"family":"baseline","timestamp":"1970-01-01T00:00:00Z","candidate_id":"baseline","h_main_result":null,"ablation_results":{},"control_result":null,"robustness_result":null,"prediction_accuracy":null,"principles_extracted":[],"frontier_update":null}]}`

func TestValidators(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(string) bool
		ok   []string
		bad  []string
	}{
		{"ledger type", ValidLedgerType, []string{LedgerTypeBaseline, LedgerTypeExperiment, LedgerTypeDryRun, LedgerTypeAnalysis, LedgerTypeShadowAnalysis}, []string{"", "Experiment", "active", "nous_kick"}},
		{"ledger status", ValidLedgerStatus, []string{LedgerStatusPending, LedgerStatusActive, LedgerStatusCompleted, LedgerStatusAborted}, []string{"", "done", "dry_run", "CONFIRMED"}},
		{"principle type", ValidPrincipleType, []string{PrincipleTypeDomain, PrincipleTypeMeta}, []string{"", "testing", "Domain"}},
		{"principle status", ValidPrincipleStatus, []string{PrincipleStatusActive, PrincipleStatusUpdated, PrincipleStatusPruned}, []string{"", "archived", "Active"}},
	} {
		for _, v := range tc.ok {
			if !tc.fn(v) {
				t.Errorf("%s: %q should be valid", tc.name, v)
			}
		}
		for _, v := range tc.bad {
			if tc.fn(v) {
				t.Errorf("%s: %q should be invalid", tc.name, v)
			}
		}
	}
}

func TestNormalizeLedgerEntry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         map[string]interface{}
		wantType   string
		wantStatus string
		wantOut    string
		wantErr    bool
	}{
		{"nous baseline row", map[string]interface{}{"family": "baseline", "candidate_id": "baseline"}, LedgerTypeBaseline, LedgerStatusCompleted, "", false},
		{"nous baseline by candidate", map[string]interface{}{"candidate_id": "baseline", "family": "routing"}, LedgerTypeBaseline, LedgerStatusCompleted, "", false},
		{"nous iteration confirmed", map[string]interface{}{"iteration": 3.0, "family": "routing", "h_main_result": "CONFIRMED"}, LedgerTypeExperiment, LedgerStatusCompleted, "confirmed", false},
		{"nous iteration refuted", map[string]interface{}{"h_main_result": "REFUTED"}, LedgerTypeExperiment, LedgerStatusCompleted, "refuted", false},
		{"nous null result", map[string]interface{}{"h_main_result": nil}, LedgerTypeExperiment, LedgerStatusCompleted, "", false},
		{"bare legacy row", map[string]interface{}{"id": "one"}, LedgerTypeExperiment, LedgerStatusCompleted, "", false},
		{"legacy dry_run", map[string]interface{}{"type": "dry_run", "id": "d1"}, LedgerTypeDryRun, LedgerStatusCompleted, "", false},
		{"legacy state-as-type active", map[string]interface{}{"type": "active"}, LedgerTypeExperiment, LedgerStatusActive, "", false},
		{"legacy state-as-type aborted", map[string]interface{}{"type": "aborted", "outcome": "fast_fail"}, LedgerTypeExperiment, LedgerStatusAborted, "fast_fail", false},
		{"legacy analysis", map[string]interface{}{"type": "shadow_analysis"}, LedgerTypeShadowAnalysis, LedgerStatusCompleted, "", false},
		{"explicit both", map[string]interface{}{"type": "dry_run", "status": "pending"}, LedgerTypeDryRun, LedgerStatusPending, "", false},
		{"explicit outcome kept over h_main_result", map[string]interface{}{"outcome": "noisy", "h_main_result": "CONFIRMED"}, LedgerTypeExperiment, LedgerStatusCompleted, "noisy", false},
		{"unknown type", map[string]interface{}{"type": "nous_kick"}, LedgerTypeExperiment, LedgerStatusCompleted, "", true},
		{"unknown status", map[string]interface{}{"type": "experiment", "status": "done"}, LedgerTypeExperiment, LedgerStatusCompleted, "", true},
		{"status word as type with explicit status is not remapped", map[string]interface{}{"type": "active", "status": "aborted"}, LedgerTypeExperiment, LedgerStatusAborted, "", true},
		{"non-string type", map[string]interface{}{"type": 7.0}, LedgerTypeExperiment, LedgerStatusCompleted, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeLedgerEntry(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrSchema) {
				t.Fatalf("err %v does not wrap ErrSchema", err)
			}
			if got[KeyType] != tc.wantType || got[KeyStatus] != tc.wantStatus {
				t.Fatalf("type/status = %v/%v, want %s/%s", got[KeyType], got[KeyStatus], tc.wantType, tc.wantStatus)
			}
			if out, _ := got[KeyOutcome].(string); out != tc.wantOut {
				t.Fatalf("outcome = %q, want %q", out, tc.wantOut)
			}
			if !ValidLedgerType(got[KeyType].(string)) || !ValidLedgerStatus(got[KeyStatus].(string)) {
				t.Fatal("normalized entry is not valid")
			}
			// The input is never mutated; the panel may hold the raw row.
			if _, had := tc.in[KeyType]; !had {
				if _, now := tc.in[KeyType]; now {
					t.Fatal("input mutated")
				}
			}
			for k := range tc.in {
				if k == KeyType || k == KeyStatus || k == KeyOutcome {
					continue
				}
				if _, ok := got[k]; !ok {
					t.Fatalf("field %q dropped", k)
				}
			}
		})
	}
}

func TestNormalizePrinciple(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         map[string]interface{}
		wantType   string
		wantStatus string
		wantConf   float64
		wantErr    bool
	}{
		{"nous domain active", map[string]interface{}{"id": "p1", "statement": "s", "confidence": "high", "category": "domain", "status": "active"}, PrincipleTypeDomain, PrincipleStatusActive, ConfidenceHigh, false},
		{"nous meta pruned", map[string]interface{}{"id": "p2", "confidence": "low", "category": "meta", "status": "pruned"}, PrincipleTypeMeta, PrincipleStatusPruned, ConfidenceLow, false},
		{"legacy free category", map[string]interface{}{"id": "p3", "confidence": "medium", "category": "testing"}, PrincipleTypeDomain, PrincipleStatusActive, ConfidenceMedium, false},
		{"no fields at all", map[string]interface{}{"id": "p4"}, PrincipleTypeDomain, PrincipleStatusActive, ConfidenceUnknown, false},
		{"numeric confidence", map[string]interface{}{"id": "p5", "confidence": 0.85}, PrincipleTypeDomain, PrincipleStatusActive, 0.85, false},
		{"out of range confidence", map[string]interface{}{"id": "p6", "confidence": 42.0}, PrincipleTypeDomain, PrincipleStatusActive, ConfidenceUnknown, false},
		{"explicit type wins over category", map[string]interface{}{"id": "p7", "type": "meta", "category": "domain"}, PrincipleTypeMeta, PrincipleStatusActive, ConfidenceUnknown, false},
		{"unknown type", map[string]interface{}{"id": "p8", "type": "folk"}, PrincipleTypeDomain, PrincipleStatusActive, ConfidenceUnknown, true},
		{"unknown status", map[string]interface{}{"id": "p9", "status": "archived"}, PrincipleTypeDomain, PrincipleStatusActive, ConfidenceUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePrinciple(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrSchema) {
				t.Fatalf("err %v does not wrap ErrSchema", err)
			}
			if got.Type != tc.wantType || got.Status != tc.wantStatus || got.Confidence != tc.wantConf {
				t.Fatalf("got %+v, want %s/%s/%v", got, tc.wantType, tc.wantStatus, tc.wantConf)
			}
			if got.ID != tc.in["id"] {
				t.Fatalf("id = %q", got.ID)
			}
			if cat, _ := tc.in["category"].(string); got.Category != cat {
				t.Fatalf("category = %q, want raw %q", got.Category, cat)
			}
		})
	}
}

func TestConfidenceScore(t *testing.T) {
	for in, want := range map[interface{}]float64{
		"high": ConfidenceHigh, " High ": ConfidenceHigh, "medium": ConfidenceMedium, "low": ConfidenceLow,
		"": ConfidenceUnknown, "certain": ConfidenceUnknown, nil: ConfidenceUnknown,
		0.0: 0, 1.0: 1, 0.33: 0.33, -0.1: ConfidenceUnknown, 1.5: ConfidenceUnknown, true: ConfidenceUnknown,
	} {
		if got := ConfidenceScore(in); got != want {
			t.Errorf("ConfidenceScore(%v) = %v, want %v", in, got, want)
		}
	}
}

// Round trip: what the reader serves is itself a valid document for the
// reader. An entry normalized once, encoded, and read back through
// ParseLedger/ParsePrinciples comes out identical and without schema
// errors — so a writer that emits the normalized shape (type/status set)
// and a reader of Nous's native shape agree on one schema.
func TestKnowledgeRoundTrip(t *testing.T) {
	entries, schema, err := ParseLedger([]byte(nousLedgerTemplate))
	if err != nil || len(schema) != 0 || len(entries) != 1 {
		t.Fatalf("template ledger: entries=%d schema=%v err=%v", len(entries), schema, err)
	}
	if entries[0][KeyType] != LedgerTypeBaseline || entries[0][KeyStatus] != LedgerStatusCompleted {
		t.Fatalf("template row normalized to %v/%v", entries[0][KeyType], entries[0][KeyStatus])
	}
	entries = append(entries, mustNormalize(t, map[string]interface{}{"iteration": 1.0, "family": "routing", "h_main_result": "PARTIALLY_CONFIRMED"}))
	entries = append(entries, mustNormalize(t, map[string]interface{}{"type": "dry_run", "status": "pending", "id": "d-1"}))

	data, err := json.Marshal(map[string]interface{}{"iterations": entries})
	if err != nil {
		t.Fatal(err)
	}
	again, schema, err := ParseLedger(data)
	if err != nil || len(schema) != 0 {
		t.Fatalf("re-read: schema=%v err=%v", schema, err)
	}
	if len(again) != len(entries) {
		t.Fatalf("re-read %d entries, want %d", len(again), len(entries))
	}
	for i := range entries {
		if again[i][KeyType] != entries[i][KeyType] || again[i][KeyStatus] != entries[i][KeyStatus] || again[i][KeyOutcome] != entries[i][KeyOutcome] {
			t.Fatalf("entry %d drifted: %v -> %v", i, entries[i], again[i])
		}
	}
	if again[1][KeyOutcome] != "partially_confirmed" {
		t.Fatalf("outcome = %v", again[1][KeyOutcome])
	}

	principles, schema, err := ParsePrinciples([]byte(`{"principles":[{"id":"p1","statement":"Prefer seams","confidence":"high","regime":"all","evidence":[],"contradicts":[],"extraction_iteration":1,"mechanism":"m","applicability_bounds":"b","superseded_by":null,"category":"meta","status":"updated"}]}`))
	if err != nil || len(schema) != 0 || len(principles) != 1 {
		t.Fatalf("principles: %d schema=%v err=%v", len(principles), schema, err)
	}
	pdata, err := json.Marshal(map[string]interface{}{"principles": principles})
	if err != nil {
		t.Fatal(err)
	}
	pagain, schema, err := ParsePrinciples(pdata)
	if err != nil || len(schema) != 0 || len(pagain) != 1 {
		t.Fatalf("principles re-read: %d schema=%v err=%v", len(pagain), schema, err)
	}
	if pagain[0] != principles[0] {
		t.Fatalf("principle drifted: %+v -> %+v", principles[0], pagain[0])
	}
	if pagain[0].Type != PrincipleTypeMeta || pagain[0].Status != PrincipleStatusUpdated || pagain[0].Confidence != ConfidenceHigh {
		t.Fatalf("principle = %+v", pagain[0])
	}
}

func mustNormalize(t *testing.T, row map[string]interface{}) LedgerEntry {
	t.Helper()
	e, err := NormalizeLedgerEntry(row)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestParseReportsSchemaProblemsButKeepsRows(t *testing.T) {
	entries, schema, err := ParseLedger([]byte(`{"iterations":[{"type":"nous_kick"},{"type":"dry_run"},{"status":"done"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || len(schema) != 2 {
		t.Fatalf("entries=%d schema=%d, want 3/2", len(entries), len(schema))
	}
	for _, e := range entries {
		if !ValidLedgerType(e[KeyType].(string)) || !ValidLedgerStatus(e[KeyStatus].(string)) {
			t.Fatalf("entry %v not coerced to a valid schema", e)
		}
	}
	if _, _, err := ParseLedger([]byte(`{nope`)); err == nil {
		t.Fatal("malformed ledger must error")
	}
	principles, schema, err := ParsePrinciples([]byte(`{"principles":[{"id":"a","type":"folk"},{"id":"b"}]}`))
	if err != nil || len(principles) != 2 || len(schema) != 1 {
		t.Fatalf("principles=%d schema=%d err=%v", len(principles), len(schema), err)
	}
	if _, _, err := ParsePrinciples([]byte(`[]`)); err == nil {
		t.Fatal("malformed principles must error")
	}
}

// touch writes data to path and gives it a distinct mtime so a change
// detector driven by stat cannot miss it, whatever the filesystem's
// timestamp granularity. No sleeping.
func touch(t *testing.T, path string, data string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeReaderLiveReload(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	r := l.KnowledgeReader("")
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	// Nothing on disk: empty, and the first Load always reports a change.
	k, changed, err := r.Load()
	if err != nil || !changed || len(k.Ledger) != 0 || len(k.Principles) != 0 || k.Dir != l.GovernorDir() {
		t.Fatalf("empty: %+v changed=%v err=%v", k, changed, err)
	}
	if _, changed, _ = r.Load(); changed {
		t.Fatal("second Load of nothing reported a change")
	}

	// The agent writes the first round.
	touch(t, l.LedgerPath(), nousLedgerTemplate, t0)
	k, changed, err = r.Load()
	if err != nil || !changed || len(k.Ledger) != 1 {
		t.Fatalf("after ledger: %d changed=%v err=%v", len(k.Ledger), changed, err)
	}
	if _, changed, _ = r.Load(); changed {
		t.Fatal("unchanged ledger reported a change")
	}

	// Another round lands with a different mtime (same size would still be
	// caught by the mtime; a different size by the size).
	touch(t, l.LedgerPath(), `{"iterations":[{"family":"baseline"},{"iteration":1,"family":"r","h_main_result":"REFUTED"}]}`, t0.Add(time.Minute))
	k, changed, err = r.Load()
	if err != nil || !changed || len(k.Ledger) != 2 || k.Ledger[1][KeyOutcome] != "refuted" {
		t.Fatalf("after round 2: %+v changed=%v err=%v", k.Ledger, changed, err)
	}

	// Principles appear independently.
	touch(t, l.PrinciplesPath(), `{"principles":[{"id":"p1","statement":"s","confidence":"high","category":"meta","status":"active"}]}`, t0.Add(2*time.Minute))
	k, changed, err = r.Load()
	if err != nil || !changed || len(k.Principles) != 1 || k.Principles[0].Type != PrincipleTypeMeta || len(k.Ledger) != 2 {
		t.Fatalf("after principles: %+v changed=%v err=%v", k, changed, err)
	}
	if _, changed, _ = r.Load(); changed {
		t.Fatal("steady state reported a change")
	}

	// A half-written ledger is an error; the last good content is kept and
	// the principles are untouched.
	touch(t, l.LedgerPath(), `{"iterations":[`, t0.Add(3*time.Minute))
	k, changed, err = r.Load()
	if err == nil || changed || len(k.Ledger) != 2 || len(k.Principles) != 1 {
		t.Fatalf("corrupt ledger: %d/%d changed=%v err=%v", len(k.Ledger), len(k.Principles), changed, err)
	}
	// Once repaired it is picked up.
	touch(t, l.LedgerPath(), `{"iterations":[{"type":"dry_run"},{"type":"active"},{"type":"completed"}]}`, t0.Add(4*time.Minute))
	k, changed, err = r.Load()
	if err != nil || !changed || len(k.Ledger) != 3 || k.Ledger[1][KeyStatus] != LedgerStatusActive || k.Ledger[1][KeyType] != LedgerTypeExperiment {
		t.Fatalf("repaired: %+v changed=%v err=%v", k.Ledger, changed, err)
	}

	// Schema problems ride along without blocking the load, and are not
	// duplicated when the other file reloads.
	touch(t, l.LedgerPath(), `{"iterations":[{"type":"nous_kick"}]}`, t0.Add(5*time.Minute))
	k, _, err = r.Load()
	if err != nil || len(k.Schema) != 1 {
		t.Fatalf("schema: %v err=%v", k.Schema, err)
	}
	touch(t, l.PrinciplesPath(), `{"principles":[{"id":"p1","status":"archived"}]}`, t0.Add(6*time.Minute))
	k, _, err = r.Load()
	if err != nil || len(k.Schema) != 2 {
		t.Fatalf("schema after both: %v err=%v", k.Schema, err)
	}
	k, _, _ = r.Load()
	if len(k.Schema) != 2 {
		t.Fatalf("schema duplicated on steady state: %v", k.Schema)
	}

	// Deleting a file empties it.
	if err := os.Remove(l.PrinciplesPath()); err != nil {
		t.Fatal(err)
	}
	k, changed, err = r.Load()
	if err != nil || !changed || len(k.Principles) != 0 || len(k.Schema) != 1 {
		t.Fatalf("after delete: %+v changed=%v err=%v", k, changed, err)
	}

	// Returned slices are copies: mutating them does not touch the cache.
	k.Ledger[0][KeyType] = "mutated"
	k, _, _ = r.Load()
	if k.Ledger[0][KeyType] == "mutated" {
		t.Fatal("caller mutation leaked into the cache")
	}
}

func TestKnowledgeReaderFollowsLegacyFallback(t *testing.T) {
	base := t.TempDir()
	l := New(filepath.Join(base, "nous"))
	legacy := filepath.Join(base, "legacy")
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	touch(t, filepath.Join(legacy, PrinciplesFile), `{"principles":[{"id":"old"}]}`, t0)

	r := l.KnowledgeReader(legacy)
	k, changed, err := r.Load()
	if err != nil || !changed || k.Dir != legacy || len(k.Principles) != 1 || k.Principles[0].ID != "old" {
		t.Fatalf("legacy: %+v changed=%v err=%v", k, changed, err)
	}

	// The durable dir gaining a file switches the reader over, even though
	// the legacy file itself did not change.
	touch(t, l.LedgerPath(), nousLedgerTemplate, t0.Add(time.Minute))
	k, changed, err = r.Load()
	if err != nil || !changed || k.Dir != l.GovernorDir() || len(k.Ledger) != 1 || len(k.Principles) != 0 {
		t.Fatalf("switched: %+v changed=%v err=%v", k, changed, err)
	}

	// With no legacy dir the fallback is off.
	none := New(filepath.Join(base, "none")).KnowledgeReader("")
	if k, _, _ := none.Load(); k.Dir != filepath.Join(base, "none", GovernorSubdir) {
		t.Fatalf("dir = %q", k.Dir)
	}
}

func TestKnowledgeReaderUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	l := New(filepath.Join(t.TempDir(), "nous"))
	t0 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	touch(t, l.LedgerPath(), nousLedgerTemplate, t0)
	touch(t, l.PrinciplesPath(), `{"principles":[]}`, t0)
	if err := os.Chmod(l.PrinciplesPath(), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(l.PrinciplesPath(), 0o644) })
	k, _, err := l.KnowledgeReader("").Load()
	if err == nil || len(k.Ledger) != 1 {
		t.Fatalf("unreadable principles: ledger=%d err=%v", len(k.Ledger), err)
	}
}

// A directory where a knowledge file should be is a read error (not "no
// file"), reported for both files and surfaced through the reader.
func TestKnowledgeReaderDirectoryInPlaceOfFile(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "nous"))
	for _, path := range []string{l.LedgerPath(), l.PrinciplesPath()} {
		if err := os.MkdirAll(path, dirMode); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := l.KnowledgeReader("").Load()
	if err == nil {
		t.Fatal("Load over directories must fail")
	}
	for _, want := range []string{"read ledger", "read principles"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}
