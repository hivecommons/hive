// One note line per Activity Monitor row. The owner saw a row carry two
// hive comments — "Needs human: … · requested by a reviewer View PR #16619"
// over "Hive: moved to Needs human decision" — and asked for one:
// needs-human / decision > blocker > next move > stage note, with the
// loser's facts folded in only when they add something the row does not
// already say. Runs the shipped tracker-motion + tracker-human-attention
// sources so the real tuckable line (⬡ icon, View link, one age) is what
// is asserted.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = name => fs.readFileSync(path.join(__dirname, 'static-src/js', name + '.js'), 'utf8');
const fn = (file, name) => {
  const text = source(file);
  const match = text.match(new RegExp('function ' + name + '\\([^\\n]*\\) \\{[^\\n]*\\}\\s*(?:\\n|$)'))
    || text.match(new RegExp('function ' + name + '\\([^\\n]*\\) \\{[\\s\\S]*?\\n    \\}'));
  assert.ok(match, name);
  return match[0];
};
const esc = value => String(value).replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
const STAGES = [
  { number: 40, label: 'stage/040-covered-by-pr', short: 'Covered by PR' },
  { number: 50, label: 'stage/050-ci-review', short: 'CI / Review' },
  { number: 85, label: 'stage/085-needs-human-decision', short: 'Needs human decision' },
];
function load() {
  const c = vm.createContext({ Date, esc, console,
    document: { hidden: false, getElementById: () => null },
    localStorage: { getItem: () => null, setItem: () => {} },
    setTimeout: () => 0, clearTimeout: () => {},
    dashboardSetInterval: () => 0,
    STAGE_TRACKER_TERMINAL: 100, STAGE_TRACKER_PR_COLUMN: 40,
    STAGE_TRACKER_PATH: [0, 10, 20, 40, 50, 80],
    STAGE_TRACKER_LINKED_GLYPH: '↳', STAGE_TRACKER_SOFT_LINK_TIP: 'refs',
    stageTrackerAnchor: n => Number(n) || 0,
    stageTrackerAttentionTier: () => 1,
    stageTrackerLeadStage: lp => lp.stage || 40,
    stageTrackerIcon: (kind, cls) => `<svg class="${cls || ''}" data-kind="${kind}"></svg>`,
    stageTrackerPRRef: (n, url) => `<a href="${esc(url || '#')}">#${n}</a>`,
    stageTrackerPRPillsHtml: ref => ref,
    stageTrackerTitleHtml: text => `<span>${esc(text)}</span>`,
    stageTrackerTooltip: it => it.caption || '',
    stageTrackerLinkedTip: () => '',
    stageTrackerActedBadgeHtml: () => '', stageTrackerRowToolsHtml: () => '',
    stageTrackerChipsHtml: () => '', stageTrackerMoveAttrs: () => '', stageTrackerSinceHtml: () => '',
    stageTrackerStripItems: it => `<li data-stage="${it.stage}"></li>`,
    stageTrackerCurrentStageHtml: it => `<p class="ham-current-stage">${esc((STAGES.find(s => s.number === Number(it.stage)) || {}).short || it.stage)}</p>`,
    stageTrackerActionCtx: () => ({ user: 'viewer' }),
    stageTrackerLeadActionOnRow: () => null,
  });
  vm.runInContext(fn('tracker-words-stage', 'stageTrackerStageName'), c);
  for (const name of ['stageTrackerLinkedSuperseder', 'stageTrackerLinkedSupersededGone', 'stageTrackerLinkedSupersededText', 'stageTrackerLinkedSupersededTip']) vm.runInContext(fn('tracker-words-gate', name), c);
  for (const name of ['stageTrackerItemKey', 'stageTrackerPRKey', 'stageTrackerRowPRKeys']) vm.runInContext(fn('tracker-merged', name), c);
  for (const name of ['stageTrackerLinkedPRItem', 'stageTrackerLinkedPROpen', 'stageTrackerLinkedDrawnHere', 'stageTrackerLinkedSorted', 'stageTrackerHeadColsHtml', 'stageTrackerSubRowHtml', 'stageTrackerBasePillHtml']) vm.runInContext(fn('tracker-row-layout', name), c);
  vm.runInContext(fn('tracker-row-render', 'stageTrackerRowHtml'), c);
  vm.runInContext(fn('tracker-filter', 'stageTrackerPRIssueSlot'), c);
  vm.runInContext(source('tracker-motion'), c);
  vm.runInContext(source('tracker-human-attention'), c);
  // Row chrome outside the note line is not under test.
  vm.runInContext('stageTrackerCueClasses = () => ""; stageTrackerCueLineHtml = () => "";', c);
  return c;
}
const NOW = 1_700_000_000_000;
const lines = html => html.match(/<p class="cue-event[^"]*"[^>]*>[\s\S]*?<\/p>/g) || [];
const ages = html => (html.match(/cue-event-age" data-at="(\d+)"/g) || []).length;

// The screenshot: the reviewer's ask on the row plus the stage note that
// landed 3 min earlier, under a strip lit at Needs human decision.
function screenshotRow() {
  const pr = { kind: 'pr', repo: 'hivecommons/hive', number: 16619, stage: 85, state: 'open', head: 'h16619',
    url: 'https://github.com/hivecommons/hive/pull/16619', title: 'fix', base_ref: 'v5',
    human_attention: { kind: 'pr', number: 16619, head: 'h16619', asked_by: 'a reviewer',
      reason: '@clubanderson should decide whether #16619 or your own #16622 lands for #16617 — both close the same issue' },
    stage_actor: { kind: 'human', login: 'clubanderson', why: 'decide which PR lands' } };
  const item = { kind: 'issue', repo: 'hivecommons/hive', number: 16617, stage: 85, mirrored: true, linked_pr: 16619,
    title: 'Fleet relaunch', linked_prs: [pr], url: 'https://github.com/hivecommons/hive/issues/16617', stage_actor: pr.stage_actor };
  const events = { 'hivecommons/hive/issue#16617': { who: 'hive', text: 'moved to Needs human decision', at: NOW - 180000, openAt: NOW - 180000 } };
  return { item, events };
}

test('needs-human ask + stage note → one line, the chevron restatement dropped, View PR and one age kept', () => {
  const c = load(), { item, events } = screenshotRow();
  const html = c.stageTrackerRowHtml(item, STAGES, { nowMs: NOW, events, noteTuck: true });
  const found = lines(html);
  assert.equal(found.length, 1, html);
  const line = found[0];
  assert.match(line, /ham-human-context/);
  assert.match(line, /cue-event-ico/, 'keeps its ⬡ icon');
  assert.match(line, /Needs human: @clubanderson should decide whether #16619 or your own #16622 lands for #16617/);
  assert.match(line, /· requested by a reviewer <a[^>]*>View PR #16619<\/a>/);
  assert.doesNotMatch(line, /moved to Needs human decision/, 'the lit chevron already reads Needs human decision');
  assert.doesNotMatch(html, /cue-event-who/, 'no second "Hive:" line');
  assert.equal(ages(line), 1, 'one relative timestamp');
});

test('a stage note that says more than the chevron rides the human line as a trailing clause', () => {
  const c = load(), { item, events } = screenshotRow();
  events['hivecommons/hive/issue#16617'].text = 'fell back to Needs human decision — reviewers split on the approach';
  const html = c.stageTrackerRowHtml(item, STAGES, { nowMs: NOW, events, noteTuck: true });
  const found = lines(html);
  assert.equal(found.length, 1);
  assert.match(found[0], /View PR #16619<\/a> · fell back to Needs human decision — reviewers split on the approach/);
  assert.equal(ages(found[0]), 1);
});

test('a row with only a stage note still shows exactly one line, the hive note itself', () => {
  const c = load();
  const item = { kind: 'issue', repo: 'hivecommons/hive', number: 16630, stage: 50, title: 'solo', state: 'open',
    url: 'https://github.com/hivecommons/hive/issues/16630', linked_prs: [], stage_actor: { kind: 'hive' } };
  const events = { 'hivecommons/hive/issue#16630': { who: 'hive', text: 'moved to CI / Review', at: NOW - 5000 } };
  const html = c.stageTrackerRowHtml(item, STAGES, { nowMs: NOW, events, noteTuck: true });
  const found = lines(html);
  assert.equal(found.length, 1, html);
  assert.match(found[0], /cue-event-who">Hive:<\/span> <span class="cue-event-text">moved to CI \/ Review/);
  assert.doesNotMatch(html, /ham-human-context/);
});

test('remediation next move + stage note → one line with the move first and the note folded', () => {
  const c = load();
  const item = { kind: 'pr', repo: 'hivecommons/hive', number: 16640, stage: 50, state: 'open', head: 'h', title: 'pr',
    url: 'https://github.com/hivecommons/hive/pull/16640', base_ref: 'v5',
    stage_actor: { kind: 'human', login: 'author', why: '@author must push a fix; the hive does not edit a person’s PR' } };
  const events = { 'hivecommons/hive/pr#16640': { who: 'hive', text: 'needs a rebase', at: NOW - 60000 } };
  const html = c.stageTrackerRowHtml(item, STAGES, { nowMs: NOW, events, noteTuck: true });
  const found = lines(html);
  assert.equal(found.length, 1, html);
  assert.match(found[0], /Next move: @author — @author must push a fix; the hive does not edit a person’s PR · needs a rebase/);
  assert.doesNotMatch(html, /cue-event-who/);
  assert.equal(ages(found[0]), 1);
});

test('PR sub-rows under an issue follow the same one-line rule', () => {
  const c = load(), { item, events } = screenshotRow();
  // Two covering PRs: the asking one is drawn as a sub-row with its own note.
  const other = { kind: 'pr', repo: 'hivecommons/hive', number: 16622, stage: 50, state: 'open', head: 'h2', title: 'mine',
    url: 'https://github.com/hivecommons/hive/pull/16622', base_ref: 'v5', stage_actor: { kind: 'hive' } };
  item.linked_prs.push(other); item.mirrored = false; item.stage = 40;
  events['hivecommons/hive/pr#16619'] = { who: 'hive', text: 'moved to Needs human decision', at: NOW - 180000 };
  const html = c.stageTrackerRowHtml(item, STAGES, { nowMs: NOW, events, noteTuck: true });
  const sub = html.slice(html.indexOf('data-ham-sub-key="hivecommons/hive/pr#16619"'), html.indexOf('data-ham-sub-key="hivecommons/hive/pr#16622"'));
  const found = lines(sub);
  assert.equal(found.length, 1, sub);
  assert.match(found[0], /Needs human: @clubanderson should decide/);
  assert.doesNotMatch(found[0], /moved to Needs human decision/);
});

test('stage-note fold rules: restatements of the line or the lit chevron are dropped, new facts kept', () => {
  const c = load();
  const item = { kind: 'pr', number: 1, repo: 'r', stage: 85 };
  assert.equal(c.stageTrackerNoteFoldText('moved to Needs human decision', 'Needs human: x', item, STAGES), '');
  assert.equal(c.stageTrackerNoteFoldText('waiting on a decision', 'PR #1 — Needs human: x', item, STAGES), '');
  assert.equal(c.stageTrackerNoteFoldText('Confirm the policy', 'Needs human: Confirm the policy', item, STAGES), '');
  assert.equal(c.stageTrackerNoteFoldText('moved to CI / Review', 'Needs human: x', item, STAGES), 'moved to CI / Review');
  assert.equal(c.stageTrackerNoteFoldText('moved to Needs human decision — why', 'Needs human: x', item, STAGES), 'moved to Needs human decision — why');
  assert.equal(c.stageTrackerNoteFoldText('moved to Needs human decision', 'Needs human: x', { kind: 'pr', number: 1, repo: 'r', stage: 85 }, []), 'moved to Needs human decision', 'no stage table: keep it');
});
