// CI-only markup and grid-track arithmetic, not browser/visual proof.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const html = fs.readFileSync(path.join(__dirname, 'static/index.html'), 'utf8');
const pick = (re, name) => {
  const found = html.match(re);
  assert.ok(found, name);
  return found[0];
};
const fn = name => pick(new RegExp('function ' + name + '\\([^\\n]*\\) \\{[\\s\\S]*?\\n    \\}'), name);
const esc = value => String(value).replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
const title = 'A long identity and title ' + 'unbroken-title-'.repeat(30);
const chips = '<span class="ham-badge ham-badge-waiting">waiting on @a-very-long-contributor · acknowledged</span><span class="ham-badge ham-badge-push">pushed current-head · re-review pending</span><span class="ham-badge ham-mod-regressed">regressed</span>';

function load() {
  const ctx = vm.createContext({
    esc, Date,
    STAGE_TRACKER_LINKED_GLYPH: '<span class="ham-linked-ico">↳</span>',
    stageTrackerItemKey: it => it.kind + ':' + it.number,
    stageTrackerLeadStage: it => it.stage,
    stageTrackerAnchor: n => Number(n) || 0,
    stageTrackerLinkedSorted: it => it.linked_prs || [],
    stageTrackerLinkedPRItem: (issue, pr) => ({ ...pr, repo: issue.repo, kind: 'pr' }),
    stageTrackerLinkedTip: () => 'linked PR',
    stageTrackerIcon: (kind, cls) => `<svg class="${cls || ''}"></svg>`,
    stageTrackerRepoChipHtml: () => '<button class="ham-repo-chip">long-repo-name</button>',
    stageTrackerTooltip: () => 'tooltip',
    stageTrackerPRPillsHtml: ref => `<span class="ham-pr-line">${ref}<span class="ham-badge">base v5</span></span>`,
    stageTrackerActedBadgeHtml: () => '',
    stageTrackerChipsHtml: () => chips,
    stageTrackerMoveAttrs: () => '',
    stageTrackerSinceHtml: () => '<span class="ham-since">2m</span>',
    stageTrackerCurrentStageHtml: () => '',
    stageTrackerRowToolsHtml: () => '',
    stageTrackerStripItems: () => '',
  });
  for (const name of ['STAGE_TRACKER_PATH', 'STAGE_TRACKER_PR_COLUMN', 'STAGE_TRACKER_TERMINAL',
    'STAGE_TRACKER_CLOSED_ISSUE_TITLE', 'STAGE_TRACKER_UNTITLED_ISSUE', 'STAGE_TRACKER_SOFT_LINK_VIAS', 'STAGE_TRACKER_SOFT_LINK_TIP']) {
    vm.runInContext(pick(new RegExp('const ' + name + ' = [^\\n]*'), name), ctx);
  }
  for (const name of ['stageTrackerHeadColsHtml', 'stageTrackerHeaderHtml', 'stageTrackerRowHtml', 'stageTrackerSubRowHtml',
    'stageTrackerPRRef', 'stageTrackerPRIssueSlot', 'stageTrackerTitleHtml', 'stageTrackerSplitTitleTag']) {
    vm.runInContext(fn(name), ctx);
  }
  return ctx;
}
const pr = { kind: 'pr', repo: 'hivecommons/hive', number: 7, stage: 70, title, author: 'a-very-long-contributor', author_kind: 'human', url: 'https://github.com/hivecommons/hive/pull/7' };
const issue = { kind: 'issue', repo: pr.repo, number: 1, stage: 20, title: '[scanner] ' + title, url: 'https://github.com/hivecommons/hive/issues/1' };

