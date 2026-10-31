// Execute the shipped preparation projection/rendering helpers in CI. The
// pipeline owns filtering/sorting/paging; the rail only references its result.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = name => fs.readFileSync(path.join(__dirname, 'static-src/js', name + '.js'), 'utf8');
const fn = (file, name) => {
  const found = source(file).match(new RegExp('function ' + name + '\\([^\\n]*\\) \\{[\\s\\S]*?\\n    \\}'));
  assert.ok(found, name);
  return found[0];
};
const NOW = Date.parse('2026-10-31T01:00:00Z');
const REPO = 'hivecommons/hive';
const esc = value => String(value).replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
const plain = value => JSON.parse(JSON.stringify(value));

function load() {
  const region = { hidden: true, innerHTML: '' };
  const elements = new Map();
  let present = true;
  const ctx = vm.createContext({ console, Date, esc,
    document: { getElementById: id => id === 'activity-monitor-preparation' ? present ? region : null : elements.get(id) || null },
    stageTrackerAnchor: stage => Number(stage) || 0,
    stageTrackerAttentionTier: () => 1,
    stageTrackerNormalizeFilter: f => f,
    STAGE_TRACKER_TERMINAL: 100,
  });
  const core = source('tracker-words-core');
  for (const name of ['STAGE_TRACKER_TRAIN_ICON_PATH', 'STAGE_TRACKER_TRAIN_ICON_TITLE', 'STAGE_TRACKER_TRAIN_ACCENTS']) {
    const found = core.match(new RegExp('const ' + name + ' = [^\\n]+'));
    assert.ok(found, name);
    vm.runInContext(found[0], ctx);
  }
  for (const name of ['stageTrackerTrainIconHtml', 'stageTrackerTrainAccentClass', 'stageTrackerOrdinal']) vm.runInContext(fn('tracker-words-core', name), ctx);
  for (const name of ['stageTrackerLinkedSuperseder', 'stageTrackerLinkedSupersededGone']) vm.runInContext(fn('tracker-words-gate', name), ctx);
  for (const name of ['stageTrackerLinkedPRItem', 'stageTrackerLinkedPROpen', 'stageTrackerLinkedDrawnHere', 'stageTrackerLinkedSorted', 'stageTrackerAdoptOrphanPRs']) vm.runInContext(fn('tracker-row-layout', name), ctx);
  for (const name of ['stageTrackerPreparationWord', 'stageTrackerPreparationAge', 'stageTrackerPreparationChipHtml']) vm.runInContext(fn('tracker-queue', name), ctx);
  for (const name of ['stageTrackerSort', 'stageTrackerPage']) vm.runInContext(fn('tracker-filter', name), ctx);
  vm.runInContext(source('tracker-preparation'), ctx);
  return { ctx, region, elements, removeRegion() { present = false; } };
}
function fixture() {
  const pr = (number, stage, caption) => ({ kind: 'pr', repo: REPO, number, stage, caption, state: 'open',
    head: 'head-' + number, base_ref: 'v5', title: 'PR ' + number, url: 'https://github.com/' + REPO + '/pull/' + number,
    created_at: '2026-10-30T00:00:00Z' });
  const owner = pr(45, 70, 'Review · no free reviewer');
  const first = pr(9, 50, 'CI · checks pending');
  const second = pr(2, 70, 'Review · exact-head bundle pending');
  const ticket = (it, role) => ({ role, number: it.number, state: 'current', head_sha: it.head,
    current_head: it.head, since: '2026-10-31T00:30:00Z', pipeline: it.caption,
    detail: 'Ticket matches the current open PR snapshot', ...(role === 'waiting' ? { behind: owner.number } : {}) });
  const q = { repo: REPO, base_ref: 'v5', state: 'observed', owner: ticket(owner, 'owner'), waiting: [ticket(first, 'waiting'), ticket(second, 'waiting')] };
  owner.preparation = q.owner;
  first.preparation = q.waiting[0];
  second.preparation = q.waiting[1];
  return { owner, first, second, q, data: { items: [second, first, owner], base_preparation: [q] } };
}
function model(ctx, f, listed = f.data.items, page = listed) {
  return ctx.stageTrackerPreparationVisualModel(f.data, listed, page, []);
}

