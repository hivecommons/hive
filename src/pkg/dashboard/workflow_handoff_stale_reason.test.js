// hivecommons/hive#18916: a replayed hand-off decline carries its age and
// session, and a stale one never reads as the item's current state.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'config-workflows-handoff.js'), 'utf8');
// The file is one pure function with no dependency on the page: no stubs.
const workflowHandoffNote = new Function(src + '\nreturn workflowHandoffNote;')();

test('a current decline names its age and session and what happens next', () => {
  const n = workflowHandoffNote({ ref: 'o/r#7', reason: 'q declined the hand-off: no', reason_age: '3h 5m', reason_session: 'q@2026-11-17T01:00:00Z', retrying: true, stale: false }, 'writer');
  assert.match(n, /not started: q declined the hand-off: no\./);
  assert.match(n, /Decided 3h 5m ago in session q@2026-11-17T01:00:00Z\./);
  assert.match(n, /another agent/);
  assert.doesNotMatch(n, /stale/);
});

test('a stale decline is history and never shown as not started', () => {
  const n = workflowHandoffNote({ ref: 'o/r#7', reason: 'old text', reason_age: '9h 0m', reason_session: 's1', stale: true, retrying: true }, 'writer');
  assert.match(n, /^Earlier hand-off of o\/r#7 \(stale\): old text\./);
  assert.doesNotMatch(n, /not started/);
  assert.doesNotMatch(n, /Send to/);
  assert.match(n, /not its state now/);
});

test('a record from before provenance was kept says so', () => {
  const n = workflowHandoffNote({ ref: 'o/r#1', reason: 'x' }, 'p');
  assert.match(n, /Decided at an unknown time in session unknown \(before provenance was recorded\)\./);
});
