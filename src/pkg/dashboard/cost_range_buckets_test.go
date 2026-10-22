package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// costRangeScript extracts the Cost section's window/bucket/run-rate code
// (constants through costTrendHtml) from index.html so node can execute it
// against a synthetic history.
func costRangeScript(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, "    const COST_HOUR_MS = ")
	if start < 0 {
		t.Fatal("index.html does not define COST_HOUR_MS")
	}
	body, ok := jsFuncOptional(html, "costTrendHtml")
	fn := strings.Index(html[start:], body)
	if !ok || fn < 0 {
		t.Fatal("index.html does not define costTrendHtml after COST_HOUR_MS")
	}
	return html[start : start+fn+len(body)]
}

// TestCostRangeBucketsFollowSelector pins the Cost section's window model:
// the bar chart, "spend in selected range" and the run-rate are all derived
// from the ONE window the selector names — hourly = 24 hour bars, daily = 14
// day bars (M/D labels once there are more than 7), weekly = 5 week bars,
// monthly = 5 month bars, custom = hour or day bars by span — each count
// adjustable with the −/+ stepper within named bounds, and the run-rate is
// spend ÷ elapsed window in the bucket's unit.
func TestCostRangeBucketsFollowSelector(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — cost range bucket behaviour was NOT executed by this run")
	}
	html := indexHTML(t)

	script := "const assert = require('node:assert/strict');\n" +
		"process.env.TZ = 'UTC';\n" +
		"let _costHistory = [];\n" +
		"let _lastCost = null;\n" +
		"const localStorage = { _m: {}, getItem(k){ return k in this._m ? this._m[k] : null; }, setItem(k, v){ this._m[k] = String(v); } };\n" +
		"function esc(s){ return String(s); }\n" +
		"function fmtUSD(n){ return '$' + Number(n).toFixed(2); }\n" +
		"function formatDashboardDateTime(v){ return new Date(v).toISOString(); }\n" +
		"function renderSparkline(){ return ''; }\n" +
		"function fmtSparkVal(v){ return String(v); }\n" +
		"function capDashboardArray(a){ return a; }\n" +
		costRangeScript(t, html) + "\n" +
		`
// Wed 2026-10-21 12:30 UTC.
const NOW = Date.UTC(2026, 9, 21, 12, 30, 0);
const H = 3600e3, D = 24 * H;
// Synthetic cumulative history: one model spending exactly $1 per hour for
// the last 40 days, observed once an hour on the hour (sparser than the
// accrual-liveness gap, so each $1 lands in the bucket it was observed in),
// priced at $1 per 1000 tokens.
const HIST_START = NOW - 40 * D - 30 * 60e3;
_costHistory = [];
for (let t = HIST_START; t <= NOW; t += H) {
  const hours = (t - HIST_START) / H;
  _costHistory.push({ t, usd: hours, models: { m: { i: hours * 1000, o: 0, usd: hours } } });
}

function setRange(k){ _costRange = k; }
const approx = (a, b, tol, msg) => assert.ok(Math.abs(a - b) <= tol, msg + ': ' + a + ' vs ' + b);
const sum = (bs) => bs.filter(b => b.hasData).reduce((s, b) => s + b.spend, 0);

// Defaults and stepper bounds.
assert.deepEqual(Object.keys(COST_RANGE_COUNT), ['hourly', 'daily', 'weekly', 'monthly']);
assert.equal(costRangeCount('hourly'), 24);
assert.equal(costRangeCount('daily'), 14);
assert.equal(costRangeCount('weekly'), 5);
assert.equal(costRangeCount('monthly'), 5);
assert.equal(costRangeCount('custom'), null, 'custom has no count');
for (const k in COST_RANGE_COUNT) {
  const c = COST_RANGE_COUNT[k];
  assert.ok(c.min <= c.def && c.def <= c.max && c.step > 0, k + ' bounds bracket the default');
  assert.equal((c.def - c.min) % c.step, 0, k + ' default sits on the step grid');
}
assert.equal(costClampCount('daily', 100), 60, 'clamps above max');
assert.equal(costClampCount('daily', 1), 7, 'clamps below min');
assert.equal(costClampCount('daily', 16), 14, 'snaps onto the 7-day grid');
assert.equal(costClampCount('hourly', NaN), 24, 'garbage falls back to the default');

// hourly: 24 one-hour bars, the last one partial (clock hour → now).
setRange('hourly');
let win = costWindow(NOW);
assert.equal(win.count, 24, 'hourly bucket count');
assert.equal(win.bucket, 'hour');
assert.equal(win.start, Date.UTC(2026, 9, 20, 13, 0, 0), 'hourly window start');
assert.equal(win.end, NOW);
let buckets = costBuckets(win);
assert.deepEqual(buckets.slice(0, 3).map(b => costBucketAxisLabel(b, win)), ['13', '14', '15']);
assert.equal(costBucketAxisLabel(buckets[23], win), '12');
let total = sum(buckets);
assert.equal(total, 23, 'hourly spend: 23 observed $1 steps inside (13:00, 12:30]');
let rate = costWindowRunRate(total, win);
assert.equal(costRunRateUnit(win).label, '/hr');
approx(rate * H, 1, 0.05, 'hourly run-rate ≈ $1/hr');
// Stepper: +6h twice → 36 bars; −6h back to 30; floor/ceiling respected.
assert.equal(costStepCount(1), true);
assert.equal(costStepCount(1), true);
assert.equal(costRangeCount(), 36);
assert.equal(costWindow(NOW).count, 36, 'hourly window follows the count');
assert.equal(costWindow(NOW).start, Date.UTC(2026, 9, 20, 1, 0, 0));
assert.equal(costStepCount(-1), true);
assert.equal(costRangeCount(), 30);
assert.equal(JSON.parse(localStorage.getItem('hiveCostRangeCounts')).hourly, 30, 'count persisted per range');
for (let i = 0; i < 20; i++) costStepCount(-1);
assert.equal(costRangeCount(), 6, 'hourly floor');
assert.equal(costStepCount(-1), false, 'at the floor the step is a no-op');
for (let i = 0; i < 20; i++) costStepCount(1);
assert.equal(costRangeCount(), 72, 'hourly ceiling');
_costCounts.hourly = 24;

// daily: last 14 calendar days incl. today, labelled by M/D (14 weekday
// names would repeat).
setRange('daily');
win = costWindow(NOW);
assert.equal(win.count, 14, 'daily bucket count');
assert.equal(win.start, Date.UTC(2026, 9, 8), 'daily window starts at local midnight 13 days ago');
buckets = costBuckets(win);
assert.deepEqual(buckets.slice(0, 2).map(b => costBucketAxisLabel(b, win)), ['10/8', '10/9']);
assert.equal(costBucketAxisLabel(buckets[13], win), '10/21');
total = sum(buckets);
assert.equal(total, 13 * 24 + 12, 'daily spend: $1 steps inside the 14 calendar days');
rate = costWindowRunRate(total, win);
assert.equal(costRunRateUnit(win).label, '/day');
approx(rate * D, 24, 0.5, 'daily run-rate ≈ $24/day');
assert.equal((costAxisLabelsHtml(buckets, win).match(/<span>/g) || []).length, 5, '14 bars get 5 evenly spaced labels');
// Shrunk to 7 days the weekday labels come back, unambiguous again.
assert.equal(costStepCount(-1), true);
win = costWindow(NOW);
assert.equal(win.count, 7);
buckets = costBuckets(win);
assert.deepEqual(buckets.map(b => costBucketAxisLabel(b, win)), ['Th', 'F', 'Sa', 'Su', 'M', 'Tu', 'W']);
assert.equal((costAxisLabelsHtml(buckets, win).match(/<span>/g) || []).length, 7, '7 bars label every bar');
assert.equal(costStepCount(-1), false, 'daily floor is 7');
_costCounts.daily = 14;

// weekly: the current Mon→Sun week plus the 4 before it, one bar per week
// labelled by its Monday; the rest of this week carries no data.
setRange('weekly');
win = costWindow(NOW);
assert.equal(win.count, 5, 'weekly bucket count');
assert.equal(win.bucket, 'week');
assert.equal(win.start, Date.UTC(2026, 8, 21), 'first week starts on a Monday 4 weeks back');
assert.equal(win.end, Date.UTC(2026, 9, 26), 'window ends next Monday 00:00');
buckets = costBuckets(win);
assert.deepEqual(buckets.map(b => costBucketAxisLabel(b, win)), ['9/21', '9/28', '10/5', '10/12', '10/19']);
assert.deepEqual(buckets.map(b => b.hasData), [true, true, true, true, true]);
total = sum(buckets);
assert.equal(total, 4 * 7 * 24 + 2 * 24 + 12, 'weekly spend: four full weeks + Mon, Tue and half of Wed');
assert.equal(win.elapsedMs, 4 * 7 * D + 2 * D + 12.5 * H, 'elapsed stops at now, not at the end of the week');
rate = costWindowRunRate(total, win);
assert.equal(costRunRateUnit(win).label, '/day');
approx(rate * D, 24, 0.5, 'weekly run-rate is $/day over elapsed time');
assert.equal((costAxisLabelsHtml(buckets, win).match(/<span>/g) || []).length, 5, 'weekly axis labels every bar');
assert.equal(costStepCount(1), true);
assert.equal(costWindow(NOW).count, 6);
assert.equal(costWindow(NOW).start, Date.UTC(2026, 8, 14));
_costCounts.weekly = 5;

// monthly: the current calendar month plus the 4 before it, one bar per
// month labelled by name. History only reaches 40 days back, so the three
// oldest months have no data at all.
setRange('monthly');
win = costWindow(NOW);
assert.equal(win.count, 5, 'monthly bucket count');
assert.equal(win.bucket, 'month');
assert.equal(win.start, Date.UTC(2026, 5, 1));
assert.equal(win.end, Date.UTC(2026, 10, 1));
buckets = costBuckets(win);
assert.deepEqual(buckets.map(b => costBucketAxisLabel(b, win)), ['Jun', 'Jul', 'Aug', 'Sep', 'Oct']);
assert.deepEqual(buckets.map(b => b.hasData), [false, false, false, true, true], 'months before history are uncovered');
total = sum(buckets);
assert.equal(total, 40 * 24, 'monthly spend: everything since history began');
approx(costWindowRunRate(total, win) * D, 40 * 24 / (142 + 12.5 / 24), 0.5, 'monthly run-rate in $/day over the elapsed Jun 1 → now');
assert.equal((costAxisLabelsHtml(buckets, win).match(/<span>/g) || []).length, 5);

// Daily rollup (from /api/cost) backs buckets older than the snapshots:
// $10/day for Jun 1 → Sep 10 UTC, every day ending before the first snapshot.
const roll = [];
for (let d = Date.UTC(2026, 5, 1); d < Date.UTC(2026, 8, 11); d += D) {
  roll.push({ day: new Date(d).toISOString().slice(0, 10), cost_usd: 10, input_tokens: 1000, output_tokens: 100, samples: 288 });
}
// A day the snapshots already cover must NOT be double counted, and a day
// with no samples is not data.
roll.push({ day: '2026-10-01', cost_usd: 999, samples: 288 });
roll.push({ day: '2026-05-31', cost_usd: 999, samples: 0 });
_lastCost = { daily_rollup: roll };
assert.equal(costHistoryStart(), Date.UTC(2026, 5, 1), 'history starts at the first rollup day');
buckets = costBuckets(win);
assert.deepEqual(buckets.map(b => b.hasData), [true, true, true, true, true], 'rollup covers the older months');
assert.deepEqual(buckets.map(b => b.rollup), [true, true, true, true, false], 'September mixes rollup days and snapshots');
assert.equal(buckets[0].spend, 300, 'June: 30 rollup days × $10');
assert.equal(buckets[1].spend, 310, 'July: 31 rollup days × $10');
// September: rollup Sep 1–10 ($100) + snapshots from Sep 11 12:30 → Oct 1 ($1/hr).
assert.equal(buckets[3].spend, 100 + Math.floor((Date.UTC(2026, 9, 1) - HIST_START) / H), 'September sums rollup days before the snapshots with the snapshots');
assert.equal(buckets[4].spend, 20 * 24 + 12, 'October ignores the rollup for a day the snapshots cover');

// custom: hour buckets up to 2 days, day buckets beyond.
_lastCost = null;
setRange('custom');
_costCustom = { start: NOW - 36 * H, end: NOW };
win = costWindow(NOW);
assert.equal(win.bucket, 'hour');
assert.equal(win.count, 36);
assert.equal(costRunRateUnit(win).label, '/hr');
_costCustom = { start: NOW - 5 * D, end: NOW };
win = costWindow(NOW);
assert.equal(win.bucket, 'day');
assert.equal(win.count, 5);
assert.equal(costRunRateUnit(win).label, '/day');
_costCustom = { start: NOW - 2 * D - 1, end: NOW };
assert.equal(costWindow(NOW).bucket, 'day', 'just over 2 days switches to day buckets');
assert.equal(costStepCount(1), false, 'custom has no stepper');

// Run-rate guards.
assert.equal(costWindowRunRate(0, win), null, 'no spend → no run-rate');
assert.equal(costWindowRunRate(5, { elapsedMs: 0 }), null, 'no elapsed time → no run-rate');

// The rendered summary carries both values and the chart switches with the selector.
setRange('weekly');
let out = costTrendHtml(NOW);
assert.match(out, /spend in selected range: <b>\$73[0-9]\.[0-9]{2}<\/b>/, 'weekly summary spend (4 weeks + 2.5 days at $24/day)');
assert.match(out, /run-rate: <b>~\$2[34]\.[0-9]{2}\/day<\/b>/, 'weekly run-rate per day');
assert.equal((out.match(/class="cost-bar( zero)?"/g) || []).length, 5, 'weekly chart has 5 bars');
assert.match(out, /class="cost-range-step" role="group"/, 'stepper rendered');
assert.match(out, /data-cost-step="-1"[^>]*aria-label="Shrink weekly window by 1 weeks"/, 'shrink chip labelled');
assert.match(out, /data-cost-step="1"[^>]*aria-label="Grow weekly window by 1 weeks"/, 'grow chip labelled');
assert.match(out, /class="cost-range-step-val"[^>]*>5&thinsp;wk</, 'count with unit');
assert.ok(!out.includes('history starts'), 'no horizon note when every bar is covered');
setRange('hourly');
out = costTrendHtml(NOW);
assert.match(out, /run-rate: <b>~\$[01]\.[0-9]{2}\/hr<\/b>/, 'hourly run-rate per hour');
assert.equal((out.match(/class="cost-bar( zero)?"/g) || []).length, 24, 'hourly chart has 24 bars');
assert.ok(!out.includes('(est.)'), 'no run-rate-extrapolated estimate marker');
assert.match(out, />24&thinsp;h</, 'hourly count');
// Monthly without a rollup: three hatched no-data bars and a horizon note.
setRange('monthly');
out = costTrendHtml(NOW);
assert.equal((out.match(/class="cost-bar nodata"/g) || []).length, 3, 'months before history draw hatched');
assert.match(out, /history starts 9\/11/, 'summary names the first data point');
// Stepper at a bound disables the matching chip.
_costCounts.monthly = COST_RANGE_COUNT.monthly.max;
out = costTrendHtml(NOW);
assert.match(out, /data-cost-step="1"[^>]*disabled/, 'grow disabled at the ceiling');
assert.ok(!/data-cost-step="-1"[^>]*disabled/.test(out), 'shrink still enabled at the ceiling');
setRange('custom');
assert.ok(!costTrendHtml(NOW).includes('cost-range-step'), 'custom range shows no stepper');
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(cmd.Environ(), "TZ=UTC")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node cost range bucket check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// TestCostRangeSummaryAndChartShareWindow pins that costTrendHtml derives the
// bars, the spend and the run-rate from one costWindow() call instead of a
// fixed trailing window.
func TestCostRangeSummaryAndChartShareWindow(t *testing.T) {
	html := indexHTML(t)
	fn := jsFunc(t, html, "costTrendHtml")
	for _, want := range []string{
		"const win = costWindow(now);",
		"costBuckets(win)",
		"costWindowRunRate(total, win)",
		"costRunRateUnit(win)",
		"costAxisLabelsHtml(buckets, win)",
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("costTrendHtml should contain %q", want)
		}
	}
	for _, stale := range []string{"costRunRate()", "cfg.windowMs", "(est.)"} {
		if strings.Contains(fn, stale) {
			t.Errorf("costTrendHtml still uses %q", stale)
		}
	}
	if strings.Contains(html, "function costRunRate()") {
		t.Error("fixed-trailing-window costRunRate() should be gone")
	}
}

// TestCostDiscontinuityGuardIsGapAware pins #13482: a step above the fixed
// per-step ceiling is still spend when a plausible rate over the observation
// gap explains it (the sampler was simply not running), while the same step
// at normal cadence remains a correction. Mirrors costRollupStepAllowed.
func TestCostDiscontinuityGuardIsGapAware(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — cost discontinuity behaviour was NOT executed by this run")
	}
	html := indexHTML(t)

	script := "const assert = require('node:assert/strict');\n" +
		"process.env.TZ = 'UTC';\n" +
		"let _costHistory = [];\n" +
		"let _lastCost = null;\n" +
		"const localStorage = { _m: {}, getItem(k){ return k in this._m ? this._m[k] : null; }, setItem(k, v){ this._m[k] = String(v); } };\n" +
		"function esc(s){ return String(s); }\n" +
		"function fmtUSD(n){ return '$' + Number(n).toFixed(2); }\n" +
		"function formatDashboardDateTime(v){ return new Date(v).toISOString(); }\n" +
		"function renderSparkline(){ return ''; }\n" +
		"function fmtSparkVal(v){ return String(v); }\n" +
		"function capDashboardArray(a){ return a; }\n" +
		costRangeScript(t, html) + "\n" +
		`
