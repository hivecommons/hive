// The human gate's one source of truth (hivecommons/hive#18769). A row
// showed "Needs human · review" with Approve / Request changes… / Let hive
// decide while the click answered "nothing to act on — #18667 is not
// waiting on that decision": the pill read the cached reviewer verdict, the
// action read the live park. This runs the shipped pill renderer and the
// shipped action mapping over testdata/gate_states.json — the table the
// server rule (stage_tracker_gate_test.go) runs too — and proves that every
// verb a row draws is one the action accepts, and that a row nobody owes a
// decision on names what it waits on instead.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const table = JSON.parse(fs.readFileSync(path.join(__dirname, 'testdata', 'gate_states.json'), 'utf8'));

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}
const fn = (name, args) => pick(new RegExp('function ' + name + '\\(' + args + '\\) \\{[\\s\\S]*?\\n    \\}\\n'), name);
const line = name => pick(new RegExp('const ' + name + ' = [^\\n]*\\n'), name);

const src = [
  'var toasts = []; var fetched = 0; var _stageTrackerEtag = "e"; var _stageTrackerSig = "s"; var _stageTracker = null;',
  'function showToast(msg, type) { toasts.push(type + ":" + msg); }',
  'function fetchStageTracker() { fetched++; }',
  'function esc(s) { return String(s == null ? "" : s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }',
  line('STAGE_TRACKER_TERMINAL'),
  line('STAGE_TRACKER_DECISION_STAGES'),
  line('STAGE_TRACKER_DELEGATE_ALL_MIN'),
  pick(/function stageTrackerRefOf\(item\) \{[^\n]*\n/, 'stageTrackerRefOf'),
  pick(/function stageTrackerItemKey\(it\) \{[^\n]*\n/, 'stageTrackerItemKey'),
  fn('stageTrackerLinkedPRItem', 'issue, lp'),
  fn('stageTrackerLinkedPROpen', 'lp, issue'),
  fn('stageTrackerNestedOpenPRs', 'issue'),
  fn('stageTrackerPRParked', 'it'),
  fn('stageTrackerParkedNestedPRs', 'item'),
  fn('stageTrackerDecisionTarget', 'item'),
  fn('stageTrackerDelegateTargets', 'item'),
  'function stageTrackerActionLabel(id) { return id === "decide:delegate" ? "Let hive decide" : id; }',
  fn('stageTrackerNothingToActOn', 'id, rows'),
  // The two modules under test, as shipped.
  fs.readFileSync(path.join(__dirname, 'static-src/js/tracker-human-gate.js'), 'utf8'),
  fs.readFileSync(path.join(__dirname, 'static-src/js/tracker-gate-truth.js'), 'utf8'),
  `return { reasons: stageTrackerHumanGateReasons, gateHtml: stageTrackerHumanGateHtml, waitingHtml: stageTrackerGateWaitingHtml, waitText: stageTrackerGateWaitText,
    targets: stageTrackerGateTargets, accepts: stageTrackerGateAccepts, answered: stageTrackerGateAnswered, stuckWord: stageTrackerStuckChecksWord,
    nothing: stageTrackerNothingToActOn, VERBS: STAGE_TRACKER_GATE_VERBS, toasts, fetched: () => fetched, sig: () => _stageTrackerSig + _stageTrackerEtag };`,
].join('\n');
const api = new Function(src)();

const reasonsOf = h => ((/data-ham-gate-reasons="([^"]*)"/.exec(h) || [])[1] || '').split(' ').filter(Boolean);
const verbsOf = h => Array.from(h.matchAll(/data-ham-act="([^"]*)"/g)).map(m => m[1]).filter((v, i, a) => a.indexOf(v) === i);

// The shipped bundle carries the module, and the action runs the mapping
// the pill filters on — one function, not two copies.
test('the pill and the action share one mapping', () => {
  assert.ok(html.includes('function stageTrackerGateAccepts(item, id)'), 'tracker-gate-truth.js is in the bundle');
  assert.ok(html.includes('items.map(it => stageTrackerGateTargets(it, id))'), 'runRowAction maps its decide targets through stageTrackerGateTargets');
  assert.ok(html.includes('stageTrackerGateAccepts(item, v.id)'), 'the pill draws only the verbs stageTrackerGateAccepts allows');
});

for (const tc of table.cases) {
  test('gate table: ' + tc.name, () => {
    const item = tc.item;
    const row = api.gateHtml(item, item, Date.now());
    assert.deepStrictEqual(reasonsOf(row), tc.want.reasons, 'reasons on the pill: ' + row);
    assert.deepStrictEqual(verbsOf(row), tc.want.actions, 'verbs on the pill: ' + row);
    assert.strictEqual(!!row, tc.want.reasons.length > 0, 'a pill renders exactly when a person is owed something');
    // Offered ⇔ accepted: each drawn decide verb has something to act on,
    // and each verb the reasons would list but the pill left out has not.
    for (const id of verbsOf(row)) {
      if (id.indexOf('decide:') === 0) assert.ok(api.targets(item, id).length > 0, id + ' is drawn but runRowAction would find nothing to act on');
    }
    for (const key of tc.want.reasons) {
      for (const id of api.VERBS[key]) {
        if (verbsOf(row).indexOf(id) >= 0 || (item.kind !== 'pr' && (id === 'decide:approve' || id === 'gate:request-changes'))) continue;
        assert.strictEqual(api.accepts(item, id), false, id + ' would act but the pill does not offer it');
      }
    }
    assert.strictEqual(api.answered(item), tc.want.answered, 'answered sign-off');
  });
}

test('hive#18667: approved head, stuck required checks — no decision buttons, the blocker named', () => {
  const tc = table.cases[0];
  // The row as the server sends it: the payload names the blocker.
  const item = Object.assign({}, tc.item, { human_gate: { reasons: [], actions: [], answered: true, answered_by: 'clubanderson',
    waiting_on: { kind: 'checks-stuck', checks: ['PR Type', 'PR Issue'], text: 'required checks stuck: PR Type, PR Issue (in progress 3h, far longer than a normal run) · CI running (54/69)' } } });
  assert.strictEqual(api.gateHtml(item, item, Date.now()), '', 'no gate pill: nobody is asked to decide');
  const pill = api.waitingHtml(item);
  assert.ok(pill.includes('class="ham-badge ham-gate-waiting"') && pill.includes('data-ham-gate-waiting="checks-stuck"'), pill);
  assert.ok(pill.includes('>Approved by @clubanderson · waiting on required checks stuck: PR Type, PR Issue · CI running (54/69)<'), 'the pill names the approver and the stuck checks: ' + pill);
  assert.ok(!/data-ham-act=/.test(pill) && !/<button/.test(pill), 'the pill offers no action: ' + pill);
  assert.ok(pill.includes('Nothing here needs a decision.') && pill.includes('in progress 3h'), 'the hover carries the full sentence: ' + pill);
  for (const id of ['decide:approve', 'decide:delegate']) assert.strictEqual(api.accepts(item, id), false, id);

  // Without the server field (an older payload) the CI track's own words name the stuck checks.
  assert.strictEqual(api.waitText(tc.item), "required checks: 2 running · 2 stuck ('PR Type', 'PR Issue')");
  assert.strictEqual(api.stuckWord({ checks_stuck: ['PR Type', 'PR Issue'] }, 2), "'PR Type', 'PR Issue' stuck");
  assert.strictEqual(api.stuckWord({ checks_pending: ['build'] }, 1), '', 'no stuck check, no stuck word');

  // The same head while the hive still parks it on the reviewer's question.
  const parked = Object.assign({}, tc.item, { stage: 85, modifiers: ['needs-decision'], human_approver: '', reason: '' });
  const row = api.gateHtml(parked, parked, Date.now());
  assert.deepStrictEqual(reasonsOf(row), ['review']);
  assert.deepStrictEqual(verbsOf(row), ['decide:approve', 'gate:request-changes', 'decide:delegate']);
  assert.strictEqual(api.waitingHtml(parked), '', 'an open ask is the gate pill, never the waiting pill');
});

test('a verb clicked in a stale view says what the item is waiting on and refreshes the tracker', () => {
  const tc = table.cases[0];
  const item = Object.assign({}, tc.item, { human_gate: { reasons: [], actions: [], answered: true,
    waiting_on: { kind: 'checks-stuck', text: 'required checks stuck: PR Type, PR Issue' } } });
  api.toasts.length = 0;
  const before = api.fetched();
  api.nothing('decide:delegate', [item]);
  assert.deepStrictEqual(api.toasts, ['warning:Let hive decide: nothing to act on — hivecommons/hive#18667 is waiting on required checks stuck: PR Type, PR Issue']);
  assert.strictEqual(api.fetched(), before + 1, 'the tracker is re-read');
  assert.strictEqual(api.sig(), '', 'the cached signature and ETag are dropped so the row re-renders');
  // Nothing known about the wait: the old sentence stands.
  api.toasts.length = 0;
  api.nothing('decide:delegate', [{ repo: 'acme/api', number: 9, kind: 'issue', stage: 15 }]);
  assert.deepStrictEqual(api.toasts, ['warning:Let hive decide: nothing to act on — acme/api#9 is not waiting on that decision']);
});

// hivecommons/hive#18889: the pill still names what parked the row, but the
// verbs that decide on the head are disabled, with the reason, while the
// CURRENT head's CI or review is still running — and the pill says so in
// words ("waiting on CI running 2/5"), not only in a hover.
test('hive#18889: mid-flight head — decision verbs disabled with the reason, release stays live', () => {
  const parked = { repo: 'acme/api', number: 5, kind: 'pr', stage: 85, head: 'aaa1111', author_kind: 'hive', modifiers: ['needs-decision'], needs_human: true,
    reason: 'contributor-lane:no-linked-issue' };
  const disabledOf = h => Array.from(h.matchAll(/data-ham-act="([^"]*)"([^>]*)>/g)).filter(m => /aria-disabled="true"/.test(m[2])).map(m => m[1]);

  // Settled head: nothing disabled, no wait chip.
  const settled = api.gateHtml(parked, parked, Date.now());
  assert.deepStrictEqual(disabledOf(settled), []);
  assert.ok(!settled.includes('ham-gate-wait'), settled);

  // CI running on the current head.
  const ci = Object.assign({}, parked, { mid_flight: { kind: 'ci-running', text: 'CI running 2/5', head: 'aaa1111', park: 'it links no issue' } });
  const row = api.gateHtml(ci, ci, Date.now());
  assert.deepStrictEqual(reasonsOf(row), ['decision'], 'the pill still says what parked it');
  assert.deepStrictEqual(verbsOf(row), ['gate:decide', 'decide:approve', 'decide:delegate']);
  assert.deepStrictEqual(disabledOf(row), ['gate:decide', 'decide:approve', 'decide:delegate'], 'Decide…, Approve as-is and Let hive decide are all disabled');
  assert.ok(row.includes('class="ham-gate-wait"') && row.includes('>waiting on CI running 2/5<'), 'the reason is visible on the pill: ' + row);
  assert.ok(row.includes('parked because it links no issue'), 'the real park reason rides the hover: ' + row);
  assert.ok(row.includes('title="Waiting on CI running 2/5: the decision opens when CI and review finish on the current head'), row);

  // Review queued on the current head, and a push that left the old park behind.
  for (const text of ['review queued on aaa1111 (0/5)', 'CI and review not yet run on aaa1111']) {
    const it = Object.assign({}, parked, { mid_flight: { kind: 'x', text, head: 'aaa1111' } });
    const h = api.gateHtml(it, it, Date.now());
    assert.deepStrictEqual(disabledOf(h), ['gate:decide', 'decide:approve', 'decide:delegate'], text);
    assert.ok(h.includes('>waiting on ' + text + '<'), h);
  }

  // A hold is a person's to release whatever the head is doing.
  const held = Object.assign({}, ci, { modifiers: ['needs-decision', 'hold'], hold: { by: 'octocat' } });
  const heldRow = api.gateHtml(held, held, Date.now());
  assert.ok(verbsOf(heldRow).includes('release') && !disabledOf(heldRow).includes('release'), heldRow);
});
