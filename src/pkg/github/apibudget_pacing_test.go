package github

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

var apiBudgetPacingT0 = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

// apiBudgetPacingIn is a polling hive: a 120s loop capped at 1800s, reserve 800.
func apiBudgetPacingIn() APIBudgetPacingInput {
	return APIBudgetPacingInput{Enabled: true, Interval: 120 * time.Second, MaxInterval: 1800 * time.Second, Reserve: 800}
}

// apiBudgetPacingRead is a reading taken at T0+at with the window resetting
// toReset later.
func apiBudgetPacingRead(at, toReset time.Duration, remaining int, burn float64) apiBudgetPaceReading {
	now := apiBudgetPacingT0.Add(at)
	return apiBudgetPaceReading{now: now, hasCore: true, remaining: remaining, reset: now.Add(toReset), burnPerMin: burn, hasBurn: true}
}

func TestAPIBudgetPaceStepHealthyProjectionDoesNotStretch(t *testing.T) {
	// 50 a minute for 60 minutes is 3000 calls; 6200 are left above the reserve.
	st, out := apiBudgetPaceStep(apiBudgetPaceState{}, apiBudgetPacingIn(), apiBudgetPacingRead(0, time.Hour, 7000, 50))
	if out.Active || out.Factor != 1 || out.EffectiveIntervalS != 120 || !out.Enabled {
		t.Fatalf("healthy projection paced: %+v", out)
	}
	if out.BurnPerMin != 50 || !strings.HasPrefix(out.Reason, "on track") {
		t.Fatalf("state = %+v", out)
	}
	if st.factor != 1 || !st.since.IsZero() {
		t.Fatalf("pacer state = %+v, want an unheld factor", st)
	}
}

func TestAPIBudgetPaceStepProjectedExhaustionMatchesFormula(t *testing.T) {
	// The measured hour: 200 calls a minute against 7000 an hour. 200 × 60 =
	// 12000 calls wanted, 7000 − 800 = 6200 available: factor 12000/6200.
	st, out := apiBudgetPaceStep(apiBudgetPaceState{}, apiBudgetPacingIn(), apiBudgetPacingRead(0, time.Hour, 7000, 200))
	if !out.Active || out.Factor != 1.94 {
		t.Fatalf("factor = %v active = %v, want 1.94 (12000/6200)", out.Factor, out.Active)
	}
	// 120s × 1.94 = 232.8s.
	if out.EffectiveIntervalS != 233 {
		t.Fatalf("effective interval = %ds, want 233", out.EffectiveIntervalS)
	}
	if out.Remaining != 7000 || out.ResetIn != time.Hour || !strings.Contains(out.Reason, "would run out before the reset") {
		t.Fatalf("state = %+v", out)
	}
	if st.unpaced != 200 || !st.since.Equal(apiBudgetPacingT0) {
		t.Fatalf("pacer state = %+v", st)
	}
}

func TestAPIBudgetPaceStepClampsToMaxIntervalAndReset(t *testing.T) {
	in := apiBudgetPacingIn()
	// 100 calls above the reserve and 30 minutes to go: the formula says 60,
	// eval_interval_max_s (1800s over a 120s loop) allows 15.
	_, out := apiBudgetPaceStep(apiBudgetPaceState{}, in, apiBudgetPacingRead(0, 30*time.Minute, 900, 200))
	if out.Factor != 15 || out.EffectiveIntervalS != 1800 {
		t.Fatalf("clamped factor = %v interval = %ds, want 15 and 1800", out.Factor, out.EffectiveIntervalS)
	}
	// Below the reserve the divisor is held at 1, never zero or negative.
	_, out = apiBudgetPaceStep(apiBudgetPaceState{}, in, apiBudgetPacingRead(0, 30*time.Minute, 500, 200))
	if out.Factor != 15 || out.EffectiveIntervalS != 1800 {
		t.Fatalf("below the reserve: factor = %v interval = %ds, want 15 and 1800", out.Factor, out.EffectiveIntervalS)
	}
	// Four minutes before the reset the loop does not sleep 30 minutes past a
	// fresh budget: the next tick lands just after the reset.
	_, out = apiBudgetPaceStep(apiBudgetPaceState{}, in, apiBudgetPacingRead(0, 4*time.Minute, 500, 200))
	if out.EffectiveIntervalS != 245 || !out.Active {
		t.Fatalf("near the reset: interval = %ds, want 245 (4m + 5s slack)", out.EffectiveIntervalS)
	}
	// No cap configured: one rate-limit window bounds it.
	in.MaxInterval = 0
	_, out = apiBudgetPaceStep(apiBudgetPaceState{}, in, apiBudgetPacingRead(0, 2*time.Hour, 500, 200))
	if out.EffectiveIntervalS != 3600 {
		t.Fatalf("uncapped interval = %ds, want 3600", out.EffectiveIntervalS)
	}
}

