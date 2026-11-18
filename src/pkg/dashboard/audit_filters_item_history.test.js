// Audit log server-side filters and the item history dialog: the audit
// view sends repo/number/action/contributor/since to GET /api/audit (the
// admin MCP's audit_log arguments), and a merged or closed item's history
// (GET /api/audit/item, the MCP's item_history_read) opens from its audit
// row, the filter row and the Merged list. Asserts against the shipped
// index.html.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

function fn(name) {
  return pick(new RegExp('function ' + name + '\\([^)]*\\) \\{[\\s\\S]*?\\n    \\}\\n'), name);
}

const esc = s => typeof s === 'string' ? s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;') : '';
const ctx = vm.createContext({ esc, formatDashboardDateTime: d => d.toISOString(), renderAuditRow: e => '<tr><td>' + esc(e.action) + '</td></tr>' });
vm.runInContext([fn('auditEntryHistoryLink'), fn('auditServerQueryString'), fn('auditServerScanActive'), fn('auditServerMatchNote'), fn('auditItemHistoryHtml')].join('\n'), ctx);

test('the filter row sends every filter, and the sensitive chip, to GET /api/audit', () => {
  assert.strictEqual(ctx.auditServerQueryString({}, false), '');
  assert.strictEqual(ctx.auditServerQueryString({ repo: 'o/r', number: '42', action: 'pr_*', contributor: 'alice', since: '7d' }, true),
    '?repo=o%2Fr&number=42&action=pr_*&contributor=alice&since=7d&sensitive=1');
  assert.strictEqual(ctx.auditServerQueryString({ repo: '  ', number: '' }, false), '');
  const fetchLine = pick(/var resp = await fetch\('\/api\/audit' \+ auditServerQueryString\(_auditServerFilters, _auditSensitiveOnly\)\);/, 'fetchAuditLog filter fetch');
  assert.ok(fetchLine);
  for (const id of ['audit-filter-repo', 'audit-filter-number', 'audit-filter-action', 'audit-filter-contributor', 'audit-filter-since', 'audit-item-history-btn']) {
    assert.ok(html.includes('id="' + id + '"'), id + ' missing');
  }
});

test('the count says how many rows the server matched', () => {
  assert.strictEqual(ctx.auditServerMatchNote(null), '');
  assert.strictEqual(ctx.auditServerMatchNote({ active: true, matched: 250, truncated: true, shown: 200, scanned: 'files' }), ' · 250 matched (newest 200 shown)');
  assert.strictEqual(ctx.auditServerMatchNote({ active: true, matched: 3, truncated: false, scanned: 'ring' }), ' · 3 matched · recent only');
});

test('an audit row that names an item links to its history', () => {
  assert.strictEqual(ctx.auditEntryHistoryLink({ action: 'repos_rescan' }), '');
  const link = ctx.auditEntryHistoryLink({ action: 'pr_closed', repo: 'o/r', target: 9 });
  assert.ok(link.includes('data-action="openAuditItemHistory"') && link.includes('data-arg0="o/r"') && link.includes('data-arg1="9"') && link.includes('data-arg-types="s,j"'), link);
  assert.ok(fn('renderAuditRow').includes('historyLink'), 'renderAuditRow appends the link');
});

test('the item history body shows state, who merged, the monitor row and the rows', () => {
  const merged = ctx.auditItemHistoryHtml({ state: 'merged', merged: { merged_at: '2026-11-18T19:58:16Z', merged_by: 'hive', lane: 'issue' }, linked_issue: 41, entries: [{ action: 'pr_merged' }, { action: 'agent_issue_claimed' }], scanned: 'files' });
  assert.ok(merged.includes('<b>State:</b> merged') && merged.includes('by the hive') && merged.includes('lane issue') && merged.includes('#41'), merged);
  assert.ok(merged.includes('<td>pr_merged</td>') && merged.includes('2 audit entries'), merged);
  const open = ctx.auditItemHistoryHtml({ state: 'open', tracker: { stage_label: 'In review', reason: 'awaiting review' }, entries: [] });
  assert.ok(open.includes('In review — awaiting review') && open.includes('recorded no audit entries'), open);
  const human = ctx.auditItemHistoryHtml({ state: 'merged', merged: { merged_at: '2026-11-18T19:58:16Z', merged_by: 'alice' }, entries: [] });
  assert.ok(human.includes('by @alice'), human);
  const opener = fn('openAuditItemHistory');
  assert.ok(opener.includes("fetch('/api/audit/item?repo=' + encodeURIComponent(repo) + '&number='") && opener.includes('hiveWireDialog(overlay'), opener);
});

test('the Merged list offers each PR\'s history', () => {
  const pop = pick(/function stageTrackerMergedPopHtml\([\s\S]*?\n    \}\n/, 'stageTrackerMergedPopHtml');
  assert.ok(pop.includes('class="ham-merged-hist" data-action="openAuditItemHistory"'), pop);
});

// A filter that makes the server scan every retained file turns the view
// into a query: the section's 30s poll skips it (review on #19153).
test('a scanning filter stops the audit poll; contributor and sensitive alone do not', () => {
  assert.strictEqual(ctx.auditServerScanActive({}), false);
  assert.strictEqual(ctx.auditServerScanActive({ contributor: 'alice' }), false);
  for (const k of ['repo', 'number', 'action', 'since']) {
    assert.strictEqual(ctx.auditServerScanActive({ [k]: 'x' }), true, k);
  }
  assert.strictEqual(ctx.auditServerScanActive({ repo: '  ' }), false);
  assert.ok(html.includes("dashboardSetInterval('audit-log', auditPollTick, AUDIT_POLL_MS)"), 'the poll must go through auditPollTick');
  assert.ok(/function auditPollTick\(\) \{\s+if \(auditServerScanActive\(_auditServerFilters\)\) return Promise\.resolve\(\);\s+return fetchAuditLog\(\);/.test(html), 'auditPollTick must skip a scanning query');
});
