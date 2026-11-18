const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const strip = fs.readFileSync(path.join(__dirname, 'static-src/js/tracker-row-strip.js'), 'utf8');
const actions = fs.readFileSync(path.join(__dirname, 'static-src/js/tracker-actions.js'), 'utf8');
const pick = (source, re) => { const m = source.match(re); assert.ok(m); return m[0]; };
const helpers = new Function('esc', 'stageTrackerActionCtx', 'stageTrackerActionAllowed',
  pick(strip, /function stageTrackerControlLabels\(item\) \{[\s\S]*?\n    \}/) + '\n' +
  pick(strip, /function stageTrackerControlLabelsHtml\(item\) \{[\s\S]*?\n    \}/) +
  '\nreturn { labels: stageTrackerControlLabels, html: stageTrackerControlLabelsHtml };');
const escape = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
const issue = { kind: 'issue', repo: 'o/r', labels: ['needs-sha', 'needs-spec'], sha_hold_label: 'needs-sha' };

test('control labels explain the gate and how to lift it', () => {
  const h = helpers(escape, () => ({}), () => ({ enabled: true }));
  const labels = h.labels(issue);
  assert.equal(labels.length, 2);
  assert.match(labels[0].tip, /SHA.*comment.*sweep/);
  assert.match(labels[1].tip, /acceptance criteria.*remove/);
  assert.equal(h.labels({ kind: 'pr', labels: ['needs-sha'], sha_hold_label: 'needs-sha' }).length, 0);
  assert.equal(h.labels({ kind: 'issue', labels: ['bug'] }).length, 0);
  assert.equal(h.labels({ kind: 'issue', labels: ['WAIT-SHA'], sha_hold_label: 'wait-sha' })[0].sha, true);
  assert.match(strip, /sg\.number === 0[\s\S]*?stageTrackerControlLabels\(item\)/);
});

test('SHA override follows hold permission and does not release other holds', () => {
  const allowed = helpers(escape, () => ({}), () => ({ enabled: true })).html(issue);
  assert.match(allowed, /data-ham-act="override-sha"/);
  assert.doesNotMatch(allowed, /aria-disabled/);
  const denied = helpers(escape, () => ({}), () => ({ enabled: false, why: 'write access required' })).html(issue);
  assert.match(denied, /aria-disabled="true"/);
  assert.match(denied, /write access required/);
  assert.match(actions, /case 'override-sha':.*held: false, type, override_sha: true/);
});
