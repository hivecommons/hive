package dashboard

import (
	"strings"
	"testing"
)

// The actor icon on a lit stage pill comes from the server's stage_actor
// kind, never from a guess on the login: hive → honeycomb, clanker → robot,
// human → the same person glyph "Needs human decision" uses, bot → cog,
// none → no icon and a tooltip that says nobody is on it and why
// (hivecommons/docs#500 drew "Fixing" with no icon and nobody fixing).
func stageTrackerActorIconJS(t *testing.T, html string) string {
	t.Helper()
	return strings.Join([]string{
		jsFunc(t, html, "stageTrackerFixingWord"),
		jsFunc(t, html, "stageTrackerLinkedElsewhere"),
		jsFunc(t, html, "stageTrackerLinkedElsewhereHtml"),
	}, "\n")
}

func TestStageTrackerActorIconKindsJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+stageTrackerRowsFixtureJS+`
const actorSvg = (html) => (/<svg class="ham-ico ham-actor-ico"[^>]*aria-label="([^"]*)"[^>]*>([\s\S]*?)<\/svg>/.exec(strip(html)) || []);
const detour = (html) => (/<span class="ham-detour">([^<]*)<\/span>/.exec(strip(html)) || [])[1];
const litTitle = (html) => (/<li class="ham-seg active[^"]*"[^>]*title="([^"]*)"/.exec(strip(html)) || [])[1] || '';
const since = new Date(now - 10 * 3600000).toISOString();
const pr = (n, extra) => Object.assign({ repo: 'hivecommons/docs', number: n, kind: 'pr', title: 'docs: resync', url: 'https://x/' + n, stage: 70, stage_since: since, caption: 'Fixing', author: 'alice', author_kind: 'human' }, extra || {});

// hive → honeycomb.
let row = stageTrackerRowHtml(pr(501, { author: 'hive-bot', author_kind: 'hive', stage_actor: { kind: 'hive', login: 'fixer', why: 'fixer commented · 3m ago' } }), stages, ctx);
assert.strictEqual(actorSvg(row)[1], 'Next move: hive', row);
assert.ok(actorSvg(row)[2].includes(STAGE_TRACKER_ICONS.hive), 'hive draws the honeycomb');
assert.strictEqual(detour(row), 'Fixing', 'an agent on it: the pill stays Fixing');
assert.ok(litTitle(row).includes('Next move: hive @fixer — fixer commented · 3m ago'), litTitle(row));

// clanker → robot head.
row = stageTrackerRowHtml(pr(502, { author_kind: 'clanker', stage_actor: { kind: 'clanker', login: 'acme-agent', why: '@acme-agent must push a fix' } }), stages, ctx);
assert.strictEqual(actorSvg(row)[1], 'Next move: clanker');
assert.ok(actorSvg(row)[2].includes(STAGE_TRACKER_ICONS.clanker));
assert.strictEqual(detour(row), 'Fixing');

// human → the person glyph "Needs human decision" uses.
row = stageTrackerRowHtml(pr(503, { stage_actor: { kind: 'human', login: 'alice', why: '@alice must push a fix' } }), stages, ctx);
assert.strictEqual(actorSvg(row)[1], 'Next move: human');
assert.ok(actorSvg(row)[2].includes(STAGE_TRACKER_ICONS.human), 'human draws the person glyph');
const decision = stageTrackerRowHtml(pr(509, { stage: 85, stage_actor: { kind: 'human' } }), stages, ctx);
assert.strictEqual(actorSvg(decision)[2], actorSvg(row)[2], 'same glyph as the Needs human decision pill');

// bot → cog, and the pill reads the state instead of an active Fixing (docs#500).
const botWhy = 'bot-authored PR (@github-actions[bot]) — the hive does not fix bot PRs; it waits for the bot\'s next run to replace it (superseded by #497)';
row = stageTrackerRowHtml(pr(500, { author: 'github-actions[bot]', author_kind: '', modifiers: ['stalled', 'regressed', 'needs-rebase'], caption: 'Conflicting with the base branch; ' + botWhy, stage_actor: { kind: 'bot', login: 'github-actions[bot]', why: botWhy } }), stages, ctx);
assert.strictEqual(actorSvg(row)[1], 'Next move: bot', row);
assert.ok(actorSvg(row)[2].includes(STAGE_TRACKER_ICONS.bot), 'bot draws the cog');
assert.ok(!actorSvg(row)[2].includes(STAGE_TRACKER_ICONS.clanker), 'a bot is not drawn as a clanker');
assert.strictEqual(detour(row), 'Conflicting · bot PR', 'nobody is fixing: the pill says the state in plain words');
assert.ok(litTitle(row).includes('Next move: bot @github-actions[bot] — bot-authored PR'), litTitle(row));
assert.ok(text(row).includes('Conflicting · bot PR'), 'the compact caption carries the same words: ' + text(row));

// none → no icon; the tooltip says nobody is on it and why.
row = stageTrackerRowHtml(pr(504, { author_kind: '', stage_actor: { kind: 'none', why: 'the PR is red and no agent or author is on it' } }), stages, ctx);
assert.strictEqual(actorSvg(row).length, 0, 'no actor icon when nobody is working: ' + strip(row));
assert.strictEqual(detour(row), 'Red · nobody on it');
assert.ok(litTitle(row).includes('no one is working on this right now — the PR is red and no agent or author is on it'), litTitle(row));
assert.ok(tooltip(row).includes('no one is working on this right now — the PR is red'), 'row tooltip too: ' + tooltip(row));

// No stage_actor (older server): the stage-table heuristic still applies and the word stays Fixing.
row = stageTrackerRowHtml(pr(505, { author_kind: 'clanker' }), stages, ctx);
assert.strictEqual(actorSvg(row)[1], 'Next move: clanker', 'legacy payloads keep the author-kind fallback');
assert.strictEqual(detour(row), 'Fixing');
assert.ok(litTitle(row).includes(' · Next move: clanker'), litTitle(row));

// The server word wins over the heuristic: a hive stage with none → no icon.
row = stageTrackerRowHtml(pr(506, { stage: 50, caption: 'CI running', stage_actor: { kind: 'none', why: 'CI is still running' } }), stages, ctx);
assert.strictEqual(actorSvg(row).length, 0, strip(row));
assert.ok(strip(row).includes('<span class="ham-seg-t">CI / Review</span>'), 'a path column keeps its name');
assert.ok(litTitle(row).includes(STAGE_TRACKER_NOBODY_LEAD + ' — CI is still running'));

// An issue riding its one covering PR takes the PR's actor.
const lp = { number: 500, title: 'docs: resync', url: 'https://x/500', author: 'github-actions[bot]', author_kind: '', stage: 70, stage_since: since, modifiers: ['needs-rebase'], stage_actor: { kind: 'bot', login: 'github-actions[bot]', why: botWhy } };
row = stageTrackerRowHtml({ repo: 'hivecommons/docs', number: 496, kind: 'issue', title: 'Resync', url: 'https://x/496', stage: 40, stage_since: since, linked_prs: [lp] }, stages, ctx);
assert.strictEqual(actorSvg(row)[1], 'Next move: bot', 'single covering PR: the issue strip carries the PR actor: ' + row);
assert.strictEqual(detour(row), 'Conflicting · bot PR');

// Sub-rows carry their own actor: one with an agent, one with nobody.
row = stageTrackerRowHtml({ repo: 'hivecommons/docs', number: 497, kind: 'issue', title: 'Two', url: 'https://x/497', stage: 40, stage_since: since, linked_prs: [
  Object.assign({}, lp, { number: 510, stage_actor: { kind: 'hive', login: 'fixer', why: 'fixer is on it' } }),
  Object.assign({}, lp, { number: 511, modifiers: [], stage_actor: { kind: 'none', why: 'no agent is on it' } }),
] }, stages, ctx);
const subRows = subs(row);
assert.strictEqual(subRows.length, 2);
assert.ok(subRows[0].includes('aria-label="Next move: hive"') && subRows[0].includes('<span class="ham-detour">Fixing</span>'), subRows[0]);
assert.ok(!subRows[1].includes('ham-actor-ico') && subRows[1].includes('<span class="ham-detour">Red · nobody on it</span>') && subRows[1].includes(STAGE_TRACKER_NOBODY_LEAD + ' — no agent is on it'), subRows[1]);
`)
}

