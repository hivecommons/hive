// hivecommons/hive#18916: a paused agent says who or what paused it, when and
// why, and who last resumed it; text is escaped; an old pause reads unknown.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'agent-pause-provenance.js'), 'utf8');
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
// formatDashboardDateTime is stubbed: the page's own formatter is not under test.
const api = new Function('esc', 'formatDashboardDateTime',
  src + '\nreturn { pauseProvenanceSentence, pauseResumeSentence, pauseProvenanceTip, pauseCardLineHtml };')(esc, t => 'T(' + t + ')');

const alice = { paused: true, pauseProvenance: { pause: { kind: 'dashboard_user', label: 'dashboard user alice', at: '2026-11-17T12:00:00Z', reason: 'manual pause' },
  lastResume: { kind: 'auto_rate_limit', label: 'auto-pause for provider rate limit or backoff', at: '2026-11-16T01:00:00Z' } } };

test('who, when, why and the last resume', () => {
  assert.strictEqual(api.pauseProvenanceSentence(alice), 'paused by dashboard user alice at T(2026-11-17T12:00:00Z): manual pause');
  assert.match(api.pauseResumeSentence(alice), /^last resumed by auto-pause for provider rate limit or backoff at T\(/);
  assert.match(api.pauseProvenanceTip(alice), /^paused by dashboard user alice .*\. last resumed by .*\. $/);
});

test('a pause that predates provenance reads unknown, never a guess', () => {
  const old = { paused: true, pauseProvenance: { pause: { kind: 'unknown', label: 'unknown (before provenance was recorded)' } } };
  assert.strictEqual(api.pauseProvenanceSentence(old), 'paused: unknown (before provenance was recorded)');
  assert.match(api.pauseCardLineHtml({ paused: true }), /paused: unknown \(before provenance was recorded\)/);
});

test('card line only for a paused agent, and the reason is escaped', () => {
  assert.strictEqual(api.pauseCardLineHtml({ paused: false, pauseProvenance: alice.pauseProvenance }), '');
  const evil = { paused: true, pauseProvenance: { pause: { kind: 'auto', label: 'auto-pause (x)', reason: '<img src=x onerror=alert(1)>' } } };
  const h = api.pauseCardLineHtml(evil);
  assert.ok(!h.includes('<img'), h);
  assert.ok(h.includes('&lt;img'), h);
});

test('nothing known gives an empty tip', () => {
  assert.strictEqual(api.pauseProvenanceTip({}), '');
});
