// CI-only behavioral tests of the shipped human context and actual row adapters.
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
const plain = value => JSON.parse(JSON.stringify(value));
function load() {
  const c = vm.createContext({ Date, esc,
    STAGE_TRACKER_TERMINAL: 100, STAGE_TRACKER_PR_COLUMN: 40,
    STAGE_TRACKER_LINKED_GLYPH: '↳', STAGE_TRACKER_SOFT_LINK_TIP: 'refs',
    stageTrackerAnchor: n => Number(n) || 0,
    stageTrackerAttentionTier: () => 1,
    stageTrackerLeadStage: lp => lp.stage || 40,
    stageTrackerIcon: (kind, cls) => `<svg class="${cls || ''}" data-kind="${kind}"></svg>`,
    stageTrackerPRRef: (n, url, kind, author, title) => `<a href="${esc(url || '#')}">#${n} ${esc(title || '')}</a>`,
    stageTrackerPRPillsHtml: ref => ref,
    stageTrackerTitleHtml: text => `<span>${esc(text)}</span>`,
    stageTrackerTooltip: it => it.caption || '',
    stageTrackerLinkedTip: () => '',
    stageTrackerActedBadgeHtml: () => '', stageTrackerRowToolsHtml: () => '',
    stageTrackerChipsHtml: () => '', stageTrackerMoveAttrs: () => '', stageTrackerSinceHtml: () => '',
    stageTrackerStripItems: it => `<li data-stage="${it.stage}"></li>`,
    stageTrackerCurrentStageHtml: it => `<span class="ham-current-stage">${it.stage}</span>`,
    stageTrackerEventHtml: (key, ctx) => `<p class="cue-event cue-event-tuckable" data-cue-key="${esc(key)}">${esc(ctx && ctx.note || 'live note')}<button>Reopen</button></p>`,
    stageTrackerActionCtx: () => ({ user: 'viewer' }),
    stageTrackerLeadActionOnRow: item => item.optionalAction ? { id: 'decide:delegate', label: 'Let hive decide' } : null,
  });
  for (const name of ['stageTrackerLinkedSuperseder', 'stageTrackerLinkedSupersededGone', 'stageTrackerLinkedSupersededText', 'stageTrackerLinkedSupersededTip']) vm.runInContext(fn('tracker-words-gate', name), c);
  for (const name of ['stageTrackerItemKey', 'stageTrackerPRKey', 'stageTrackerRowPRKeys']) vm.runInContext(fn('tracker-merged', name), c);
  for (const name of ['stageTrackerLinkedPRItem', 'stageTrackerLinkedPROpen', 'stageTrackerLinkedDrawnHere', 'stageTrackerLinkedSorted', 'stageTrackerHeadColsHtml', 'stageTrackerRowHtml', 'stageTrackerSubRowHtml']) vm.runInContext(fn('tracker-row-layout', name), c);
  vm.runInContext(fn('tracker-filter', 'stageTrackerPRIssueSlot'), c);
  vm.runInContext(source('tracker-human-attention'), c);
  return c;
}
function pr(number = 15985) {
  return { kind: 'pr', repo: 'hivecommons/hive', number, stage: 85, state: 'open', head: 'current-' + number,
    title: 'PR ' + number, url: 'https://github.com/hivecommons/hive/pull/' + number, base_ref: 'v5',
    human_attention: { kind: 'pr', number, head: 'current-' + number, reason: 'Confirm the compatibility policy',
      asked_by: 'a reviewer', owner: 'maintainer' },
    stage_actor: { kind: 'human', login: 'maintainer', why: 'Confirm the compatibility policy' } };
}
function issue(linked) {
  return { kind: 'issue', repo: 'hivecommons/hive', number: 15937, stage: 40, linked_pr: linked[0].number,
    title: 'Matrix overlay', linked_prs: linked, url: 'https://github.com/hivecommons/hive/issues/15937',
    stage_actor: linked[0].stage_actor };
}

