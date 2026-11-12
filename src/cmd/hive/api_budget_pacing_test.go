package main

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// apiBudgetPacingSeed gives the pacer a store that has seen 200 of the
// hive's own calls a minute for ten minutes, in a window with `remaining`
// calls left that resets `toReset` from now.
func apiBudgetPacingSeed(t *testing.T, remaining int, toReset time.Duration) {
	t.Helper()
	start := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	now := start
	t.Cleanup(github.ResetAPIBudgetPacingForTest(func() time.Time { return now }))
	reset := start.Add(10*time.Minute + toReset)
	const calls = 2000
	for i := 0; i < calls; i++ {
		now = start.Add(time.Duration(i) * 300 * time.Millisecond)
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "7000")
		h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining+calls-1-i))
		h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		h.Set("X-RateLimit-Resource", "core")
		github.RecordRESTRequest("hive:eval_cycle", http.MethodGet, "/repos/acme/widgets/pulls", http.StatusOK, h)
	}
	now = start.Add(10 * time.Minute)
}

func apiBudgetPacingConfig(base, maxS, webhookS int, pacing bool) *config.Config {
	cfg := &config.Config{}
	cfg.Governor.EvalIntervalS = base
	cfg.Governor.EvalIntervalMaxS = maxS
	cfg.Governor.EvalIntervalWebhookS = webhookS
	cfg.Governor.ConserveIntervalMultiplier = 2
	cfg.Governor.APIBudgetPacing = pacing
	return cfg
}

// With the setting off the loop interval is today's, whatever the budget
// evidence says: here the evidence would stretch a paced loop.
func TestEvalIntervalWithPacingOffIsTodaysInterval(t *testing.T) {
	apiBudgetPacingSeed(t, 4000, 30*time.Minute)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	for _, tc := range []struct{ base, maxS, webhookS int }{
		{120, 1800, 900}, {300, 1800, 900}, {600, 600, 0}, {30, 1800, 900}, {0, 0, 0},
	} {
		cfg := apiBudgetPacingConfig(tc.base, tc.maxS, tc.webhookS, false)
		want := webhookAwareEvalInterval(apiBudgetIntervalForConfig(cfg, nil), tc.webhookS, tc.maxS, false)
		got, mode := evalIntervalAndModeForConfig(cfg, nil, nil)
		if got != want || mode != evalModePoll {
			t.Errorf("%+v: interval = %v mode = %q, want %v poll", tc, got, mode, want)
		}
		if st := github.APIBudgetPacingSnapshot(); st.Enabled || st.Active || st.Factor != 1 {
			t.Errorf("%+v: pacing state with the setting off = %+v", tc, st)
		}
	}
	// The same evidence with the setting on does stretch the loop, so the
	// table above is not passing for want of evidence.
	if got := evalIntervalForConfig(apiBudgetPacingConfig(120, 1800, 900, true), nil, nil); got != 226*time.Second {
		t.Fatalf("pacing on: interval = %v, want 226s (120s × 1.88)", got)
	}
	// And off again releases it at once.
	if got := evalIntervalForConfig(apiBudgetPacingConfig(120, 1800, 900, false), nil, nil); got != 120*time.Second {
		t.Fatalf("pacing turned off: interval = %v, want 120s", got)
	}
	// While healthy webhooks set the cadence, off is still today's interval.
	github.RecordWebhookEventForTest("acme/widgets")
	if got, mode := evalIntervalAndModeForConfig(apiBudgetPacingConfig(120, 1800, 900, false), nil, nil); got != 900*time.Second || mode != evalModeWebhook {
		t.Fatalf("webhook cadence, pacing off: interval = %v mode = %q, want 900s webhook", got, mode)
	}
}

