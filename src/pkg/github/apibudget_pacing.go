package github

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// API budget pacing (governor.api_budget_pacing, off by default) spreads the
// hive's GitHub calls over the rate-limit window instead of spending the
// whole budget early and then making no calls until the reset.
//
// The rule: take the hive's own charged core calls per minute over the last
// apiBudgetPacingBurnSpan as the burn rate. If, at that rate, the calls left
// above the reserve (github.api_reserve) would run out before the window
// resets, stretch the loop interval by
//
//	factor = burn × minutes_to_reset / max(remaining − reserve, 1)
//
// clamped to at least 1 and so that the interval never exceeds
// governor.eval_interval_max_s. Otherwise the factor is 1.
//
// A stretched loop spends less, so the measured rate falls as soon as pacing
// works. Sizing the factor from that lower rate would release the stretch,
// the rate would rise and the stretch would come back. So the rule works on
// the UNPACED rate: the measured rate multiplied by the stretch in effect
// while it was measured, and for apiBudgetPacingBurnSpan after the factor
// changes (while the measurement still mixes two cadences) the estimate from
// before the change.
//
// Hysteresis: a held factor is kept until the needed stretch leaves a
// apiBudgetPacingResizeBand band around it, falls to
// apiBudgetPacingReleaseBelow or lower (the projection is healthy by a
// margin, so the stretch is released), or a new window starts.
//
// Pacing only ever lengthens an interval. It does not change what a cycle
// does, and an early wake (a webhook, a review verdict) still runs at once.
const (
	// apiBudgetPacingBurnSpan is the recent span the burn rate is measured over.
	apiBudgetPacingBurnSpan = 10 * time.Minute
	// apiBudgetPacingMinHistory is how much recorded call history a burn rate
	// needs. A fresh start has none, so it is not paced until this much exists.
	apiBudgetPacingMinHistory = 5 * time.Minute
	// apiBudgetPacingReleaseBelow releases a held stretch once the needed
	// factor is this or lower: the unpaced rate would leave a 20% margin.
	apiBudgetPacingReleaseBelow = 0.8
	// apiBudgetPacingResizeBand is how far the needed factor may drift from
	// the held one (as a ratio, both ways) before the held one is re-sized.
	apiBudgetPacingResizeBand = 0.25
	// apiBudgetPacingResetSlack lands a stretched tick just after the reset
	// instead of sleeping past a fresh budget.
	apiBudgetPacingResetSlack = 5 * time.Second
	// apiBudgetPacingNoCap bounds the interval when governor.eval_interval_max_s
	// is unset: one rate-limit window.
	apiBudgetPacingNoCap = restAccountingWindow

	apiBudgetPacingOffReason = "off (governor.api_budget_pacing)"
)

// APIBudgetPacing is the pacing state /api/status and the general-advanced
// settings report.
type APIBudgetPacing struct {
	// Enabled is governor.api_budget_pacing as the loop last read it.
	Enabled bool `json:"enabled"`
	// Active is true while a stretch is held.
	Active bool `json:"active"`
	// Factor is the held stretch; 1 when not pacing.
	Factor float64 `json:"factor"`
	// EffectiveIntervalS is the loop interval after pacing.
	EffectiveIntervalS int `json:"effective_interval_s"`
	// BurnPerMin is the hive's own charged core calls per minute over the
	// last apiBudgetPacingBurnSpan, as measured (under any stretch in effect).
	BurnPerMin float64 `json:"burn_per_min"`
	// Reason says in plain words why pacing is or is not stretching.
	Reason string `json:"reason"`

	// Remaining and ResetIn are the core reading the decision used, for the
	// log line; they go stale, so they are not served.
	Remaining int           `json:"-"`
	ResetIn   time.Duration `json:"-"`
}

// APIBudgetPacingInput is what the loop hands the pacer each time it
// recomputes its interval.
type APIBudgetPacingInput struct {
	Enabled bool
	// Interval is the loop interval without pacing: the API budget mode
	// stretch composed with the webhook cadence.
	Interval time.Duration
	// MaxInterval is governor.eval_interval_max_s; zero means unset.
	MaxInterval time.Duration
	// Reserve is github.api_reserve.
	Reserve int
}

// apiBudgetPaceReading is the evidence one decision is made from.
type apiBudgetPaceReading struct {
	now       time.Time
	hasCore   bool
	remaining int
	reset     time.Time
	// burnPerMin is the measured rate; hasBurn is false without enough history.
	burnPerMin float64
	hasBurn    bool
}

