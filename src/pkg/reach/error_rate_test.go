package reach

import (
	"math"
	"testing"
	"time"
)

func TestComputeErrorDeltas(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	deployed := []string{"commitA", "commitB", "commitC"}

	reports := map[string][]ComponentReach{
		"hive-1": {
			{Component: "governor", Commit: "commitA", SpansTotal: 100, SpansError: 5, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now.Add(-1 * time.Hour)},
			{Component: "governor", Commit: "commitB", SpansTotal: 200, SpansError: 20, FirstSeen: now.Add(-30 * time.Minute), LastSeen: now},
			{Component: "agent", Commit: "commitA", SpansTotal: 50, SpansError: 1, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now.Add(-1 * time.Hour)},
			{Component: "agent", Commit: "commitB", SpansTotal: 100, SpansError: 2, FirstSeen: now.Add(-30 * time.Minute), LastSeen: now},
		},
		"hive-2": {
			{Component: "governor", Commit: "commitA", SpansTotal: 100, SpansError: 5, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now.Add(-1 * time.Hour)},
			{Component: "governor", Commit: "commitB", SpansTotal: 200, SpansError: 10, FirstSeen: now.Add(-30 * time.Minute), LastSeen: now},
		},
	}

	tests := []struct {
		name          string
		report        PRReachReport
		wantBefore    *float64
		wantAfter     *float64
		wantDelta     *float64
		wantCompCount int
	}{
		{
			name: "single component delta computed",
			report: PRReachReport{
				PR:           101,
				DeployWindow: "commitB",
				Attribution: Attribution{
					Components: []string{"governor"},
				},
			},
			// Before: (5+5)/(100+100) = 10/200 = 0.05
			// After:  (20+10)/(200+200) = 30/400 = 0.075
			// Delta:  0.075 - 0.05 = 0.025
			wantBefore:    ptrFloat(0.05),
			wantAfter:     ptrFloat(0.075),
			wantDelta:     ptrFloat(0.025),
			wantCompCount: 1,
		},
		{
			name: "multi-component aggregate delta",
			report: PRReachReport{
				PR:           102,
				DeployWindow: "commitB",
				Attribution: Attribution{
					Components: []string{"governor", "agent"},
				},
			},
			// Before: (10 + 1) / (200 + 50) = 11 / 250 = 0.044
			// After:  (30 + 2) / (400 + 100) = 32 / 500 = 0.064
			// Delta:  0.064 - 0.044 = 0.02
			wantBefore:    ptrFloat(0.044),
			wantAfter:     ptrFloat(0.064),
			wantDelta:     ptrFloat(0.02),
			wantCompCount: 2,
		},
		{
			name: "earliest commit has no before window",
			report: PRReachReport{
				PR:           103,
				DeployWindow: "commitA",
				Attribution: Attribution{
					Components: []string{"governor"},
				},
			},
			wantBefore:    nil,
			wantAfter:     ptrFloat(0.05),
			wantDelta:     nil,
			wantCompCount: 1,
		},
		{
			name: "undeployed PR produces no error delta",
			report: PRReachReport{
				PR:           104,
				DeployWindow: "",
				Attribution: Attribution{
					Components: []string{"governor"},
				},
			},
			wantBefore:    nil,
			wantAfter:     nil,
			wantDelta:     nil,
			wantCompCount: 0,
		},
		{
			name: "docs only PR produces no error delta",
			report: PRReachReport{
				PR:           105,
				DeployWindow: "commitB",
				Attribution: Attribution{
					Components: []string{},
				},
			},
			wantBefore:    nil,
			wantAfter:     nil,
			wantDelta:     nil,
			wantCompCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.report
			ComputeErrorDeltas(&r, deployed, reports)

			if len(r.ComponentErrorDeltas) != tt.wantCompCount {
				t.Errorf("ComponentErrorDeltas len = %d, want %d", len(r.ComponentErrorDeltas), tt.wantCompCount)
			}
			assertFloatPtrEqual(t, "ErrorRateBefore", r.ErrorRateBefore, tt.wantBefore)
			assertFloatPtrEqual(t, "ErrorRateAfter", r.ErrorRateAfter, tt.wantAfter)
			assertFloatPtrEqual(t, "ErrorRateDelta", r.ErrorRateDelta, tt.wantDelta)
		})
	}
}

func TestPRReachRate(t *testing.T) {
	tests := []struct {
		name         string
		reports      []PRReachReport
		wantRate     float64
		wantMeasured bool
	}{
		{
			name: "normal mix of reached and unreached deployed PRs",
			reports: []PRReachReport{
				{PR: 1, Deployed: true, Attribution: Attribution{Components: []string{"governor"}}, ReachCount: 3},
				{PR: 2, Deployed: true, Attribution: Attribution{Components: []string{"agent"}}, ReachCount: 0},
				{PR: 3, Deployed: true, Attribution: Attribution{Components: []string{"main"}}, ReachCount: 1},
				{PR: 4, Deployed: false, Attribution: Attribution{Components: []string{"proxy"}}, ReachCount: 0}, // undeployed excluded
				{PR: 5, Deployed: true, Attribution: Attribution{Components: []string{}}, ReachCount: 0},        // unattributable excluded
			},
			wantRate:     2.0 / 3.0, // 2 reached out of 3 eligible
			wantMeasured: true,
		},
		{
			name: "no eligible PRs is unmeasured",
			reports: []PRReachReport{
				{PR: 1, Deployed: false, Attribution: Attribution{Components: []string{"governor"}}, ReachCount: 0},
				{PR: 2, Deployed: true, Attribution: Attribution{Components: []string{}}, ReachCount: 0},
			},
			wantRate:     0.0,
			wantMeasured: false,
		},
		{
			name:         "empty report list is unmeasured",
			reports:      []PRReachReport{},
			wantRate:     0.0,
			wantMeasured: false,
		},
		{
			name: "all reached",
			reports: []PRReachReport{
				{PR: 1, Deployed: true, Attribution: Attribution{Components: []string{"governor"}}, ReachCount: 2},
				{PR: 2, Deployed: true, Attribution: Attribution{Components: []string{"agent"}}, ReachCount: 1},
			},
			wantRate:     1.0,
			wantMeasured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rate, measured := PRReachRate(tt.reports)
			if measured != tt.wantMeasured {
				t.Fatalf("measured = %v, want %v", measured, tt.wantMeasured)
			}
			if math.Abs(rate-tt.wantRate) > 1e-6 {
				t.Errorf("rate = %v, want %v", rate, tt.wantRate)
			}
		})
	}
}

func ptrFloat(f float64) *float64 {
	return &f
}

func assertFloatPtrEqual(t *testing.T, label string, got, want *float64) {
	t.Helper()
	if got == nil && want == nil {
		return
	}
	if got == nil || want == nil {
		t.Fatalf("%s: got %v, want %v", label, got, want)
	}
	if math.Abs(*got-*want) > 1e-6 {
		t.Errorf("%s: got %v, want %v", label, *got, *want)
	}
}
