package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// TestIsRegressionReservationStages pins that leaving 020 (a claim or start
// mark, nothing built), 030 → 020 (the claim still held, its PR evidence
// gone quiet for a tick) and 040 → 030 (the agent back on the covering PR)
// are not regressions, while genuine falls still are. The scanner's freshly
// filed split-outs wore a red "regressed" badge minutes after being filed
// because a lapsed claim counted as a fall; hive#14525 wore it for a
// 030 ↔ 020 flap every claim renewal.
func TestIsRegressionReservationStages(t *testing.T) {
	cases := []struct {
		name     string
		from, to Stage
		want     bool
	}{
		{"claim released to the pool", Claimed, Actionable, false},
		{"claim released to triage", Claimed, TriageNeeded, false},
		{"claim parked by prior-art gate", Claimed, PossiblyExisting, false},
		{"covered PR being worked again", CoveredByPR, PRInProgress, false},
		{"covered PR closed unmerged", CoveredByPR, Actionable, true},
		{"covered PR to bare claim", CoveredByPR, Claimed, true},
		{"PR under way abandoned", PRInProgress, Actionable, true},
		{"PR under way abandoned on an untriaged issue", PRInProgress, TriageNeeded, true},
		{"PR evidence flickers, claim still held", PRInProgress, Claimed, false},
		{"review knocked back to claim", ReviewPending, Claimed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRegression(tc.from, tc.to); got != tc.want {
				t.Fatalf("IsRegression(%v, %v) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// TestStoreObserveProgressFall pins ObserveProgress: a fall flagged as
// progress (the covering PR merged and addressed part of the issue) books
// no regression and resolves one already open; the same fall without the
// flag is a regression as before.
func TestStoreObserveProgressFall(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	newStore := func(t *testing.T) *Store {
		t.Helper()
		return NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	}
	cases := []struct {
		name          string
		before        []Stage
		progress      bool
		wantRegressed bool
		wantRecovered bool
		wantFrom      Stage
		wantHopFlag   bool
	}{
		{"covered PR merged partially", []Stage{Actionable, Claimed, CoveredByPR}, true, false, false, None, false},
		{"covered PR closed unmerged", []Stage{Actionable, Claimed, CoveredByPR}, false, true, false, CoveredByPR, true},
		{"progress heals an open regression", []Stage{Actionable, CoveredByPR, Claimed}, true, false, true, None, false},
		{"first observation is never a fall", nil, false, false, false, None, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			key := Key("o/r", 14557)
			for _, s := range tc.before {
				st.Observe(key, s, "setup")
			}
			if tc.wantRecovered {
				// Setup walks 040 → 020 (a real fall) so the record is
				// regressed before the progress observation arrives.
				if rec, _ := st.Get(key); !rec.Regressed() {
					t.Fatalf("setup should leave the record regressed: %+v", rec)
				}
			}
			tr := st.ObserveProgress(key, Actionable, "actionable", tc.progress)
			rec, _ := st.Get(key)
			if tr.Regressed != tc.wantRegressed || tr.Recovered != tc.wantRecovered || rec.RegressedFrom != tc.wantFrom {
				t.Fatalf("tr=%+v rec.RegressedFrom=%v; want regressed=%v recovered=%v from=%v", tr, rec.RegressedFrom, tc.wantRegressed, tc.wantRecovered, tc.wantFrom)
			}
			if len(rec.History) == 0 {
				t.Fatal("no hop recorded")
			}
			if last := rec.History[len(rec.History)-1]; last.Regressed != tc.wantHopFlag {
				t.Fatalf("last hop %+v; want regressed=%v", last, tc.wantHopFlag)
			}
		})
	}
}

// TestStoreObserveClaimLapseNoRegression replays the scanner split-out
// shape — filed, claimed by the scanner, claim lapsed three minutes later —
// and asserts the record never regresses, and that a record persisted
// before the rule changed (regressed from 020) heals on its next sweep.
func TestStoreObserveClaimLapseNoRegression(t *testing.T) {
	now := time.Date(2026, 10, 27, 11, 38, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("hivecommons/hive", 14558)
	st.Observe(key, Claimed, "live issue claim")
	now = now.Add(3 * time.Minute)
	tr := st.Observe(key, Actionable, "actionable")
	rec, _ := st.Get(key)
	if tr.Regressed || rec.Regressed() || rec.History[len(rec.History)-1].Regressed {
		t.Fatalf("lapsed claim booked as a regression: tr=%+v rec=%+v", tr, rec)
	}

	// A record written by the old rule heals on the next observation.
	stale := Key("hivecommons/hive", 14554)
	st.mu.Lock()
	st.items[stale] = Record{Stage: Actionable, RegressedFrom: Claimed, RegressedAt: now, History: []Hop{{Stage: Claimed, At: now}, {From: Claimed, Stage: Actionable, At: now, Regressed: true}}}
	st.mu.Unlock()
	tr = st.Observe(stale, Actionable, "actionable")
	rec, _ = st.Get(stale)
	if !tr.Recovered || rec.Regressed() {
		t.Fatalf("stale 020 regression did not heal: tr=%+v rec=%+v", tr, rec)
	}

	// hive#14525: 030 ↔ 020 every claim renewal, the holder unchanged.
	flap := Key("hivecommons/hive", 14525)
	st.Observe(flap, Claimed, "live agent claim (no PR yet)")
	for i := 0; i < 3; i++ {
		now = now.Add(15 * time.Minute)
		if tr = st.Observe(flap, PRInProgress, "live claim with a PR under way"); tr.Regressed {
			t.Fatalf("flap %d up: %+v", i, tr)
		}
		now = now.Add(15 * time.Minute)
		if tr = st.Observe(flap, Claimed, "live agent claim (no PR yet)"); tr.Regressed {
			t.Fatalf("flap %d down booked as a regression: %+v", i, tr)
		}
	}
	if rec, _ = st.Get(flap); rec.Regressed() {
		t.Fatalf("flapping claim reads as regressed: %+v", rec)
	}
}

// TestStoreRegressionClearsOnForwardHop pins the lifecycle the dashboard
// badge follows: set by a genuine fall, kept across a non-regression
// backward hop, cleared by the first forward hop (not only by climbing all
// the way back to the stage it fell from).
func TestStoreRegressionClearsOnForwardHop(t *testing.T) {
	cases := []struct {
		name  string
		walk  []Stage
		after []bool // Regressed() after each hop of walk
	}{
		{"issue: fall, slide, first step up clears", []Stage{Actionable, CoveredByPR, Claimed, Actionable, Claimed}, []bool{false, false, true, true, false}},
		{"pr: fall, partial climb clears", []Stage{ReviewPending, MergeQueue, ReviewPending, Reviewed, MergeQueue}, []bool{false, false, true, false, false}},
		{"pr: fall, repair route clears", []Stage{Reviewed, AwaitingMergeAdmission, ReviewPending, Fixing}, []bool{false, false, true, false}},
		{"pr: fall then lateral re-attribution keeps it", []Stage{AwaitingMergeAdmission, ReturnedToContributor, Fixing}, []bool{false, true, true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
			key := Key("o/r", 1)
			for i, s := range tc.walk {
				now = now.Add(time.Minute)
				tr := st.Observe(key, s, "walk")
				rec, _ := st.Get(key)
				if rec.Regressed() != tc.after[i] {
					t.Fatalf("hop %d → %v: Regressed()=%v, want %v (tr=%+v)", i, s, rec.Regressed(), tc.after[i], tr)
				}
				if i > 0 && tc.after[i-1] && !tc.after[i] && !tr.Recovered {
					t.Fatalf("hop %d → %v cleared the regression without reporting Recovered: %+v", i, s, tr)
				}
			}
		})
	}
}