func TestAPIBudgetPaceStepWithoutEvidenceDoesNotPace(t *testing.T) {
	in := apiBudgetPacingIn()
	exhausting := apiBudgetPacingRead(0, time.Hour, 1000, 500)
	for name, r := range map[string]apiBudgetPaceReading{
		"no core reading": {now: apiBudgetPacingT0, burnPerMin: 500, hasBurn: true},
		"no call history": {now: apiBudgetPacingT0, hasCore: true, remaining: 1000, reset: apiBudgetPacingT0.Add(time.Hour)},
		"reset passed":    {now: apiBudgetPacingT0, hasCore: true, remaining: 1000, reset: apiBudgetPacingT0.Add(-time.Second), burnPerMin: 500, hasBurn: true},
		"reset is now":    {now: apiBudgetPacingT0, hasCore: true, remaining: 1000, reset: apiBudgetPacingT0, burnPerMin: 500, hasBurn: true},
	} {
		st, out := apiBudgetPaceStep(apiBudgetPaceState{}, in, r)
		if out.Active || out.Factor != 1 || out.EffectiveIntervalS != 120 || out.Reason == "" {
			t.Errorf("%s: paced: %+v", name, out)
		}
		if st.factor != 1 || st.stretch != 1 {
			t.Errorf("%s: pacer state = %+v", name, st)
		}
	}
	// Off: nothing is held, whatever the evidence says.
	held := apiBudgetPaceState{factor: 3, stretch: 3, unpaced: 500, since: apiBudgetPacingT0, reset: exhausting.reset}
	in.Enabled = false
	st, out := apiBudgetPaceStep(held, in, exhausting)
	if out.Enabled || out.Active || out.Factor != 1 || out.EffectiveIntervalS != 120 || out.Reason != apiBudgetPacingOffReason {
		t.Fatalf("off: %+v", out)
	}
	if st != (apiBudgetPaceState{}) {
		t.Fatalf("off kept pacer state: %+v", st)
	}
	// A reading that vanishes drops the stretch for that decision but keeps
	// the rate estimate for the next window.
	st, out = apiBudgetPaceStep(held, apiBudgetPacingIn(), apiBudgetPaceReading{now: apiBudgetPacingT0.Add(time.Minute)})
	if out.Active || out.Factor != 1 || st.factor != 1 || st.unpaced != 500 || !st.since.Equal(apiBudgetPacingT0.Add(time.Minute)) {
		t.Fatalf("lost reading: out = %+v state = %+v", out, st)
	}
}

