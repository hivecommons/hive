// #18813: a paused agent must always show a reachable (or explained) resume
// control, and the role-gated slot repaints when the role settles.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const js = n => fs.readFileSync(path.join(__dirname, 'static-src', 'js', n), 'utf8');
const cards = js('agent-cards.js');
const detail = js('agent-detail.js');
const gov = js('governor-view.js');

// Pull a top-level `function name(...) {...}` out of a source by brace matching.
function fn(src, name) {
  const start = src.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let i = src.indexOf('{', src.indexOf(')', start));
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) return src.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
}
function consts(src, names) {
  return names.map(n => src.match(new RegExp('const ' + n + ' = [^;]+;'))[0]).join('\n');
}

const win = {};
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const body = [
  consts(cards, ['AGENT_GATE_NEEDS_OWNER', 'AGENT_GATE_CHECKING']),
  fn(gov, 'dashboardRoleAtLeast'),
  fn(cards, 'agentRoleKnown'),
  fn(cards, 'agentToggleGateReason'),
  fn(cards, 'agentGatedToggleHtml'),
  fn(cards, 'agentToggleTitle'),
  fn(cards, 'agentCadenceModesLabel' in {} ? '' : 'pausedAgentActionHtml'),
  'return { agentToggleGateReason, agentGatedToggleHtml, pausedAgentActionHtml };',
].join('\n');
const api = new Function('window', 'esc', 'agentCadenceOffLabel', 'agentCadenceModesLabel', body)(win, esc, () => 'no cadence', () => 'busy');

function setRole(role, settled) {
  win._hiveRole = role;
  if (settled) win._hiveRoleSettled = true; else delete win._hiveRoleSettled;
}
const paused = (o) => Object.assign({ name: 'reviewer', paused: true, state: 'paused', enabled: true }, o);
const matrix = [
  paused({ sandboxed: true }), paused({ sandboxed: false }),
  paused({ sandboxed: true, state: 'running' }), paused({ sandboxed: false, state: 'running' }),
];

test('role unknown: control is disabled with the checking hint, never empty', () => {
  setRole(undefined, false);
  assert.strictEqual(api.agentToggleGateReason(), 'Checking your access...');
  for (const a of matrix) {
    const h = api.agentGatedToggleHtml(a, true, false);
    assert.ok(/<button[^>]* disabled /.test(h));
    assert.ok(h.includes('title="Checking your access..."'));
    assert.ok(h.includes('resume'));
    assert.ok(!h.includes('data-action'));
  }
});

test('role read and read-write: disabled with the owner hint', () => {
  for (const role of ['read', 'read-write', 'merger']) {
    setRole(role, true);
    assert.strictEqual(api.agentToggleGateReason(), 'Needs the owner role');
    const h = api.agentGatedToggleHtml(paused({ sandboxed: true }), true, false);
    assert.ok(h.includes('title="Needs the owner role"') && h.includes(' disabled '));
  }
  setRole(undefined, true); // /api/role gave up
  assert.strictEqual(api.agentToggleGateReason(), 'Needs the owner role');
});

test('role settles to owner: the live resume control appears', () => {
  setRole(undefined, false);
  assert.notStrictEqual(api.agentToggleGateReason(), '');
  setRole('owner', true);
  assert.strictEqual(api.agentToggleGateReason(), '');
  for (const a of matrix) {
    const h = api.pausedAgentActionHtml(a);
    assert.ok(h.includes('data-action='));
    assert.ok(!h.includes(' disabled'));
  }
});

test('sandboxed paused agent gets plain resume, not start & resume', () => {
  const sb = paused({ sandboxed: true });
  assert.ok(api.pausedAgentActionHtml(sb).includes('data-action="toggleAgent"'));
  assert.ok(api.pausedAgentActionHtml(sb).includes('▶ resume'));
  assert.ok(!api.pausedAgentActionHtml(sb).includes('start &amp; resume'));
  assert.ok(!api.agentGatedToggleHtml(sb, true, false).includes('start &amp; resume'));
  const plain = paused({ sandboxed: false });
  assert.ok(api.pausedAgentActionHtml(plain).includes('data-action="startResumeAgent"'));
  assert.ok(api.agentGatedToggleHtml(plain, true, false).includes('start &amp; resume'));
});

test('running agent below owner shows a disabled pause', () => {
  setRole('read', true);
  const h = api.agentGatedToggleHtml({ name: 'deps', state: 'running' }, false, false);
  assert.ok(h.includes('pause') && h.includes(' disabled ') && h.includes('Needs the owner role'));
});

test('card grid, detail and fast path use the gate and repaint on role settle', () => {
  assert.ok(cards.includes('(window._hiveRoleListeners = window._hiveRoleListeners || []).push(agentsOnRoleReady)'));
  assert.ok(fn(cards, 'agentsOnRoleReady').includes('renderAgents(window._lastAgents)'));
  assert.ok(fn(cards, 'agentsOnRoleReady').includes('ocRenderAgentDetail()'));
  assert.ok(detail.includes('agentToggleGateReason()].join'), 'role is part of the detail fingerprint');
  assert.ok(detail.includes('agentGatedToggleHtml(a, true, false)'));
  assert.ok(!/!dashboardRoleAtLeast\(window\._hiveRole \|\| 'read', 'owner'\)\s*\? ''/.test(detail), 'no silent empty slot');
});

test('collapsed cadence row: one paused chip, plain text in the mode columns', () => {
  assert.ok(gov.includes("showPaused ? '<span class=\"muted-xs\">paused</span>' : val"));
  const chipUses = gov.match(/pausedBadgeHtml/g) || [];
  // definition + the single name-column use
  assert.strictEqual(chipUses.length, 2);
});
