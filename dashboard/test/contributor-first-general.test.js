'use strict';

// The agent's General tab controls for the contributor-first lane keys
// (agents.<name>.contributor_first and contributor_first_grace, #16518). The
// toggle must show the stored state, carry its help mark, and be disabled with
// the fix named when the role is not one contributors may act as. The grace
// field shows the stored value and saves seconds, blank clearing to 10m.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

// Everything renderAgentGeneral touches that this test does not exercise
// resolves to a black hole: callable, any property is another black hole, and
// it prints as ''. The functions under test are the real ones from index.html.
const blackHole = new Proxy(function () {}, {
  get: (_t, key) => (key === Symbol.toPrimitive ? () => '' : blackHole),
  apply: () => blackHole,
});

const realSrc = ['renderAgentGeneral', 'agentContributorFirstControlsHtml', 'settingHelpMark', 'yamlHint', 'esc', 'cadenceToHuman']
  .map(extractFunction).join('\n');

function renderGeneral(g, agent) {
  const own = {
    _configState: { agent, isCreate: false },
    window: { _lastAgents: [], _lastStatus: {} },
    settingHelpSeq: 0,
    dashboardDocsHref: (page, anchor) => '/docs/' + page + '#' + anchor,
    setTimeout: () => {},
    SECONDS_PER_MIN: 60,
    SECONDS_PER_HOUR: 3600,
    SECONDS_PER_DAY: 86400,
  };
  const passthrough = new Set(['String', 'Number', 'Boolean', 'Array', 'Object', 'JSON', 'Math', 'parseInt', 'parseFloat', 'isNaN', 'undefined', 'Symbol', 'Date', 'RegExp', 'Error', 'encodeURIComponent']);
  const scope = new Proxy(own, {
    has: (target, key) => !passthrough.has(key),
    get: (target, key) => (key === Symbol.unscopables ? undefined : key in target ? target[key] : blackHole),
    set: (target, key, value) => { target[key] = value; return true; },
  });
  // eslint-disable-next-line no-new-func
  const render = new Function('scope', 'with (scope) { ' + realSrc + '\n return renderAgentGeneral; }')(scope);
  return render(g);
}

function toggle(html) {
  const m = html.match(/<div class="config-toggle-switch[^"]*" id="cfg-contributor-first"[^>]*>/);
  assert.ok(m, 'contributor-first toggle rendered');
  return m[0];
}

function helpMarks(html, label) {
  return html.match(new RegExp('<a class="config-info section-help-mark" href="/docs/agent-configuration#contributor-first-lanes-contributor_first"[^>]*aria-label="Help: ' + label + '"', 'g')) || [];
}

test('toggle on for a delegatable role, with help marks on both controls', () => {
  const html = renderGeneral({ contributorFirst: true, contributorFirstGrace: 900, contributorFirstDelegatable: true }, 'scanner');
  const t = toggle(html);
  assert.match(t, /config-toggle-switch on /);
  assert.doesNotMatch(t, /class="[^"]*\bdisabled\b/);
  assert.match(t, /data-action="toggleConfigSwitch"/);
  assert.match(t, /data-key="contributorFirst"/);
  assert.equal(helpMarks(html, 'Contributor-first').length, 1, 'toggle help mark');
  assert.equal(helpMarks(html, 'Contributor-first grace').length, 1, 'grace help mark');
  for (const phrase of ["primary worker and the resident agent's kicks are withheld", 'Kicks resume on the normal cadence', 'Budget pause and standby are unaffected', 'a manual Kick still works']) {
    assert.ok(html.includes(phrase), `toggle help says: ${phrase}`);
  }
  assert.match(html, /id="cfg-contributor-first-grace" value="15m" placeholder="10m" data-change-action="contributorFirstGraceChanged"/);
  assert.doesNotMatch(html, /id="cfg-contributor-first-disabled"/);
  assert.match(html, /agents\.&lt;key&gt;\.contributor_first<\/code>/);
});