test('owner first and persisted FIFO, not PR number/age, with original stages and train facts unchanged', () => {
  const { ctx } = load(), f = fixture();
  f.owner.merge_train = { id: 'real-ready-train', position: 2 };
  f.first.created_at = '2020-01-01T00:00:00Z';
  const before = plain(f.data);
  const groups = model(ctx, f), html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.equal(groups[0].owner.ticket.number, 45);
  assert.deepEqual(plain(groups[0].waiting.map(e => [e.ticket.number, e.position])), [[9, 1], [2, 2]]);
  assert.ok(html.indexOf('Preparing</span>') < html.indexOf('Waiting 1st for preparation'));
  assert.ok(html.indexOf('Waiting 1st for preparation') < html.indexOf('Waiting 2nd for preparation'));
  assert.match(html, /Review · no free reviewer/);
  assert.match(html, /CI · checks pending/);
  assert.match(html, /ticket age 30m/);
  assert.match(html, /not guaranteed merge order/);
  assert.deepEqual(plain(f.data), before);
  assert.doesNotMatch(html, /data-ham-train=|data-ham-key=|ham-row|aria-label="Merge queue"|Ready for merge|Merged!/);
});

test('release/handoff clears former owner and recomputes waiting positions from new snapshot', () => {
  const { ctx, region } = load(), f = fixture();
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, f.data.items, [], NOW);
  assert.equal(ctx.stageTrackerPreparationChipHtml(f.first), '');
  f.q.owner = { ...f.first.preparation, role: 'owner' };
  f.first.preparation = f.q.owner;
  f.q.waiting = [f.second.preparation];
  f.second.preparation.behind = f.first.number;
  f.data.items = [f.first, f.second, f.owner];
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, f.data.items, [], NOW);
  assert.match(region.innerHTML, />#9 PR 9<\/a>/);
  assert.doesNotMatch(region.innerHTML, /Owner #45|Waiting 2nd/);
  assert.equal(ctx.stageTrackerPreparationRepresented(f.owner), false);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.owner), /Preparing base/);
  assert.equal(f.first.stage, 50);
  assert.equal(f.second.stage, 70);
});

test('inactive merged/held/expired tickets are excluded and readmission uses new FIFO order', () => {
  const { ctx } = load(), f = fixture();
  f.q.waiting.unshift(...['merged', 'held', 'expired'].map((detail, i) => ({ number: 100 + i, role: 'waiting', state: 'inactive', detail })));
  let groups = model(ctx, f);
  assert.equal(groups[0].inactive, 3);
  assert.deepEqual(plain(groups[0].waiting.map(e => e.position)), [1, 2]);
  const html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.match(html, /3 inactive tickets excluded/);
  assert.doesNotMatch(html, /#100|#101|#102/);
  f.q.waiting = [f.second.preparation, f.first.preparation];
  groups = model(ctx, f);
  assert.deepEqual(plain(groups[0].waiting.map(e => [e.ticket.number, e.position])), [[2, 1], [9, 2]]);
});

test('unverified refreshed owner stays ahead as a warning without old-head pipeline proof', () => {
  const { ctx } = load(), f = fixture();
  f.owner.head = 'own-refreshed-head';
  f.q.owner.state = 'unverified';
  f.q.owner.current_head = f.owner.head;
  f.q.owner.detail = 'Head changed; current preparation unverified';
  f.q.owner.pipeline = 'old-head APPROVED proof';
  const groups = model(ctx, f), html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.equal(groups[0].owner, null);
  assert.equal(groups[0].unverified[0].ticket.number, 45);
  assert.ok(html.indexOf('Unverified recorded owner #45') < html.indexOf('Waiting 1st'));
  assert.doesNotMatch(html, /old-head APPROVED proof/);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.owner), /unverified/);
});

