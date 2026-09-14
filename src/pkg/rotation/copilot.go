package rotation

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// CopilotProber probes GitHub Copilot premium-request usage for the calendar
// month (kubestellar/hive#6980).
//
// SOURCE (the recorded decision #6980 asks for): the documented enhanced
// billing platform REST endpoint
//
//	GET /users/{username}/settings/billing/premium_request/usage
//
// reached through `gh api`, so authentication, host selection and token
// storage stay the gh CLI's problem — a logged-out CLI or a token missing the
// "Plan: read-only" scope surfaces as an explicit probe error (needs-login /
// forbidden), never as a permissive reading. The undocumented endpoint the IDE
// clients use for their quota display is NOT read: it is not a supported
// machine-readable surface. The Copilot CLI itself has no documented
// non-interactive usage output, and `~/.copilot/session-state/*/events.jsonl`
// (already read elsewhere by detectCopilotModel) carries no premium-request
// accounting keys — checked before reaching for a network call, per #6980.
//
// WHAT THE SOURCE CAN AND CANNOT SAY: the endpoint reports CONSUMED premium
// requests for the month (gross/discount/net per product/SKU/model). It does
// not report the plan's included allowance, so a remaining percentage cannot
// be derived from it alone. When the operator states the plan allowance
// (rotation.providers.<name>.monthly_allowance), the probe emits a normalized
// `monthly` window with pct_remaining, duration, and reset on the first of the
// next month (UTC). Without it, the probe reports an explicit unknown — the
// consumed count is surfaced in the error text informationally — instead of
// manufacturing headroom (#6833: a guard that silently misreads is worse than
// no guard).
//
// PAID OVERAGE: netQuantity > 0 is the documented signal that billed
// pay-per-request usage is already being consumed this month, i.e. the
// contributor's additional-premium-request budget is live. It is read, never
// written: no code path calls any budget endpoint (pinned by
// TestNoCopilotBudgetMutation).
//
// A probe consumes no model turn and no premium request: billing reads are
// not Copilot requests.
type CopilotProber struct {
	ThresholdPct int
	// MonthlyAllowance is the plan's included premium-request count per
	// calendar month, stated by the operator
	// (rotation.providers.<name>.monthly_allowance). 0 means unstated, and the
	// reading stays an explicit unknown.
	MonthlyAllowance int
	// Run overrides CLI execution (tests). Defaults to runCLI.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Now overrides the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

func (p CopilotProber) Provider() string { return "github" }

func (p CopilotProber) run(ctx context.Context, args ...string) (string, error) {
	if p.Run != nil {
		return p.Run(ctx, "gh", args...)
	}
	return runCLI(ctx, "gh", args...)
}

// copilotPremiumUsageResponse mirrors the subset of the documented
// premium-request usage report this probe consumes. UsageItems stays a
// RawMessage first so "the key is absent" (an unrecognized schema, which must
// be reported, #6980) is distinguishable from "the key holds an empty list"
// (a valid month with no usage).
type copilotPremiumUsageResponse struct {
	TimePeriod *struct {
		Year  int `json:"year"`
		Month int `json:"month"`
	} `json:"timePeriod"`
	UsageItems json.RawMessage `json:"usageItems"`
}

type copilotUsageItem struct {
	Product          string  `json:"product"`
	Model            string  `json:"model"`
	UnitType         string  `json:"unitType"`
	GrossQuantity    float64 `json:"grossQuantity"`
	DiscountQuantity float64 `json:"discountQuantity"`
	NetQuantity      float64 `json:"netQuantity"`
}

func (p CopilotProber) Probe(ctx context.Context) Headroom {
	// The documented path is keyed by username; resolve the authenticated
	// login first. This is also the needs-login check #6980 asks for up
	// front: a logged-out gh (or one with no usable token) fails here with
	// gh's own explanation, which the fleet surfaces as an explicit probe
	// error rather than a permissive reading.
	loginOut, err := p.run(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return failOpen(p.Provider(), fmt.Errorf("copilot usage probe needs a logged-in gh CLI (gh auth login): %w", err))
	}
	login := strings.TrimSpace(loginOut)
	if login == "" {
		return failOpen(p.Provider(), fmt.Errorf("copilot usage probe: gh api user returned no login"))
	}
	// Read-only by construction: `gh api` without --method is a GET. A token
	// missing the "Plan: read-only" scope gets an HTTP 403 here, reported
	// verbatim.
	raw, err := p.run(ctx, "api", "/users/"+login+"/settings/billing/premium_request/usage")
	if err != nil {
		return failOpen(p.Provider(), fmt.Errorf("copilot premium-request usage read failed (token may lack the Plan read scope): %w", err))
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	return copilotHeadroom(p.Provider(), p.ThresholdPct, p.MonthlyAllowance, now().UTC(), []byte(raw))
}

// copilotMonthWindow returns the start of the reported (or current) calendar
// month and the first instant of the next one, both UTC.
func copilotMonthWindow(res copilotPremiumUsageResponse, now time.Time) (time.Time, time.Time) {
	year, month := now.Year(), now.Month()
	if res.TimePeriod != nil && res.TimePeriod.Year > 0 && res.TimePeriod.Month >= 1 && res.TimePeriod.Month <= 12 {
		year, month = res.TimePeriod.Year, time.Month(res.TimePeriod.Month)
	}
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

// copilotHeadroom normalizes a premium-request usage report. Split from Probe
// so the parse runs against fixtures (kubestellar/hive#6980 acceptance).
func copilotHeadroom(provider string, thresholdPct, monthlyAllowance int, now time.Time, raw []byte) Headroom {
	var res copilotPremiumUsageResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return failOpen(provider, fmt.Errorf("copilot usage parse: %w", err))
	}
	// An absent usageItems key means this is not the schema the adapter was
	// written against — schema drift is reported, never silently read as "no
	// usage" (#6980).
	if res.UsageItems == nil {
		return failOpen(provider, fmt.Errorf("copilot usage response carries no usageItems — unrecognized schema"))
	}
	var items []copilotUsageItem
	if err := json.Unmarshal(res.UsageItems, &items); err != nil {
		return failOpen(provider, fmt.Errorf("copilot usageItems parse: %w", err))
	}

	var gross, net float64
	for _, it := range items {
		// The report can carry non-Copilot products on the same billing
		// platform; count only Copilot premium requests. An empty product is
		// counted too, so a leaner per-endpoint payload is not read as zero
		// usage.
		if it.Product != "" && !strings.EqualFold(it.Product, "copilot") {
			continue
		}
		gross += it.GrossQuantity
		net += it.NetQuantity
	}

	start, reset := copilotMonthWindow(res, now)
	h := Headroom{Provider: provider, Available: true, ResetAt: reset}
	if net > 0 {
		// Documented, read-only signal that the contributor's budget for
		// additional premium requests is enabled and already being consumed —
		// #6833's "paid credits / extra usage available" bit, observed without
		// enabling anything.
		paid := true
		h.PaidCreditsAvailable = &paid
	}

	if monthlyAllowance <= 0 {
		// No entitlement source: the endpoint alone cannot yield a remaining
		// percentage, and inventing one would be the silent misread #6833
		// rules out. Report unknown, with the consumed count surfaced
		// informationally. ProbeErr readings are never treated as exhaustion.
		h.ProbeErr = fmt.Errorf(
			"copilot premium-request usage: %d consumed this month (%d billed as overage); the documented endpoint reports consumption only, so remaining headroom is unknown — set rotation.providers.<github>.monthly_allowance to your plan's included premium requests to derive it",
			int(math.Round(gross)), int(math.Round(net)))
		return h
	}

	usedPct := int(math.Round(gross / float64(monthlyAllowance) * 100))
	if usedPct > fullPct {
		usedPct = fullPct
	}
	if usedPct < 0 {
		usedPct = 0
	}
	// Kind "monthly" is not in the relay guard's recognized set; the recorded
	// decision (#6980) is that Copilot's monthly window is governed by the
	// guard's BASE reserve via the unrecognized-kind path
	// (guarded_unknown_window) — enforced, with the kind named in the banner —
	// rather than growing the documented kind set. See
	// src/docs/contributor-relay.md.
	h.Limits = []LimitWindow{{
		ID:           "monthly-premium-requests",
		Kind:         "monthly",
		PercentUsed:  usedPct,
		PctRemaining: fullPct - usedPct,
		ResetAt:      reset,
		DurationMins: int(reset.Sub(start).Minutes()),
	}}
	h.PctRemaining = fullPct - usedPct
	h.Available = usedPct < thresholdPct
	return h
}
