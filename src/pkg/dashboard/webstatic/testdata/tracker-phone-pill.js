// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Phone tier (hivecommons/hive#17160): the Activity monitor row shows a
// progress pill instead of the chevron strip. Asserts the pill contract at
// the phone viewport and that tapping the pill opens the full strip in a
// .hive-dialog.modal-sheet; then plays one row move and checks the cue
// lands on the pill (no overlay geometry against a hidden strip, no error).
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const tolerance = 1;
  const failures = [];
  const assert = (ok, message) => { if (!ok) failures.push(message); };
  const frame = () => new Promise(requestAnimationFrame);
  const visible = el => el && el.getClientRects().length && getComputedStyle(el).display !== 'none';
  const phone = matchMedia('(max-width: 640px), ((pointer: coarse) and (max-width: 820px))').matches;
  if (!phone) return { viewport: innerWidth, phone: false, skipped: 'strip tier: the chevron strip is drawn, see tracker-header-geometry', failures };
  const panel = document.getElementById('activity-monitor-panel');
  const rows = [...panel.querySelectorAll('.ham-row, .ham-sub')].filter(visible);
  const pills = rows.map(row => row.querySelector(':scope > .ham-strip-row > .ham-pill')).filter(visible);
  assert(pills.length > 0, 'phone: rows render progress pills');
  const tapTarget = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--tap-target')) || 40;
  for (const [index, pill] of pills.entries()) {
    const row = pill.closest('.ham-row, .ham-sub');
    const box = pill.getBoundingClientRect(), bounds = row.getBoundingClientRect();
    assert(box.left >= bounds.left - tolerance && box.right <= bounds.right + tolerance, 'pill ' + index + ' inside its row');
    const hitPad = Math.max(0, -parseFloat(getComputedStyle(pill, '::before').top) || 0);
    assert(box.height + 2 * hitPad >= tapTarget - tolerance, 'pill ' + index + ' hit area is at least the tap target tall: ' + (box.height + 2 * hitPad));
    assert(parseFloat(getComputedStyle(pill).fontSize) >= 12, 'pill ' + index + ' text is at least 12px');
    const stripRow = pill.parentElement;
    assert(stripRow.scrollWidth <= stripRow.clientWidth + tolerance, 'pill ' + index + ' strip row does not scroll sideways');
  }
  // Tap → sheet with the full strip.
  const first = pills[0];
  first.scrollIntoView({ block: 'center', behavior: 'instant' });
  await frame();
  const hit = document.elementFromPoint((first.getBoundingClientRect().left + first.getBoundingClientRect().right) / 2, (first.getBoundingClientRect().top + first.getBoundingClientRect().bottom) / 2);
  assert(hit === first || first.contains(hit), 'pill centre hits the pill, not an overlay: ' + (hit?.className || hit?.tagName || null));
  first.click();
  await frame(); await frame();
  const sheet = document.querySelector('.hive-dialog-overlay .hive-dialog.modal-sheet.ham-pill-sheet');
  assert(!!sheet && visible(sheet), 'tapping the pill opens a .hive-dialog.modal-sheet');
  if (sheet) {
    const strip = sheet.querySelector('.ham-strip');
    assert(visible(strip), 'sheet shows the chevron strip');
    assert(!!strip && strip.querySelectorAll('.ham-seg').length >= 2 && !!strip.querySelector('.ham-seg.active'), 'sheet strip carries the row’s chevrons with the lit step');
    const sb = sheet.getBoundingClientRect();
    assert(sb.width >= innerWidth - tolerance, 'sheet spans the phone viewport');
    const close = sheet.querySelector('.modal-sheet-close');
    assert(visible(close), 'sheet has the ✕ close');
    (close || sheet.querySelector('.primary')).click();
    await frame(); await frame();
    assert(!document.querySelector('.ham-pill-sheet'), 'sheet closes');
  }
  // Row move cue: stamp a move on a strip and play it once.
  const strip = first.parentElement.querySelector(':scope > ol[aria-label="Pipeline progress"]');
  const errors = [];
  const onError = event => errors.push(String(event.error || event.message));
  addEventListener('error', onError);
  let played = 0;
  try {
    strip.dataset.hamPrevCol = '1'; strip.dataset.hamCol = String(Math.max(2, [...strip.children].findIndex(c => c.classList.contains('active'))));
    played = stageTrackerAnimateMoves(panel);
  } catch (error) { errors.push(String(error)); }
  await frame();
  removeEventListener('error', onError);
  assert(errors.length === 0, 'row move on a phone raises no tracker-motion error: ' + errors.join('; '));
  assert(played === 1, 'row move cue fires once per move: ' + played);
  assert(first.classList.contains('ham-pill-moved'), 'row move cue lands on the pill');
  assert(!first.parentElement.querySelector(':scope > .ham-anim-layer') || !visible(first.parentElement.querySelector(':scope > .ham-anim-layer')), 'no overlay is drawn against the hidden strip');
  if (failures.length) throw new Error(JSON.stringify({ viewport: innerWidth, phone: true, pills: pills.length, failures }));
  return { viewport: innerWidth, phone: true, pills: pills.length, failures };
})
