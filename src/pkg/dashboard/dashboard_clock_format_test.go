package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDashboardClockFormatControlExists(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"const DASHBOARD_CLOCK_STORAGE_KEY = 'hive-dashboard-clock-format';",
		"id=\"dashboard-clock-select\"",
		"data-change-action=\"setDashboardClockPreference\"",
		"function setDashboardClockPreference(",
		"Browser locale (default)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static/index.html missing clock format marker %q", want)
		}
	}
}

// TestDashboardClockFormatAndMergeBannerTime runs the shipped date helpers
// (not jsFunc's UTC stand-ins) in America/New_York: the Time format choice
// reaches every formatted time, and the merge banner carries the date and
// time on its head instead of at the end of the clipped title line.
func TestDashboardClockFormatAndMergeBannerTime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: clock format helpers were not executed")
	}
	html := indexHTML(t)
	fn := func(name string) string {
		src, ok := jsFuncOptional(html, name)
		if !ok {
			t.Fatalf("index.html does not define %s()", name)
		}
		return src
	}
	script := strings.Join([]string{
		"const assert = require('node:assert/strict');",
		// The helpers format in the runner's default locale; pin en-US so
		// an en-GB runner's "4:12 pm GMT-5" does not fail the wording.
		"const RealDTF = Intl.DateTimeFormat; Intl.DateTimeFormat = function (locales, opts) { return new RealDTF(locales && locales.length ? locales : 'en-US', opts); };",
		"const store ={ 'hive-dashboard-display-timezone': 'America/New_York' };",
		"const localStorage = { getItem: k => (k in store ? store[k] : null) };",
		jsConstLine(t, html, "DASHBOARD_TIMEZONE_STORAGE_KEY"),
		jsConstLine(t, html, "DASHBOARD_TIMEZONE_BROWSER"),
		jsConstLine(t, html, "DASHBOARD_TIMEZONE_SERVER"),
		jsConstLine(t, html, "DASHBOARD_CLOCK_STORAGE_KEY"),
		jsConstLine(t, html, "_dashboardDateFormatters"),
		"let _dashboardServerTimeZone = '';",
		fn("validDashboardTimeZone"),
		fn("browserDashboardTimeZone"),
		fn("dashboardTimeZonePreference"),
		fn("dashboardClockPreference"),
		fn("selectedDashboardTimeZone"),
		fn("dashboardDateFormatter"),
		fn("formatDashboardDateTime"),
		fn("stageTrackerMergedByText"),
		fn("stageTrackerMergerText"),
		fn("stageTrackerMergeBannerNumber"),
		fn("stageTrackerMergeBannerText"),
		`
const plain = s => String(s).replace(/[  ]/g, ' ');
const at = Date.UTC(2026, 10, 17, 21, 12); // 4:12 PM EST
const clock = v => { if (v) store[DASHBOARD_CLOCK_STORAGE_KEY] = v; else delete store[DASHBOARD_CLOCK_STORAGE_KEY]; };

clock('');
assert.equal(dashboardClockPreference(), '');
assert.equal(formatDashboardDateTime(at), '11/17 4:12 PM EST', 'default keeps the caller format');
const b = { repo: 'o/r', number: 5, title: 'fix: five', lane: 'hive', merged_by: 'hive', at };
const t = stageTrackerMergeBannerText(b, 0);
assert.equal(t.when, '11/17 4:12 PM EST', 'the banner names date and local time');
assert.equal(t.meta, '(hive, by hive)', 'the clipped title line no longer carries the time');
assert.equal(t.text, 'fix: five (hive, by hive) · merged 11/17 4:12 PM EST');

clock('24');
assert.equal(formatDashboardDateTime(at), '11/17 16:12 EST', '24-hour overrides the caller hour12');
assert.equal(stageTrackerMergeBannerText(b, 0).when, '11/17 16:12 EST');
assert.match(formatDashboardDateTime(Date.UTC(2026, 10, 18, 5, 0)), /^11\/18 0?0:00 EST$/, 'midnight is 00:00, never 24:00');

clock('12');
assert.equal(plain(formatDashboardDateTime(at, { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' })), '04:12 PM', '12-hour overrides a caller 24-hour cycle');

clock('bogus');
assert.equal(dashboardClockPreference(), '', 'an unknown stored value is ignored');
`,
	}, "\n")
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
