    // ── Pinned filters in the sidebar + cross-repo view ──────────────────
    function hfNavKeyAttr(key) { return String(key).replace(/[^a-z0-9:_-]/gi, '_'); }
    /** Renders one sidebar entry per pinned filter with a live count across every project. */
    function hfRenderPinned() {
      const group = document.getElementById('hf-nav-group');
      const host = document.getElementById('hf-nav-items');
      if (!group || !host) return;
      const store = hfStore();
      const presets = hfPresets();
      const keys = hfPinnedKeys(store, presets);
      const entries = hfAllEntries();
      const html = keys.map(key => {
        const r = hfResolveKey(store, presets, key);
        if (r.source === 'custom') return '';
        const n = hfMatchingEntries(r.filter, entries).length;
        const active = _hfUI.crossRepo && store.activeKey === key;
        const tip = r.name + ': ' + n + ' matching issue' + (n === 1 ? '' : 's') + '/PR' + (n === 1 ? '' : 's') + ' across all projects — open the cross-repo view';
        return `<a class="oc-nav-item${active ? ' active' : ''}" data-hf-key="${esc(hfNavKeyAttr(key))}" data-action="hfOpenCrossRepo" data-arg0="${esc(key)}" data-arg-types="s" title="${esc(tip)}"><span class="oc-nav-emoji">📌</span><span class="oc-nav-text">${esc(r.name)}</span><span class="badge-count oc-nav-count hf-nav-count"${n === 0 ? ' data-zero' : ''}>${n}</span></a>`;
      }).join('');
      setIfChanged(host, html);
      group.hidden = keys.length === 0;
    }
    /** Open the cross-repo view for a pinned filter: activates it, shows the facet bar on top and swaps the card grid for one list grouped by repo. */
    function hfOpenCrossRepo(key) {
      const store = hfStore();
      if (key && store.activeKey !== key) { store.activeKey = String(key); store.active = null; _hfUI.confirmDelete = ''; hfSaveStore(); }
      // Navigate first: ocNavigate calls hfOnNavigate, which closes any open cross-repo view.
      if (typeof ocNavigate === 'function') ocNavigate('repos-section');
      _hfUI.crossRepo = true;
      _hfUI.editorOpen = true;
      document.querySelectorAll('.oc-nav-item.active').forEach(i => i.classList.remove('active'));
      const item = document.querySelector(`#hf-nav-items .oc-nav-item[data-hf-key="${hfNavKeyAttr(key)}"]`);
      if (item) item.classList.add('active');
      hfRerender();
    }
    function hfCloseCrossRepo() {
      if (!_hfUI.crossRepo) return;
      _hfUI.crossRepo = false;
      hfRerender();
    }
    /** Sidebar navigation elsewhere (including back to Projects) leaves the cross-repo view. */
    function hfOnNavigate() { if (_hfUI.crossRepo) { _hfUI.crossRepo = false; hfRenderCrossRepo(); hfRenderPinned(); } }
    const HF_CROSSREPO_SECTION_ID = 'hf-crossrepo-all';
    function hfCrossSectionId(repoFull) { return 'hf-x-' + String(repoFull || '').toLowerCase().replace(/[^a-z0-9]+/g, '-'); }
    function hfRenderCrossRepo() {
      const panel = document.getElementById('hf-crossrepo');
      const grid = document.getElementById('repos');
      const swarm = document.getElementById('swarm-panel');
      if (!panel) return;
      panel.hidden = !_hfUI.crossRepo;
      if (grid) grid.hidden = !!_hfUI.crossRepo;
      if (swarm) swarm.hidden = !!_hfUI.crossRepo;
      if (!_hfUI.crossRepo) return;
      const cur = hfCurrent();
      const filter = hfEffectiveFilter(cur.filter);
      const repos = (window._lastStatus || {}).repos || [];
      let total = 0;
      const groups = repos.map(repo => {
        const repoFull = repo.full || repo.name || '';
        const entries = hfSortEntries(hfMatchingEntries(filter, hfRepoEntries(repo)), filter);
        total += entries.length;
        if (!entries.length) return '';
        const id = hfCrossSectionId(repoFull);
        const collapsed = hfSectionCollapsed(id, entries.length);
        return `<div class="hf-crossrepo-group"><div class="hf-section${collapsed ? ' collapsed' : ''}" id="${id}" data-hf-count="${entries.length}">`
          + `<div class="hf-header" role="button" tabindex="0" aria-expanded="${collapsed ? 'false' : 'true'}" aria-controls="${id}-body" data-action="hfToggleSection" data-keydown-action="hfToggleSection" data-keys="Enter, " data-arg0="${id}" data-arg-types="s"><span class="section-chevron${collapsed ? ' collapsed' : ''}">▼</span><span>${esc(repoFull)}</span><span class="badge-count">${entries.length}</span></div>`
          + `<div class="hf-body${collapsed ? ' collapsed' : ''}" id="${id}-body">${entries.map(e => hfRowHtml(e, filter)).join('')}</div></div></div>`;
      }).join('');
      const name = cur.name + (cur.modified ? ' *' : '');
      // The whole list collapses like its per-repo groups do, keyed with the same section-collapse store so it survives reload.
      const allId = HF_CROSSREPO_SECTION_ID;
      const allCollapsed = hfSectionCollapsed(allId, total);
      const head = `<div class="hf-crossrepo-head hf-header" role="button" tabindex="0" aria-expanded="${allCollapsed ? 'false' : 'true'}" aria-controls="${allId}-body" data-action="hfToggleSection" data-keydown-action="hfToggleSection" data-keys="Enter, " data-arg0="${allId}" data-arg-types="s" title="${allCollapsed ? 'Expand' : 'Collapse'} the filtered list"><span class="section-chevron${allCollapsed ? ' collapsed' : ''}">▼</span><span class="hf-crossrepo-title">🔎 ${esc(name)}</span><span class="badge-count"${total === 0 ? ' data-zero' : ''}>${total}</span><span class="hf-muted">across ${repos.length} project${repos.length === 1 ? '' : 's'} — refine with the facet bar above and save to keep the change · collapse with the chevron, or ← Project cards to close (reopen from 📌 ${esc(cur.name)} in the sidebar)</span><button type="button" class="hv-btn btn-secondary btn-sm hf-editor-close" data-action="hfCloseCrossRepo" data-stop="1" title="Close this list and show the project cards again">← Project cards</button></div>`;
      const empty = total === 0 ? `<div class="hf-empty">${hfFilterIsEmpty(filter) ? 'No filter — pick labels, facets or text above' : 'Nothing matches this filter in any project'}</div>` : '';
      setIfChanged(panel, `<div class="hf-section hf-crossrepo-all${allCollapsed ? ' collapsed' : ''}" id="${allId}" data-hf-count="${total}">${head}<div class="hf-body hf-crossrepo-body${allCollapsed ? ' collapsed' : ''}" id="${allId}-body">${empty}${groups}</div></div>`);
    }
    function hfToggleFacetKinds(value) { hfToggleFacet('kinds', value); }
    function hfToggleFacetStates(value) { hfToggleFacet('states', value); }
    function hfToggleFacetAuthors(value) { hfToggleFacet('authors', value); }

