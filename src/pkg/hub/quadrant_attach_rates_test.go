package hub

// The budget normalizer must preserve missing evidence rather than inventing
// zero spend, and must divide by elapsed days, not the configured window size.

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