test('unknown predecessor prevents fabricated exact ranks for subsequent confirmed tickets', () => {
  const { ctx } = load(), f = fixture();
  f.q.waiting.unshift({ number: 77, role: 'waiting', state: 'unverified', head_sha: 'missing', detail: 'Not in current tracker snapshot' });
  const groups = model(ctx, f), html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.deepEqual(plain(groups[0].waiting.map(e => e.position)), [0, 0]);
  assert.match(html, /position unverified/);
  assert.match(html, /Unverified recorded ticket #77/);
  assert.doesNotMatch(html, /Waiting 1st|Waiting 2nd/);
});

test('exact repo/base/head join rejects stale or missing identities instead of borrowing proofs', () => {
  for (const change of [
    f => { f.first.head = 'new-head'; },
    f => { f.first.base_ref = 'different-base'; },
    f => { f.first.repo = 'someone/else'; },
    f => { f.first.preparation.current_head = 'different-current-head'; },
    f => { f.data.items = [f.owner, f.second]; },
  ]) {
    const { ctx } = load(), f = fixture();
    change(f);
    const groups = model(ctx, f), html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
    assert.ok(groups[0].unverified.some(e => e.ticket.number === f.first.number));
    assert.equal(groups[0].waiting[0].position, 0);
    assert.doesNotMatch(html, /CI · checks pending/);
  }
});

test('filters and pagination preserve FIFO ordinals but never draw absent members as visible seats', () => {
  const { ctx } = load(), f = fixture();
  const groups = model(ctx, f, [f.owner, f.second], [f.second]);
  const html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.equal(groups[0].owner.location, 'offpage');
  assert.deepEqual(plain(groups[0].waiting.map(e => [e.location, e.position])), [['filtered', 1], ['page', 2]]);
  assert.match(html, /1 of 3 confirmed tickets shown/);
  assert.match(html, /On another page or in a collapsed repo: Preparing #45/);
  assert.match(html, /Not shown by the current filters: Waiting 1st for preparation #9/);
  assert.equal((html.match(/<li /g) || []).length, 1);
  assert.match(html, /Waiting 2nd for preparation<\/span>/);
  assert.match(html, /Owner #45/);
});

test('actual number/custom sorting and page slice remain unchanged by the rail', () => {
  const { ctx } = load(), f = fixture();
  const ordered = ctx.stageTrackerSort(f.data.items, [], { sort: 'number', dir: 'desc' });
  const page = ctx.stageTrackerPage(ordered, { size: 25, page: 1 });
  const before = plain(page);
  const groups = model(ctx, f, ordered, page.rows);
  ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.deepEqual(plain(page), before);
  assert.deepEqual(plain(page.rows.map(it => it.number)), [45, 9, 2]);
  assert.deepEqual(plain(groups[0].waiting.map(e => e.ticket.number)), [9, 2]);
  assert.deepEqual(plain(f.data.items.map(it => it.stage)), [70, 50, 70]);
});

test('linked multiple PRs and shared issue trees are referenced once without reparenting rows', () => {
  const { ctx } = load(), f = fixture();
  const issue = { kind: 'issue', repo: REPO, number: 500, stage: 40,
    linked_prs: [{ ...f.first, drawn_under: 500 }, { ...f.second, drawn_under: 500 }] };
  const other = { ...issue, number: 501, linked_prs: issue.linked_prs.map(lp => ({ ...lp })) };
  f.data.items = [issue, other, f.owner];
  const before = plain(f.data);
  const groups = model(ctx, f), html = ctx.stageTrackerPreparationVisualHtml(groups, NOW);
  assert.equal(groups[0].waiting.length, 2);
  assert.deepEqual(plain(groups[0].waiting.map(e => e.row.host.number)), [500, 500]);
  assert.equal((html.match(/<li /g) || []).length, 3);
  assert.match(html, /shown under issue #500/);
  assert.deepEqual(plain(f.data), before);
  const adopted = model(ctx, f, [other, f.owner], [{ item: other }, { group: { key: 'repo:other' } }]);
  assert.equal(adopted[0].waiting[0].row.host.number, 501);
  assert.equal(adopted[0].waiting[1].row.host.number, 501);
  assert.deepEqual(plain(f.data), before);
});

test('current-page linked head mismatch does not create a visible seat or suppress a warning', () => {
  const { ctx } = load(), f = fixture();
  const stale = { kind: 'issue', repo: REPO, number: 500, linked_prs: [{ ...f.first, head: 'old-linked-head' }] };
  f.data.items.push(stale);
  const groups = model(ctx, f, [stale], [stale]);
  assert.ok(groups[0].unverified.some(e => e.ticket.number === f.first.number));
  assert.equal(groups[0].waiting.some(e => e.ticket.number === f.first.number), false);
});

test('only represented exact current members lose redundant pills; ungrouped/unknown fallback stays', () => {
  const { ctx, region, removeRegion } = load(), f = fixture();
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation behind #45/);
  ctx.stageTrackerPreparationVisualSync(f.data, [f.first], [f.first], [], NOW);
  assert.equal(ctx.stageTrackerPreparationChipHtml(f.first), '');
  assert.match(ctx.stageTrackerPreparationChipHtml(f.second), /Waiting for preparation behind #45/);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.owner), /Preparing base/);
  assert.equal(f.first.stage, 50);
  assert.match(region.innerHTML, /Waiting 1st for preparation/);
  f.first.preparation.state = 'unverified';
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /unverified/);
  f.first.preparation.state = 'current';
  f.first.head = 'changed-after-render';
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation/);
  f.first.head = f.first.preparation.head_sha;
  removeRegion();
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, f.data.items, [], NOW);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation behind #45/);
});

test('single covering PR suppresses only redundant preparation annotation, not its real stage or signals', () => {
  const { ctx } = load(), f = fixture();
  const issue = { kind: 'issue', repo: REPO, number: 500, linked_prs: [{ ...f.first }] };
  f.data.items = [issue, f.owner, f.second];
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, f.data.items, [], NOW);
  const shown = { ...issue, via_pr: true, preparation: f.first.preparation, head: f.first.head, stage: f.first.stage, hold: { reason: 'needs human' }, caption: f.first.caption };
  assert.equal(ctx.stageTrackerPreparationChipHtml(shown), '');
  assert.equal(shown.stage, 50);
  assert.deepEqual(shown.hold, { reason: 'needs human' });
  assert.equal(shown.caption, 'CI · checks pending');
  shown.preparation = { ...shown.preparation, state: 'unverified' };
  assert.match(ctx.stageTrackerPreparationChipHtml(shown), /unverified/);
});

test('inactive policy hides the rail; unavailable/loading state is explicit and clears pill suppression', () => {
  const { ctx, region } = load(), f = fixture();
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, f.data.items, [], NOW);
  f.data.base_preparation = [];
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, f.data.items, [], NOW);
  assert.equal(region.hidden, true);
  assert.match(ctx.stageTrackerPreparationChipHtml(f.first), /Waiting for preparation/);
  delete f.data.base_preparation;
  assert.deepEqual(plain(model(ctx, f)), []);
  f.data.base_preparation = [{ repo: REPO, base_ref: 'v5', state: 'unavailable', message: 'durable snapshot read failed' }];
  ctx.stageTrackerPreparationVisualSync(f.data, f.data.items, [], [], NOW);
  assert.equal(region.hidden, false);
  assert.match(region.innerHTML, /durable snapshot read failed/);
  ctx.stageTrackerPreparationVisualSync(null, [], [], [], NOW);
  assert.match(region.innerHTML, /Preparation queue unavailable/);
  assert.doesNotMatch(region.innerHTML, /0 confirmed|<li /);
});

