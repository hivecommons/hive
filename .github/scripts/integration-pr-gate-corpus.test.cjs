'use strict';
// Replays the shared golden corpus of PR title verdicts against the JS
// verifier. The Go port in src/pkg/prgate replays the SAME file
// (verdict_corpus_test.go), so the hive-posted `PR Type` / `PR Issue` gate and
// the workflow verifier cannot drift apart: change either implementation and
// one of the two tests fails until the corpus (or the code) is reconciled.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const policy = require('./integration-governance.cjs');

const corpus = JSON.parse(fs.readFileSync(path.join(__dirname, 'testdata', 'pr-gate', 'verdict-corpus.json'), 'utf8'));

test('the corpus is large enough to mean something', () => {
  assert.ok(Array.isArray(corpus) && corpus.length >= 50);
  assert.ok(corpus.some(c => c.type.conclusion === 'failure'));
  assert.ok(corpus.some(c => c.issue.conclusion === 'failure'));
});

test('JS verdicts match the shared golden corpus', () => {
  for (const c of corpus) {
    assert.deepEqual(policy.prTypeVerdict(c.title), c.type, `PR Type for ${JSON.stringify(c.title)}`);
    assert.deepEqual(policy.prIssueVerdict(c.title), c.issue, `PR Issue for ${JSON.stringify(c.title)}`);
  }
});