// Pacing on: the loop stretches, logs the change once (not once per cycle),
// and the status block carries the state.
func TestEvalIntervalWithPacingOnStretchesAndLogsOnce(t *testing.T) {
	apiBudgetPacingSeed(t, 4000, 30*time.Minute)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	cfg := apiBudgetPacingConfig(120, 1800, 900, true)
	cfg.GitHub.APIReserve = 800
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))

	// 200 × 30 / (4000 − 800) = 1.875, held as 1.88: 120s becomes 226s.
	for i := 0; i < 3; i++ {
		got, mode := evalIntervalAndModeForConfig(cfg, nil, logger)
		if got != 226*time.Second || mode != evalModePoll {
			t.Fatalf("call %d: interval = %v mode = %q, want 226s poll", i, got, mode)
		}
	}
	if n := strings.Count(sb.String(), "github api budget pacing changed"); n != 1 {
		t.Fatalf("want one pacing INFO line for one change, got %d in %s", n, sb.String())
	}
	for _, want := range []string{"level=INFO", "active=true", "factor=1.88", "remaining=4000", "reset_in_s=1800", "burn_per_min=200", "unpaced_interval_s=120", "effective_interval_s=226"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("pacing log lacks %s: %s", want, sb.String())
		}
	}

	// /api/status: the eval interval is the paced one, and the pacing block
	// says why.
	loop := &evalLoopState{}
	loop.setInterval(226*time.Second, evalModePoll)
	status := loop.snapshot()
	if status.IntervalS != 226 || status.APIBudgetPacing == nil {
		t.Fatalf("status = %+v", status)
	}
	if p := status.APIBudgetPacing; !p.Enabled || !p.Active || p.Factor != 1.88 || p.EffectiveIntervalS != 226 || p.BurnPerMin != 200 || !strings.Contains(p.Reason, "would run out before the reset") {
		t.Fatalf("status pacing = %+v", *p)
	}

	// The reserve comes from github.api_reserve: a larger reserve leaves less
	// to spend, so the same evidence needs a longer stretch.
	// 200 × 30 / (4000 − 2000) = 3: outside the held factor's band.
	cfg.GitHub.APIReserve = 2000
	if got := evalIntervalForConfig(cfg, nil, logger); got != 360*time.Second {
		t.Fatalf("reserve 2000: interval = %v, want 360s (120s × 3)", got)
	}
	if n := strings.Count(sb.String(), "github api budget pacing changed"); n != 2 {
		t.Fatalf("want a second pacing INFO line for the re-sized factor, got %d", n)
	}

	// Turning it off is logged too, and the interval is today's again.
	cfg.Governor.APIBudgetPacing = false
	if got := evalIntervalForConfig(cfg, nil, logger); got != 120*time.Second {
		t.Fatalf("pacing off: interval = %v, want 120s", got)
	}
	if n := strings.Count(sb.String(), "github api budget pacing changed"); n != 3 || !strings.Contains(sb.String(), "active=false") {
		t.Fatalf("want a third pacing INFO line for the release, got %d in %s", n, sb.String())
	}
}

// Pacing composes with today's interval and never shortens it: in normal,
// conserve and critical budget mode, on the polling and the webhook cadence,
// with a healthy budget, a projected exhaustion and a budget under the
// reserve.
func TestAPIBudgetPacingNeverShortensTodaysInterval(t *testing.T) {
	for _, reading := range []struct {
		name      string
		remaining int
		toReset   time.Duration
		stretches bool
	}{
		{"healthy", 6900, 20 * time.Minute, false},
		{"projected exhaustion", 4000, 30 * time.Minute, true},
		{"under the reserve", 500, 50 * time.Minute, true},
		{"minutes before the reset", 900, 3 * time.Minute, true},
	} {
		for _, mode := range []github.APIBudgetMode{github.APIBudgetNormal, github.APIBudgetConserve, github.APIBudgetCritical} {
			for _, webhooks := range []bool{false, true} {
				for _, base := range []int{60, 120, 300, 900} {
					const maxS, webhookS = 1800, 900
					today := webhookAwareEvalInterval(apiBudgetEffectiveEvalInterval(base, maxS, 2, mode), webhookS, maxS, webhooks)
					name := reading.name + "/" + mode.String() + "/webhooks=" + strconv.FormatBool(webhooks) + "/base=" + strconv.Itoa(base)

					apiBudgetPacingSeed(t, reading.remaining, reading.toReset)
					off := apiBudgetPacedEvalInterval(apiBudgetPacingConfig(base, maxS, webhookS, false), today, nil)
					if off != today {
						t.Errorf("%s: pacing off changed the interval: %v, want %v", name, off, today)
					}
					paced := apiBudgetPacedEvalInterval(apiBudgetPacingConfig(base, maxS, webhookS, true), today, nil)
					if paced < today {
						t.Errorf("%s: pacing shortened the interval: %v < %v", name, paced, today)
					}
					if paced > maxS*time.Second {
						t.Errorf("%s: paced interval %v is over eval_interval_max_s", name, paced)
					}
					if untilReset := reading.toReset + 5*time.Second; paced > today && paced > untilReset {
						t.Errorf("%s: paced interval %v sleeps past the reset in %v", name, paced, reading.toReset)
					}
					// A loop already at the cap, or already slower than the
					// time left in the window, has nothing left to stretch.
					room := today < maxS*time.Second && today < reading.toReset
					if stretched := paced > today; stretched != (reading.stretches && room) {
						t.Errorf("%s: today %v paced %v, stretched = %v, want %v", name, today, paced, stretched, reading.stretches && room)
					}
				}
			}
		}
	}
}

