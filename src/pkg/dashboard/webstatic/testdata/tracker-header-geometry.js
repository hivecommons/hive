// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Scrolls every header control into view for hit-testing, then restores scroll.
// Never run in the user's live tab; never activates controls or changes rows.
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const tolerance = 1;
  const failures = [];
  const coverage = { issueOnly: 0, linked: 0, prOnly: 0, nested: 0, multiIssue: 0, unknown: 0, queuedApproved: 0, wrapped: 0, singleLineRows: 0, clampedRows: 0, controls: 0, controlTargets: 0, hitTested: 0, currentRows: 0, preparation: 0, superseded: 0, states: {} };
  const near = (a, b) => Math.abs(a - b) <= tolerance;
  const visible = el => el && el.getClientRects().length && getComputedStyle(el).display !== 'none';
  const rect = el => el.getBoundingClientRect();
  const assert = (ok, label) => { if (!ok) failures.push(label); };
  const settleScroll = async (control, name) => {
    const maxFrames = 12;
    const requiredStableFrames = 2;
    let previous = '', stable = 0;
    for (let frame = 0; frame < maxFrames; frame++) {
      await new Promise(requestAnimationFrame);
      const box = rect(control), viewport = window.visualViewport;
      const next = JSON.stringify([box.x, box.y, scrollX, scrollY,
        viewport?.offsetLeft, viewport?.offsetTop, viewport?.scale]);
      stable = next === previous ? stable + 1 : 0;
      if (stable >= requiredStableFrames) return;
      previous = next;
    }
    assert(false, name + 'control/visual viewport must settle before its mandatory hit-test');
  };
  const inspectControl = async (control, toolbar, name) => {
    coverage.controlTargets++;
    assert(visible(control), name + 'expected header control is not hidden');
    if (!visible(control)) return; // Already a failure, never an untested success.
    assert(!control.closest('[inert]'), name + 'controls remain focusable/clickable');
    const contained = () => {
      const box = rect(control), bounds = rect(toolbar);
      assert(box.width > 0 && box.height > 0 && box.left >= Math.max(0, bounds.left) - tolerance &&
        box.right <= Math.min(innerWidth, bounds.right) + tolerance, name + 'entire header control stays horizontally inside the toolbar and viewport');
      return box;
    };
    contained(); // Horizontal overflow must fail even if scrolling could hide it.
    const scrollParents = [];
    for (let parent = control.parentElement; parent; parent = parent.parentElement) {
      if (parent.scrollHeight > parent.clientHeight || parent.scrollWidth > parent.clientWidth) {
        scrollParents.push({ el: parent, left: parent.scrollLeft, top: parent.scrollTop });
      }
    }
    try {
      control.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'instant' });
      await settleScroll(control, name);
      const box = contained();
      assert(box.top >= -tolerance && box.bottom <= innerHeight + tolerance, name + 'entire control is vertically reachable after deliberate scrolling');
      const hit = document.elementFromPoint((box.left + box.right) / 2, (box.top + box.bottom) / 2);
      coverage.hitTested++;
      assert(hit === control || control.contains(hit), name + 'every control center hits that control, not an overlay or offscreen null: ' +
        JSON.stringify({ control: control.className, box: box.toJSON(), hit: hit?.className || hit?.tagName || null,
          viewport: window.visualViewport?.toJSON?.() || { offsetLeft: window.visualViewport?.offsetLeft,
            offsetTop: window.visualViewport?.offsetTop, width: window.visualViewport?.width,
            height: window.visualViewport?.height, scale: window.visualViewport?.scale } }));
    } finally {
      for (const { el, left, top } of scrollParents) el.scrollTo({ left, top, behavior: 'instant' });
      await new Promise(requestAnimationFrame);
    }
  };
  const rows = [...document.querySelectorAll('#activity-monitor-section .ham-row, #activity-monitor-section .ham-sub')];
  const references = new Map();
  for (const [index, row] of rows.entries()) {
    const header = row.querySelector(':scope > .ham-row-head > .ham-head-cols');
    const strip = row.querySelector(':scope > .ham-strip-row > ol[aria-label="Pipeline progress"]');
    if (!visible(header) || !visible(strip)) continue;
    const lead = header.querySelector('.ham-head-lead');
    // A row without PR refs still has the PR cell (ham-head-tools) as the
    // home of its chips; only a cell with refs counts as PR metadata here.
    const prCell = header.querySelector('.ham-head-pr');
    const pr = prCell && !prCell.classList.contains('ham-head-tools') ? prCell : null;
    const badges = header.querySelector('.ham-badges');
    const columns = [...strip.children];
    // The renderer emits this boundary from its private canonical lane path.
    const prStart = Number(header.style.getPropertyValue('--ham-pr-start')) - 1;
    const lastLane = columns.length - 2;
    const name = 'row ' + index + ' ';
    if (row.matches('.ham-sub')) coverage.nested++;
    else if (row.dataset.hamKind === 'pr') coverage.prOnly++;
    else if (pr) coverage.linked++;
    else coverage.issueOnly++;
    if (row.matches('.ham-preparation-member')) coverage.preparation++;
    if ([...row.querySelectorAll(':scope > .ham-current-stage, :scope > .ham-human-context')]
      .some(context => /superseded/i.test(context.textContent))) coverage.superseded++;
    if (row.matches('.ham-row') && row.querySelectorAll(':scope > .ham-sub').length > 1) coverage.multiIssue++;
    // Height is deliberately absent from the cross-row signature; within-row
    // uniformity and the one-line height contract are asserted separately
    // below.
    const signature = columns.map(cell => ({ left: rect(cell).left - rect(strip).left, width: rect(cell).width }));
    const stripLeft = rect(strip.parentElement).left;
    // Recorded preparation roots retain the existing rail gutter. Compare
    // like gutters across rows; header-to-lane checks below remain exact.
    const gutter = row.closest('.ham-row.ham-train') ? 'train' : 'ordinary';
    if (!references.has(gutter)) references.set(gutter, { signature, left: stripLeft });
    const reference = references.get(gutter);
    assert(near(stripLeft, reference.left), name + 'root/nested progress containers with the same rail gutter share one horizontal origin');
    for (const [lane, box] of signature.entries()) {
      assert(near(box.left, reference.signature[lane].left) && near(box.width, reference.signature[lane].width), name + 'physical lane ' + lane + ' aligns across issue, PR-only and multi-PR rows');
    }
    const flow = [...row.children].filter(child => visible(child) && !['absolute', 'fixed'].includes(getComputedStyle(child).position));
    const rowGap = parseFloat(getComputedStyle(row).rowGap) || 0;
    for (let child = 1; child < flow.length; child++) {
      const previous = flow[child - 1], next = flow[child];
      const spacing = rowGap + (parseFloat(getComputedStyle(previous).marginBottom) || 0) + (parseFloat(getComputedStyle(next).marginTop) || 0);
      const measured = rect(next).top - rect(previous).bottom;
      assert(measured >= -tolerance && measured <= spacing + tolerance, name + 'no overlap or unexplained vertical gap between successive content/sub-rows: ' +
        JSON.stringify({ previous: previous.className, next: next.className, measured, allowed: spacing }));
    }
    if (matchMedia('(min-width: 1101px)').matches) {
      assert(near(rect(lead).left, rect(columns[0]).left), name + 'issue starts at Open');
      assert(near(rect(lead).right, rect(columns[prStart - 1]).right), name + 'issue ends at Claimed');
      if (pr) {
        assert(near(rect(pr).left, rect(columns[prStart]).left), name + 'PR starts at PR in progress');
        assert(near(rect(pr).right, rect(columns[lastLane]).right), name + 'PR ends at Merge gate');
      }
      if (visible(badges) && badges.children.length) {
        // Chips ride the PR line (or wrap under its metadata), never a
        // line of their own between the title block and the chevrons.
        assert(rect(badges).top >= rect(prCell).top - tolerance && rect(badges).bottom <= rect(prCell).bottom + tolerance, name + 'badges inside the PR cell');
        const metadata = [...prCell.children].filter(child => child !== badges && visible(child));
        if (metadata.length) {
          const metaTop = Math.min(...metadata.map(child => rect(child).top));
          assert(rect(badges).bottom > metaTop + tolerance, name + 'badges share the PR metadata line or wrap under it, never a line above');
        } else {
          assert(near(rect(badges).top, rect(header).top), name + 'badges on the title line');
        }
      }
      assert(near(rect(strip.parentElement).top - rect(header).bottom, rowGap), name + 'chevrons sit directly under the title block (row gap only)');
    } else if (pr && visible(lead)) {
      assert(rect(pr).top >= rect(lead).bottom - tolerance, name + 'narrow PR below issue');
    }
    for (const slot of [lead, pr, badges]) {
      if (!visible(slot)) continue;
      const bounds = rect(slot);
      for (const child of slot.querySelectorAll('.ham-title, .ham-pr-title, .ham-badge, .ham-pr-ref, button')) {
        if (!visible(child)) continue;
        const box = rect(child);
        assert(box.left >= bounds.left - tolerance && box.right <= bounds.right + tolerance, name + child.className + ' within its header slot');
        if (child.clientWidth) assert(child.scrollWidth <= child.clientWidth + tolerance, name + child.className + ' no horizontal clipping');
      }
    }
    if (badges) {
      if (badges.children.length) {
        assert(visible(badges), name + 'nonempty action/history toolbar is not hidden');
        coverage.controls++;
        // The chips are the trailing, right-anchored flex item of the PR
        // cell: they end where the PR span ends (Merge gate), sit beside
        // the issue metadata rather than under it, and never overprint the
        // PR metadata they follow (wrapping under it when space runs out).
        const toolbar = badges.parentElement;
        const desktop = matchMedia('(min-width: 1101px)').matches;
        const expectedRight = desktop ? rect(toolbar).right : rect(header).right;
        assert(near(rect(badges).right, expectedRight), name + 'controls are right-anchored at the end of the PR span, not in an issue-side column');
        if (desktop) assert(rect(badges).left >= rect(lead).right - tolerance, name + 'toolbar does not intersect issue metadata');
        else assert(rect(toolbar).top >= rect(lead).bottom - tolerance, name + 'narrow toolbar below issue metadata');
        for (const child of toolbar.children) {
          if (child === badges || !visible(child)) continue;
          const box = rect(child), chips = rect(badges);
          const apart = box.right <= chips.left + tolerance || box.left >= chips.right - tolerance || box.bottom <= chips.top + tolerance || box.top >= chips.bottom - tolerance;
          assert(apart, name + 'toolbar does not intersect PR metadata: ' + JSON.stringify({ child: child.className, box, chips }));
        }
        for (const control of badges.querySelectorAll('button, a, [tabindex]')) {
          if (!control.closest('.config-tooltip')) await inspectControl(control, toolbar, name);
        }
      }
      const chips = [...badges.children].filter(visible).map(rect);
      for (let a = 0; a < chips.length; a++) for (let b = a + 1; b < chips.length; b++) {
        const overlapX = Math.min(chips[a].right, chips[b].right) - Math.max(chips[a].left, chips[b].left);
        const overlapY = Math.min(chips[a].bottom, chips[b].bottom) - Math.max(chips[a].top, chips[b].top);
        assert(overlapX <= tolerance || overlapY <= tolerance, name + 'badges do not overlap');
      }
    }
    const heights = columns.map(cell => rect(cell).height);
    assert(Math.max(...heights) - Math.min(...heights) <= tolerance, name + 'all physical chevrons have uniform height');
    // Compact height: captions are clamped to one line, so every row is
    // exactly one text line plus the chevron's own padding and border
    // (~20px) — the pre-hivecommons/hive#16454 density. Icon-only rows
    // share the same one-line floor.
    const lineWidth = parseFloat(getComputedStyle(strip).getPropertyValue('--line-width')) || 0;
    const cellBudget = (cell, lines, border) => {
      const style = getComputedStyle(cell);
      return lines * parseFloat(style.lineHeight) + parseFloat(style.paddingTop) + parseFloat(style.paddingBottom) +
        (border === undefined ? parseFloat(style.borderTopWidth) + parseFloat(style.borderBottomWidth) : border);
    };
    let rowLines = 0, expectedHeight = cellBudget(columns[0], 1, 2 * lineWidth);
    for (const cell of columns) {
      for (const copy of cell.querySelectorAll('.ham-chevron-copy, .cue-acting-cap')) {
        if (!visible(copy)) continue;
        const lines = Math.max(1, Math.round(rect(copy).height / parseFloat(getComputedStyle(copy).lineHeight)));
        rowLines = Math.max(rowLines, lines);
        expectedHeight = Math.max(expectedHeight, cellBudget(cell, lines));
      }
    }
    assert(rowLines <= 1, name + 'captions are clamped to a single line: ' + JSON.stringify({ lines: rowLines }));
    assert(near(Math.max(...heights), expectedHeight), name + 'chevron height is one text line + padding, not a two-line box: ' +
      JSON.stringify({ measured: Math.max(...heights), expected: expectedHeight }));
    coverage.singleLineRows++;
    if (strip.querySelector('.ham-chevron-copy') && [...strip.querySelectorAll('.ham-chevron-copy')].some(c => visible(c) && c.scrollHeight > rect(c).height + tolerance)) coverage.clampedRows++;
    const ci = strip.querySelector('[data-track="ci"]');
    const review = strip.querySelector('[data-track="review"]');
    const current = columns.filter(cell => !cell.classList.contains('cap') &&
      (cell.classList.contains('active') || cell.classList.contains('ham-current')));
    const delegated = row.matches('.ham-row') && row.querySelector(':scope > .ham-sub ol[aria-label="Pipeline progress"]');
    assert(current.length > 0 || !!delegated, name + 'every actionable row owns a visible current-phase location or delegates to drawn PR children');
    if (current.length) coverage.currentRows++;
    if (ci?.dataset.activityState === 'queued' && review?.dataset.activityState === 'approved') coverage.queuedApproved++;
    for (const cell of columns) {
      if (cell.dataset.activityState) coverage.states[cell.dataset.activityState] = (coverage.states[cell.dataset.activityState] || 0) + 1;
      if (cell.dataset.routeState?.split(' ').includes('active')) {
        assert(cell.classList.contains('ham-current'), name + 'current phase remains highlighted independently of work');
        if (cell.classList.contains('active') || !cell.dataset.track) {
          const style = getComputedStyle(cell);
          assert(Number(style.opacity) >= 0.9 && style.backgroundColor !== 'rgba(0, 0, 0, 0)' && style.backgroundColor !== 'transparent', name + 'active and stalled current tracks retain a visible steady fill');
        } else {
          // Inactive CI/Review lanes keep their current-phase location but
          // render only the state-coloured icon (#16599); words stay in the label.
          const words = cell.querySelector('.ham-track-state')?.innerText.trim() || '';
          assert(!!cell.querySelector('svg') && words === '' && /\b(CI|Review): (CI|Review)\b/.test(cell.getAttribute('aria-label') || ''), name + 'inactive current tracks retain an icon with their words in the label');
        }
      }
      if (cell.dataset.track) {
        // CI / Review lanes are chevrons like their neighbours: same notch
        // polygon, padding and height, state expressed by fill, not a box.
        const style = getComputedStyle(cell);
        const peer = columns.find(other => other !== cell && !other.dataset.track && !other.classList.contains('cap') && other !== columns[0]);
        const peerStyle = getComputedStyle(peer);
        assert(style.clipPath !== 'none' && style.clipPath === peerStyle.clipPath, name + 'activity track keeps the chevron notch polygon: ' + JSON.stringify({ track: style.clipPath, peer: peerStyle.clipPath }));
        assert(style.borderTopWidth === '0px' && style.borderLeftWidth === '0px' && style.borderRadius === peerStyle.borderRadius, name + 'activity track is not a bordered box');
        assert(style.paddingLeft === peerStyle.paddingLeft && style.paddingRight === peerStyle.paddingRight && style.paddingTop === peerStyle.paddingTop, name + 'activity track shares chevron padding');
        assert(near(rect(cell).height, rect(peer).height), name + 'activity track shares chevron height');
      }
      if (cell.dataset.displayState === 'unknown') {
        coverage.unknown++;
        // #16493: a track in the current phase always carries count words
        // ("CI ?/? · no checks reported yet"); only a not-started track is
        // the glyph alone.
        const words = cell.querySelector('.ham-track-state')?.innerText.trim() || '';
        if (cell.classList.contains('todo')) assert(words === '', name + 'not-started track is icon-only');
        // A settled verdict (passed / failed, approved / changes requested)
        // is the glyph alone in its colour; its count words ride in the
        // tooltip and accessible label.
        else if (cell.dataset.verdict) assert(words === '' && /\b(CI|Review): (CI|Review)\b/.test(cell.getAttribute('aria-label') || ''), name + 'settled verdict is the glyph alone with its words in the label: ' + JSON.stringify(words));
        else assert(/^(CI|Review)\b/.test(words), name + 'lit unknown track carries count words, never a bare glyph: ' + JSON.stringify(words));
        assert(!!cell.querySelector('svg'), name + 'unknown track retains the column glyph');
        assert(!cell.hasAttribute('data-motion'), name + 'unknown cannot animate as observed work');
      }
      const info = cell.querySelector('.ham-chevron-info');
      if (info?.classList.contains('ham-chevron-decision')) {
        const button = info.querySelector('.ham-decision-trigger');
        assert(!!button && button.getAttribute('aria-haspopup') === 'dialog', name + 'decision retains its native keyboard/pointer trigger');
        assert(!info.matches('.config-info, [tabindex]') && !info.querySelector('.config-tooltip') && !button?.closest('.config-info'), name + 'decision dialog is the sole hover/focus detail surface');
        assert(!!button?.dataset.hamTip, name + 'decision dialog receives full unclamped overflow details');
      } else if (info) {
        const tip = document.getElementById(info.getAttribute('aria-describedby'));
        assert(info.tabIndex === 0 && !!tip && tip.getAttribute('role') === 'tooltip', name + 'full details are keyboard/hover accessible');
        assert(!!info.getAttribute('aria-label'), name + 'full accurate accessible label is retained');
      }
      for (const copy of cell.querySelectorAll('.ham-chevron-copy, .cue-acting-cap')) {
        if (!visible(copy)) continue;
        const style = getComputedStyle(copy);
        const lineHeight = parseFloat(style.lineHeight);
        const bounds = rect(copy);
        assert(style.whiteSpace === 'normal' && style.overflowWrap === 'anywhere', name + 'text wraps, including unbroken handles');
        assert(bounds.height <= lineHeight + tolerance, name + 'maximum one visible text line');
        if (copy.scrollHeight > bounds.height + tolerance) coverage.wrapped++;
        assert(bounds.left >= rect(cell).left + parseFloat(getComputedStyle(cell).paddingLeft) - tolerance, name + 'text clears the left chevron notch');
        assert(bounds.right <= rect(cell).right - parseFloat(getComputedStyle(cell).paddingRight) + tolerance, name + 'text clears the right chevron point');
        // Centred as before the two-line budget: the glyphs sit centred in
        // their copy box (a stretched copy with start-aligned text fails
        // here), and the copy together with its reserved icons sits centred
        // in the chevron's content box, both horizontally and vertically.
        const content = { left: rect(cell).left + parseFloat(getComputedStyle(cell).paddingLeft), right: rect(cell).right - parseFloat(getComputedStyle(cell).paddingRight),
          top: rect(cell).top + parseFloat(getComputedStyle(cell).paddingTop), bottom: rect(cell).bottom - parseFloat(getComputedStyle(cell).paddingBottom) };
        const range = document.createRange();
        range.selectNodeContents(copy);
        const glyphs = [...range.getClientRects()].filter(line => line.width > 0 && line.top >= bounds.top - tolerance && line.bottom <= bounds.bottom + tolerance);
        const glyphTolerance = parseFloat(style.fontSize) / 2; // a trailing wrap space, never a ragged-left line
        if (glyphs.length) {
          const text = { left: Math.min(...glyphs.map(line => line.left)), right: Math.max(...glyphs.map(line => line.right)) };
          assert(Math.abs((text.left + text.right) / 2 - (bounds.left + bounds.right) / 2) <= glyphTolerance, name + 'caption text is horizontally centred within its copy box: ' +
            JSON.stringify({ text, copy: { left: bounds.left, right: bounds.right } }));
        }
        // A `display: contents` wrapper (.ham-active-ico-wrap) has no box of
        // its own: its children are the flex items that take space, so the
        // union measures them, not the box-less wrapper.
        const boxes = el => getComputedStyle(el).display === 'contents' ? [...el.children].flatMap(boxes) : [el];
        const group = copy.parentElement === info
          ? [...info.children].flatMap(boxes).filter(child => visible(child) && !child.classList.contains('config-tooltip')).map(rect)
          : [bounds];
        const union = { left: Math.min(...group.map(box => box.left)), right: Math.max(...group.map(box => box.right)),
          top: Math.min(...group.map(box => box.top)), bottom: Math.max(...group.map(box => box.bottom)) };
        assert(Math.abs((union.left + union.right) / 2 - (content.left + content.right) / 2) <= tolerance, name + 'caption and icons are horizontally centred in the chevron: ' +
          JSON.stringify({ union, content }));
        assert(Math.abs((union.top + union.bottom) / 2 - (content.top + content.bottom) / 2) <= tolerance, name + 'caption and icons are vertically centred in the chevron: ' +
          JSON.stringify({ union, content }));
        const icons = info ? [...info.querySelectorAll(':scope > svg, :scope > .ham-active-ico-wrap svg, :scope > .ham-gate-ico svg')].filter(visible) : [];
        for (const icon of icons) {
          const box = rect(icon);
          assert(box.right <= bounds.left + tolerance || box.left >= bounds.right - tolerance, name + 'text does not overlap reserved icons');
        }
      }
    }
    const progressTop = rect(strip).top;
    for (const slot of [lead, pr, badges]) {
      if (visible(slot)) assert(rect(slot).bottom <= progressTop + tolerance, name + 'header above progress tracks');
    }
  }
  assert(coverage.issueOnly > 0 && coverage.linked > 0 && coverage.prOnly > 0, 'preview must contain issue-only, linked, and PR-only fixtures');
  assert(coverage.multiIssue > 0 && coverage.nested >= 2, 'preview must contain a real multi-PR issue with independent nested rows');
  assert(coverage.preparation >= 2 && coverage.superseded > 0, 'preview must contain recorded/unverified preparation fronts and an actionable superseded-human-decision row');
  for (const state of ['queued', 'unknown', 'assigned', 'approved', 'waiting', 'failing', 'passing', 'running']) {
    assert(coverage.states[state] > 0, 'preview must exercise current-head activity state ' + state);
  }
  assert(coverage.unknown > 0 && coverage.queuedApproved > 0 && coverage.wrapped > 0 && coverage.controls > 0, 'preview must include unknown, queued-CI/approved-review, long clamped text and action/history controls');
  assert(coverage.singleLineRows > 0 && coverage.clampedRows > 0, 'preview must include a row whose caption overflows its one-line clamp (long text stays a one-line chevron)');
  assert(coverage.controlTargets > 0 && coverage.controlTargets === coverage.hitTested, 'every relevant header control must be hit-tested, never skipped as offscreen');
  if (failures.length) throw new Error(JSON.stringify({ viewport: innerWidth, coverage, failures }));
  return { viewport: innerWidth, coverage, failures };
})