func TestAPIBudgetPaceStepHysteresisHoldsThenReleases(t *testing.T) {
	in := apiBudgetPacingIn()
	reset := apiBudgetPacingT0.Add(time.Hour)
	at := func(elapsed time.Duration, remaining int, burn float64) apiBudgetPaceReading {
		return apiBudgetPaceReading{now: apiBudgetPacingT0.Add(elapsed), hasCore: true, remaining: remaining, reset: reset, burnPerMin: burn, hasBurn: true}
	}
	st, out := apiBudgetPaceStep(apiBudgetPaceState{}, in, at(0, 7000, 200))
	if out.Factor != 1.94 {
		t.Fatalf("engage factor = %v, want 1.94", out.Factor)
	}
	engaged := st.since

	// Four minutes on, the measured rate has fallen because pacing works
	// (the span still mixes both cadences). The estimate stays at 200, so
	// the needed factor is 200 × 56 / 5800 = 1.93: held, not re-sized.
	st, out = apiBudgetPaceStep(st, in, at(4*time.Minute, 6600, 160))
	if out.Factor != 1.94 || !st.since.Equal(engaged) || st.unpaced != 200 || out.BurnPerMin != 160 {
		t.Fatalf("unsettled step: out = %+v state = %+v", out, st)
	}

	// Past the burn span the measured rate is the paced one, 103 a minute,
	// and is scaled back by the stretch in effect (233s / 120s) to about 200.
	st, out = apiBudgetPaceStep(st, in, at(12*time.Minute, 5770, 103))
	if out.Factor != 1.94 || !st.since.Equal(engaged) || st.unpaced < 199 || st.unpaced > 201 {
		t.Fatalf("settled step: out = %+v state = %+v", out, st)
	}

	// The work thins out: 110 unpaced calls a minute for 40 minutes is 4400,
	// against 4800 above the reserve. Needed factor 0.92: no longer
	// exhausting, but not healthy by the margin either, so the stretch holds.
	st, out = apiBudgetPaceStep(st, in, at(20*time.Minute, 5600, 110/st.stretch))
	if !out.Active || out.Factor != 1.94 || !st.since.Equal(engaged) || !strings.HasPrefix(out.Reason, "holding the stretch") {
		t.Fatalf("inside the margin: out = %+v state = %+v", out, st)
	}

	// 80 a minute for 39 minutes is 3120 against 4750: needed factor 0.66,
	// under the 0.8 release line.
	st, out = apiBudgetPaceStep(st, in, at(21*time.Minute, 5550, 80/st.stretch))
	if out.Active || out.Factor != 1 || out.EffectiveIntervalS != 120 || !st.since.Equal(apiBudgetPacingT0.Add(21*time.Minute)) {
		t.Fatalf("release: out = %+v state = %+v", out, st)
	}

	// Released, the estimate is frozen for the burn span, so a measured rate
	// that still reflects the old stretch cannot re-engage it at once.
	st, out = apiBudgetPaceStep(st, in, at(22*time.Minute, 5500, 300))
	if out.Active || st.unpaced > 81 {
		t.Fatalf("flapped straight back: out = %+v state = %+v", out, st)
	}
}

func TestAPIBudgetPaceStepResizesOutsideTheBand(t *testing.T) {
	in := apiBudgetPacingIn()
	reset := apiBudgetPacingT0.Add(time.Hour)
	held := apiBudgetPaceState{factor: 2, stretch: 2, unpaced: 200, since: apiBudgetPacingT0, reset: reset}
	read := func(remaining int) apiBudgetPaceReading {
		return apiBudgetPaceReading{now: apiBudgetPacingT0.Add(20 * time.Minute), hasCore: true, remaining: remaining, reset: reset, burnPerMin: 100, hasBurn: true}
	}
	// 200 × 40 = 8000 wanted. With 4000 above the reserve the need is 2.0;
	// 3400 → 2.35 (inside 2 × 1.25); 3000 → 2.67 (outside, re-sized up);
	// 6000 → 1.33 (outside 2 / 1.25 = 1.6, re-sized down, still pacing).
	for _, tc := range []struct {
		remaining int
		want      float64
		moved     bool
	}{{4800, 2, false}, {4200, 2, false}, {3800, 2.67, true}, {6800, 1.33, true}} {
		st, out := apiBudgetPaceStep(held, in, read(tc.remaining))
		if out.Factor != tc.want || !out.Active || st.since.Equal(held.since) == tc.moved {
			t.Errorf("remaining %d: factor = %v (since %v), want %v moved=%v", tc.remaining, out.Factor, st.since, tc.want, tc.moved)
		}
	}
}

