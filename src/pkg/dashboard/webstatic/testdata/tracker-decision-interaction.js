// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// Exercises pointer/focus/keyboard UI, never clicks a row/decision action.
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const tolerance = 1;
  const hoverSettleMS = 200;
  const failures = [];
  const assert = (ok, message) => { if (!ok) failures.push(message); };
  const triggers = [...document.querySelectorAll('#activity-monitor-panel .ham-chevron-decision .ham-decision-trigger')];
  const nativeStage = trigger => Number(trigger.closest('[data-ham-stage]')?.dataset.hamStage);
  const gate = triggers.find(trigger => nativeStage(trigger) === 80 && trigger.querySelector('.ham-seg-t'));
  if (!gate) throw new Error('Preview fixture must include native stage 80 with a gate caption and claim-conflict decision');
  const other = triggers.find(trigger => trigger !== gate && nativeStage(trigger) !== 80);
  const targets = other ? [gate, other] : [gate];
  const help = document.querySelector('#activity-monitor-panel .ham-chevron-info.config-info');
  if (!help) throw new Error('Preview fixture must include an ordinary chevron tooltip to test mutual exclusion');
  const originalFocus = document.activeElement;
  const scrollParents = new Map();
  for (const target of [...targets, help]) {
    for (let parent = target.parentElement; parent; parent = parent.parentElement) {
      if (!scrollParents.has(parent) && (parent.scrollHeight > parent.clientHeight || parent.scrollWidth > parent.clientWidth)) {
        scrollParents.set(parent, { left: parent.scrollLeft, top: parent.scrollTop });
      }
    }
  }
  const frame = () => new Promise(requestAnimationFrame);
  const close = () => document.body.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, pointerType: 'mouse' }));
  const portalOpen = () => {
    const portal = document.getElementById('config-tooltip-portal');
    return portal && !portal.hidden && getComputedStyle(portal).display !== 'none';
  };
  const hit = (element, label) => {
    const box = element.getBoundingClientRect();
    assert(box.width > 0 && box.height > 0 && box.left >= -tolerance && box.right <= innerWidth + tolerance &&
      box.top >= -tolerance && box.bottom <= innerHeight + tolerance, label + ' is wholly reachable in the viewport');
    const actual = document.elementFromPoint((box.left + box.right) / 2, (box.top + box.bottom) / 2);
    assert(actual === element || element.contains(actual), label + ' is not intercepted by a tooltip or other overlay');
  };
  const inspectDialog = (trigger, mode) => {
    const dialogs = [...document.querySelectorAll('.ham-decision-pop')];
    assert(dialogs.length === 1, mode + ': exactly one decision dialog');
    assert(!portalOpen(), mode + ': no competing shared-tooltip portal');
    assert(trigger.getAttribute('aria-expanded') === 'true', mode + ': native trigger owns the dialog');
    const dialog = dialogs[0];
    if (!dialog) return null;
    assert(dialog.getAttribute('role') === 'dialog', mode + ': dialog semantics retained');
    const actions = [...dialog.querySelectorAll('.ham-decision-act')];
    assert(actions.length > 0, mode + ': decision actions remain present');
    for (const action of actions) hit(action, mode + ': ' + action.textContent.trim());
    const details = dialog.querySelector('.ham-decision-details');
    assert(!!details?.querySelector('summary'), mode + ': full overflow details have a keyboard-accessible disclosure');
    const fullTip = trigger.dataset.hamTip || '';
    // The existing dialog deduplicates sentences and normalizes separators.
    const words = fullTip.toLowerCase().match(/[\p{L}\p{N}_@/#-]+/gu) || [];
    const dialogWords = new Set(dialog.textContent.toLowerCase().match(/[\p{L}\p{N}_@/#-]+/gu) || []);
    assert(words.length > 0 && words.every(word => dialogWords.has(word)), mode + ': unclamped caption/head/reasons survive in the sole dialog');
    return dialog;
  };
  try {
    for (const trigger of targets) {
      close();
      trigger.scrollIntoView({ block: 'center', inline: 'nearest', behavior: 'instant' });
      await frame();
      hit(trigger, 'decision trigger');
      assert(!trigger.closest('.config-info') && !trigger.closest('.ham-chevron-info').querySelector('.config-tooltip'), 'decision has no generic tooltip ancestor/source');
      help.focus({ preventScroll: true });
      assert(portalOpen(), 'ordinary tooltip is open before testing decision takeover');
      trigger.dispatchEvent(new PointerEvent('pointerover', { bubbles: true, pointerType: 'mouse' }));
      trigger.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
      assert(!portalOpen(), 'pointer decision opening immediately cancels the previous shared tooltip');
      await new Promise(resolve => setTimeout(resolve, hoverSettleMS));
      inspectDialog(trigger, 'pointer hover');
      close();
      trigger.focus({ preventScroll: true });
      inspectDialog(trigger, 'keyboard focus');
      for (const key of ['ArrowDown', 'Enter', ' ']) {
        // The real delegated key handler moves focus into the dialog; no
        // synthetic native default or action-button activation is assumed.
        trigger.focus({ preventScroll: true });
        trigger.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true }));
        const dialog = inspectDialog(trigger, 'keyboard ' + JSON.stringify(key));
        assert(!!dialog?.contains(document.activeElement), 'keyboard entry focuses the existing dialog actions');
      }
      const dialog = document.querySelector('.ham-decision-pop');
      const summary = dialog?.querySelector('.ham-decision-details > summary');
      summary?.focus({ preventScroll: true });
      assert(!!summary && document.activeElement === summary, 'overflow disclosure is keyboard reachable');
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    }
  } finally {
    close();
    originalFocus?.focus?.({ preventScroll: true });
    close();
    for (const [el, position] of scrollParents) el.scrollTo({ ...position, behavior: 'instant' });
  }
  if (failures.length) throw new Error(JSON.stringify({ viewport: innerWidth, tested: targets.length, failures }));
  return { viewport: innerWidth, tested: targets.length, failures };
})
