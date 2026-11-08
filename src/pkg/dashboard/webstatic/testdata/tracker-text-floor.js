// Dedicated preview only: evaluate (<file contents>)({ dedicatedPreview: true }).
// On phones every rendered text node in the Activity monitor must be >= 12px (#17164).
(async ({ dedicatedPreview = false } = {}) => {
  if (!dedicatedPreview) throw new Error('Use a dedicated published-preview tab, not the user’s live dashboard');
  const root = document.getElementById('activity-monitor-section');
  if (!root) throw new Error(JSON.stringify({ viewport: innerWidth, failures: ['#activity-monitor-section missing'] }));
  const phone = innerWidth <= 640;
  const failures = [];
  const small = [];
  let checked = 0;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node; node = walker.nextNode()) {
    if (!node.nodeValue.trim()) continue;
    const el = node.parentElement;
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden' || !el.getClientRects().length) continue;
    checked++;
    if (parseFloat(style.fontSize) < 12) small.push(el.className + ' ' + style.fontSize);
  }
  if (phone) {
    if (!checked) failures.push('no visible text nodes in the Activity monitor');
    if (small.length) failures.push(small.length + ' text nodes below 12px: ' + [...new Set(small)].slice(0, 10).join('; '));
    if (document.documentElement.scrollWidth > innerWidth) failures.push('horizontal overflow: scrollWidth ' + document.documentElement.scrollWidth + ' > ' + innerWidth);
  }
  if (failures.length) throw new Error(JSON.stringify({ viewport: innerWidth, checked, failures }));
  return { viewport: innerWidth, checked, below12: small.length, failures };
})
