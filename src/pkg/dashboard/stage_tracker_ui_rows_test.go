package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerRows executes the pure row renderer under node and checks
// the unified strip geometry (Open → 010 → 020 → 030 → 040 → 050 → 080 → 100 →
// merged cap): which segments are done / active / detoured, the tooltip and
// the chip cap, for issues and PRs alike.
func TestStageTrackerRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — Hive activity monitor rows were NOT executed by this run")
	}
	html := indexHTML(t)
	script := stageTrackerRowsPreludeJS(t, html) + stageTrackerRowsFixtureJS + `

// Issue claimed 30 min ago: Open + Actionable done, Claimed active, five ahead, cap todo.
let row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', title: 'Fix <login>', url: 'https://x/7', stage: 20, stage_since: new Date(now - 30*60000).toISOString(), caption: 'Claimed by scanner', author: 'alice', author_kind: 'human', author_kind_source: 'label' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, 'active', T, T, T, 'todo cap'], 'an issue with no PR yet draws the whole strip ahead');
assert.ok(row.includes('aria-current="step"') && subs(row).length === 0);
assert.ok(row.includes('Fix &lt;login&gt;'), 'title escaped');
assert.strictEqual(prCol(row).length, 0, 'issue without a PR has an empty PR column');
assert.ok(tooltip(row).includes('(person'), 'author kind stays in the tooltip, in plain words');
assert.ok(row.includes('<span class="ham-since">30m</span>'), row);
// The age and the chip belong to the strip they describe, not to a title line
// that may wrap: the since text closes the strip row (an empty spacer keeps
// the title line's width) and the chip is the last thing in the title line's
// ghost strip — trailing the PR cell (ham-head-tools when there is no PR),
// right-anchored on the title line, never a toolbar line of its own.
assert.ok(row.indexOf('<span class="ham-since">30m</span>') > row.indexOf('aria-label="Pipeline progress"'), 'since sits on the strip row, in line with the chevrons');
assert.ok(/aria-label="Pipeline progress">[\s\S]*?<\/ol>\s*<span class="ham-since">30m<\/span>\s*<\/div>/.test(row), 'the strip row ends with the since cell');
assert.ok(/<li class="ham-seg ham-head-pr ham-head-tools"><span class="ham-badges">[\s\S]*?<\/span><\/li><\/ol><span class="ham-since"><\/span><\/div>\s*<\/div>\s*<div class="ham-strip-row">/.test(row), 'the chip cell closes the title line, directly above its own strip row');
assert.ok(!row.includes('<li class="ham-seg cap">'), 'no separate toolbar line between the title and the chevrons');
assert.ok(row.indexOf('<div class="ham-row-head">') < row.indexOf('ham-badges') && row.indexOf('ham-badges') < row.indexOf('aria-label="Pipeline progress"'), 'chip and strip share one row container');
assert.ok(row.includes('ham-kind-ico'), 'kind icon');
assert.strictEqual((strip(row).match(/<svg class="ham-ico( ham-history-ico)?"/g) || []).length, 7, 'native icons remain; unknown CI and Review each use the existing column glyph');
assert.ok(/<li class="ham-seg todo cap"><svg class="ham-ico ham-history-ico"/.test(strip(row)), 'the unmerged cap wears the history mark, not the ✓');
assert.ok(!strip(row).includes('✓'), 'no ✓ in the strip before the PR merges');
assert.ok(tooltip(row).includes('Claimed by scanner') && tooltip(row).includes('@alice') && tooltip(row).includes('30 min ago'), tooltip(row));
assert.ok(!row.includes('✓'), 'no text check glyphs');

// A hive-filed issue leads with its filing agent in brackets; the tag is
// drawn once as a chip ahead of the title, the title text no longer repeats
// it, and the full title stays in the tooltip. A bracket that is not an
// agent slug is part of the subject and stays in the title.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', title: '[reviewer] Review backlog (security): token <logged>', url: 'https://x/8', stage: 0, caption: 'No stage recorded yet' }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-title-tag" title="Filed by [reviewer]">reviewer</span> <span class="ham-title" title="[reviewer] Review backlog (security): token &lt;logged&gt;">Review backlog (security): token &lt;logged&gt;</span>'), 'agent tag chip then the untagged title: ' + row);
assert.strictEqual((row.match(/\[reviewer\]/g) || []).length, 2, 'the tag appears only in the chip tooltip and the title tooltip, never twice in the text: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 10, kind: 'issue', title: '[ACMM L0] Add CI/CD pipeline', url: 'https://x/10', stage: 0, caption: 'No stage recorded yet' }, stages, ctx);
assert.ok(!row.includes('ham-title-tag') && row.includes('<span class="ham-title" title="[ACMM L0] Add CI/CD pipeline">[ACMM L0] Add CI/CD pipeline</span>'), 'a non-agent bracket is not a chip: ' + row);
// The chip also leads a PR row's linked-issue slot.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 12, kind: 'pr', title: 'fix it', url: 'https://x/12', stage: 50, caption: 'In review', nested_under: 8, linked_issues: [{ number: 8, state: 'open', title: '[scanner] logscrub misses attrs', url: 'https://x/8' }] }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-title-tag" title="Filed by [scanner]">scanner</span> <span class="ham-title" title="[scanner] logscrub misses attrs">logscrub misses attrs</span>'), 'linked issue slot draws the chip: ' + row);

// Open but held: the Open segment itself names the hold state and the row
// tooltip names the matching hold label, while the trailing flag pill remains.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'pr', title: 'Paused', stage: 0, caption: 'No stage recorded yet', labels: ['do-not-merge/hold'], modifiers: ['hold'] }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap']);
assert.ok(row.includes('ham-mod-hold">hold</span>'), row);
assert.ok(tooltip(row).includes('Open · on hold (do-not-merge/hold) · No stage recorded yet · acme/console'), tooltip(row));

// Open and awaiting triage: the action chip reads "triage needed"; the Open
// column itself is untouched (owner: the Open column keeps its behaviour —
// "triage needed" is the 015 stage's word in the Actionable column).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', title: 'New report', stage: 0, caption: 'No stage recorded yet', labels: ['needs-triage'], modifiers: ['needs-triage'] }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);
assert.ok(row.includes('ham-mod-needs-triage">triage needed</span>'), row);
assert.ok(/aria-current="step"><span class="ham-chevron-info config-info"[^>]*><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-chevron-copy"><span class="ham-seg-t">Open<\/span><\/span>/.test(row), row);
assert.ok(tooltip(row).startsWith('Open No stage recorded yet') && tooltip(row).includes('Flags: triage needed'), tooltip(row));

// 015 triage-needed: the Actionable column lights (not a detour) with the
// word "Triage needed" and a human (maintainer) next move; the headline and
// chevron tooltip name the stage; the server already dropped the chip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 13262, kind: 'issue', title: 'Review backlog (security)', stage: 15, author: 'hive-bot', author_kind: 'hive', caption: 'Actionable, but no maintainer has looked at it yet; waiting for triage' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, 'active', T, T, T, T, 'todo cap'], '015 lights the Actionable column: ' + row);
assert.ok(/aria-current="step"><span class="ham-chevron-info config-info"[^>]*><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-chevron-copy"><span class="ham-seg-t">Triage needed<\/span><\/span><svg class="ham-ico ham-actor-ico"/.test(row) && row.includes('aria-label="Next move: human"'), '015 word + maintainer move: ' + row);
assert.ok(!row.includes('ham-detour'), '015 is not a detour');
assert.ok(strip(row).includes('title="Triage needed — Actionable but no maintainer has looked at it yet · Next move: human"'), 'chevron tooltip names 015: ' + row);
assert.ok(tooltip(row).startsWith('Triage needed Actionable, but no maintainer has looked at it yet; waiting for triage'), tooltip(row));
assert.ok(text(row).includes('#13262 Review backlog (security) Triage needed'), text(row));
assert.strictEqual(stageTrackerHeadline({ stage: 15, kind: 'issue' }), 'Triage needed');
assert.strictEqual(stageTrackerAnchor(15, stages, 'issue'), 10, '015 anchors on the Actionable column');
assert.ok(!stageTrackerNeedsAttention({ kind: 'issue', stage: 15 }), '015 is a tracking state, not an attention flag');
// A plain 010 row is unchanged by the new stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 12999, kind: 'issue', title: 'Epic', stage: 10 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, 'active', T, T, T, T, 'todo cap']);
assert.ok(/aria-current="step"><span class="ham-chevron-info config-info"[^>]*><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-chevron-copy"><span class="ham-seg-t">Actionable<\/span><\/span>/.test(row), row);

// Plain Open (no stage label, not held) says how long it has sat there, from
// the same instant the since column draws (stage_since), with the ISO time on
// the chevron tooltip; past STAGE_TRACKER_OPEN_STALE_DAYS it is tinted stale.
const openWord = (html) => (strip(html).match(/<span class="ham-seg-t">([^<]*)<\/span>/) || [])[1];
row = stageTrackerRowHtml({ repo: 'acme/console', number: 20, kind: 'issue', title: 'Fresh', stage: 0, stage_since: new Date(now - 14 * 3600000).toISOString(), created_at: new Date(now - 30 * 86400000).toISOString() }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · since 14h', row);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap'], 'under the stale threshold: no tint');
assert.ok(strip(row).includes('open since ' + new Date(now - 14 * 3600000).toISOString()), 'tooltip names the instant');
assert.ok(!strip(row).includes('(opened)'), 'stage_since is not the created_at fallback');
assert.strictEqual(stageTrackerSinceHtml({ stage_since: new Date(now - 14 * 3600000).toISOString() }, now), '<span class="ham-since">14h</span>', 'the since column shows the same age');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 21, kind: 'issue', title: 'Old', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString() }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · since 3d');
assert.deepStrictEqual(segStates(row), ['active ham-open-stale', T, T, T, T, T, 'todo cap'], 'past STAGE_TRACKER_OPEN_STALE_DAYS: stale tint');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 22, kind: 'issue', title: 'Only created', stage: 0, created_at: new Date(now - 5 * 86400000).toISOString() }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · since 5d', 'falls back to created_at');
assert.ok(strip(row).includes('open since ' + new Date(now - 5 * 86400000).toISOString() + ' (opened)'), 'the fallback is named');
assert.deepStrictEqual(stageTrackerOpenSince({ stage_since: '2026-01-01T00:00:00Z', created_at: '2025-01-01T00:00:00Z' }), { ms: Date.parse('2026-01-01T00:00:00Z'), fromCreated: false });
assert.deepStrictEqual(stageTrackerOpenSince({ created_at: '2025-01-01T00:00:00Z' }), { ms: Date.parse('2025-01-01T00:00:00Z'), fromCreated: true });
assert.deepStrictEqual(stageTrackerOpenSince({}), { ms: 0, fromCreated: false });
row = stageTrackerRowHtml({ repo: 'acme/console', number: 23, kind: 'pr', title: 'Held', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), modifiers: ['hold'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · hold', 'Open · hold never carries the age (owner)');
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap'], 'held: no stale tint either');
// Open · blocked (the blocked label) mirrors Open · hold but, unlike it,
// keeps the age — a stale blocked item is what the owner wants to see
// (hivecommons/hive#14194). hold wins when both are present.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 25, kind: 'issue', title: 'Stuck', stage: 0, stage_since: new Date(now - 7 * 3600000).toISOString(), labels: ['blocked'], modifiers: ['blocked'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 7h', row);
assert.deepStrictEqual(segStates(row), ['active ham-open-blocked', T, T, T, T, T, 'todo cap'], 'blocked: its own class, no stale tint under the threshold');
assert.ok(strip(row).includes(' — carries the blocked label — open since '), 'chevron tooltip names the label and the instant: ' + strip(row));
assert.ok(tooltip(row).startsWith('Open · blocked'), tooltip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 26, kind: 'issue', title: 'Stuck long', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), modifiers: ['blocked'], blocked_on: 'acme/api#7' }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 3d');
assert.deepStrictEqual(segStates(row), ['active ham-open-blocked ham-open-stale', T, T, T, T, T, 'todo cap'], 'blocked past STAGE_TRACKER_OPEN_STALE_DAYS: stale tint too');
assert.ok(strip(row).includes(' — blocked on acme/api#7 — '), 'a known blocker is named: ' + strip(row));
assert.strictEqual(stageTrackerBlockedTip({ blocked_on: ['acme/api#7', 'acme/api#8'] }), 'blocked on acme/api#7, acme/api#8');
assert.strictEqual(stageTrackerBlockedTip({}), 'carries the blocked label');
// A review-backlog spin-off blocked on a PR (hive:blocked-on marker, hivecommons/hive#14863): the pill names
// and links the PR, the tooltip says what lifts the block, the row title carries it too.
const blockedOnPR = { repo: 'hivecommons/hive', number: 14862, url: 'https://github.com/hivecommons/hive/pull/14862', state: 'open' };
row = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 14863, kind: 'issue', title: 'Review backlog: a', stage: 0, created_at: new Date(now - 7 * 3600000).toISOString(), labels: ['agent/reviewer', 'blocked'], modifiers: ['blocked'], caption: 'Blocked on PR #14862 — lifts when it merges', blocked_on_pr: blockedOnPR }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 7h');
assert.ok(row.includes('<a class="ham-badge ham-badge-mod ham-mod-blocked" href="https://github.com/hivecommons/hive/pull/14862" target="_blank" rel="noopener" title="Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged" aria-label="Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged">blocked on #14862</a>'), 'pill links the blocker: ' + row);
assert.ok(strip(row).includes(' — blocked on hivecommons/hive#14862 — '), 'chevron names the blocker: ' + strip(row));
assert.ok(tooltip(row).includes('Blocked on PR hivecommons/hive#14862 (open) — lifts when it merges; re-triage if closed unmerged'), 'row title carries the blocker: ' + tooltip(row));
assert.ok(tooltip(row).includes('Blocked on PR #14862 — lifts when it merges'), 'caption in the row title: ' + tooltip(row));
assert.ok(!tooltip(row).includes('No stage recorded yet'), tooltip(row));
assert.strictEqual(stageTrackerBlockedOnPR({ repo: 'acme/console', blocked_on_pr: { repo: 'acme/api', number: 7, state: 'merged' } }).tip, 'Blocked on PR acme/api#7 (merged) — merged; the blocked sweep lifts the label next');
assert.strictEqual(stageTrackerBlockedOnPR({ repo: 'acme/console', blocked_on_pr: { repo: 'acme/api', number: 7 } }).short, 'acme/api#7', 'foreign repo keeps owner/repo');
assert.strictEqual(stageTrackerBlockedOnPR({ repo: 'console', blocked_on_pr: { repo: 'acme/console', number: 7, state: 'closed' } }).short, '#7', 'bare configured name matches its full repo');
assert.strictEqual(stageTrackerBlockedOnPR({ blocked_on_pr: { repo: 'acme/api', number: 0 } }), null);
assert.strictEqual(stageTrackerBlockedOnPR({}), null);
assert.strictEqual(stageTrackerBlockedTip({ blocked_on_pr: blockedOnPR }), 'blocked on hivecommons/hive#14862');
row = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 14868, kind: 'issue', title: 'No url', stage: 0, modifiers: ['blocked'], blocked_on_pr: { repo: 'hivecommons/hive', number: 14821 } }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-badge-mod ham-mod-blocked" title="Blocked on PR hivecommons/hive#14821 — lifts when it merges; re-triage if closed unmerged">blocked on #14821</span>'), 'without a url the pill still names the PR, as a span: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 29, kind: 'issue', title: 'Blocked no since', stage: 0, created_at: new Date(now - 3 * 86400000).toISOString(), modifiers: ['blocked'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · blocked · since 3d', 'created_at fallback in the chevron');
assert.ok(row.includes('<span class="ham-since">3d</span>'), 'created_at fallback in the since column: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 30, kind: 'issue', title: 'Plain no since', stage: 0, created_at: new Date(now - 3 * 86400000).toISOString() }, stages, ctx);
assert.ok(!row.includes('<span class="ham-since">3d</span>'), 'only blocked rows borrow created_at for the since column');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 27, kind: 'pr', title: 'Held and blocked', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), modifiers: ['blocked', 'hold'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · hold', 'hold is a human instruction: it wins over blocked and still carries no age');
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap']);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 28, kind: 'pr', title: 'Blocked later', stage: 50, modifiers: ['blocked'] }, stages, ctx);
assert.ok(!row.includes('ham-open-blocked') && !row.includes('Open · blocked'), 'blocked past Open is a chip, not an Open word');
// A draft PR with no stage (hivecommons/hive#14572): the far-right pill
// reads "draft" with the explanatory tooltip, the Open word reads "Draft",
// keeps its age but never tints stale — nothing is stuck, the lanes skip
// drafts until the author marks it ready.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14427, kind: 'pr', title: 'WIP: image scanner', stage: 0, stage_since: new Date(now - 18 * 3600000).toISOString(), labels: ['do-not-merge/work-in-progress'], modifiers: ['draft'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Draft · since 18h', row);
assert.deepStrictEqual(segStates(row), ['active ham-open-draft', T, T, T, T, T, 'todo cap'], 'draft: its own class, no hold/blocked class');
assert.ok(row.includes('ham-mod-draft" title="' + STAGE_TRACKER_DRAFT_TIP + '">draft</span>'), 'draft pill with tooltip: ' + row);
assert.ok(strip(row).includes(' — ' + STAGE_TRACKER_DRAFT_TIP + ' — open since '), 'chevron tooltip explains the draft: ' + strip(row));
assert.ok(tooltip(row).includes('Open · draft'), tooltip(row));
assert.ok(tooltip(row).includes('Flags: draft'), tooltip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14428, kind: 'pr', title: 'Old draft', stage: 0, stage_since: new Date(now - 5 * 86400000).toISOString(), modifiers: ['draft'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Draft · since 5d');
assert.deepStrictEqual(segStates(row), ['active ham-open-draft', T, T, T, T, T, 'todo cap'], 'a long-lived draft is not stale: it is waiting on its author');
assert.strictEqual(stageTrackerCurrentStageHtml({ stage: 0, stage_since: new Date(now - 18 * 3600000).toISOString(), modifiers: ['draft'] }, stages, null, now), '<p class="ham-current-stage">Draft · since 18h</p>', 'the compact caption carries it too');
// Draft plus a human hold: hold leads the pill ("hold +1") and the Open word,
// and the folded draft still explains itself on hover.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 31, kind: 'pr', title: 'Held draft', stage: 0, stage_since: new Date(now - 3 * 86400000).toISOString(), labels: ['do-not-merge/hold'], modifiers: ['hold', 'draft'] }, stages, ctx);
assert.strictEqual(openWord(row), 'Open · hold', 'hold wins the Open word over draft');
assert.deepStrictEqual(segStates(row), ['active ham-open-hold', T, T, T, T, T, 'todo cap']);
assert.ok(row.includes('ham-mod-hold" title="' + STAGE_TRACKER_DRAFT_TIP + '">hold +1</span>'), 'hold leads, draft folded: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 32, kind: 'pr', title: 'Still drafting', stage: 50, modifiers: ['draft'] }, stages, ctx);
assert.ok(!row.includes('ham-open-draft') && !row.includes('>Draft'), 'draft past Open is a chip, not an Open word: ' + row);
assert.ok(row.includes('ham-mod-draft"'), row);
assert.strictEqual(stageTrackerHeadline({ stage: 0, modifiers: ['draft'] }), 'Open · draft');
assert.strictEqual(stageTrackerHeadline({ stage: 0, modifiers: ['hold', 'draft'] }), 'Open · on hold', 'hold leads the headline too');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 24, kind: 'issue', title: 'Staged', stage: 10, stage_since: new Date(now - 3 * 86400000).toISOString() }, stages, ctx);
assert.ok(!row.includes('since '), 'a staged item has no Open age');
assert.strictEqual(stageTrackerCurrentStageHtml({ stage: 0, stage_since: new Date(now - 14 * 3600000).toISOString() }, stages, null, now), '<p class="ham-current-stage">Open · since 14h</p>', 'the compact caption carries it too');

// Issue with exactly one covering PR (only its number known, legacy linked_pr):
// a single entry whose strip is driven by the PR; with no PR stage it lights
// PR in progress.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', url: 'https://github.com/acme/console/issues/8', stage: 100, mirrored: true, linked_pr: 102, caption: 'Covered by PR #102' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap'], 'one covering PR: single entry');
assert.ok(tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.deepStrictEqual(prCol(row).length, 1, 'the covering PR sits in the PR column of the title line');
assert.strictEqual(subs(row).length, 0, 'no sub-rows for a single covering PR');
const prCell = (html) => (html.slice(0, html.indexOf('aria-label="Pipeline progress"')).match(/<li class="ham-seg ham-head-pr"[^>]*>([\s\S]*?)<\/li>/) || [])[1] || '';
assert.ok(prCell(row).startsWith(STAGE_TRACKER_LINKED_GLYPH + '<a class="ham-pr-ref"'), 'an in-row PR is led by the linked glyph: ' + prCell(row));
assert.ok(prCell(row).includes('title="Author: hive · ' + esc(STAGE_TRACKER_LINKED_TIP) + '"'), 'the PR link tooltip explains the shared row: ' + prCell(row));
// A PR listed by two open issues shows on both rows; every row but the
// lowest-numbered issue says where else it is.
const sharedCtx = Object.assign({}, ctx, { sharedPRs: stageTrackerSharedPRs([
  { kind: 'issue', repo: 'acme/console', number: 14175, linked_prs: [{ number: 14189 }] },
  { kind: 'issue', repo: 'acme/console', number: 14176, linked_prs: [{ number: 14189 }] },
  { kind: 'issue', repo: 'acme/console', number: 14177, linked_prs: [{ number: 14190 }] },
  { kind: 'pr', repo: 'acme/console', number: 14189 },
]) });
assert.deepStrictEqual(sharedCtx.sharedPRs, { 'acme/console#14189': [14175, 14176] }, 'only PRs listed by more than one issue');
const first = stageTrackerRowHtml({ repo: 'acme/console', number: 14175, kind: 'issue', title: 'A', stage: 20, linked_prs: [{ number: 14189, stage: 50 }] }, stages, sharedCtx);
const second = stageTrackerRowHtml({ repo: 'acme/console', number: 14176, kind: 'issue', title: 'B', stage: 20, linked_prs: [{ number: 14189, stage: 50 }] }, stages, sharedCtx);
assert.ok(prCell(first).includes(esc(STAGE_TRACKER_LINKED_TIP) + '"') && !prCell(first).includes('also under'), 'the lowest-numbered issue carries the plain hint');
assert.ok(prCell(second).includes(esc(STAGE_TRACKER_LINKED_TIP) + ' · also under #14175"'), 'the other row names the first: ' + prCell(second));
assert.strictEqual(stageTrackerLinkedTip({ repo: 'acme/console', number: 14176 }, 14189, sharedCtx), STAGE_TRACKER_LINKED_TIP + ' · also under #14175');
assert.strictEqual(stageTrackerLinkedTip({ repo: 'acme/console', number: 14175 }, 14189, sharedCtx), STAGE_TRACKER_LINKED_TIP);
assert.ok(!stageTrackerRowHtml({ repo: 'acme/console', number: 30, kind: 'pr', title: 'Alone', stage: 50 }, stages, sharedCtx).includes('ham-linked-ico'), 'a PR on its own row is not marked');
// With the PR's stage known the issue strip rides it (merge queue → Merge gate lit, hive move).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 40, linked_prs: [{ number: 102, title: 'Fix it', author_kind: 'human', author: 'bob', stage: 100, stage_since: new Date(now - 7200000).toISOString(), modifiers: ['stalled'], caption: 'Admitted, merging' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('<span class="ham-seg-t">Merging</span></span><svg class="ham-ico ham-actor-ico"') && row.includes('aria-label="Next move: hive"'), row);
assert.ok(row.includes('ham-mod-stalled">stalled</span>') && row.includes('<span class="ham-since ham-stalled">2h</span>'), 'chips + since come from the covering PR: ' + row);
assert.ok(tooltip(row).includes('Admitted, merging') && tooltip(row).includes('Covering PR: #102'), tooltip(row));
assert.ok(prCol(row)[0].includes('ham-author-human') && prCol(row)[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(row)[0]);
assert.strictEqual(subs(row).length, 0);
// Two covering PRs, one of unknown stage: that sub-row is a blank lead-in with nothing lit.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 40, linked_prs: [{ number: 102, stage: 100 }, { number: 104 }] }, stages, ctx);
assert.ok(subs(row)[1].includes('#104') && segStates(subs(row)[1]).every(st => st.startsWith('blank') || st === 'todo' || st === 'todo cap'), 'unknown PR stage: blank lead-in, nothing lit');
assert.ok(tooltip(row).includes('acme/console'), 'repo lives in the tooltip');
// Full PR row for 100: Merge gate lit as Merging with the hive actor icon (#15073 vocabulary).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', stage: 100, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(strip(row).includes('title="Merge queue — admitted, merging · Next move: hive"'), 'chevron tooltip carries 100: ' + row);
assert.ok(text(row).includes('#102') && text(row).includes('Merging'), text(row));
// Full PR row for 095: Merge gate lit as Soaking (not a detour), hive actor,
// chevron tooltip names the real stage and the time left; headline carries it too.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 13020, kind: 'pr', stage: 95, author: 'hive-bot', author_kind: 'hive', soak_remaining_seconds: 17 * 60 + 1 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap'], '095 lights the Merge gate column, no detour');
assert.ok(strip(row).includes('title="Pre-merge soak — admitted, soaking — 18m left · Next move: hive"'), 'chevron tooltip carries 095 + time left: ' + row);
assert.ok(tooltip(row).startsWith('Pre-merge soak — 18m left'), 'headline: ' + tooltip(row));
assert.strictEqual(stageTrackerHeadline({ stage: 95 }), 'Pre-merge soak', 'no remaining known → plain headline');
assert.strictEqual(stageTrackerHeadline({ stage: 95, soak_remaining_seconds: 5400 }), 'Pre-merge soak — 1h30m left');
assert.strictEqual(stageTrackerHeadline({ stage: 95, kind: 'issue', soak_remaining_seconds: 60 }), 'Pre-merge soak — 1m left (its PR)');
assert.strictEqual(stageTrackerAnchor(95, stages, 'pr'), STAGE_TRACKER_MERGE_COLUMN, '095 anchors on the merge gate column');

// PR returned to contributor: detour badge rides Review; chip cap = author kind + first modifier (+N).
row = stageTrackerRowHtml({ repo: 'acme/console', number: 102, kind: 'pr', title: 'Fix flaky', stage: 72, lane: 'contributor', tier: 'trusted', author: 'karibhou', author_kind: 'clanker', author_kind_source: 'inferred', modifiers: ['needs-rebase', 'needs-human'], caption: 'Returned to @karibhou', history: [{number:50,slug:'review-pending'},{number:60,slug:'reviewed'},{number:72,slug:'returned-to-contributor',from:60}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active detoured', T, T, 'todo cap']);
// hivecommons/hive#15159: the chevron names the routing — the contributor's clanker, for a clanker's PR.
assert.ok(row.includes('<span class="ham-detour">Returned to @karibhou&#39;s clanker</span>'), row);
// Invariant: a PR never occupies the issue slot. A PR with no linked issue
// puts the muted "no linked issue" placeholder on the left and names itself
// — author icon, #N, title — in the PR column, where a covering PR sits on
// an issue row (owner reference: the #100 → #101 row).
assert.deepStrictEqual(prCol(row).length, 1, 'a PR row names its PR in the PR column');
assert.ok(row.includes('class="ham-seg ham-head-pr"'), 'PR cell present on a PR row');
assert.ok(!row.includes('ham-head-wide'), 'lead cell is not wide: the PR column is in use');
assert.strictEqual(lead(row), 'no linked issue', 'issue slot: placeholder');
assert.ok(/<div class="ham-head-main ham-no-issue">\s*<svg class="ham-kind-ico"[\s\S]*?<span class="ham-title ham-no-issue-t">no linked issue<\/span>/.test(row), 'muted issue icon + placeholder text: ' + row);
assert.ok(!/<a class="ham-ref"/.test(row), 'no #N link in the issue slot when there is no issue');
assert.ok(prCol(row)[0].includes('ham-author-ico ham-author-clanker') && prCol(row)[0].includes('aria-label="clanker · @karibhou"') && prCol(row)[0].includes('<span class="ham-pr-num">#102</span>') && prCol(row)[0].includes('<span class="ham-pr-title">Fix flaky</span>'), 'coloured author icon, #102, title in the PR column: ' + prCol(row)[0]);
assert.strictEqual(titles(row, 'Fix flaky'), 1, 'title appears once');
assert.strictEqual(chips(row), 1, 'author kind is an icon, not a chip');
assert.ok(row.includes('ham-mod-needs-human">needs human +1</span>'), row);
assert.ok(tooltip(row).includes('Flags: needs-rebase, needs-human') && tooltip(row).includes('Trail: CI / Review → Reviewed → Returned to contributor'), tooltip(row));

// PR reviewed (060): pre-admission (CI pending or soak), so CI / Review stays lit; the merge gate starts at 080.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 60 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active', T, 'todo cap'], '060 reviewed lights CI / Review, not the merge gate: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 80, caption: 'soak 20m left' }, stages, ctx);
assert.ok(row.includes('<span class="ham-seg-t">Ready for auto merge</span>') && strip(row).includes('title="Awaiting merge admission — not yet admitted — soak 20m left · Next move: hive"'), 'chevron tooltip carries 080: ' + row);

// PR regressed from the queue back to review-pending: Review lit, "Sent back" in the tooltip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 103, kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], history: [{number:80,slug:'awaiting-merge-admission'},{number:100,slug:'merge-queue'},{number:70,slug:'fixing',from:100,regressed:true}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, 'active detoured', T, 'todo cap']);
assert.ok(tooltip(row).startsWith('Sent back'));
assert.ok(row.includes('ham-mod-regressed'), 'regressed outranks needs-rebase for the single chip');

// Stalled PR awaiting admission: since text turns red.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 104, kind: 'pr', stage: 80, stage_since: new Date(now - 3*3600000).toISOString(), modifiers: ['stalled'] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active', 'todo cap']);
assert.ok(row.includes('<span class="ham-since ham-stalled">3h</span>'), row);

// Human-merge detour off Merge gate.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 105, kind: 'pr', stage: 90 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Ready for human merge</span>'));
// 085 has its own word; needs-decision as a modifier elsewhere reads "needs human decision".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 85, modifiers: ['blocked'], caption: 'DCO failing — fix, waive or close' }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap']);
assert.ok(row.includes('<span class="ham-detour">Needs human decision</span>') && !row.includes('ham-mod-needs-decision'), row);
assert.ok(tooltip(row).includes('Needs a human decision'), row);
// A blocked issue with a hive:recheck-after deadline names it on the chip
// (hivecommons/hive#13310); a bare blocked row keeps the plain chip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'issue', stage: 0, modifiers: ['blocked'], bucket: 'filtered', bucket_reason: 'blocked until 2020-01-02T03:04:00Z' }, stages, ctx);
// The time goes through agentNowClockTime, so only the stable parts are pinned: the chip text and the minute.
assert.ok(/>blocked · re-check at [^<]*03:04[^<]*<\/span>/.test(row), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'issue', stage: 0, modifiers: ['blocked'], bucket: 'filtered', bucket_reason: 'blocked' }, stages, ctx);
assert.ok(row.includes('>blocked</span>') && !row.includes('re-check'), row);
// An ISSUE parked on a person wears 045 (owner decision: 085 is PR-only)
// before any PR exists: it lights Actionable (the issue side) as a detour,
// not the merge gate, and its headline carries no "(its PR)" suffix. A
// pre-045 issue still wearing 085 is drawn the same way until re-staged.
for (const parked of [45, 85]) {
  row = stageTrackerRowHtml({ repo: 'acme/console', number: 355, kind: 'issue', stage: parked }, stages, ctx);
  assert.deepStrictEqual(segStates(row), [D, 'active detoured', T, T, T, T, 'todo cap'], row);
  assert.ok(row.includes('<span class="ham-detour">Needs human decision</span>') && tooltip(row).includes('Needs a human decision') && !tooltip(row).includes('(its PR)'), row);
}
// 045 needs attention like the other parking stages.
assert.ok(stageTrackerNeedsAttention({ kind: 'issue', stage: 45 }) && stageTrackerNeedsAttention({ kind: 'pr', stage: 85 }) && !stageTrackerNeedsAttention({ kind: 'issue', stage: 10 }));
// A regression the hive is working through is not a call on a person: out of "Needs attention", no "Sent back" headline.
assert.ok(!stageTrackerNeedsAttention({ kind: 'issue', stage: 10, modifiers: ['regressed'], regressed_from: 40 }), 'regressed at Actionable is hive-handled');
assert.ok(!stageTrackerNeedsAttention({ kind: 'issue', stage: 30, modifiers: ['regressed', 'draft'], regressed_from: 40 }), 'draft is not a human gate');
assert.ok(stageTrackerNeedsAttention({ kind: 'issue', stage: 85, modifiers: ['regressed'], regressed_from: 100 }), 'regressed while parked on a person');
assert.ok(stageTrackerNeedsAttention({ kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], regressed_from: 100 }), 'regressed with a human-call modifier');
assert.notStrictEqual(stageTrackerHeadline({ kind: 'issue', stage: 10, modifiers: ['regressed'], regressed_from: 40 }), 'Sent back');
assert.strictEqual(stageTrackerHeadline({ kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], regressed_from: 100 }), 'Sent back');
// ...but an issue whose single covering PR sits on 085 still rides the PR's
// merge gate, with the "(its PR)" suffix.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 356, kind: 'issue', stage: 40, linked_prs: [{ number: 109, author_kind: 'hive', stage: 85 }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, 'active detoured', 'todo cap'], row);
assert.ok(tooltip(row).includes('Needs a human decision (its PR)'), row);
// A PR parked at 085 lights the 080 column as a detour; its chevron hover
// names the parking stage, not the column it rides on — the owner read
// "Awaiting merge admission … Next move: human" on hivecommons/hive#14821
// and #14822 while the engine still said 085.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14821, kind: 'pr', stage: 85, caption: 'decision requested' }, stages, ctx);
const chevronTip = (r) => (/<li class="ham-seg active[^"]*"[^>]*title="([^"]*)" aria-current="step"/.exec(r) || [])[1] || '';
assert.ok(chevronTip(row).startsWith('Needs human decision') && !chevronTip(row).includes('Awaiting merge admission') && !chevronTip(row).includes('not yet admitted') && chevronTip(row).includes('Next move: human'), 'detour chevron names its own stage: ' + chevronTip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 14830, kind: 'pr', stage: 80, caption: 'CI green' }, stages, ctx);
assert.ok(chevronTip(row).startsWith('Awaiting merge admission — not yet admitted') && chevronTip(row).includes('Next move: hive'), '080 keeps its own hover: ' + chevronTip(row));
row = stageTrackerRowHtml({ repo: 'acme/console', number: 107, kind: 'pr', stage: 50, modifiers: ['needs-decision'] }, stages, ctx);
assert.ok(row.includes('ham-mod-needs-decision">needs human decision</span>') && tooltip(row).includes('Flags: needs human decision'), row);
// Whose move it is, as an ink-coloured icon after the word: Review = hive,
// PR in progress = the PR author, Claimed = the claimant; Open/Actionable carry none.
assert.ok(row.includes('data-track="ci"') && row.includes('data-track="review"') && !row.includes('ham-actor-ico'), 'a native stage cannot identify the actual reviewer: ' + row);
assert.ok(!row.includes('ham-author-ico ham-actor') && !row.includes('ham-actor-ico ham-author-'), 'actor icon must not carry the author colour classes: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 72, author_kind: 'clanker' }, stages, ctx);
assert.ok(row.includes('Review unknown') && row.includes('CI unknown'), 'legacy author waits do not fabricate observed activity: ' + row);
// 071 needs-author: the owner wait returns to PR in progress without pretending coding.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 109, kind: 'pr', stage: 71, author: 'sam', author_kind: 'human', history: [{number:50,slug:'review-pending'},{number:71,slug:'needs-author',from:50}] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active detoured', T, T, 'todo cap']);
assert.ok(row.includes('aria-label="Next move: human"') && row.includes('<span class="ham-chevron-copy"><span class="ham-detour">Needs @sam</span></span><svg class="ham-ico ham-actor-ico"'), row);
assert.ok(tooltip(row).includes('Trail: CI / Review → Needs author'), tooltip(row));
// The trail names hops from the payload's {number, slug} entries (#14787
// showed "undefined → undefined → …" when it read a field the server never
// sent); the last STAGE_TRACKER_TRAIL_HOPS hops only; an unnamed hop is left
// out rather than printed as "undefined"; the clause is omitted when fewer
// than two hops can be named.
const live14787 = { kind: 'pr', number: 14787, stage: 85, modifiers: ['needs-human'], history: [
  { number: 50, slug: 'review-pending', at: '2026-10-27T21:27:53Z' }, { number: 85, slug: 'needs-decision', from: 50 },
  { number: 50, slug: 'review-pending', from: 85 }, { number: 71, slug: 'needs-author', from: 50 },
  { number: 50, slug: 'review-pending', from: 71 }, { number: 71, slug: 'needs-author', from: 50 },
  { number: 50, slug: 'review-pending', from: 71 }, { number: 85, slug: 'needs-decision', from: 50 }] };
assert.strictEqual(stageTrackerTrailText(live14787), 'CI / Review → Needs author → CI / Review → Needs human decision');
assert.ok(!stageTrackerTooltip(live14787, now).includes('undefined'), stageTrackerTooltip(live14787, now));
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }, { number: 999 }, { number: 85 }] }), 'CI / Review → Needs human decision', 'an unnamed hop is skipped');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }, {}, { foo: 1 }] }), '', 'one nameable hop is no trail');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }] }), '', 'a single hop is no trail');
assert.strictEqual(stageTrackerTrailText({ history: [] }), '');
assert.strictEqual(stageTrackerTrailText({}), '');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 0 }, { number: 10 }] }), 'Open → Actionable', '000 reads as Open');
assert.ok(!stageTrackerTooltip({ kind: 'pr', number: 1, stage: 50, history: [{ number: 999 }, { number: 998 }] }, now).includes('Trail:'), 'no Trail clause when nothing can be named');
// hivecommons/hive#16015: a registered contributor's PR reads as their clanker,
// and the tooltip says the registry decided it; an inferred kind still says so.
assert.ok(stageTrackerTooltip({ kind: 'pr', number: 15998, stage: 74, author: 'Karibhou', author_kind: 'clanker', author_kind_source: 'registry' }, now).includes('Author: @Karibhou (agent, registered contributor)'), 'registry source named in the tooltip');
assert.ok(stageTrackerTooltip({ kind: 'pr', number: 1, stage: 50, author: 'pat', author_kind: 'human', author_kind_source: 'inferred' }, now).includes('Author: @pat (person, inferred)'), 'inferred source still named');
assert.ok(stageTrackerTooltip({ kind: 'pr', number: 1, stage: 50, author: 'pat', author_kind: 'human', author_kind_source: 'label' }, now).includes('Author: @pat (person)'), 'label source says nothing');
const savedStages = _stageTracker;
_stageTracker = null;
assert.strictEqual(stageTrackerTrailText(live14787), 'review-pending → needs-author → review-pending → needs-decision', 'the slug stands in before the stage table loads');
assert.strictEqual(stageTrackerTrailText({ history: [{ number: 50 }, { number: 85 }] }), '', 'numbers alone, no table, no slug: nothing to name');
_stageTracker = savedStages;
row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', stage: 40, linked_prs: [{ number: 108, author_kind: 'clanker', stage: 30 }] }, stages, ctx);
assert.ok(subs(row).length === 0 && strip(row).includes('aria-label="Next move: clanker"') && strip(row).includes('<span class="ham-seg-t">PR in progress</span></span><svg class="ham-ico ham-actor-ico"'), 'single covering PR: the issue strip lights PR in progress with the PR author as the mover: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 7, kind: 'issue', stage: 40, linked_prs: [{ number: 108, author_kind: 'clanker', stage: 30 }, { number: 109, author_kind: 'hive', stage: 50 }] }, stages, ctx);
assert.ok(!strip(row).includes('ham-actor-ico') && !strip(row).includes('aria-current') && subs(row).length === 2 && subs(row)[1].includes('aria-label="Next move: clanker"') && subs(row)[1].includes('<span class="ham-seg-t">PR in progress</span></span><svg class="ham-ico ham-actor-ico"'), 'two covering PRs: the sub-rows, not the issue, light their stage: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 20, claimant_kind: 'hive' }, stages, ctx);
assert.ok(row.includes('aria-label="Next move: hive"') && row.includes('<span class="ham-seg-t">Claimed</span></span><svg class="ham-ico ham-actor-ico"'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 8, kind: 'issue', stage: 20 }, stages, ctx);
assert.ok(!row.includes('ham-actor-ico') && row.includes('<span class="ham-seg-t">Claimed</span>'), 'no claimant known, no icon: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', stage: 10 }, stages, ctx);
assert.ok(!row.includes('ham-actor-ico') && !tooltip(row).includes('Next move'), row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 105, kind: 'pr', stage: 90 }, stages, ctx);
assert.ok(row.includes('aria-label="Next move: human"') && row.includes('<span class="ham-chevron-copy"><span class="ham-detour">Ready for human merge</span></span><svg class="ham-ico ham-actor-ico"'), row);

// Merged: everything neutral-done, the cap carries the only check.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, D, 'active cap merged']);
assert.ok(/<li class="ham-seg active cap merged"[^>]*><svg class="ham-ico"/.test(row), 'merged cap is the lit segment with the check icon');

// Hive-authored PR on its own: hive icon on its PR-column entry, no chips.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 108, kind: 'pr', stage: 50, author: 'hive-bot', author_kind: 'hive' }, stages, ctx);
assert.strictEqual(chips(row), 0);
assert.strictEqual(prCol(row).length, 1);
assert.ok(prCol(row)[0].includes('ham-author-ico ham-author-hive') && lead(row) === 'no linked issue', row);
// PR seen on its own row (Pull requests view) whose open issue is listed:
// the issue — icon, #N link, title — on the left, the PR on the right.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 110, kind: 'pr', title: 'Fix sort', stage: 50, author: 'hive-bot', author_kind: 'hive', nested_under: 9, linked_issues: [{ number: 9, title: 'Ordering drifts', url: 'https://x/9', state: 'open', listed: true }] }, stages, ctx);
assert.strictEqual(lead(row), '#9 Ordering drifts', 'issue slot names the issue: ' + lead(row));
assert.ok(/<a class="ham-ref" href="https:\/\/x\/9"[^>]*>#9<\/a>/.test(row) && !/<a class="ham-ref"[^>]*title=/.test(row), 'issue link, no tooltip on it: ' + row);
assert.ok(prCol(row)[0].includes('<span class="ham-pr-num">#110</span>') && prCol(row)[0].includes('<span class="ham-pr-title">Fix sort</span>'), prCol(row)[0]);
assert.ok(/<a class="ham-pr-ref"[^>]*title="Fix sort · Waiting for review/.test(row), 'row tooltip rides the PR link: ' + row);
assert.strictEqual(titles(row, 'Fix sort'), 1);
// A PR whose only linked issue GitHub reports closed (a superseded PR whose
// body still says Fixes #N): the issue slot keeps "#N title" as a link,
// struck through under a "closed" chip, so the pairing reads at a glance.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 111, kind: 'pr', title: 'Docs', stage: 95, linked_issues: [{ number: 13409, state: 'closed', title: 'Usage on --help', url: 'https://x/13409' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13409 Usage on --help closed · this PR unmerged', lead(row));
assert.ok(/<div class="ham-head-main ham-issue-closed">/.test(row) && /<a class="ham-ref" href="https:\/\/x\/13409"[^>]*>#13409<\/a>/.test(row), 'closed issue stays a link in a struck-through slot: ' + row);
assert.ok(/<span class="ham-title" title="Usage on --help">Usage on --help<\/span> <span class="ham-badge ham-issue-closed-chip" title="Issue #13409 is closed, but this PR is still open and unmerged[^"]*">closed · this PR unmerged<\/span>/.test(row), 'title then closed chip naming the unmerged PR: ' + row);
assert.ok(!row.includes('ham-no-issue-t') && !/<a class="ham-ref"[^>]*title=/.test(row) && prCol(row).length === 1 && prCol(row)[0].includes('#111'), row);
// The same with no title resolvable: a muted "(closed issue)" placeholder, never a blank.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 111, kind: 'pr', title: 'Docs', stage: 95, linked_issues: [{ number: 13409, state: 'closed', url: 'https://x/13409' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13409 (closed issue) closed · this PR unmerged', lead(row));
assert.ok(/<span class="ham-title ham-no-issue-t">\(closed issue\)<\/span> <span class="ham-badge ham-issue-closed-chip" title="[^"]*">closed · this PR unmerged<\/span>/.test(row), row);
// hivecommons/hive#15232: a PR whose closing link names a closed issue
// (closed_issue set by the server, hivecommons/hive#14655) still puts the
// ISSUE in the left slot — struck through under its chip — and itself in the
// PR column, exactly like the soft-linked case above; the PR never leads.
// When the server names the merged PR that closed the issue, the chip reads
// "closed by #N" and links to that PR.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 15191, kind: 'pr', title: 'Let hive decide', url: 'https://x/acme/console/pull/15191', stage: 90, author: 'clubanderson', author_kind: 'human',
  linked_issues: [{ number: 15173, state: 'closed', state_reason: 'completed', title: 'Sweep merges approved PRs', url: 'https://x/acme/console/issues/15173', closed_by: 15181 }],
  closed_issue: { number: 15173, state: 'closed', state_reason: 'completed', title: 'Sweep merges approved PRs', url: 'https://x/acme/console/issues/15173', closed_by: 15181 } }, stages, ctx);
assert.strictEqual(lead(row), '#15173 Sweep merges approved PRs closed by #15181', 'closed issue leads the row, with its closer: ' + lead(row));
assert.ok(/<div class="ham-head-main ham-issue-closed">/.test(row) && /<a class="ham-ref" href="https:\/\/x\/acme\/console\/issues\/15173"[^>]*>#15173<\/a>/.test(row), 'struck-through issue link in the slot: ' + row);
assert.ok(/<a class="ham-badge ham-issue-closed-chip ham-closed-by-chip" href="https:\/\/x\/acme\/console\/pull\/15181"[^>]*>closed by #15181<\/a>/.test(row), 'closed-by chip links the merged PR: ' + row);
assert.ok(!row.includes('ham-head-pr-anchor') && !row.includes('closes <s>'), 'the PR no longer takes the issue slot: ' + row);
assert.ok(prCol(row).length === 1 && prCol(row)[0].includes('#15191') && prCol(row)[0].includes('Let hive decide'), 'PR sits in the PR column: ' + prCol(row));
assert.ok(!row.includes('ham-closed-not-planned'), 'completed close: no moot chip');
// Closer unknown: the chip says the issue closed while this PR is unmerged
// (never a bare "closed" that reads as closed-by-this-PR); a not-planned
// close adds the moot chip.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 15191, kind: 'pr', title: 'Let hive decide', stage: 90,
  linked_issues: [{ number: 15173, state: 'closed', state_reason: 'not_planned', title: 'Sweep', url: 'https://x/15173' }],
  closed_issue: { number: 15173, state: 'closed', state_reason: 'not_planned', title: 'Sweep', url: 'https://x/15173' } }, stages, ctx);
assert.strictEqual(lead(row), '#15173 Sweep closed · this PR unmerged issue closed (not planned)', lead(row));
assert.ok(/<span class="ham-badge ham-issue-closed-chip" title="[^"]*">closed · this PR unmerged<\/span> <span class="ham-badge ham-issue-closed-chip ham-closed-not-planned"/.test(row), row);
// A closer equal to the row's own PR is not a duplicate: the unmerged chip.
assert.ok(/^<span class="ham-badge ham-issue-closed-chip" title="Issue #496 is closed, but this PR is still open and unmerged[^"]*">closed · this PR unmerged<\/span>$/.test(stageTrackerClosedChipHtml({ repo: 'acme/console', number: 500 }, { number: 496, closed_by: 500, title: 'T' })));
assert.deepStrictEqual(stageTrackerPRIssueSlot({ number: 500, linked_issues: [{ number: 496, state: 'closed', title: 'C', url: 'u', closed_by: 501 }] }), { number: 496, title: 'C', url: 'u', closed: true, placeholder: '', closed_by: 501 }, 'closed_by rides the slot');
assert.strictEqual(stageTrackerItemURL('acme/console', 501, 'pull', 'https://ghe.example/acme/console/pull/500'), 'https://ghe.example/acme/console/pull/501', 'closer URL follows the sibling host');
assert.strictEqual(stageTrackerItemURL('acme/console', 501, 'pull', ''), 'https://github.com/acme/console/pull/501');
// A linked issue of unknown state that is not listed still goes in the issue slot as a link, no chip; a missing title is said so.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 112, kind: 'pr', title: 'Fix', stage: 50, linked_issues: [{ number: 77, url: 'https://x/77' }] }, stages, ctx);
assert.strictEqual(lead(row), '#77 (title unavailable)', lead(row));
assert.ok(!row.includes('ham-issue-closed'), 'no closed marker on an issue of unknown state: ' + row);
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 5, state: 'closed' }, { number: 6, state: 'open', title: 'T' }] }), { number: 6, title: 'T', url: '' }, 'an open linked issue beats a closed one');
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 4 }, { number: 5, state: 'closed', title: 'C', url: 'u' }] }), { number: 5, title: 'C', url: 'u', closed: true, placeholder: '' }, 'a known-closed issue beats one of unknown state');
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 5, state: 'closed' }] }), { number: 5, title: '', url: '', closed: true, placeholder: STAGE_TRACKER_CLOSED_ISSUE_TITLE });
assert.deepStrictEqual(stageTrackerPRIssueSlot({}), { number: 0, text: 'no linked issue' });
// Soft links (hivecommons/hive#13544): a Refs #N / (#N, part 1) reference fills the issue slot under a "refs" chip; a closing link never wears one.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 13544, kind: 'pr', title: 'Model heads (#13542, part 1)', stage: 50, nested_under: 13542, linked_issues: [{ number: 13542, title: 'Split the reviewer', url: 'https://x/13542', state: 'open', listed: true, via: 'refs' }] }, stages, ctx);
assert.strictEqual(lead(row), '#13542 Split the reviewer refs', lead(row));
assert.ok(row.includes('<span class="ham-badge ham-issue-refs-chip" title="' + STAGE_TRACKER_SOFT_LINK_TIP + '">refs</span>'), 'refs chip: ' + row);
assert.ok(!row.includes('ham-no-issue'), 'no placeholder once a soft link resolves: ' + row);
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 6, state: 'open', title: 'T', via: 'title' }] }), { number: 6, title: 'T', url: '', soft: true });
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 6, via: 'mention' }] }), { number: 6, title: '', url: '', closed: false, placeholder: STAGE_TRACKER_UNTITLED_ISSUE, soft: true });
assert.deepStrictEqual(stageTrackerPRIssueSlot({ linked_issues: [{ number: 6, state: 'open', title: 'T', via: 'fixes' }] }), { number: 6, title: 'T', url: '' }, 'a hive-read closing keyword is not soft');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 113, kind: 'pr', title: 'Fix', stage: 50, linked_issues: [{ number: 77, state: 'open', title: 'Closed by it' }] }, stages, ctx);
assert.ok(!row.includes('ham-issue-refs-chip'), 'closing link draws no refs chip: ' + row);

// Refs-only issue coverage shows tracker progress and the covering PR's base.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Tracker', stage: 40, claim_reference: true, is_tracker: true, children: { total: 3, completed: 1 }, linked_prs: [{ number: 203, title: 'Draft work', stage: 10, base_ref: 'v6' }] }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-issue-refs-chip" title="' + STAGE_TRACKER_SOFT_LINK_TIP + '">refs</span>'), 'issue refs chip: ' + row);
assert.ok(row.includes('<span class="ham-badge">tracker 1/3</span>'), 'tracker progress: ' + row);
assert.ok(row.includes('<span class="ham-badge">v6</span>'), 'covering PR base: ' + row);

// 040 shares the PR in progress column with 030; the chevron tooltip names the real stage.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Ordering drifts', stage: 40, linked_prs: [{ number: 103, title: 'Backport', author_kind: 'clanker', stage: 30 }, { number: 102, title: 'Fix it', url: 'u', author_kind: 'human', stage: 70, stage_since: new Date(now - 3600000).toISOString(), modifiers: ['needs-rebase'], caption: 'Rebasing onto main' }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, B, B, B, 'blank cap']);
assert.ok(!strip(row).includes('aria-current'), 'issue strip has no lit chevron once PRs cover it');
assert.ok(row.includes('<span class="ham-title" title="Ordering drifts">Ordering drifts</span>'), 'issue keeps its own title on the left');
assert.ok(tooltip(row).includes('Covering PR: #102, #103'));
// Nested sub-rows, furthest along first: "[icon] #N title" at the PR column, then the PR's strip from that column on.
let sr = subs(row);
assert.strictEqual(sr.length, 2, 'one sub-row per covering PR');
assert.ok(prCol(sr[0])[0].includes('ham-author-human') && prCol(sr[0])[0].includes('#102') && prCol(sr[0])[0].includes('<span class="ham-pr-title">Fix it</span>'), prCol(sr[0])[0]);
assert.deepStrictEqual(segStates(sr[0]), [B, B, B, D, 'active detoured', T, 'todo cap'], 'PR strip starts at PR in progress');
assert.ok(sr[0].includes('Waiting for fixer') && !sr[0].includes('ham-actor-ico') && sr[0].indexOf('<span class="ham-since">1h</span>') > sr[0].indexOf('aria-label="Pipeline progress"') && sr[0].includes('ham-mod-needs-rebase">needs rebase</span>'), 'a rebase label and next mover cannot manufacture observed coding: ' + sr[0]);
assert.ok(/^ data-ham-kind="pr" data-ham-stage="70" data-ham-sub-key="acme\/console\/pr#102">/.test(sr[0]) && /<a class="ham-pr-ref"[^>]*title="Fix it · Remediating CI or review findings Rebasing onto main/.test(sr[0]), 'sub-row tooltip rides the PR link, not the container: ' + sr[0].slice(0, 400));
assert.ok(prCol(sr[1])[0].includes('ham-author-clanker') && prCol(sr[1])[0].includes('#103'));
assert.deepStrictEqual(segStates(sr[1]), [B, B, B, 'active', T, T, 'todo cap'], 'a PR at 030 lights PR in progress on its own sub-row');
assert.ok(!row.slice(0, row.indexOf('<div class="ham-sub"')).includes('ham-pr-ref'), 'the issue title line itself lists no PRs');
// One-line sub-row header: connector, author icon + #N + title, lineage chip
// and base pill are direct siblings inside the single flex-row PR cell — no
// block wrapper or <br> between them — and the title carries its full text
// as hover for the ellipsis.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Three PRs', stage: 40, linked_prs: [
  { number: 15343, title: 'docs: remove duplicate README run routes', author_kind: 'hive', stage: 50, base_ref: 'v5', lineage: { kind: 'adopted', from_number: 15226, from_author: 'x', from_author_kind: 'clanker' } },
  { number: 15344, title: 'other', stage: 30 },
] }, stages, ctx);
sr = subs(row);
{
  const head = sr[0].slice(0, sr[0].indexOf('aria-label="Pipeline progress"'));
  const cell = (head.match(/<li class="ham-seg ham-head-pr"[^>]*>([\s\S]*?)<\/li>/) || [])[1] || '';
  assert.ok(cell, 'sub-row header has one PR cell: ' + head);
  assert.ok(!/<br|<div|<p\b/.test(cell), 'no block-level element inside the PR cell: ' + cell);
  const order = [cell.indexOf('class="ham-linked-ico"'), cell.indexOf('<a class="ham-pr-ref"'), cell.indexOf('class="ham-badge ham-lineage"'), cell.lastIndexOf('<span class="ham-badge">v5</span>')];
  assert.ok(order.every((i, k) => i >= 0 && (k === 0 || i > order[k - 1])), 'connector, PR link, lineage chip, base pill in that order as siblings: ' + cell);
  assert.ok(cell.indexOf('<span class="ham-badge">v5</span>') > cell.indexOf('</a>', cell.indexOf('<a class="ham-pr-ref"')), 'the base pill is a sibling after the PR link, not inside it: ' + cell);
  assert.ok(/<a class="ham-pr-ref"[^>]*title="docs: remove duplicate README run routes · /.test(cell), 'the PR link hover starts with the full title: ' + cell);
  assert.ok(!head.includes('ham-lineage" title') || head.indexOf('ham-lineage') < head.indexOf('<span class="ham-badges">'), 'the lineage chip rides the PR cell ahead of the modifier chips, not the chip toolbar: ' + head);
  assert.strictEqual((head.match(/ham-lineage/g) || []).length, 1, 'the lineage chip is drawn once');
}
// The same order on an issue row with a single covering PR: lineage chip
// before the base pill, both after the PR link.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'One PR', stage: 40, linked_prs: [{ number: 15343, title: 'T', stage: 50, base_ref: 'v5', lineage: { kind: 'adopted', from_number: 15226 } }] }, stages, ctx);
{
  const cell = prCell(row);
  assert.ok(cell.indexOf('<a class="ham-pr-ref"') < cell.indexOf('ham-lineage') && cell.indexOf('ham-lineage') < cell.indexOf('<span class="ham-badge">v5</span>'), 'PR link, lineage, base pill: ' + cell);
}
// An incomplete lineage has no chip and must not add an extra separator.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 9, kind: 'issue', title: 'Empty lineage', stage: 40, linked_prs: [{ number: 15343, title: 'T', stage: 50, base_ref: 'v5', lineage: { kind: 'adopted' } }] }, stages, ctx);
assert.match(prCell(row), /<\/a> <span class="ham-badge">v5<\/span>/, 'empty lineage leaves one separator before the base pill');
assert.ok(!prCell(row).includes('ham-lineage'), 'empty lineage renders no chip');
row = stageTrackerRowHtml({ repo: 'acme/console', number: 10, kind: 'issue', stage: 30 }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, 'active', T, T, 'todo cap'], 'kick running, no PR yet: the issue lights PR in progress and draws the steps ahead');
assert.strictEqual(subs(row).length, 0, 'no PR yet');
// A merged covering PR: single entry with the green cap lit; nested when there are two.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', stage: 40, linked_prs: [{ number: 120, title: 'Done', author_kind: 'hive', stage: 110 }] }, stages, ctx);
assert.deepStrictEqual(segStates(row), [D, D, D, D, D, D, 'active cap merged']);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 11, kind: 'issue', stage: 40, linked_prs: [{ number: 121, title: 'Follow-up', author_kind: 'hive', stage: 50 }, { number: 120, title: 'Done', author_kind: 'hive', stage: 110 }] }, stages, ctx);
assert.deepStrictEqual(segStates(subs(row)[0]), [B, B, B, D, D, D, 'active cap merged'], 'furthest along first');
// Title line: lead cell (Open..Claimed) and PR cell (PR in progress..Merge gate) each truncate with an ellipsis; the full title is on the element itself.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 12, kind: 'issue', title: 'A very long issue title that must not wrap', stage: 10 }, stages, ctx);
// No PR to list: the lead cell widens over the empty PR column so a long title has room before it truncates.
assert.ok(/<li class="ham-seg ham-head-lead ham-head-wide"><div class="ham-head-main">[\s\S]*?<span class="ham-title" title="A very long issue title that must not wrap">A very long issue title that must not wrap<\/span>[\s\S]*?<\/li><li class="ham-seg ham-head-pr ham-head-tools"><span class="ham-badges"><\/span><\/li>/.test(row), row);
// Sort key: a covered issue counts as far along as its furthest PR.
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 40, linked_prs: [{ number: 1, stage: 30 }, { number: 2, stage: 90 }] }, stages).anchor, 80);
// An issue parked on 045 (or a pre-045 085) with no PR sorts at Actionable, not past the merge gate.
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 45 }, stages).anchor, 10);
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 15 }, stages).anchor, 10, '015 sorts with Actionable');
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 85 }, stages).anchor, 10);
assert.strictEqual(stageTrackerSortKey({ kind: 'pr', stage: 85 }, stages).anchor, 80);
assert.strictEqual(stageTrackerSortKey({ kind: 'issue', stage: 40, linked_prs: [{ number: 1 }] }, stages).anchor, 30, 'unknown PR stage falls back to the issue');
assert.strictEqual(stageTrackerSortKey({ kind: 'pr', stage: 72 }, stages).anchor, 50);

// Header: names once, aligned with the chevrons, plus the since column.
const head = stageTrackerHeaderHtml(stages);
assert.deepStrictEqual(Array.from(head.matchAll(/<span class="ham-seg-t">(?:<svg[\s\S]*?<\/svg>)?([^<]*)<\/span>/g)).map(m => m[1]), ['Open', 'Actionable', 'Claimed', 'PR in progress', 'CI', 'Review', 'Merge queue']);
// The Merge queue column wears the train icon (hover "Merge queue"), the
// same railcar as the train header chip and the 080 chevron (#15395).
assert.ok(head.includes('<span class="ham-seg-t">' + stageTrackerTrainIconHtml('ham-ico ham-train-icon ham-col-ico') + 'Merge queue</span>'), head);
assert.strictEqual((head.match(/<title>Merge queue<\/title>/g) || []).length, 1);
assert.ok(head.includes('<span class="ham-since">since</span>'));

// Unknown / zero stage lights "Open".
row = stageTrackerRowHtml({ repo: 'acme/console', number: 1, kind: 'issue', stage: 0 }, stages, ctx);
assert.deepStrictEqual(segStates(row), ['active', T, T, T, T, T, 'todo cap']);

// Every PR stage has an active word and an actor on its segment; the word
// is what the chevron says, the actor is whose move it is. The active
// chevron now leads with its stage icon, so the regex skips that svg.
const prWords = {};
for (const [st, word, actor] of [[50, 'CI / Review', 'hive'], [60, 'Review passed', 'hive'], [70, 'Fixing', 'author'], [71, 'Needs author', 'author'], [72, 'Returned to contributor', 'author'], [74, 'Adopted by hive', 'hive'], [80, 'Ready for auto merge', 'hive'], [85, 'Needs human decision', 'human'], [90, 'Ready for human merge', 'human'], [100, 'Merging', 'hive']]) {
  const r = stageTrackerRowHtml({ repo: 'acme/console', number: 500 + st, kind: 'pr', stage: st, author_kind: 'clanker' }, stages, ctx);
  if ([50, 60, 70, 71, 72, 74].includes(st)) {
    assert.ok(r.includes('data-track="ci"') && r.includes('data-track="review"'), 'pre-gate tracks remain separate at ' + st);
    assert.ok(r.includes('CI unknown') && r.includes('Review unknown'), 'legacy labels alone cannot supply activity at ' + st);
    continue;
  }
  const m = /aria-current="step"><span class="ham-chevron-info (?:config-info|ham-chevron-decision)"[^>]*><svg class="ham-ico ham-active-ico"[^>]*>[\s\S]*?<\/svg><span class="ham-chevron-copy"><span class="ham-(?:seg-t|detour)">([^<]*)<\/span><\/span><svg class="ham-ico ham-actor-ico"[^>]*aria-label="Next move: ([a-z]+)"[^>]*>/.exec(r);
  assert.ok(m, 'stage ' + st + ' has no active word + actor: ' + r);
  assert.deepStrictEqual([m[1], m[2]], [word, actor === 'author' ? 'clanker' : actor], 'stage ' + st);
  prWords[st] = m[1];
}
// The responsive caption names the column, the detour word when there is one,
// and the next mover for the strip's active segment; it stays empty when no
// active segment is in range.
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 570, kind: 'pr', stage: 70, author_kind: 'clanker' }, stages), '<p class="ham-current-stage">CI + Review · Waiting for fixer · activity unknown</p>');
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 560, kind: 'pr', stage: 60 }, stages), '<p class="ham-current-stage">CI + Review</p>', 'a reviewed label is not current-head independent approval evidence');
assert.strictEqual(stageTrackerCurrentStageHtml({ repo: 'acme/console', number: 570, kind: 'pr', stage: 70, author_kind: 'clanker' }, stages, { from: 500 }), '', 'no caption when the active segment is out of range');
assert.strictEqual(STAGE_TRACKER_ACTOR[70], 'author', '070 fixing is the author\'s move');
// Filters.
const items = [
  { kind: 'pr', repo: 'a/b', stage: 50 }, { kind: 'issue', repo: 'a/c', number: 1, stage: 10 },
  { kind: 'pr', repo: 'a/b', stage: 72 }, { kind: 'pr', repo: 'a/c', number: 9, stage: 80, modifiers: ['stalled'], nested_under: 1 },
  { kind: 'pr', repo: 'a/c', number: 10, stage: 80, nested_under: 2 },
];
// All / Issues nest a PR under its listed issue; Pull requests flattens; a PR whose issue is not listed stands alone.
assert.deepStrictEqual(stageTrackerVisible(items, { kind: 'all', repo: '' }).map(it => it.number), [undefined, 1, undefined, 10]);
assert.deepStrictEqual(stageTrackerVisible(items, { kind: 'issue', repo: '' }).map(it => it.number), [1]);
assert.strictEqual(stageTrackerVisible(items, { kind: 'pr', repo: '' }).length, 4);
assert.strictEqual(stageTrackerVisible(items).length, 4, 'defaults to the live filter');
_stageTrackerFilter.kind = 'pr'; assert.strictEqual(stageTrackerVisible(items).length, 4);
_stageTrackerFilter.repo = 'a/c'; assert.strictEqual(stageTrackerVisible(items).length, 2);
assert.strictEqual(items.filter(stageTrackerNeedsAttention).length, 2);

// Golden: one visible title per item across every row shape. A PR must never
// be its own parent issue, and must never appear under the Issues filter.
const g = [
  { kind: 'issue', repo: 'g/r', number: 1, title: 'Lone issue', stage: 20, author: 'ann', author_kind: 'human' },
  { kind: 'issue', repo: 'g/r', number: 2, title: 'Issue with one PR', stage: 40, linked_prs: [{ number: 21, title: 'The one PR', author: 'hive-bot', author_kind: 'hive', stage: 50 }] },
  { kind: 'pr', repo: 'g/r', number: 21, title: 'The one PR', stage: 50, author: 'hive-bot', author_kind: 'hive', nested_under: 2 },
  { kind: 'issue', repo: 'g/r', number: 3, title: 'Issue with two PRs', stage: 40, linked_prs: [{ number: 31, title: 'First PR', author: 'bo', author_kind: 'clanker', stage: 72 }, { number: 32, title: 'Second PR', author: 'cy', author_kind: 'human', stage: 90 }] },
  { kind: 'pr', repo: 'g/r', number: 31, title: 'First PR', stage: 72, author: 'bo', author_kind: 'clanker', nested_under: 3 },
  { kind: 'pr', repo: 'g/r', number: 32, title: 'Second PR', stage: 90, author: 'cy', author_kind: 'human', nested_under: 3 },
  { kind: 'pr', repo: 'g/r', number: 40, title: 'Standalone PR', stage: 80, author: 'cy', author_kind: 'human', modifiers: ['needs-rebase'] },
];
const golden = stageTrackerVisible(g, { kind: 'all', repo: '' }).map(it => stageTrackerRowHtml(it, stages, ctx)).join('\n');
for (const it of g) assert.strictEqual(titles(golden, it.title), 1, 'exactly one visible title for #' + it.number + ' (' + it.title + ')');
assert.strictEqual((golden.match(/<div class="ham-row"/g) || []).length, 4, 'four top-level rows: 3 issues + 1 standalone PR');
assert.strictEqual((golden.match(/<div class="ham-sub"/g) || []).length, 2, 'two sub-rows, only under the two-PR issue');
assert.strictEqual((golden.match(/ham-head-wide/g) || []).length, 2, 'wide lead wherever the title line has no PR entry: PR-less issue, two-PR issue (PRs in sub-rows)');
assert.strictEqual((golden.match(/<span class="ham-pr-num">#40<\/span>/g) || []).length, 1, 'the standalone PR sits once in the PR column of its own row');
assert.strictEqual((golden.match(/ham-no-issue-t">no linked issue</g) || []).length, 1, 'exactly one placeholder issue slot: the standalone PR');
for (const r of golden.split('<div class="ham-row"').slice(1)) assert.ok(!/<li class="ham-seg ham-head-lead[^"]*">(?:(?!<\/li>)[\s\S])*ham-pr-ref/.test(r), 'a PR never sits in the issue slot: ' + r.slice(0, 300));
assert.ok(golden.includes('ham-mod-needs-rebase">needs rebase</span>'), 'standalone PR keeps its chip on the title line');
assert.deepStrictEqual(stageTrackerVisible(g, { kind: 'issue', repo: '' }).map(it => it.kind), ['issue', 'issue', 'issue'], 'Issues filter never shows a PR');
assert.deepStrictEqual(stageTrackerVisible(g, { kind: 'pr', repo: '' }).map(it => it.number), [21, 31, 32, 40], 'Pull requests filter flattens every PR once');

// Hover text budget per row (owner request): only the #N link, the PR link,
// the item title and the active chevron carry nonempty title="…". The row container,
// sub-row container, kind / author / actor icons, chips, since cells and
// every non-active chevron carry none; icons expose their label via aria-label.
// Accessible chevron wrappers use title="" only to suppress inherited native
// hover text while the shared portal exposes their full details.
const tags = (html) => Array.from(html.matchAll(/<(\w+)\b([^>]*)>/g)).map(m => ({ tag: m[1], attrs: m[2] }));
const titleAttribute = (tag) => (/(?:^|\s)title="([^"]*)"/.exec(tag.attrs) || [])[1];
const chevronHelp = (tag) => tag.tag === 'span' && /(?:^|\s)class="ham-chevron-info config-info"/.test(tag.attrs);
const titled = (html) => tags(html).filter(t => titleAttribute(t) !== undefined && !t.attrs.includes('data-track=') &&
  !(chevronHelp(t) && titleAttribute(t) === ''));
const expectTitles = (html, want, what) => {
  for (const tag of tags(html).filter(chevronHelp)) {
    assert.strictEqual(titleAttribute(tag), '', what + ': portal wrapper must suppress inherited native hover text');
    assert.match(tag.attrs, /(?:^|\s)tabindex="0"/, what + ': full details remain keyboard reachable');
    assert.match(tag.attrs, /(?:^|\s)aria-label="[^"]+"/, what + ': full accessible caption remains');
    assert.match(tag.attrs, /(?:^|\s)aria-describedby="ham-(?:chevron|track)-tip-[^"]+"/, what + ': shared tooltip stays associated');
  }
  assert.deepStrictEqual(titled(html).map(t => t.tag + '.' + ((/class="([^" ]+)/.exec(t.attrs) || [])[1] || '')), want, what + ': ' + html);
};
assert.strictEqual(titled('<span class="ham-chevron-info config-info" title=""></span>').length, 0, 'only the exact empty portal suppressor is exempt');
assert.strictEqual(titled('<span class="ham-chevron-info config-info" title="unexpected native tooltip"></span>').length, 1, 'nonempty wrapper titles still violate the exact hover budget');
assert.strictEqual(titled('<li class="ham-seg" title=""></li><span class="unexpected" title=""></span>').length, 2, 'unrelated title-bearing extras are never whitelisted');
assert.ok(!/<svg[^>]*>\s*<title>/.test(golden), 'no svg <title> tooltips anywhere in the tracker');
assert.ok(!/<(div|span)[^>]*class="ham-(row|sub|since|badge|kind-ico)[^>]*title="/.test(golden), 'containers, since cells, chips and kind icons carry no title');
for (const r of golden.split('<div class="ham-row"').slice(1)) {
  const seg = Array.from(r.matchAll(/<li class="ham-seg ([^"]*)"([^>]*)>/g));
  for (const m of seg) {
    if (m[2].includes('data-track=')) continue; // Independent evidence tooltips are pinned in the activity-track tests.
    const isActive = /\bactive\b/.test(m[1]);
    assert.strictEqual(/ title="/.test(m[2]), isActive, (isActive ? 'active chevron needs a title' : 'inactive chevron must have no title') + ': ' + m[0]);
  }
}
// Issue with no PR: #N link, issue title, active chevron.
row = stageTrackerRowHtml(g[0], stages, ctx);
expectTitles(row, ['a.ham-ref', 'span.ham-title', 'li.ham-seg'], 'lone issue');
assert.ok(/<li class="ham-seg active" title="Claimed" aria-current="step">/.test(row), 'active chevron names the stage (no claimant known, no mover line): ' + row);
assert.ok(/<a class="ham-ref"[^>]*title="[^"]*@ann[^"]*"/.test(row), 'the #N link carries the full row tooltip');
// Issue with one PR: #N link, issue title, PR link (its title + author), active chevron.
row = stageTrackerRowHtml(g[1], stages, ctx);
expectTitles(row, ['a.ham-ref', 'span.ham-title', 'a.ham-pr-ref'], 'issue with one PR; independent track tooltips are separate');
assert.ok(/<a class="ham-pr-ref"[^>]*title="The one PR Author: hive · @hive-bot · linked PR, shown on its issue&#39;s row"/.test(row), 'the PR link also explains the shared row: ' + row);
// tracker-cues: with no agent on the item the chevron is static (waiting);
// the pulse and the "Working now" lead need a worker in ctx.workers.
assert.ok(row.includes('CI unknown') && row.includes('Review unknown') && !strip(row).includes('data-motion='), 'a stage label alone supplies no activity: ' + row);
row = stageTrackerRowHtml(g[1], stages, Object.assign({}, ctx, { workers: { 'g/r#21': { agent: 'scout', action: 'agent_comment_created', at: new Date(now - 120000).toISOString() } } }));
assert.ok(row.includes('Review unknown') && !strip(row).includes('data-motion='), 'a generic commenter is not a current-head reviewer: ' + row);
assert.ok(/class="ham-row cue-working"/.test(row), 'the row carries the working cue: ' + row);
assert.ok(!/<li class="ham-seg active" data-motion=[^>]*title="Claimed"/.test(stageTrackerRowHtml(g[0], stages, ctx)), 'Claimed is static: no data-motion');
// Issue with two PRs: #N link + title on the issue line, nothing lit there;
// each sub-row has its PR link (full PR tooltip) and its active chevron.
row = stageTrackerRowHtml(g[3], stages, ctx);
expectTitles(row.split('<div class="ham-sub"')[0], ['a.ham-ref', 'span.ham-title'], 'two-PR issue line');
for (const s of subs(row)) expectTitles(s, ['a.ham-pr-ref', 'li.ham-seg'], 'sub-row');
assert.ok(/<a class="ham-pr-ref"[^>]*title="First PR · Back with the contributor[^"]*@bo \(agent\)[^"]*"/.test(subs(row)[1]), 'sub-row PR link: PR title, then the PR tooltip: ' + subs(row)[1]);
// Standalone PR: the PR link (its title, then the row tooltip) and the active
// chevron; the placeholder issue slot carries none.
row = stageTrackerRowHtml(g[6], stages, ctx);
expectTitles(row, ['a.ham-pr-ref', 'li.ham-seg'], 'standalone PR');
assert.ok(/<a class="ham-pr-ref"[^>]*title="Standalone PR · Not yet admitted to auto merge[^"]*@cy \(person\)/.test(row), 'PR link: title, then the row tooltip: ' + row);
assert.ok(row.includes('aria-label="human · @cy"') && !row.includes('<title>'), 'author icon label is aria-only: ' + row);
// Since cells are untouched: plain text, no title.
assert.ok(/<span class="ham-since( ham-stalled)?">[^<]*<\/span>/.test(row) && !/ham-since[^>]*title=/.test(golden), 'since cells unchanged');
// hive#16509 row: adopted by hive with a finished review (5/5, human
// decision asked). The Review track is done-style and muted — never the amber
// in-progress fill — and reads the verdict, not "awaiting human decision";
// 11/11 passed CI is done-style too.
{
  const seen = new Date(now - 60000).toISOString();
  const adoptedRow = stageTrackerRowHtml({ repo: 'acme/console', number: 16509, kind: 'pr', stage: 74, display_column: 30, head: 'f269349', author: 'thatguywob', author_kind: 'human',
    code_activity: { head: 'f269349', state: 'adopted', caption: 'Adopted by hive · activity', source: 'adoption', observed_at: seen },
    ci_activity: { head: 'f269349', state: 'passing', caption: 'CI 11/11 passed', checks_passed: 11, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: 'f269349', state: 'human_decision', caption: 'Review findings · human decision', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } }, stages, ctx);
  const review = /<li class="ham-seg ham-activity-track ([^"]*)" data-track="review"[^>]*>[\s\S]*?<\/li>/.exec(adoptedRow);
  assert.ok(review, 'review track present: ' + adoptedRow);
  assert.strictEqual(review[1], 'done decided', 'a finished review back in PR in progress is settled, not amber: ' + review[0]);
  assert.ok(review[0].includes('Review: Review 5/5 · changes requested · ') && !review[0].includes('awaiting human') && !review[0].includes('data-motion=') && review[0].includes('ham-ring-decided'), 'the verdict rides the tooltip; the inactive track is its ring alone: ' + review[0]);
  const ci = /<li class="ham-seg ham-activity-track ([^"]*)" data-track="ci"/.exec(adoptedRow);
  assert.strictEqual(ci[1], 'done', '11/11 passed is done-style: ' + adoptedRow);
  const parkedRow = stageTrackerRowHtml({ repo: 'acme/console', number: 16510, kind: 'pr', stage: 85, display_column: 85, head: 'f269349', needs_human: true, author: 'thatguywob', author_kind: 'human',
    review_activity: { head: 'f269349', state: 'human_decision', caption: 'Review findings · human decision', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } }, stages, ctx);
  assert.ok(/<li class="ham-seg ham-activity-track done decided" data-track="review"[^>]*title="Review: Review 5\/5 · needs owner sign-off · /.test(parkedRow), 'genuinely parked on a person names the sign-off in the tooltip, still not amber: ' + parkedRow);
}
// hivecommons/hive#16540 rows. #16509: adopted, CI 7/11 unreported, review
// decided → one lit chevron. #16268: waiting for the contributor, CI 7/11
// unreported, review approved → exactly one. Three qualifying (fix running,
// CI running, review running) → the tracks win and the stage chevron is
// settled-current. The ≤2 invariant itself runs on every fixture above.
{
  const seen = new Date(now - 60000).toISOString();
  const unreported = { head: 'f269349', state: 'missing', caption: 'CI waiting · required check "test" hasn\'t reported for this head yet', checks_passed: 7, checks_total: 11, blocker: 'test', source: 'checks', observed_at: seen };
  const lit = (html) => litChevrons(strip(html));
  const three = stageTrackerRowHtml({ repo: 'acme/console', number: 16509, kind: 'pr', stage: 74, display_column: 30, head: 'f269349', author: 'thatguywob', author_kind: 'human',
    code_activity: { head: 'f269349', state: 'adopted', caption: 'Adopted by hive · activity', source: 'adoption', observed_at: seen }, ci_activity: unreported,
    review_activity: { head: 'f269349', state: 'human_decision', caption: 'Review findings · human decision', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(three), 1, 'adopted + unreported CI + decided review lights one chevron: ' + strip(three));
  assert.ok(/<li class="ham-seg ham-activity-track todo" data-track="ci"[^>]*title="CI: CI 7\/11 · test not reported · /.test(three), 'unreported checks are the neutral icon, words in the tooltip: ' + three);
  const owner = stageTrackerRowHtml({ repo: 'acme/console', number: 16268, kind: 'pr', stage: 71, display_column: 30, head: 'f269349', author: 'hanthor', author_kind: 'human',
    code_activity: { head: 'f269349', state: 'waiting_owner', caption: 'Waiting for @hanthor · not observed coding', actor: 'hanthor', source: 'native-author-wait', observed_at: seen }, ci_activity: unreported,
    review_activity: { head: 'f269349', state: 'approved', caption: 'Review approved', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(owner), 1, 'with the contributor: only the stage chevron is lit: ' + strip(owner));
  assert.ok(/<li class="ham-seg active detoured"[^>]*aria-current="step"/.test(owner) && owner.includes('Waiting for @hanthor'), owner);
  assert.ok(/<li class="ham-seg ham-activity-track todo" data-track="ci"[^>]*title="CI: CI 7\/11 · test not reported · /.test(owner) && /<li class="ham-seg ham-activity-track done" data-track="review"[^>]*data-verdict="pass"[^>]*>[\s\S]*?ham-track-verdict"><svg [^>]*>[\s\S]*?<\/svg><\/span>[\s\S]*?Review: Review 5\/5 approved · /.test(owner), 'with the contributor both tracks are icons alone, words in the tooltips: ' + owner);
  const busy = stageTrackerRowHtml({ repo: 'acme/console', number: 16456, kind: 'pr', stage: 70, display_column: 30, head: 'f269349', author: 'hive[bot]', author_kind: 'hive',
    code_activity: { head: 'f269349', state: 'remediating', caption: 'Remediating · scanner · 4m', actor: 'scanner', active: true, source: 'fix-dispatch', observed_at: seen },
    ci_activity: { head: 'f269349', state: 'running', active: true, caption: 'CI running', checks_passed: 4, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: 'f269349', state: 'reviewing', active: true, actor: 'reviewer-2', caption: 'Review working', perspectives_done: 2, perspectives_total: 5, source: 'dispatch', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(busy), 2, 'three qualify, two light: ' + strip(busy));
  // #16456: back in PR in progress with changes requested — one filled
  // chevron; CI 11/11 is green words (done), the review finding red words
  // (failing), neither a fill.
  const changes = stageTrackerRowHtml({ repo: 'acme/console', number: 16456, kind: 'pr', stage: 70, display_column: 30, head: 'f269349', author: 'hive[bot]', author_kind: 'hive',
    code_activity: { head: 'f269349', state: 'unassigned', caption: 'Changes requested · unassigned · next: scanner', source: 'no-pickup-evidence', since: seen, observed_at: seen },
    ci_activity: { head: 'f269349', state: 'passing', caption: 'CI green', checks_passed: 11, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: 'f269349', state: 'changes_requested', caption: 'Review findings · changes requested', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: seen } }, stages, ctx);
  assert.strictEqual(lit(changes), 1, 'changes requested in PR in progress: one filled chevron: ' + strip(changes));
  assert.ok(/<li class="ham-seg ham-activity-track done" data-track="ci"[^>]*data-verdict="pass"[^>]*>[\s\S]*?CI: CI 11\/11 passed · /.test(changes) && /<li class="ham-seg ham-activity-track failing" data-track="review"[^>]*data-verdict="fail"[^>]*>[\s\S]*?Review: Review 5\/5 · changes requested · /.test(changes), changes);
  assert.ok(!/ham-track-verdict"><svg [^>]*>[\s\S]*?<\/svg>(?:CI|Review) /.test(changes), 'settled verdicts carry no words beside the glyph: ' + changes);
  assert.ok(/<li class="ham-seg active ham-settled-current ham-remediation-remediating detoured"/.test(busy) && busy.includes('Remediating · scanner · 4m'), 'the stage chevron is settled-current with its words: ' + busy);
}
// hivecommons/hive#16524: 085 under hive:unauthorized (a maintainer must
// rerun shared CI on GitHub), CI unknown, review findings open. The ask is
// the person's move ON THE PR, not a merge matter: the chevron lights PR in
// progress with "Waiting for @clubanderson · rerun shared ci on GitHub"
// and the person icon; the Merge queue column stays empty. A merge-ready
// twin (approved + CI green) or a queued one keeps the gate.
{
  const seen = new Date(now - 60000).toISOString();
  const head = '7879a51';
  const ask = { question: "The hive's GitHub App may not rerun shared ci here. A maintainer has to do that step on GitHub.", asked_by: 'GitHub permissions', since: seen, blocker_reason: 'unauthorized:rerun-shared-ci', options: [{ action: 'decide:delegate', label: 'Let hive decide', enabled: false }] };
  const shape = (extra) => Object.assign({ repo: 'hivecommons/hive', number: 16524, kind: 'pr', stage: 85, head, author: 'clubanderson', author_kind: 'human', modifiers: ['unauthorized'],
    reason: 'unauthorized:rerun-shared-ci', unauthorized_step: 'rerun shared ci', unauthorized: "The hive's GitHub App lacks the permission to rerun shared ci, so only a person can take that step.",
    caption: 'Waiting for a maintainer · rerun shared ci on GitHub — the hive\'s GitHub App is not permitted to rerun shared ci; a maintainer must', decision_needed: ask,
    human_attention: { number: 16524, kind: 'pr', head, reason: ask.question, asked_by: 'GitHub permissions' },
    ci_activity: { head, state: 'unknown', caption: 'CI unknown · no check results observed for this head yet', checks_passed: 34, checks_total: 42, checks_failed: 1, source: 'github-graphql-cache+required-branch', observed_at: seen },
    review_activity: { head, state: 'changes_requested', caption: 'Review findings · awaiting author', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen, since: seen } }, extra || {});
  const rerun = stageTrackerRowHtml(shape(), stages, ctx);
  assert.strictEqual(stageTrackerAnchor(85, stages, 'pr', shape()), 30, 'the ask is the person\'s move on the PR: PR in progress');
  assert.deepStrictEqual(segStates(rerun), [D, D, D, 'active detoured', T, T, 'todo cap'], 'PR in progress lit, Merge queue empty: ' + strip(rerun));
  assert.ok(rerun.includes('Waiting for @clubanderson · rerun shared ci on GitHub'), 'the chevron names the person and the step: ' + strip(rerun));
  assert.ok(!/<li class="ham-seg active[^"]*"[^>]*>[\s\S]*?Awaiting human/.test(strip(rerun)), 'no "Awaiting human" chevron: ' + strip(rerun));
  const litSeg = /<li class="ham-seg active detoured"[^>]*>([\s\S]*?)<\/li>/.exec(strip(rerun));
  assert.ok(litSeg && litSeg[1].includes('ham-actor-ico'), 'the person icon stays on the lit chevron: ' + strip(rerun));
  const mergeSeg = Array.from(strip(rerun).matchAll(/<li class="ham-seg ([^"]*)"[^>]*>([\s\S]*?)<\/li>/g)).filter(m => !m[1].includes('ham-activity-track') && !m[1].includes('ham-head'))[4];
  assert.ok(mergeSeg && mergeSeg[1] === 'todo' && !/ham-chevron-copy/.test(mergeSeg[2]), 'the merge-queue chevron carries no words: ' + mergeSeg[0]);
  assert.strictEqual(litChevrons(strip(rerun)), 1, 'one lit chevron: ' + strip(rerun));
  // Merge-ready (approved + CI green) or queued twins are merge matters: the gate keeps them.
  const ready = shape({ ci_activity: { head, state: 'passing', caption: 'CI green', checks_passed: 42, checks_total: 42, source: 'checks', observed_at: seen }, review_activity: { head, state: 'approved', caption: 'Review approved', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } });
  assert.strictEqual(stageTrackerAnchor(85, stages, 'pr', ready), 80, 'merge-ready 085 keeps the gate');
  assert.strictEqual(stageTrackerAnchor(85, stages, 'pr', shape({ merge_train: { id: 'v5#1', state: 'queued-behind', front: 1, position: 2, size: 2 } })), 80, 'a train member keeps the gate');
  // A bare 085 decision (no track evidence, no step) still decides at the gate (#14821).
  assert.strictEqual(stageTrackerAnchor(85, stages, 'pr', { kind: 'pr', stage: 85, caption: 'decision requested' }), 80);
  // A nested PR under its issue follows the same column.
  const nested = stageTrackerRowHtml({ repo: 'hivecommons/hive', number: 16500, kind: 'issue', stage: 40, linked_prs: [shape()] }, stages, ctx);
  assert.ok(nested.includes('Waiting for @clubanderson · rerun shared ci on GitHub'), 'nested PR carries the word: ' + nested);
}
// hivecommons/hive#16599 rows (canonical: active = amber + words, inactive =
// icon only). #16567 (dependabot, 050, review approved, 10 workflow runs
// behind GitHub's approve-workflows gate): nothing moves, so the CI column
// is the stalled active track with the person icon and the words "1/11 ·
// waiting on workflow approval"; Review is the green ring alone. #427 (050,
// swarm queued, CI passed): Review is the stage's own track, active even
// while queued; CI is the green icon only. CI running + swarm adjudicating
// lights both tracks with words, never a third.
{
  const seen = new Date(now - 60000).toISOString();
  const lit = (html) => litChevrons(strip(html));
  const gated = stageTrackerRowHtml({ repo: 'acme/console', number: 16567, kind: 'pr', stage: 50, display_column: 50, head: 'b9cd4ba', author: 'dependabot[bot]', author_kind: 'bot',
    ci_activity: { head: 'b9cd4ba', state: 'missing', caption: 'CI waiting · 10 workflow runs need approval on GitHub', checks_passed: 1, checks_total: 11, blocker: 'workflow-approval-required:10', actor: 'dependabot[bot]', source: 'github-check-runs', observed_at: seen, workflow_approval: { pending: 10 } },
    review_activity: { head: 'b9cd4ba', state: 'approved', caption: 'Review approved', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(gated), 1, 'the stalled CI column is the one active chevron: ' + strip(gated));
  assert.ok(/<li class="ham-seg ham-activity-track active ham-stalled ham-current" data-track="ci" data-stalled="true"(?![^>]*data-motion)[^>]*>[\s\S]*?CI 1\/11 · waiting on workflow approval<\/span><svg class="ham-ico ham-actor-ico"[^>]*aria-label="Needs a maintainer to approve the workflow runs on GitHub[^"]*"/.test(gated), 'stalled CI is amber-muted, still, names the wait and wears the person icon: ' + gated);
  assert.ok(/<svg class="ham-ico ham-ring ham-ring-pass" [^>]*aria-label="CI: CI 1\/11 · waiting on workflow approval" data-ring-total="11" data-ring-done="1" data-ring-failed="0">/.test(gated), 'the CI glyph is the 11-step ring with one step done: ' + gated);
  assert.ok(/<li class="ham-seg ham-activity-track done ham-current" data-track="review"[^>]*data-verdict="pass"[^>]*>[\s\S]*?ham-track-verdict"><svg class="ham-ico ham-ring ham-ring-pass"[^>]*data-ring-done="5"[\s\S]*?<\/svg><\/span><span id=/.test(gated), 'approved review is the green ring alone: ' + gated);
  assert.ok(gated.includes('10 workflow runs need approval on GitHub'), 'the tooltip keeps the backend sentence: ' + gated);
  const queued = stageTrackerRowHtml({ repo: 'acme/promptargs', number: 427, kind: 'pr', stage: 50, display_column: 50, head: '9bdf058', author: 'hive[bot]', author_kind: 'hive',
    ci_activity: { head: '9bdf058', state: 'passing', caption: 'CI green', checks_passed: 11, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: '9bdf058', state: 'unassigned', caption: 'Review queued · unassigned', perspectives_done: 0, perspectives_total: 5, source: 'review-dispatch-wait', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(queued), 1, 'the stage\'s own track is active while queued: ' + strip(queued));
  assert.ok(/<li class="ham-seg ham-activity-track active ham-current" data-track="review"(?![^>]*data-motion)[^>]*>[\s\S]*?Review 0\/5 · queued<\/span>/.test(queued), 'Review 0/5 · queued is amber and steady: ' + queued);
  assert.ok(/<li class="ham-seg ham-activity-track done ham-current" data-track="ci"[^>]*data-verdict="pass"[^>]*>[\s\S]*?<svg class="ham-ico ham-ring ham-ring-pass"[^>]*data-ring-done="11"[\s\S]*?<\/svg><\/span><span id=/.test(queued) && queued.includes('CI: CI 11/11 passed · '), 'passed CI is the green ring, words in the tooltip: ' + queued);
  const ahead = stageTrackerRowHtml({ repo: 'acme/promptargs', number: 427, kind: 'pr', stage: 50, display_column: 50, head: '9bdf058', author: 'hive[bot]', author_kind: 'hive',
    ci_activity: { head: '9bdf058', state: 'passing', caption: 'CI green', checks_passed: 11, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: '9bdf058', state: 'unassigned', caption: 'Review queued · unassigned', perspectives_done: 0, perspectives_total: 5, queue_ahead: 3, source: 'review-dispatch-wait', observed_at: seen } }, stages, ctx);
  assert.ok(ahead.includes('Review 0/5 · queued, 3 ahead</span>'), ahead);
  const progress = stageTrackerRowHtml({ repo: 'acme/promptargs', number: 427, kind: 'pr', stage: 50, display_column: 50, head: '9bdf058', author: 'hive[bot]', author_kind: 'hive',
    ci_activity: { head: '9bdf058', state: 'missing', caption: 'CI waiting', checks_passed: 7, checks_total: 11, blocker: 'test', source: 'checks', observed_at: seen },
    review_activity: { head: '9bdf058', state: 'reviewing', active: true, caption: 'Review 2/5 · in progress', perspectives_done: 2, perspectives_total: 5, source: 'verified-review-bundle+stage-review-pending', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(progress), 1, 'review in progress lights the Review column only: ' + strip(progress));
  assert.ok(/<li class="ham-seg ham-activity-track active ham-current" data-track="review"[^>]*data-motion="active"[^>]*>[\s\S]*?Review 2\/5 · in progress<\/span>/.test(progress), progress);
  assert.ok(/<li class="ham-seg ham-activity-track todo ham-current" data-track="ci"[^>]*>[\s\S]*?<svg class="ham-ico ham-ring ham-ring-pass"[^>]*data-ring-done="7"[\s\S]*?<\/svg><\/span><span id=/.test(progress) && progress.includes('CI: CI 7/11 · test not reported · '), 'unreported checks are the neutral ring, words in the tooltip: ' + progress);
  assert.ok(/<svg class="ham-ico ham-ring ham-ring-live ham-ring-running" [^>]*data-ring-total="5" data-ring-done="2"/.test(progress) && (progress.match(/ham-ring-pulse/g) || []).length === 1, 'the review ring pulses its next step: ' + progress);
  const both = stageTrackerRowHtml({ repo: 'acme/promptargs', number: 427, kind: 'pr', stage: 50, display_column: 50, head: '9bdf058', author: 'hive[bot]', author_kind: 'hive',
    ci_activity: { head: '9bdf058', state: 'running', active: true, caption: 'CI running', checks_passed: 4, checks_total: 11, source: 'checks', observed_at: seen },
    review_activity: { head: '9bdf058', state: 'adjudicating', active: true, caption: 'Review 5/5 · verdicts in · aggregating', perspectives_done: 5, perspectives_total: 5, source: 'verified-review-bundle', observed_at: seen } }, stages, ctx);
  assert.strictEqual(lit(both), 2, 'CI running + adjudicating: both tracks, no third: ' + strip(both));
  assert.ok(both.includes('CI 4/11 · running</span>') && both.includes('Review 5/5 · adjudicating</span>'), both);
}
// Merged: the lit cap is the only chevron with hover text.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 106, kind: 'pr', stage: 110 }, stages, ctx);
assert.ok(/<li class="ham-seg active cap merged" title="Merged — [^"]*" aria-current="step">/.test(row) && titled(strip(row)).length === 1, row);
// Who finishes it: a row the your-turn decision hands to a person carries the
// "needs a person" chip with the verb and reason on hover, even while the lit
// step is Hive's; an issue riding one covering PR takes the PR's answer.
const approval = "Waiting on a person's review: an approving review is required by branch protection (0 given)";
row = stageTrackerRowHtml({ repo: 'acme/console', number: 120, kind: 'pr', stage: 50, finisher: 'person', finisher_verb: 'REVIEW', finisher_reason: approval }, stages, ctx);
assert.ok(row.includes('<span class="ham-badge ham-finisher-person" title="Hive cannot finish this on its own: REVIEW — Waiting on a person&#39;s review: an approving review is required by branch protection (0 given)">🧑 needs a person</span>'), row);
for (const finisher of ['hive', 'elsewhere', '']) {
  row = stageTrackerRowHtml({ repo: 'acme/console', number: 121, kind: 'pr', stage: 50, finisher }, stages, ctx);
  assert.ok(!row.includes('ham-finisher-person'), finisher + ': ' + row);
}
// The needs-human and needs-decision pills only repeat the chip, so they fold
// into its hover; any other modifier still gets its pill.
row = stageTrackerRowHtml({ repo: 'acme/console', number: 128, kind: 'pr', stage: 85, finisher: 'person', finisher_verb: 'DECIDE', finisher_reason: 'Waiting on a decision', modifiers: ['needs-human', 'needs-decision'] }, stages, ctx);
assert.ok(row.includes('title="Hive cannot finish this on its own: DECIDE — Waiting on a decision · also flagged: needs human, needs human decision">🧑 needs a person</span>'), 'folded pills are named on the chip: ' + row);
assert.ok(!row.includes('ham-mod-needs-human') && !row.includes('ham-mod-needs-decision'), 'no repeated pill beside the chip: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 129, kind: 'pr', stage: 85, finisher: 'person', modifiers: ['needs-human', 'hold'] }, stages, ctx);
assert.ok(row.includes('ham-mod-hold') && !row.includes('ham-mod-needs-human') && !row.includes(' +1'), 'only the repeated pill folds; hold keeps its own, with no +1 for the folded one: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 130, kind: 'pr', stage: 85, finisher: 'hive', modifiers: ['needs-human'] }, stages, ctx);
assert.ok(row.includes('ham-mod-needs-human') && !row.includes('ham-finisher-person'), 'without the chip the needs-human pill stays: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 122, kind: 'issue', stage: 40, finisher: 'elsewhere', linked_prs: [{ number: 123, stage: 50, finisher: 'person', finisher_verb: 'REVIEW', finisher_reason: approval }] }, stages, ctx);
assert.ok(row.includes('ham-finisher-person'), 'an issue riding one PR takes the PR answer: ' + row);
row = stageTrackerRowHtml({ repo: 'acme/console', number: 124, kind: 'issue', stage: 40, finisher: 'person', linked_prs: [{ number: 125, stage: 50, finisher: 'hive' }] }, stages, ctx);
assert.ok(!row.includes('ham-finisher-person'), 'the PR answer wins once the PR has one: ' + row);
const linkedMetadata = { number: 127, stage: 50, finisher: 'person', finisher_verb: 'REVIEW', finisher_reason: approval,
  claimant: { kind: 'hive', agent: 'reviewer' }, merge_train: { state: 'waiting' }, lineage: { kind: 'adopted', from_number: 125 },
  push: { state: 'waiting', login: 'zoe' }, left_train: { why: 'fixing' }, overlap: { other: 128 } };
const combinedLinked = stageTrackerLinkedPRItem({ repo: 'acme/console', number: 126, linked_prs: [linkedMetadata] }, linkedMetadata);
assert.strictEqual(combinedLinked.finisher, 'person', 'nested PR keeps its finisher alongside newer tracker metadata');
assert.strictEqual(combinedLinked.finisher_reason, approval);
for (const field of ['claimant', 'merge_train', 'lineage', 'push', 'left_train', 'overlap']) assert.deepStrictEqual(combinedLinked[field], linkedMetadata[field], 'nested PR retains ' + field);
`
	// Over stdin: the lifted fixture exceeds Linux's per-argument limit.
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node Hive activity monitor fixture failed: %v\n%s", err, out)
	}
}