// apiBudgetPaceState is what the pacer carries between decisions.
type apiBudgetPaceState struct {
	// factor is the held stretch; below 1 means none.
	factor float64
	// since is when factor last changed.
	since time.Time
	// reset identifies the window factor was sized for.
	reset time.Time
	// stretch is effective interval / unpaced interval at the last decision,
	// which can be below factor when a cap applied.
	stretch float64
	// unpaced is the estimated calls per minute at the unpaced cadence.
	unpaced float64
}

// apiBudgetPaceStep is the pacing rule: pure, so every branch is testable.
func apiBudgetPaceStep(prev apiBudgetPaceState, in APIBudgetPacingInput, r apiBudgetPaceReading) (apiBudgetPaceState, APIBudgetPacing) {
	out := APIBudgetPacing{Enabled: in.Enabled, Factor: 1, EffectiveIntervalS: int(in.Interval / time.Second)}
	if !in.Enabled {
		out.Reason = apiBudgetPacingOffReason
		return apiBudgetPaceState{}, out
	}
	next := prev
	if next.factor < 1 {
		next.factor = 1
	}
	if next.stretch < 1 {
		next.stretch = 1
	}
	if r.hasBurn {
		out.BurnPerMin = apiBudgetPacingRound(r.burnPerMin)
	}
	// unpacedNow keeps the estimate but drops any stretch for this decision.
	unpacedNow := func(reason string) (apiBudgetPaceState, APIBudgetPacing) {
		if next.factor != 1 {
			next.factor, next.since = 1, r.now
		}
		next.stretch = 1
		out.Reason = reason
		return next, out
	}
	if !r.hasCore {
		return unpacedNow("no GitHub rate-limit reading yet")
	}
	toReset := r.reset.Sub(r.now)
	if toReset <= 0 {
		return unpacedNow("the rate-limit window has reset; waiting for a reading from the new one")
	}
	out.Remaining, out.ResetIn = r.remaining, toReset

	settled := next.since.IsZero() || r.now.Sub(next.since) >= apiBudgetPacingBurnSpan
	if r.hasBurn && (settled || next.unpaced <= 0) {
		next.unpaced = r.burnPerMin * next.stretch
	} else if next.unpaced <= 0 {
		return unpacedNow("not enough GitHub call history yet")
	}

	limit := in.MaxInterval
	if limit <= 0 {
		limit = apiBudgetPacingNoCap
	}
	maxFactor := 1.0
	if in.Interval > 0 && limit > in.Interval {
		maxFactor = float64(limit) / float64(in.Interval)
	}
	headroom := float64(r.remaining - in.Reserve)
	if headroom < 1 {
		headroom = 1
	}
	need := math.Min(next.unpaced*toReset.Minutes()/headroom, maxFactor)

	newWindow := apiBudgetPacingOtherWindow(next.reset, r.reset)
	factor := next.factor
	inBand := need <= factor*(1+apiBudgetPacingResizeBand) && need >= factor/(1+apiBudgetPacingResizeBand)
	switch {
	case factor <= 1 || newWindow:
		// Not holding a stretch, or a new window: size it from scratch. A
		// held factor the new window would size within the band is kept, so
		// two windows reported in turn do not flap it.
		if need <= 1 {
			factor = 1
		} else if factor <= 1 || !inBand {
			factor = need
		}
	case need <= apiBudgetPacingReleaseBelow:
		factor = 1
	case need > 1 && !inBand:
		factor = need
	}
	factor = apiBudgetPacingRound(factor)
	if factor < 1 {
		factor = 1
	}
	if factor != next.factor {
		next.factor, next.since = factor, r.now
	}
	next.reset = r.reset

	paced := time.Duration(float64(in.Interval) * factor).Round(time.Second)
	if paced > limit {
		paced = limit
	}
	if untilReset := (toReset + apiBudgetPacingResetSlack).Round(time.Second); paced > untilReset {
		paced = untilReset
	}
	if paced < in.Interval {
		// Never shorter than the unpaced interval.
		paced = in.Interval
	}
	next.stretch = 1
	if in.Interval > 0 {
		next.stretch = float64(paced) / float64(in.Interval)
	}

	out.Factor, out.Active, out.EffectiveIntervalS = factor, factor > 1, int(paced/time.Second)
	rate, mins := apiBudgetPacingRound(next.unpaced), int(math.Ceil(toReset.Minutes()))
	switch {
	case !out.Active:
		out.Reason = fmt.Sprintf("on track: about %v calls a minute fits the %d left above the reserve of %d until the reset in %d min",
			rate, max(r.remaining-in.Reserve, 0), in.Reserve, mins)
	case need <= 1:
		out.Reason = fmt.Sprintf("holding the stretch until about %v calls a minute would leave a %d%% margin before the reset in %d min",
			rate, int(math.Round((1-apiBudgetPacingReleaseBelow)*100)), mins)
	default:
		out.Reason = fmt.Sprintf("at about %v calls a minute the %d left above the reserve of %d would run out before the reset in %d min",
			rate, max(r.remaining-in.Reserve, 0), in.Reserve, mins)
	}
	return next, out
}

