package dashboard

import "testing"

func TestStageTrackerCloseIssueEveryStageAndExactTarget(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+"\n"+jsFunc(t, html, "stageTrackerLinkedIssueToolsHtml")+`
const ctx = { user: true, canHold: true };
const issue = { repo: 'hivecommons/dibs', kind: 'issue', number: 527, title: 'Selected issue', url: 'u527' };
for (let stage = 0; stage <= 100; stage++) {
  for (const author_kind of ['human', 'hive', 'clanker']) {
    const it = { ...issue, stage, author_kind, mirrored: true, claimant: { login: 'agent' }, modifiers: ['hold', 'needs-human'],
      linked_prs: [{ number: 528, stage: 85, modifiers: ['needs-decision'] }, { number: 529, stage: 60 }] };
    const close = stageTrackerRowActions(it, ctx).find(a => a.id === 'decide:close-issue');
    assert.ok(close && close.enabled, 'every open issue stage/author/hold has Close: ' + stage);
    assert.deepStrictEqual(stageTrackerActionRequest(it, close.id, { comment: 'done' }), {
      url: '/api/repos/hivecommons/dibs/items/527/decision',
      body: { decision: 'close-issue', type: 'issue', comment: 'done' },
    }, 'never chooses covering PR or another issue');
    for (const ro of [{ user: false, canHold: false }, { user: true, canHold: false }]) {
      const denied = stageTrackerRowActions(it, ro).find(a => a.id === close.id);
      assert.ok(denied && !denied.enabled && denied.why, 'permission denial stays visible');
    }
  }
}
for (const it of [{ ...issue, state: 'closed' }, { ...issue, kind: 'pr' }]) {
  assert.ok(!stageTrackerRowActions(it, ctx).some(a => a.id === 'decide:close-issue'));
  assert.strictEqual(stageTrackerActionRequest(it, 'decide:close-issue'), null);
}
const pr = { repo: 'other/repository', kind: 'pr', number: 900, linked_issues: [
  { number: 901, title: 'First issue', state: 'open', url: 'u901' },
  { number: 902, title: 'Selected nested issue', state: 'open', url: 'u902' },
  { number: 903, title: 'Closed issue', state: 'closed', url: 'u903' },
] };
const tools = stageTrackerLinkedIssueToolsHtml(pr);
assert.ok(tools.includes('other/repository/issue#901') && tools.includes('other/repository/issue#902'));
assert.ok(!tools.includes('issue#903'), 'no close menu for a closed linked issue');
const nested = stageTrackerItemByKey({ items: [pr] }, 'other/repository/issue#902');
assert.strictEqual(nested.title, 'Selected nested issue');
assert.strictEqual(stageTrackerActionRequest(nested, 'decide:close-issue').url, '/api/repos/other/repository/items/902/decision');
assert.strictEqual(stageTrackerItemByKey({ items: [issue, pr] }, 'hivecommons/dibs/issue#527'), issue, 'canonical root DTO wins');
const pend = { keys: ['other/repository/issue#902'], key: 'other/repository/issue#902', state: stageTrackerItemState(nested), at: Date.now() };
const refreshed = { ...pr, linked_issues: pr.linked_issues.map(li => li.number === 902 ? { ...li, state: 'closed' } : li) };
assert.strictEqual(stageTrackerPendingLive([pend], { items: [refreshed] }, Date.now()).length, 0, 'canonical closed linked issue settles pending without hiding its PR');
assert.strictEqual(refreshed.number, 900);
`)
}

