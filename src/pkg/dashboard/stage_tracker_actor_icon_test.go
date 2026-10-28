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

// The "Merged today" counter lives in the column header, over the Merge
// gate and ✓ columns (not in the filter chip bar), so a merge token's flight
// from a row's ✓ cell is short and ends above the gate. It is one node for
// the life of the page: the renderer mounts it into the header slot after
// each reconcile and parks it in its toolbar home when the board is empty.
func TestStageTrackerMergedCounterPlacement(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<span class="ham-merged-home" id="activity-monitor-merged-home"><span class="ham-chip ham-merged-count" id="activity-monitor-merged" role="status" aria-live="polite"`,
		`<li class="ham-seg ham-merged-slot-seg"><span class="ham-merged-slot"></span></li>`,
		`.ham-cols-top .ham-seg.ham-merged-slot-seg { flex: 1 1 calc(4em + var(--sp-1)); justify-content: flex-end;`,
		`stageTrackerMergedCountMount(el);`,
		`stageTrackerMergedCountMount(null);`,
		`@media (prefers-reduced-motion: reduce) { .cue-token { display: none; } .ham-merged-count.cue-tick { animation: none; }`,
		`id="sts-celebrate-on" data-change-action="stageTrackerCelebrateChange"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing merged-counter placement wiring %q", want)
		}
	}
	for _, fn := range []string{"celebrateRow", "stageTrackerMergedCountDraw"} {
		src := jsFunc(t, html, fn)
		if strings.Contains(src, "getElementById('activity-monitor-merged')") || !strings.Contains(src, "stageTrackerMergedChip()") {
			t.Errorf("%s must resolve the counter through stageTrackerMergedChip() so the token targets the mounted node", fn)
		}
	}
	// The toolbar-home markup and the header slot CSS name no literal colour.
	for _, line := range strings.Split(html, "\n") {
		if strings.Contains(line, ".ham-merged-slot") || strings.Contains(line, ".ham-cols-top") || strings.Contains(line, ".ham-merged-home") {
			if strings.Contains(line, "#") || strings.Contains(line, "rgb(") {
				t.Errorf("merged-counter CSS must use tokens only: %s", strings.TrimSpace(line))
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
		jsFunc(t, html, "stageTrackerMergedCountMount"),
	}, "\n")
	runStageTrackerRowActionsScript(t, lifted+`
const assert = require('assert');
const stages = [{ number: 10, short: 'Actionable' }, { number: 20, short: 'Claimed' }, { number: 30, short: 'PR in progress' }, { number: 50, short: 'CI / Review' }, { number: 80, short: 'Merge gate' }];
const header = stageTrackerHeaderHtml(stages);
const top = /<div class="ham-strip-row ham-cols-top">([\s\S]*?)<\/div>/.exec(header)[1];
const names = header.slice(header.indexOf('</div>') + 6);
assert.ok(header.indexOf('ham-cols-top') < header.indexOf('Merge gate'), 'the counter row sits above the column names');
assert.ok(names.startsWith('<div class="ham-strip-row" aria-hidden="true">'), 'only the decorative column names are hidden from AT; the live counter is not: ' + names);
assert.ok(!header.startsWith('<div class="ham-cols" aria-hidden'), 'the header wrapper is no longer aria-hidden (it hosts the live counter)');
const lis = top.match(/<li class="ham-seg[^"]*"/g);
assert.strictEqual(lis.length, STAGE_TRACKER_PATH.length, 'one cell per column; the slot cell spans Merge gate + ✓');
assert.strictEqual(lis.filter(x => x.includes('blank')).length, STAGE_TRACKER_PATH.length - 1, 'blank spacers under every column but the last');
assert.ok(lis[lis.length - 1].includes('ham-merged-slot-seg'), 'the slot is the last cell, over the Merge gate and the ✓ cap');
assert.ok(top.endsWith('<span class="ham-merged-slot"></span></li></ol><span class="ham-since"></span>'), 'an empty since spacer keeps the slot aligned with the strips below: ' + top);

// ---- DOM fake: the chip is mounted into the slot and survives a header re-render ----
function el(tag, attrs) {
  const e = { tag, attrs: Object.assign({}, attrs || {}), children: [], parentNode: null, classList: { contains(c) { return (e.attrs['class'] || '').split(' ').includes(c); } },
    get isConnected() { let n = e; while (n.parentNode) n = n.parentNode; return n === body; },
    appendChild(c) { if (c.parentNode) c.parentNode.children = c.parentNode.children.filter(x => x !== c); c.parentNode = e; e.children.push(c); return c; },
    remove() { if (e.parentNode) e.parentNode.children = e.parentNode.children.filter(c => c !== e); e.parentNode = null; },
    // Descendant class selectors only ('.a .b'), enough for the mount lookup.
    querySelector(sel) { const parts = sel.trim().split(/\s+/).map(p => p.slice(1)); const walk = (n, i) => { for (const c of n.children) { if (c.classList.contains(parts[i])) { if (i === parts.length - 1) return c; const d = walk(c, i + 1); if (d) return d; } const d = walk(c, i); if (d) return d; } return null; }; return walk(e, 0); },
    byId(id) { if (e.attrs.id === id) return e; for (const c of e.children) { const f = c.byId(id); if (f) return f; } return null; },
  };
  return e;
}
const body = el('body');
const toolbar = el('div'); body.appendChild(toolbar);
const home = el('span', { id: 'activity-monitor-merged-home', class: 'ham-merged-home' }); toolbar.appendChild(home);
const chip = el('span', { id: 'activity-monitor-merged', class: 'ham-chip ham-merged-count' }); home.appendChild(chip);
const panel = el('div', { id: 'activity-monitor-panel' }); body.appendChild(panel);
global.document = { body, getElementById(id) { return body.byId(id); } };
function headerNode() { const h = el('div', { class: 'ham-cols', 'data-ham-key': '__cols' }); const row = el('div', { class: 'ham-strip-row ham-cols-top' }); const slot = el('span', { class: 'ham-merged-slot' }); row.appendChild(slot); h.appendChild(row); return h; }

// Empty board: the chip stays (or goes) home.
assert.strictEqual(stageTrackerMergedCountMount(panel), home, 'no header → toolbar home');
assert.strictEqual(chip.parentNode, home);
// Rows drawn: the chip moves into the header slot and is still reachable by id.
let h1 = headerNode(); panel.appendChild(h1);
assert.strictEqual(stageTrackerMergedCountMount(panel), h1.querySelector('.ham-merged-slot'));
assert.strictEqual(chip.parentNode.attrs['class'], 'ham-merged-slot', 'the counter lives in the header slot');
assert.strictEqual(document.getElementById('activity-monitor-merged'), chip, 'the celebration target id still resolves');
assert.strictEqual(stageTrackerMergedChip(), chip, 'celebrateRow and the count draw get the same node');
assert.strictEqual(home.children.length, 0, 'the home is empty (and hidden by :empty) while the board is drawn');
// The header is re-rendered (stages changed): the detached chip is re-mounted into the new slot, same node.
h1.remove(); const h2 = headerNode(); panel.appendChild(h2);
assert.strictEqual(chip.isConnected, false, 'detached with the old header');
assert.strictEqual(stageTrackerMergedChip(), chip, 'the cached node is kept although detached');
stageTrackerMergedCountMount(panel);
assert.strictEqual(chip.parentNode, h2.querySelector('.ham-merged-slot'));
assert.strictEqual(chip.isConnected, true);
// Back to empty: mount(null) parks it home.
assert.strictEqual(stageTrackerMergedCountMount(null), home);
assert.strictEqual(chip.parentNode, home);
assert.strictEqual(document.getElementById('activity-monitor-merged'), chip);
`)
}
