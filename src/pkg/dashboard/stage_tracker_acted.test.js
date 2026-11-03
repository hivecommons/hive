// "Did I act on this?" badge in the Hive activity monitor: the tracker
// payload carries `last_operator_action` (operator_actions.go) and the row
// wears a history badge (clock + counter-clockwise arrow, never a ✓: the
// tick read as "done") whose in-app tooltip names the action, when, the outcome
// and the current stage; amber once the row has sat at the same stage past
// the window; a Decisions filter narrows to awaiting / acted / stuck. The
// functions are lifted straight out of index.html so this runs against the
// shipped source.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

const src = [
  'const window = { _hiveUser: "clubanderson" };',
  'const _stageTracker = { stages: [{ number: 85, short: "Merge gate" }, { number: 100, short: "Merged" }] };',
  'function esc(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }',
  'function agentNowClockTime(ms) { return new Date(ms).toISOString().slice(11, 16); }',
  'function stageTrackerDecisionTarget(item) { return item && item.parked ? item : null; }',
  pick(/function stageTrackerStageWord\(n\) \{[\s\S]*?\n    \}\n/, 'stageTrackerStageWord'),
  pick(/const STAGE_TRACKER_ACTED_LABELS = \{[\s\S]*?\n    \};\n/, 'STAGE_TRACKER_ACTED_LABELS'),
  pick(/const STAGE_TRACKER_ACTED_STATES = [^\n]*\n/, 'STAGE_TRACKER_ACTED_STATES'),
  pick(/const STAGE_TRACKER_ACTED_FILTERS = [^\n]*\n/, 'STAGE_TRACKER_ACTED_FILTERS'),
  pick(/const STAGE_TRACKER_ACTED_STUCK_DEFAULT_S = [^\n]*\n/, 'STAGE_TRACKER_ACTED_STUCK_DEFAULT_S'),
  pick(/const STAGE_TRACKER_ICONS = \{[\s\S]*?\n    \};\n/, 'STAGE_TRACKER_ICONS'),
  pick(/const STAGE_TRACKER_HISTORY_ICON = [^\n]*\n/, 'STAGE_TRACKER_HISTORY_ICON'),
  pick(/const STAGE_TRACKER_ACTED_GLYPH = [^\n]*\n/, 'STAGE_TRACKER_ACTED_GLYPH'),
  pick(/function stageTrackerActedLabel\(action\) \{[^\n]*\n/, 'stageTrackerActedLabel'),
  pick(/function stageTrackerActedSource\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedSource'),
  pick(/function stageTrackerActedState\(action, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedState'),
  pick(/function stageTrackerActedAgo\(ms\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedAgo'),
  pick(/function stageTrackerActedOutcomeWords\(action\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedOutcomeWords'),
  pick(/function stageTrackerActedStageName\(n\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedStageName'),
  pick(/const STAGE_TRACKER_ACTED_PLAIN = \{[\s\S]*?\n    \};\n/, 'STAGE_TRACKER_ACTED_PLAIN'),
  pick(/function stageTrackerActedPlain\(text\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedPlain'),
  pick(/const STAGE_TRACKER_ACTED_OPTION_ID = [^\n]*\n/, 'STAGE_TRACKER_ACTED_OPTION_ID'),
  pick(/function stageTrackerActedOptions\(item, action, state\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedOptions'),
  pick(/function stageTrackerActedTip\(item, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedTip'),
  pick(/function stageTrackerActedBadgeHtml\(item, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedBadgeHtml'),
  pick(/function stageTrackerActedMatches\(item, mode, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerActedMatches'),
].join('\n');
const api = new Function(src + '; return { plain: stageTrackerActedPlain, state: stageTrackerActedState, tip: stageTrackerActedTip, badge: stageTrackerActedBadgeHtml, matches: stageTrackerActedMatches, source: stageTrackerActedSource };')();

const NOW = Date.parse('2026-10-28T16:00:00Z');
function act(over) {
  return Object.assign({ action: 'approve', actor: 'clubanderson', at: new Date(NOW - 5 * 60000).toISOString(), outcome: 'applied', stage_at: 85, state: 'waiting', stuck_after_seconds: 1800 }, over || {});
}
function row(over) {
  return Object.assign({ repo: 'hivecommons/hive', number: 42, kind: 'pr', stage: 85, modifiers: [], caption: 'waiting on merge gate', last_operator_action: act(), operator_action_count: 1 }, over || {});
}

test('no recorded action: no badge, acted filters exclude, awaiting includes parked rows', () => {
  const bare = { repo: 'r', number: 1, kind: 'pr', stage: 85, parked: true };
  assert.strictEqual(api.badge(bare, NOW), '');
  assert.strictEqual(api.matches(bare, 'any', NOW), true);
  assert.strictEqual(api.matches(bare, 'acted', NOW), false);
  assert.strictEqual(api.matches(bare, 'stuck', NOW), false);
  assert.strictEqual(api.matches(bare, 'awaiting', NOW), true);
  assert.strictEqual(api.matches({ repo: 'r', number: 2, kind: 'pr', stage: 30 }, 'awaiting', NOW), false);
});

test('fresh action: history badge (clock + arrow, count 1) with an in-app tooltip, no native title', () => {
  const h = api.badge(row(), NOW);
  assert.match(h, /class="ham-badge ham-acted ham-acted-waiting config-info"/);
  assert.match(h, /<svg class="ham-ico ham-history-ico"[^>]*aria-hidden="true"><path d="M2 8a6 6 0 1 0 6-6/, 'history glyph');
  assert.match(h, /data-ham-acted-count="1"/);
  assert.match(h, /<span class="ham-acted-n" aria-hidden="true">1<\/span>/);
  assert.match(h, /aria-label="History: 1 action — Approve — waiting"/);
  assert.doesNotMatch(h, /[✓✔✗]/, 'no check glyph in the history badge');
  assert.match(h, /tabindex="0"/);
  assert.match(h, /data-action="gh3" data-stop="1"/);
  assert.match(h, /aria-describedby="ham-acted-tip-hivecommons-hive-42"/);
  assert.match(h, /<span id="ham-acted-tip-hivecommons-hive-42" class="config-tooltip ham-acted-tip" role="tooltip">/);
  assert.doesNotMatch(h, / title="/);
  const tip = api.tip(row(), NOW);
  assert.strictEqual(tip, 'At Merge gate — waiting on merge gate.\nYou chose Approve · 15:55 (5 min ago) · done.');
  assert.doesNotMatch(tip, /085/);
  assert.strictEqual(api.matches(row(), 'acted', NOW), true);
  assert.strictEqual(api.matches(row(), 'awaiting', NOW), false);
  assert.strictEqual(api.matches(row(), 'stuck', NOW), false);
});

test('another operator acted: tooltip names them', () => {
  assert.match(api.tip(row({ last_operator_action: act({ actor: 'someone-else' }) }), NOW), /\n@someone-else chose Approve/);
});

test('addressed: stage moved on, badge stays green, tooltip says addressed — now at <stage>', () => {
  const r = row({ stage: 100, last_operator_action: act({ state: 'addressed' }) });
  assert.match(api.badge(r, NOW), /ham-acted-addressed/);
  assert.match(api.tip(r, NOW), /^Addressed — now at Merged\.\nYou chose Approve/);
  assert.strictEqual(api.matches(r, 'stuck', NOW), false);
});

test('stuck: server-derived or aged client-side past the window → amber, "requested … still at …; hive says"', () => {
  const server = row({ last_operator_action: act({ state: 'stuck', at: new Date(NOW - 42 * 60000).toISOString() }) });
  assert.match(api.badge(server, NOW), /ham-acted-stuck/);
  assert.strictEqual(api.tip(server, NOW).split('\n')[0], 'Still at Merge gate 42 min after you asked — hive says: waiting on merge gate.');
  assert.strictEqual(api.matches(server, 'stuck', NOW), true);
  // Still `waiting` in the payload but older than the window by now.
  const aged = row({ last_operator_action: act({ at: new Date(NOW - 31 * 60000).toISOString() }) });
  assert.strictEqual(api.state(aged.last_operator_action, NOW), 'stuck');
  assert.strictEqual(api.state(act(), NOW), 'waiting');
});

test('pending and failed states', () => {
  const pending = row({ last_operator_action: act({ action: 'let-hive-decide', outcome: 'pending', state: 'pending' }) });
  assert.match(api.badge(pending, NOW), /ham-acted-pending config-info"[^>]*><svg class="ham-ico ham-history-ico"/);
  assert.doesNotMatch(api.badge(pending, NOW).split('<span id=')[0], /[✓✗…]/, 'glyph slot carries no text mark');
  assert.match(api.tip(pending, NOW), /^At Merge gate while the hive decides\.\nYou chose Let hive decide · .* · the hive is deciding…\.$/);
  const failed = row({ last_operator_action: act({ outcome: 'failed', detail: 'github 502', state: 'failed' }) });
  assert.match(api.badge(failed, NOW), /ham-acted-failed config-info"[^>]*><svg class="ham-ico ham-history-ico"/);
  assert.doesNotMatch(api.badge(failed, NOW).split('<span id=')[0], /[✓✗]/);
  assert.match(api.tip(failed, NOW), /^Still at Merge gate — try again, or act on GitHub\.\n.* · failed — github 502\./);
  assert.strictEqual(api.matches(Object.assign(failed, { parked: true }), 'awaiting', NOW), true);
});

test('target, note and earlier-action count surface in the tooltip; HTML is escaped', () => {
  const r = row({ operator_action_count: 3, last_operator_action: act({ action: 'close-superseded', target: '#41', note: '<b>dup</b>' }) });
  const tip = api.tip(r, NOW);
  assert.match(tip, /chose Close as superseded #41 · /);
  assert.match(tip, /Your note: <b>dup<\/b>/);
  assert.match(tip, /2 earlier actions on this item\./);
  const h = api.badge(r, NOW);
  assert.match(h, /Your note: &lt;b&gt;dup&lt;\/b&gt;/);
  assert.match(h, /<br>/);
  assert.match(h, /data-ham-acted-count="3"/);
  assert.match(h, /<span class="ham-acted-n" aria-hidden="true">3<\/span>/);
  assert.match(h, /aria-label="History: 3 actions — Close as superseded — waiting"/);
});

test('the history column: header and unmerged cap chevron wear the history mark, not a ✓; merged keeps its ✓', () => {
  // Column header (the "since" row): history icon, labelled, with the hover text.
  const head = html.match(/<li class="ham-seg cap ham-history-head" title="History — decisions and actions on this item">\$\{stageTrackerIcon\('history', 'ham-ico ham-history-ico', 'History — decisions and actions on this item'\)\}<\/li>/);
  assert.ok(head, 'column header cap renders the history icon with its hover text');
  assert.doesNotMatch(html, /<li class="ham-seg cap">\$\{stageTrackerIcon\(STAGE_TRACKER_TERMINAL, 'ham-ico'\)\}<\/li>/, 'no ✓ column header remains');
  // Row cap chevron: ✓ (icon 110) only once merged (sg.terminal), history otherwise.
  assert.match(html, /if \(sg\.cap\) inner = sg\.terminal \? stageTrackerIcon\(sg\.number, 'ham-ico'\) : stageTrackerIcon\('history', 'ham-ico ham-history-ico', 'History — no decisions or actions recorded yet'\);/);
  // The icon map carries the shared path and the badge is neutral, not green.
  assert.match(html, /      history: '<path d="M2 8a6 6 0 1 0 6-6 6\.5 6\.5 0 0 0-4\.5 1\.83L2 5\.33"\/><path d="M2 2v3\.33h3\.33"\/><path d="M8 4\.67V8l2\.67 1\.33"\/>',\n/);
  // The ✓ (icon 110) is still the merged mark, nowhere else in the column.
  assert.match(html, /      110: '<path d="M3 8l3 3 7-7"\/>',\n/);
  assert.match(html, /\.config-info\.ham-acted \{[^}]*color: var\(--text\);/, 'neutral at rest');
  assert.doesNotMatch(html, /\.config-info\.ham-acted \{[^}]*var\(--green\)/, 'not the green ok colour');
  assert.doesNotMatch(html, /\.config-info\.ham-acted \{[^}]*var\(--brand\)/, 'not the amber the stuck state uses');
  const glyphLine = html.match(/const STAGE_TRACKER_ACTED_GLYPH = [^\n]*/)[0];
  assert.doesNotMatch(glyphLine, /[✓✗…]/, 'no text glyphs left in the acted badge map');
});

test('issue row drawn over a single covering PR borrows that PR\'s action', () => {
  const issue = { repo: 'r', number: 7, kind: 'issue', stage: 85, linked_prs: [{ number: 8, stage: 85, last_operator_action: act(), operator_action_count: 1 }] };
  const s = api.source(issue);
  assert.ok(s);
  assert.strictEqual(s.item.number, 8);
  assert.match(api.badge(issue, NOW), /ham-acted-tip-r-8/);
  const two = Object.assign({}, issue, { linked_prs: [issue.linked_prs[0], { number: 9 }] });
  assert.strictEqual(api.source(two), null);
});

test('toolbar carries the Decisions select and the filter plumbing knows `acted`', () => {
  assert.match(html, /<select id="activity-monitor-acted" class="ham-select" data-change-action="hamFilter" data-arg0="acted"/);
  assert.match(html, /<option value="awaiting">Awaiting my decision<\/option><option value="acted">I acted<\/option><option value="stuck">Acted, still stuck<\/option>/);
  assert.match(html, /acted: 'any', sort: 'furthest'/);
  assert.match(html, /setVal\('activity-monitor-acted', f\.acted\);/);
  assert.match(html, /case 'acted': f\.acted = value \|\| 'any'; break;/);
  assert.match(html, /\.config-info\.ham-acted\.ham-acted-stuck \{[^}]*var\(--yellow\)/);
});

test('decision trail: the hive moves that followed are listed in order, in plain words', () => {
  const r = row({ stage: 100, last_operator_action: act({ state: 'addressed', outcome: 'applied', detail: 'unparked', followed: [
    { number: 90, slug: 'ready-for-auto-merge', at: new Date(NOW - 4 * 60000).toISOString() },
    { number: 100, slug: 'merged', at: new Date(NOW - 60000).toISOString(), reason: 'contributor-lane:approved' },
  ] }) });
  const lines = api.tip(r, NOW).split('\n');
  assert.strictEqual(lines[0], 'Addressed — now at Merged.');
  assert.strictEqual(lines[1], 'You chose Approve · 15:55 (5 min ago) · done — released to the hive.');
  assert.strictEqual(lines[2], 'Then the hive moved it to stage 90 · 4 min ago.');
  assert.strictEqual(lines[3], 'Then the hive moved it to Merged — a reviewer approved it · 1 min ago.');
  const back = row({ last_operator_action: act({ followed: [{ number: 30, regressed: true, at: new Date(NOW - 60000).toISOString(), reason: 'contributor-lane:changes-requested' }] }) });
  assert.match(api.tip(back, NOW), /Then the hive sent it back to stage 30 — a reviewer requested changes; needs a new commit · 1 min ago\./);
});

test('plain words: no slugs or codes reach the operator', () => {
  assert.strictEqual(api.plain('contributor-lane:changes-requested'), 'a reviewer requested changes; needs a new commit');
  assert.strictEqual(api.plain('some-new:slug_here'), 'some new slug here');
  assert.strictEqual(api.plain('hive decided: approve (auto-merge-when-green)'), 'the hive decided: approve (rule: auto merge when green)');
  assert.strictEqual(api.plain('Waiting on CI'), 'Waiting on CI');
  assert.strictEqual(api.plain(''), '');
});

test('no-op outcome still shows the badge and says what hive had already decided plus the remaining options', () => {
  const a = act({ action: 'let-hive-decide', outcome: 'no-op', detail: "the hive had already decided 'supersede' at 13:01 UTC and it has not taken effect — still blocked: contributor-lane:changes-requested" });
  const item = { number: 14822, kind: 'pr', stage: 85, modifiers: [], last_operator_action: a, operator_action_count: 1,
    decision_needed: { question: 'Merge or send back?', options: [
      { action: 'decide:delegate', label: 'Let hive decide', enabled: true },
      { action: 'decide:approve', label: 'Merge as maintainer', enabled: true },
      { action: 'decide:direction', label: 'Send back for a new commit', enabled: true },
      { action: 'hold', label: 'Hold', enabled: false, reason_if_disabled: 'already held' },
    ] } };
  const html = api.badge(item, NOW);
  assert.match(html, /ham-acted/, 'badge renders for a no-op');
  const tip = api.tip(item, NOW);
  assert.match(tip, /You chose Let hive decide/);
  assert.match(tip, /nothing changed: the hive had already decided 'supersede' at 13:01 UTC/);
  assert.match(tip, /still blocked: a reviewer requested changes; needs a new commit/, 'lane slug translated to plain words');
  assert.doesNotMatch(tip, /contributor-lane/);
  assert.match(tip, /Options: Merge as maintainer · Send back for a new commit\./, 'offers the other enabled options, not the one just taken nor disabled ones');
});