func TestAPIBudgetPaceStepNewWindowReleases(t *testing.T) {
	in := apiBudgetPacingIn()
	reset := apiBudgetPacingT0.Add(30 * time.Minute)
	held := apiBudgetPaceState{factor: 3, stretch: 3, unpaced: 100, since: apiBudgetPacingT0, reset: reset}
	now := apiBudgetPacingT0.Add(5 * time.Minute)
	// 100 a minute for the 25 minutes left against 2583 above the reserve:
	// needed factor 0.97. In the window the factor was sized for, that is
	// inside the release margin and the stretch holds.
	same := apiBudgetPaceReading{now: now, hasCore: true, remaining: 3383, reset: reset, burnPerMin: 33, hasBurn: true}
	if _, out := apiBudgetPaceStep(held, in, same); out.Factor != 3 {
		t.Fatalf("same window: factor = %v, want the held 3", out.Factor)
	}
	// One window whose reset header moves by a second is still that window.
	jitter := same
	jitter.reset = reset.Add(time.Second)
	if _, out := apiBudgetPaceStep(held, in, jitter); out.Factor != 3 {
		t.Fatalf("reset jitter re-sized the factor to %v", out.Factor)
	}
	// The same need in a NEW window (100 × 60 / 6200) is sized from scratch:
	// released.
	fresh := apiBudgetPaceReading{now: now, hasCore: true, remaining: 7000, reset: now.Add(time.Hour), burnPerMin: 33, hasBurn: true}
	st, out := apiBudgetPaceStep(held, in, fresh)
	if out.Active || out.Factor != 1 || out.EffectiveIntervalS != 120 || !st.reset.Equal(fresh.reset) {
		t.Fatalf("new window: out = %+v state = %+v", out, st)
	}
	// A new window that still needs a stretch gets one sized for it, from
	// the carried estimate: 200 × 60 / 6200.
	held.unpaced = 200
	if _, out := apiBudgetPaceStep(held, in, fresh); out.Factor != 1.94 {
		t.Fatalf("new window needing a stretch: factor = %v, want 1.94", out.Factor)
	}
}

// apiBudgetPacingRecord records one call at the store's current time.
func apiBudgetPacingRecord(caller, path string, status, remaining int, reset time.Time, resource string) {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "7000")
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	if resource != "" {
		h.Set("X-RateLimit-Resource", resource)
	}
	RecordRESTRequest(caller, http.MethodGet, path, status, h)
}

func TestRESTAccountingChargedBurnCountsTheHivesOwnCoreCalls(t *testing.T) {
	now := apiBudgetPacingT0
	advance := resetRESTAccountingForTest(t, now)
	store := sharedRESTAccounting()
	reset := now.Add(time.Hour)
	if _, ok := store.chargedBurn(now, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory); ok {
		t.Fatal("an empty store reported a burn rate")
	}
	// One call a second for four minutes: not enough history yet.
	for i := 0; i < 240; i++ {
		advance(now.Add(time.Duration(i) * time.Second))
		apiBudgetPacingRecord("hive:eval_cycle", "/repos/acme/widgets/pulls", http.StatusOK, 6999-i, reset, "core")
	}
	at := now.Add(4 * time.Minute)
	if _, ok := store.chargedBurn(at, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory); ok {
		t.Fatal("four minutes of history reported a burn rate")
	}
	// At six minutes the rate is over the history there is: 240 calls / 6 min.
	at = now.Add(6 * time.Minute)
	advance(at)
	if got, ok := store.chargedBurn(at, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory); !ok || got != 40 {
		t.Fatalf("burn = %v ok = %v, want 40 a minute", got, ok)
	}
	// None of these are the hive's own charged core calls.
	apiBudgetPacingRecord("agent:worker-1", "/repos/acme/widgets/issues", http.StatusOK, 6000, reset, "core")
	apiBudgetPacingRecord("hive:eval_cycle", "/repos/acme/widgets/issues", http.StatusNotModified, 6000, reset, "core")
	apiBudgetPacingRecord("hive:metrics_collector", "/search/issues", http.StatusOK, 29, reset, "search")
	apiBudgetPacingRecord("hive:eval_cycle", "/rate_limit", http.StatusOK, 6000, reset, "core")
	RecordLocalServe("dashboard", "/api/status", http.StatusOK)
	if got, _ := store.chargedBurn(at, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory); got != 40 {
		t.Fatalf("burn = %v after uncounted calls, want 40", got)
	}
	// A response that names no bucket is counted as core.
	apiBudgetPacingRecord("hive", "/repos/acme/widgets", http.StatusOK, 6000, reset, "")
	if got, _ := store.chargedBurn(at, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory); got != 241.0/6 {
		t.Fatalf("burn = %v, want 241 calls over 6 minutes", got)
	}
	// Only the last span counts: at 16 minutes every call is older than it.
	at = now.Add(16*time.Minute + time.Second)
	advance(at)
	if got, ok := store.chargedBurn(at, apiBudgetPacingBurnSpan, apiBudgetPacingMinHistory); !ok || got != 0 {
		t.Fatalf("burn = %v ok = %v after a quiet span, want 0", got, ok)
	}
}

