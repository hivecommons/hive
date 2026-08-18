package reach

// Tests for the D4 error-rate delta (#3995, phase 2c of #3973). The
// load-bearing assertions are PER-COMPONENT NUMERIC delta values (not just
// shape counts), the divide-by-zero guard (a zero-span side must read as
// unmeasured, never as NaN or a fabricated 0.0 error rate), and the dominant
// production case on this fleet: auto-upgrade converges in ~a minute, so a
// commit whose pre-deploy history was never retained must come back
// measured=false — before-side absence is normal, not an edge.

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// histBucket returns the n-th hourly bucket of a fixed test day.
func histBucket(n int) time.Time {
	return time.Date(2026, 8, 17, n, 0, 0, 0, time.UTC)
}

func TestComputeErrorDelta(t *testing.T) {
	const oldCommit = "01dc0de"
	const newCommit = "beef456"

	cases := []struct {
		name       string
		samples    []HistorySample
		deployed   string
		wantBefore float64
		wantAfter  float64
		wantDelta  float64
		wantWin    int
		measured   bool
	}{
		{
			name: "measured both sides, exact per-component rates",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 100, SpansError: 10},
				{WindowStart: histBucket(1), Commit: oldCommit, SpansTotal: 100, SpansError: 10},
				{WindowStart: histBucket(2), Commit: newCommit, SpansTotal: 100, SpansError: 20},
				{WindowStart: histBucket(3), Commit: newCommit, SpansTotal: 100, SpansError: 20},
			},
			deployed:   newCommit,
			wantBefore: 0.1, wantAfter: 0.2, wantDelta: 0.1, wantWin: 2, measured: true,
		},
		{
			name: "staggered rollout: old-binary traffic after the boundary never dilutes the new code's ratio",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 100, SpansError: 10},
				{WindowStart: histBucket(1), Commit: newCommit, SpansTotal: 100, SpansError: 20},
				// A laggard hive still on the old binary in the after window,
				// erroring wildly: must not count toward the after side.
				{WindowStart: histBucket(1), Commit: oldCommit, SpansTotal: 1000, SpansError: 1000},
			},
			deployed:   newCommit,
			wantBefore: 0.1, wantAfter: 0.2, wantDelta: 0.1, wantWin: 1, measured: true,
		},
		{
			name: "converged fleet with no retained before-history is UNMEASURED (the dominant production case)",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: newCommit, SpansTotal: 100, SpansError: 5},
				{WindowStart: histBucket(1), Commit: newCommit, SpansTotal: 100, SpansError: 5},
			},
			deployed: newCommit,
			measured: false,
		},
		{
			name: "deploy never observed running is unmeasured",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 100, SpansError: 10},
			},
			deployed: newCommit,
			measured: false,
		},
		{
			name: "zero-span before side is unmeasured, never a divide-by-zero",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 0, SpansError: 0},
				{WindowStart: histBucket(1), Commit: newCommit, SpansTotal: 10, SpansError: 1},
			},
			deployed: newCommit,
			measured: false,
		},
		{
			name: "zero-span after side is unmeasured, never a divide-by-zero",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 10, SpansError: 1},
				{WindowStart: histBucket(1), Commit: newCommit, SpansTotal: 0, SpansError: 0},
			},
			deployed: newCommit,
			measured: false,
		},
		{
			name: "equal counts taken adjacent to the boundary, not from deep history",
			samples: []HistorySample{
				// An ancient terrible window that a naive whole-history
				// average would drag in (100% errors)…
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 100, SpansError: 100},
				{WindowStart: histBucket(1), Commit: oldCommit, SpansTotal: 100, SpansError: 0},
				{WindowStart: histBucket(2), Commit: oldCommit, SpansTotal: 100, SpansError: 0},
				// …one after-window means one before-window: bucket 2 only.
				{WindowStart: histBucket(3), Commit: newCommit, SpansTotal: 100, SpansError: 0},
			},
			deployed:   newCommit,
			wantBefore: 0, wantAfter: 0, wantDelta: 0, wantWin: 1, measured: true,
		},
		{
			name: "short spoke SHA matches a full deploy SHA by prefix",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 100, SpansError: 10},
				{WindowStart: histBucket(1), Commit: "beef456", SpansTotal: 100, SpansError: 30},
			},
			deployed:   "beef456aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantBefore: 0.1, wantAfter: 0.3, wantDelta: 0.2, wantWin: 1, measured: true,
		},
		{
			name: "a sub-minimum prefix does not match (collision guard)",
			samples: []HistorySample{
				{WindowStart: histBucket(0), Commit: oldCommit, SpansTotal: 100, SpansError: 10},
				{WindowStart: histBucket(1), Commit: "beef45", SpansTotal: 100, SpansError: 30},
			},
			deployed: "beef456aaaaaaaa",
			measured: false,
		},
		{
			name:     "no samples at all is unmeasured",
			samples:  nil,
			deployed: newCommit,
			measured: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeErrorDelta(tc.samples, "hub", tc.deployed)
			if got.Component != "hub" {
				t.Errorf("Component = %q, want hub", got.Component)
			}
			if got.Measured != tc.measured {
				t.Fatalf("Measured = %v, want %v (unmeasured must be distinguishable from zero-delta)", got.Measured, tc.measured)
			}
			if !tc.measured {
				// The unmeasured contract: every numeric field zero, so no
				// consumer can mistake absence of data for a measured 0.0.
				if got.ErrorRateBefore != 0 || got.ErrorRateAfter != 0 || got.Delta != 0 || got.WindowsCompared != 0 {
					t.Errorf("unmeasured delta carries numbers: %+v", got)
				}
				return
			}
			const eps = 1e-9
			if math.Abs(got.ErrorRateBefore-tc.wantBefore) > eps {
				t.Errorf("ErrorRateBefore = %v, want %v", got.ErrorRateBefore, tc.wantBefore)
			}
			if math.Abs(got.ErrorRateAfter-tc.wantAfter) > eps {
				t.Errorf("ErrorRateAfter = %v, want %v", got.ErrorRateAfter, tc.wantAfter)
			}
			if math.Abs(got.Delta-tc.wantDelta) > eps {
				t.Errorf("Delta = %v, want %v", got.Delta, tc.wantDelta)
			}
			if got.WindowsCompared != tc.wantWin {
				t.Errorf("WindowsCompared = %d, want %d", got.WindowsCompared, tc.wantWin)
			}
			// The result must always survive JSON encoding: a NaN from a
			// removed divide-guard fails Marshal outright at runtime, which
			// would break /api/reach's whole response.
			if _, err := json.Marshal(got); err != nil {
				t.Errorf("delta does not JSON-encode: %v", err)
			}
		})
	}
}

