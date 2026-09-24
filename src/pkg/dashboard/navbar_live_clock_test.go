package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func jsVarLine(t *testing.T, html, name string) string {
	t.Helper()
	for _, prefix := range []string{"const ", "let ", "var "} {
		needle := prefix + name + " = "
		start := strings.Index(html, needle)
		if start < 0 {
			continue
		}
		end := strings.Index(html[start:], "\n")
		if end < 0 {
			t.Fatalf("unterminated %s declaration in index.html", name)
		}
		return strings.TrimSpace(html[start : start+end])
	}
	t.Fatalf("index.html does not define %s", name)
	return ""
}

func TestNavbarClockTicksLocallyAndFlagsStaleStatus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the navbar clock rules were NOT executed by this run")
	}
	html := indexHTML(t)
	for _, snippet := range []string{
		`<span class="status-stale" id="status-stale" hidden></span>`,
		`<span class="status-stale" id="oc-status-stale" hidden></span>`,
		"const NAVBAR_CLOCK_TICK_MS = 1000;",
		"const STATUS_STALE_AFTER_MS = STATUS_EXPECTED_REFRESH_MS * STATUS_STALE_AFTER_POLLS;",
		"setInterval(tickNavbarClock, NAVBAR_CLOCK_TICK_MS);",
		"updateNavbarClockTimeZone(data);",
		"noteStatusPayloadTimestamp(data);",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "const tsDt = new Date(data.timestamp);") {
		t.Fatal("render() still formats the navbar clock from the status payload timestamp")
	}

	var script strings.Builder
	script.WriteString(`
const elements = {
  ts: { textContent: '', hidden: false, title: '' },
  'oc-ts': { textContent: '', hidden: false, title: '' },
  'status-stale': { textContent: '', hidden: true, title: '' },
  'oc-status-stale': { textContent: '', hidden: true, title: '' },
};
const document = { getElementById: (id) => elements[id] || null };
const window = {};
`)
	for _, name := range []string{
		"NAVBAR_CLOCK_TICK_MS", "STATUS_EXPECTED_REFRESH_MS", "STATUS_STALE_AFTER_POLLS",
		"STATUS_STALE_AFTER_MS", "_navbarClockTimeZone", "_lastStatusPayloadAtMs",
	} {
		script.WriteString(jsVarLine(t, html, name) + "\n")
	}
	for _, name := range []string{
		"validDashboardTimeZone", "browserDashboardTimeZone", "updateNavbarClockTimeZone",
		"noteStatusPayloadTimestamp", "formatNavbarClock", "formatStatusPayloadAge",
		"setNavbarClockText", "updateStatusStaleness", "tickNavbarClock",
	} {
		script.WriteString(jsFunc(t, html, name) + "\n")
	}
	script.WriteString(`
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}
const payload = { timestamp: '2026-09-24T00:07:00Z', timeZone: 'America/New_York' };
updateNavbarClockTimeZone(payload);
noteStatusPayloadTimestamp(payload);
tickNavbarClock(new Date('2026-09-24T00:17:00Z'));
check('uses hub timezone', _navbarClockTimeZone === 'America/New_York');
check('clock ticks from Date.now-equivalent, not payload timestamp', /8:17/.test(elements.ts.textContent) && !/8:07/.test(elements.ts.textContent));
check('topbar clock matches main clock', elements['oc-ts'].textContent === elements.ts.textContent);
check('stale status is visible', elements['status-stale'].hidden === false && /status 10m old/.test(elements['status-stale'].textContent));
check('topbar stale status is visible', elements['oc-status-stale'].hidden === false && elements['oc-status-stale'].textContent === elements['status-stale'].textContent);
updateNavbarClockTimeZone({ timeZone: 'Mars/Olympus' });
check('invalid hub timezone falls back', _navbarClockTimeZone !== 'Mars/Olympus');
if (fails) { console.log(fails + ' failure(s)'); process.exit(1); }
console.log('ok');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("navbar clock check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