const M = 60e3, H = 3600e3;
const T0 = Date.UTC(2026, 8, 1, 0, 0, 0);
// $40 every 5 minutes, 24 samples → median step $40 ($800 fixed ceiling); ≈$20/h average → ≈$399/h plausible rate.
const hist = [];
for (let i = 0; i < 24; i++) hist.push({ timestamp: T0 + i * 5 * M, usd: 40 * i, models: { m: { i: 1000 * i, o: 0, usd: 40 * i } } });
const lastUsd = 40 * 23, lastTok = 1000 * 23;
const gapEnd = T0 + 48 * H;
// 48 h silence then +$10,000 (gap-explained: 48 h × ≈$399/h ≈ $19,100 ≥ $10,000).
hist.push({ timestamp: gapEnd, usd: lastUsd + 10000, models: { m: { i: lastTok + 250000, o: 0, usd: lastUsd + 10000 } } });
// 5 min later another +$10,000 — a correction.
hist.push({ timestamp: gapEnd + 5 * M, usd: lastUsd + 20000, models: { m: { i: lastTok + 500000, o: 0, usd: lastUsd + 20000 } } });
// then an ordinary +$40.
hist.push({ timestamp: gapEnd + 10 * M, usd: lastUsd + 20040, models: { m: { i: lastTok + 501000, o: 0, usd: lastUsd + 20040 } } });

