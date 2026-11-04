// Serialized into the existing harness's isolated file:// Chrome page only.
export function waitForDashboardPaint({ dedicatedPreview = false, timeoutMs = 10000, stableFrames = 2 } = {}) {
  if (!dedicatedPreview) throw new Error('Render readiness requires a dedicated preview page');
  return new Promise((resolve, reject) => {
    const started = performance.now();
    let frame, last = null, previous = '', stable = 0;
    const finish = (error, value) => {
      clearTimeout(timer);
      cancelAnimationFrame(frame);
      if (error) reject(error); else resolve(value);
    };
    const timer = setTimeout(() => finish(new Error(JSON.stringify({
      failures: ['Dashboard render readiness timed out'], timeoutMs, last,
    }))), timeoutMs);
    const sample = () => {
      const body = document.body, root = document.documentElement;
      const bodyStyle = body && getComputedStyle(body), rootStyle = root && getComputedStyle(root);
      const box = body?.getBoundingClientRect();
      last = { readyState: document.readyState, visibilityState: document.visibilityState,
        opacity: bodyStyle?.opacity, visibility: bodyStyle?.visibility, display: bodyStyle?.display,
        rootOpacity: rootStyle?.opacity, rootVisibility: rootStyle?.visibility, rootDisplay: rootStyle?.display,
        width: box?.width, height: box?.height };
      const ready = document.readyState !== 'loading' && document.visibilityState === 'visible' &&
        last.opacity === '1' && last.visibility === 'visible' && last.display !== 'none' &&
        last.rootOpacity === '1' && last.rootVisibility === 'visible' && last.rootDisplay !== 'none' &&
        last.width > 0 && last.height > 0;
      const signature = JSON.stringify(last);
      stable = ready ? (signature === previous ? stable + 1 : 1) : 0;
      previous = signature;
      if (stable >= stableFrames) {
        finish(null, { ready: true, elapsedMs: performance.now() - started, stableFrames: stable, state: last });
      } else {
        frame = requestAnimationFrame(sample);
      }
    };
    frame = requestAnimationFrame(sample);
  });
}

export async function resetDashboardFixtureOverlays({ dedicatedPreview = false, maxDismissals = 32, settleTimeoutMs = 1000 } = {}) {
  if (!dedicatedPreview) throw new Error('Fixture cleanup requires a dedicated preview page');
  if (typeof hiveDismissTopOverlay !== 'function' || typeof hiveOverlayIsOpen !== 'function') {
    throw new Error('Native dashboard overlay cleanup is unavailable');
  }
  // These are the three modal producers in the legacy screenshot sequence.
  const ownedSelector = '#config-overlay, #kb-modal-overlay, .hive-dialog-overlay';
  const remaining = () => [...document.querySelectorAll(ownedSelector)]
    .filter(overlay => hiveOverlayIsOpen(overlay))
    .map(overlay => overlay.id || overlay.className);
  let dismissals = 0;
  for (; dismissals < maxDismissals; dismissals++) {
    if (!hiveDismissTopOverlay()) break;
  }
  if (dismissals === maxDismissals) {
    throw new Error(JSON.stringify({ failures: ['Native overlay cleanup exceeded its dismissal bound'],
      maxDismissals, remaining: remaining() }));
  }
  // Let the production modal observer release its root/body scroll lock.
  await new Promise((resolve, reject) => {
    const frame = requestAnimationFrame(() => { clearTimeout(timer); resolve(); });
    const timer = setTimeout(() => {
      cancelAnimationFrame(frame);
      reject(new Error('Native fixture cleanup observer did not settle before timeout'));
    }, settleTimeoutMs);
  });
  const state = { dismissals, remaining: remaining(),
    scrollLocked: document.documentElement.classList.contains('modal-open') || document.body.classList.contains('modal-open') };
  if (state.remaining.length || state.scrollLocked) {
    throw new Error(JSON.stringify({ failures: ['Prior fixture overlay or modal-open scroll lock survived native cleanup'], ...state }));
  }
  return state;
}
