// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Phone tier (hivecommons/hive#17162): with the activity-density fixture
// drawn, every top-level row that needs a person (stageTrackerNeedsAttention
// and not sunk — hold / draft rows keep sinking last, as on desktop) precedes
// every row that does not, and the sticky "⚠ N waiting on you · M need
// attention" line is inside the viewport at rest with the section scrolled
// to its top, is declared sticky like the compact bar above it, is the
// Waiting on you toggle (hamFilter waitingOnYou), and meets the tap target.
// Whether sticky engages while scrolling is the page's scroll-container
// contract, shared with .ham-sticky / .ham-cols, and not asserted here.
// Desktop returns a skip marker. Restores the scroll position.
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const tolerance = 1;
  const failures = [];
  const assert = (ok, message) => { if (!ok) failures.push(message); };
  const frame = () => new Promise(requestAnimationFrame);
  const visible = el => !!(el && el.getClientRects().length && getComputedStyle(el).display !== 'none');
  const phone = matchMedia('(max-width: 640px), ((pointer: coarse) and (max-width: 820px))').matches;
  if (!phone) return { viewport: innerWidth, phone: false, skipped: 'strip tier: column-first ordering, no count line', failures };
  const panel = document.getElementById('activity-monitor-panel');
  const section = document.getElementById('activity-monitor-section');
  if (!panel || !section) throw new Error('#activity-monitor-panel / #activity-monitor-section missing');
  const items = Array.isArray(_stageTrackerRows) ? _stageTrackerRows : [];
  const byKey = new Map(items.map(it => [stageTrackerItemKey(it), it]));
  // Top-level rows in DOM order (nested sub-rows ride their issue).
  const rows = [...panel.querySelectorAll('.ham-row[data-ham-key]')].filter(visible)
    .map(row => ({ key: row.dataset.hamKey, item: byKey.get(row.dataset.hamKey) })).filter(r => r.item);
  assert(rows.length >= 2, 'fixture rows are drawn: ' + rows.length);
  const leads = it => stageTrackerAttentionTier(it) === STAGE_TRACKER_TIER_ATTENTION;
  const order = rows.map(r => (leads(r.item) ? 'A' : stageTrackerNeedsAttention(r.item) ? 'S' : '-'));
  const attentionRows = order.filter(o => o === 'A').length;
  assert(attentionRows > 0, 'fixture carries rows that need attention');
  const lastAttention = order.lastIndexOf('A');
  const firstOther = order.indexOf('-');
  assert(firstOther < 0 || lastAttention < firstOther, 'every needs-attention row precedes every non-attention row: ' + order.join(''));
  // Held / draft rows are needs-attention by modifier but nobody should act
  // on them; they sink last, after the plain rows, as on desktop.
  const lastPlain = order.lastIndexOf('-');
  const firstSunk = order.indexOf('S');
  assert(firstSunk < 0 || lastPlain < 0 || lastPlain < firstSunk, 'held / draft rows sink below the plain rows: ' + order.join(''));

  // Count line: present, sticky, inside the viewport at rest.
  const wrap = document.getElementById('activity-monitor-attn-line');
  const line = wrap && wrap.querySelector('.ham-attn-line');
  assert(visible(line), 'the count line is drawn above the list');
  const scroller = document.scrollingElement || document.documentElement;
  const savedScroll = scroller.scrollTop;
  try {
    section.scrollIntoView({ block: 'start', behavior: 'instant' });
    await frame();
    if (line) {
      const box = line.getBoundingClientRect();
      assert(box.top >= -tolerance && box.bottom <= innerHeight + tolerance, 'count line inside the viewport at rest: top ' + Math.round(box.top) + ' bottom ' + Math.round(box.bottom) + ' of ' + innerHeight);
      assert(getComputedStyle(wrap).position === 'sticky', 'count line wrap is sticky');
      const tapTarget = parseFloat(getComputedStyle(section).getPropertyValue('--tap-target')) || 44;
      assert(box.height >= tapTarget - tolerance, 'count line is at least the tap target tall: ' + Math.round(box.height));
      assert(line.dataset.action === 'hamFilter' && line.dataset.arg0 === 'waitingOnYou', 'count line is the Waiting on you toggle');
      assert(/\d+ waiting on you · \d+ needs? attention/.test(line.textContent), 'count line reads "N waiting on you · M need attention": ' + line.textContent.trim());
      const attentionAll = items.filter(stageTrackerNeedsAttention).length;
      assert(line.textContent.indexOf('· ' + attentionAll + ' need') >= 0, 'count line counts every needs-attention item (' + attentionAll + '): ' + line.textContent.trim());
    }
  } finally {
    scroller.scrollTop = savedScroll;
    await frame();
  }
  const result = { viewport: innerWidth, phone: true, rows: rows.length, attentionRows, order: order.join(''), countLine: line ? line.textContent.trim() : null, failures };
  if (failures.length) throw new Error(JSON.stringify(result));
  return result;
})
