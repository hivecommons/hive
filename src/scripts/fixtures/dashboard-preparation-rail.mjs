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
    items = stageTrackerPreparationOrderRows(data, items, []);
    stageTrackerPreparationVisualSync(data, items, items, [], nowMs);
    const ctx = { nowMs, items, events: {}, noteTuck: false, sharedPRs: {}, workers: {} };
    const rows = items.map((item, i) => ({ key: stageTrackerItemKey(item),
      html: stageTrackerRowHtml(item, [], { ...ctx, prevRow: items[i - 1], nextRow: items[i + 1] }) }));
    const block = document.createElement('div');
    const rendered = stageTrackerPreparationInlineRows(rows, items, []);
    block.innerHTML = rendered.map(row => row.html).join('');
    // The production reconciler stamps keys separately from the row renderer.
    [...block.children].forEach((element, index) => { element.dataset.hamKey = rendered[index].key; });
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
      const box = members[i].getBoundingClientRect();
      check(Math.abs(lines[i].bottom - box.bottom) < 1, name + ': full member height including final caption and padding');
      const node = members[i].querySelector(':scope > .ham-strip-row > .ham-preparation-node');
      const strip = members[i].querySelector(':scope > .ham-strip-row');
      if (node && strip) {
        const dot = getComputedStyle(node, '::after'), hit = node.getBoundingClientRect(), anchor = strip.getBoundingClientRect();
        const center = hit.top + Number.parseFloat(dot.top) + Number.parseFloat(dot.height) / 2;
        check(Math.abs(center - (anchor.top + anchor.height / 2)) < 1, name + ': dot stays on item anchor, not comment midpoint');
      } else check(false, name + ': actual member dot belongs to its strip anchor');
      const caption = members[i].querySelector('.ham-preparation-comment');
      check(caption && caption.getBoundingClientRect().height > 0, name + ': real expanded caption');
      if (caption) check(lines[i].bottom >= caption.getBoundingClientRect().bottom, name + ': rail spans full caption');
      if (i === members.length - 1) continue;
      check(lines[i].bottom >= lines[i + 1].top, name + ': rail crosses caption, padding and inter-row gap');
      check(Math.abs(lines[i].x - lines[i + 1].x) < 1, name + ': aligned rail gutter');
    }
  };
  try {
    document.getElementById(host.id)?.remove();
    const owner = pr(45, 70), first = pr(9, 50), second = pr(2, 70);
    const roots = append([second, first, owner], owner, [first, second]);
    const singleOwner = pr(245, 50), single = append([singleOwner], singleOwner, []);
    const nestedOwner = pr(145, 70), nestedWaiter = pr(109, 50), unrelated = pr(180, 90);
    const issue = { kind: 'issue', repo, number: 500, stage: 40, title: 'Preserved mixed issue tree',
      linked_prs: [unrelated, nestedOwner, nestedWaiter] };
    const nested = append([issue], nestedOwner, [nestedWaiter]);
    document.body.appendChild(host);
    inspect(roots, '.ham-row.ham-preparation-member', 'root reverse-FIFO rows', 3);
    check(roots.querySelector('.ham-preparation-member')?.dataset.hamKey === stageTrackerItemKey(owner), 'verified front is first despite scrambled source sort');
    inspect(nested, '.ham-sub.ham-preparation-member', 'nested PRs with expanded captions', 2);
    inspect(single, '.ham-row.ham-preparation-member', 'single-member train with multiline caption', 1);
    for (const [block, selector, name, count] of [
      [roots, '.ham-row.ham-preparation-member', 'last root dynamic comment', 3],
      [nested, '.ham-sub.ham-preparation-member', 'last nested dynamic comment', 2],
      [single, '.ham-row.ham-preparation-member', 'single dynamic comment', 1],
    ]) {
      const last = [...block.querySelectorAll(selector)].at(-1), before = last.getBoundingClientRect().height;
      const comment = document.createElement('div');
      comment.className = 'cue-event ham-preparation-comment';
      comment.textContent = 'New live commentary must extend the final rail without moving the member anchor. '.repeat(24);
      last.appendChild(comment);
      check(last.getBoundingClientRect().height > before, name + ': layout really grew after commentary update');
      inspect(block, selector, name, count);
    }
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
