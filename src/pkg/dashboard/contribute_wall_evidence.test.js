'use strict';

// #18721: a wall post's model evidence explains its "failed" figure with the
// model's scenario split, labelled as in the per-run history.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'contribute_landing.go'), 'utf8');

function extractFunction(name) {
  const start = source.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  const end = source.indexOf('\n}\n', start);
  assert.ok(end > start, name + ' has no end');
  return source.slice(start, end + 3);
}

const context = {};
vm.runInNewContext(extractFunction('ccScenarioLabel') + extractFunction('ccModelEvidenceFailTip'), context);

test('tooltip lists the scenario split in run-history labels, then failed runs by kind', () => {
  const tip = context.ccModelEvidenceFailTip({
    runs: 7,
    failure_rate: 4 / 7,
    abandoned: 2,
    scenarios: {
      verdict_complete: 1, env_failure: 2, task_failure: 1, unspecified_failure: 1,
      abandoned_handback: 1, abandoned_disconnect: 1,
    },
    failure_kinds: { environment: 2, task: 1, unspecified: 1 },
  });
  assert.equal(tip, [
    'completed on its own verdict: 1',
    'environment failure: 2',
    'failed on the work: 1',
    'failed (unspecified): 1',
    'handed back (relay asked for new work): 1',
    'handed back (connection lost): 1',
    'Failed runs by kind: environment 2 · task 1 · unspecified 1',
  ].join('\n'));
});

test('an unknown scenario still shows, after the known ones', () => {
  const tip = context.ccModelEvidenceFailTip({ scenarios: { zz_new: 1, idle_complete: 2 } });
  assert.equal(tip, 'completed via idle fallback: 2\nzz new: 1');
});

test('evidence from an older hub (no split) yields no tooltip', () => {
  assert.equal(context.ccModelEvidenceFailTip({ runs: 3, failure_rate: 0.5 }), '');
  assert.equal(context.ccModelEvidenceFailTip(null), '');
});

test('the wall post puts the tooltip on the failed figure and escapes it', () => {
  const body = extractFunction('ccWallPostHTML');
  assert.match(body, /var failTip=ccModelEvidenceFailTip\(p\.model_evidence\);/);
  assert.match(body, /<span'\+\(failTip\?' title="'\+esc\(failTip\)\+'"':''\)\+'>'\+Math\.round\(\(p\.model_evidence\.failure_rate\|\|0\)\*100\)\+'%% failed<\/span>/);
});
