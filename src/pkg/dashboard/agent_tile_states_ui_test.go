package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAgentTileStatesLabelsRunningAndUpNext(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: agent tile state behavior was not executed")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	start := strings.Index(html, "function agentCollapsedParseNextKick")
	if start < 0 {
		t.Fatal("agent tile helper block not found")
	}
	end := strings.Index(html[start:], "    function agentCollapsedAuthInfo")
	if end < 0 {
		t.Fatal("agent tile helper block end not found")
	}
	block := html[start : start+end]

	script := `
function agentIsDisabled(a) { return !!a && a.enabled === false; }
` + block + `
const now = Date.parse('2026-10-06T12:00:00Z');
function compact(rows) {
  return rows.map(row => ({ id: row.id, label: row.label, countdown: row.countdown, running: row.running }));
}
function assertSame(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) throw new Error(name + '\n got  ' + g + '\n want ' + w);
}
function assertNoBareNow(name, rows) {
  rows.forEach(row => { if (row.countdown === 'now') throw new Error(name + ' rendered bare now for ' + row.id); });
}
let rows = compact(agentTileStates([
  { name: 'ci', state: 'running', busy: 'working', busySince: '2026-10-06T11:56:00Z', nextKickIn: 'now' },
  { name: 'scanner', state: 'running', busy: 'working', busySince: '2026-10-06T11:55:00Z', nextKickIn: 'now' },
  { name: 'guide', state: 'running', busy: 'idle', nextKickIn: 'now' },
  { name: 'quality', state: 'running', busy: 'idle', nextKickIn: 'now' }
], now, { nextAgents: ['ci', 'scanner', 'guide'] }));
assertSame('two running and one queued labels exactly one non-running agent up next', rows, [
  { id: 'ci', label: 'running', countdown: 'running 4m', running: true },
  { id: 'scanner', label: 'running', countdown: 'running 5m', running: true },
  { id: 'guide', label: 'next', countdown: 'queued', running: false },
  { id: 'quality', label: null, countdown: '', running: false }
]);
assertNoBareNow('queued fixture', rows);
rows = compact(agentTileStates([
  { name: 'ci', state: 'running', busy: 'working', busySince: '2026-10-06T11:56:00Z', nextKickIn: 'now' },
  { name: 'scanner', state: 'running', busy: 'working', busySince: '2026-10-06T11:55:00Z', nextKickIn: 'now' },
  { name: 'guide', state: 'running', busy: 'idle', nextKickIn: 'now' }
], now, { nextAgents: ['ci', 'scanner'] }));
assertSame('running queued agents leave no up next target', rows, [
  { id: 'ci', label: 'running', countdown: 'running 4m', running: true },
  { id: 'scanner', label: 'running', countdown: 'running 5m', running: true },
  { id: 'guide', label: null, countdown: '', running: false }
]);
assertNoBareNow('no queued target fixture', rows);
rows = compact(agentTileStates([
  { name: 'guide', state: 'running', busy: 'idle', nextKickIn: 'now' },
  { name: 'quality', state: 'running', busy: 'idle', nextKickIn: 'now' }
], now, { nextAgents: [] }));
assertSame('paused governor or empty queue renders no up next', rows, [
  { id: 'guide', label: null, countdown: '', running: false },
  { id: 'quality', label: null, countdown: '', running: false }
]);
assertNoBareNow('paused fixture', rows);
rows = compact(agentTileStates([
  { name: 'reviewer', state: 'running', busy: 'idle', nextKickIn: 'in 16m' },
  { name: 'scanner', state: 'running', busy: 'idle', nextKickIn: 'in 38m' }
], now, { nextAgents: ['reviewer'] }));
assertSame('future queued agent keeps ETA', rows, [
  { id: 'reviewer', label: 'next', countdown: 'in 16m', running: false },
  { id: 'scanner', label: null, countdown: 'in 38m', running: false }
]);
assertNoBareNow('future queued fixture', rows);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent tile state JS failed: %v\n%s", err, out)
	}
}
