// Settings → Workflows → Marketplace (hivecommons/hive#17620, RFC phase 7).
    //
    // Browses the hub's workflow-model catalogue through the spoke proxy
    // (GET /api/marketplace/workflows, ETag-cached server-side), imports an
    // entry into the local library (the spoke re-verifies the bundle and
    // recompiles the model; the imported model is pinned name@version and
    // no binding moves), and lets an owner publish a local model back to
    // the hub. Catalogue tiers: built-in · curated · ingested · unreviewed ·
    // local-published. Ratings/adoption columns are reserved and render
    // "—" until the hub fills them.
    //
    // Render functions are pure (node-testable in
    // workflows_marketplace_ui_test.go); state lives in _wfMkt.
    const WF_MKT_TIERS = ['built-in', 'curated', 'ingested-unreviewed', 'local-published'];
    const WF_MKT_TIER_LABELS = {
      'built-in': 'built-in',
      'curated': 'curated',
      'ingested-unreviewed': 'ingested · unreviewed',
      'local-published': 'local-published',
    };
    const WF_MKT_TIER_TIPS = {
      'built-in': 'Ships with hive; identical on every spoke.',
      'curated': 'Reviewed and vouched for by a hub maintainer. Never overwritten by ingest; a newer ingest only sets a flag.',
      'ingested-unreviewed': 'Derived nightly from a CNCF project\'s pipeline files; nobody has reviewed it. Compiles with warnings.',
      'local-published': 'Published by another hive from its local library. Verified by the hub, not reviewed.',
    };
    const _wfMkt = { loaded: false, loading: false, index: null, stale: false, error: '', fetchedAt: '', search: '', tier: '', busy: '' };

    function wfMktTierBadge(tier) {
      const t = WF_MKT_TIER_LABELS[tier] ? tier : 'local-published';
      return `<span class="wf-prov wf-prov-${esc(t)}" title="${esc(WF_MKT_TIER_TIPS[t] || '')}">${esc(WF_MKT_TIER_LABELS[t])}</span>`;
    }

    // Reserved read-only cells: "—" until the hub populates the object.
    function wfMktReservedCell(obj, key, suffix) {
      if (!obj || typeof obj !== 'object' || obj[key] === undefined || obj[key] === null || obj[key] === '') return '<span class="text-muted">—</span>';
      return esc(String(obj[key])) + (suffix ? esc(suffix) : '');
    }

    // Case-insensitive match on name, description and source; tier filter
    // exact. Pure.
    function wfMktFilter(entries, search, tier) {
      const q = String(search || '').trim().toLowerCase();
      return (Array.isArray(entries) ? entries : []).filter(e => {
        if (!e) return false;
        if (tier && e.tier !== tier) return false;
        if (!q) return true;
        return [e.name, e.version, e.description, e.source, e.publishedBy].some(v => String(v || '').toLowerCase().includes(q));
      });
    }

    // Is name@version already in the local library? Imported models keep
    // the catalogue's version when free, so a match means "installed".
    function wfMktInstalled(entry, workflows) {
      return (Array.isArray(workflows) ? workflows : []).some(w => w && w.name === entry.name && w.version === entry.version);
    }

    function renderWorkflowMarketplaceRows(entries, workflows, owner, busyKey) {
      const list = Array.isArray(entries) ? entries.slice() : [];
      list.sort((a, b) => {
        const ta = WF_MKT_TIERS.indexOf(a.tier), tb = WF_MKT_TIERS.indexOf(b.tier);
        if (ta !== tb) return (ta < 0 ? 99 : ta) - (tb < 0 ? 99 : tb);
        return (a.name + '@' + a.version).localeCompare(b.name + '@' + b.version);
      });
      if (!list.length) return '<tr><td colspan="9" class="text-muted">No catalogue entries match.</td></tr>';
      return list.map(e => {
        const key = e.name + '@' + e.version;
        const installed = wfMktInstalled(e, workflows);
        const busy = busyKey === key;
        let action;
        if (installed) action = `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="openWorkflowLibraryEntry" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Open</button>`;
        else if (owner) action = `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfMktImport" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}"${busy ? ' disabled' : ''} title="Fetch the bundle, verify its hash, recompile the model locally and save it to the library. Nothing is bound.">${busy ? 'Importing…' : 'Import'}</button>`;
        else action = '<span class="text-muted" title="Owner role is required to import.">—</span>';
        return `<tr data-wf-mkt="${esc(key)}">` +
          `<td><span class="wf-key mono">${esc(key)}</span>${e.description ? `<div class="wf-desc text-muted">${esc(e.description)}</div>` : ''}</td>` +
          `<td>${wfMktTierBadge(e.tier)}${e.newerIngestAvailable ? '<div><span class="wf-mkt-flag" title="A later ingest of this project exists in the catalogue. This curated version is unchanged; review the newer ingest before switching.">newer ingest available</span></div>' : ''}</td>` +
          `<td>${esc(e.source || '')}${e.sourceSHA ? `<div class="mono text-muted wf-mkt-sub" title="${esc(e.sourceSHA)}">${esc(String(e.sourceSHA).slice(0, 12))}</div>` : ''}</td>` +
          `<td class="wf-num">${esc(String(e.states || 0))} / ${esc(String(e.columns || 0))}</td>` +
          `<td>${e.publishedAt ? esc(formatDashboardDateTime(e.publishedAt)) : '—'}${e.publishedBy ? `<div class="text-muted wf-mkt-sub">${esc(e.publishedBy)}</div>` : ''}</td>` +
          `<td class="wf-num">${wfMktReservedCell(e.adoption, 'hives')}</td>` +
          `<td class="wf-num">${wfMktReservedCell(e.ratings, 'average')}</td>` +
          `<td>${installed ? '<span class="wf-compiled wf-compiled-ok">in library</span>' : '<span class="text-muted">—</span>'}</td>` +
          `<td class="kc-actions">${action}</td></tr>`;
      }).join('');
    }

    function renderWorkflowMarketplace(state, workflows, owner) {
      const st = state || {};
      const entries = (st.index && Array.isArray(st.index.entries)) ? st.index.entries : [];
      const shown = wfMktFilter(entries, st.search, st.tier);
      const counts = {};
      entries.forEach(e => { counts[e.tier] = (counts[e.tier] || 0) + 1; });
      let banner = '';
      if (st.stale) banner = `<div class="wf-mkt-banner wf-mkt-stale" role="status">Showing the last catalogue this hive cached${st.fetchedAt ? ' (' + esc(formatDashboardDateTime(st.fetchedAt)) + ')' : ''}: the hub could not be reached${st.error ? ' — ' + esc(st.error) : ''}.</div>`;
      else if (st.error) banner = `<div class="wf-mkt-banner wf-ingest-error" role="alert">${esc(st.error)}</div>`;
      const tierOpts = ['<option value="">all tiers</option>'].concat(WF_MKT_TIERS.map(t =>
        `<option value="${esc(t)}"${st.tier === t ? ' selected' : ''}>${esc(WF_MKT_TIER_LABELS[t])}${counts[t] ? ' (' + counts[t] + ')' : ''}</option>`)).join('');
      const toolbar = '<div class="wf-mkt-toolbar">' +
        `<input type="search" class="wf-mkt-search" placeholder="Search name, description, source…" value="${esc(st.search || '')}" aria-label="Search catalogue" data-input-action="wfMktSetSearch" data-arg-types="v">` +
        `<select class="wf-mkt-tier" aria-label="Filter by tier" data-change-action="wfMktSetTier" data-arg-types="v">${tierOpts}</select>` +
        `<span class="text-muted wf-mkt-count">${shown.length} of ${entries.length}</span>` +
        `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfMktReload" data-arg0="1"${st.loading ? ' disabled' : ''} title="Ask the hub for the current catalogue (revalidates the spoke's cached copy).">${st.loading ? 'Loading…' : 'Reload'}</button>` +
        (owner ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfMktRefreshCatalogue"${st.busy === 'refresh' ? ' disabled' : ''} title="Ask the hub to re-ingest the CNCF corpus now. The hub accepts this hive's credential; one full refresh per hour across hives.">${st.busy === 'refresh' ? 'Requesting…' : 'Refresh catalogue'}</button>` : '') +
        '</div>';
      let body;
      if (!st.loaded && st.loading) body = '<p class="text-muted">Loading the catalogue…</p>';
      else body = '<table class="kc-table wf-table wf-mkt-table"><thead><tr><th>Model</th><th>Tier</th><th>Source</th><th title="states / columns">S / C</th><th>Published</th><th title="Hives that imported this entry (reserved; the hub does not report it yet).">Adoption</th><th title="Reserved for operator ratings; the hub does not collect them yet.">Rating</th><th>Local</th><th></th></tr></thead>' +
        '<tbody>' + renderWorkflowMarketplaceRows(shown, workflows, owner, st.busy) + '</tbody></table>';
      return '<div class="config-section" data-section="workflows-marketplace">' +
        '<h3>Marketplace</h3>' +
        '<p class="text-muted fs-base">Workflow models published to the hub: built-in and curated lifecycles, nightly ingests of CNCF project pipelines (unreviewed), and models other hives published. Importing verifies the bundle\'s hash, recompiles the model on this hive and saves it pinned to its catalogue version; no repository is bound and nothing moves automatically. The hub to browse is ' + yamlHint('workflow.marketplace.url') + ' (defaults to this hive\'s hub); the spoke re-checks its cached copy every ' + yamlHint('workflow.marketplace.refresh_interval') + '. Turn the catalogue off with ' + yamlHint('workflow.marketplace.enabled') + '.</p>' +
        banner + toolbar + body + '</div>';
    }

    // Publish button for library rows and the detail view; '' unless the
    // caller is an owner and the model is publishable (not built-in).
    function wfMktPublishButton(entry, owner) {
      if (!owner || !entry || wfProvenanceOf(entry) === 'built-in' || entry.compiled === false) return '';
      return `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfMktPublish" data-arg0="${esc(entry.name)}" data-arg1="${esc(entry.version)}" title="Export a signed bundle of this model and publish it to the hub marketplace as local-published.">Publish</button>`;
    }

    function wfMktRerender() { if (typeof wfLibRerender === 'function') wfLibRerender(); }
    function wfMktSetSearch(value) { _wfMkt.search = String(value || ''); wfMktRerender(); }
    function wfMktSetTier(value) { _wfMkt.tier = String(value || ''); wfMktRerender(); }

    async function wfMktLoad(force) {
      if (_wfMkt.loading) return;
      _wfMkt.loading = true; _wfMkt.error = '';
      wfMktRerender();
      try {
        const res = await fetch('/api/marketplace/workflows' + (force ? '?refresh=1' : ''), { headers: _inceptionAuthHeaders() });
        let data = null;
        try { data = await res.json(); } catch (e) { data = null; }
        if (!res.ok && !(data && data.index)) throw new Error((data && data.error) || ('HTTP ' + res.status));
        _wfMkt.index = data.index || { entries: [] };
        _wfMkt.stale = !!data.stale;
        _wfMkt.fetchedAt = data.fetchedAt || '';
        _wfMkt.error = data.error || '';
        _wfMkt.loaded = true;
      } catch (err) {
        _wfMkt.error = 'Could not load the catalogue: ' + err.message;
        _wfMkt.loaded = true;
      }
      _wfMkt.loading = false;
      wfMktRerender();
    }

    function wfMktReload() { wfMktLoad(true); }

    async function wfMktImport(name, version) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to import.', 'error'); return; }
      const key = name + '@' + version;
      _wfMkt.busy = key; wfMktRerender();
      try {
        const res = await fetch('/api/marketplace/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version) + '/import', { method: 'POST', headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        const e = data.entry || {};
        showToast('Imported ' + key + ' as ' + e.name + '@' + e.version + ' (verified, compiled locally)');
        _wfMkt.busy = '';
        if (typeof _workflowState !== 'undefined') _workflowState.loaded = false;
        if (typeof loadWorkflowLibrary === 'function') await loadWorkflowLibrary();
        openWorkflowLibraryEntry(e.name, e.version);
      } catch (err) {
        _wfMkt.busy = '';
        showToast('Import failed: ' + err.message, 'error');
      }
      wfMktRerender();
    }

    async function wfMktPublish(name, version) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to publish.', 'error'); return; }
      const ok = await hiveConfirm({ title: 'Publish ' + name + '@' + version + ' to the hub?', message: 'A signed bundle (model + compiled profile + hash) is sent to the hub marketplace under this hive\'s identity as tier local-published. Catalogue entries are immutable: once published, this name@version cannot be changed, only superseded by a new version.', confirmLabel: 'Publish' });
      if (!ok) return;
      try {
        const res = await fetch('/api/marketplace/workflows/publish', { method: 'POST', headers: Object.assign({ 'Content-Type': 'application/json' }, _inceptionAuthHeaders()), body: JSON.stringify({ name: name, version: version }) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        showToast('Published ' + (data.published || name + '@' + version) + ' (' + (data.tier || 'local-published') + ')');
        _wfMkt.loaded = false;
      } catch (err) {
        showToast('Publish failed: ' + err.message, 'error');
      }
    }

    async function wfMktRefreshCatalogue() {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required.', 'error'); return; }
      _wfMkt.busy = 'refresh'; wfMktRerender();
      try {
        const res = await fetch('/api/marketplace/refresh', { method: 'POST', headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        showToast('Hub refresh requested; the catalogue updates as repositories are re-ingested.');
      } catch (err) {
        showToast('Refresh failed: ' + err.message, 'error');
      }
      _wfMkt.busy = '';
      wfMktRerender();
    }

    // Called from renderGovWorkflows when the Marketplace view is active.
    function renderWorkflowMarketplaceView(owner) {
      if (!_wfMkt.loaded && !_wfMkt.loading) wfMktLoad(false);
      return renderWorkflowMarketplace(_wfMkt, (typeof _workflowState !== 'undefined' && _workflowState.workflows) || [], owner);
    }
