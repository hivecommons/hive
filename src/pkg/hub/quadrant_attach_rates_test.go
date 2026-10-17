package hub

// Tests for the two evidence-normalisation helpers in quadrant_attach.go:
// budgetSpendPerDay and mergeAcceptanceRate. Both feed the fleet quadrant
// scorer, and both must return ok=false — "not a measurement" — rather than
// a misleading zero whenever the underlying signal is absent or too thin.

import (
	"math"
	"testing"
	"time"
)

func i64ptr(v int64) *int64 { return &v }

func TestBudgetSpendPerDay(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		spend    *int64
		start    time.Time
		wantRate float64
		wantOK   bool
	}{
		{
			name:  "nil spend is not a measurement",
			spend: nil,
			start: now.Add(-48 * time.Hour),
		},
		{
			name:  "zero window start is not a measurement",
			spend: i64ptr(1000),
			start: time.Time{},
		},
		{
			name:  "window younger than six hours is noise",
			spend: i64ptr(1000),
			start: now.Add(-minBudgetWindowElapsed + time.Minute),
		},
		{
			name:  "window start in the future (clock skew) is rejected",
			spend: i64ptr(1000),
			start: now.Add(2 * time.Hour),
		},
		{
			name:     "spend over two elapsed days halves to a daily rate",
			spend:    i64ptr(1000),
			start:    now.Add(-48 * time.Hour),
			wantRate: 500,
			wantOK:   true,
		},
		{
			name:     "exactly the six-hour floor is accepted",
			spend:    i64ptr(240),
			start:    now.Add(-minBudgetWindowElapsed),
			wantRate: 960, // 240 tokens in a quarter day
			wantOK:   true,
		},
		{
			name:     "zero spend over a valid window is a real zero rate",
			spend:    i64ptr(0),
			start:    now.Add(-24 * time.Hour),
			wantRate: 0,
			wantOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := RegistryEntry{BudgetCurrentSpend: tc.spend, BudgetWindowStartsAt: tc.start}
			rate, ok := budgetSpendPerDay(e, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if rate != 0 {
					t.Fatalf("rate = %v, want 0 when not measurable", rate)
				}
				return
			}
			if math.Abs(rate-tc.wantRate) > 1e-9 {
				t.Fatalf("rate = %v, want %v", rate, tc.wantRate)
			}
		})
	}
}

func TestMergeAcceptanceRate(t *testing.T) {
	ip := func(v int) *int { return &v }

	cases := []struct {
		name     string
		merged   *int
		rejected *int
		wantRate float64
		wantOK   bool
	}{
		{
			name:     "nil merged count is not a measurement",
			merged:   nil,
			rejected: ip(3),
		},
		{
			name:     "nil rejected count is not a measurement",
			merged:   ip(3),
			rejected: nil,
		},
		{
			name:     "below the minimum PR floor is suppressed",
			merged:   ip(1),
			rejected: ip(1), // total 2 < minPRsForReworkRate
		},
		{
			name:     "three merged one rejected reaches the floor at 0.75",
			merged:   ip(3),
			rejected: ip(1),
			wantRate: 0.75,
			wantOK:   true,
		},
		{
			name:     "all rejected at the floor is a real zero",
			merged:   ip(0),
			rejected: ip(4),
			wantRate: 0,
			wantOK:   true,
		},
		{
			name:     "all merged is a perfect rate",
			merged:   ip(10),
			rejected: ip(0),
			wantRate: 1,
			wantOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := RegistryEntry{PRsMerged90d: tc.merged, PRsRejected90d: tc.rejected}
			rate, ok := mergeAcceptanceRate(e)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if rate != 0 {
					t.Fatalf("rate = %v, want 0 when not measurable", rate)
				}
				return
			}
			if math.Abs(rate-tc.wantRate) > 1e-9 {
				t.Fatalf("rate = %v, want %v", rate, tc.wantRate)
			}
		})
	}
}