for (const [name, item] of [
  ['issue-only', issue],
  ['linked issue/PR', { ...issue, linked_prs: [pr] }],
  ['PR-only', pr],
]) test(name + ': full metadata stays in the correct header slots', () => {
  const ctx = load();
  const rendered = ctx.stageTrackerRowHtml(item, [], { nowMs: 1, repoChips: {} });
  const start = Number(rendered.match(/data-ham-pr-start="(\d+)"/)[1]);
  assert.equal(start, 4, 'PR metadata starts at PR in progress, not Actionable');
  const lead = rendered.match(/<li class="ham-seg ham-head-lead[^"]*">([\s\S]*?)<\/li>/)[1];
  if (name === 'PR-only') {
    assert.match(lead, /no linked issue/);
    assert.doesNotMatch(lead, /#7/);
  } else {
    assert.match(lead, /#1/);
    assert.ok(lead.includes(title), 'complete issue title, not a shortened replacement');
    assert.match(lead, /ham-title-tag/);
  }
  if (name !== 'issue-only') {
    const ref = rendered.match(/<li class="ham-seg ham-head-pr"[^>]*>([\s\S]*?)<\/li>/)[1];
    assert.match(ref, /#7/);
    assert.ok(ref.includes(title));
    assert.match(ref, /ham-author-human/);
    if (name === 'linked issue/PR') assert.match(rendered, /data-ham-target-key="pr:7"/);
  }
  assert.ok(rendered.includes(chips), 'waiting, push, and regression metadata is retained');
});

function rule(selector) {
  // Match the whole selector, not its suffix in ".ham-cols .ham-strip".
  const found = html.match(new RegExp('(?:^|[\\r\\n])[\\t ]*' + selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + ' \\{([^}]+)\\}'));
  assert.ok(found, selector);
  return found[1];
}

test('persistent human/preparation context wins the generic event-entry animation cascade', () => {
  assert.match(rule('.cue-event'), /animation: cue-event-in/);
  for (const selector of ['.cue-event.ham-human-context', '.cue-event.ham-preparation-comment']) {
    assert.match(rule(selector), /animation: none/);
    assert.ok(selector.split('.').length > '.cue-event'.split('.').length,
      'persistent context has greater specificity even when the generic event rule appears later');
  }
});

test('row materialization binds canonical header boundaries for root and nested rows', () => {
  const headers = ['4', '5'].map(hamPrStart => ({
    dataset: { hamPrStart }, properties: {},
    style: { setProperty(name, value) { this.owner.properties[name] = value; } },
  }));
  for (const header of headers) header.style.owner = header;
  const node = { dataset: {}, querySelectorAll(selector) {
    assert.equal(selector, '.ham-head-cols[data-ham-pr-start]');
    return headers;
  } };
  const ctx = vm.createContext({ _stageTrackerRowSrc: null,
    document: { createElement: () => ({ content: { firstElementChild: node } }) } });
  vm.runInContext(fn('stageTrackerRowNode'), ctx);
  assert.equal(ctx.stageTrackerRowNode('issue:1', '<fixture>'), node);
  assert.equal(node.dataset.hamKey, 'issue:1');
  assert.deepEqual(headers.map(header => header.properties['--ham-pr-start']), ['4', '5']);
});

test('spans share the physical lane grid, including independent concurrent CI/Review', () => {
  const ctx = load();
  const header = ctx.stageTrackerHeaderHtml([]);
  const physicalCount = (header.match(/<li /g) || []).length;
  assert.equal(physicalCount, 8, 'seven lanes plus history, not six aggregate lanes');
  assert.match(header, />CI<\/span>/);
  assert.match(header, />Review<\/span>/);
  const strip = rule('.ham-strip');
  assert.match(strip, /display: grid/);
  assert.match(strip, /grid-template-columns: repeat\(var\(--ham-visible-columns\), minmax\(0, 1fr\)\) 4em/);
  const laneCount = Number(strip.match(/--ham-visible-columns: (\d+)/)[1]);
  assert.equal(laneCount, physicalCount - 1, 'CSS and actual rendered lane count must agree');
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-lead'), /grid-column: 1 \/ var\(--ham-pr-start\)/);
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr'), /grid-column: var\(--ham-pr-start\) \/ -2/);
  assert.doesNotMatch(html, /\.ham-head-cols \.ham-seg\.cap \{/, 'no separate toolbar line between the title line and the chevrons');
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-badges'), /margin-left: auto/, 'chips right-anchor on the PR line');
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-badges'), /justify-content: flex-end/);
  // Resolve the declared grid lines using the actual header's track count.
  // Content cannot alter minmax(0,1fr) track widths; covered gaps belong to spans.
  const start = Number(ctx.stageTrackerHeadColsHtml(['PR'], 'issue', chips).match(/data-ham-pr-start="(\d+)"/)[1]);
  const gap = 4, cap = 48;
  for (const width of [2400, 1440, 1110]) {
    const lane = (width - cap - laneCount * gap) / laneCount;
    const prLeft = (start - 1) * (lane + gap);
    const issueRight = (start - 1) * lane + (start - 2) * gap;
    const prRight = laneCount * lane + (laneCount - 1) * gap;
    assert.ok(Math.abs(prLeft - issueRight - gap) < 1e-9, 'issue span ends one gutter before PR');
    assert.ok(Math.abs((prRight - prLeft) - ((laneCount - start + 1) * lane + (laneCount - start) * gap)) < 1e-9);
    assert.ok(prRight < width - cap, 'PR span excludes the history cap');
  }
  vm.runInContext('STAGE_TRACKER_PATH.splice(2, 0, 15)', ctx);
  assert.match(ctx.stageTrackerHeadColsHtml(['PR']), /data-ham-pr-start="5"/, 'boundary derives from canonical stages, not a second hard-coded span');
});

test('chevron height is one caption line (compact strip, single-line clamp) and the caption stays centred', () => {
  const strip = rule('.ham-strip[aria-label="Pipeline progress"]');
  const seg = rule('.ham-strip[aria-label="Pipeline progress"] > .ham-seg');
  const copy = rule('.ham-chevron-copy');
  assert.match(strip, /grid-auto-rows: minmax\(calc\(var\(--ham-chevron-line-height\) \* 1em \+ 2 \* var\(--sp-1\) \+ 2 \* var\(--line-width\)\), auto\)/,
    'rows size to content above a one-line floor with sp-1 vertical padding (~20px strip)');
  assert.match(seg, /padding: var\(--sp-1\) calc\(var\(--ham-notch\) \+ var\(--sp-2\)\)/, 'sp-2 vertical padding doubled the strip height (hivecommons/hive#16454)');
  assert.match(rule('.ham-strip'), /--ham-chevron-text-lines: 1;/, 'captions are clamped to one line so the strip stays compact');
  assert.doesNotMatch(strip, /grid-auto-rows: calc\(var\(--ham-chevron-text-lines\)/, 'no fixed two-line row height around one line of text');
  assert.match(seg, /align-self: stretch/);
  assert.doesNotMatch(seg, /height: 100%/, 'siblings follow the row, the row follows its content');
  assert.match(copy, /max-height: calc\(var\(--ham-chevron-text-lines\) \* var\(--ham-chevron-line-height\) \* 1em\)/, 'the clamp caps the copy at --ham-chevron-text-lines lines');
  assert.match(copy, /flex: 0 1 auto/, 'the copy shrink-wraps its text so justify-content: center centres it');
  assert.match(copy, /text-align: center/, 'a wrapped second line is centred under the first');
  assert.match(rule('.ham-chevron-info.config-info, .ham-chevron-info.ham-chevron-decision'), /align-items: center; justify-content: center/);
});

test('the issue title sits inline after its chips on the first line, not on a line of its own', () => {
  const ctx = load();
  const rendered = ctx.stageTrackerRowHtml({ ...issue, number: 16475 }, [], { nowMs: 1, repoChips: {} });
  // One flex container holds icon, repo chip, #ref, agent chip and title.
  const main = rendered.match(/<div class="ham-head-main">([\s\S]*?)<\/div>/)[1];
  assert.match(main, /ham-kind-ico[\s\S]*ham-repo-chip[\s\S]*ham-ref[\s\S]*#16475[\s\S]*ham-title-tag[\s\S]*class="ham-title"/,
    'kind icon, repo chip, #ref, agent chip and title render in that order in one container');
  assert.doesNotMatch(main, /<(div|p|br)\b/, 'no block element splits the title from its chips');
  const container = rule('.ham-head-main');
  assert.match(container, /display: flex/);
  assert.match(container, /flex-wrap: wrap/, 'chips may still wrap when a line is genuinely full');
  const t = rule('.ham-head-main > .ham-title');
  assert.match(t, /flex: 1 1 0/, 'a zero flex-basis keeps the title on the chip line and fills the rest of it (a max-content basis drops the whole title to line 2)');
  assert.match(t, /min-width: 0/);
  assert.match(t, /white-space: normal/, 'the title wraps its own words only when there is no room');
  const prRef = rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-pr-ref, .ham-head-cols .ham-seg.ham-head-pr > .ham-pr-line');
  assert.match(prRef, /flex: 1 1 0; min-width: min\(100%, 20em\)/, 'the PR ref shares its line with the right-anchored pills');
  assert.match(rule('.ham-head-pr .ham-pr-title'), /flex: 1 1 0/);
});

test('long metadata wraps in bounded slots and the narrow layout resets both grid axes', () => {
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr'), /flex-wrap: wrap/);
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-badges'), /flex-wrap: wrap/);
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-badges'), /max-width: 100%/);
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-badges'), /justify-content: flex-end/);
  assert.match(rule('.ham-head-cols .ham-badge'), /white-space: normal; overflow-wrap: anywhere/);
  assert.match(html, /\.ham-head-main > \.ham-title \{ flex: 1 1 0; min-width: 0; white-space: normal; overflow: visible; overflow-wrap: anywhere; \}/);
  assert.match(html, /\.ham-head-pr \.ham-pr-title \{ flex: 1 1 0; white-space: normal; overflow: visible; overflow-wrap: anywhere; \}/);
  assert.match(html, /@media \(max-width: 1100px\) \{[\s\S]*?\.ham-head-cols \.ham-strip > \.ham-seg \{ grid-column: 1 \/ -1; grid-row: auto; padding: 0; \}/);
  assert.doesNotMatch(html, /\.ham-strip:not\(\.ham-head-cols\)/, 'the old ancestor/element selector mistake must not return');
});

test('issue-only, linked and PR-only controls ride the title line, right-anchored in the PR span, never a line of their own', () => {
  for (const [number, label, history, blocked] of [
    [15329, 'Let hive decide', 1, false],
    [16242, '', 0, false],
    [16422, 'Accept triage', 0, false],
    [16423, '', 1, false],
    [16363, '', 0, false],
    [16424, 'Close now', 0, false],
    [431, '', 1, true],
  ]) {
    const ctx = load();
    ctx.stageTrackerActedBadgeHtml = () => history ? '<span class="ham-badge ham-acted" tabindex="0">history ' + history + '</span>' : '';
    ctx.stageTrackerChipsHtml = () => blocked ? '<span class="ham-badge">blocked</span>' : '';
    ctx.stageTrackerLeadButtonHtml = () => label ? '<button type="button" class="ham-lead-act" data-action="fixtureAction">' + label + '</button>' : '';
    for (const item of [{ ...issue, number, is_tracker: number === 16242 }, { ...issue, number, linked_prs: [pr] }, { ...pr, number }]) {
      const rendered = ctx.stageTrackerRowHtml(item, [], { nowMs: 1 });
      assert.doesNotMatch(rendered, /<li class="ham-seg cap">/, 'the title line has no separate toolbar cell');
      const prCell = rendered.match(/<li class="ham-seg ham-head-pr[^"]*"[^>]*>([^]*?)<\/li>/)[1];
      const toolbar = prCell.match(/<span class="ham-badges">([^]*)<\/span>$/);
      assert.ok(toolbar, 'chips are the trailing flex item of the PR cell');
      const chipsHtml = toolbar[1];
      if (!item.linked_prs && item.kind === 'issue') assert.match(rendered, /<li class="ham-seg ham-head-pr ham-head-tools">/, 'an issue without PR refs still gets the PR cell for its chips');
      if (label) {
        assert.ok(chipsHtml.includes(label));
        assert.equal(rendered.split(label).length - 1, 1, 'action is not duplicated to fake alignment');
        assert.match(chipsHtml, /type="button"[^]*data-action="fixtureAction"/);
      }
      if (history) assert.match(chipsHtml, /ham-acted" tabindex="0">history 1/);
      if (blocked) assert.match(chipsHtml, />blocked</);
      const lead = rendered.match(/<li class="ham-seg ham-head-lead[^"]*">([^]*?)<\/li>/)[1];
      assert.doesNotMatch(lead, /ham-lead-act|ham-acted|>blocked</);
    }
  }
  const toolbar = rule('.ham-head-cols .ham-seg.ham-head-pr > .ham-badges');
  assert.match(toolbar, /position: static/);
  assert.match(toolbar, /margin-left: auto/);
  assert.match(toolbar, /justify-content: flex-end/);
  assert.doesNotMatch(toolbar, /position: absolute|[^-]left:|[^-]right:/, "flow layout, never an absolute overlay over the title");
  assert.match(rule('.ham-head-cols .ham-seg.ham-head-pr'), /grid-row: 1/, 'the PR cell, and so its chips, shares the title line');
  assert.doesNotMatch(html, /\.ham-head-cols [^{]*\{[^}]*grid-row: 2/, 'nothing in the title block claims a second grid row');
});

test('12:01 screenshot density: real multi-PR/sub-row structure preserves distinct headers and full-width control ownership', () => {
  const fixture = JSON.parse(fs.readFileSync(path.join(__dirname, 'webstatic/testdata/tracker-activity-density.json'), 'utf8'));
  const ctx = load();
  vm.runInContext(fn('stageTrackerPRPillsHtml'), ctx);
  ctx.stageTrackerChipsHtml = item => item.push ? `<span class="ham-badge ham-badge-waiting">${esc('@' + item.push.login + ' · ' + (item.rework?.state || item.push.state))}</span>` : '';
  ctx.stageTrackerLeadButtonHtml = item => {
    const text = item.linked_prs?.length > 1 ? 'Let hive decide — all ' + item.linked_prs.length + ' PRs' : item.decision_needed ? 'Let hive decide' : item.manual_merge ? 'Approve and let hive merge' : '';
    return text ? `<button type="button" class="ham-lead-act">${esc(text)}</button>` : '';
  };
  ctx.stageTrackerActedBadgeHtml = item => item.operator_action_count ? '<span class="ham-badge ham-acted" tabindex="0">history 1</span>' : '';
  ctx.stageTrackerHumanContextHtml = item => item.reason ? `<div class="cue-event ham-human-context"><span>${esc(item.reason)}</span></div>` : '';
  let multi = 0, nested = 0;
  for (const item of fixture.rows) {
    const rendered = ctx.stageTrackerRowHtml(item, [], { nowMs: Date.parse(fixture.clock), repoChips: {} });
    const children = item.linked_prs?.length > 1 ? item.linked_prs : [];
    multi += children.length > 1 ? 1 : 0;
    nested += children.length;
    assert.equal((rendered.match(/<div class="ham-sub(?:[" ])/g) || []).length, children.length, 'actual sub-row renderer is exercised, not a flattened replacement');
    assert.equal((rendered.match(/class="ham-strip-row ham-head-cols"/g) || []).length, 1 + children.length);
    for (const header of rendered.matchAll(/<div class="ham-strip-row ham-head-cols"([^]*?)<\/ol>/g)) {
      assert.match(header[1], /data-ham-pr-start="4"/);
      assert.equal((header[1].match(/class="ham-badges"/g) || []).length, 1, 'one right-anchored toolbar per identity, on its title line');
      assert.doesNotMatch(header[1], /class="ham-seg cap"/, 'no separate toolbar line');
      const lead = header[1].match(/ham-head-lead[^"]*">([^]*?)<\/li>/)[1];
      assert.doesNotMatch(lead, /ham-badge-waiting|ham-lead-act|ham-acted/, 'activity/action/history controls never overprint the issue title slot');
    }
    for (const pr of children) {
      const start = rendered.indexOf('data-ham-sub-key="pr:' + pr.number + '"');
      assert.ok(start >= 0);
      const sub = rendered.slice(start).split('<div class="ham-sub')[0];
      const metadata = sub.match(/class="ham-seg ham-head-pr"[^>]*>([^]*?)<span class="ham-badges">([^]*?)<\/span><\/li>/);
      assert.ok(metadata, 'PR metadata then its chips close the PR cell');
      assert.ok(metadata[1].includes(esc(pr.title)), 'complete PR title stays in its own PR-side cell');
      assert.ok(metadata[1].includes('>' + pr.base_ref + '</span>'));
      assert.doesNotMatch(metadata[1], /ham-badge-waiting|ham-lead-act/, 'chips follow the metadata, never interleave it');
      if (pr.push) assert.ok(metadata[2].includes(esc('@' + pr.push.login)), 'owner/activity pill trails the PR line, right-anchored');
    }
  }
  assert.equal(multi, 2, 'both #16404 and #16438 exercise multiple linked PRs');
  assert.equal(nested, 4, '#16442/#16378 and #16446/#16449 each have independent sub-row content');
  assert.ok(fixture.rows.some(item => item.kind === 'pr'), 'PR-only view is also represented');
  assert.match(rule('.ham-sub'), /grid-template-columns: minmax\(0, 1fr\); min-width: 0/, 'nested rows share the root bounded-track contract, not implicit min-content sizing');
});