// TestComputeErrorDeltaUnsortedInput: the delta must not depend on callers
// honoring the oldest-first contract.
func TestComputeErrorDeltaUnsortedInput(t *testing.T) {
	samples := []HistorySample{
		{WindowStart: histBucket(3), Commit: "beef456", SpansTotal: 100, SpansError: 20},
		{WindowStart: histBucket(0), Commit: "01dc0de", SpansTotal: 100, SpansError: 10},
		{WindowStart: histBucket(2), Commit: "beef456", SpansTotal: 100, SpansError: 20},
		{WindowStart: histBucket(1), Commit: "01dc0de", SpansTotal: 100, SpansError: 10},
	}
	got := ComputeErrorDelta(samples, "hub", "beef456")
	if !got.Measured || got.WindowsCompared != 2 || math.Abs(got.Delta-0.1) > 1e-9 {
		t.Errorf("unsorted input mis-computed: %+v", got)
	}
}

// TestAttachErrorDeltas: deltas attach per attributable component AFTER
// window assignment, include unmeasured components (never dropped), and stay
// absent entirely without a deploy window or history source.
func TestAttachErrorDeltas(t *testing.T) {
	history := &StubHistorySource{Windows: map[string][]HistorySample{
		"hub": {
			{WindowStart: histBucket(0), Commit: "01dc0de", SpansTotal: 100, SpansError: 10},
			{WindowStart: histBucket(1), Commit: "beef456", SpansTotal: 100, SpansError: 30},
		},
		// "governor" has no history at all.
	}}
	a := &Analyzer{History: history}

	report := PRReachReport{
		DeployWindow: "beef456",
		Attribution:  Attribute([]string{"src/pkg/hub/saas.go", "src/pkg/governor/governor.go"}),
	}
	a.AttachErrorDeltas(&report)
	if len(report.ErrorDeltas) != 2 {
		t.Fatalf("ErrorDeltas = %d entries, want 2 (one per attributable component, unmeasured included): %+v",
			len(report.ErrorDeltas), report.ErrorDeltas)
	}
	byComponent := map[string]ComponentErrorDelta{}
	for _, d := range report.ErrorDeltas {
		byComponent[d.Component] = d
	}
	hub := byComponent["hub"]
	if !hub.Measured || math.Abs(hub.Delta-0.2) > 1e-9 || math.Abs(hub.ErrorRateBefore-0.1) > 1e-9 || math.Abs(hub.ErrorRateAfter-0.3) > 1e-9 {
		t.Errorf("hub delta = %+v, want measured before=0.1 after=0.3 delta=0.2", hub)
	}
	gov := byComponent["governor"]
	if gov.Component != "governor" || gov.Measured {
		t.Errorf("governor delta = %+v, want present and unmeasured", gov)
	}

	// No deploy window → no deltas, not zero-valued deltas.
	undeployed := PRReachReport{Attribution: report.Attribution}
	a.AttachErrorDeltas(&undeployed)
	if undeployed.ErrorDeltas != nil {
		t.Errorf("ErrorDeltas without a deploy window = %+v, want nil", undeployed.ErrorDeltas)
	}

	// No history source → no deltas.
	noHistory := &Analyzer{}
	withWindow := PRReachReport{DeployWindow: "beef456", Attribution: report.Attribution}
	noHistory.AttachErrorDeltas(&withWindow)
	if withWindow.ErrorDeltas != nil {
		t.Errorf("ErrorDeltas without a history source = %+v, want nil", withWindow.ErrorDeltas)
	}
}
