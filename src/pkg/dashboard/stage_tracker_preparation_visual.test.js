// CI exercises the shipped projection and actual root/nested row renderers.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = name => fs.readFileSync(path.join(__dirname, 'static-src/js', name + '.js'), 'utf8');
const fn = (file, name) => {
  const found = source(file).match(new RegExp('function ' + name + '\\([^\\n]*\\) \\{[^\\n]*\\}\\s*(?:\\n|$)'))
    || source(file).match(new RegExp('function ' + name + '\\([^\\n]*\\) \\{[\\s\\S]*?\\n    \\}'));
  assert.ok(found, name);
  return found[0];
};
const NOW = Date.parse('2026-10-31T01:00:00Z');
const REPO = 'hivecommons/hive';
const esc = value => String(value).replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
const plain = value => JSON.parse(JSON.stringify(value));

function load() {
  const elements = new Map();
  const ctx = vm.createContext({
    console, Date, esc, document: { getElementById: id => elements.get(id) || null },
    STAGE_TRACKER_TERMINAL: 100, STAGE_TRACKER_PR_COLUMN: 40,
    STAGE_TRACKER_LINKED_GLYPH: '<span class="ham-linked-ico">↳</span>',
    STAGE_TRACKER_SOFT_LINK_TIP: 'soft link',
    stageTrackerAnchor: stage => Number(stage) || 0,
    stageTrackerLeadStage: it => it.stage,
    stageTrackerAttentionTier: () => 1, stageTrackerNormalizeFilter: f => f,
    stageTrackerTrainKey: it => it && it.merge_train ? it.merge_train.id : '',
    stageTrackerTrainRowClass: it => it.merge_train ? ' ham-train ham-train-k0' : '',
    stageTrackerPRRef: (n, url, kind, author, title) => `<a href="${esc(url)}">#${n} ${esc(title || '')}</a>`,
    stageTrackerPRPillsHtml: ref => ref,
    stageTrackerTitleHtml: title => `<span class="ham-title">${esc(title)}</span>`,
    stageTrackerTooltip: it => it.caption || '',
    stageTrackerLinkedTip: () => 'linked PR',
    stageTrackerIcon: () => '',
    stageTrackerActedBadgeHtml: () => '',
    stageTrackerRowToolsHtml: () => '<span class="ham-row-tools"></span>',
    stageTrackerMoveAttrs: () => '',
    stageTrackerSinceHtml: () => '<span class="ham-since"></span>',
    stageTrackerCurrentStageHtml: it => `<span class="ham-current-stage">${it.stage}</span>`,
    stageTrackerStripItems: (it, stages, range) => `<li data-stage="${it.stage}" data-range="${esc(JSON.stringify(range))}"></li>`,
    stageTrackerEventHtml: (key, near) => near && near.events && near.events[key] ? `<p class="cue-event">${esc(near.events[key].text)}</p>` : '',
  });
  const core = source('tracker-words-core');
  for (const name of ['STAGE_TRACKER_TRAIN_ICON_PATH', 'STAGE_TRACKER_TRAIN_ICON_TITLE', 'STAGE_TRACKER_TRAIN_ACCENTS']) {
    const found = core.match(new RegExp('const ' + name + ' = [^\\n]+'));
    assert.ok(found, name);
    vm.runInContext(found[0], ctx);
  }
  for (const name of ['stageTrackerTrainIconHtml', 'stageTrackerTrainAccentClass', 'stageTrackerOrdinal']) vm.runInContext(fn('tracker-words-core', name), ctx);
  for (const name of ['stageTrackerLinkedSuperseder', 'stageTrackerLinkedSupersededGone', 'stageTrackerLinkedSupersededText', 'stageTrackerLinkedSupersededTip']) vm.runInContext(fn('tracker-words-gate', name), ctx);
  for (const name of ['stageTrackerPRKey', 'stageTrackerItemKey', 'stageTrackerRowPRKeys']) vm.runInContext(fn('tracker-merged', name), ctx);
  for (const name of ['stageTrackerLinkedPRItem', 'stageTrackerLinkedPROpen', 'stageTrackerLinkedDrawnHere', 'stageTrackerLinkedSorted', 'stageTrackerAdoptOrphanPRs', 'stageTrackerHeadColsHtml', 'stageTrackerSubRowHtml', 'stageTrackerRowHtml']) vm.runInContext(fn('tracker-row-layout', name), ctx);
  for (const name of ['stageTrackerPreparationWord', 'stageTrackerPreparationAge', 'stageTrackerPreparationChipHtml']) vm.runInContext(fn('tracker-queue', name), ctx);
  for (const name of ['stageTrackerSort', 'stageTrackerPage', 'stageTrackerPRIssueSlot']) vm.runInContext(fn('tracker-filter', name), ctx);
  ctx.stageTrackerChipsHtml = item => ctx.stageTrackerPreparationChipHtml(item);
  vm.runInContext(source('tracker-preparation'), ctx);
  return { ctx, elements };
}
function fixture() {
  const pr = (number, stage, caption) => ({ kind: 'pr', repo: REPO, number, stage, caption, state: 'open',
    head: 'head-' + number, base_ref: 'v5', title: 'PR ' + number, url: 'https://github.com/' + REPO + '/pull/' + number,
    created_at: '2026-10-30T00:00:00Z' });
  const owner = pr(45, 70, 'Review swarm requested; waiting for a verdict on the current head');
  const first = pr(9, 50, 'CI · checks pending');
  const second = pr(2, 70, 'Review · exact-head bundle pending');
  const ticket = (it, role) => ({ role, number: it.number, state: 'current', head_sha: it.head,
    current_head: it.head, since: '2026-10-31T00:30:00Z', pipeline: it.caption,
    detail: 'Ticket matches the current open PR snapshot', ...(role === 'waiting' ? { behind: owner.number } : {}) });
  const q = { repo: REPO, base_ref: 'v5', state: 'observed', observed_at: '2026-10-31T00:59:00Z',
    owner: ticket(owner, 'owner'), waiting: [ticket(first, 'waiting'), ticket(second, 'waiting')] };
  owner.preparation = q.owner; first.preparation = q.waiting[0]; second.preparation = q.waiting[1];
  return { owner, first, second, q, data: { items: [second, first, owner], base_preparation: [q] } };
}
function render(ctx, f, page = f.data.items, listed = page, events = {}) {
  ctx.stageTrackerPreparationVisualSync(f.data, listed, page, [], NOW);
  const items = ctx.stageTrackerAdoptOrphanPRs(page);
  const rows = items.map((item, i) => ({ key: ctx.stageTrackerItemKey(item),
    html: ctx.stageTrackerRowHtml(item, [], { nowMs: NOW, events, prevRow: items[i - 1], nextRow: items[i + 1] }) }));
  return plain(ctx.stageTrackerPreparationInlineRows(rows, items, []));
}
function content(rows) { return rows.map(row => row.html).join(''); }
function rootKeys(rows) { return rows.filter(row => !row.key.startsWith('preparation')).map(row => row.key); }
function rootRow(rows, number, kind = 'pr') {
  const row = rows.find(row => row.key === REPO + '/' + kind + '#' + number);
  assert.ok(row, 'actual ' + kind + ' row #' + number);
  return row;
}
function model(ctx, f, listed = f.data.items, page = listed) {
  return ctx.stageTrackerPreparationVisualModel(f.data, listed, page, []);
}

