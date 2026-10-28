// Run by the dashboard's node fixture job in CI.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

test('merged counter belongs to the persistent gate header and the flight resolves it', () => {
  const headerStart = html.indexOf('<div class="ham-cols ham-counter-header"');
  const panelStart = html.indexOf('<div id="activity-monitor-panel">');
  assert.ok(headerStart >= 0 && panelStart > headerStart);
  const header = html.slice(headerStart, panelStart);
  assert.match(header, /id="activity-monitor-merged"/);
  assert.match(header, /class="ham-strip ham-counter-align"/);
  assert.match(header, /class="ham-since"/); // same gate/since geometry as column labels
  const toolbar = html.slice(html.indexOf('<div class="ham-toolbar"'), headerStart);
  assert.doesNotMatch(toolbar, /id="activity-monitor-merged"/);
  assert.equal((html.match(/id="activity-monitor-merged"/g) || []).length, 1);

  // Use the real celebration function with a header fixture as its DOM.
  const src = html.match(/    function celebrateRow\(rowEl, opts\) \{[\s\S]*?\n    \}/);
  assert.ok(src);
  const target = { getBoundingClientRect: () => ({ left: 800, top: 20, width: 120, height: 24 }) };
  const ids = Object.fromEntries([...header.matchAll(/id="([^"]+)"/g)].map(m => [m[1], target]));
  let resolved, token;
  const document = {
    getElementById(id) { resolved = id; return ids[id]; },
    createElement() { return token = { style: { values: {}, setProperty(k, v) { this.values[k] = v; } }, remove() {} }; },
    body: { appendChild() {} },
  };
  const celebrate = new Function('document', `
    const STAGE_TRACKER_CELEBRATE_PIN_MS = 2500, STAGE_TRACKER_CELEBRATE_MAX_FLIGHTS = 3, STAGE_TRACKER_CELEBRATE_FLY_MS = 500, STAGE_TRACKER_TOKEN_SVG = '';
    let _stageTrackerFlights = 0;
    const setTimeout = () => {}, stageTrackerAfter = () => {}, stageTrackerReducedMotion = () => false;
    ${src[0]}
    return celebrateRow;
  `)(document);
  const row = { classList: { add() {} }, dataset: {}, querySelector: () => ({ getBoundingClientRect: () => ({ left: 800, top: 200, width: 24, height: 24 }) }) };
  assert.equal(celebrate(row, { preview: true }).flew, true);
  assert.equal(resolved, 'activity-monitor-merged');
  assert.equal(token.style.values['--cue-dy'], '-180px', 'token flies upward to the gate header');
});
