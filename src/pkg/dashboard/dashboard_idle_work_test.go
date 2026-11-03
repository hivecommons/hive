package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboardIdleWork(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — idle-work behavior not executed")
	}
	html := indexHTML(t)
	start := strings.Index(html, "    function dashboardSetInterval(")
	end := strings.Index(html, "    let historyData =")
	if start < 0 || end <= start {
		t.Fatal("interval registry block missing")
	}
	script := `
const assert = require('node:assert/strict');
const window = {};
const listeners = {};
const document = { hidden: false, addEventListener: (type, fn) => listeners[type] = fn };
const timers = new Map(); let nextTimer = 0;
function setInterval(fn, ms) { const id = ++nextTimer; timers.set(id, {fn, ms}); return id; }
function clearInterval(id) { timers.delete(id); }
function visibility(hidden) { document.hidden = hidden; listeners.visibilitychange(); }
`
	script += jsVarLine(t, html, "DASHBOARD_INTERVALS") + "\n" + html[start:end]
	for _, name := range []string{"_dashboardDateFormatters", "DASHBOARD_TIMEZONE_STORAGE_KEY", "DASHBOARD_TIMEZONE_BROWSER", "DASHBOARD_TIMEZONE_SERVER", "_dashboardServerTimeZone"} {
		script += jsVarLine(t, html, name) + "\n"
	}
	for _, name := range []string{"dashboardDateFormatter", "selectedDashboardTimeZone", "dashboardTimeZonePreference", "validDashboardTimeZone", "browserDashboardTimeZone", "animateSparklinePart"} {
		script += jsFunc(t, html, name) + "\n"
	}
	script += `
let ticks = 0;
dashboardSetInterval('panel', () => ticks++, 1000);
assert.equal(timers.size, 1);
visibility(true);
assert.equal(timers.size, 0, 'hidden tabs have no registered timer wakeups');
dashboardSetInterval('hidden-panel', () => ticks++, 2000);
assert.equal(timers.size, 0, 'registration while hidden remains paused');
visibility(false);
assert.equal(ticks, 2, 'one immediate refresh per entry on resume');
assert.equal(timers.size, 2);
for (const timer of timers.values()) timer.fn();
assert.equal(ticks, 4);
dashboardSetInterval('panel', () => ticks += 10, 3000);
assert.equal(timers.size, 2, 'replacement clears the old timer');
visibility(true);
dashboardClearInterval('hidden-panel');
visibility(false);
assert.equal(ticks, 14, 'cleared hidden entries do not resume');
assert.equal(timers.size, 1);
dashboardClearInterval('panel');
dashboardSetInterval('first', () => dashboardClearInterval('second'), 1000);
dashboardSetInterval('second', () => { throw Error('removed callback ran'); }, 1000);
visibility(true); visibility(false);
assert.equal(timers.size, 1, 'a resume callback may remove a later entry');
dashboardClearInterval('first');

let preference = 'UTC';
const localStorage = { getItem: () => preference };
const originalFormatter = Intl.DateTimeFormat;
let constructions = 0;
Intl.DateTimeFormat = function(...args) { constructions++; return new originalFormatter(...args); };
const a = dashboardDateFormatter({hour:'numeric', minute:'2-digit'});
assert.equal(a, dashboardDateFormatter({minute:'2-digit', hour:'numeric'}), 'option ordering does not defeat cache');
assert.equal(constructions, 1);
preference = 'America/New_York';
const b = dashboardDateFormatter({hour:'numeric', minute:'2-digit'});
assert.notEqual(a, b, 'timezone changes select a new formatter');
const count = constructions;
for (let i = 0; i < 10; i++) dashboardDateFormatter({hour:'numeric', minute:'2-digit'});
assert.equal(constructions, count, 'timezone validation is cached too');
preference = 'browser';
dashboardDateFormatter({year:'numeric'});
const browserCount = constructions;
dashboardDateFormatter({year:'numeric'});
assert.equal(constructions, browserCount, 'browser timezone lookup is cached');
assert.equal(dashboardDateFormatter({year:'numeric'}).format(new Date(0)), new originalFormatter([], {year:'numeric', timeZone: selectedDashboardTimeZone()}).format(new Date(0)));

for (const frames of [
  [{strokeDashoffset: 50}, {strokeDashoffset: 0}],
  [{opacity: 0}, {opacity: 0.14}],
  [{opacity: 0, transform:'scale(0.35)'}, {opacity: 1, transform:'scale(1)'}],
]) {
  let canceled = false;
  const animation = {cancel() { canceled = true; }};
  const element = {style: {}, animate() { return animation; }};
  animateSparklinePart(element, frames, {fill:'forwards'});
  animation.onfinish();
  assert.equal(canceled, true, 'finished effect released');
  for (const [key, value] of Object.entries(frames.at(-1))) assert.equal(element.style[key], String(value));
}
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("idle-work behavior failed: %v\n%s", err, out)
	}
}
