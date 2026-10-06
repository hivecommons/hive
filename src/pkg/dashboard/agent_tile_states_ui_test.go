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
assertSame('one running agent labels next scheduled agent up next', compact(agentTileStates([
  { name: 'adjudicator', state: 'running', busy: 'working', nextKickIn: 'now' },
  { name: 'reviewer', state: 'running', busy: 'idle', nextKickIn: 'in 16m' },
  { name: 'scanner', state: 'running', busy: 'idle', nextKickIn: 'in 38m' }
], now)), [
  { id: 'adjudicator', label: 'running', countdown: 'now', running: true },
  { id: 'reviewer', label: 'next', countdown: 'in 16m', running: false },
  { id: 'scanner', label: null, countdown: 'in 38m', running: false }
]);
assertSame('no running agent labels only soonest tile up next', compact(agentTileStates([
  { name: 'adjudicator', state: 'running', busy: 'idle', nextKickIn: 'in 20m' },
  { name: 'reviewer', state: 'running', busy: 'idle', nextKickIn: 'in 16m' },
  { name: 'scanner', state: 'running', busy: 'idle', nextKickIn: 'in 38m' }
], now)), [
  { id: 'reviewer', label: 'next', countdown: 'in 16m', running: false },
  { id: 'adjudicator', label: null, countdown: 'in 20m', running: false },
  { id: 'scanner', label: null, countdown: 'in 38m', running: false }
]);
assertSame('multiple running agents all show running now and first queued agent is up next', compact(agentTileStates([
  { name: 'adjudicator', running: true, busy: 'idle', nextKickIn: 'in 8m' },
  { name: 'reviewer', state: 'running', busy: 'working', nextKickIn: 'in 16m' },
  { name: 'scanner', state: 'running', busy: 'idle', nextKickIn: 'in 38m' }
], now)), [
  { id: 'adjudicator', label: 'running', countdown: 'now', running: true },
  { id: 'reviewer', label: 'running', countdown: 'now', running: true },
  { id: 'scanner', label: 'next', countdown: 'in 38m', running: false }
]);
assertSame('due now is not running without an execution signal', compact(agentTileStates([
  { name: 'adjudicator', state: 'running', busy: 'idle', nextKickIn: 'now' },
  { name: 'reviewer', state: 'running', busy: 'idle', nextKickIn: 'now' },
  { name: 'scanner', state: 'running', busy: 'idle', nextKickIn: 'now' }
], now)), [
  { id: 'adjudicator', label: 'next', countdown: 'now', running: false },
  { id: 'reviewer', label: null, countdown: 'now', running: false },
  { id: 'scanner', label: null, countdown: 'now', running: false }
]);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent tile state JS failed: %v\n%s", err, out)
	}
}
