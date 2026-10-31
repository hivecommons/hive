package dashboard

import "testing"

func TestStageTrackerCopyLinkTargets(t *testing.T) {
	html := indexHTML(t)
	script := stageTrackerRowActionsPreamble(t, html) + "\n" + jsFunc(t, html, "stageTrackerMenuTargets") + `
const issue = { repo: 'acme/console', kind: 'issue', number: 7, url: 'https://github.com/acme/console/issues/7',
  linked_prs: [
    { number: 42, url: 'https://github.com/acme/console/pull/42', stage: 50 },
    { number: 43, url: 'https://forge.example/other/project/pull/43', stage: 50 }
  ] };
const rootPR = { repo: 'acme/console', kind: 'pr', number: 44, url: 'https://github.com/acme/console/pull/44',
  linked_issues: [{ number: 7, url: issue.url }] };
_stageTracker = { items: [issue, rootPR] };
const root = { dataset: { hamKey: stageTrackerItemKey(issue) }, parentNode: hamPanel };
const boundary = (key, single) => ({ dataset: single ? { hamTargetKey: key } : { hamSubKey: key }, querySelector() { return null; } });
function eventTarget(row, sub) {
  return { closest(selector) {
    if (selector === '.ham-row[data-ham-key]') return row;
    if (selector.includes('[data-ham-sub-key]')) return sub;
    return null;
  } };
}
const writes = [], toasts = [];
const navigator = { clipboard: { async writeText(text) { writes.push(text); } } };
global.showToast = (text, kind) => toasts.push({ text, kind });
(async () => {
  const cases = [
    [eventTarget(root, null), issue.url, 'issue'],
    [eventTarget({ dataset: { hamKey: stageTrackerItemKey(rootPR) }, parentNode: hamPanel }, null), rootPR.url, 'pr'],
    [eventTarget(root, boundary('acme/console/pr#42', true)), issue.linked_prs[0].url, 'pr'],
    [eventTarget(root, boundary('acme/console/pr#42', false)), issue.linked_prs[0].url, 'pr'],
    [eventTarget(root, boundary('other/project/pr#43', true)), issue.linked_prs[1].url, 'pr'],
    [eventTarget(root, boundary('other/project/pr#43', false)), issue.linked_prs[1].url, 'pr']
  ];
  for (const [target, url, kind] of cases) {
    const mapped = stageTrackerMenuTargets(target);
    assert.strictEqual(mapped.items.length, 1);
    assert.strictEqual(mapped.items[0].kind, kind);
    await runRowAction('copy', mapped.items);
    assert.strictEqual(toasts.at(-1).kind, 'success', toasts.at(-1).text);
    assert.strictEqual(writes.at(-1), url, 'copy follows the exact root, single-linked or nested target');
  }
  const explicit = stageTrackerLinkedPRItem(issue, { repo: 'other/project', number: 45, url: '' });
  assert.strictEqual(explicit.repo, 'other/project', 'explicit linked repo is not replaced by the parent repo');
  await runRowAction('copy', [issue, rootPR, stageTrackerItemByKey(_stageTracker, 'other/project/pr#43')]);
  assert.strictEqual(writes.at(-1), [issue.url, rootPR.url, issue.linked_prs[1].url].join('\n'));
  assert.strictEqual(toasts.at(-1).text, 'Copied 3 links');
  const count = writes.length;
  for (const bad of [
    { ...rootPR, url: issue.url },
    { ...rootPR, url: 'https://github.com/acme/console/pull/99' },
    { ...rootPR, url: '' },
    { ...rootPR, url: 'javascript:alert(1)' },
    { ...issue, url: rootPR.url }
  ]) {
    await runRowAction('copy', [issue, bad]);
    assert.strictEqual(writes.length, count, 'never copy a parent, wrong-kind, wrong-number or partial selection');
    assert.strictEqual(toasts.at(-1).kind, 'error');
  }
  navigator.clipboard.writeText = async () => { throw new Error('denied'); };
  await runRowAction('copy', issue);
  assert.strictEqual(toasts.at(-1).kind, 'error');
  assert.ok(toasts.at(-1).text.includes('denied'), 'clipboard failures use styled app feedback');
})().catch(err => { console.error(err); process.exit(1); });
`
	runStageTrackerRowActionsScript(t, script)
}

func TestStageTrackerSingleLinkedPRCopyBoundary(t *testing.T) {
	html := indexHTML(t)
	script := stageTrackerRowsPreludeJS(t, html) + stageTrackerRowsFixtureJS + `
const issue = { repo: 'acme/console', kind: 'issue', number: 7, title: 'Issue', url: 'https://github.com/acme/console/issues/7',
  stage: 40, linked_prs: [{ number: 42, title: 'PR', url: 'https://forge.example/other/project/pull/42', stage: 50 }] };
const row = stageTrackerRowHtml(issue, stages, ctx);
assert.ok(row.includes('<li class="ham-seg ham-head-pr" data-ham-target-key="other/project/pr#42">'), 'the single covering PR cell has its own menu target without changing flex children');
assert.ok(/data-ham-target-key="other\/project\/pr#42">[\s\S]*?href="https:\/\/forge.example\/other\/project\/pull\/42"/.test(row), 'the target surrounds the actual PR reference');
assert.ok(row.includes('href="' + issue.url + '"'), 'the parent issue keeps its own link');
assert.ok(row.includes('aria-label="Actions for acme/console#7"'), 'the issue kebab remains issue-targeted');
`
	runStageTrackerRowActionsScript(t, script)
}