test('covered issue shows the exact PR requirement/requester in visible prose without overriding its actual stage', () => {
  const c = load(), child = pr(), item = issue([child]), before = plain(item);
  const html = c.stageTrackerRowHtml(item, [], { nowMs: 1, note: 'review comment' });
  assert.match(html, /data-ham-kind="issue".*data-ham-stage="40"/);
  assert.match(html, /data-stage="85"/);
  assert.match(html, /PR #15985 — Needs human: Confirm the compatibility policy · next move: @maintainer · requested by a reviewer/);
  assert.match(html, />View PR #15985<\/a>/);
  assert.match(html, /cue-event-tuckable.*data-cue-key=.*review comment.*Reopen/);
  assert.doesNotMatch(html, /Needs you|ham-preparation-head|<section/);
  assert.deepEqual(plain(item), before);
});

test('external RFC hosting is attributed to the actual PR and does not become an issue-wide hold', () => {
  const c = load(), child = { ...pr(), drawn_under: 15862 }, item = issue([child]);
  const html = c.stageTrackerRowHtml(item, [], { nowMs: 1 });
  assert.match(html, /PR #15985 — Needs human:.*shown under issue #15862/);
  assert.doesNotMatch(html, /data-ham-sub-key=/);
  assert.equal(item.stage, 40);
  assert.equal(item.human_attention, undefined);
});

test('mixed multiple PR trees render only the asking child and preserve child order/stages', () => {
  const c = load(), asking = pr(), ordinary = { ...pr(15998), stage: 50, human_attention: null, stage_actor: { kind: 'hive', why: 'CI pending' } };
  const item = issue([ordinary, asking]), before = plain(item);
  const html = c.stageTrackerRowHtml(item, [], { nowMs: 1 });
  assert.equal((html.match(/Needs human:/g) || []).length, 1);
  assert.match(html, /data-ham-stage="85" data-ham-sub-key="hivecommons\/hive\/pr#15985"/);
  assert.match(html, /data-ham-stage="50" data-ham-sub-key="hivecommons\/hive\/pr#15998"/);
  assert.ok(html.indexOf('data-ham-sub-key="hivecommons/hive/pr#15985"') < html.indexOf('Needs human:'));
  assert.ok(html.indexOf('Needs human:') < html.indexOf('data-ham-sub-key="hivecommons/hive/pr#15998"'));
  assert.deepEqual(plain(item), before);
});

test('optional Let hive decide is explicitly an optional action, never evidence of a human gate', () => {
  const c = load(), item = { ...pr(), human_attention: null, optionalAction: true, stage: 50, stage_actor: { kind: 'hive' } };
  const html = c.stageTrackerHumanContextHtml(item, [], 1);
  assert.match(html, /Optional delegation: Let hive decide is available/);
  assert.doesNotMatch(html, /Needs human:|Needs you/);
});

test('claim owner and human next mover are explained separately from a required decision', () => {
  const c = load(), item = { ...pr(), human_attention: null, stage: 40, stage_actor: { kind: 'none' }, claimant: { kind: 'person', login: 'author' } };
  assert.match(c.stageTrackerHumanContextHtml(item, [], 1), /Claimed by @author · claim ownership, not a human decision request/);
  item.stage_actor = { kind: 'human', login: 'author', why: '@author must push a fix; the hive does not edit a person’s PR' };
  const html = c.stageTrackerHumanContextHtml(item, [], 1);
  assert.match(html, /Next move: @author — @author must push a fix/);
  assert.doesNotMatch(html, /Needs human:|Needs you/);
});

test('changed head/identity, closed/draft/held/superseded and answered observations cannot retain a requirement', () => {
  for (const mutate of [
    it => { it.head = 'new-head'; }, it => { it.number++; }, it => { it.kind = 'issue'; },
    it => { it.state = 'closed'; }, it => { it.modifiers = ['draft']; }, it => { it.modifiers = ['hold']; },
    it => { it.hold = { by: 'owner' }; }, it => { it.superseded_by = 16000; },
    it => { it.human_attention = null; it.stage_actor = { kind: 'hive' }; },
  ]) {
    const c = load(), item = pr(); mutate(item);
    assert.equal(c.stageTrackerHumanAttentionOf(item), null);
    assert.doesNotMatch(c.stageTrackerHumanContextHtml(item, [], 1), /Needs human:/);
  }
});

test('unknown reason is explicit with a detail route; data is escaped and no owner is invented', () => {
  const c = load(), item = pr();
  item.human_attention.reason = ''; delete item.human_attention.owner; delete item.human_attention.asked_by;
  let html = c.stageTrackerHumanContextHtml(item, [], 1);
  assert.match(html, /Needs human: Human decision required; reason unavailable/);
  assert.match(html, />View PR #15985/);
  assert.doesNotMatch(html, /next move: @|requested by|Needs you/);
  item.human_attention.reason = '<script>alert(1)</script>';
  html = c.stageTrackerHumanContextHtml(item, [], 1);
  assert.match(html, /&lt;script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});

test('partial reference metadata and unrelated RFC requirements are not inherited by a child PR', () => {
  const c = load(), child = { ...pr(), human_attention: null, stage: 50, stage_actor: { kind: 'hive' } };
  child.linked_issues = [{ number: 15862, via: 'refs', state: 'open' }];
  const rfc = { ...issue([child]), number: 15862, stage: 45,
    human_attention: { kind: 'issue', number: 15862, reason: 'Approve future deployment' } };
  const before = plain(child);
  assert.match(c.stageTrackerHumanContextHtml(rfc, [], 1), /Needs human: Approve future deployment/);
  assert.doesNotMatch(c.stageTrackerHumanContextHtml(child, [], 1), /Needs human:|deployment/);
  assert.deepEqual(plain(child), before);
});

test('unverified head does not inherit current human proof from a matching number in another snapshot', () => {
  const c = load(), child = pr(), item = issue([child]);
  child.head = 'next-head';
  const stale = c.stageTrackerHumanContextHtml(item, [], 1);
  assert.doesNotMatch(stale, /Needs human:|Confirm the compatibility policy|Next move:/);
  assert.match(stale, /decision context unavailable for the displayed identity\/head/);
  child.human_attention = { ...child.human_attention, head: 'next-head', reason: 'New current-head question' };
  assert.match(c.stageTrackerHumanContextHtml(item, [], 1), /Needs human: New current-head question/);
  child.human_attention = null;
  assert.doesNotMatch(c.stageTrackerHumanContextHtml(item, [], 1), /Needs human:/);
});

test('strip uses the person glyph only for a current requirement; claimant and stage text remain intact', () => {
  const c = load(), item = pr();
  Object.assign(c, {
    STAGE_TRACKER_MOTION_ACTIVE: 'active', STAGE_TRACKER_MOTION_STATIC: 'static',
    stageTrackerSegments: () => [{ number: 40, state: 'active', label: 'PR in progress', short: 'PR in progress' }],
    stageTrackerActorKind: it => it.stage_actor.kind === 'none' ? '' : it.stage_actor.kind,
    stageTrackerActorTip: () => '', stageTrackerMotion: () => 'static',
    stageTrackerHumanReason: () => 'needs a person',
    stageTrackerClaimant: it => it.claimant || null,
    stageTrackerClaimantShort: it => it.login ? '@' + it.login : '',
    stageTrackerClaimantIconKind: it => it.kind === 'person' ? 'human' : it.kind,
    stageTrackerClaimantLine: it => it.login,
    STAGE_TRACKER_PRE_MERGE_SOAK: 95, STAGE_TRACKER_READY_FOR_HUMAN_MERGE: 90,
  });

  vm.runInContext(fn('tracker-row-strip', 'stageTrackerStripItems'), c);
  item.stage = 40;
  assert.match(c.stageTrackerStripItems(item, [], null, 1, {}), /data-kind="human"/);
  item.human_attention = null;
  item.claimant = { kind: 'person', login: 'author' };
  let html = c.stageTrackerStripItems(item, [], null, 1, {});
  assert.doesNotMatch(html, /data-kind="human"/);
  assert.doesNotMatch(html, /needs a person/);
  assert.match(html, /PR in progress.*@author/);
  item.stage_actor = { kind: 'none' };
  html = c.stageTrackerStripItems(item, [], null, 1, {});
  assert.doesNotMatch(html, /data-kind="human"/);
});

test('operator approval plus current changes-requested shows the real blocker on PR and covered issue, not readiness', () => {
  const c = load(), child = { ...pr(15977), stage: 72, author_kind: 'clanker', human_attention: null,
    stage_actor: { kind: 'clanker' }, decided: { effect: 'none', word: 'Review changes requested', head: 'current-15977',
      current_blocker: "Review changes requested on the current head · waiting on @thatguywob's contributor agent to address the review" } };
  const item = issue([child]);
  const html = c.stageTrackerRowHtml(item, [], { nowMs: 1 });
  assert.match(html, /data-stage="72"/);
  assert.match(html, /PR #15977 — Current blocker: Review changes requested on the current head/);
  assert.match(html, /operator approval recorded separately, not review approval or merge readiness/);
  assert.doesNotMatch(html, /Approved · merge queue|Needs human:|data-stage="80"/);
  for (const mutate of [
    it => { it.head = 'new-head'; }, it => { it.decided = null; }, it => { it.state = 'closed'; },
    it => { it.modifiers = ['draft']; }, it => { it.modifiers = ['hold']; }, it => { it.stage = 100; },
  ]) {
    const changed = plain(child); mutate(changed);
    assert.doesNotMatch(c.stageTrackerHumanContextHtml(changed, [], 1), /Current blocker: Review changes requested/);
  }
});

test('adoption absence is not a refusal; ownership policy and actual capability blocks have distinct words', () => {
  const c = load();
  Object.assign(c, { stageTrackerAdoptOffered: () => false });
  vm.runInContext(fn('tracker-words', 'stageTrackerAdoptionChipText'), c);
  assert.equal(c.stageTrackerAdoptionChipText(pr()), '');
  for (const [blocked_by, word] of [
    ['author-is-person', 'author-owned'], ['author-is-maintainer', 'maintainer-owned'],
    ['operator-admission', 'contributor-managed'], ['capability:workflows-permission', 'adoption blocked'],
  ]) {
    const item = { ...pr(), adoption: { author: 'clubanderson', blocked_by } };
    assert.equal(c.stageTrackerAdoptionChipText(item), word);
  }
});