// A PR that closes two open issues is drawn once: under the issue the server
// names in drawn_under; the other issue shows a compact "under #M" chip in
// place of a second identical strip (hivecommons/hive#14921 under #14910 and
// #14911 drew twice).
func TestStageTrackerLinkedDrawnUnderJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const lp = (n, under, extra) => Object.assign({ number: n, title: 'fix', url: 'https://x/' + n, author: 'hive-bot', author_kind: 'hive', stage: 70, stage_since: since, drawn_under: under, stage_actor: { kind: 'hive', login: 'fixer', why: 'fixer is on it' } }, extra || {});
const issue = (n, prs) => ({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/' + n, stage: 40, stage_since: since, caption: 'open PR #14921 covers it', linked_prs: prs });
const elsewhere = (row) => Array.from(row.matchAll(/<a class="ham-badge ham-linked-elsewhere"[^>]*title="([^"]*)"[^>]*>([^<]*)<\/a>/g)).map(m => ({ title: m[1], text: m[2] }));

// The owning issue draws the PR as its single covering PR, no chip.
let row = stageTrackerRowHtml(issue(14910, [lp(14921, 14910)]), stages, ctx);
assert.deepStrictEqual(prCol(row).length, 1, 'owner: the PR sits in the PR column: ' + row);
assert.ok(prCol(row)[0].includes('#14921'));
assert.strictEqual(strips(row).length, 1);
assert.ok(row.includes('<span class="ham-detour">Fixing</span>') && row.includes('aria-label="Next move: hive"'), row);
assert.deepStrictEqual(elsewhere(row), [], 'owner carries no elsewhere chip');

// The other issue does not redraw the PR: its own strip (040 Covered by PR) and a chip pointing at the owner row.
row = stageTrackerRowHtml(issue(14911, [lp(14921, 14910)]), stages, ctx);
assert.strictEqual(prCol(row).length, 0, 'the PR column is empty, no second ↳ #14921 row: ' + row);
assert.strictEqual(subs(row).length, 0);
assert.strictEqual(strips(row).length, 1);
assert.ok(strip(row).includes('<span class="ham-seg-t">PR in progress</span>') && strip(row).includes('title="Covered by PR'), 'the issue draws its own 040 stage (lit at the PR column): ' + strip(row));
assert.deepStrictEqual(elsewhere(row), [{ title: 'PR #14921 covers this issue too; its row is drawn under #14910', text: '↳ #14921 · under #14910' }]);
assert.ok(!strip(row).includes('<span class="ham-detour">Fixing</span>'), 'no second lit Fixing pill');
assert.ok(tooltip(row).includes('Covering PR: #14921'), 'the row tooltip still names the covering PR');

// Several PRs: the ones drawn here are sorted and drawn, the rest become chips.
row = stageTrackerRowHtml(issue(14912, [lp(14921, 14910), lp(14930, 14912), lp(14931, 14912, { stage: 50 })]), stages, ctx);
assert.strictEqual(subs(row).length, 2, 'two PRs drawn here → two sub-rows: ' + row);
assert.ok(!row.includes('data-ham-sub-key="hivecommons/hive/pr#14921"'), 'the PR drawn elsewhere is not a sub-row');
assert.deepStrictEqual(elsewhere(row).map(e => e.text), ['↳ #14921 · under #14910']);

// Older payloads without drawn_under draw under every issue as before.
row = stageTrackerRowHtml(issue(14911, [lp(14921, 0)]), stages, ctx);
assert.strictEqual(prCol(row).length, 1);
assert.deepStrictEqual(elsewhere(row), []);
assert.deepStrictEqual(stageTrackerLinkedElsewhere(issue(14911, [lp(14921, 0)])), []);
assert.deepStrictEqual(stageTrackerLinkedSorted(issue(14911, [lp(14921, 14910)]), stages), [], 'drawn elsewhere → not in this row\'s list');
`)
}

// A PR drawn under an issue that is filtered out or on another page is drawn
// under the first listed issue that covers it instead of vanishing.
func TestStageTrackerLinkedDrawnUnderHiddenParentJS(t *testing.T) {
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+"\n"+stageTrackerActorIconJS(t, html)+jsFunc(t, html, "stageTrackerAdoptOrphanPRs")+stageTrackerRowsFixtureJS+`
const since = new Date(now - 3600000).toISOString();
const lp = (n, under) => ({ number: n, title: 'fix', url: 'https://x/' + n, author: 'hive-bot', author_kind: 'hive', stage: 70, stage_since: since, drawn_under: under });
const issue = (n, prs) => ({ repo: 'hivecommons/hive', number: n, kind: 'issue', title: 'Issue ' + n, url: 'https://x/' + n, stage: 40, stage_since: since, caption: 'open PR covers it', linked_prs: prs });
const hasPR = (row, n) => row.includes('#' + n) && !row.includes('under #');

// The owner (#14910) is not on the page: #14911 draws the PR itself.
let rows = stageTrackerAdoptOrphanPRs([issue(14911, [lp(14921, 14910)])]);
let html1 = stageTrackerRowHtml(rows[0], stages, ctx);
assert.ok(hasPR(html1, 14921), 'the PR is drawn when its owner is hidden: ' + html1);
assert.deepStrictEqual(stageTrackerLinkedElsewhere(rows[0]), []);

// Two listed issues cover it: only the first draws it, the other chips to it.
rows = stageTrackerAdoptOrphanPRs([issue(14911, [lp(14921, 14910)]), issue(14912, [lp(14921, 14910)])]);
assert.strictEqual(rows[0].linked_prs[0].drawn_under, 0);
assert.strictEqual(rows[1].linked_prs[0].drawn_under, 14911);

// The owner is listed: nothing changes, and inputs are never mutated.
const owner = issue(14910, [lp(14921, 14910)]), other = issue(14911, [lp(14921, 14910)]);
rows = stageTrackerAdoptOrphanPRs([owner, other]);
assert.strictEqual(rows[0], owner);
assert.strictEqual(rows[1], other);
rows = stageTrackerAdoptOrphanPRs([other, null]);
assert.strictEqual(other.linked_prs[0].drawn_under, 14910);
assert.strictEqual(rows[1], null);
`)
}

// The Merged chip lives in a sticky bar at the top of the tracker — pinned
// under the card header while the list scrolls, with the stage column
// header strip pinned just below it — so the chip is in view whenever the
// list is (hivecommons/hive#15147; the operator never saw the celebration
// land in a toolbar 2,300 px above the viewport). It is one node for the
// life of the page, resolved through stageTrackerMergedChip() by the count
// draw, the popover and the flight alike; the column header is a single
// decorative row again (no counter slot).
func TestStageTrackerMergedCounterPlacement(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<div class="ham-sticky" id="activity-monitor-sticky">`,
		`<button type="button" class="ham-chip ham-merged-count" id="activity-monitor-merged" data-action="hamMergedToggle" data-stop="1" aria-haspopup="dialog" aria-expanded="false" aria-controls="activity-monitor-merged-pop"`,
		`<div class="ham-merged-pop" id="activity-monitor-merged-pop" role="dialog" aria-label="Recent merges" hidden></div>`,
		`.ham-sticky { position: sticky; top: var(--navbar-sticky-height); z-index: calc(var(--z-sticky) + 1);`,
		`.ham-cols { position: sticky; top: calc(var(--navbar-sticky-height) + var(--ham-sticky-bar-h));`,
		`@media (prefers-reduced-motion: reduce) { .cue-flight { animation-name: cue-flight-fade; } .ham-merged-count.cue-tick { animation: none; }`,
		`id="sts-celebrate-on" data-change-action="stageTrackerCelebrateChange"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing merged-chip placement wiring %q", want)
		}
	}
	if i, j := strings.Index(html, `id="activity-monitor-sticky"`), strings.Index(html, `id="activity-monitor-panel"`); i < 0 || j < 0 || i > j {
		t.Errorf("the sticky bar must precede the panel: sticky=%d panel=%d", i, j)
	}
	for _, gone := range []string{"ham-merged-home", "ham-merged-slot", "ham-cols-top", "stageTrackerMergedCountMount", "Merged today"} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still carries the retired %q", gone)
		}
	}
	for _, fn := range []string{"stageTrackerFlyGlyph", "stageTrackerMergedCountDraw", "hamMergedToggle"} {
		src := jsFunc(t, html, fn)
		if strings.Contains(src, "getElementById('activity-monitor-merged')") || !strings.Contains(src, "stageTrackerMergedChip()") {
			t.Errorf("%s must resolve the chip through stageTrackerMergedChip() so the flight, the draw and the popover share one node", fn)
		}
	}
	// The sticky bar and chip CSS name no literal colour.
	for _, line := range strings.Split(html, "\n") {
		if strings.Contains(line, ".ham-sticky") || strings.Contains(line, ".ham-merged-pop") {
			if strings.Contains(line, "#") || strings.Contains(line, "rgb(") {
				t.Errorf("merged-chip CSS must use tokens only: %s", strings.TrimSpace(line))
			}
		}
	}
	lifted := strings.Join([]string{
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		jsFunc(t, html, "stageTrackerIcon"),
		jsFunc(t, html, "stageTrackerHeaderHtml"),
		jsFunc(t, html, "stageTrackerMergedChip"),
	}, "\n")
	runStageTrackerRowActionsScript(t, lifted+`
const assert = require('assert');
const stages = [{ number: 10, short: 'Actionable' }, { number: 20, short: 'Claimed' }, { number: 30, short: 'PR in progress' }, { number: 50, short: 'CI / Review' }, { number: 80, short: 'Merge gate' }];
const header = stageTrackerHeaderHtml(stages);
assert.ok(header.startsWith('<div class="ham-cols" aria-hidden="true"><div class="ham-strip-row">'), 'one decorative row, hidden from AT: ' + header);
assert.strictEqual((header.match(/<div class="ham-strip-row/g) || []).length, 1, 'no counter row above the names');
assert.ok(!header.includes('ham-merged'), 'the header carries no chip slot');
const lis = header.match(/<li class="ham-seg[^"]*"/g);
assert.strictEqual(lis.length, STAGE_TRACKER_PATH.length + 1, 'one cell per column plus the ✓ cap');
assert.ok(lis[lis.length - 1].includes('cap'), 'the last cell is the cap');

// ---- DOM fake: the chip is one node, cached, re-resolved by id when detached ----
function el(tag, attrs) {
  const e = { tag, attrs: Object.assign({}, attrs || {}), children: [], parentNode: null,
    get isConnected() { let n = e; while (n.parentNode) n = n.parentNode; return n === body; },
    appendChild(c) { if (c.parentNode) c.parentNode.children = c.parentNode.children.filter(x => x !== c); c.parentNode = e; e.children.push(c); return c; },
    remove() { if (e.parentNode) e.parentNode.children = e.parentNode.children.filter(c => c !== e); e.parentNode = null; },
    byId(id) { if (e.attrs.id === id) return e; for (const c of e.children) { const f = c.byId(id); if (f) return f; } return null; },
  };
  return e;
}
const body = el('body');
const bar = el('div', { id: 'activity-monitor-sticky', class: 'ham-sticky' }); body.appendChild(bar);
const chip = el('button', { id: 'activity-monitor-merged', class: 'ham-chip ham-merged-count' }); bar.appendChild(chip);
global.document = { body, getElementById(id) { return body.byId(id); } };
assert.strictEqual(stageTrackerMergedChip(), chip, 'resolved by id');
assert.strictEqual(stageTrackerMergedChip(), chip, 'cached');
chip.remove();
assert.strictEqual(stageTrackerMergedChip(), chip, 'a detached chip stays cached until a node with the id is back');
const chip2 = el('button', { id: 'activity-monitor-merged' }); bar.appendChild(chip2);
assert.strictEqual(stageTrackerMergedChip(), chip2, 'a new node with the id replaces the detached one');
`)
}
