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
async function treeRequest(path, change={}) {
  const response = await fetch(path, {method:'POST', headers:{...headers, 'If-Match':review.revision}, body:JSON.stringify({session:review.session, version:review.version, ...change})});
  let data;
  if((response.headers.get('Content-Type') || '').includes('application/json')) data = await response.json();
  else { const text = await response.text(); if(response.status === 409) { review.diff.stale = true; treeControls(); } throw new Error(text); }
  if(data.tree) { review = data; renderTree(); }
  if(data.receipt) {
    receiptText.textContent = 'Saved: ' + data.receipt.saved + '\nRelay reload: ' + data.receipt.relay_reload + '\nAcknowledged: ' + data.receipt.acknowledged + '\nChange saved does not prove relay adoption. In-flight tasks finish under their original lease.';
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
function button(text, action, key) {
  const b = node('button', text); b.type = 'button'; b.dataset.focusKey = key; b.onclick = action; return b;
}
function selectionText(s) {
  return (s.legacy_unrestricted ? 'Unrestricted — legacy' : ({off:'Off', partial:'Partial', 'all-current':'All current'}[s.state] || s.state)) +
    '; ' + s.selected + '/' + s.repositories + ' selected; ' + s.excluded + ' excluded; ' + s.unreviewed + ' unreviewed' +
    (s.membership_incomplete ? '; Membership incomplete' : '');
}
function visibleItems() { return Array.from(treeRoot.querySelectorAll('[role=treeitem]')).filter(e => !e.closest('[hidden]')); }
function focusItem(item) {
  if(!item) return;
  treeRoot.querySelectorAll('[role=treeitem]').forEach(e => e.tabIndex = -1); item.tabIndex = 0; item.focus();
}
function treeItem(parent, key, label, selection, change, branch) {
  const item = node('li'); item.setAttribute('role', 'treeitem'); item.tabIndex = -1; item.dataset.focusKey = key;
  item.setAttribute('aria-label', label);
  const row = node('div', undefined, 'tree-row'); item.appendChild(row); parent.appendChild(item);
  if(branch) {
    item.setAttribute('aria-expanded', String(!collapsed.has(key)));
    row.appendChild(button(collapsed.has(key) ? 'Expand' : 'Collapse', () => expandItem(item), key + ':expand'));
  }
  const check = document.createElement('input'); check.type = 'checkbox'; check.dataset.focusKey = key + ':select';
  check.checked = selection.state === 'all-current'; check.indeterminate = selection.state === 'partial';
  check.setAttribute('aria-checked', check.indeterminate ? 'mixed' : String(check.checked));
  check.setAttribute('aria-label', 'Select current ' + label);
  check.onchange = () => { check.checked = selection.state === 'all-current'; check.indeterminate = selection.state === 'partial'; editTree({...change, value:selection.state !== 'all-current'}); };
  row.append(check, node('span', label, 'identity'), node('span', selectionText(selection), 'tree-meta'));
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
  item.querySelector(':scope > .tree-row > button').textContent = value ? 'Collapse' : 'Expand';
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
    const h = treeItem(treeRoot, hkey, hub.name + ' (' + hub.id + ')', hub.selection, {operation:'current', target:hub.id, hub:true}, true);
    h.row.append(node('span','Follow: ' + hub.follow + ' · Readiness: unavailable · Activity: unavailable','tree-meta'));
    h.row.append(button('Deselect current', () => editTree({operation:'current',target:hub.id,hub:true,value:false}),hkey+':off'));
    h.row.append(button('Enable Hives', () => editTree({operation:'enabled',target:hub.id,hub:true,value:true}),hkey+':enable'));
    h.row.append(button('Disable Hives and stop hub following', () => editTree({operation:'enabled',target:hub.id,hub:true,value:false}),hkey+':disable'));
    for(const sub of members.filter(matches)) {
      const skey = JSON.stringify([hub.id,sub.id]);
      const s = treeItem(h.group, skey, sub.id + ' (' + sub.hive_id + ')', sub.selection, {operation:'current',target:sub.id}, true);
      s.row.append(node('span',(sub.disabled ? 'Disabled; saved scope retained · ' : '') + (sub.shared_across_hubs > 1 ? 'Shared across ' + sub.shared_across_hubs + ' hubs · ' : '') + 'Follow: ' + (sub.follow_new_repositories ? 'on' : 'off') + ' · Readiness: unavailable · Activity: unavailable','tree-meta'));
      s.row.append(button(sub.disabled ? 'Enable Hive' : 'Disable Hive', () => editTree({operation:'enabled',target:sub.id,value:sub.disabled}),skey+':enabled'));
      if(sub.selection.legacy_unrestricted) {
        s.row.append(button('Review and restrict', async () => {
          if(await styledConfirm('Stage restriction of ' + sub.id + ' to the reviewed current repositories? Following remains off. Review the diff before Apply.', 'Stage restriction')) editTree({operation:'restrict',target:sub.id});
        },skey+':restrict'));
      } else {
        s.row.append(button('Deselect current', () => editTree({operation:'current',target:sub.id,value:false}),skey+':off'));
        s.row.append(button(sub.follow_new_repositories ? 'Stop following' : 'Follow future repositories', () => editTree({operation:'follow',target:sub.id,value:!sub.follow_new_repositories}),skey+':follow'));
        const add = node('form');
        const input = document.createElement('input'); input.type = 'text'; input.required = true; input.placeholder = 'github/github.com/owner/repo'; input.setAttribute('aria-label','Repository to add to ' + sub.id); input.dataset.focusKey = skey+':add-input';
        const submit = node('button','Stage repository'); submit.type = 'submit'; submit.dataset.focusKey = skey+':add';
        add.append(input,submit); add.onsubmit = e => { e.preventDefault(); editTree({operation:'repository',target:sub.id,repository:input.value,value:true}); }; s.row.appendChild(add);
      }
      if(sub.sources && sub.sources.length) s.row.append(node('span','Selection sources: ' + sub.sources.join(', ') + '; per-repository provenance unavailable','tree-meta'));
      for(const repo of sub.repositories) {
        if(!hubMatch && !(sub.id + ' ' + sub.hive_id).toLowerCase().includes(search) && !repo.id.toLowerCase().includes(search)) continue;
        const rkey = JSON.stringify([hub.id,sub.id,repo.id]);
        const sel = {state:repo.selected ? 'all-current':'off',selected:repo.selected ? 1:0,repositories:1,excluded:(repo.excluded_by || []).length ? 1:0,unreviewed:repo.unreviewed ? 1:0};
        const r = treeItem(s.group,rkey,repo.id,sel,{operation:'repository',target:sub.id,repository:repo.id},false);
        r.row.append(node('span','Offered: ' + repo.offered + ' · Readiness: unavailable · Activity: unavailable','tree-meta'));
        if((repo.excluded_by || []).length) r.row.append(node('span','Excluded by ' + repo.excluded_by.join(', '),'pill'));
        if(repo.unreviewed) r.row.append(node('span','Unreviewed','pill'));
        if(!sub.selection.legacy_unrestricted) {
          const excludedHere = (repo.excluded_by || []).includes(sub.id);
          r.row.append(button(excludedHere ? 'Clear this exclusion' : 'Exclude here', () => editTree({operation:'exclude',target:sub.id,repository:repo.id,value:!excludedHere}),rkey+':exclude'));
        }
      }
      if(search) expandItem(s.item,true);
    }
    if(search) expandItem(h.item,true);
  }
  if(!treeRoot.children.length) treeRoot.appendChild(node('li','No matching saved subscriptions. Use profile management to subscribe.'));
  const d = review.diff;
  document.getElementById('diff-summary').textContent = d.stale ? 'Stale review — discard and review again before Apply.' : hasTreeEdits() ? '+' + d.selected_count + ' repositories selected, −' + d.deselected_count + ' deselected, ' + d.excluded_count + ' exclusions added, ' + d.cleared_exclusion_count + ' cleared; follow changes: ' + (d.follow || []).length + '; enable/disable: ' + (d.enabled || []).length + '; legacy conversions: ' + (d.legacy_conversions || []).length + '. Review named targets and shared effects.' : 'No staged changes.';
  document.getElementById('tree-diff').textContent = hasTreeEdits() ? JSON.stringify(d,null,2) : '';
  // Restore the initiating control, never move focus into a status message.
  const items = visibleItems();
  const restored = focusKey && Array.from(treeRoot.querySelectorAll('[data-focus-key]')).find(e => e.dataset.focusKey === focusKey && !e.closest('[hidden]'));
  if(restored) { restored.closest('[role=treeitem]').tabIndex = 0; restored.focus(); }
  else if(oldIndex >= 0) focusItem(items[Math.min(oldIndex,items.length-1)]);
  else if(items[0]) items[0].tabIndex = 0;
  treeControls();
}
treeRoot.addEventListener('keydown', e => {
  if(e.target.getAttribute('role') !== 'treeitem') return;
  const item = e.target, items = visibleItems(), index = items.indexOf(item);
  switch(e.key) {
    case 'ArrowDown': focusItem(items[Math.min(index+1,items.length-1)]); break;
    case 'ArrowUp': focusItem(items[Math.max(index-1,0)]); break;
    case 'Home': focusItem(items[0]); break;
    case 'End': focusItem(items[items.length-1]); break;
    case 'ArrowRight': if(item.getAttribute('aria-expanded') === 'false') expandItem(item,true); else focusItem(item.querySelector('[role=treeitem]')); break;
    case 'ArrowLeft': if(item.getAttribute('aria-expanded') === 'true') expandItem(item,false); else focusItem(item.parentElement.closest('[role=treeitem]')); break;
    case ' ': item.querySelector(':scope > .tree-row input[type=checkbox]').click(); break;
    case 'Enter': item.querySelector(':scope > .tree-row input[type=checkbox]').focus(); break;
    default: return;
  }
  e.preventDefault(); e.stopPropagation();
});
document.getElementById('tree-search').addEventListener('input', renderTree);
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
    treeBusy = true; treeControls(); await loadTree(true); say('Membership refreshed for review. Nothing saved.');
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