test('toggle off for a delegatable role; unset grace shows the placeholder', () => {
  const html = renderGeneral({ contributorFirst: false, contributorFirstGrace: 0, contributorFirstDelegatable: true }, 'quality');
  const t = toggle(html);
  assert.doesNotMatch(t, /config-toggle-switch on /);
  assert.match(t, /data-action="toggleConfigSwitch"/);
  assert.match(t, /data-disabled="false"/);
  assert.match(html, /id="cfg-contributor-first-grace" value="" placeholder="10m"/);
  assert.doesNotMatch(html, /id="cfg-contributor-first-disabled"/);
});

test('non-delegatable role: toggle disabled and the fix named', () => {
  const html = renderGeneral({ contributorFirst: false, contributorFirstDelegatable: false }, 'architect');
  const t = toggle(html);
  assert.match(t, /class="[^"]*\bdisabled\b/);
  assert.match(t, /data-action=""/);
  assert.match(t, /data-disabled="true"/);
  const reason = html.match(/<div class="text-muted" id="cfg-contributor-first-disabled"[^>]*>([\s\S]*?)<\/div>/);
  assert.ok(reason, 'disabled reason rendered');
  assert.match(reason[1], /add <code>architect<\/code> to <code>hub\.contribute_delegatable_roles<\/code>/);
  assert.equal(helpMarks(html, 'Contributor-first').length, 1, 'help mark still rendered when disabled');
});

test('supervisor names why it can never be delegated', () => {
  const html = renderGeneral({ contributorFirstDelegatable: false }, 'supervisor');
  assert.match(html, /id="cfg-contributor-first-disabled"[^>]*>The supervisor role is never delegatable to contributors\.</);
});

test('agent creation shows no contributor-first controls', () => {
  const own = { _configState: { agent: '', isCreate: true } };
  const passthrough = new Set(['String', 'Number', 'Boolean', 'Array', 'Object', 'JSON', 'Math', 'parseInt', 'parseFloat', 'isNaN', 'undefined', 'Symbol']);
  const base = { window: { _lastAgents: [] }, settingHelpSeq: 0, dashboardDocsHref: () => '#', setTimeout: () => {}, SECONDS_PER_MIN: 60, SECONDS_PER_HOUR: 3600, SECONDS_PER_DAY: 86400 };
  const target = Object.assign(base, own);
  const scope = new Proxy(target, {
    has: (t, key) => !passthrough.has(key),
    get: (t, key) => (key === Symbol.unscopables ? undefined : key in t ? t[key] : blackHole),
    set: (t, key, value) => { t[key] = value; return true; },
  });
  const render = new Function('scope', 'with (scope) { ' + realSrc + '\n return renderAgentGeneral; }')(scope);
  assert.doesNotMatch(render({}), /cfg-contributor-first/);
});

test('grace field saves seconds; blank or unparseable clears to the default', () => {
  const line = indexHtml.split('\n').find(l => l.includes('contributorFirstGraceChanged: function'));
  assert.ok(line, 'grace change handler exists');
  const body = line.trim().replace(/,$/, '');
  const cadenceFromHuman = new Function('SECONDS_PER_MIN', 'SECONDS_PER_HOUR', 'SECONDS_PER_DAY', extractFunction('cadenceFromHuman') + '; return cadenceFromHuman;')(60, 3600, 86400);
  const cadenceToHuman = new Function('SECONDS_PER_MIN', 'SECONDS_PER_HOUR', 'SECONDS_PER_DAY', extractFunction('cadenceToHuman') + '; return cadenceToHuman;')(60, 3600, 86400);
  for (const [input, secs, shown] of [['15m', 900, '15m'], ['90s', 90, '90s'], ['1h', 3600, '1h'], ['', 0, ''], ['-5m', 0, ''], ['soon', 0, '']]) {
    const dirty = [];
    const handlers = new Function('markDirty', 'cadenceFromHuman', 'cadenceToHuman', 'return {' + body + '};')((section, key, value) => dirty.push([section, key, value]), cadenceFromHuman, cadenceToHuman);
    const el = { value: input };
    handlers.contributorFirstGraceChanged.call(el, {}, []);
    assert.deepEqual(dirty, [['general', 'contributorFirstGrace', secs]], `input ${JSON.stringify(input)}`);
    assert.equal(el.value, shown, `input ${JSON.stringify(input)} is shown back as ${JSON.stringify(shown)}`);
  }
});
