// The server owns consent, diffs and receipts. Browser state is only presentation.
const treeRoot = document.getElementById('contribution-tree');
const applyButton = document.getElementById('tree-apply');
const discardButton = document.getElementById('tree-discard');
const receiptText = document.getElementById('tree-receipt');
let review = null, treeBusy = false;
const collapsed = new Set();
function hasTreeEdits() {
  const d = review && review.diff;
  return !!(d && ((d.membership || []).length || (d.enabled || []).length || (d.hub_follow || []).length));
}
function unavailable(button, value) {
  // Keep the initiating button focused while announcing results.
  button.disabled = false; button.setAttribute('aria-disabled', String(value));
}
function treeControls() {
  unavailable(applyButton, treeBusy || !hasTreeEdits() || review.diff.stale);
  unavailable(discardButton, treeBusy || !review || review.projection_pending || (!hasTreeEdits() && !review.diff.stale));
  unavailable(document.getElementById('tree-refresh'), treeBusy || !!(review && review.projection_pending));
  treeRoot.setAttribute('aria-busy', String(treeBusy));
}
const SAVED_TEXT = {
  'profiles-and-projection-saved': 'your choices are written to profiles.yml and contributor.env.',
  'profiles-saved/projection-failed': 'profiles.yml was written but contributor.env was not. Press Apply again to retry.',
  'not-saved': 'nothing was saved.',
  'unchanged': 'nothing changed, so nothing was saved.'
};
const RELOAD_TEXT = {
  'notified': 'the running relay was asked to reload your choices.',
  'pending-failed': 'the running relay could not be reached. Your choices apply the next time it starts.',
  'no-managed-relay-found': 'no running relay was found, so your choices apply the next time it starts.',
  'not-requested': 'a relay reload was not requested.'
};
const ACK_TEXT = {
  'pending': 'not confirmed yet. The relay has not reported that it adopted this change.',
  'unknown': 'unknown.'
};
// Plain sentence first, the machine value in brackets so it can still be quoted in a bug report.
function describeReceipt(saved, reload, ack) {
  // The server may append " — explanation" to a code; key on the code alone.
  const code = v => String(v).split(' — ')[0];
  return 'Saved: ' + (SAVED_TEXT[code(saved)] || saved) + ' [' + code(saved) + ']' +
    '\nRelay reload: ' + (RELOAD_TEXT[code(reload)] || reload) + ' [' + code(reload) + ']' +
    '\nAcknowledged: ' + (ACK_TEXT[code(ack)] || ack) + ' [' + code(ack) + ']' +
    '\nChange saved does not prove relay adoption. In-flight tasks finish under their original lease.';
}
function diffText(d) {
  const at = t => t.repository + ' on ' + t.subscription, lines = [];
  (d.selected || []).forEach(t => lines.push('+ select ' + at(t)));
  (d.deselected || []).forEach(t => lines.push('− deselect ' + at(t)));
  (d.exclusions || []).forEach(x => {
    lines.push((x.excluded ? '⊘ exclude ' : '○ clear exclusion of ') + x.repository + ' (source: ' + (x.sources || []).join(', ') + ')');
    if((x.scoped_subscriptions || []).length) lines.push('    also affects scoped Hives: ' + x.scoped_subscriptions.join(', '));
    if((x.legacy_unrestricted || []).length) lines.push('    unrestricted Hives that can still receive it: ' + x.legacy_unrestricted.join(', '));
  });
  (d.follow || []).forEach(f => lines.push((f.after ? '↻ follow future repositories on ' : '■ stop following future repositories on ') + f.target));
  (d.enabled || []).forEach(e => lines.push((e.after ? '▶ enable ' : '⏸ disable ') + e.target));
  (d.hub_follow || []).forEach(h => lines.push('hub ' + h.hub + ': follow ' + h.before + ' → ' + h.after));
  (d.legacy_conversions || []).forEach(n => lines.push('restrict unrestricted Hive ' + n + ' to its reviewed repositories'));
  (d.shared_branches || []).forEach(s => lines.push('shared: ' + s.subscription + ' also appears under hubs ' + (s.hubs || []).join(', ')));
  return lines.join('\n');
}
async function treeRequest(path, change={}) {
  const response = await fetch(path, {method:'POST', headers:{...headers, 'If-Match':review.revision}, body:JSON.stringify({session:review.session, version:review.version, ...change})});
  let data;
  if((response.headers.get('Content-Type') || '').includes('application/json')) data = await response.json();
  else { const text = await response.text(); if(response.status === 409) { review.diff.stale = true; treeControls(); } throw new Error(text); }
  if(data.tree) { review = data; renderTree(); }
  if(data.receipt) {
    receiptText.textContent = describeReceipt(data.receipt.saved, data.receipt.relay_reload, data.receipt.acknowledged);
  }
  if(data.complete) {
    review = null;
    await loadTree();
    await load();
  }
  if(!response.ok) throw new Error(data.error || 'Contribution request failed.');
  return data;
}
async function loadTree(discover=false) {
  // Refreshing an active review preserves its frozen membership and edits.
  const query = review ? '?session=' + encodeURIComponent(review.session) : (discover ? '?discover=1' : '');
  const response = await fetch('/api/tree' + query, {headers});
  if(!response.ok) throw new Error(await response.text());
  review = await response.json(); renderTree();
}
async function editTree(change) {
  if(treeBusy || !review) return;
  if(review.diff.stale) { say('Discard the stale review and review again before editing.','error'); return; }
  treeBusy = true; treeControls();
  try { await treeRequest('/api/tree/diff', change); say('Staged for review. Nothing saved.'); }
  catch(e) { say(e.message, 'error'); }
  finally { treeBusy = false; treeControls(); }
}
function node(tag, text, cls) {
  const e = document.createElement(tag); if(text !== undefined) e.textContent = text; if(cls) e.className = cls; return e;
}
function button(text, action, key, context) {
  const b = node('button', text); b.type = 'button'; b.dataset.focusKey = key; b.onclick = action;
  // Same words first (WCAG label-in-name), then what the button acts on, so repeated buttons are distinguishable.
  if(context) b.setAttribute('aria-label', text + ': ' + context);
  return b;
}
function selectionText(s) {
  if(s.legacy_unrestricted) return 'Unrestricted — legacy: can receive work from ' + (s.repositories ? 'all ' + s.repositories + ' repositories' : 'every repository it offers') + ' until you restrict it' + (s.membership_incomplete ? '; Membership incomplete' : '');
  return ({off:'Off', partial:'Partial', 'all-current':'All current'}[s.state] || s.state) +
    '; ' + s.selected + '/' + s.repositories + ' selected; ' + s.excluded + ' excluded; ' + s.unreviewed + ' unreviewed' +
    (s.membership_incomplete ? '; Membership incomplete' : '');
}
// A hub or the Direct Hives group must not let one unrestricted Hive describe its restricted siblings.
function groupText(s, members) {
  const legacy = members.filter(m => m.selection.legacy_unrestricted);
  if(!legacy.length) return selectionText(s);
  const restricted = members.filter(m => !m.selection.legacy_unrestricted);
  const sel = restricted.reduce((n, m) => n + m.selection.selected, 0), total = restricted.reduce((n, m) => n + m.selection.repositories, 0);
  const parts = [];
  if(restricted.length) parts.push(sel + '/' + total + ' selected in ' + restricted.length + ' restricted Hive' + (restricted.length > 1 ? 's' : ''));
  parts.push(legacy.length + ' Hive' + (legacy.length > 1 ? 's' : '') + ' unrestricted — legacy (can receive all of their work)');
  if(s.membership_incomplete) parts.push('Membership incomplete');
  return parts.join('; ');
}
function visibleRepoCount() { return visibleItems().filter(i => !i.hasAttribute('aria-expanded')).length; }
function matchingCandidates() {
  const q = document.getElementById('tree-search').value.toLowerCase();
  if(!q || !review) return [];
  const seen = new Set(), out = [];
  for(const sub of review.tree.subscriptions || []) {
    if(sub.selection.legacy_unrestricted) continue;
    for(const repo of sub.repositories) {
      const k = sub.id + '\u0000' + repo.id;
      if(seen.has(k) || repo.selected || (repo.excluded_by || []).length || repo.offered === 'no' || !repo.id.toLowerCase().includes(q)) continue;
      seen.add(k); out.push({sub: sub.id, repo: repo.id});
    }
  }
  return out;
}
function visibleItems() { return Array.from(treeRoot.querySelectorAll('[role=treeitem]')).filter(e => !e.closest('[hidden]')); }
let activeRow = null;
// Roving tab stop: Tab visits one row's controls instead of every button in the tree (92 buttons for 6 Hives).
// Arrow keys move between rows; screen-reader browse modes still list every control.
function activateRow(item) {
  treeRoot.querySelectorAll('[role=treeitem]').forEach(e => e.tabIndex = -1);
  treeRoot.querySelectorAll('.tree-row button, .tree-row input').forEach(c => c.tabIndex = -1);
  activeRow = item || null;
  if(!item) return;
  item.tabIndex = 0;
  item.querySelectorAll(':scope > .tree-row button, :scope > .tree-row input').forEach(c => c.tabIndex = 0);
}
function focusItem(item) {
  if(!item) return;
  activateRow(item); item.focus();
}
const nodeInfo = new WeakMap();
// "Accept all new repositories" is the Hive's follow flag. Unrestricted — legacy Hives cannot follow until restricted.
function followState(subs) {
  const eligible = subs.filter(s => !s.selection.legacy_unrestricted);
  const on = eligible.filter(s => s.follow_new_repositories).length;
  return {eligible, on, state: !eligible.length ? 'na' : on === 0 ? 'off' : on === eligible.length ? 'on' : 'mixed'};
}
// New = offered, not yet chosen, not vetoed, under a restricted Hive.
function newRepos(subs) {
  const out = [];
  for(const s of subs) {
    if(s.selection.legacy_unrestricted) continue;
    for(const r of s.repositories) if(r.unreviewed && !(r.excluded_by || []).length && r.offered !== 'no') out.push({sub: s.id, repo: r.id});
  }
  return out;
}
// Individual repository edits through the existing operation; stops if the review went stale.
async function stageList(list) {
  for(const c of list) {
    await editTree({operation:'repository', target:c.sub, repository:c.repo, value:true});
    if(!review || review.diff.stale) return false;
  }
  return true;
}
// Tick-down: a hub's box sets the follow flag on every restricted Hive under it.
async function setFollowAll(subs, value) {
  for(const s of subs.filter(s => !s.selection.legacy_unrestricted && s.follow_new_repositories !== value)) {
    await editTree({operation:'follow', target:s.id, value});
    if(!review || review.diff.stale) return;
  }
}
async function acceptNew(list, where) {
  if(treeBusy || !list.length) return;
  const noun = list.length === 1 ? 'repository' : 'repositories';
  if(!await styledConfirm('Stage selection of ' + list.length + ' new ' + noun + ' in ' + where + '? Excluded repositories are skipped. Nothing is saved until Apply.', 'Stage selection')) return;
  if(await stageList(list)) say('Staged ' + list.length + ' new ' + noun + '. Nothing saved.');
}
function acceptBox(context, fs, key, toggle) {
  const wrap = node('label', undefined, 'accept');
  const c = document.createElement('input'); c.type = 'checkbox'; c.dataset.focusKey = key;
  c.checked = fs.state === 'on'; c.indeterminate = fs.state === 'mixed'; c.disabled = fs.state === 'na';
  c.setAttribute('aria-checked', c.indeterminate ? 'mixed' : String(c.checked));
  c.setAttribute('aria-label', 'Accept all new repositories: ' + context);
  c.onchange = () => { c.checked = fs.state === 'on'; c.indeterminate = fs.state === 'mixed'; toggle(fs.state !== 'on'); };
  wrap.append(c, node('span', 'Accept all new repositories'));
  if(fs.state === 'na') wrap.title = 'Restrict this Hive first: an unrestricted — legacy Hive already receives everything.';
  return wrap;
}
let shownItem = null;
function setInfo(item) {
  const box = document.getElementById('tree-info'), info = item && nodeInfo.get(item);
  if(item === shownItem && box.childNodes.length) return;
  shownItem = item; box.textContent = '';
  if(!info) { box.append(node('p', 'Hover or focus a hub, Hive or repository to see what it means here.', 'muted')); return; }
  box.append(node('p', info.kind, 'info-kind'), node('p', info.title, 'info-title'));
  info.lines.forEach(l => box.append(node('p', l, 'info-line')));
}
function treeItem(parent, key, label, selection, change, branch, text, info) {
  const item = node('li'); item.setAttribute('role', 'treeitem'); item.tabIndex = -1; item.dataset.focusKey = key;
  item.setAttribute('aria-label', label); if(info) nodeInfo.set(item, info);
  const row = node('div', undefined, 'tree-row'); item.appendChild(row); parent.appendChild(item);
  if(branch) {
    item.setAttribute('aria-expanded', String(!collapsed.has(key)));
    row.appendChild(button(collapsed.has(key) ? 'Expand' : 'Collapse', () => expandItem(item), key + ':expand', label));
  }
  const check = document.createElement('input'); check.type = 'checkbox'; check.dataset.focusKey = key + ':select';
  check.checked = selection.state === 'all-current'; check.indeterminate = selection.state === 'partial';
  check.setAttribute('aria-checked', check.indeterminate ? 'mixed' : String(check.checked));
  check.setAttribute('aria-label', 'Select current ' + label);
  check.onchange = () => { check.checked = selection.state === 'all-current'; check.indeterminate = selection.state === 'partial'; editTree({...change, value:selection.state !== 'all-current'}); };
  row.append(check, node('span', label, 'identity'), node('span', text || selectionText(selection), 'tree-meta'));
  const group = node('ul'); group.setAttribute('role','group'); group.hidden = collapsed.has(key);
  if(branch) item.appendChild(group);
  return {item, row, group, check};
}
function expandItem(item, expanded) {
  if(!item.hasAttribute('aria-expanded')) return;
  const value = expanded === undefined ? item.getAttribute('aria-expanded') !== 'true' : expanded;
  if(value) collapsed.delete(item.dataset.focusKey); else collapsed.add(item.dataset.focusKey);
  item.setAttribute('aria-expanded', String(value));
  item.querySelector(':scope > [role=group]').hidden = !value;
  const toggle = item.querySelector(':scope > .tree-row > button');
  toggle.textContent = value ? 'Collapse' : 'Expand';
  toggle.setAttribute('aria-label', toggle.textContent + ': ' + item.getAttribute('aria-label'));
}
function renderTree() {
  if(!review) return;
  const active = document.activeElement;
  const focusKey = active && active.dataset.focusKey;
  const oldItems = visibleItems(), oldIndex = oldItems.indexOf(active && active.closest('[role=treeitem]'));
  treeRoot.textContent = '';
  const search = document.getElementById('tree-search').value.toLowerCase();
  const subscriptions = new Map((review.tree.subscriptions || []).map(s => [s.id,s]));
  for(const hub of review.tree.hubs || []) {
    const hubMatch = (hub.name + ' ' + hub.id).toLowerCase().includes(search);
    const members = (hub.subscriptions || []).map(id => subscriptions.get(id)).filter(Boolean);
    const matches = s => hubMatch || (s.id + ' ' + s.hive_id).toLowerCase().includes(search) || s.repositories.some(r => r.id.toLowerCase().includes(search));
    if(!hubMatch && !members.some(matches)) continue;
    const hkey = JSON.stringify(['hub',hub.id]);
    const hubFollow = followState(members), hubNew = newRepos(members);
    const hubTitle = hub.name + ' (' + hub.id + ')';
    const h = treeItem(treeRoot, hkey, hubTitle, hub.selection, {operation:'current', target:hub.id, hub:true}, true, groupText(hub.selection, members), {kind: 'Hub', title: hubTitle, lines: [
      groupText(hub.selection, members),
      hubFollow.state === 'na' ? 'Accept all new repositories: not available — every Hive here is unrestricted — legacy. Restrict one first.' : 'Accept all new repositories: ' + ({on: 'on for all ' + hubFollow.eligible.length + ' restricted Hives', mixed: 'on for ' + hubFollow.on + ' of ' + hubFollow.eligible.length + ' restricted Hives', off: 'off'}[hubFollow.state]) + '. Ticking the box sets it on every restricted Hive under this hub.',
      hubNew.length ? hubNew.length + ' new ' + (hubNew.length === 1 ? 'repository is' : 'repositories are') + ' waiting for your choice across these Hives.' : 'No new repositories are waiting.',
      'The hub’s own follow policy is separate and unchanged: ' + hub.follow + '.',
      'Readiness and activity: unavailable (no relay evidence).']});
    h.row.append(node('span','Follow: ' + hub.follow + ' · Readiness: unavailable · Activity: unavailable','tree-meta'));
    h.row.append(acceptBox(hub.name, hubFollow, hkey+':accept', value => setFollowAll(members, value)));
    if(hubNew.length) h.row.append(button('Accept ' + hubNew.length + ' new', () => acceptNew(hubNew, hub.name), hkey+':acceptnew', hub.name));
    h.row.append(button('Deselect current', () => editTree({operation:'current',target:hub.id,hub:true,value:false}),hkey+':off', hub.name));
    h.row.append(button('Enable Hives', () => editTree({operation:'enabled',target:hub.id,hub:true,value:true}),hkey+':enable', hub.name));
    h.row.append(button('Disable Hives and stop hub following', () => editTree({operation:'enabled',target:hub.id,hub:true,value:false}),hkey+':disable', hub.name));
    for(const sub of members.filter(matches)) {
      const skey = JSON.stringify([hub.id,sub.id]);
      const subFollow = followState([sub]), subNew = newRepos([sub]), subTitle = sub.id + ' (' + sub.hive_id + ')';
      const s = treeItem(h.group, skey, subTitle, sub.selection, {operation:'current',target:sub.id}, true, undefined, {kind: 'Hive', title: subTitle, lines: [
        selectionText(sub.selection),
        sub.selection.legacy_unrestricted ? 'Unrestricted — legacy: it receives work from every repository it offers whatever the boxes below show. Use “Review and restrict” to choose.' : (sub.follow_new_repositories ? 'Accepting all new repositories: they are chosen automatically when membership is refreshed.' : 'New repositories wait for your choice and get no work until you select them.'),
        subNew.length ? subNew.length + ' new ' + (subNew.length === 1 ? 'repository is' : 'repositories are') + ' waiting for your choice.' : 'No new repositories are waiting.',
        (sub.disabled ? 'Disabled: saved choices are kept but no new work is requested. ' : '') + (sub.shared_across_hubs > 1 ? 'Shared across ' + sub.shared_across_hubs + ' hubs: a change here appears under each of them.' : ''),
        sub.sources && sub.sources.length ? 'Selection sources: ' + sub.sources.join(', ') + ' (per-repository provenance unavailable).' : '',
        'Endpoint: ' + sub.hive_id].filter(Boolean)});
      s.row.append(node('span',(sub.disabled ? 'Disabled; saved scope retained · ' : '') + (sub.shared_across_hubs > 1 ? 'Shared across ' + sub.shared_across_hubs + ' hubs · ' : '') + 'Follow: ' + (sub.follow_new_repositories ? 'on' : 'off') + ' · Readiness: unavailable · Activity: unavailable','tree-meta'));
      s.row.append(button(sub.disabled ? 'Enable Hive' : 'Disable Hive', () => editTree({operation:'enabled',target:sub.id,value:sub.disabled}),skey+':enabled', sub.id));
      s.row.append(acceptBox(sub.id, subFollow, skey+':accept', value => editTree({operation:'follow', target:sub.id, value})));
      if(subNew.length) s.row.append(button('Accept ' + subNew.length + ' new', () => acceptNew(subNew, sub.id), skey+':acceptnew', sub.id));
      if(sub.selection.legacy_unrestricted) {
        s.row.append(button('Review and restrict', async () => {
          if(await styledConfirm('Restrict ' + sub.id + ' to the repositories it offers now? All ' + sub.repositories.length + ' start selected; afterwards use “Deselect current” and tick only the ones you want. Following stays off. Nothing is saved until Apply.', 'Stage restriction')) editTree({operation:'restrict',target:sub.id});
        },skey+':restrict', sub.id));
      } else {
        s.row.append(button('Deselect current', () => editTree({operation:'current',target:sub.id,value:false}),skey+':off', sub.id));
        const add = node('form');
        const input = document.createElement('input'); input.type = 'text'; input.required = true; input.placeholder = 'github/github.com/owner/repo'; input.setAttribute('aria-label','Repository to add to ' + sub.id); input.dataset.focusKey = skey+':add-input';
        const submit = node('button','Stage repository'); submit.type = 'submit'; submit.dataset.focusKey = skey+':add'; submit.setAttribute('aria-label', 'Stage repository: add to ' + sub.id);
        add.append(input,submit); add.onsubmit = e => { e.preventDefault(); editTree({operation:'repository',target:sub.id,repository:input.value,value:true}); }; s.row.appendChild(add);
      }
      if(sub.sources && sub.sources.length) s.row.append(node('span','Selection sources: ' + sub.sources.join(', ') + '; per-repository provenance unavailable','tree-meta'));
      for(const repo of sub.repositories) {
        if(!hubMatch && !(sub.id + ' ' + sub.hive_id).toLowerCase().includes(search) && !repo.id.toLowerCase().includes(search)) continue;
        const rkey = JSON.stringify([hub.id,sub.id,repo.id]);
        const sel = {state:repo.selected ? 'all-current':'off',selected:repo.selected ? 1:0,repositories:1,excluded:(repo.excluded_by || []).length ? 1:0,unreviewed:repo.unreviewed ? 1:0};
        const excl = repo.excluded_by || [];
        const r = treeItem(s.group,rkey,repo.id,sel,{operation:'repository',target:sub.id,repository:repo.id},false,undefined,{kind: 'Repository', title: repo.id, lines: [
          repo.selected ? 'Selected: your machine may receive work from it.' : repo.unreviewed ? 'New: you have not chosen it yet, so it receives no work until you select it.' : 'Not selected.',
          excl.length ? 'Excluded by ' + excl.join(', ') + ': a veto that selecting a parent never overrides.' : '',
          sub.selection.legacy_unrestricted ? 'This Hive is unrestricted — legacy, so it receives this repository whatever the box shows. Restrict the Hive first.' : '',
          'Offered by ' + sub.id + ': ' + repo.offered + '.'].filter(Boolean)});
        r.row.append(node('span','Offered: ' + repo.offered + ' · Readiness: unavailable · Activity: unavailable','tree-meta'));
        if((repo.excluded_by || []).length) r.row.append(node('span','Excluded by ' + repo.excluded_by.join(', '),'pill'));
        if(repo.unreviewed) r.row.append(node('span','Unreviewed','pill'));
        if(!sub.selection.legacy_unrestricted) {
          const excludedHere = (repo.excluded_by || []).includes(sub.id);
          r.row.append(button(excludedHere ? 'Clear this exclusion' : 'Exclude here', () => editTree({operation:'exclude',target:sub.id,repository:repo.id,value:!excludedHere}),rkey+':exclude', repo.id + ' in ' + sub.id));
        }
      }
      if(search) expandItem(s.item,true);
    }
    if(search) expandItem(h.item,true);
  }
  if(!treeRoot.children.length) treeRoot.appendChild(node('li','No matching saved subscriptions. Use profile management to subscribe.'));
  const d = review.diff;
  document.getElementById('diff-summary').textContent = d.stale ? 'Stale review — discard and review again before Apply.' : hasTreeEdits() ? '+' + d.selected_count + ' repositories selected, −' + d.deselected_count + ' deselected, ' + d.excluded_count + ' exclusions added, ' + d.cleared_exclusion_count + ' cleared; follow changes: ' + (d.follow || []).length + '; Hives enabled or disabled: ' + (d.enabled || []).length + '; unrestricted Hives being restricted: ' + (d.legacy_conversions || []).length + '. Open “Named targets and shared effects” for the full list.' : 'No staged changes.';
  document.getElementById('tree-diff').textContent = hasTreeEdits() ? diffText(d) : '';
  document.getElementById('tree-diff-raw').textContent = hasTreeEdits() ? JSON.stringify(d,null,2) : '';
  const candidates = matchingCandidates(), selectMatching = document.getElementById('tree-select-matching');
  selectMatching.hidden = !candidates.length;
  selectMatching.textContent = 'Select ' + candidates.length + ' matching ' + (candidates.length === 1 ? 'repository' : 'repositories');
  // Restore the initiating control, never move focus into a status message.
  const items = visibleItems();
  const restored = focusKey && Array.from(treeRoot.querySelectorAll('[data-focus-key]')).find(e => e.dataset.focusKey === focusKey && !e.closest('[hidden]'));
  if(restored) { restored.closest('[role=treeitem]').tabIndex = 0; restored.focus(); }
  else if(oldIndex >= 0) focusItem(items[Math.min(oldIndex,items.length-1)]);
  else if(items[0]) items[0].tabIndex = 0;
  activateRow((treeRoot.contains(document.activeElement) && document.activeElement.closest('[role=treeitem]')) || items[0]);
  setInfo(activeRow);
  treeControls();
}
const typeahead = {buf: '', timer: 0};
// Explorer-style keys. Text fields keep their own keys; Space/Enter on a focused button or checkbox keep their native action.
treeRoot.addEventListener('keydown', e => {
  const t = e.target, onItem = t.getAttribute('role') === 'treeitem';
  if(!onItem && !(t.tagName === 'BUTTON' || (t.tagName === 'INPUT' && t.type === 'checkbox'))) return;
  const item = t.closest('[role=treeitem]'), items = visibleItems(), index = items.indexOf(item);
  const checkbox = () => item.querySelector(':scope > .tree-row input[type=checkbox]');
  switch(e.key) {
    case 'ArrowDown': focusItem(items[Math.min(index+1,items.length-1)]); break;
    case 'ArrowUp': focusItem(items[Math.max(index-1,0)]); break;
    case 'PageDown': focusItem(items[Math.min(index+10,items.length-1)]); break;
    case 'PageUp': focusItem(items[Math.max(index-10,0)]); break;
    case 'Home': focusItem(items[0]); break;
    case 'End': focusItem(items[items.length-1]); break;
    case 'ArrowRight': if(item.getAttribute('aria-expanded') === 'false') expandItem(item,true); else focusItem(item.querySelector('[role=treeitem]') || item); break;
    case 'ArrowLeft': if(item.getAttribute('aria-expanded') === 'true') expandItem(item,false); else focusItem(item.parentElement.closest('[role=treeitem]') || item); break;
    case '*': item.querySelectorAll('[role=treeitem][aria-expanded]').forEach(i => expandItem(i,true)); expandItem(item,true); break;
    case ' ': if(!onItem) return; checkbox().click(); break;
    case 'Enter': if(!onItem) return; if(item.hasAttribute('aria-expanded')) expandItem(item); else checkbox().click(); break;
    default: {
      if(!onItem || e.key.length !== 1 || e.ctrlKey || e.metaKey || e.altKey) return;
      clearTimeout(typeahead.timer); typeahead.buf += e.key.toLowerCase(); typeahead.timer = setTimeout(() => typeahead.buf = '', 700);
      const names = i => { const l = (i.getAttribute('aria-label') || '').toLowerCase(); return [l, l.split('/').pop()]; };
      const order = items.slice(index + (typeahead.buf.length > 1 ? 0 : 1)).concat(items.slice(0, index + 1));
      const hit = order.find(i => names(i).some(n => n.startsWith(typeahead.buf)));
      if(hit) focusItem(hit);
    }
  }
  e.preventDefault(); e.stopPropagation();
});
// Clicking a name selects the row for the keyboard (like clicking a file), without toggling anything.
treeRoot.addEventListener('click', e => { const id = e.target.closest('.identity'); if(id) focusItem(id.closest('[role=treeitem]')); });
treeRoot.addEventListener('mouseover', e => { const it = e.target.closest('[role=treeitem]'); if(it) setInfo(it); });
treeRoot.addEventListener('mouseleave', () => setInfo(activeRow));
let searchTimer;
document.getElementById('tree-search').addEventListener('input', () => {
  renderTree();
  // Announce the result count (debounced) so screen-reader and sighted users both learn what the filter did.
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    const q = document.getElementById('tree-search').value, n = visibleRepoCount();
    say(q ? n + (n === 1 ? ' repository matches “' : ' repositories match “') + q + '”.' : 'Search cleared.');
  }, 400);
});
treeRoot.addEventListener('focusin', e => {
  const it = e.target.closest('[role=treeitem]');
  if(it && it !== activeRow) activateRow(it);
  if(it) setInfo(it);
});
applyButton.onclick = async () => {
  if(treeBusy || !hasTreeEdits() || review.diff.stale) return;
  treeBusy = true; treeControls();
  try { await treeRequest('/api/tree/apply'); say('Apply finished. Review Saved, Relay reload and Acknowledged below.', 'ok'); }
  catch(e) { say(e.message, 'error'); }
  finally { treeBusy = false; treeControls(); }
};
async function discardTree() {
  if(review) await treeRequest('/api/tree/discard');
  review = null;
}
discardButton.onclick = async () => {
  if(treeBusy || !review || review.projection_pending || (!hasTreeEdits() && !review.diff.stale)) return;
  treeBusy = true; treeControls();
  try { await discardTree(); await loadTree(); say('Staged changes discarded.'); }
  catch(e) { say(e.message,'error'); }
  finally { treeBusy = false; treeControls(); }
};
async function leaveTreeEdits() {
  if(treeBusy) { say('Wait for the current contribution request.'); return false; }
  if(review && review.projection_pending) { say('Profiles were saved, but projection failed. Retry Apply before leaving this review.','error'); return false; }
  if(hasTreeEdits() && !await styledConfirm('Discard unapplied contribution edits?', 'Discard edits')) return false;
  await discardTree(); return true;
}
document.getElementById('tree-refresh').onclick = async () => {
  if(treeBusy) return;
  // Ordinary refresh keeps pending edits and focus; renewed discovery requires
  // explicitly discarding the old reviewed snapshot, never replaying its edits.
  try {
    if(hasTreeEdits() && !review.diff.stale) { await loadTree(); say('Staged edits preserved. Apply or Discard before discovering new membership.'); return; }
    if(!await leaveTreeEdits()) return;
    treeBusy = true; treeControls(); say('Checking each Hive for its current repositories…'); await loadTree(true); say('Membership refreshed for review. Nothing saved.');
  } catch(e) { say(e.message,'error'); }
  finally { treeBusy = false; treeControls(); }
};
document.getElementById('tree-leave').onclick = async () => {
  try { if(await leaveTreeEdits()) location.assign('about:blank'); } catch(e) { say(e.message,'error'); }
};
// In-page navigation and Back use our dialog. Browsers do not permit custom
// dialogs for toolbar reload/tab close; no native beforeunload prompt is added.
history.pushState({commonsReview:true}, '', location.pathname);
let leavingTree = false;
window.addEventListener('popstate', async () => {
  if(leavingTree) return;
  history.pushState({commonsReview:true}, '', location.pathname);
  try { if(await leaveTreeEdits()) { leavingTree = true; history.go(-2); } } catch(e) { say(e.message,'error'); }
});
document.addEventListener('click', async e => {
  const link = e.target.closest('a[href]'); if(!link || !hasTreeEdits()) return;
  e.preventDefault(); try { if(await leaveTreeEdits()) location.assign(link.href); } catch(err) { say(err.message,'error'); }
});
function setAllExpanded(value) {
  treeRoot.querySelectorAll('[role=treeitem][aria-expanded]').forEach(i => expandItem(i, value));
  const first = visibleItems()[0]; if(first) activateRow(first);
  say(value ? 'All hubs and Hives expanded.' : 'All hubs and Hives collapsed.');
}
document.getElementById('tree-collapse-all').onclick = () => setAllExpanded(false);
document.getElementById('tree-expand-all').onclick = () => setAllExpanded(true);
document.getElementById('tree-select-matching').onclick = async () => {
  const list = matchingCandidates();
  if(!list.length || treeBusy) return;
  const q = document.getElementById('tree-search').value;
  if(!await styledConfirm('Stage selection of ' + list.length + ' repositories whose names match “' + q + '”? Excluded repositories and unrestricted Hives are skipped. Nothing is saved until Apply.', 'Stage selection')) return;
  if(await stageList(list)) say('Staged ' + list.length + ' ' + (list.length === 1 ? 'repository' : 'repositories') + '. Nothing saved.');
};
document.getElementById('skip-apply').onclick = () => applyButton.focus();
document.getElementById('skip-profiles').onclick = () => document.getElementById('profiles-title').focus();
// "/" jumps to search from anywhere that is not a text field or dialog.
document.addEventListener('keydown', e => {
  if(e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey || e.target.closest('input,textarea,select,dialog')) return;
  e.preventDefault(); document.getElementById('tree-search').focus();
});
