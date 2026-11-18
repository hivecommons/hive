package hub

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The "Does this build contain PR #N?" lookup lives inside the dashboard's
// inline script (#18915). These tests run the real render functions in node,
// with stubs for the two DOM-backed helpers (esc, escAttr) the slice needs.

const containsJSStubs = `
function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
function escAttr(s) { return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }
`

func runContainsJS(t *testing.T, script string) map[string]string {
	t.Helper()
	src := strings.Join([]string{
		containsJSStubs,
		jsFunc(t, "buildContainsNote"),
		jsFunc(t, "renderBuildContains"),
		script,
	}, "\n")
	stdout, err := exec.Command("node", "-e", src).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("node failed: %v\nstderr:\n%s", err, ee.Stderr)
		}
		t.Skipf("node unavailable: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	return got
}

func TestBuildContainsUIVerdictsAndEscaping(t *testing.T) {
	got := runContainsJS(t, `
var resp = {query: {kind: 'pr', pr: 7}, commit: '1a2b3c4d5e6f', hives: [
  {id: 'a', name: '<b>alpha</b>', commit: 'aaaaaaa1', channel: 'stable', contains: 'yes'},
  {id: 'b', name: 'beta', commit: 'bbbbbbb2', contains: 'no'},
  {id: 'c', name: 'gamma', commit: '', contains: 'unknown', reason: 'the hive reports no resolvable commit'},
  {id: 'd', name: 'delta', commit: 'ddddddd4', contains: 'definitely'}
]};
var rows = {a: {commitsBehindTarget: 3, behindTargetRef: ':stable', behindTargetSHA: '1111111', branchTipSHA: '2222222'}};
process.stdout.write(JSON.stringify({
  html: renderBuildContains(resp, '', rows),
  failed: renderBuildContains(null, 'Lookup failed', rows),
  empty: renderBuildContains({query: {kind: 'commit', commit: 'abc1234'}, hives: []}, '', {}),
}));`)
	html := got["html"]
	if strings.Contains(html, "<b>alpha</b>") {
		t.Fatalf("hive names must be escaped: %s", html)
	}
	if strings.Count(html, `hive-contains-chip yes`) != 1 || strings.Count(html, `hive-contains-chip no`) != 1 {
		t.Fatalf("expected exactly one yes and one no chip: %s", html)
	}
	// An unrecognised or missing verdict must render as unknown, never as a
	// confident answer.
	if strings.Count(html, `hive-contains-chip unknown`) != 2 {
		t.Fatalf("empty and unrecognised verdicts must both render unknown: %s", html)
	}
	if !strings.Contains(html, "newest image 1111111") || !strings.Contains(html, "newest merged 2222222") {
		t.Fatalf("the image commit and the merged commit must be shown separately: %s", html)
	}
	if !strings.Contains(html, "3 behind :stable") {
		t.Fatalf("the behind count must be shown: %s", html)
	}
	if !strings.Contains(html, "no commit") {
		t.Fatalf("a hive with no commit must say so: %s", html)
	}
	if !strings.Contains(got["failed"], "Lookup failed") || strings.Contains(got["failed"], "hive-contains-chip") {
		t.Fatalf("a failed lookup must show the error and no verdicts: %s", got["failed"])
	}
	if !strings.Contains(got["empty"], "No hives to check") {
		t.Fatalf("empty fleet: %s", got["empty"])
	}
}

func TestBuildContainsUIWiring(t *testing.T) {
	html := dashScript(t)
	for _, want := range []string{
		`id="hive-contains-q"`,
		`onclick="runBuildContains()"`,
		`onclick="clearBuildContains()"`,
		"/api/saas/build-contains?q=",
		"function runBuildContains(",
		"function clearBuildContains(",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard is missing %q", want)
		}
	}
	// The lookup is on demand: no timer may drive it.
	for _, name := range []string{"runBuildContains", "clearBuildContains", "renderBuildContains", "buildContainsNote"} {
		if body := jsFunc(t, name); strings.Contains(body, "setInterval") {
			t.Fatalf("%s must not use setInterval", name)
		}
	}
}
