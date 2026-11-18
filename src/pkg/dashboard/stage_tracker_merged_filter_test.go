package dashboard

import "testing"

// The Merged list's filters (since / merged by / repo) read GET
// /api/merged-prs, the endpoint behind the admin MCP's merged_prs_read, and
// the popover draws its answer; the default view is the recent list as
// before.
func TestStageTrackerMergedFilterNode(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, "function esc(s) { if (typeof s !== 'string') return ''; return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+
		jsFunc(t, html, "stageTrackerLocalMidnight")+"\n"+
		jsFunc(t, html, "stageTrackerMergedByText")+"\n"+
		jsFunc(t, html, "stageTrackerMergedFilterQuery")+"\n"+
		jsFunc(t, html, "stageTrackerMergedFilterHtml")+"\n"+
		jsFunc(t, html, "stageTrackerMergedPopHtml")+`
const assert = require('assert');
const now = Date.parse('2026-11-18T21:00:00Z');
assert.strictEqual(stageTrackerMergedFilterQuery({ since: '', merged_by: '', repo: '' }), '', 'the default view reads nothing');
assert.strictEqual(stageTrackerMergedFilterQuery({ merged_by: 'hive' }), 'since=7d&merged_by=hive&limit=200', 'a filter on Recent reads 7 days');
assert.strictEqual(stageTrackerMergedFilterQuery({ since: '24h', repo: 'acme/console' }), 'since=24h&repo=acme%2Fconsole&limit=200');

const bar = stageTrackerMergedFilterHtml({ since: '30d', merged_by: 'human', repo: 'acme/console' }, ['acme/console', 'acme/widgets']);
assert.ok(bar.includes('data-ham-merged-filter="since"') && bar.includes('data-ham-merged-filter="merged_by"') && bar.includes('data-ham-merged-filter="repo"'), bar);
assert.ok(bar.includes('<option value="30d" selected>') && bar.includes('<option value="human" selected>') && bar.includes('<option value="acme/console" selected>'), bar);

const txt = { today: 1, week: 2, total30d: 3, count: 9, tz: 'EST', recent: [{ repo: 'acme/console', number: 5, title: 'fix: recent', merged_by: 'hive', at: now - 60000 }] };
let pop = stageTrackerMergedPopHtml(txt, now, { since: '', merged_by: '', repo: '' });
assert.ok(pop.includes('<span>Recent merges</span>') && pop.includes('>fix: recent<') && pop.includes('ham-merged-filter'), pop);

const f = { since: '24h', merged_by: 'hive', repo: '', result: { matched: 1, counts: { total: 3, by_hive: 2, by_human: 1 }, entries: [
  { repo: 'acme/console', number: 7, title: 'feat: filtered', merged_by: 'hive', author: 'alice', merged_at: new Date(now - 3600000).toISOString() },
] } };
pop = stageTrackerMergedPopHtml(txt, now, f);
assert.ok(pop.includes('<span>Merges</span>') && pop.includes('1 shown · 2 by the hive · 1 by people in the window'), pop);
assert.ok(pop.includes('>feat: filtered<') && pop.includes('PR by @alice') && !pop.includes('fix: recent<'), pop);
assert.ok(stageTrackerMergedPopHtml(txt, now, Object.assign({}, f, { result: null, loading: true })).includes('Loading…'));
assert.ok(stageTrackerMergedPopHtml(txt, now, Object.assign({}, f, { result: null, error: 'since must be within 90 days' })).includes('Could not load the merges: since must be within 90 days'));
assert.ok(stageTrackerMergedPopHtml(txt, now, Object.assign({}, f, { result: { matched: 0, counts: {}, entries: [] } })).includes('No merges match these filters.'));
console.log('ok');
`)
}
