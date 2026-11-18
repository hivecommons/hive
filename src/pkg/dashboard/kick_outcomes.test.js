const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, 'static-src/js/kick-outcomes.js'), 'utf8');
const esc = value => String(value).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
function build(document = {}, fetch = () => {}, formatDashboardDateTime = date => date.toISOString()) {
  return new Function('esc', 'document', 'fetch', '_inceptionAuthHeaders', 'formatDashboardDateTime', source + '\nreturn {kickOutcomesHtml, refreshKickOutcomes};')(esc, document, fetch, () => ({Authorization: 'Bearer fixture'}), formatDashboardDateTime);
}
const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, '../../scripts/fixtures/render-evidence/api/kick-outcomes.json'), 'utf8'));

test('shipped dashboard wires outcomes into both full render and status refresh', () => {
  const html = fs.readFileSync(path.join(__dirname, 'static/index.html'), 'utf8');
  assert.ok(html.includes('function kickOutcomesHtml(data)'));
  assert.ok(html.includes('aria-label="Recent kick outcomes"'));
  assert.equal((html.match(/refreshKickOutcomes\(a\.name\);/g) || []).length, 2);
  assert.ok(html.includes('overflow-wrap: anywhere'));
});

test('all seven outcome kinds and real evidence render', () => {
  const html = build().kickOutcomesHtml(fixture);
  for (const label of ['Opened PR', 'Pushed commit', 'Commented', 'Stood down', 'Refused', 'Still running', 'Ended — no visible action']) assert.ok(html.includes(label), label);
  for (const field of ['fixture-operator', 'PR #42', '0123456789abcdef', 'paused', 'No eligible work.']) assert.ok(html.includes(field), field);
  assert.ok(html.includes('&lt;This is untrusted agent text.&gt;'));
});

test('timestamps use the shared dashboard formatter and preserve unknown-time fallbacks', () => {
  const timestamps = [];
  const {kickOutcomesHtml} = build({}, () => {}, date => {
    timestamps.push(date.toISOString());
    return '<selected timezone & clock>';
  });
  const kick = {kind: 'stood_down', at: '2026-11-18T12:00:00Z', started_at: '2026-11-18T11:00:00Z', ended_at: '2026-11-18T12:01:00Z'};
  const html = kickOutcomesHtml({detail: true, kicks: [kick]});
  assert.deepEqual(timestamps.sort(), [kick.at, kick.started_at, kick.ended_at].map(value => new Date(value).toISOString()).sort());
  assert.equal((html.match(/&lt;selected timezone &amp; clock&gt;/g) || []).length, 3);
  timestamps.length = 0;
  const unknown = kickOutcomesHtml({detail: true, kicks: [{kind: 'stood_down', at: 'invalid', ended_at: 'invalid'}]});
  assert.equal((unknown.match(/Unknown time/g) || []).length, 3);
  assert.equal(timestamps.length, 0);
});

test('read-only response renders only its latest summary, even with stray detail fields', () => {
  const html = build().kickOutcomesHtml({detail: false, latest: {...fixture.kicks[0], summary: 'Latest summary'}, kicks: fixture.kicks});
  assert.ok(html.includes('Latest summary'));
  for (const hidden of ['fixture-operator', 'href=', 'Trigger:', 'Started:', 'blockquote', 'Branch:']) assert.ok(!html.includes(hidden), hidden);
  assert.equal((html.match(/<li>/g) || []).length, 1);
});

test('empty history, malicious quotes, summaries and link protocols are safe', () => {
  assert.match(build().kickOutcomesHtml({detail: true, kicks: null}), /No recorded kicks/);
  assert.match(build().kickOutcomesHtml({detail: false}), /No recorded kicks/);
  const html = build().kickOutcomesHtml({detail: true, kicks: [{kind: 'refused', summary: '<img onerror=x>', quote: '<script>bad()</script>', pr_url: 'javascript:alert(1)', branch: '<b>branch</b>', refusal_code: '<x>'}]});
  assert.ok(!html.includes('<script>') && !html.includes('<img') && !html.includes('href='));
  assert.ok(html.includes('&lt;script&gt;') && html.includes('&lt;b&gt;'));
});

function panel(agent) {
  const body = {innerHTML: ''};
  return {dataset: {agent}, isConnected: true, querySelector: () => body, body};
}

test('refresh authenticates, encodes agent, throttles and uses server detail flag', async () => {
  const p = panel('a/b');
  let calls = 0;
  const {refreshKickOutcomes} = build({querySelector: () => p}, async (url, opts) => {
    calls++;
    assert.equal(url, '/api/kick-outcomes?agent=a%2Fb');
    assert.equal(opts.headers.Authorization, 'Bearer fixture');
    return {ok: true, json: async () => ({detail: false, latest: fixture.kicks[0]})};
  });
  await refreshKickOutcomes('a/b');
  await refreshKickOutcomes('a/b');
  assert.equal(calls, 1);
  assert.ok(!p.body.innerHTML.includes('href='));
});

test('error clears stale evidence and disconnected panels ignore late responses', async () => {
  const p = panel('agent');
  p.body.innerHTML = 'old evidence';
  await build({querySelector: () => p}, async () => ({ok: false})).refreshKickOutcomes('agent');
  assert.match(p.body.innerHTML, /unavailable/);
  p.dataset.fetchedAt = '0';
  p.body.innerHTML = 'unchanged';
  await build({querySelector: () => p}, async () => {
    p.isConnected = false;
    return {ok: true, json: async () => fixture};
  }).refreshKickOutcomes('agent');
  assert.equal(p.body.innerHTML, 'unchanged');
});