test('accessible static train language escapes data and preserves native queue icon semantics', () => {
  const { ctx } = load(), f = fixture();
  f.first.title = '<img onerror="bad"> & title';
  f.q.waiting[0].pipeline = '<script>old()</script>';
  const html = ctx.stageTrackerPreparationVisualHtml(model(ctx, f), NOW);
  assert.match(html, /aria-label="Preparation queue"/);
  assert.match(html, /aria-label="Confirmed preparation order on this page"/);
  assert.match(html, /&lt;img onerror=&quot;bad&quot;&gt;/);
  assert.doesNotMatch(html, /<script>|<img /);
  assert.match(ctx.stageTrackerTrainIconHtml(), /aria-label="Merge queue"/);
  const shipped = source('tracker-preparation');
  assert.doesNotMatch(shipped, /setInterval|setTimeout|fetch\(|alert\(|confirm\(|\.merge_train\s*=/);
  const css = fs.readFileSync(path.join(__dirname, 'static-src/css/tracker.css'), 'utf8');
  for (const rule of css.match(/\.ham-preparation[^{]*\{[^}]*\}/g) || []) assert.doesNotMatch(rule, /animation:|transition:/);
});

test('visible preparation rail removes redundant gold summary/popover but retains genuine ready trains', () => {
  const { ctx, elements, removeRegion } = load(), f = fixture();
  const count = { textContent: '' }, attrs = {};
  const chip = { hidden: false, title: '', querySelector: () => count, classList: { toggle() {} },
    setAttribute: (key, value) => { attrs[key] = value; } };
  elements.set('activity-monitor-queue', chip);
  vm.runInContext(`
    var _stageTrackerQueueItems = [], _stageTrackerPreparationQueues = [], closedPopover = false;
    function stageTrackerTrain(it) { return it && it.merge_train || null; }
    function stageTrackerQueuePopDraw() {}
    function hamQueueToggle(open) { closedPopover = !open; }
  `, ctx);
  for (const name of ['stageTrackerQueueTrains', 'stageTrackerQueueChip', 'stageTrackerQueueSync']) vm.runInContext(fn('tracker-queue', name), ctx);
  ctx.stageTrackerQueueSync(f.data.items, NOW, [f.q]);
  assert.equal(chip.hidden, true);
  assert.equal(ctx.closedPopover, true);
  assert.equal(count.textContent, '0');
  assert.deepEqual(plain(ctx._stageTrackerPreparationQueues), []);
  f.owner.merge_train = { id: 'real-ready', front: 45, position: 1 };
  ctx.stageTrackerQueueSync(f.data.items, NOW, [f.q]);
  assert.equal(chip.hidden, false);
  assert.equal(count.textContent, '1');
  assert.match(chip.title, /Merge queue: 1 PRs in 1 train/);
  assert.doesNotMatch(chip.title, /preparation|owner|waiting/);
  assert.deepEqual(plain(ctx._stageTrackerPreparationQueues), []);
  delete f.owner.merge_train;
  removeRegion();
  ctx.stageTrackerQueueSync(f.data.items, NOW, [f.q]);
  assert.equal(chip.hidden, false);
  assert.match(count.textContent, /0 train PRs · 1 owner · 2 waiting/);
  assert.equal(ctx._stageTrackerPreparationQueues.length, 1);
});
