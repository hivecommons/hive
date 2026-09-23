package persona

import (
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

var learningClock = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func enabledLearning() LearningConfig {
	return LearningConfig{Enabled: true}
}

func recordSignals(t *testing.T, r Record, signal string, n int, at time.Time, cfg LearningConfig) Record {
	t.Helper()
	for i := 0; i < n; i++ {
		var err error
		r, err = r.RecordSignal(signal, at, cfg)
		if err != nil {
			t.Fatalf("RecordSignal(%s #%d): %v", signal, i+1, err)
		}
	}
	return r
}

func TestLearningSignalsAccumulateAndSuggestOnlyPastThreshold(t *testing.T) {
	cfg := enabledLearning()
	r := Record{Depth: DepthOutcomes, SummaryLength: SummaryStandard}

	r = recordSignals(t, r, SignalExpanded, DefaultLearningThreshold-1, learningClock, cfg)
	if r.Learning == nil || r.Learning.Signals.Expanded != DefaultLearningThreshold-1 {
		t.Fatalf("expanded counter = %#v, want %d", r.Learning, DefaultLearningThreshold-1)
	}
	if len(r.Suggestions()) != 0 {
		t.Fatalf("suggestion appeared below threshold: %#v", r.Suggestions())
	}
	if r.Depth != DepthOutcomes {
		t.Fatalf("depth mutated by signals: %q", r.Depth)
	}

	r = recordSignals(t, r, SignalExpanded, 1, learningClock, cfg)
	got := r.Suggestions()
	if len(got) != 1 || got[0].Key != SuggestionKeyDepth || got[0].From != DepthOutcomes || got[0].To != DepthTechnical {
		t.Fatalf("suggestion = %#v, want depth outcomes->technical", got)
	}
	if want := "5 expansions in 7 days"; got[0].Evidence != want {
		t.Fatalf("evidence = %q, want %q", got[0].Evidence, want)
	}
	if r.Depth != DepthOutcomes {
		t.Fatalf("suggestion silently applied: depth = %q", r.Depth)
	}
	if r.Learning.Signals.Total() != 0 {
		t.Fatalf("counters not spent after suggestion: %#v", r.Learning.Signals)
	}
}

func TestLearningNeverStacksPastTheTop(t *testing.T) {
	cfg := enabledLearning()
	r := Record{Depth: DepthTechnical, SummaryLength: SummaryDetailed}
	r = recordSignals(t, r, SignalExpanded, DefaultLearningThreshold, learningClock, cfg)
	if len(r.Suggestions()) != 0 {
		t.Fatalf("record at the top of the ladder got a suggestion: %#v", r.Suggestions())
	}
	nextWeek := learningClock.Add(cfg.EffectiveWindow())
	r = recordSignals(t, r, SignalExpanded, DefaultLearningThreshold, nextWeek, cfg)
	if len(r.Suggestions()) != 0 || r.Depth != DepthTechnical || r.SummaryLength != SummaryDetailed {
		t.Fatalf("second week of expansions moved a top record: %#v", r)
	}
}

func TestLearningOnePendingSuggestionPerKeyAndWindowRollover(t *testing.T) {
	cfg := LearningConfig{Enabled: true, Threshold: 2, WindowDays: 1}
	r := Record{Depth: DepthOutcomes, SummaryLength: SummaryStandard}
	r = recordSignals(t, r, SignalReAsked, 2, learningClock, cfg)
	if got := r.Suggestions(); len(got) != 1 || got[0].Evidence != "2 re-asks in 1 days" {
		t.Fatalf("re-ask suggestion = %#v", got)
	}
	r = recordSignals(t, r, SignalExpanded, 2, learningClock, cfg)
	if got := r.Suggestions(); len(got) != 1 {
		t.Fatalf("duplicate suggestion for the same key: %#v", got)
	}

	r = recordSignals(t, r, SignalSkipped, 1, learningClock, cfg)
	if r.Learning.Signals.Skipped != 1 {
		t.Fatalf("skipped counter = %d", r.Learning.Signals.Skipped)
	}
	r = recordSignals(t, r, SignalSkipped, 1, learningClock.Add(cfg.EffectiveWindow()), cfg)
	if r.Learning.Signals.Skipped != 1 || !r.Learning.Signals.WindowStart.Equal(learningClock.Add(cfg.EffectiveWindow())) {
		t.Fatalf("window did not roll over: %#v", r.Learning.Signals)
	}
}

func TestLearningSkipsSuggestLessDetailAndAcceptRejectUndoPin(t *testing.T) {
	cfg := enabledLearning()
	r := Record{Depth: DepthTechnical, SummaryLength: SummaryStandard}
	r = recordSignals(t, r, SignalSkipped, DefaultLearningThreshold, learningClock, cfg)
	got := r.Suggestions()
	if len(got) != 1 || got[0].Key != SuggestionKeyDepth || got[0].To != DepthOutcomes || got[0].Evidence != "5 skips in 7 days" {
		t.Fatalf("skip suggestion = %#v", got)
	}

	if _, _, err := r.AcceptSuggestion(2, learningClock); err == nil {
		t.Fatal("accepting a missing suggestion should fail")
	}
	accepted, adj, err := r.AcceptSuggestion(1, learningClock)
	if err != nil {
		t.Fatalf("AcceptSuggestion: %v", err)
	}
	if accepted.Depth != DepthOutcomes || len(accepted.Suggestions()) != 0 {
		t.Fatalf("accepted record = %#v", accepted)
	}
	if adj.Key != SuggestionKeyDepth || adj.From != DepthTechnical || adj.To != DepthOutcomes || !adj.AppliedAt.Equal(learningClock) {
		t.Fatalf("adjustment = %#v", adj)
	}
	if accepted.Learning.LastAdjustment == nil || *accepted.Learning.LastAdjustment != adj {
		t.Fatalf("last adjustment not recorded: %#v", accepted.Learning)
	}
	if r.Depth != DepthTechnical {
		t.Fatal("AcceptSuggestion mutated its receiver")
	}

	undone, last, err := accepted.UndoAdjustment(learningClock)
	if err != nil {
		t.Fatalf("UndoAdjustment: %v", err)
	}
	if undone.Depth != DepthTechnical || !undone.Pinned || undone.Learning.LastAdjustment != nil || last != adj {
		t.Fatalf("undone record = %#v, last = %#v", undone, last)
	}
	if _, _, err := undone.UndoAdjustment(learningClock); err == nil {
		t.Fatal("second undo should fail")
	}
	pinned := recordSignals(t, undone, SignalSkipped, DefaultLearningThreshold, learningClock, cfg)
	if pinned.Learning.Signals.Total() != 0 || len(pinned.Suggestions()) != 0 {
		t.Fatalf("pinned record accumulated signals: %#v", pinned.Learning)
	}
	unpinned := pinned.SetPinned(false, learningClock)
	unpinned = recordSignals(t, unpinned, SignalSkipped, DefaultLearningThreshold, learningClock, cfg)
	if len(unpinned.Suggestions()) != 1 {
		t.Fatalf("unpinned record did not resume learning: %#v", unpinned.Learning)
	}

	rejected := unpinned.RejectSuggestions(learningClock)
	if len(rejected.Suggestions()) != 0 || rejected.Depth != DepthTechnical || rejected.Learning.Signals.Total() != 0 {
		t.Fatalf("rejected record = %#v", rejected)
	}
	if (Record{}).RejectSuggestions(learningClock).Learning != nil {
		t.Fatal("reject on an empty record should stay empty")
	}
	if !(Record{}).Empty() || (Record{Pinned: true}).Empty() || rejected.Empty() {
		t.Fatal("Empty must account for pinned and learning state")
	}
}

func TestLearningDisabledRecordsNothing(t *testing.T) {
	r := Record{Depth: DepthOutcomes}
	r = recordSignals(t, r, SignalExpanded, DefaultLearningThreshold*2, learningClock, LearningConfig{})
	if r.Learning != nil || len(r.Suggestions()) != 0 {
		t.Fatalf("learning off still accumulated: %#v", r.Learning)
	}
	if _, err := r.RecordSignal("unknown", learningClock, enabledLearning()); err == nil {
		t.Fatal("unknown signal should fail")
	}
	if got := (LearningConfig{}).EffectiveThreshold(); got != DefaultLearningThreshold {
		t.Fatalf("default threshold = %d", got)
	}
	if got := (LearningConfig{WindowDays: 3}).EffectiveWindow(); got != 3*hoursPerDay*time.Hour {
		t.Fatalf("window = %s", got)
	}
}

func TestLearningSummaryLengthLadder(t *testing.T) {
	cfg := LearningConfig{Enabled: true, Threshold: 1}
	up := Record{Depth: DepthTechnical, SummaryLength: SummaryShort}
	up = recordSignals(t, up, SignalExpanded, 1, learningClock, cfg)
	if got := up.Suggestions(); len(got) != 1 || got[0].Key != SuggestionKeySummaryLength || got[0].To != SummaryStandard {
		t.Fatalf("short->standard suggestion = %#v", got)
	}
	up, _, _ = up.AcceptSuggestion(1, learningClock)
	up = recordSignals(t, up, SignalExpanded, 1, learningClock, cfg)
	if got := up.Suggestions(); len(got) != 1 || got[0].To != SummaryDetailed {
		t.Fatalf("standard->detailed suggestion = %#v", got)
	}

	down := Record{Depth: DepthOutcomes, SummaryLength: SummaryDetailed}
	down = recordSignals(t, down, SignalSkipped, 1, learningClock, cfg)
	if got := down.Suggestions(); len(got) != 1 || got[0].To != SummaryStandard {
		t.Fatalf("detailed->standard suggestion = %#v", got)
	}
	down, _, _ = down.AcceptSuggestion(1, learningClock)
	down = recordSignals(t, down, SignalSkipped, 1, learningClock, cfg)
	if got := down.Suggestions(); len(got) != 1 || got[0].To != SummaryShort {
		t.Fatalf("standard->short suggestion = %#v", got)
	}
	down, _, _ = down.AcceptSuggestion(1, learningClock)
	down = recordSignals(t, down, SignalSkipped, 1, learningClock, cfg)
	if len(down.Suggestions()) != 0 {
		t.Fatalf("record at the bottom got a suggestion: %#v", down.Suggestions())
	}
}

func TestLearningStateNormalizeClonesInsteadOfAliasing(t *testing.T) {
	r := Record{Learning: &Learning{Suggestions: []Suggestion{{Key: SuggestionKeyDepth}}, LastAdjustment: &Adjustment{Key: SuggestionKeyDepth}}}
	n := r.Normalize()
	if n.Learning == r.Learning || n.Learning.LastAdjustment == r.Learning.LastAdjustment {
		t.Fatal("Normalize must clone learning state")
	}
	if !reflect.DeepEqual(n.Learning, r.Learning) {
		t.Fatalf("clone differs: %#v vs %#v", n.Learning, r.Learning)
	}
}

// TestLearningSchemaSeparateFromAutonomyFields extends the #8315 disjointness
// guard to the learning state: no persona, signal, suggestion, or adjustment
// key may share a name with ACMM or agent-mode configuration.
func TestLearningSchemaSeparateFromAutonomyFields(t *testing.T) {
	autonomyFields := jsonFields(reflect.TypeOf(config.Config{}))
	for field := range jsonFields(reflect.TypeOf(config.AgentConfig{})) {
		autonomyFields[field] = struct{}{}
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Record{}),
		reflect.TypeOf(Learning{}),
		reflect.TypeOf(Signals{}),
		reflect.TypeOf(Suggestion{}),
		reflect.TypeOf(Adjustment{}),
	} {
		for field := range jsonFields(typ) {
			if _, ok := autonomyFields[field]; ok {
				t.Fatalf("%s field %q overlaps ACMM or agent-mode configuration", typ.Name(), field)
			}
		}
	}
}

// TestLearningImportBoundary is the conformance check from #8363 step 4: the
// adjustment code path is stdlib-only, so it cannot reach ACMM, agent mode,
// or policy packages even by accident.
func TestLearningImportBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			if strings.Contains(imported, ".") {
				t.Errorf("persona import boundary: %s imports %q; the persona adjustment path must stay stdlib-only so it can never touch ACMM, agent mode, or policy (hivecommons/hive#8363)", name, imported)
			}
		}
	}
}
