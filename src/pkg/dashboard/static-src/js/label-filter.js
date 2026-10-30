    // ── Label filter (hf) pure helpers (begin) ──────────────────────────────
    // Faceted label filter for the project cards (hivecommons/hive#12172).
    // Everything between the begin/end markers is DOM-free so
    // TestDashboardLabelFilterHelpers can run it under node.
    const HF_STORE_PREFIX = 'hive-label-filters:';
    const HF_STORE_VERSION = 1;
    const HF_DEFAULT_PRESET_KEY = 'preset:needs-human';
    const HF_KINDS = Object.freeze([{ key: 'issue', label: 'Issues' }, { key: 'pr', label: 'PRs' }]);
    const HF_STATES = Object.freeze([
      { key: 'unclaimed', label: 'Unclaimed', tip: 'Issue with no assignee or live claim' },
      { key: 'claimed', label: 'Claimed', tip: 'Issue assigned, claimed or withheld behind a claim' },
      { key: 'parked', label: 'Parked', tip: 'Held or hard-suppressed — out of every agent lane' },
      { key: 'covered-by-pr', label: 'Covered by PR', tip: 'Issue hive has verified a PR covers' }
    ]);
    const HF_AUTHORS = Object.freeze([
      { key: 'hive', label: 'Hive', tip: 'Filed by this hive\u2019s agents' },
      { key: 'human', label: 'Human', tip: 'Filed by a person' },
      { key: 'bot', label: 'Bot', tip: 'Filed by another bot (renovate, dependabot, …)' }
    ]);
    const HF_LABEL_TONE = Object.freeze({ 'stage/045-needs-decision': 'decision', 'stage/085-needs-decision': 'decision', 'needs-decision': 'decision', 'needs-spec': 'spec', 'needs-signal': 'signal', 'needs-human': 'human' });
    const HF_TEXT_DEBOUNCE_MS = 250;
    const HF_MAX_ROW_LABELS = 3;

    function hfEmptyFilter() {
      return { labels: [], terms: [], kinds: [], states: [], authors: [], matchAll: false };
    }
    function hfNormalizeFilter(raw) {
      const f = hfEmptyFilter();
      if (!raw || typeof raw !== 'object') return f;
      const strs = v => Array.isArray(v) ? v.map(x => String(x == null ? '' : x).trim()).filter(Boolean) : [];
      f.labels = Array.from(new Set(strs(raw.labels).map(l => l.toLowerCase())));
      f.terms = Array.from(new Set(strs(raw.terms).map(t => { const p = hfParseTerm(t); return p ? p.raw : ''; }).filter(Boolean)));
      f.kinds = strs(raw.kinds).filter(k => HF_KINDS.some(x => x.key === k));
      f.states = strs(raw.states).filter(k => HF_STATES.some(x => x.key === k));
      f.authors = strs(raw.authors).filter(k => HF_AUTHORS.some(x => x.key === k));
      f.matchAll = !!raw.matchAll;
      return f;
    }
    /** Canonical string for equality checks: label order matters (it is the sort priority); the rest is order-free. */
    function hfFilterSignature(filter) {
      const f = hfNormalizeFilter(filter);
      return JSON.stringify([f.labels, f.terms.slice().sort(), f.kinds.slice().sort(), f.states.slice().sort(), f.authors.slice().sort(), f.matchAll]);
    }
    function hfFilterIsEmpty(filter) {
      const f = hfNormalizeFilter(filter);
      return !f.labels.length && !f.terms.length && !f.kinds.length && !f.states.length && !f.authors.length;
    }
    /**
     * One text chip: `-` prefix excludes, surrounding quotes make a phrase,
     * otherwise every word must appear (AND within a term). Plain substring —
     * users never type regex.
     */
    function hfParseTerm(raw) {
      let s = String(raw == null ? '' : raw).trim();
      if (!s) return null;
      let negate = false;
      if (s.charAt(0) === '-') { negate = true; s = s.slice(1).trim(); if (!s) return null; }
      let phrase = false;
      const quoted = /^"(.*)"$/.exec(s) || /^\u201c(.*)\u201d$/.exec(s);
      if (quoted) { phrase = true; s = quoted[1].trim(); }
      s = s.toLowerCase();
      if (!s) return null;
      const words = phrase ? [s.replace(/\s+/g, ' ')] : s.split(/\s+/).filter(Boolean);
      return { raw: (negate ? '-' : '') + (phrase ? '"' + words[0] + '"' : words.join(' ')), negate, phrase, words };
    }
    function hfTermHits(term, haystack) {
      return !!term && term.words.every(w => haystack.indexOf(w) >= 0);
    }
    function hfAuthorKind(item, aiAuthor) {
      if (!item) return 'human';
      const author = String(item.author || '').trim();
      if (item.app_authored || item.hive_attributed || (aiAuthor && author && author.toLowerCase() === String(aiAuthor).toLowerCase())) return 'hive';
      if (/\[bot\]$/i.test(author) || /-bot$|\bbot\b/i.test(author)) return 'bot';
      return 'human';
    }
    /** Normalise one snapshot record into a filterable entry. kind is 'issue' or 'pr'. */
    function hfEntry(kind, item, repo, opts) {
      const o = opts || {};
      const labels = (item && Array.isArray(item.labels) ? item.labels : []).map(l => String(l == null ? '' : l).trim()).filter(Boolean);
      const states = new Set();
      if (o.held || o.bucket === 'filtered') states.add('parked');
      if (kind === 'issue') {
        const claimed = (Array.isArray(item.assignees) && item.assignees.length > 0) || !!item.claimed_by || o.bucket === 'claimed' || o.bucket === 'claimed_by_pr';
        states.add(claimed ? 'claimed' : 'unclaimed');
        if (o.coveringPR) states.add('covered-by-pr');
      }
      const hay = [item.title || '', '#' + (item.number || ''), labels.join(' '), item.body || ''].join('\n').toLowerCase();
      return {
        kind, item, repo: repo || item.repo || '', held: !!o.held, bucket: o.bucket || '',
        number: Number(item.number || 0), title: String(item.title || ''), url: String(item.url || ''),
        labels, labelsLower: new Set(labels.map(l => l.toLowerCase())), states, authorKind: hfAuthorKind(item, o.aiAuthor),
        createdAt: Date.parse(item.created_at || '') || 0, updatedAt: Date.parse(item.updated_at || item.created_at || '') || 0,
        haystack: hay
      };
    }
    function hfSplitTerms(filter) {
      const parsed = (filter.terms || []).map(hfParseTerm).filter(Boolean);
      return { positive: parsed.filter(t => !t.negate), negative: parsed.filter(t => t.negate) };
    }
    /** Labels + positive text terms form one criteria pool, combined by matchAll (AND) or any (OR). Negative terms always exclude. */
    function hfCriteriaMatch(filter, entry, terms) {
      const t = terms || hfSplitTerms(filter);
      if (t.negative.some(term => hfTermHits(term, entry.haystack))) return false;
      const checks = [];
      (filter.labels || []).forEach(l => checks.push(entry.labelsLower.has(String(l).toLowerCase())));
      t.positive.forEach(term => checks.push(hfTermHits(term, entry.haystack)));
      if (!checks.length) return true;
      return filter.matchAll ? checks.every(Boolean) : checks.some(Boolean);
    }
    function hfGroupMatch(selected, has) {
      return !selected || !selected.length || selected.some(has);
    }
    function hfGroupsMatch(filter, entry, skip) {
      return (skip === 'kinds' || hfGroupMatch(filter.kinds, k => k === entry.kind))
        && (skip === 'states' || hfGroupMatch(filter.states, k => entry.states.has(k)))
        && (skip === 'authors' || hfGroupMatch(filter.authors, k => k === entry.authorKind));
    }
    function hfEntryMatches(filter, entry, terms) {
      return hfGroupsMatch(filter, entry) && hfCriteriaMatch(filter, entry, terms);
    }
    function hfMatchingEntries(filter, entries) {
      const f = hfNormalizeFilter(filter);
      const terms = hfSplitTerms(f);
      return (entries || []).filter(e => hfEntryMatches(f, e, terms));
    }
    /** Priority index of the first filter label the entry carries; Infinity when none (or the filter has no labels). */
    function hfLabelRank(filter, entry) {
      const labels = (filter && filter.labels) || [];
      for (let i = 0; i < labels.length; i++) if (entry.labelsLower.has(String(labels[i]).toLowerCase())) return i;
      return Infinity;
    }
    /** Preset label priority first, then newest-first. */
    function hfSortEntries(entries, filter) {
      return (entries || []).slice().sort((a, b) => {
        const ra = hfLabelRank(filter, a), rb = hfLabelRank(filter, b);
        if (ra !== rb) return ra < rb ? -1 : 1;
        return (b.createdAt - a.createdAt) || (b.number - a.number);
      });
    }
    /**
     * Live facet counts. Labels: entries that pass the kind/state/author
     * groups and the negative terms and carry the label (independent of the
     * other positive criteria, so the chips read "how many would this add").
     * Other groups: entries matching the filter with that group swapped for
     * the single value.
     */
    function hfFacetCounts(filter, entries) {
      const f = hfNormalizeFilter(filter);
      const terms = hfSplitTerms(f);
      const labels = new Map();
      const kinds = {}, states = {}, authors = {};
      (entries || []).forEach(e => {
        if (hfGroupsMatch(f, e) && !terms.negative.some(t => hfTermHits(t, e.haystack))) {
          e.labels.forEach(l => {
            const key = l.toLowerCase();
            const cur = labels.get(key) || { label: l, count: 0 };
            cur.count++;
            labels.set(key, cur);
          });
        }
        const crit = hfCriteriaMatch(f, e, terms);
        if (crit && hfGroupsMatch(f, e, 'kinds')) kinds[e.kind] = (kinds[e.kind] || 0) + 1;
        if (crit && hfGroupsMatch(f, e, 'states')) e.states.forEach(s => { states[s] = (states[s] || 0) + 1; });
        if (crit && hfGroupsMatch(f, e, 'authors')) authors[e.authorKind] = (authors[e.authorKind] || 0) + 1;
      });
      f.labels.forEach(l => { if (!labels.has(l)) labels.set(l, { label: l, count: 0 }); });
      return { labels, kinds, states, authors };
    }
    function hfToggleIn(list, value) {
      const idx = list.indexOf(value);
      if (idx >= 0) list.splice(idx, 1); else list.push(value);
      return list;
    }
    function hfPresetFilter(preset) {
      return hfNormalizeFilter({ labels: (preset && preset.labels) || [], states: (preset && preset.states) || [] });
    }
    function hfLabelToneClass(label) {
      const key = String(label || '').toLowerCase();
      if (HF_LABEL_TONE[key]) return 'hf-label-' + HF_LABEL_TONE[key];
      if (/^hold(\/|$)|^on-hold$|hold\//.test(key)) return 'hf-label-hold';
      if (key === 'stage/stalled') return 'hf-label-hold';
      return '';
    }
    function hfDefaultStore() {
      return { v: HF_STORE_VERSION, activeKey: HF_DEFAULT_PRESET_KEY, active: null, saved: [], hiddenPresets: [], presetPins: {}, order: [] };
    }
    function hfNormalizeStore(raw) {
      const d = hfDefaultStore();
      if (!raw || typeof raw !== 'object') return d;
      d.activeKey = typeof raw.activeKey === 'string' ? raw.activeKey : d.activeKey;
      d.active = raw.active ? hfNormalizeFilter(raw.active) : null;
      d.saved = Array.isArray(raw.saved) ? raw.saved.filter(s => s && s.id && s.name).map(s => ({ id: String(s.id), name: String(s.name).slice(0, 60), filter: hfNormalizeFilter(s.filter), pinned: !!s.pinned })) : [];
      d.hiddenPresets = Array.isArray(raw.hiddenPresets) ? raw.hiddenPresets.map(String) : [];
      d.presetPins = raw.presetPins && typeof raw.presetPins === 'object' ? Object.assign({}, raw.presetPins) : {};
      d.order = Array.isArray(raw.order) ? raw.order.map(String) : [];
      return d;
    }
    /** Resolve the key ('preset:<key>' | 'saved:<id>' | '') to {name, filter, source}. */
    function hfResolveKey(store, presets, key) {
      if (key && key.indexOf('preset:') === 0) {
        const p = (presets || []).find(x => 'preset:' + x.key === key);
        if (p) return { key, name: p.label, filter: hfPresetFilter(p), source: 'preset', preset: p };
      }
      if (key && key.indexOf('saved:') === 0) {
        const s = (store.saved || []).find(x => 'saved:' + x.id === key);
        if (s) return { key, name: s.name, filter: hfNormalizeFilter(s.filter), source: 'saved', saved: s };
      }
      return { key: '', name: 'Custom', filter: hfEmptyFilter(), source: 'custom' };
    }
    /** The filter in force: the explicit edit if there is one, else the active preset/saved filter. */
    function hfActiveFilter(store, presets) {
      if (store.active) return hfNormalizeFilter(store.active);
      return hfResolveKey(store, presets, store.activeKey).filter;
    }
    function hfPinnedKeys(store, presets) {
      const keys = [];
      (presets || []).forEach(p => {
        const k = 'preset:' + p.key;
        const pinned = Object.prototype.hasOwnProperty.call(store.presetPins || {}, p.key) ? !!store.presetPins[p.key] : k === HF_DEFAULT_PRESET_KEY;
        if (pinned && (store.hiddenPresets || []).indexOf(p.key) < 0) keys.push(k);
      });
      (store.saved || []).forEach(s => { if (s.pinned) keys.push('saved:' + s.id); });
      const order = store.order || [];
      return keys.sort((a, b) => {
        const ia = order.indexOf(a), ib = order.indexOf(b);
        return (ia < 0 ? order.length : ia) - (ib < 0 ? order.length : ib) || keys.indexOf(a) - keys.indexOf(b);
      });
    }
    function hfNewId() {
      return 'f' + Date.now().toString(36) + Math.random().toString(36).slice(2, 7);
    }
    // ── Label filter (hf) pure helpers (end) ────────────────────────────────

    let _hfStore = null;
    let _hfStoreKey = '';
    const _hfUI = { editorOpen: false, confirmDelete: '', showHidden: false, draftTerm: '', textTimer: null, crossRepo: false };
    function hfStoreKey() { return HF_STORE_PREFIX + miniHiveKey(); }
    function hfStore() {
      const key = hfStoreKey();
      if (!_hfStore || _hfStoreKey !== key) {
        let raw = null;
        try { raw = JSON.parse(localStorage.getItem(key) || 'null'); } catch (e) { raw = null; }
        _hfStore = hfNormalizeStore(raw);
        _hfStoreKey = key;
      }
      return _hfStore;
    }
    function hfSaveStore() {
      try { localStorage.setItem(hfStoreKey(), JSON.stringify(hfStore())); } catch (e) { /* storage unavailable */ }
    }
    function hfPresets() {
      const list = ((window._lastStatus || {}).labelPresets) || [];
      return Array.isArray(list) ? list.filter(p => p && p.key) : [];
    }
    function hfVisiblePresets() {
      const hidden = hfStore().hiddenPresets || [];
      return hfPresets().filter(p => hidden.indexOf(p.key) < 0);
    }
    function hfCurrent() {
      const store = hfStore();
      const presets = hfPresets();
      const resolved = hfResolveKey(store, presets, store.activeKey);
      const filter = hfActiveFilter(store, presets);
      const modified = !!store.active && hfFilterSignature(store.active) !== hfFilterSignature(resolved.filter);
      return { store, presets, resolved, filter, modified, name: resolved.name };
    }
    /** Every open issue/PR of a repo as filter entries, from the snapshot the card already renders. */
    function hfRepoEntries(repo) {
      if (!repo) return [];
      const name = repo.full || repo.name || '';
      const aiAuthor = window._aiAuthor || '';
      const seen = new Set();
      const out = [];
      const push = (kind, item, opts) => {
        if (!item || !item.number) return;
        const key = kind + ':' + item.number;
        if (seen.has(key)) return;
        seen.add(key);
        out.push(hfEntry(kind, item, name, Object.assign({ aiAuthor }, opts)));
      };
      (repo.actionableIssues || []).forEach(i => push('issue', i, { coveringPR: !!issueCoveringPR(i) }));
      (repo.heldIssues || []).forEach(i => push('issue', i, { held: true, coveringPR: !!issueCoveringPR(i) }));
      (repo.nonActionableIssues || []).forEach(i => push('issue', i, { bucket: String(i.bucket || ''), coveringPR: !!issueCoveringPR(i) }));
      (repo.openPrs || []).forEach(p => push('pr', p, {}));
      (repo.heldPrs || []).forEach(p => push('pr', p, { held: true }));
      return out;
    }
    function hfAllEntries(repos) {
      return (repos || (window._lastStatus || {}).repos || []).reduce((acc, r) => acc.concat(hfRepoEntries(r)), []);
    }
    /** Filter with the still-being-typed text term folded in, for live counts. */
    function hfEffectiveFilter(filter) {
      const draft = hfParseTerm(_hfUI.draftTerm);
      if (!draft) return filter;
      const f = hfNormalizeFilter(filter);
      if (f.terms.indexOf(draft.raw) < 0) f.terms = f.terms.concat([draft.raw]);
      return f;
    }
    function hfSectionId(repoFull) {
      return 'hf-repo-' + String(repoFull || '').toLowerCase().replace(/[^a-z0-9]+/g, '-');
    }
    function hfSectionCollapsed(sectionId, count) {
      let raw = null;
      try { raw = localStorage.getItem(SECTION_LS_PREFIX + sectionId); } catch (e) { raw = null; }
      if (raw === '1') return true;
      if (raw === '0') return false;
      return count === 0;
    }
    const HF_PR_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true" fill="currentColor"><path d="M4.5 3a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0Zm0 10a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0ZM3.75 5.25v5.5h-1.5v-5.5h1.5ZM14.5 13a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0Zm-.75-2.25h-1.5V6.5a1 1 0 0 0-1-1H9.56l1.22 1.22-1.06 1.06L6.69 4.75l3.03-3.03 1.06 1.06L9.56 4h1.69a2.5 2.5 0 0 1 2.5 2.5v4.25Z"/></svg>';
    const HF_ISSUE_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true" fill="currentColor"><path d="M8 1.5a6.5 6.5 0 1 0 0 13 6.5 6.5 0 0 0 0-13ZM0 8a8 8 0 1 1 16 0A8 8 0 0 1 0 8Zm9 3a1 1 0 1 1-2 0 1 1 0 0 1 2 0Zm-.25-6.25v4h-1.5v-4h1.5Z"/></svg>';
    function hfRowLabels(entry, filter) {
      const wanted = (filter && filter.labels) || [];
      let show = entry.labels.filter(l => wanted.indexOf(l.toLowerCase()) >= 0);
      show.sort((a, b) => wanted.indexOf(a.toLowerCase()) - wanted.indexOf(b.toLowerCase()));
      if (!show.length) show = entry.labels.slice();
      const extra = show.length - HF_MAX_ROW_LABELS;
      return show.slice(0, HF_MAX_ROW_LABELS).map(l => `<span class="hf-label ${hfLabelToneClass(l)}" title="${esc(l)}">${esc(l)}</span>`).join('')
        + (extra > 0 ? `<span class="hf-label" title="${esc(entry.labels.join(', '))}">+${extra}</span>` : '');
    }
    function hfRowHtml(entry, filter) {
      const base = (window._githubBaseUrl || 'https://github.com') + '/' + entry.repo;
      const url = entry.url || (base + (entry.kind === 'pr' ? '/pull/' : '/issues/') + entry.number);
      const age = pillAge(entry.item.created_at);
      const tip = (entry.kind === 'pr' ? 'PR ' : 'Issue ') + '#' + entry.number + ': ' + entry.title + (entry.item.author ? ' — @' + entry.item.author : '') + (entry.labels.length ? ' — labels: ' + entry.labels.join(', ') : '') + (entry.held ? ' — held' : '') + (entry.bucket ? ' — ' + entry.bucket : '');
      return `<div class="hf-item hf-item-${entry.kind}"><a class="hf-item-link" href="${esc(url)}" target="_blank" rel="noopener" title="${esc(tip)}"><span class="hf-item-icon ${entry.kind}">${entry.kind === 'pr' ? HF_PR_ICON : HF_ISSUE_ICON}</span><span class="hf-item-num">#${entry.number}</span><span class="hf-item-title">${esc(entry.title)}</span></a><span class="hf-item-labels">${hfRowLabels(entry, filter)}</span><span class="hf-item-age" title="${esc(entry.item.created_at || '')}">${esc(age)}</span></div>`;
    }
    /** The collapsible "Filter" section inside one repo card. */
    function hfRepoSectionHtml(repo) {
      const cur = hfCurrent();
      const repoFull = repo.full || repo.name || '';
      const filter = hfEffectiveFilter(cur.filter);
      const entries = hfSortEntries(hfMatchingEntries(filter, hfRepoEntries(repo)), filter);
      const id = hfSectionId(repoFull);
      const collapsed = hfSectionCollapsed(id, entries.length);
      const name = cur.name + (cur.modified ? ' *' : '');
      const rows = entries.length ? entries.map(e => hfRowHtml(e, filter)).join('') : `<div class="hf-empty">${hfFilterIsEmpty(filter) ? 'No filter — open the editor to pick labels or text' : 'Nothing in ' + esc(repoFull) + ' matches this filter'}</div>`;
      return `<div class="hf-section${collapsed ? ' collapsed' : ''}" id="${id}" data-hf-repo="${esc(repoFull)}" data-hf-count="${entries.length}">`
        + `<div class="hf-header" role="button" tabindex="0" aria-expanded="${collapsed ? 'false' : 'true'}" aria-controls="${id}-body" data-action="hfToggleSection" data-keydown-action="hfToggleSection" data-keys="Enter, " data-arg0="${id}" data-arg-types="s" title="${esc(cur.filter.labels.length ? 'Labels: ' + cur.filter.labels.join(', ') : 'Filtered view')}${esc(cur.filter.terms.length ? ' · text: ' + cur.filter.terms.join(', ') : '')}"><span class="section-chevron${collapsed ? ' collapsed' : ''}">▼</span><span class="hf-title">Filter · ${esc(name)}</span><span class="badge-count hf-count"${entries.length ? '' : ' data-zero'}>${entries.length}</span><button type="button" class="hf-edit" data-action="hfOpenEditor" data-stop="1" title="Edit the filter (applies to every project card)">edit</button></div>`
        + `<div class="hf-body${collapsed ? ' collapsed' : ''}" id="${id}-body">${rows}</div></div>`;
    }
    function hfToggleSection(sectionId) {
      const section = document.getElementById(sectionId);
      if (!section) return;
      const body = section.querySelector('.hf-body');
      const collapsed = !section.classList.contains('collapsed');
      writeSectionCollapsedKey(sectionId, collapsed);
      if (!collapsed) {
        // Make the default explicit so an empty section the user opened stays open.
        try { localStorage.setItem(SECTION_LS_PREFIX + sectionId, '0'); } catch (e) { /* storage unavailable */ }
      }
      section.classList.toggle('collapsed', collapsed);
      if (body) body.classList.toggle('collapsed', collapsed);
      const chevron = section.querySelector('.section-chevron');
      if (chevron) chevron.classList.toggle('collapsed', collapsed);
      const header = section.querySelector('.hf-header');
      if (header) header.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    }
    function hfRerender() {
      if (typeof renderRepos === 'function' && window._lastStatus) renderRepos(window._lastStatus.repos || []);
      hfRenderEditor(true);
      hfRenderPinned();
      hfRenderCrossRepo();
    }
    function hfSetActive(filter) {
      const store = hfStore();
      store.active = hfNormalizeFilter(filter);
      hfSaveStore();
      hfRerender();
    }
    function hfActivate(key) {
      const store = hfStore();
      store.activeKey = String(key || '');
      store.active = null;
      _hfUI.confirmDelete = '';
      hfSaveStore();
      hfRerender();
    }
    function hfOpenEditor() {
      _hfUI.editorOpen = true;
      hfRenderEditor();
      const el = document.getElementById('hf-editor');
      if (el && el.scrollIntoView) el.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }
    function hfToggleEditor() {
      _hfUI.editorOpen = !_hfUI.editorOpen;
      hfRenderEditor();
    }
    function hfToggleLabel(label) {
      const f = hfCurrent().filter;
      hfToggleIn(f.labels, String(label).toLowerCase());
      hfSetActive(f);
    }
    function hfToggleFacet(group, value) {
      const f = hfCurrent().filter;
      if (!Array.isArray(f[group])) return;
      hfToggleIn(f[group], value);
      hfSetActive(f);
    }
    function hfSetMatchAll(on) {
      const f = hfCurrent().filter;
      f.matchAll = !!on;
      hfSetActive(f);
    }
    function hfAddTerm(raw) {
      const term = hfParseTerm(raw);
      _hfUI.draftTerm = '';
      if (!term) { hfRerender(); return; }
      const f = hfCurrent().filter;
      if (f.terms.indexOf(term.raw) < 0) f.terms.push(term.raw);
      hfSetActive(f);
      const input = document.getElementById('hf-term-input');
      if (input) { input.value = ''; input.focus(); }
    }
    function hfRemoveTerm(raw) {
      const f = hfCurrent().filter;
      f.terms = f.terms.filter(t => t !== raw);
      hfSetActive(f);
    }
    function hfTermInput(value) {
      _hfUI.draftTerm = String(value || '');
      if (_hfUI.textTimer) clearTimeout(_hfUI.textTimer);
      _hfUI.textTimer = setTimeout(() => {
        _hfUI.textTimer = null;
        hfRenderFacets();
        if (typeof renderRepos === 'function' && window._lastStatus) renderRepos(window._lastStatus.repos || []);
        if (typeof hfRenderPinned === 'function') hfRenderPinned();
      }, HF_TEXT_DEBOUNCE_MS);
    }
    function hfTermKey(event) {
      const input = event && event.target;
      if (!input) return;
      if (event.key === 'Enter' || event.key === ',') {
        event.preventDefault();
        hfAddTerm(input.value);
      } else if (event.key === 'Escape') {
        input.value = '';
        hfTermInput('');
      }
    }
    function hfReset() {
      const store = hfStore();
      store.activeKey = '';
      store.active = hfEmptyFilter();
      hfSaveStore();
      hfRerender();
    }
    function hfSaveAsNew() {
      const cur = hfCurrent();
      const input = document.getElementById('hf-name-input');
      const name = String((input && input.value) || '').trim();
      if (!name) { showToast('Give the filter a name first', 'error'); if (input) input.focus(); return; }
      if (hfFilterIsEmpty(cur.filter)) { showToast('Pick at least one label, facet or text term before saving', 'error'); return; }
      const id = hfNewId();
      cur.store.saved.push({ id, name: name.slice(0, 60), filter: hfNormalizeFilter(cur.filter), pinned: false });
      cur.store.activeKey = 'saved:' + id;
      cur.store.active = null;
      hfSaveStore();
      showToast('Saved filter “' + name + '”', 'success');
      hfRerender();
    }
    function hfUpdateSaved() {
      const cur = hfCurrent();
      if (cur.resolved.source !== 'saved') return;
      cur.resolved.saved.filter = hfNormalizeFilter(cur.filter);
      cur.store.active = null;
      hfSaveStore();
      showToast('Updated “' + cur.resolved.saved.name + '”', 'success');
      hfRerender();
    }
    function hfRenameSaved() {
      const cur = hfCurrent();
      if (cur.resolved.source !== 'saved') return;
      const input = document.getElementById('hf-name-input');
      const name = String((input && input.value) || '').trim();
      if (!name) { showToast('Enter the new name first', 'error'); if (input) input.focus(); return; }
      cur.resolved.saved.name = name.slice(0, 60);
      hfSaveStore();
      hfRerender();
    }
    function hfAskDelete(id) {
      _hfUI.confirmDelete = String(id || '');
      hfRenderEditor();
    }
    function hfDeleteSaved(id) {
      const store = hfStore();
      const victim = store.saved.find(s => s.id === id);
      store.saved = store.saved.filter(s => s.id !== id);
      if (store.activeKey === 'saved:' + id) { store.activeKey = ''; store.active = victim ? hfNormalizeFilter(victim.filter) : hfEmptyFilter(); }
      _hfUI.confirmDelete = '';
      hfSaveStore();
      if (victim) showToast('Deleted filter “' + victim.name + '”', 'info');
      hfRerender();
    }
    function hfHidePreset(key) {
      const store = hfStore();
      if (store.hiddenPresets.indexOf(key) < 0) store.hiddenPresets.push(key);
      if (store.activeKey === 'preset:' + key) { store.activeKey = ''; store.active = hfCurrent().filter; }
      hfSaveStore();
      hfRerender();
    }
    function hfUnhidePreset(key) {
      const store = hfStore();
      store.hiddenPresets = store.hiddenPresets.filter(k => k !== key);
      hfSaveStore();
      hfRerender();
    }
    function hfToggleShowHidden() {
      _hfUI.showHidden = !_hfUI.showHidden;
      hfRenderEditor();
    }
    function hfTogglePin(key) {
      const store = hfStore();
      const presets = hfPresets();
      const pinned = hfPinnedKeys(store, presets).indexOf(key) >= 0;
      if (key.indexOf('preset:') === 0) store.presetPins[key.slice('preset:'.length)] = !pinned;
      else if (key.indexOf('saved:') === 0) { const s = store.saved.find(x => 'saved:' + x.id === key); if (s) s.pinned = !pinned; }
      hfSaveStore();
      hfRerender();
    }
    function hfChipHtml(label, active, action, arg, count, tip, extraClass) {
      return `<button type="button" class="hf-chip${active ? ' active' : ''}${extraClass ? ' ' + extraClass : ''}" data-action="${action}" data-arg0="${esc(arg)}" data-arg-types="s" title="${esc(tip || label)}">${esc(label)}${count == null ? '' : ` <span class="hf-chip-count">${count}</span>`}</button>`;
    }
    function hfFacetsHtml(cur, entries) {
      const filter = hfEffectiveFilter(cur.filter);
      const counts = hfFacetCounts(filter, entries);
      const labelRows = Array.from(counts.labels.values()).sort((a, b) => {
        const sa = filter.labels.indexOf(a.label.toLowerCase()), sb = filter.labels.indexOf(b.label.toLowerCase());
        if ((sa >= 0) !== (sb >= 0)) return sa >= 0 ? -1 : 1;
        if (sa >= 0 && sb >= 0) return sa - sb;
        return b.count - a.count || a.label.localeCompare(b.label);
      });
      const labelChips = labelRows.length ? labelRows.map(r => hfChipHtml(r.label, filter.labels.indexOf(r.label.toLowerCase()) >= 0, 'hfToggleLabel', r.label, r.count, r.count + ' open item' + (r.count === 1 ? '' : 's') + ' carry ' + r.label)).join('') : '<span class="hf-muted">No labels on any open item yet</span>';
      const group = (title, defs, key, countMap, tipFn) => `<div class="hf-facet-group hf-facet-${key}"><div class="hf-facet-title">${title}</div><div class="hf-facet-chips">${defs.map(d => hfChipHtml(d.label, filter[key].indexOf(d.key) >= 0, 'hfToggleFacet' + key.charAt(0).toUpperCase() + key.slice(1), d.key, countMap[d.key] || 0, tipFn ? tipFn(d) : d.tip || d.label)).join('')}</div></div>`;
      const mode = `<span class="hf-mode" title="How selected labels and text terms combine"><button type="button" class="${filter.matchAll ? '' : 'active'}" data-action="hfSetMatchAll" data-arg0="false" data-arg-types="j">match any</button><button type="button" class="${filter.matchAll ? 'active' : ''}" data-action="hfSetMatchAll" data-arg0="true" data-arg-types="j">match all</button></span>`;
      return `<div class="hf-facet-group hf-facet-labels"><div class="hf-facet-title">Labels ${mode}</div><div class="hf-facet-chips">${labelChips}</div></div>`
        + group('Type', HF_KINDS, 'kinds', counts.kinds)
        + group('State', HF_STATES, 'states', counts.states)
        + group('Author', HF_AUTHORS, 'authors', counts.authors);
    }
    function hfTermsHtml(cur) {
      const terms = cur.filter.terms.map(t => {
        const p = hfParseTerm(t);
        const tip = p && p.negate ? 'Excludes items whose title, number, labels or body contain ' + t.slice(1) : (p && p.phrase ? 'Exact phrase' : 'Every word must appear');
        return `<span class="hf-chip active${p && p.negate ? ' hf-chip-exclude' : ''}" title="${esc(tip)}">${esc(t)}<button type="button" class="hf-chip-x" data-action="hfRemoveTerm" data-arg0="${esc(t)}" data-arg-types="s" aria-label="Remove ${esc(t)}">×</button></span>`;
      }).join('');
      return `<span class="hf-row-label">Text</span>${terms}<input id="hf-term-input" class="hf-input" type="text" autocomplete="off" spellcheck="false" placeholder="keyword · &quot;exact phrase&quot; · -exclude · Enter adds" value="${esc(_hfUI.draftTerm)}" data-input-action="hfTermInput" data-keydown-action="hfTermKey" data-keys="*" data-arg-types="v" title="Case-insensitive match over title, number, labels and body excerpt. Quote a phrase, prefix - to exclude. Enter or comma adds the chip.">`;
    }
    function hfManageHtml(cur) {
      const store = cur.store;
      const r = cur.resolved;
      const saved = r.source === 'saved';
      const preset = r.source === 'preset';
      const pinned = hfPinnedKeys(store, cur.presets).indexOf(r.key) >= 0;
      const bits = [];
      bits.push(`<input id="hf-name-input" class="hf-input" type="text" maxlength="60" placeholder="${saved ? 'New name' : 'Name this filter'}" value="${esc(saved ? r.name : '')}" data-keydown-action="hfNameKey" data-keys="Enter" data-arg-types="e" title="Name for Save as new / Rename">`);
      bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfSaveAsNew" title="Save the current selection as a new named filter">Save as new</button>`);
      if (saved && cur.modified) bits.push(`<button type="button" class="hv-btn btn-primary btn-sm" data-action="hfUpdateSaved" title="Overwrite “${esc(r.name)}” with the current selection">Update “${esc(r.name)}”</button>`);
      if (saved) bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfRenameSaved" title="Rename to the name typed on the left">Rename</button>`);
      if (saved) {
        if (_hfUI.confirmDelete === r.saved.id) bits.push(`<span class="hf-confirm">Delete “${esc(r.name)}”? <button type="button" class="hv-btn btn-danger btn-sm" data-action="hfDeleteSaved" data-arg0="${esc(r.saved.id)}" data-arg-types="s">Delete</button><button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfAskDelete" data-arg0="" data-arg-types="s">Cancel</button></span>`);
        else bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfAskDelete" data-arg0="${esc(r.saved.id)}" data-arg-types="s" title="Delete this saved filter">Delete</button>`);
      }
      if (preset) bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfHidePreset" data-arg0="${esc(r.preset.key)}" data-arg-types="s" title="Hide this built-in preset from the chips row (restore it from “hidden”)">Hide preset</button>`);
      if (saved || preset) bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfTogglePin" data-arg0="${esc(r.key)}" data-arg-types="s" aria-pressed="${pinned ? 'true' : 'false'}" title="${pinned ? 'Unpin from the sidebar' : 'Pin to the sidebar with a live count across all projects'}">${pinned ? '★ Pinned' : '☆ Pin to sidebar'}</button>`);
      bits.push(`<button type="button" class="hv-btn btn-secondary btn-sm" data-action="hfReset" title="Clear every facet and text term">Clear</button>`);
      if (cur.modified && !saved) bits.push(`<span class="hf-muted">Unsaved changes to ${esc(r.name)}</span>`);
      return bits.join('');
    }
    function hfNameKey(event) {
      if (!event || event.key !== 'Enter') return;
      event.preventDefault();
      if (hfCurrent().resolved.source === 'saved') hfRenameSaved(); else hfSaveAsNew();
    }
    function hfPresetsRowHtml(cur, entries) {
      const store = cur.store;
      const chips = [];
      const countFor = f => hfMatchingEntries(f, entries).length;
      hfVisiblePresets().forEach(p => {
        const key = 'preset:' + p.key;
        chips.push(hfChipHtml(p.label, store.activeKey === key, 'hfActivate', key, countFor(hfPresetFilter(p)), (p.description || p.label) + (p.labels && p.labels.length ? ' — ' + p.labels.join(', ') : ''), store.activeKey === key && cur.modified ? 'hf-chip-modified' : ''));
      });
      store.saved.forEach(s => {
        const key = 'saved:' + s.id;
        chips.push(hfChipHtml((s.pinned ? '★ ' : '') + s.name, store.activeKey === key, 'hfActivate', key, countFor(s.filter), 'Saved filter' + (s.filter.labels.length ? ' — ' + s.filter.labels.join(', ') : '') + (s.filter.terms.length ? ' — ' + s.filter.terms.join(', ') : ''), store.activeKey === key && cur.modified ? 'hf-chip-modified' : ''));
      });
      if (!store.activeKey) chips.push(hfChipHtml('Custom', true, 'hfActivate', '', countFor(cur.filter), 'The current unsaved selection'));
      const hidden = hfPresets().filter(p => store.hiddenPresets.indexOf(p.key) >= 0);
      let hiddenHtml = '';
      if (hidden.length) {
        hiddenHtml = `<button type="button" class="hf-link" data-action="hfToggleShowHidden">${hidden.length} hidden</button>`
          + (_hfUI.showHidden ? hidden.map(p => `<button type="button" class="hf-chip" data-action="hfUnhidePreset" data-arg0="${esc(p.key)}" data-arg-types="s" title="Restore the ${esc(p.label)} preset">↩ ${esc(p.label)}</button>`).join('') : '');
      }
      return `<span class="hf-row-label">Filter</span>${chips.join('')}${hiddenHtml}<button type="button" class="hf-chip-x hf-editor-close" data-action="hfToggleEditor" aria-label="Close the filter editor" title="Close">×</button>`;
    }
    function hfRenderFacets() {
      const el = document.getElementById('hf-facets');
      if (!el) return;
      const cur = hfCurrent();
      setIfChanged(el, hfFacetsHtml(cur, hfAllEntries()));
    }
    function hfRenderEditor(force) {
      const el = document.getElementById('hf-editor');
      if (!el) return;
      el.hidden = !_hfUI.editorOpen;
      const btn = document.getElementById('repos-filter-editor-btn');
      if (btn) btn.setAttribute('aria-pressed', _hfUI.editorOpen ? 'true' : 'false');
      if (!_hfUI.editorOpen) return;
      const cur = hfCurrent();
      const entries = hfAllEntries();
      const presetsEl = document.getElementById('hf-presets');
      const manageEl = document.getElementById('hf-manage');
      const termsEl = document.getElementById('hf-terms');
      const facetsEl = document.getElementById('hf-facets');
      // A poll-time repaint must not yank the caret out of a field the user is typing in.
      const active = document.activeElement;
      const typingIn = node => !force && !!active && active.tagName === 'INPUT' && !!node && node.contains(active);
      if (presetsEl) setIfChanged(presetsEl, hfPresetsRowHtml(cur, entries));
      if (manageEl && !typingIn(manageEl)) setIfChanged(manageEl, hfManageHtml(cur));
      if (termsEl && !typingIn(termsEl)) setIfChanged(termsEl, hfTermsHtml(cur));
      if (facetsEl) setIfChanged(facetsEl, hfFacetsHtml(cur, entries));
    }
    function hfSummaryLabel() {
      const cur = hfCurrent();
      return cur.name + (cur.modified ? ' *' : '');
    }
