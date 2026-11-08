// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Phone ergonomics gate for the activity monitor (#17158). Asserts only at the
// 390×844 phone viewport (dashboard-mobile-shots.mjs runs probes at every
// viewport); elsewhere it reports { skipped: true } so desktop stays a
// no-diff. Opens and closes the first row's ⋯ menu via its public entry
// point; never activates a menu item or changes rows.
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const PHONE = { width: 390, height: 844 };
  if (innerWidth !== PHONE.width || innerHeight !== PHONE.height) return { skipped: true, viewport: [innerWidth, innerHeight] };
  const section = document.getElementById('activity-monitor-section');
  if (!section) throw new Error('#activity-monitor-section missing');
  const failures = [];
  const assert = (ok, label) => { if (!ok) failures.push(label); };
  const tolerance = 0.5;
  // #17163: Apple HIG / WCAG 2.5.5 coarse-pointer floor, the value base.css
  // publishes as --tap-target-touch and the phone tier remaps --tap-target to.
  const tapTarget = parseFloat(getComputedStyle(section).getPropertyValue('--tap-target')) || 0;
  assert(tapTarget >= 44 - tolerance, `--tap-target on the section is ${tapTarget}px, want ≥ 44px`);
  const visible = el => el.getClientRects().length && getComputedStyle(el).visibility !== 'hidden';
  // Exclusions: inline text links (issue / PR refs, prose anchors) and the
  // inline chevron-word decision triggers flow with their text and cannot
  // be 44px tall without breaking the line (tracker-decision-interaction
  // hit-tests the triggers on their own terms); the
  // chevron-strip info cells fill their strip cell, whose height is the
  // row-strip layout (#17162), not a control size. Everything else is a
  // block-ish control and must meet the floor.
  const inlineText = el => (getComputedStyle(el).display === 'inline' && el.matches('a, button.ham-decision-trigger'))
    || el.matches('a.ham-ref, a.ham-pr-ref');
  const stripCell = el => el.matches('.ham-chevron-info, .ham-chevron-decision, .ham-chevron-copy .ham-decision-trigger');
  const controls = [...section.querySelectorAll('button, a[href], input, select, summary, [role="button"], [role="menuitem"], [tabindex]:not([tabindex="-1"]):not(.ham-row):not(.ham-sub)')]
    .filter(el => visible(el) && !inlineText(el) && !stripCell(el));
  // A compact control may extend its hit area with an absolutely positioned
  // empty ::before or ::after (the progress pill, #17183; the inline repo
  // chip and lead action on phone card rows, #17161): negative insets grow
  // the tap target past the painted box. Positive insets (the ?/⚙ ring drawn
  // inside the --tap-target square) never shrink it. Layout size, not the
  // client rect, so a one-shot transform animation (.ham-pill-moved) cannot
  // fail it.
  const hitRect = el => {
    const r = { width: el.offsetWidth || el.getBoundingClientRect().width, height: el.offsetHeight || el.getBoundingClientRect().height };
    if (getComputedStyle(el).position === 'static') return r;
    const grow = v => Math.max(0, -(parseFloat(v) || 0));
    let out = r;
    for (const pseudo of ['::before', '::after']) {
      const b = getComputedStyle(el, pseudo);
      if (b.position !== 'absolute' || b.content !== '""') continue;
      const g = { width: r.width + grow(b.left) + grow(b.right), height: r.height + grow(b.top) + grow(b.bottom) };
      if (g.width >= out.width && g.height >= out.height) out = g;
    }
    return out;
  };
  const small = [];
  for (const el of controls) {
    const r = hitRect(el);
    if (r.width + tolerance < 44 || r.height + tolerance < 44) small.push(`${el.tagName.toLowerCase()}.${[...el.classList].join('.')} ${Math.round(r.width)}×${Math.round(r.height)}`);
  }
  assert(small.length === 0, `${small.length} control(s) under 44×44 at 390×844: ${[...new Set(small)].slice(0, 12).join(', ')}`);
  // Row tools live in the row header, not the (hidden) left gutter.
  const row = section.querySelector('.ham-row');
  const tools = row && row.querySelector(':scope > .ham-row-tools');
  const head = row && row.querySelector(':scope > .ham-row-head');
  assert(!!(tools && head), 'first row has .ham-row-tools and .ham-row-head');
  if (tools && head) {
    const t = tools.getBoundingClientRect(), h = head.getBoundingClientRect(), rr = row.getBoundingClientRect();
    assert(t.left >= rr.left - tolerance, `row tools start inside the row (left ${Math.round(t.left)} vs row ${Math.round(rr.left)})`);
    assert(t.right <= rr.right + tolerance, 'row tools end inside the row');
    assert(t.top >= rr.top - tolerance && t.top <= h.bottom + tolerance, 'row tools sit on the row header line');
    for (const b of tools.querySelectorAll('.ham-sel, .ham-kebab')) assert(parseFloat(getComputedStyle(b).opacity) === 1, `${b.className} is always visible on a phone`);
  }
  // ⋯ opens as a .hive-dialog.modal-sheet (not the anchored popover).
  // Open the first row kebab that has menu targets (fixture rows without a
  // resolvable terminal view have none), through the same click path a
  // thumb takes.
  let kebab = null;
  if (typeof stageTrackerMenuTargets === 'function') {
    for (const k of section.querySelectorAll('.ham-row > .ham-row-tools > .ham-kebab')) {
      let t = null;
      try { t = stageTrackerMenuTargets(k); } catch (e) { t = null; }
      if (t && t.items && t.items.length) { kebab = k; break; }
    }
  }
  let menu = { opened: false };
  if (kebab && typeof stageTrackerOpenMenu === 'function' && typeof stageTrackerCloseMenu === 'function') {
    kebab.click();
    await new Promise(requestAnimationFrame);
    const el = document.querySelector('.ham-menu');
    const sheet = el && el.closest('.hive-dialog.modal-sheet');
    assert(!!el, 'kebab opens a .ham-menu');
    assert(!!sheet, 'phone kebab menu is hosted in a .hive-dialog.modal-sheet');
    if (el) {
      // Collapsed submenu items (e.g. "Decide PR ▸") have no box until opened.
      const items = [...el.querySelectorAll('[role="menuitem"]')].filter(i => i.getClientRects().length);
      const shortItems = items.filter(i => i.getBoundingClientRect().height + tolerance < 44).length;
      assert(shortItems === 0, `${shortItems} menu item(s) shorter than 44px`);
      const r = (sheet || el).getBoundingClientRect();
      assert(r.left >= -tolerance && r.right <= innerWidth + tolerance, 'menu sheet fits the viewport width');
      menu = { opened: true, sheet: !!sheet, items: items.length };
    }
    stageTrackerCloseMenu(false);
    await new Promise(requestAnimationFrame);
    assert(!document.querySelector('.ham-menu'), 'menu closes cleanly');
  } else assert(false, 'kebab button and menu entry points present');
  if (failures.length) throw new Error(JSON.stringify({ probe: 'tracker-phone-ergonomics', failures }));
  return { ok: true, tapTarget, controls: controls.length, menu };
})
