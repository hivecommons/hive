package hub

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

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
	UsedTokens      int64   `json:"usedTokens,omitempty"`
	LimitTokens     int64   `json:"limitTokens,omitempty"`
	PercentUsed     float64 `json:"percentUsed,omitempty"`
	Bucket          string  `json:"bucket"`
	Exhausted       bool    `json:"exhausted,omitempty"`
	ProviderLimited bool    `json:"providerLimited,omitempty"`
	// Misconfigured marks an exhaustion caused by a below-floor LIMIT rather
	// than by real spend (#5508) — the remedy is to fix the number, not to
	// wait for or reset the window.
	Misconfigured  bool   `json:"misconfigured,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Ignored        bool   `json:"ignored,omitempty"`
	WindowStartsAt string `json:"windowStartsAt,omitempty"`
	WindowEndsAt   string `json:"windowEndsAt,omitempty"`
}

func budgetHealthFor(e RegistryEntry) BudgetHealth {
	if reason := providerLimitHealthReason(e.ProviderLimitReason, e.ProviderLimitRebuffs); reason != "" {
		return BudgetHealth{Bucket: budgetBucketExhausted, Exhausted: true, ProviderLimited: true, Reason: reason}
	}
	if n := quotaExhaustedAgentCountForBudget(e.Agents); n > 0 {
		return BudgetHealth{Bucket: budgetBucketExhausted, Exhausted: true, ProviderLimited: true, Reason: fmt.Sprintf("%d agent(s) out of provider quota", n)}
	}
	if e.BudgetIgnored != nil && *e.BudgetIgnored {
		return BudgetHealth{Bucket: budgetBucketUnknown, Ignored: true}
	}
	// The governor's exhausted flag is authoritative even when the numeric
	// spend/limit pair is absent or zero: a spoke that reports "budget
	// reached" without usable numbers (e.g. limit reset to 0 at the window
	// boundary, or an older heartbeat shape) must show EXHAUSTED, not a gray
	// "n/a" — a depleted budget rendered as unknown hides the exact condition
	// the badge exists to surface.
	if e.BudgetExhausted != nil && *e.BudgetExhausted {
		out := BudgetHealth{Bucket: budgetBucketExhausted, Exhausted: true,
			WindowStartsAt: formatOptionalTime(e.BudgetWindowStartsAt),
			WindowEndsAt:   formatOptionalTime(e.BudgetWindowEndsAt),
		}
		if e.BudgetCurrentSpend != nil && e.BudgetLimit != nil && *e.BudgetLimit > 0 {
			used := *e.BudgetCurrentSpend
			if used < 0 {
				used = 0
			}
			out.UsedTokens = used
			out.LimitTokens = *e.BudgetLimit
			out.PercentUsed = float64(used) * float64(percentDenominator) / float64(*e.BudgetLimit)
		}
		// Name a misconfigured limit rather than reporting a spend problem the
		// operator cannot fix by waiting (#5508). Set after the numbers above so
		// the badge still shows used/limit alongside the reason.
		if e.BudgetLimit != nil && config.BudgetLimitBelowFloor(*e.BudgetLimit) {
			out.Misconfigured = true
			out.Reason = fmt.Sprintf("limit of %d tokens cannot fund a single model call — likely a unit mistake", *e.BudgetLimit)
		}
		return out
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

func quotaExhaustedAgentCountForBudget(agents []AgentSummary) int {
	count := 0
	for _, a := range agents {
		if a.QuotaExhausted && !a.Paused &&
			!strings.EqualFold(a.State, agentStatePaused) &&
			strings.EqualFold(a.State, agentStateRunning) {
			count++
		}
	}
	return count
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