// apiBudgetPacingOtherWindow reports whether two resets name different
// rate-limit windows. One window's reset header can differ by a second
// between responses, so a minute of tolerance is allowed.
func apiBudgetPacingOtherWindow(held, seen time.Time) bool {
	if held.IsZero() {
		return true
	}
	d := seen.Sub(held)
	return d > time.Minute || d < -time.Minute
}

func apiBudgetPacingRound(v float64) float64 { return math.Round(v*100) / 100 }

// chargedBurn is the hive's own charged core calls per minute over the last
// span. Agent callers are left out (their calls show up in the falling
// remaining instead), and so are calls that draw on another bucket (search,
// GraphQL). It reports false until the store holds minHistory of calls.
func (s *restAccountingStore) chargedBurn(now time.Time, span, minHistory time.Duration) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.events) == 0 {
		return 0, false
	}
	history := now.Sub(s.events[0].at)
	if history < minHistory || history <= 0 {
		return 0, false
	}
	if history < span {
		span = history
	}
	cutoff := now.Add(-span)
	calls := 0
	for i := len(s.events) - 1; i >= 0 && !s.events[i].at.Before(cutoff); i-- {
		ev := s.events[i]
		if ev.charged && ev.core && !strings.HasPrefix(ev.caller, agentRESTCallerPrefix) {
			calls++
		}
	}
	return float64(calls) / span.Minutes(), true
}

type apiBudgetPacer struct {
	mu    sync.Mutex
	state apiBudgetPaceState
	last  APIBudgetPacing
	// interval is the unpaced interval of the last decision.
	interval time.Duration
}

var sharedAPIBudgetPacer apiBudgetPacer

// ObserveAPIBudgetPacing advances the pacer by one decision (the governor
// calls it wherever it recomputes the eval interval) and reports the state
// plus whether the held factor changed, so the caller logs once per change.
// Off reads nothing and holds nothing.
func ObserveAPIBudgetPacing(in APIBudgetPacingInput) (state APIBudgetPacing, changed bool) {
	p := &sharedAPIBudgetPacer
	p.mu.Lock()
	defer p.mu.Unlock()
	before := math.Max(p.state.factor, 1)
	var r apiBudgetPaceReading
	if in.Enabled {
		store := sharedRESTAccounting()
		r.now = store.now()
		if ev, ok := store.coreEvidence(); ok && !ev.Reset.IsZero() {
			r.hasCore, r.remaining, r.reset = true, ev.Remaining, ev.Reset
		}
		r.burnPerMin, r.hasBurn = store.chargedBurn(r.now, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory)
	}
	p.state, p.last = apiBudgetPaceStep(p.state, in, r)
	p.interval = in.Interval
	return p.last, p.last.Factor != before
}

// APIBudgetPacingSnapshot reports the last decision without advancing the
// pacer. A stretch sized for a window that has since reset reads as released.
func APIBudgetPacingSnapshot() APIBudgetPacing {
	p := &sharedAPIBudgetPacer
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.last
	if out.Factor < 1 {
		out.Factor = 1
	}
	if out.Reason == "" {
		out.Reason = apiBudgetPacingOffReason
	}
	if out.Active && !sharedRESTAccounting().now().Before(p.state.reset) {
		out.Active, out.Factor, out.EffectiveIntervalS = false, 1, int(p.interval/time.Second)
		out.Reason = "the rate-limit window has reset; pacing is sized again at the next eval cycle"
	}
	return out
}

// APIBudgetPacingStretch is how much longer than unpaced the loop interval
// currently is (1 when not pacing), for the sweep workers, which run on their
// own intervals. It drops back to 1 the moment the window resets.
func APIBudgetPacingStretch() float64 {
	p := &sharedAPIBudgetPacer
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.last.Active || p.state.stretch <= 1 || !sharedRESTAccounting().now().Before(p.state.reset) {
		return 1
	}
	return p.state.stretch
}

// ResetAPIBudgetPacingForTest gives the pacer a fresh accounting store on the
// given clock and returns the restore function.
func ResetAPIBudgetPacingForTest(now func() time.Time) (restore func()) {
	old := sharedRESTAccountingPtr.Swap(&restAccountingStore{now: now})
	apiBudgetPacerReset()
	return func() {
		sharedRESTAccountingPtr.Store(old)
		apiBudgetPacerReset()
	}
}

func apiBudgetPacerReset() {
	p := &sharedAPIBudgetPacer
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state, p.last, p.interval = apiBudgetPaceState{}, APIBudgetPacing{}, 0
}