// A nil config keeps the interval, as before.
func TestAPIBudgetPacedEvalIntervalNilConfig(t *testing.T) {
	if got := apiBudgetPacedEvalInterval(nil, 300*time.Second, nil); got != 300*time.Second {
		t.Fatalf("nil config: interval = %v, want 300s", got)
	}
}

func TestAPIBudgetStretchedSweepInterval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		d       time.Duration
		stretch float64
		maxS    int
		want    time.Duration
	}{
		{"not pacing", time.Minute, 1, 1800, time.Minute},
		{"a stretch under 1 is ignored", time.Minute, 0.5, 1800, time.Minute},
		{"the eval loop's ratio applies", time.Minute, 226.0 / 120.0, 1800, 113 * time.Second},
		{"capped at eval_interval_max_s", 15 * time.Minute, 3, 1800, 30 * time.Minute},
		{"a sweep slower than the cap keeps its interval", time.Hour, 2, 1800, time.Hour},
		{"no cap configured", 15 * time.Minute, 3, 0, 45 * time.Minute},
		{"a zero interval stays zero", 0, 3, 1800, 0},
	} {
		if got := apiBudgetStretchedSweepInterval(tc.d, tc.stretch, tc.maxS); got != tc.want {
			t.Errorf("%s: interval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The sweep workers run on their own intervals, so each is stretched by the
// eval loop's ratio while pacing holds one, and left alone otherwise.
func TestSweepIntervalFollowsAPIBudgetPacing(t *testing.T) {
	apiBudgetPacingSeed(t, 4000, 30*time.Minute)
	t.Cleanup(github.ResetWebhookTrackerForTest())
	minute := apiBudgetPacedSweepInterval(func(*config.Config) time.Duration { return time.Minute })
	on, off := apiBudgetPacingConfig(120, 1800, 900, true), apiBudgetPacingConfig(120, 1800, 900, false)

	// Nothing decided yet: no stretch.
	if got := minute(on); got != time.Minute {
		t.Fatalf("before the loop decided: sweep interval = %v, want 1m", got)
	}
	if got := evalIntervalForConfig(on, nil, nil); got != 226*time.Second {
		t.Fatalf("eval interval = %v, want 226s", got)
	}
	if got := minute(on); got != 113*time.Second {
		t.Fatalf("paced sweep interval = %v, want 113s (1m × 226/120)", got)
	}
	// A config snapshot with the setting off is never stretched, even while
	// the pacer holds a factor.
	if got := minute(off); got != time.Minute {
		t.Fatalf("setting off: sweep interval = %v, want 1m", got)
	}
	// Every sweep job goes through the wrapper unchanged while not pacing.
	b := &boot{}
	evalIntervalForConfig(off, nil, nil)
	for _, job := range b.sweepJobs() {
		if got, want := apiBudgetPacedSweepInterval(job.interval)(on), job.interval(on); got != want {
			t.Errorf("sweep %s: interval = %v while not pacing, want %v", job.name, got, want)
		}
	}
}
