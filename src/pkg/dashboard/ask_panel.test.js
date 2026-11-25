// Ask AI help panel view model (#19184). The shared component in
// pkg/askai/web renders only what askViewModel returns, with textContent, so
// this pins what reaches the page: answers need an https source, everything
// else becomes a notice with the docs link.
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

const { askViewModel } = require(path.join(__dirname, '..', 'askai', 'web', 'ask-panel.js'));

test('an answer keeps only https sources and names their host', () => {
  const v = askViewModel({
    outcome: 'ok', answer: 'Open Settings.', uncertain: true, conversation: 'h1', docs_url: 'https://docs.hivecommons.dev/',
    sources: [{ title: 'Settings', url: 'https://docs.hivecommons.dev/settings/' }, { title: 'x', url: 'javascript:alert(1)' }, { url: 'http://insecure.example/' }],
  });
  assert.equal(v.kind, 'answer');
  assert.equal(v.text, 'Open Settings.');
  assert.equal(v.uncertain, true);
  assert.equal(v.conversation, 'h1');
  assert.deepEqual(v.sources, [{ label: 'Settings', host: 'docs.hivecommons.dev', href: 'https://docs.hivecommons.dev/settings/' }]);
});

test('an answer with no usable source is shown as no cited answer', () => {
  const v = askViewModel({ outcome: 'ok', answer: 'Trust me.', sources: [{ url: 'ftp://x' }] });
  assert.equal(v.kind, 'notice');
  assert.match(v.text, /No cited answer/);
  assert.equal(v.uncertain, false);
  assert.deepEqual(v.sources, []);
});

test('every other outcome is a notice carrying the backend message', () => {
  for (const outcome of ['rate_limited', 'breaker_open', 'upstream_error', 'refused_secret_shape', 'not_consented', 'relay_disabled']) {
    const v = askViewModel({ outcome, message: 'msg ' + outcome, retry_after_seconds: 7, answer: '<b>ignored</b>' });
    assert.equal(v.kind, 'notice');
    assert.equal(v.text, 'msg ' + outcome);
    assert.deepEqual(v.sources, []);
  }
  assert.equal(askViewModel({ outcome: 'rate_limited', retry_after_seconds: 7 }).retryAfter, 7);
  assert.equal(askViewModel(null).kind, 'notice');
  assert.match(askViewModel(undefined).text, /unavailable/);
});

const { answerBlocks } = require(path.join(__dirname, '..', 'askai', 'web', 'ask-panel.js'));

test('an answer splits into paragraphs, lists, headings and fenced code', () => {
  const blocks = answerBlocks('## Setup\nOpen Settings.\nThen save.\n\n- one\n- two\n\n1. first\n\n```yaml\nkey: <b>v</b>\n\n  nested: 1\n```\nDone.');
  assert.deepEqual(blocks, [
    { type: 'h', text: 'Setup' },
    { type: 'p', text: 'Open Settings.\nThen save.' },
    { type: 'list', ordered: false, items: ['one', 'two'] },
    { type: 'list', ordered: true, items: ['first'] },
    { type: 'code', text: 'key: <b>v</b>\n\n  nested: 1' },
    { type: 'p', text: 'Done.' },
  ]);
});

test('an unclosed fence still keeps its code, and empty text has no blocks', () => {
  assert.deepEqual(answerBlocks('```\nx = 1'), [{ type: 'code', text: 'x = 1' }]);
  assert.deepEqual(answerBlocks(''), []);
  assert.deepEqual(answerBlocks(null), []);
});
