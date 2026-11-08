// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Phone-only acceptance for the Activity monitor chrome (#17158): on a
// portrait phone the compact filter bar replaces the toolbar, the chrome
// between the section header and the first row stays short enough that a
// row is visible without scrolling (a fixture row is a whole work-item
// cluster, so a tall first row only has to show its header line), and the
// ⋯ Filters sheet carries the full toolbar and returns it on close. On a
// landscape phone (the responsive.css landscape tier) the same compact bar
// is the whole sticky stack (column header hidden), merge banners are
// capped to one and at least two rows start inside the viewport. Desktop
// returns a skip marker. Restores scroll positions.
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const phoneMaxWidth = 640;
  const maxChromePx = 160;
  const minRowPeekPx = 120;
  const minLandscapeRowShare = 50;
  const referenceLandscapeHeight = 390;
  const failures = [];
  const assert = (ok, label) => { if (!ok) failures.push(label); };
  const rect = el => el.getBoundingClientRect();
  const visible = el => !!(el && el.getClientRects().length && getComputedStyle(el).display !== 'none');
  const frames = async (n) => { for (let i = 0; i < n; i++) await new Promise(requestAnimationFrame); };
  const landscape = matchMedia('(max-width: 1024px) and (max-height: 500px) and (orientation: landscape) and (pointer: coarse)').matches;
  if (!landscape && innerWidth > phoneMaxWidth) return { skipped: 'not a phone-width viewport', viewport: innerWidth };
  const tier = landscape ? 'landscape phone' : 'portrait phone';

  const section = document.getElementById('activity-monitor-section');
  const header = section && section.querySelector('.dash-card-header');
  const panel = document.getElementById('activity-monitor-panel');
  if (!section || !header || !panel) throw new Error(JSON.stringify({ failures: ['Activity monitor section, header or panel is missing'] }));
  const navbar = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--navbar-sticky-height')) || 0;
  const savedScroll = { x: scrollX, y: scrollY };
  const measurements = {};
  try {
    header.scrollIntoView({ block: 'start', behavior: 'instant' });
    await frames(3);
    const compact = document.getElementById('activity-monitor-compact');
    const toolbar = document.getElementById('activity-monitor-toolbar');
    const labels = document.getElementById('activity-monitor-labels');
    const more = document.getElementById('activity-monitor-filters');
    assert(visible(compact), 'compact filter bar is shown on a ' + tier);
    assert(toolbar && !visible(toolbar), 'full toolbar is hidden behind the Filters sheet on a ' + tier);
    assert(labels && !visible(labels), 'label chips are hidden behind the Filters sheet on a ' + tier);
    assert(visible(more), '⋯ Filters button is shown on a ' + tier);
    const chips = compact ? [...compact.querySelectorAll('.ham-chip')] : [];
    assert(chips.length >= 5, 'compact bar carries kind, needs-attention and waiting-on-you chips');
    for (const chip of chips) {
      const box = rect(chip);
      assert(box.height >= 40 - 1, 'compact chip reaches tap height: ' + chip.textContent.trim());
    }
    const rows = [...panel.querySelectorAll('.ham-row')];
    assert(rows.length > 0, 'fixture rendered at least one row');
    if (rows.length) {
      const first = rows[0];
      // The acceptance fixture prefixes its rows with a provenance note that
      // real renders do not have; measure the chrome without it.
      const note = panel.querySelector('#activity-density-layout-fixture > p');
      const noteStyle = note && getComputedStyle(note);
      measurements.fixtureNote = note ? Math.round(rect(note).height + parseFloat(noteStyle.marginTop) + parseFloat(noteStyle.marginBottom)) : 0;
      measurements.chrome = Math.round(rect(first).top - rect(header).bottom - measurements.fixtureNote);
      assert(measurements.chrome <= maxChromePx, 'chrome between the section header and the first row is ' + measurements.chrome + 'px (max ' + maxChromePx + ')');
      measurements.rowsFullyVisible = rows.filter(row => { const box = rect(row); return box.top >= navbar - 1 && box.bottom <= innerHeight + 1; }).length;
      // The density fixture's first row is a parent issue with its child PR,
      // decision and notes: taller than a 667px phone leaves below the chrome
      // (#17176). Accept it when enough of it shows to read its header line.
      const firstBox = rect(first);
      measurements.firstRowPeek = Math.round(Math.min(firstBox.bottom, innerHeight) - Math.max(firstBox.top, navbar));
      const firstRowTooTall = firstBox.height > innerHeight - Math.max(firstBox.top, navbar);
      assert(measurements.rowsFullyVisible >= 1 || (firstRowTooTall && measurements.firstRowPeek >= minRowPeekPx),
        'at least one row is fully visible with the section header at the top of the viewport (or the first row is taller than the space left and shows at least ' + minRowPeekPx + 'px, got ' + measurements.firstRowPeek + 'px)');
    }
    const sticky = document.getElementById('activity-monitor-sticky');
    if (sticky) {
      measurements.stickyHeight = Math.round(rect(sticky).height);
      const cols = panel.querySelector('.ham-cols');
      measurements.colsHeight = cols && visible(cols) ? Math.round(rect(cols).height) : 0;
      assert(measurements.stickyHeight + measurements.colsHeight <= 100, 'sticky bar plus column header stays within 100px');
      if (landscape) assert(!cols || !visible(cols), 'column header is hidden on a landscape phone');
    }
    if (landscape) {
      assert(typeof stageTrackerMergeBannerCap === 'function' && stageTrackerMergeBannerCap() === 1, 'merge banners are capped to one on a landscape phone');
      const banners = [...document.querySelectorAll('.merge-banner')].filter(visible);
      assert(banners.length <= 1, 'at most one merge banner is visible on a landscape phone');
      if (sticky && rows.length) {
        // A fixture row is a whole work-item cluster, taller than a landscape
        // phone; the contract is that with the bar at the top, rows (not
        // chrome) fill most of the viewport and, at the epic's reference
        // 844×390, a second row starts inside it.
        sticky.scrollIntoView({ block: 'start', behavior: 'instant' });
        await frames(3);
        measurements.rowsStartingInViewport = rows.filter(row => { const box = rect(row); return box.top >= navbar - 1 && box.top < innerHeight; }).length;
        const rowArea = rows.reduce((sum, row) => { const box = rect(row); return sum + Math.max(0, Math.min(box.bottom, innerHeight) - Math.max(box.top, navbar)); }, 0);
        measurements.rowShare = Math.round(100 * rowArea / (innerHeight - navbar));
        assert(measurements.rowsStartingInViewport >= 1, 'a row starts inside the viewport on a landscape phone with the bar at the top');
        assert(measurements.rowShare >= minLandscapeRowShare, 'rows fill at least ' + minLandscapeRowShare + '% of the landscape viewport below the navbar (' + measurements.rowShare + '%)');
        if (innerHeight >= referenceLandscapeHeight) assert(measurements.rowsStartingInViewport >= 2, 'at least two rows start inside the viewport on a landscape phone (' + measurements.rowsStartingInViewport + ')');
        header.scrollIntoView({ block: 'start', behavior: 'instant' });
        await frames(2);
      }
    }

    assert(typeof openStageTrackerFilterSheet === 'function', 'openStageTrackerFilterSheet is wired');
    if (typeof openStageTrackerFilterSheet === 'function' && toolbar) {
      openStageTrackerFilterSheet();
      await frames(2);
      const overlay = document.getElementById('activity-monitor-filters-overlay');
      const sheet = overlay && overlay.querySelector('.hive-dialog.modal-sheet.ham-filter-sheet');
      assert(!!sheet, 'Filters opens a .hive-dialog.modal-sheet');
      if (sheet) {
        assert(sheet.contains(toolbar) && sheet.contains(labels), 'sheet carries the toolbar and label chips');
        assert(more.getAttribute('aria-expanded') === 'true', '⋯ Filters reports aria-expanded while the sheet is open');
        const controls = [...sheet.querySelectorAll('select, input, button, .ham-chip')].filter(visible);
        measurements.sheetControls = controls.length;
        for (const control of controls) {
          const box = rect(control);
          assert(box.right <= innerWidth + 1 && box.left >= -1, 'sheet control stays inside the viewport: ' + (control.id || control.className));
        }
        const inputs = controls.filter(el => el.matches('select, input'));
        for (const input of inputs) {
          assert(parseFloat(getComputedStyle(input).fontSize) >= 16, 'sheet input uses a 16px font so iOS does not zoom: ' + (input.id || input.className));
        }
        if (typeof overlay.hiveDialogClose === 'function') overlay.hiveDialogClose(); else overlay.remove();
        await frames(2);
        assert(!document.getElementById('activity-monitor-filters-overlay'), 'sheet closes');
        assert(section.contains(toolbar) && !toolbar.closest('.hive-dialog'), 'toolbar returns to the section when the sheet closes');
        assert(section.contains(labels) && !labels.closest('.hive-dialog'), 'label chips return to the section when the sheet closes');
        assert(more.getAttribute('aria-expanded') === 'false', '⋯ Filters clears aria-expanded when the sheet closes');
      }
    }
  } finally {
    scrollTo({ left: savedScroll.x, top: savedScroll.y, behavior: 'instant' });
    await frames(1);
  }
  if (failures.length) throw new Error(JSON.stringify({ viewport: innerWidth, measurements, failures }));
  return { viewport: innerWidth, measurements, failures };
})
