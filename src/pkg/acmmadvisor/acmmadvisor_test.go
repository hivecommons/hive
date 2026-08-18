package acmmadvisor

import (
	"strings"
	"testing"
)

// readySignals returns a Signals value that satisfies EVERY criterion for a
// raise from the given current level, so tests can then knock out one signal at
// a time to exercise not-ready paths.
func readySignals(currentLevel int) Signals {
	return Signals{
		CurrentLevel:     currentLevel,
		CoveragePct:      100,
		GreenStreak:      100,
		MergeSuccessRate: 1.0,
		ActionableIssues: 0,
		HoldCount:        0,
		HasQualityAgent:  true,
	}
}

func TestRecommend_ReadyRaisesEachLevel(t *testing.T) {
	// From L1 through L5, fully-satisfied signals must advise raising exactly
	// one level with Ready and no unmet criteria.
	for from := MinLevel; from < MaxLevel; from++ {
		from := from
		t.Run("L"+itoa(from), func(t *testing.T) {
			rec := Recommend(readySignals(from))
			if rec.Advise != AdviseRaise {
				t.Fatalf("L%d: want raise, got %q (%s)", from, rec.Advise, rec.Rationale)
			}
			if rec.TargetLevel != from+1 {
				t.Fatalf("L%d: want target %d, got %d", from, from+1, rec.TargetLevel)
			}
			if !rec.Ready {
				t.Fatalf("L%d: want Ready=true", from)
			}
			if len(rec.Unmet) != 0 {
				t.Fatalf("L%d: want no unmet, got %+v", from, rec.Unmet)
			}
			if len(rec.Met) == 0 {
				t.Fatalf("L%d: want some met criteria", from)
			}
			if !strings.Contains(rec.Rationale, "Human approval required") {
				t.Fatalf("L%d: rationale must note human approval, got %q", from, rec.Rationale)
			}
		})
	}
}

func TestRecommend_L6NeverProposesHigher(t *testing.T) {
	rec := Recommend(readySignals(MaxLevel))
	if rec.Advise != AdviseStay {
		t.Fatalf("L6: want stay, got %q", rec.Advise)
	}
	if rec.TargetLevel != MaxLevel {
		t.Fatalf("L6: target must stay at %d, got %d", MaxLevel, rec.TargetLevel)
	}
	if !rec.Ready {
		t.Fatalf("L6: at max should be Ready (nothing to earn)")
	}
	if len(rec.Unmet) != 0 || len(rec.Met) != 0 {
		t.Fatalf("L6: no criteria expected, got met=%d unmet=%d", len(rec.Met), len(rec.Unmet))
	}
	if !strings.Contains(rec.Rationale, "maximum") {
		t.Fatalf("L6: rationale should mention maximum, got %q", rec.Rationale)
	}
}

func TestRecommend_NeverSkipsLevels(t *testing.T) {
	// Even with maximal signals, the proposed target is never more than one
	// level above current.
	for from := MinLevel; from <= MaxLevel; from++ {
		rec := Recommend(readySignals(from))
		if rec.TargetLevel > from+levelStep {
			t.Fatalf("L%d: proposed skipping to %d", from, rec.TargetLevel)
		}
		if rec.TargetLevel > MaxLevel {
			t.Fatalf("L%d: proposed above MaxLevel: %d", from, rec.TargetLevel)
		}
	}
}

func TestRecommend_L1GentleGate(t *testing.T) {
	// L1→L2 must NOT require coverage/streak/merge — only a quality agent.
	s := Signals{
		CurrentLevel:    1,
		CoveragePct:     0,
		GreenStreak:     0,
		HasQualityAgent: true,
	}
	rec := Recommend(s)
	if rec.Advise != AdviseRaise {
		t.Fatalf("L1 gentle gate: want raise with only quality agent, got %q (%s)", rec.Advise, rec.Rationale)
	}
	if len(rec.Met) != 1 {
		t.Fatalf("L1 gentle gate: want exactly 1 criterion, got %d", len(rec.Met))
	}
}

func TestRecommend_L1MissingQualityAgentStays(t *testing.T) {
	rec := Recommend(Signals{CurrentLevel: 1, HasQualityAgent: false})
	if rec.Advise != AdviseStay {
		t.Fatalf("want stay without quality agent, got %q", rec.Advise)
	}
	if len(rec.Unmet) != 1 || rec.Unmet[0].Name != "Quality agent present" {
		t.Fatalf("want quality-agent unmet, got %+v", rec.Unmet)
	}
	if !strings.Contains(rec.Rationale, "Quality agent present") {
		t.Fatalf("rationale must name the unmet criterion, got %q", rec.Rationale)
	}
}

