package commands

// commonsWebTreeHTML is the Hub → Hive → Repository section of the Commons
// page. It is appended to commonsWebTemplate and shares its token, headers and
// api() helper. All consent state lives server-side in the staged session:
// the page only sends operations and renders the tree/diff it gets back.
// Rules: no inline style attributes, no third-party assets, untrusted
// publisher text only ever goes through textContent.
const commonsWebTreeHTML = `
<section id="tree-section" aria-labelledby="tree-heading">
<h2 id="tree-heading">Contribution tree</h2>
<p class="muted">Hub → Hive → Repository consent over the same profile store. Toggling stages an edit; nothing is saved and no new work is solicited until you press Apply. Readiness and activity come from the local relay's status snapshot; history from the local activity store. Each column reads <em>unavailable</em> with a reason when its source is missing — never idle or zero without evidence.</p>
<dl id="tree-runtime" class="muted" aria-label="Runtime sources"><dt>Live</dt><dd id="tree-live">unavailable</dd><dt>Durable</dt><dd id="tree-durable">unavailable</dd></dl>
<div class="tree-tools">
  <label>Search <input id="tree-search" type="search" placeholder="hub, hive or owner/repo" aria-describedby="tree-search-help"></label>
  <span id="tree-search-help" class="muted">Matches keep their ancestors; bulk actions still affect the whole branch.</span>
  <fieldset class="tree-filters"><legend>Filter</legend>
    <label><input type="radio" name="tree-filter" value="all" checked> All</label>
    <label><input type="radio" name="tree-filter" value="review"> Needs review</label>
    <label><input type="radio" name="tree-filter" value="changed"> Changed</label>
  </fieldset>
  <button id="tree-refresh" type="button">Refresh discovery</button>
</div>
<div class="tree-layout">
  <ul id="tree" role="tree" aria-label="Contribution tree" aria-describedby="tree-keys"></ul>
  <aside id="detail" aria-live="polite" aria-label="Details"><p class="muted">Select a row and press Enter for its canonical identity and effective selection.</p></aside>
</div>
<p id="tree-keys" class="muted">Keys: ↑/↓ move, ←/→ collapse/expand, space select/deselect current, enter details, f follow, x exclude, A apply, U discard.</p>
<div id="stage" class="stage" role="region" aria-label="Staged changes">
  <span id="stage-summary">No staged changes.</span>
  <span class="row-actions"><button id="apply" type="button" disabled>Apply</button> <button id="discard" type="button" disabled>Discard</button></span>
</div>
</section>
<script>
(function(){
const treeEl = document.getElementById('tree');
const detailEl = document.getElementById('detail');
const summaryEl = document.getElementById('stage-summary');
const applyBtn = document.getElementById('apply');
const discardBtn = document.getElementById('discard');
const searchEl = document.getElementById('tree-search');
let state = null, expanded = {}, focusId = '', filter = 'all', query = '', treeRevision = '';
const liveEl = document.getElementById('tree-live');
const durableEl = document.getElementById('tree-durable');
function runtimeText(v){ return v && v.summary ? v.summary : 'unavailable'; }
function renderRuntime(){
  const rt = state.runtime || {};
  liveEl.textContent = runtimeText(rt.live);
  durableEl.textContent = runtimeText(rt.durable);
}
function treeApi(path, opts){
  opts = opts || {};
  const h = Object.assign({}, headers);
  if (opts.method && opts.method !== 'GET' && treeRevision) h['If-Match'] = treeRevision;
  return fetch(path, Object.assign({}, opts, {headers: h})).then(async res => {
    const text = await res.text();
    let data = null; try { data = JSON.parse(text); } catch (e) { data = null; }
    if (!res.ok && !(data && data.receipt)) throw new Error(text);
    return {status: res.status, data: data};
  });
}
function setState(data){
  state = data; treeRevision = data.revision || '';
  if (!Object.keys(expanded).length) { for (const h of data.tree.hubs) { expanded['hub:'+h.id] = true; for (const s of h.subscriptions) expanded['hub:'+h.id+'/sub:'+s] = true; } }
  render();
}
function subById(id){ return (state.tree.subscriptions||[]).find(s => s.id === id); }
function changedSet(){
  const subs = {}, repos = {};
  const d = state.diff || {};
  for (const m of d.membership||[]) subs[m.subscription] = true;
  for (const e of d.enabled||[]) subs[e.target] = true;
  for (const f of d.follow||[]) subs[f.target] = true;
  for (const t of (d.selected||[]).concat(d.deselected||[])) { subs[t.subscription] = true; repos[t.subscription+'|'+t.repository] = true; }
  for (const x of d.exclusions||[]) { subs[x.subscription] = true; repos[x.subscription+'|'+x.repository] = true; for (const s of x.scoped_subscriptions||[]) { subs[s] = true; repos[s+'|'+x.repository] = true; } }
  return {subs: subs, repos: repos};
}
function matches(text){ return !query || text.toLowerCase().indexOf(query) >= 0; }
function repoVisible(sub, r, ch){
  if (filter === 'review' && !(r.unreviewed || r.offered === 'no')) return false;
  if (filter === 'changed' && !ch.repos[sub.id+'|'+r.id]) return false;
  return matches(r.id) || matches(sub.id) || matches(sub.hive_id);
}
function subVisible(hub, sub, ch){
  if (filter === 'review' && !(sub.selection.legacy_unrestricted || sub.selection.membership_incomplete || sub.selection.unreviewed > 0 || sub.discovery === 'unavailable')) return sub.repositories.some(r => repoVisible(sub, r, ch));
  if (filter === 'changed' && !ch.subs[sub.id]) return false;
  return matches(sub.id) || matches(sub.hive_id) || matches(hub.name) || sub.repositories.some(r => repoVisible(sub, r, ch));
}
function selectionLabel(sel, discovery){
  if (sel.legacy_unrestricted) return 'Unrestricted — legacy';
  if (sel.repositories === 0) return discovery === 'complete' ? 'No repositories' : 'Off — no saved repositories';
  let label = sel.state === 'all-current' ? 'All current' : (sel.state === 'partial' ? 'Partial' : 'Off');
  label += ' (' + sel.selected + '/' + sel.repositories + ' selected';
  if (sel.excluded) label += ', ' + sel.excluded + ' excluded';
  if (sel.unreviewed) label += ', ' + sel.unreviewed + ' unreviewed';
  label += ')';
  if (sel.membership_incomplete) label += ' — Membership incomplete';
  return label;
}
function badge(parent, text, cls){ const b = document.createElement('span'); b.className = 'pill ' + (cls||''); b.textContent = text; parent.appendChild(b); parent.appendChild(document.createTextNode(' ')); }
function actionBtn(parent, label, handler){ const b = document.createElement('button'); b.type = 'button'; b.tabIndex = -1; b.textContent = label; b.addEventListener('click', ev => { ev.stopPropagation(); handler(); }); parent.appendChild(b); parent.appendChild(document.createTextNode(' ')); return b; }
function checkbox(row, sel, label, onChange, allowCheck){
  const cb = document.createElement('input'); cb.type = 'checkbox'; cb.tabIndex = -1;
  cb.setAttribute('aria-label', (sel.state === 'all-current' ? 'Deselect current for ' : 'Select current for ') + label);
  cb.checked = sel.state === 'all-current'; cb.indeterminate = sel.state === 'partial';
  cb.disabled = sel.legacy_unrestricted || sel.repositories === 0 || (!allowCheck && sel.state !== 'all-current' && sel.selected === 0);
  cb.addEventListener('click', ev => ev.stopPropagation());
  cb.addEventListener('change', () => onChange(sel.state !== 'all-current'));
  row.appendChild(cb);
  return cb;
}
function item(level, id, label, expandable){
  const li = document.createElement('li'); li.setAttribute('role', 'treeitem'); li.setAttribute('aria-level', String(level)); li.dataset.id = id; li.tabIndex = -1;
  if (expandable) li.setAttribute('aria-expanded', expanded[id] ? 'true' : 'false');
  const row = document.createElement('div'); row.className = 'tree-row';
  if (expandable) { const tg = document.createElement('button'); tg.type = 'button'; tg.tabIndex = -1; tg.className = 'tree-toggle'; tg.setAttribute('aria-hidden', 'true'); tg.textContent = expanded[id] ? '▾' : '▸'; tg.addEventListener('click', ev => { ev.stopPropagation(); expanded[id] = !expanded[id]; render(); }); row.appendChild(tg); }
  li.appendChild(row);
  li.addEventListener('click', ev => { ev.stopPropagation(); focusId = id; focusRow(); showDetail(id); });
  return {li: li, row: row};
}
function stageEdit(req, note){
  treeApi('/api/tree/edit', {method: 'POST', body: JSON.stringify(req)})
    .then(r => { setState(r.data); say(note || 'Staged — nothing saved until Apply.', 'ok'); })
    .catch(e => { say(e.message, 'error'); render(); });
}
function render(){
  if (!state) return;
  const ch = changedSet();
  treeEl.textContent = '';
  const hubs = state.tree.hubs || [];
  if (!hubs.length) { const li = document.createElement('li'); li.setAttribute('role', 'treeitem'); li.className = 'muted'; li.textContent = 'No saved hives yet — subscribe above.'; treeEl.appendChild(li); }
  for (const hub of hubs) {
    const visibleSubs = hub.subscriptions.map(subById).filter(s => s && subVisible(hub, s, ch));
    if (!visibleSubs.length && !matches(hub.name)) continue;
    const hubID = 'hub:' + hub.id;
    const it = item(1, hubID, hub.name, true);
    const hubDisc = (state.discovery.hubs||{})[hub.id];
    checkbox(it.row, hub.selection, hub.name, v => stageEdit({op: 'select-current', hub: hub.id, value: v}, v ? 'Staged: select current across ' + hub.name : 'Staged: deselect current across ' + hub.name + ' (follow policies untouched)'), !hub.selection.membership_incomplete);
    const name = document.createElement('strong'); name.textContent = hub.name; it.row.appendChild(name); it.row.appendChild(document.createTextNode(' '));
    badge(it.row, selectionLabel(hub.selection, hub.discovery));
    if (!hub.direct) badge(it.row, 'Future Hives: ' + hub.follow);
    if (hubDisc && hubDisc.status !== 'complete') badge(it.row, 'Discovery ' + hubDisc.status + (hubDisc.reason ? ' — ' + hubDisc.reason : ''), 'warn');
    if (!hub.subscriptions.length) badge(it.row, 'Empty collection', 'warn');
    const acts = document.createElement('span'); acts.className = 'row-actions';
    if (!hub.direct && hub.subscriptions.length) {
      const anyEnabled = hub.subscriptions.some(id => { const s = subById(id); return s && !s.disabled; });
      actionBtn(acts, anyEnabled ? 'Disable hub' : 'Enable hub', () => stageEdit({op: 'hub-enable', hub: hub.id, value: !anyEnabled}, anyEnabled ? 'Staged: disable ' + hub.subscriptions.length + ' subscription(s); future-Hive enrollment off' : 'Staged: enable subscriptions (saved scope restored)'));
    }
    it.row.appendChild(acts);
    treeEl.appendChild(it.li);
    if (!expanded[hubID]) continue;
    const group = document.createElement('ul'); group.setAttribute('role', 'group'); it.li.appendChild(group);
    for (const sub of visibleSubs) renderSub(group, hub, sub, ch);
  }
  renderStage(); renderRuntime();
  focusRow();
}
function renderSub(group, hub, sub, ch){
  const subID = 'hub:' + hub.id + '/sub:' + sub.id;
  const it = item(2, subID, sub.id, true);
  const sel = sub.selection;
  checkbox(it.row, sel, sub.id, v => stageEdit({op: 'select-current', subscription: sub.id, value: v}, v ? 'Staged: select current at ' + sub.id : 'Staged: deselect current at ' + sub.id + (sub.follow_new_repositories ? ' — following stays on and may repopulate this Hive on refresh' : '')), !sel.membership_incomplete);
  const name = document.createElement('strong'); name.textContent = sub.id; it.row.appendChild(name); it.row.appendChild(document.createTextNode(' '));
  badge(it.row, selectionLabel(sel, sub.discovery), sel.legacy_unrestricted ? 'warn' : '');
  if (sub.disabled) badge(it.row, 'Disabled — scope retained', 'warn');
  if (sub.shared_across_hubs > 1) badge(it.row, 'Shared across ' + sub.shared_across_hubs + ' hubs');
  badge(it.row, sub.follow_new_repositories ? 'Following new repositories' : 'Follow off');
  if (sub.discovery !== 'complete') { const d = (state.discovery.subscriptions||{})[sub.id]; badge(it.row, 'Discovery ' + (d ? d.status : 'unknown') + (d && d.reason ? ' — ' + d.reason : ''), 'warn'); }
  badge(it.row, 'Readiness: ' + sub.readiness, 'muted');
  badge(it.row, 'Activity: ' + sub.activity, 'muted');
  badge(it.row, 'History: ' + sub.history, 'muted');
  const acts = document.createElement('span'); acts.className = 'row-actions';
  if (sel.legacy_unrestricted) actionBtn(acts, 'Review and restrict', () => reviewRestrict(sub));
  else actionBtn(acts, sub.follow_new_repositories ? 'Stop following' : 'Follow new repositories', () => stageEdit({op: 'follow', subscription: sub.id, value: !sub.follow_new_repositories}));
  actionBtn(acts, sub.disabled ? 'Enable' : 'Disable', () => stageEdit({op: 'enable', subscription: sub.id, value: sub.disabled}));
  it.row.appendChild(acts);
  group.appendChild(it.li);
  if (!expanded[subID]) return;
  const rg = document.createElement('ul'); rg.setAttribute('role', 'group'); it.li.appendChild(rg);
  const repos = sub.repositories.filter(r => repoVisible(sub, r, ch) || matches(sub.id) || matches(hub.name));
  if (!sub.repositories.length) { const e = item(3, subID + '/none', 'none', false); e.row.textContent = sel.legacy_unrestricted ? 'Unrestricted — legacy; review and restrict to list repositories' : (sub.discovery === 'complete' ? 'No repositories' : 'Membership unknown — discovery ' + sub.discovery); e.row.className = 'tree-row muted'; rg.appendChild(e.li); }
  for (const r of repos) {
    const rid = subID + '/repo:' + r.id;
    const ri = item(3, rid, r.id, false);
    const cb = document.createElement('input'); cb.type = 'checkbox'; cb.tabIndex = -1; cb.checked = r.selected; cb.setAttribute('aria-label', 'Select ' + r.id + ' at ' + sub.id);
    cb.disabled = sel.legacy_unrestricted;
    cb.addEventListener('click', ev => ev.stopPropagation());
    cb.addEventListener('change', () => stageEdit({op: 'select', subscription: sub.id, repository: r.id, value: cb.checked}));
    ri.row.appendChild(cb);
    const code = document.createElement('code'); code.textContent = r.id; ri.row.appendChild(code); ri.row.appendChild(document.createTextNode(' '));
    if (r.excluded_by && r.excluded_by.length) badge(ri.row, 'Excluded by ' + r.excluded_by.join(', '), 'warn');
    if (r.unreviewed) badge(ri.row, 'Unreviewed', 'warn');
    if (r.offered === 'no') badge(ri.row, 'No longer offered', 'warn');
    if (ch.repos[sub.id + '|' + r.id]) badge(ri.row, 'Changed', 'ok');
    const acts = document.createElement('span'); acts.className = 'row-actions';
    const mine = (r.excluded_by||[]).indexOf(sub.id) >= 0;
    if (!sel.legacy_unrestricted) actionBtn(acts, mine ? 'Clear exclusion' : 'Exclude', () => stageEdit({op: 'exclude', subscription: sub.id, repository: r.id, value: !mine}, mine ? 'Staged: exclusion cleared — remaining saved membership decides selection' : 'Staged: persistent exclusion (parent actions cannot clear it)'));
    ri.row.appendChild(acts);
    rg.appendChild(ri.li);
  }
}
function reviewRestrict(sub){
  const n = sub.repositories.length;
  styledConfirm('Review and restrict ' + sub.id + ': the ' + n + ' currently discovered repositories become explicit current membership with following off. Nothing is saved until Apply. Continue?')
    .then(ok => { if (ok) stageEdit({op: 'review-restrict', subscription: sub.id}, 'Staged: legacy conversion for ' + sub.id + ' — review the diff before Apply'); });
}
function diffLines(d){
  const lines = [];
  if (d.selected_count) lines.push('+' + d.selected_count + ' repositories selected');
  if (d.deselected_count) lines.push('−' + d.deselected_count + ' deselected');
  if (d.excluded_count) lines.push(d.excluded_count + ' excluded');
  if (d.cleared_exclusion_count) lines.push(d.cleared_exclusion_count + ' exclusions cleared');
  const fOn = (d.follow||[]).filter(f => f.after).length, fOff = (d.follow||[]).filter(f => !f.after).length;
  if (fOn) lines.push('follow on for ' + fOn + ' Hive(s)');
  if (fOff) lines.push('stop following for ' + fOff + ' Hive(s)');
  const en = (d.enabled||[]).filter(e => e.after).map(e => e.target), dis = (d.enabled||[]).filter(e => !e.after).map(e => e.target);
  if (en.length) lines.push('enable ' + en.join(', '));
  if (dis.length) lines.push('disable ' + dis.join(', ') + ' (credentials and selections retained)');
  for (const h of d.hub_follow||[]) lines.push('hub ' + h.hub + ' future Hives ' + h.before + ' → ' + h.after);
  if ((d.legacy_conversions||[]).length) lines.push('legacy conversion: ' + d.legacy_conversions.join(', '));
  for (const s of d.shared_branches||[]) lines.push('shared branch ' + s.subscription + ' changes in all of: ' + s.hubs.join(', '));
  for (const x of d.exclusions||[]) {
    let l = (x.excluded ? 'exclude ' : 'clear exclusion of ') + x.repository + ' at ' + x.subscription;
    if ((x.scoped_subscriptions||[]).length) l += '; also affects saved membership at ' + x.scoped_subscriptions.join(', ');
    if ((x.sources||[]).length) l += '; exclusion still kept by ' + x.sources.join(', ');
    if ((x.legacy_unrestricted||[]).length) l += '; legacy unrestricted ' + x.legacy_unrestricted.join(', ') + ' can still receive it';
    lines.push(l);
  }
  const membershipOnly = (d.membership||[]).map(m => m.subscription).filter(n => !lines.some(l => l.indexOf(n) >= 0));
  if (membershipOnly.length) lines.push('membership rules changed at ' + membershipOnly.join(', '));
  return lines;
}
function renderStage(){
  const d = state.diff || {};
  const lines = diffLines(d);
  summaryEl.textContent = state.staged ? 'Staged: ' + lines.join('; ') : 'No staged changes.';
  applyBtn.disabled = !state.staged; discardBtn.disabled = !state.staged;
}
function listDialog(title, lines, confirmLabel){
  return new Promise(resolve => {
    const prev = document.activeElement;
    const back = document.createElement('div'); back.className = 'modal-back';
    const box = document.createElement('div'); box.className = 'modal modal-wide'; box.setAttribute('role', 'dialog'); box.setAttribute('aria-modal', 'true'); box.setAttribute('aria-labelledby', 'dlg-title');
    const h = document.createElement('h3'); h.id = 'dlg-title'; h.textContent = title; box.appendChild(h);
    const ul = document.createElement('ul'); for (const l of lines) { const li = document.createElement('li'); li.textContent = l; ul.appendChild(li); } box.appendChild(ul);
    const bar = document.createElement('div');
    const no = document.createElement('button'); no.type = 'button'; no.textContent = confirmLabel ? 'Cancel' : 'Close'; bar.appendChild(no);
    let yes = null; if (confirmLabel) { yes = document.createElement('button'); yes.type = 'button'; yes.dataset.act = 'yes'; yes.textContent = confirmLabel; bar.appendChild(yes); }
    box.appendChild(bar); back.appendChild(box);
    function done(v){ document.removeEventListener('keydown', key, true); back.remove(); if (prev && prev.focus) prev.focus(); resolve(v); }
    function key(e){ if (e.key === 'Escape') { e.preventDefault(); done(false); } if (e.key === 'Tab') { const f = [no].concat(yes ? [yes] : []); const i = f.indexOf(document.activeElement); e.preventDefault(); f[(i + (e.shiftKey ? f.length - 1 : 1)) % f.length].focus(); } }
    no.addEventListener('click', () => done(false)); if (yes) yes.addEventListener('click', () => done(true));
    document.addEventListener('keydown', key, true);
    document.body.appendChild(back); (yes || no).focus();
  });
}
function applyStaged(){
  if (!state || !state.staged) return;
  listDialog('Apply staged changes?', diffLines(state.diff), 'Apply').then(ok => {
    if (!ok) return;
    return treeApi('/api/tree/apply', {method: 'POST', body: '{}'}).then(r => {
      const d = r.data || {}; const rc = d.receipt || {};
      if (d.tree) setState(d.tree);
      const lines = ['Saved: ' + (rc.saved || 'not-saved'), 'Relay reload: ' + (rc.relay_reload || 'not-requested'), 'Acknowledged: ' + (rc.acknowledged || 'unknown') + (rc.acknowledged === 'pending' ? ' — Change saved — relay adoption pending; an in-flight task finishes under its original lease' : '')];
      if (rc.scope_version) lines.push('Saved generation: ' + rc.scope_version);
      if (d.message) lines.push(d.message);
      say(d.outcome === 'ok' ? 'Applied.' : d.message || d.outcome, d.outcome === 'ok' || d.outcome === 'reload-failed' ? 'ok' : 'error');
      return listDialog('Apply receipt (' + (d.outcome || 'ok') + ')', lines, '');
    });
  }).catch(e => say(e.message, 'error'));
}
function discardStaged(){
  treeApi('/api/tree/discard', {method: 'POST', body: '{}'}).then(r => { setState(r.data); say('Staged edits discarded; saved consent unchanged.', 'ok'); }).catch(e => say(e.message, 'error'));
}
function showDetail(id){
  const parts = id.split('/'); const sub = parts[1] ? subById(parts[1].slice(4)) : null; const hub = (state.tree.hubs||[]).find(h => 'hub:' + h.id === parts[0]);
  detailEl.textContent = '';
  const dl = document.createElement('dl');
  function kv(k, v){ const dt = document.createElement('dt'); dt.textContent = k; const dd = document.createElement('dd'); dd.textContent = v; dl.appendChild(dt); dl.appendChild(dd); }
  if (parts.length === 1 && hub) { kv('Hub', hub.name); kv('Canonical origin', hub.id); kv('Future-Hive enrollment', hub.direct ? 'not applicable (Direct Hives)' : hub.follow); kv('Discovery', hub.discovery || 'unknown'); kv('Selection', selectionLabel(hub.selection, hub.discovery)); }
  if (sub && parts.length === 2) {
    kv('Subscription', sub.id); kv('Hive origin', sub.hive_id); kv('Enabled', sub.disabled ? 'no — credentials and selections retained' : 'yes');
    kv('Selection', selectionLabel(sub.selection, sub.discovery)); kv('Follow new repositories', sub.follow_new_repositories ? 'on (refresh-driven, from the displayed source)' : 'off');
    kv('Shared across hubs', String(sub.shared_across_hubs || 0)); kv('Selection sources', (sub.selection_sources||[]).join(', ') || 'none recorded (selection-level attribution)');
    kv('Readiness', sub.readiness); kv('Activity', sub.activity); kv('History', sub.history); kv('Live source', runtimeText((state.runtime||{}).live)); kv('Durable source', runtimeText((state.runtime||{}).durable));
  }
  if (sub && parts.length === 3) {
    const r = sub.repositories.find(x => 'repo:' + x.id === parts[2]);
    if (r) { kv('Repository', r.id); kv('Subscription', sub.id + ' (' + sub.hive_id + ')'); kv('Selected', r.selected ? 'yes' : 'no'); kv('Excluded by', (r.excluded_by||[]).join(', ') || 'none'); kv('Unreviewed', r.unreviewed ? 'yes — select, exclude, or leave pending' : 'no'); kv('Offered by Hive', r.offered); kv('Recommendation attribution', 'unavailable per repository; selection-level sources: ' + ((sub.selection_sources||[]).join(', ') || 'none')); kv('Activity', r.activity); kv('History', r.history); }
  }
  detailEl.appendChild(dl);
}
function visibleItems(){ return Array.from(treeEl.querySelectorAll('li[role=treeitem][data-id]')); }
function focusRow(){
  const items = visibleItems(); if (!items.length) return;
  let target = items.find(li => li.dataset.id === focusId) || items[0];
  focusId = target.dataset.id;
  for (const li of items) li.tabIndex = li === target ? 0 : -1;
  if (treeEl.contains(document.activeElement) && document.activeElement !== target) target.focus();
}
treeEl.addEventListener('focusin', ev => { const li = ev.target.closest('li[data-id]'); if (li) { focusId = li.dataset.id; } });
treeEl.addEventListener('keydown', ev => {
  const items = visibleItems(); const i = items.findIndex(li => li.dataset.id === focusId); const li = items[i]; if (!li) return;
  const id = li.dataset.id; const exp = li.getAttribute('aria-expanded');
  const go = n => { if (items[n]) { focusId = items[n].dataset.id; focusRow(); items[n].focus(); } };
  switch (ev.key) {
    case 'ArrowDown': case 'j': ev.preventDefault(); go(i + 1); break;
    case 'ArrowUp': case 'k': ev.preventDefault(); go(i - 1); break;
    case 'Home': ev.preventDefault(); go(0); break;
    case 'End': ev.preventDefault(); go(items.length - 1); break;
    case 'ArrowRight': case 'l': ev.preventDefault(); if (exp === 'false') { expanded[id] = true; render(); } else if (exp === 'true') go(i + 1); break;
    case 'ArrowLeft': case 'h': ev.preventDefault(); if (exp === 'true') { expanded[id] = false; render(); } else { const p = id.lastIndexOf('/'); if (p > 0) { focusId = id.slice(0, p); focusRow(); const t = items.find(x => x.dataset.id === focusId); if (t) t.focus(); } } break;
    case ' ': ev.preventDefault(); { const cb = li.querySelector(':scope > .tree-row > input[type=checkbox]'); if (cb && !cb.disabled) cb.click(); } break;
    case 'Enter': ev.preventDefault(); showDetail(id); break;
    case 'f': case 'x': ev.preventDefault(); { const want = ev.key === 'f' ? /following|Follow/ : /xclu/; const b = Array.from(li.querySelectorAll(':scope > .tree-row button')).find(x => want.test(x.textContent)); if (b) b.click(); } break;
    case 'A': ev.preventDefault(); applyStaged(); break;
    case 'U': ev.preventDefault(); discardStaged(); break;
  }
  focusRow();
});
searchEl.addEventListener('input', () => { query = searchEl.value.trim().toLowerCase(); render(); });
for (const r of document.querySelectorAll('input[name=tree-filter]')) r.addEventListener('change', () => { filter = r.value; render(); });
document.getElementById('tree-refresh').addEventListener('click', () => {
  treeApi('/api/tree?refresh=1').then(r => { setState(r.data); say('Discovery refreshed; staged edits preserved only when none were pending.', 'ok'); }).catch(e => say(e.message, 'error'));
});
applyBtn.addEventListener('click', applyStaged);
discardBtn.addEventListener('click', discardStaged);
window.addEventListener('beforeunload', ev => { if (state && state.staged) { ev.preventDefault(); ev.returnValue = ''; } });
// load() in the page script calls this after every subscription-list refresh.
window.loadTree = function(){ return treeApi('/api/tree').then(r => setState(r.data)); };
})();
</script>
`