func TestObserveAPIBudgetPacingLogsOncePerChangeAndReleasesAtReset(t *testing.T) {
	now := apiBudgetPacingT0
	t.Cleanup(ResetAPIBudgetPacingForTest(func() time.Time { return now }))
	in := apiBudgetPacingIn()

	// Before any call: nothing to pace on.
	if st, changed := ObserveAPIBudgetPacing(in); changed || st.Active || !st.Enabled || st.EffectiveIntervalS != 120 {
		t.Fatalf("empty store: %+v changed=%v", st, changed)
	}

	// 200 calls a minute for ten minutes; the window resets 30 minutes later
	// with 4000 left: 200 × 30 / (4000 − 800) = 1.875.
	reset := apiBudgetPacingT0.Add(40 * time.Minute)
	for i := 0; i < 2000; i++ {
		now = apiBudgetPacingT0.Add(time.Duration(i) * 300 * time.Millisecond)
		apiBudgetPacingRecord("hive:eval_cycle", "/repos/acme/widgets/pulls", http.StatusOK, 5999-i, reset, "core")
	}
	now = apiBudgetPacingT0.Add(10 * time.Minute)
	st, changed := ObserveAPIBudgetPacing(in)
	if !changed || !st.Active || st.Factor != 1.88 || st.EffectiveIntervalS != 226 || st.BurnPerMin != 200 {
		t.Fatalf("engage: %+v changed=%v", st, changed)
	}
	if st.Remaining != 4000 || st.ResetIn != 30*time.Minute {
		t.Fatalf("reading = %d left, reset in %v", st.Remaining, st.ResetIn)
	}
	// The same evidence again is not a change, so it is not logged again.
	if _, changed := ObserveAPIBudgetPacing(in); changed {
		t.Fatal("an unchanged factor was reported as a change")
	}
	if snap := APIBudgetPacingSnapshot(); !snap.Active || snap.Factor != 1.88 || snap.EffectiveIntervalS != 226 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := APIBudgetPacingStretch(); got < 1.88 || got > 1.89 {
		t.Fatalf("stretch = %v, want 226s/120s", got)
	}

	// The window resets before the loop looks again: readers see no stretch.
	now = reset.Add(time.Second)
	if snap := APIBudgetPacingSnapshot(); snap.Active || snap.Factor != 1 || snap.EffectiveIntervalS != 120 || !snap.Enabled {
		t.Fatalf("snapshot after the reset = %+v", snap)
	}
	if got := APIBudgetPacingStretch(); got != 1 {
		t.Fatalf("stretch after the reset = %v, want 1", got)
	}

	// Turning the setting off drops the stretch and reads nothing.
	now = apiBudgetPacingT0.Add(11 * time.Minute)
	in.Enabled = false
	st, changed = ObserveAPIBudgetPacing(in)
	if !changed || st.Enabled || st.Active || st.Factor != 1 || st.EffectiveIntervalS != 120 || st.Reason != apiBudgetPacingOffReason {
		t.Fatalf("off: %+v changed=%v", st, changed)
	}
	if got := APIBudgetPacingStretch(); got != 1 {
		t.Fatalf("stretch while off = %v, want 1", got)
	}
}

func TestAPIBudgetPacingSnapshotBeforeAnyDecision(t *testing.T) {
	t.Cleanup(ResetAPIBudgetPacingForTest(time.Now))
	if snap := APIBudgetPacingSnapshot(); snap.Enabled || snap.Active || snap.Factor != 1 || snap.Reason != apiBudgetPacingOffReason {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := APIBudgetPacingStretch(); got != 1 {
		t.Fatalf("stretch = %v, want 1", got)
	}
}