func TestRecommend_L2GentlerThanL3(t *testing.T) {
	// A hive at L2 with only a quality agent (no coverage/streak) must NOT
	// advance, proving L2→L3 is stricter than L1→L2.
	rec := Recommend(Signals{CurrentLevel: 2, HasQualityAgent: true})
	if rec.Advise != AdviseStay {
		t.Fatalf("L2→L3: want stay without coverage/streak, got %q", rec.Advise)
	}
	// Coverage and streak should both be unmet; quality agent should be met.
	names := unmetNames(rec)
	if !names["Test coverage"] || !names["Green-CI streak"] {
		t.Fatalf("L2→L3: want coverage+streak unmet, got %+v", names)
	}
	if names["Quality agent present"] {
		t.Fatalf("L2→L3: quality agent should be met")
	}
}

func TestRecommend_NotReadyListsExactUnmet(t *testing.T) {
	// From L5 (target L6), knock out coverage only; everything else satisfied.
	// The unmet list must contain exactly the coverage criterion.
	s := readySignals(5)
	s.CoveragePct = coverageFloorL6 - 1 // just below the gate
	rec := Recommend(s)
	if rec.Advise != AdviseStay {
		t.Fatalf("want stay when coverage below gate, got %q", rec.Advise)
	}
	if len(rec.Unmet) != 1 {
		t.Fatalf("want exactly 1 unmet, got %d: %+v", len(rec.Unmet), rec.Unmet)
	}
	if rec.Unmet[0].Name != "Test coverage" {
		t.Fatalf("want coverage unmet, got %q", rec.Unmet[0].Name)
	}
	if !rec.Unmet[0].Met == false { // sanity
		t.Fatalf("unmet criterion should have Met=false")
	}
}

func TestRecommend_L6RequiresFullGate(t *testing.T) {
	// Exactly at the gate → met; one below → unmet.
	atGate := readySignals(5)
	atGate.CoveragePct = coverageGate
	if rec := Recommend(atGate); rec.Advise != AdviseRaise {
		t.Fatalf("coverage exactly at gate should raise, got %q", rec.Advise)
	}

	belowGate := readySignals(5)
	belowGate.CoveragePct = coverageGate - 0.1
	if rec := Recommend(belowGate); rec.Advise != AdviseStay {
		t.Fatalf("coverage just below gate should stay, got %q", rec.Advise)
	}
}

func TestRecommend_BoundaryValues(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Signals)
		from      int
		wantRaise bool
	}{
		{"streak exactly at L4 floor", func(s *Signals) { s.GreenStreak = greenStreakL4 }, 3, true},
		{"streak one below L4 floor", func(s *Signals) { s.GreenStreak = greenStreakL4 - 1 }, 3, false},
		{"merge rate exactly at L4", func(s *Signals) { s.MergeSuccessRate = mergeRateL4 }, 3, true},
		{"merge rate just below L4", func(s *Signals) { s.MergeSuccessRate = mergeRateL4 - 0.001 }, 3, false},
		{"backlog exactly at L5 ceil", func(s *Signals) { s.ActionableIssues = maxActionableL5 }, 4, true},
		{"backlog one over L5 ceil", func(s *Signals) { s.ActionableIssues = maxActionableL5 + 1 }, 4, false},
		{"holds exactly at L6 ceil", func(s *Signals) { s.HoldCount = maxHoldsL6 }, 5, true},
		{"holds one over L6 ceil", func(s *Signals) { s.HoldCount = maxHoldsL6 + 1 }, 5, false},
		{"coverage exactly at L3 floor", func(s *Signals) { s.CoveragePct = coverageFloorL3 }, 2, true},
		{"coverage just below L3 floor", func(s *Signals) { s.CoveragePct = coverageFloorL3 - 0.1 }, 2, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := readySignals(tt.from)
			tt.mutate(&s)
			rec := Recommend(s)
			gotRaise := rec.Advise == AdviseRaise
			if gotRaise != tt.wantRaise {
				t.Fatalf("%s: wantRaise=%v got advise=%q (%s)", tt.name, tt.wantRaise, rec.Advise, rec.Rationale)
			}
		})
	}
}

func TestRecommend_ZeroSignalsSafe(t *testing.T) {
	// The zero value must not panic and must advise staying at L1 (clamped).
	rec := Recommend(Signals{})
	if rec.CurrentLevel != MinLevel {
		t.Fatalf("zero signals: want current clamped to %d, got %d", MinLevel, rec.CurrentLevel)
	}
	if rec.Advise != AdviseStay {
		t.Fatalf("zero signals: want stay, got %q", rec.Advise)
	}
	if rec.Ready {
		t.Fatalf("zero signals: should not be ready")
	}
}

