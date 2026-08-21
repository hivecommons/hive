package hub

import "time"

const (
	budgetBucketOK        = "ok"
	budgetBucketWarning   = "warning"
	budgetBucketCritical  = "critical"
	budgetBucketExhausted = "exhausted"
	budgetBucketUnknown   = "unknown"

	budgetWarningPercent   int64 = 70
	budgetCriticalPercent  int64 = 90
	budgetExhaustedPercent int64 = 100
	percentDenominator     int64 = 100
)

type BudgetHealth struct {
	UsedTokens     int64   `json:"usedTokens,omitempty"`
	LimitTokens    int64   `json:"limitTokens,omitempty"`
	PercentUsed    float64 `json:"percentUsed,omitempty"`
	Bucket         string  `json:"bucket"`
	Exhausted      bool    `json:"exhausted,omitempty"`
	Ignored        bool    `json:"ignored,omitempty"`
	WindowStartsAt string  `json:"windowStartsAt,omitempty"`
	WindowEndsAt   string  `json:"windowEndsAt,omitempty"`
}

func budgetHealthFor(e RegistryEntry) BudgetHealth {
	if e.BudgetIgnored != nil && *e.BudgetIgnored {
		return BudgetHealth{Bucket: budgetBucketUnknown, Ignored: true}
	}
	if e.BudgetCurrentSpend == nil || e.BudgetLimit == nil || *e.BudgetLimit <= 0 {
		return BudgetHealth{Bucket: budgetBucketUnknown}
	}
	used := *e.BudgetCurrentSpend
	if used < 0 {
		used = 0
	}
	limit := *e.BudgetLimit
	out := BudgetHealth{
		UsedTokens:     used,
		LimitTokens:    limit,
		PercentUsed:    float64(used) * float64(percentDenominator) / float64(limit),
		Bucket:         budgetBucketOK,
		WindowStartsAt: formatOptionalTime(e.BudgetWindowStartsAt),
		WindowEndsAt:   formatOptionalTime(e.BudgetWindowEndsAt),
	}
	if e.BudgetExhausted != nil && *e.BudgetExhausted {
		out.Bucket = budgetBucketExhausted
		out.Exhausted = true
		return out
	}
	if used*percentDenominator >= limit*budgetExhaustedPercent {
		out.Bucket = budgetBucketExhausted
		out.Exhausted = true
		return out
	}
	if used*percentDenominator > limit*budgetCriticalPercent {
		out.Bucket = budgetBucketCritical
		return out
	}
	if used*percentDenominator >= limit*budgetWarningPercent {
		out.Bucket = budgetBucketWarning
	}
	return out
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
