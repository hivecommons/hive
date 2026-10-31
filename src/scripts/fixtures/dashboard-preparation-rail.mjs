// Runs against the generated SPA in the existing Chrome screenshot harness.
export function preparationRailLayoutFixture() {
  const nowMs = Date.parse('2026-10-31T12:00:00Z');
  const repo = 'fixture/preparation';
  const failures = [], geometry = [];
  const check = (ok, message) => { if (!ok) failures.push(message); };
  const host = document.createElement('div');
  host.id = 'preparation-rail-layout-fixture';
  const saved = [_stageTrackerPreparationShown, _stageTrackerPreparationRendered, _stageTrackerPreparationInline, _stageTrackerPreparationNow];
  const pr = (number, stage) => ({ kind: 'pr', repo, number, stage, state: 'open',
    head: 'head-' + number, base_ref: 'v5', title: 'Preparation connector layout fixture',
    url: 'https://github.com/' + repo + '/pull/' + number,
    caption: ('Current-head CI and review remain pending; this expanded caption must stay inside the preparation connector. ').repeat(12) });
  const ticket = (item, role) => ({ number: item.number, role, state: 'current', head_sha: item.head,
    current_head: item.head, since: '2026-10-31T11:00:00Z', pipeline: item.caption });
  const append = (items, owner, waiting) => {
    const queue = { repo, base_ref: 'v5', state: 'observed', owner: ticket(owner, 'owner'), waiting: waiting.map(p => ticket(p, 'waiting')) };
    const data = { items, base_preparation: [queue] };
    stageTrackerPreparationVisualSync(data, items, items, [], nowMs);
    const ctx = { nowMs, items, events: {}, noteTuck: false, sharedPRs: {}, workers: {} };
    const rows = items.map((item, i) => ({ key: stageTrackerItemKey(item),
      html: stageTrackerRowHtml(item, [], { ...ctx, prevRow: items[i - 1], nextRow: items[i + 1] }) }));
    const block = document.createElement('div');
    block.innerHTML = stageTrackerPreparationInlineRows(rows, items, []).map(row => row.html).join('');
    host.appendChild(block);
    return block;
  };
  const rail = el => {
    const rect = el.getBoundingClientRect(), style = getComputedStyle(el), pseudo = getComputedStyle(el, '::before');
    const num = value => Number.parseFloat(value) || 0;
    return { top: rect.top + num(style.borderTopWidth) + num(pseudo.top),
      bottom: rect.bottom - num(style.borderBottomWidth) - num(pseudo.bottom),
      x: rect.left + num(style.borderLeftWidth) + num(pseudo.left),
      border: pseudo.borderLeftStyle, rowHeight: rect.height, content: pseudo.content };
  };
  const inspect = (block, selector, name, count) => {
    const members = [...block.querySelectorAll(selector)];
    check(members.length === count, name + ': exact member count');
    check(block.querySelectorAll('.ham-preparation-head').length === 1, name + ': one header');
    check(!block.textContent.includes('continuation'), name + ': no repeated continuation');
    const lines = members.map(rail);
    geometry.push({ name, lines });
    for (let i = 0; i < members.length; i++) {
      check(lines[i].content !== 'none' && lines[i].border === 'solid', name + ': original solid connector ' + i);
      check(members[i].classList.contains('ham-train-last') === (i === members.length - 1), name + ': only final member ends rail');
      if (i === members.length - 1) continue;
      check(lines[i].bottom >= lines[i + 1].top, name + ': rail crosses caption, padding and inter-row gap');
      check(Math.abs(lines[i].x - lines[i + 1].x) < 1, name + ': aligned rail gutter');
      const caption = members[i].querySelector('.ham-preparation-comment');
      check(caption && caption.getBoundingClientRect().height > 0, name + ': real expanded caption');
      if (caption) check(lines[i].bottom >= caption.getBoundingClientRect().bottom, name + ': rail spans full caption');
    }
  };
  try {
    document.getElementById(host.id)?.remove();
    const owner = pr(45, 70), first = pr(9, 50), second = pr(2, 70);
    const roots = append([second, first, owner], owner, [first, second]);
    const nestedOwner = pr(145, 70), nestedWaiter = pr(109, 50), unrelated = pr(180, 90);
    const issue = { kind: 'issue', repo, number: 500, stage: 40, title: 'Preserved mixed issue tree',
      linked_prs: [unrelated, nestedOwner, nestedWaiter] };
    const nested = append([issue], nestedOwner, [nestedWaiter]);
    document.body.appendChild(host);
    inspect(roots, '.ham-row.ham-preparation-member', 'root reverse-FIFO rows', 3);
    inspect(nested, '.ham-sub.ham-preparation-member', 'nested PRs with expanded captions', 2);
    const nonmember = nested.querySelector('[data-ham-sub-key="' + stageTrackerItemKey(unrelated) + '"]');
    check(nonmember && !nonmember.classList.contains('ham-preparation-member'), 'unrelated child has no preparation membership');
    host.scrollIntoView();
  } catch (error) {
    failures.push(String(error.stack || error));
  } finally {
    [_stageTrackerPreparationShown, _stageTrackerPreparationRendered, _stageTrackerPreparationInline, _stageTrackerPreparationNow] = saved;
  }
  return { failures, geometry };
}