const rate = costPriceMap(hist);
const ceiling = costDiscontinuityCeiling(hist, rate);
assert.equal(ceiling, 800, 'fixed ceiling = 20 × $40 median');
const ratePerMs = costDiscontinuityRate(hist, rate);
const perHour = ratePerMs * H;
// Ordinary spend $920 (+$40 after the gap = $960) over the 48h10m span, ×20.
const expectPerHour = 20 * 960 / (48 + 10 / 60);
assert.ok(Math.abs(perHour - expectPerHour) < 1e-6, 'rate ceiling = 20 × average $/h, got ' + perHour + ' want ' + expectPerHour);

assert.equal(costStepAllowed(500, 1, 800), true, 'under the fixed ceiling');
assert.equal(costStepAllowed(10000, 5 * M, { ceiling, ratePerMs }), false, '$10,000 in 5 min is a correction');
assert.equal(costStepAllowed(10000, 48 * H, { ceiling, ratePerMs }), true, '$10,000 over 48 h is spend');
assert.equal(costStepAllowed(10000, 0, { ceiling, ratePerMs }), false, 'zero gap cannot explain it');
assert.equal(costStepAllowed(10000, 0, { ceiling, ratePerMs: Infinity }), true, 'no rate baseline → nothing dropped by rate');
assert.equal(costStepAllowed(10000, 5 * M, 800), false, 'legacy numeric cap: fixed ceiling only');

const cap = { ceiling, ratePerMs };
const spend = costSpendBetween(hist, rate, T0, gapEnd + 10 * M, cap);
assert.equal(spend, 40 * 23 + 10000 + 40, 'window spend keeps the gap-explained step and drops the instant one');
const legacy = costSpendBetween(hist, rate, T0, gapEnd + 10 * M, ceiling);
assert.equal(legacy, 40 * 23 + 40, 'the old fixed ceiling alone dropped both big steps');

const spans = costAccrualSpans(hist, rate);
const big = spans.filter(s => s.amount > 800);
assert.equal(big.length, 2);
assert.equal(big[0].gap, 48 * H - 23 * 5 * M, 'span records the observation gap');
assert.equal(big[1].gap, 5 * M);
const spread = costSpreadSpendBetween(spans, T0, gapEnd + 10 * M, cap);
assert.ok(Math.abs(spread - (40 * 23 + 10000 + 40)) < 1e-6, 'spread spend matches: ' + spread);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cost discontinuity JS failed: %v\n%s", err, out)
	}
}
