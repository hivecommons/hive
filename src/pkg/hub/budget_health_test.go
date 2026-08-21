package hub

import (
	"testing"
	"time"
)

func int64ptr(v int64) *int64 { return &v }
func boolptr(v bool) *bool    { return &v }

func TestBudgetHealthBuckets(t *testing.T) {
	cases := []struct {
		name      string
		used      int64
		limit     int64
		exhausted bool
		want      string
	}{
		{"under warning is ok", budgetWarningPercent - 1, percentDenominator, false, budgetBucketOK},
		{"at warning boundary warns", budgetWarningPercent, percentDenominator, false, budgetBucketWarning},
		{"at critical boundary still warning", budgetCriticalPercent, percentDenominator, false, budgetBucketWarning},
		{"past critical is critical", budgetCriticalPercent + 1, percentDenominator, false, budgetBucketCritical},
		{"at exhausted boundary exhausted", budgetExhaustedPercent, percentDenominator, false, budgetBucketExhausted},
		{"governor exhausted flag wins", 1, percentDenominator, true, budgetBucketExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := RegistryEntry{BudgetCurrentSpend: int64ptr(tc.used), BudgetLimit: int64ptr(tc.limit)}
			if tc.exhausted {
				e.BudgetExhausted = boolptr(true)
			}
			got := budgetHealthFor(e)
			if got.Bucket != tc.want {
				t.Fatalf("bucket = %q, want %q", got.Bucket, tc.want)
			}
		})
	}
}

func TestBudgetHealthUnknownWithoutConfiguredBudget(t *testing.T) {
	cases := []RegistryEntry{
		{},
		{BudgetCurrentSpend: int64ptr(10)},
		{BudgetCurrentSpend: int64ptr(10), BudgetLimit: int64ptr(0)},
	}
	for _, e := range cases {
		got := budgetHealthFor(e)
		if got.Bucket != budgetBucketUnknown {
			t.Fatalf("budgetHealthFor(%+v) bucket = %q, want unknown", e, got.Bucket)
		}
	}
}

func TestBudgetHealthIncludesNumbersAndWindow(t *testing.T) {
	start := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	got := budgetHealthFor(RegistryEntry{
		BudgetCurrentSpend:   int64ptr(25),
		BudgetLimit:          int64ptr(100),
		BudgetWindowStartsAt: start,
		BudgetWindowEndsAt:   end,
	})
	if got.UsedTokens != 25 || got.LimitTokens != 100 || got.PercentUsed != 25 {
		t.Fatalf("numbers = used %d limit %d pct %f, want 25/100/25", got.UsedTokens, got.LimitTokens, got.PercentUsed)
	}
	if got.WindowStartsAt != start.Format(time.RFC3339) || got.WindowEndsAt != end.Format(time.RFC3339) {
		t.Fatalf("window = %q..%q, want %q..%q", got.WindowStartsAt, got.WindowEndsAt, start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
}

func TestBudgetHealthIgnoredIsUnknown(t *testing.T) {
	got := budgetHealthFor(RegistryEntry{
		BudgetCurrentSpend: int64ptr(100),
		BudgetLimit:        int64ptr(100),
		BudgetIgnored:      boolptr(true),
	})
	if got.Bucket != budgetBucketUnknown || !got.Ignored {
		t.Fatalf("ignored budget health = %+v, want unknown ignored", got)
	}
}