func TestStageTrackerCloseIssueConfirmationPendingAndFailures(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowActionsPreamble(t, html)+stageTrackerConfirmOptOutJS(t, html)+`
const issue = { repo: 'hivecommons/dibs', kind: 'issue', number: 527, title: 'Selected issue', url: 'u527', stage: 85, mirrored: true,
  linked_prs: [{ number: 528, stage: 85, modifiers: ['needs-decision'] }] };
_stageTracker = { items: [issue] };
const messages = [], results = [], calls = [];
global.showToast = (msg, type) => { messages.push({ msg, type }); return null; };
global.dismissToast = () => {};
function stageTrackerProgressToast() { return { update() {}, done() {} }; }
let refreshes = 0;
function stageTrackerAfterActions() { refreshes++; }
function stageTrackerResultToast(result) { results.push(result); return { result }; }
function stageTrackerDecidePending(pend) { pend.decided = true; }
let releasePrompt, releaseFetch;
global.hivePrompt = cfg => { dialogs.push({ cfg }); return new Promise(resolve => { releasePrompt = resolve; }); };
global.fetch = (url, init) => { calls.push({ url, body: JSON.parse(init.body) }); return new Promise(resolve => { releaseFetch = resolve; }); };
const tick = async () => { for (let i = 0; i < 10; i++) await Promise.resolve(); };
(async () => {
  const first = runRowAction('decide:close-issue', issue);
  assert.strictEqual(dialogs.length, 1);
  assert.ok(dialogs[0].cfg.label.includes('hivecommons/dibs#527') && dialogs[0].cfg.label.includes('Selected issue'));
  assert.ok(dialogs[0].cfg.label.includes('linked pull requests and other issues stay unchanged'));
  assert.strictEqual(dialogs[0].cfg.danger, true);
  assert.strictEqual(calls.length, 0, 'confirmation before any mutation');
  await runRowAction('decide:close-issue', issue);
  assert.strictEqual(dialogs.length, 1, 'double click cannot open another confirmation');
  releasePrompt('Finished separately');
  await tick();
  assert.strictEqual(calls.length, 1);
  assert.strictEqual(calls[0].url, '/api/repos/hivecommons/dibs/items/527/decision');
  assert.strictEqual(calls[0].body.comment, 'Finished separately');
  assert.strictEqual(_stageTrackerPending[0].caption, 'Closing…');
  assert.strictEqual(issue.state, undefined, 'no optimistic false close');
  assert.strictEqual(results.length, 0, 'no success before remote response');
  await runRowAction('decide:close-issue', issue);
  assert.strictEqual(calls.length, 1, 'pending prevents double submit');
  releaseFetch({ ok: true, json: async () => ({ status: 'closed', result: { action: 'closed', target: { repo: issue.repo, number: 527, kind: 'issue' }, outcome: 'Issue closed as completed.' } }) });
  const done = await first;
  assert.strictEqual(done.results[0].ok, true);
  assert.strictEqual(results[0].target.number, 527);
  assert.strictEqual(_stageTrackerPending[0].caption, 'Closed · leaving list');
  assert.strictEqual(refreshes, 1, 'canonical tracker refresh requested');
  assert.strictEqual(_stageTracker.items[0], issue, 'existing closed-row policy owns removal; no local hide');
  await runRowAction('decide:close-issue', issue);
  assert.strictEqual(dialogs.length, 1, 'waiting for canonical DTO still prevents duplicate close');
  _stageTrackerPending = [];
  for (const error of ['write permission required', 'read-only', 'CSRF validation failed', 'forge service unavailable', 'API write denied']) {
    const attempt = runRowAction('decide:close-issue', issue);
    releasePrompt('');
    await tick();
    releaseFetch({ ok: false, status: 403, json: async () => ({ error }) });
    const failed = await attempt;
    assert.strictEqual(failed.results[0].ok, false);
    assert.strictEqual(_stageTrackerPending.length, 0, 'error clears pending');
    assert.ok(results.at(-1).outcome.includes(error), 'standard error notification includes server reason');
    assert.strictEqual(_stageTracker.items[0], issue, 'failed close does not hide issue');
  }
  for (const data of [{}, { status: 'refused', result: { action: 'refused', target: { repo: issue.repo, number: 527, kind: 'issue' }, outcome: 'Close refused: repo paused' } }]) {
    const attempt = runRowAction('decide:close-issue', issue);
    releasePrompt('');
    await tick();
    releaseFetch({ ok: true, json: async () => data });
    await attempt;
    assert.ok(results.at(-1).action === 'error' || results.at(-1).action === 'refused', 'no fake success');
    assert.strictEqual(_stageTrackerPending.length, 0);
    assert.strictEqual(_stageTracker.items[0], issue);
  }
  const cancelled = runRowAction('decide:close-issue', issue);
  const before = calls.length;
  releasePrompt(null);
  assert.strictEqual((await cancelled).cancelled, true);
  assert.strictEqual(calls.length, before, 'cancel writes nothing');
  const n = dialogs.length;
  await runRowAction('decide:close-issue', [issue, { ...issue, number: 999 }]);
  assert.strictEqual(dialogs.length, n, 'single exact selected issue only; no bulk fan-out');
})().catch(e => { console.error(e); process.exit(1); });
`)
}