func TestRecommend_ClampsDegenerateLevels(t *testing.T) {
	tests := []struct {
		in   int
		want int
	}{
		{-5, MinLevel},
		{0, MinLevel},
		{7, MaxLevel},
		{999, MaxLevel},
	}
	for _, tt := range tests {
		rec := Recommend(readySignals(tt.in))
		if rec.CurrentLevel != tt.want {
			t.Fatalf("level %d: want clamped to %d, got %d", tt.in, tt.want, rec.CurrentLevel)
		}
	}
}

func TestRecommend_ClampsOutOfRangeMetrics(t *testing.T) {
	// Negative coverage / rate and >max values must be clamped and not panic.
	s := readySignals(4)
	s.CoveragePct = -50
	s.MergeSuccessRate = -1
	rec := Recommend(s)
	if rec.Advise != AdviseStay {
		t.Fatalf("negative coverage/rate should not pass, got %q", rec.Advise)
	}
	// Actual should render clamped to 0%.
	var covActual string
	for _, c := range rec.Unmet {
		if c.Name == "Test coverage" {
			covActual = c.Actual
		}
	}
	if covActual != "0%" {
		t.Fatalf("want clamped coverage Actual 0%%, got %q", covActual)
	}

	// Over-max coverage and rate clamp to the max and pass.
	s2 := readySignals(4)
	s2.CoveragePct = 500
	s2.MergeSuccessRate = 5
	if rec2 := Recommend(s2); rec2.Advise != AdviseRaise {
		t.Fatalf("over-max coverage/rate should still pass, got %q", rec2.Advise)
	}
}

func TestRecommend_Deterministic(t *testing.T) {
	s := readySignals(3)
	s.CoveragePct = 55
	a := Recommend(s)
	b := Recommend(s)
	if a.Advise != b.Advise || a.Rationale != b.Rationale || len(a.Unmet) != len(b.Unmet) {
		t.Fatalf("non-deterministic output: %+v vs %+v", a, b)
	}
}

func TestRecommend_MergeRateRenderedAsPercent(t *testing.T) {
	s := readySignals(3)
	s.MergeSuccessRate = mergeRateL4 - 0.01 // 0.69
	rec := Recommend(s)
	var actual, required string
	for _, c := range rec.Unmet {
		if c.Name == "Merge success rate" {
			actual, required = c.Actual, c.Required
		}
	}
	if !strings.HasSuffix(actual, "%") || !strings.HasSuffix(required, "%") {
		t.Fatalf("merge rate should render as percent, got actual=%q required=%q", actual, required)
	}
	if required != "70%" {
		t.Fatalf("want required 70%%, got %q", required)
	}
}

func TestRecommendFromStatus_PassThrough(t *testing.T) {
	in := StatusInputs{
		CurrentLevel:     5,
		CoveragePct:      95,
		GreenStreak:      20,
		MergeSuccessRate: 0.99,
		ActionableIssues: 2,
		HoldCount:        1,
		HasQualityAgent:  true,
	}
	rec := RecommendFromStatus(in)
	direct := Recommend(Signals(in)) // StatusInputs mirrors Signals field-for-field.
	if rec.Advise != direct.Advise || rec.TargetLevel != direct.TargetLevel {
		t.Fatalf("RecommendFromStatus mismatch: %+v vs %+v", rec, direct)
	}
	if rec.Advise != AdviseRaise {
		t.Fatalf("healthy L5 status should advise raise, got %q", rec.Advise)
	}
}

func TestCriterionFieldsRenderable(t *testing.T) {
	// Each criterion must carry a name, comparator, both values, and a detail
	// so the dashboard can render a checklist row.
	s := readySignals(4)
	s.CoveragePct = 10 // force some unmet
	rec := Recommend(s)
	all := append(append([]Criterion{}, rec.Met...), rec.Unmet...)
	if len(all) == 0 {
		t.Fatal("expected criteria")
	}
	for _, c := range all {
		if c.Name == "" || c.Required == "" || c.Actual == "" || c.Comparator == "" || c.Detail == "" {
			t.Fatalf("criterion missing renderable field: %+v", c)
		}
	}
}

func TestFormatPct(t *testing.T) {
	cases := map[float64]string{
		90:   "90%",
		62.5: "62.5%",
		0:    "0%",
		100:  "100%",
		70:   "70%",
	}
	for in, want := range cases {
		if got := formatPct(in); got != want {
			t.Fatalf("formatPct(%v)=%q want %q", in, got, want)
		}
	}
}

func TestBoolWord(t *testing.T) {
	if boolWord(true) != "yes" || boolWord(false) != "no" {
		t.Fatal("boolWord mapping wrong")
	}
}

// ── small test helpers ──────────────────────────────────────────────────────

func unmetNames(rec Recommendation) map[string]bool {
	m := map[string]bool{}
	for _, c := range rec.Unmet {
		m[c.Name] = true
	}
	return m
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
