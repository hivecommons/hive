// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Phone tier (hivecommons/hive#17161): the Activity monitor renders a card
// per row — ☐/⋯ in the card's top right (#17163), the head clamped to two
// lines (one when a badge line follows it), the progress pill, one line of
// caption. Asserts the acceptance numbers against the activity-density
// fixture: median .ham-row height ≤ 96px, at least six rows inside the
// viewport once scrolled past the header, no title wider than its row, and
// that the pill sheet carries the full title a clamped head cut short.
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const tolerance = 1;
  const MAX_MEDIAN_ROW_HEIGHT = 96;
  // #17161 asks for six rows on a 844px-tall portrait phone; landscape phones
  // (375 / 360px tall) get the same floor scaled to their height.
  const PORTRAIT_HEIGHT = 844;
  const MIN_ROWS_IN_VIEW = Math.max(1, Math.floor(6 * Math.min(1, innerHeight / PORTRAIT_HEIGHT)));
  const failures = [];
  const assert = (ok, message) => { if (!ok) failures.push(message); };
  const frame = () => new Promise(requestAnimationFrame);
  const visible = el => el && el.getClientRects().length && getComputedStyle(el).display !== 'none';
  const phone = matchMedia('(max-width: 640px), ((pointer: coarse) and (max-width: 820px))').matches;
  if (!phone) return { viewport: innerWidth, phone: false, skipped: 'strip tier: rows keep the desktop lane layout, see tracker-header-geometry', failures };
  const panel = document.getElementById('activity-monitor-panel');
  const host = document.getElementById('activity-density-layout-fixture') || panel;
  const rows = [...host.querySelectorAll('.ham-row')].filter(visible);
  assert(rows.length > 0, 'phone: fixture renders rows');
  const heights = rows.map(row => row.getBoundingClientRect().height).sort((a, b) => a - b);
  const median = heights.length ? heights[Math.floor(heights.length / 2)] : 0;
  assert(median <= MAX_MEDIAN_ROW_HEIGHT, 'median .ham-row height ≤ ' + MAX_MEDIAN_ROW_HEIGHT + 'px: ' + Math.round(median) + ' (' + heights.map(Math.round).join(',') + ')');
  // Rows inside one viewport once the list is scrolled past the section
  // header: the fixture opens with its nested issue→PR groups, so the count
  // is taken for the densest run of consecutive rows (each row's top as the
  // scroll position) rather than only the first screen.
  const boxes = rows.map(row => row.getBoundingClientRect());
  let inView = 0;
  for (let i = 0; i < boxes.length; i++) {
    let n = 0;
    for (let j = i; j < boxes.length && boxes[j].bottom - boxes[i].top <= innerHeight + tolerance; j++) n++;
    inView = Math.max(inView, n);
  }
  assert(inView >= MIN_ROWS_IN_VIEW, 'at least ' + MIN_ROWS_IN_VIEW + ' rows inside one ' + innerHeight + 'px viewport past the header: ' + inView);
  for (const [index, row] of rows.entries()) {
    const bounds = row.getBoundingClientRect();
    // Lead actions and badges wrap onto their own line instead of being
    // clipped by the clamp (tracker-phone-ergonomics covers the ☐ / ⋯ tools).
    for (const badge of row.querySelectorAll(':scope > .ham-row-head .ham-badges > *')) {
      if (!visible(badge)) continue;
      const bb = badge.getBoundingClientRect();
      assert(bb.right <= bounds.right + tolerance && bb.bottom <= row.querySelector(':scope > .ham-row-head').getBoundingClientRect().bottom + tolerance, 'row ' + index + ' badge "' + badge.textContent.trim().slice(0, 24) + '" is visible inside the head');
    }
    for (const title of row.querySelectorAll('.ham-title, .ham-pr-title')) {
      if (!visible(title)) continue;
      const tb = title.getBoundingClientRect();
      assert(tb.right <= bounds.right + tolerance && tb.left >= bounds.left - tolerance, 'row ' + index + ' title stays inside its row');
    }
    assert(![...row.querySelectorAll(':scope > .ham-row-head .ham-no-issue-t')].some(visible), 'row ' + index + ' drops the "no linked issue" placeholder');
    const caption = row.querySelector(':scope > .ham-current-stage');
    if (visible(caption)) {
      const lh = parseFloat(getComputedStyle(caption).lineHeight) || parseFloat(getComputedStyle(caption).fontSize) * 1.4;
      assert(caption.getBoundingClientRect().height <= lh + tolerance, 'row ' + index + ' caption is one line');
    }
    assert(row.scrollWidth <= row.clientWidth + tolerance, 'row ' + index + ' does not scroll sideways');
  }
  // A clamped head exposes the full title through the pill sheet.
  const clamped = rows.find(row => [...row.querySelectorAll(':scope > .ham-row-head :is(.ham-head-main, .ham-pr-ref)')].some(h => h.scrollHeight > h.clientHeight + tolerance) && row.querySelector(':scope > .ham-strip-row > .ham-pill'));
  if (clamped) {
    const pill = clamped.querySelector(':scope > .ham-strip-row > .ham-pill');
    const want = [...clamped.querySelectorAll(':scope > .ham-row-head .ham-title:not(.ham-no-issue-t), :scope > .ham-row-head .ham-pr-title')].map(n => n.textContent.trim()).filter(Boolean).join(' · ');
    pill.click();
    await frame(); await frame();
    const sheet = document.querySelector('.hive-dialog-overlay .hive-dialog.modal-sheet.ham-pill-sheet');
    assert(!!sheet, 'pill opens the sheet for a clamped row');
    const full = sheet && sheet.querySelector('.ham-pill-sheet-title');
    assert(!!full && full.textContent.trim() === want, 'sheet carries the full title of a clamped head');
    if (sheet) {
      (sheet.querySelector('.modal-sheet-close') || sheet.querySelector('.primary')).click();
      await frame(); await frame();
    }
  }
  const out = { viewport: innerWidth, phone: true, rows: rows.length, median: Math.round(median), inView, clampedChecked: !!clamped, failures };
  if (failures.length) throw new Error(JSON.stringify(out));
  return out;
})