test('original compact pill and connector decorate actual rows with FIFO help and live comments, not copied cards', () => {
  const { ctx } = load(), f = fixture(), before = plain(f.data);
  const rows = render(ctx, f, [f.owner, f.first, f.second]), html = content(rows);
  assert.deepEqual(rootKeys(rows), [45, 9, 2].map(n => REPO + '/pr#' + n));
  assert.equal(rows[0].key, 'preparation:' + REPO + '/pr#45');
  assert.match(rows[0].html, /ham-train-head ham-train-k3 ham-preparation-head/);
  assert.match(rows[0].html, /ham-train-chip.*Preparation train · 3 PRs · front: #45/);
  assert.match(rows[1].html, /class="ham-row ham-train ham-train-k3 ham-preparation-member ham-train-front"/);
  assert.match(rows[2].html, /class="ham-row ham-train ham-train-k3 ham-preparation-member"/);
  assert.match(rows[3].html, /ham-train-last/);
  assert.match(rows[1].html, /data-ham-stage="70"/);
  assert.match(rows[2].html, /data-ham-stage="50"/);
  assert.match(rows[2].html, /CI · checks pending · queued for base refresh behind PR #45 · ticket age 30m/);
  assert.match(rows[1].html, /Review swarm requested; waiting for a verdict on the current head · preparing base/);
  assert.match(rows[3].html, /Waiting 2nd for preparation/);
  assert.doesNotMatch(html, /<li class="ham-preparation|<section|ham-preparation-rail|Preparation owner · unverified|data-ham-train=|overlapping files|Ready for merge/);
  assert.deepEqual(plain(f.data), before);
});

test('actual custom sorts, pagination and stage ranges are unchanged; backwards order starts separate runs', () => {
  const { ctx } = load(), f = fixture();
  const ordered = ctx.stageTrackerSort(f.data.items, [], { sort: 'number', dir: 'asc' });
  const page = ctx.stageTrackerPage(ordered, { size: 25, page: 1 });
  const before = plain(page), rows = render(ctx, f, page.rows, ordered);
  assert.deepEqual(plain(page), before);
  assert.deepEqual(rootKeys(rows), page.rows.map(it => ctx.stageTrackerItemKey(it)));
  assert.equal(rows.filter(row => row.key.startsWith('preparation:')).length, 3);
  assert.match(content(rows), /continuation/);
  for (const row of rows.filter(row => !row.key.startsWith('preparation:'))) assert.match(row.html, /ham-train-last/);
  assert.deepEqual(plain(model(ctx, f)[0].waiting.map(e => [e.ticket.number, e.position])), [[9, 1], [2, 2]]);
});

test('unrelated rows terminate rails; filtered FIFO gaps use dashed continuation without inventing adjacency', () => {
  const { ctx } = load(), f = fixture();
  const other = { ...f.first, number: 80, head: 'other', preparation: null };
  f.data.items.push(other);
  let rows = render(ctx, f, [f.owner, other, f.first, f.second]);
  assert.match(rootRow(rows, 45).html, /ham-train-last/);
  assert.doesNotMatch(rootRow(rows, 80).html, /ham-preparation-member|ham-preparation-node/);
  assert.equal(rows.filter(row => row.key.startsWith('preparation:')).length, 2);
  rows = render(ctx, f, [f.owner, f.second], [f.owner, f.second]);
  assert.match(rootRow(rows, 2).html, /ham-preparation-gap/);
  assert.match(rows[0].html, /2 of 3 PRs here/);
  assert.match(rows[0].html, /#9 · Waiting 1st for preparation · hidden by filters/);
  assert.deepEqual(rootKeys(rows), [45, 2].map(n => REPO + '/pr#' + n));
});

test('off-page owner remains truthful in compact header/help with real waiting ordinal', () => {
  const { ctx } = load(), f = fixture();
  const rows = render(ctx, f, [f.second], [f.owner, f.second]);
  assert.match(rows[0].html, /1 of 3 PRs here · front: #45/);
  assert.match(rows[0].html, /#45 · Preparing · another page or collapsed repo/);
  assert.match(rows[1].html, /Waiting 2nd for preparation/);
  assert.equal(rootKeys(rows).length, 1);
});

test('release/handoff clears former owner decoration and pill suppression; readmission preserves durable FIFO', () => {
  const { ctx } = load(), f = fixture();
  render(ctx, f);
  assert.equal(ctx.stageTrackerPreparationChipHtml(f.first), '');
  f.q.owner = { ...f.first.preparation, role: 'owner' };
  f.first.preparation = f.q.owner;
  f.q.waiting = [f.second.preparation];
  f.second.preparation.behind = f.first.number;
  const rows = render(ctx, f, [f.first, f.second, f.owner]);
  assert.match(rows[0].html, /front: #9/);
  assert.doesNotMatch(rootRow(rows, 45).html, /ham-preparation-member/);
  assert.equal(ctx.stageTrackerPreparationRepresented(f.owner), false);
  f.q.waiting.push({ ...f.owner.preparation, role: 'waiting', behind: 9 });
  assert.deepEqual(plain(model(ctx, f)[0].waiting.map(e => e.ticket.number)), [2, 45]);
});

test('unverified owner gets a hollow node and concise current commentary, never old-head review proof or redundant badge', () => {
  const { ctx } = load(), f = fixture();
  f.owner.head = 'refreshed';
  f.q.owner.state = 'unverified'; f.q.owner.current_head = f.owner.head;
  f.q.owner.detail = 'Head changed; current preparation unverified';
  f.q.owner.pipeline = 'OLD HEAD APPROVED';
  const rows = render(ctx, f, [f.owner, f.first, f.second]);
  assert.match(rows[0].html, /2 confirmed PRs · recorded front: #45/);
  assert.match(rows[1].html, /ham-preparation-unverified/);
  assert.doesNotMatch(rows[1].html, /ham-train-front|OLD HEAD APPROVED|<span class="ham-badge"/);
  assert.match(rows[1].html, /Head changed; current preparation unverified/);
  assert.match(rows[1].html, /Review swarm requested; waiting for a verdict on the current head/);
  assert.equal(ctx.stageTrackerPreparationChipHtml(f.owner), '');
});

test('unknown predecessor and exact repo/base/head mismatches never fabricate confirmed positions', () => {
  for (const change of [
    f => { f.first.head = 'new-head'; }, f => { f.first.base_ref = 'other'; },
    f => { f.first.repo = 'someone/else'; }, f => { f.first.preparation.current_head = 'other'; },
    f => { f.data.items = [f.owner, f.second]; },
  ]) {
    const { ctx } = load(), f = fixture(); change(f);
    const group = model(ctx, f)[0];
    assert.ok(group.unverified.some(e => e.ticket.number === 9));
    assert.equal(group.waiting[0].position, 0);
    assert.match(content(render(ctx, f)), /Waiting for preparation · position unverified/);
  }
});

test('inactive, closed, held and draft candidates do not become active rail members', () => {
  for (const change of [
    f => { f.q.waiting[0].state = 'inactive'; },
    f => { f.first.state = 'closed'; }, f => { f.first.hold = { reason: 'human' }; },
    f => { f.first.modifiers = ['draft']; },
  ]) {
    const { ctx } = load(), f = fixture(); change(f);
    const group = model(ctx, f)[0];
    assert.equal(group.inactive, 1);
    assert.deepEqual(plain(group.waiting.map(e => [e.ticket.number, e.position])), [[2, 1]]);
    const rows = render(ctx, f);
    assert.doesNotMatch(rootRow(rows, 9).html, /ham-preparation-member/);
  }
});

test('multiple/shared issue trees keep their actual nested rows and PR stages without duplication or reparenting', () => {
  const { ctx } = load(), f = fixture();
  const issue = { kind: 'issue', repo: REPO, number: 500, stage: 40, title: 'linked issue',
    linked_prs: [{ ...f.first, drawn_under: 500 }, { ...f.second, drawn_under: 500 }] };
  const other = { ...issue, number: 501, linked_prs: issue.linked_prs.map(lp => ({ ...lp })) };
  f.data.items = [issue, other, f.owner];
  const before = plain(f.data), rows = render(ctx, f), html = content(rows);
  assert.deepEqual(rootKeys(rows), [REPO + '/issue#500', REPO + '/issue#501', REPO + '/pr#45']);
  assert.equal((html.match(/data-ham-sub-key="/g) || []).length, 2);
  assert.match(html, /data-ham-stage="50" data-ham-sub-key="hivecommons\/hive\/pr#9"/);
  assert.match(html, /data-ham-stage="70" data-ham-sub-key="hivecommons\/hive\/pr#2"/);
  assert.match(html, /shown under issue #500/);
  assert.deepEqual(plain(f.data), before);
  assert.match(rootRow(rows, 500, 'issue').html, /ham-preparation-gap/);
});

test('mixed linked trees mark only preparation children and stop before an unrelated child', () => {
  const { ctx } = load(), f = fixture();
  const unrelated = { ...f.first, number: 80, stage: 60, preparation: null, head: 'other' };
  const issue = { kind: 'issue', repo: REPO, number: 500, stage: 40, linked_prs: [f.first, unrelated, f.second] };
  f.data.items = [issue, f.owner];
  const html = content(render(ctx, f));
  assert.equal((html.match(/data-ham-sub-key="/g) || []).length, 3);
  assert.match(html, /<div class="ham-row" tabindex="0" data-ham-kind="issue"/);
  assert.match(html, /class="ham-sub ham-train ham-train-k3 ham-preparation-member ham-train-last" data-ham-kind="pr" data-ham-stage="70"/);
  assert.match(html, /class="ham-sub" data-ham-kind="pr" data-ham-stage="60"/);
  assert.equal((html.match(/ham-preparation-node/g) || []).length, 3);
});

test('single covering PR retains the original issue stage and PR-driven strip; its queue note names the actual issue', () => {
  const { ctx } = load(), f = fixture();
  const issue = { kind: 'issue', repo: REPO, number: 500, stage: 40, linked_prs: [f.first] };
  f.data.items = [issue, f.owner, f.second];
  const html = rootRow(render(ctx, f), 500, 'issue').html;
  assert.match(html, /data-ham-kind="issue".*data-ham-stage="40"/);
  assert.match(html, /data-stage="50"/);
  assert.match(html, /shown under issue #500/);
  assert.doesNotMatch(html, /data-ham-sub-key=|<span class="ham-badge"/);
});

test('still-rendered closed children prevent a parent rail through unrelated work', () => {
  const { ctx } = load(), f = fixture();
  const closed = { ...f.first, number: 80, stage: 75, state: 'closed', head: 'closed', preparation: null, superseded_by: f.first.number };
  const issue = { kind: 'issue', repo: REPO, number: 500, stage: 40, linked_prs: [f.first, closed] };
  f.data.items = [issue, f.owner, f.second];
  const html = rootRow(render(ctx, f), 500, 'issue').html;
  assert.match(html, /<div class="ham-row" tabindex="0" data-ham-kind="issue"/);
  assert.match(html, /class="ham-sub ham-sub-superseded" data-ham-kind="pr" data-ham-stage="75"/);
  assert.match(html, /superseded · closed · hive adopted → #9/);
  assert.match(html, /class="ham-sub ham-train ham-train-k3 ham-preparation-member ham-train-last"/);
  assert.equal((html.match(/ham-preparation-node/g) || []).length, 1);
});

test('real ready/native train attributes and rails remain untouched rather than claiming preparation seats', () => {
  const { ctx } = load(), f = fixture();
  f.owner.merge_train = { id: 'real-ready', position: 2 };
  const before = plain(f.data), rows = render(ctx, f), owner = rootRow(rows, 45);
  assert.match(owner.html, /class="ham-row ham-train ham-train-k0"/);
  assert.match(owner.html, /data-ham-train="real-ready"/);
  assert.doesNotMatch(owner.html, /ham-train-k3|ham-preparation-member/);
  assert.match(owner.html, /preparing base/);
  assert.deepEqual(plain(f.data), before);
  const filter = source('tracker-filter-render');
  assert.match(filter, /stageTrackerNativeQueueSync/);
  assert.match(filter, /stageTrackerTrainContiguous\(stageTrackerSort/);
});

test('comments dedupe current captions, retain current event detail, and use no synthetic event timestamp', () => {
  const { ctx } = load(), f = fixture(), key = ctx.stageTrackerItemKey(f.first);
  const events = { [key]: { text: f.first.caption, who: 'hive', at: NOW - 1000 } }, before = plain(events);
  const html = render(ctx, f, [f.first], f.data.items, events)[1].html;
  const comment = html.match(/<div class="cue-event ham-preparation-comment">.*?<\/div>/)[0];
  assert.doesNotMatch(comment, /CI · checks pending/);
  assert.equal((html.match(/CI · checks pending<\/p>/g) || []).length, 1);
  assert.match(comment, /ticket age 30m/);
  assert.doesNotMatch(comment, /data-at=|data-cue-key=/);
  assert.deepEqual(events, before);
  f.first.caption = 'CI · failing current check';
  assert.match(content(render(ctx, f, [f.first])), /CI · failing current check · queued for base refresh/);
  events[key].text = 'You approved';
  const actionHtml = content(render(ctx, f, [f.first], f.data.items, events));
  assert.match(actionHtml, /<p class="cue-event">You approved<\/p>/);
  assert.match(actionHtml, /CI · failing current check · queued for base refresh/);
});

test('policy omission adds no preparation; read errors and missing snapshots explicitly say unavailable, not zero', () => {
  const { ctx } = load(), f = fixture();
  render(ctx, f);
  f.data.base_preparation = [];
  assert.doesNotMatch(content(render(ctx, f)), /ham-preparation-head|ham-preparation-member/);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation/);
  delete f.data.base_preparation;
  assert.deepEqual(plain(model(ctx, f)), []);
  f.data.base_preparation = [{ repo: REPO, base_ref: 'v5', state: 'unavailable', message: 'durable snapshot read failed' }];
  const html = content(render(ctx, f));
  assert.match(html, /Preparation train · unavailable/);
  assert.match(html, /durable snapshot read failed/);
  assert.doesNotMatch(html, /0 PRs/);
  ctx.stageTrackerPreparationVisualSync(null, [], [], [], NOW);
  assert.match(content(plain(ctx.stageTrackerPreparationMissingRows())), /Waiting for a current tracker snapshot/);
});

test('suppression starts only on actual rendered context, clears on handoff, and rejects changed heads', () => {
  const { ctx } = load(), f = fixture();
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, [f.first], [], NOW);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation/);
  ctx.stageTrackerRowHtml(f.first, [], { nowMs: NOW });
  assert.equal(ctx.stageTrackerPreparationChipHtml(f.first), '');
  assert.match(ctx.stageTrackerPreparationChipHtml(f.second), /Waiting for preparation/);
  f.first.head = 'changed-after-render';
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation/);
});

test('old CSS/token geometry and accessible styled help are reused; no external prep region, polling or animation', () => {
  const { ctx } = load(), f = fixture();
  f.q.owner.detail = '<script>bad()</script>';
  f.owner.head = 'changed'; f.q.owner.state = 'unverified';
  const html = content(render(ctx, f));
  assert.match(html, /tabindex="0" data-stop="1" aria-label=/);
  assert.match(html, /class="config-tooltip".*role="tooltip"/);
  assert.match(html, /&lt;script&gt;bad\(\)&lt;\/script&gt;/);
  assert.doesNotMatch(html, /<script>/);
  assert.match(ctx.stageTrackerTrainIconHtml(), /aria-label="Merge queue"/);
  assert.doesNotMatch(source('tracker-preparation'), /setInterval|setTimeout|fetch\(|\.merge_train\s*=/);
  const css = fs.readFileSync(path.join(__dirname, 'static-src/css/tracker.css'), 'utf8');
  assert.match(css, /\.ham-train-chip \{.*padding: 0 0\.6em;.*border-radius: var\(--r-pill\); line-height: 1\.6/);
  assert.match(css, /\.ham-row\.ham-train::before \{.*left: calc\(var\(--sp-2\) \+ 2px\)/);
  assert.match(css, /\.ham-preparation-comment \{ animation: none; \}/);
  const template = fs.readFileSync(path.join(__dirname, 'static-src/html/tracker.html'), 'utf8');
  assert.doesNotMatch(template, /id="activity-monitor-preparation"/);
  assert.match(template, /id="activity-monitor-native-queue"/);
});

test('gold summary stays ready-only without the removed external region', () => {
  const { ctx, elements } = load(), f = fixture();
  const count = { textContent: '' };
  const chip = { hidden: false, title: '', querySelector: () => count, classList: { toggle() {} }, setAttribute() {} };
  elements.set('activity-monitor-queue', chip);
  vm.runInContext(`
    var _stageTrackerQueueItems = [], _stageTrackerPreparationQueues = [];
    function stageTrackerTrain(it) { return it && it.merge_train || null; }
    function stageTrackerQueuePopDraw() {}
    function hamQueueToggle() {}
  `, ctx);
  for (const name of ['stageTrackerQueueTrains', 'stageTrackerQueueChip', 'stageTrackerQueueSync']) vm.runInContext(fn('tracker-queue', name), ctx);
  ctx.stageTrackerQueueSync(f.data.items, NOW, [f.q]);
  assert.equal(chip.hidden, true);
  assert.deepEqual(plain(ctx._stageTrackerPreparationQueues), []);
  f.owner.merge_train = { id: 'real-ready', front: 45, position: 1 };
  ctx.stageTrackerQueueSync(f.data.items, NOW, [f.q]);
  assert.equal(chip.hidden, false);
  assert.equal(count.textContent, '1');
  assert.doesNotMatch(chip.title, /preparation|owner|waiting/);
});
