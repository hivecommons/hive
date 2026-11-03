const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
function grab(name) {
  const m = html.match(new RegExp('function ' + name + '\\([\\s\\S]*?\\n    }\\n'));
  assert.ok(m, name + ' not found');
  return m[0];
}
const src = ['advisorSourceBadge', 'renderAdvisorSettings', 'advisorIntField', 'advisorTextField', 'saveAdvisorFleet'].map(grab).join('\n');
const load = (doc, put) => new Function('document', 'putAdvisorSettings', 'esc', 'formatNumber',
  src + '; return { renderAdvisorSettings, saveAdvisorFleet };')(doc, put, s => String(s), n => String(n));

// Returns the value of the fleet <select> option that is rendered selected.
function selectedFleetValue(markup) {
  const sel = markup.match(/<select id="adv-f-enabled"[\s\S]*?<\/select>/)[0];
  return sel.match(/<option value="(\w+)"\s+selected/)[1];
}

test('an unset fleet enabled renders as Off', () => {
  const { renderAdvisorSettings } = load({}, () => {});
  assert.strictEqual(selectedFleetValue(renderAdvisorSettings({ fleet: {} })), 'off');
  assert.strictEqual(selectedFleetValue(renderAdvisorSettings({})), 'off');
  assert.strictEqual(selectedFleetValue(renderAdvisorSettings({ fleet: { enabled: true } })), 'on');
});

test('saving an unset fleet enabled leaves the lane disabled', () => {
  let posted;
  const put = (url, body) => { posted = { url, body }; return true; };
  const probe = load({}, put);
  const value = selectedFleetValue(probe.renderAdvisorSettings({ fleet: {} }));
  const doc = { getElementById: id => (id === 'adv-f-enabled' ? { value } : { value: '' }) };
  load(doc, put).saveAdvisorFleet();
  assert.strictEqual(posted.url, '/api/config/advisor');
  assert.strictEqual(posted.body.enabled, false);
});
