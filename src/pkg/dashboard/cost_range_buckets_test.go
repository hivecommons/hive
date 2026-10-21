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

// TestCostRangeBucketsFollowSelector pins the fix for the Cost section: the
// bar chart, "spend in selected range" and the run-rate are all derived from
// the ONE window the selector names — hourly = 24 hour bars, daily/weekly =
// 7 weekday bars, monthly = one bar per calendar day, custom = hour or day
// bars by span — and the run-rate is spend ÷ elapsed window in the bucket's
// unit. Before, weekly/monthly drew 12 multi-week bars over 84/360 days and
// the run-rate came from a fixed trailing 6h regardless of the selector.
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
_costHistory = [];
for (let t = NOW - 40 * D - 30 * 60e3; t <= NOW; t += H) {
  const hours = (t - (NOW - 40 * D - 30 * 60e3)) / H;
  _costHistory.push({ t, usd: hours, models: { m: { i: hours * 1000, o: 0, usd: hours } } });
}

function setRange(k){ _costRange = k; }
const approx = (a, b, tol, msg) => assert.ok(Math.abs(a - b) <= tol, msg + ': ' + a + ' vs ' + b);

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
let total = buckets.filter(b => b.hasData).reduce((s, b) => s + b.spend, 0);
assert.equal(total, 23, 'hourly spend: 23 observed $1 steps inside (13:00, 12:30]');
let rate = costWindowRunRate(total, win);
assert.equal(costRunRateUnit(win).label, '/hr');
approx(rate * H, 1, 0.05, 'hourly run-rate ≈ $1/hr');

// daily: last 7 calendar days incl. today, labelled by weekday.
setRange('daily');
win = costWindow(NOW);
assert.equal(win.count, 7, 'daily bucket count');
assert.equal(win.start, Date.UTC(2026, 9, 15), 'daily window starts at local midnight 6 days ago');
buckets = costBuckets(win);
assert.deepEqual(buckets.map(b => costBucketAxisLabel(b, win)), ['Th', 'F', 'Sa', 'Su', 'M', 'Tu', 'W']);
total = buckets.filter(b => b.hasData).reduce((s, b) => s + b.spend, 0);
assert.equal(total, 6 * 24 + 12, 'daily spend: $1 steps inside the 7 calendar days');
rate = costWindowRunRate(total, win);
assert.equal(costRunRateUnit(win).label, '/day');
approx(rate * D, 24, 0.5, 'daily run-rate ≈ $24/day');

// weekly: the current calendar week Mon→Sun, one bar per day; days ahead empty.
setRange('weekly');
win = costWindow(NOW);
assert.equal(win.count, 7, 'weekly bucket count');
assert.equal(win.start, Date.UTC(2026, 9, 19), 'week starts Monday');
assert.equal(win.end, Date.UTC(2026, 9, 26), 'week ends next Monday 00:00');
buckets = costBuckets(win);
assert.deepEqual(buckets.map(b => costBucketAxisLabel(b, win)), ['M', 'Tu', 'W', 'Th', 'F', 'Sa', 'Su']);
assert.deepEqual(buckets.map(b => b.hasData), [true, true, true, false, false, false, false], 'future days carry no data');
total = buckets.filter(b => b.hasData).reduce((s, b) => s + b.spend, 0);
assert.equal(total, 2 * 24 + 12, 'weekly spend: Mon + Tue + half of Wed');
assert.equal(win.elapsedMs, 2 * D + 12.5 * H, 'elapsed stops at now, not at the end of the week');
rate = costWindowRunRate(total, win);
approx(rate * D, 24, 0.5, 'weekly run-rate is $/day over elapsed time');
const axis = costAxisLabelsHtml(buckets, win);
assert.equal((axis.match(/<span>/g) || []).length, 7, 'weekly axis labels every bar');

// monthly: calendar month, one bar per day, labelled by date.
setRange('monthly');
win = costWindow(NOW);
assert.equal(win.count, 31, 'October has 31 day buckets');
assert.equal(win.start, Date.UTC(2026, 9, 1));
assert.equal(win.end, Date.UTC(2026, 10, 1));
buckets = costBuckets(win);
assert.equal(costBucketAxisLabel(buckets[0], win), '10/1');
assert.equal(costBucketAxisLabel(buckets[30], win), '10/31');
assert.equal((costAxisLabelsHtml(buckets, win).match(/<span>/g) || []).length, 3, 'wide charts label first/mid/last');
total = buckets.filter(b => b.hasData).reduce((s, b) => s + b.spend, 0);
assert.equal(total, 20 * 24 + 12, 'monthly spend: Oct 1 through now');
approx(costWindowRunRate(total, win) * D, 24, 0.5, 'monthly run-rate in $/day');

// custom: hour buckets up to 2 days, day buckets beyond.
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

// Run-rate guards.
assert.equal(costWindowRunRate(0, win), null, 'no spend → no run-rate');
assert.equal(costWindowRunRate(5, { elapsedMs: 0 }), null, 'no elapsed time → no run-rate');

// The rendered summary carries both values and the chart switches with the selector.
setRange('weekly');
let out = costTrendHtml(NOW);
assert.match(out, /spend in selected range: <b>\$6[0-9]\.[0-9]{2}<\/b>/, 'weekly summary spend');
assert.match(out, /run-rate: <b>~\$2[34]\.[0-9]{2}\/day<\/b>/, 'weekly run-rate per day');
assert.equal((out.match(/class="cost-bar( zero)?"/g) || []).length, 7, 'weekly chart has 7 bars');
setRange('hourly');
out = costTrendHtml(NOW);
assert.match(out, /run-rate: <b>~\$[01]\.[0-9]{2}\/hr<\/b>/, 'hourly run-rate per hour');
assert.equal((out.match(/class="cost-bar( zero)?"/g) || []).length, 24, 'hourly chart has 24 bars');
assert.ok(!out.includes('(est.)'), 'no run-rate-extrapolated estimate marker');
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
