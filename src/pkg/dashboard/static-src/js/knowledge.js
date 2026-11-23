    // ── Knowledge Base ──
    let _publicKnowledgeState = { loaded: false, enabled: false, tags: [], source: 'env', endpoint: '/mcp/knowledge', url: '' };
    let _publicKnowledgeConfirmEnable = false;
    let _publicKnowledgeMessage = '';

    function viewerIsOwner() {
      return window._hiveRole === 'owner';
    }

    function publicKnowledgeTypesText() {
      return 'pattern, gotcha, regression, test_scaffold, integration, coverage_rule, general';
    }

    function publicKnowledgeTagsValue() {
      return ((_publicKnowledgeState && _publicKnowledgeState.tags) || []).join(', ');
    }

    async function fetchPublicKnowledgeSetting() {
      try {
        const r = await fetch('/api/knowledge/public');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const data = await r.json();
        _publicKnowledgeState = Object.assign({ loaded: true, enabled: false, tags: [], source: 'env', endpoint: '/mcp/knowledge', url: '' }, data || {}, { loaded: true });
        _publicKnowledgeConfirmEnable = false;
        renderPublicKnowledgeControls();
      } catch (e) {
        _publicKnowledgeState.loaded = false;
        renderPublicKnowledgeControls();
      }
    }

    function renderPublicKnowledgeControl(context) {
      const st = _publicKnowledgeState || {};
      const on = !!st.enabled;
      const owner = viewerIsOwner();
      const label = 'Public read-only MCP: ' + (on ? 'On' : 'Off');
      const source = st.source === 'setting' ? 'dashboard setting' : 'environment';
      let html = '<div class="public-knowledge-control public-knowledge-control--' + esc(context) + '" data-public-knowledge-placement="' + esc(context) + '">';
      html += '<span class="public-knowledge-label">Public read-only MCP</span>';
      html += '<span class="public-knowledge-pill" data-enabled="' + (on ? 'true' : 'false') + '" title="' + (owner ? 'Source: ' + esc(source) : 'Only the hive owner can change this') + '">' + (on ? 'On' : 'Off') + '</span>';
      if (!owner) {
        html += '<span class="muted" title="Only the hive owner can change this">Only the hive owner can change this</span>';
        html += '</div>';
        return html;
      }
      html += '<button type="button" class="public-knowledge-switch" role="switch" aria-checked="' + (on ? 'true' : 'false') + '" data-action="publicKnowledgeRequestToggle" data-arg0="' + (!on) + '" data-arg-types="j,t" data-stop="1" title="' + esc(label) + '"><span class="public-knowledge-switch-knob" aria-hidden="true"></span><span>' + (on ? 'Turn off' : 'Turn on') + '</span></button>';
      html += '<label class="public-knowledge-url"><span class="muted">Tags</span><input class="public-knowledge-tags" type="text" value="' + esc(publicKnowledgeTagsValue()) + '" placeholder="optional tags, comma-separated" data-keydown-action="publicKnowledgeSaveTagsOnEnter" data-keys="Enter" data-arg-types="v"></label>';
      html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="publicKnowledgeSaveTags" data-arg-types="t" data-stop="1">Save tags</button>';
      const endpoint = st.url || st.endpoint || '/mcp/knowledge';
      html += '<span class="public-knowledge-url"><code>' + esc(endpoint) + '</code><button type="button" class="hv-btn btn-secondary btn-sm" data-action="publicKnowledgeCopyURL" data-stop="1">Copy URL</button></span>';
      if (_publicKnowledgeConfirmEnable && !on) {
        html += '<div class="public-knowledge-confirm">Anyone with the URL can read operational facts (types ' + esc(publicKnowledgeTypesText()) + ') — no credentials required. <button type="button" class="hv-btn btn-primary btn-sm" data-action="publicKnowledgeConfirmEnable" data-arg-types="t" data-stop="1">Enable public read-only MCP</button> <button type="button" class="hv-btn btn-secondary btn-sm" data-action="publicKnowledgeCancelEnable" data-stop="1">Cancel</button></div>';
      }
      if (_publicKnowledgeMessage) html += '<span class="muted">' + esc(_publicKnowledgeMessage) + '</span>';
      html += '</div>';
      return html;
    }

    function renderPublicKnowledgeControls() {
      document.querySelectorAll('[data-public-knowledge-control]').forEach(el => {
        el.innerHTML = renderPublicKnowledgeControl(el.dataset.publicKnowledgeControl || 'header');
      });
    }

    function publicKnowledgeDraftTags(value) {
      _publicKnowledgeMessage = value ? 'Unsaved tag changes' : '';
      renderPublicKnowledgeControls();
    }

    function publicKnowledgeTagsFromDOM(el) {
      const root = el && el.closest ? el.closest('.public-knowledge-control') : null;
      const input = (root && root.querySelector('.public-knowledge-tags')) || document.querySelector('.public-knowledge-tags');
      return (input && input.value ? input.value : '').split(',').map(t => t.trim()).filter(Boolean);
    }

    async function publicKnowledgeSave(enabled, tags) {
      const r = await fetch('/api/knowledge/public', {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({enabled: !!enabled, tags: tags || []})
      });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      _publicKnowledgeState = Object.assign({ loaded: true }, data || {});
      _publicKnowledgeConfirmEnable = false;
      _publicKnowledgeMessage = 'Saved';
      renderPublicKnowledgeControls();
    }

    function publicKnowledgeRequestToggle(enable, el) {
      if (enable && !_publicKnowledgeState.enabled) {
        _publicKnowledgeConfirmEnable = true;
        _publicKnowledgeMessage = '';
        renderPublicKnowledgeControls();
        return;
      }
      publicKnowledgeSave(false, publicKnowledgeTagsFromDOM(el)).catch(e => showToast('Public knowledge update failed: ' + e.message, 'error'));
    }

    function publicKnowledgeConfirmEnable(el) {
      publicKnowledgeSave(true, publicKnowledgeTagsFromDOM(el)).catch(e => showToast('Public knowledge update failed: ' + e.message, 'error'));
    }

    function publicKnowledgeCancelEnable() {
      _publicKnowledgeConfirmEnable = false;
      renderPublicKnowledgeControls();
    }

    function publicKnowledgeSaveTags(el) {
      publicKnowledgeSave(!!_publicKnowledgeState.enabled, publicKnowledgeTagsFromDOM(el)).catch(e => showToast('Public knowledge tags failed: ' + e.message, 'error'));
    }

    function publicKnowledgeSaveTagsOnEnter(value) {
      publicKnowledgeSave(!!_publicKnowledgeState.enabled, (value || '').split(',').map(t => t.trim()).filter(Boolean)).catch(e => showToast('Public knowledge tags failed: ' + e.message, 'error'));
    }

    function publicKnowledgeCopyURL() {
      const text = _publicKnowledgeState.url || _publicKnowledgeState.endpoint || '/mcp/knowledge';
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(() => showToast('Public MCP URL copied', 'success')).catch(() => showToast(text, 'info'));
      } else {
        showToast(text, 'info');
      }
    }

    let _kbCache = null;
    let _kbSelectedLayer = 'all';
    let _kbSelectedType = '';
    let _kbSearchQuery = '';
    let _kbSelectedFact = null;
    let _kbFacts = [];
    const _kbPageSize = 50;
    let _kbPageOffset = 0;
    let _kbServerPaged = false;
    let _kbHasMore = false;
    let _kbRequest = 0;
    const KB_POLL_MS = 30000;
    const KB_LAYER_ICONS = { personal: '🔒', project: '📁', org: '🏢', community: '🌍' };
    const KB_LAYER_LABELS = { personal: 'Personal', project: 'Project', org: 'Org', community: 'Community' };
    // The automation vault the bead synthesizer owns. It is never a valid user
    // import target and is filtered out of every channel picker (issue #3581).
    const RESERVED_KB_CHANNEL = 'bead-synth-wiki';
    const KB_FACT_TYPES = ['pattern', 'gotcha', 'regression', 'decision', 'test_scaffold', 'integration', 'coverage_rule'];

    function kbTotalFactsFromStats(stats) {
      return ((stats && stats.layers) || []).reduce((sum, l) => sum + (Number(l && l.total_pages) || 0), 0);
    }
    function kbFactsLabel(n) {
      const total = Number(n) || 0;
      return formatNumber(total) + ' fact' + (total === 1 ? '' : 's');
    }

    // Validate the same layer totals used by the displayed facts badge.
    // Missing layers are unknown, not an empty knowledge base.
    function kbSidebarFactCount(stats) {
      if (!stats || typeof stats.enabled !== 'boolean') return null;
      if (!stats.enabled) return 0;
      if (!Array.isArray(stats.layers) || stats.layers.some(l => !l || !Number.isSafeInteger(l.total_pages) || l.total_pages < 0)) return null;
      const count = kbTotalFactsFromStats(stats);
      return Number.isSafeInteger(count) ? count : null;
    }

    // ── Document Sources ──
    const DOC_CONTENT_ICONS = { 'text/html': '🌐', 'application/pdf': '📄', 'text/plain': '📝', 'text/markdown': '📝', 'application/vnd.openxmlformats-officedocument.wordprocessingml.document': '📋' };

    function confColor(c) {
      if (c >= 0.9) return 'var(--green)';
      if (c >= 0.7) return 'var(--yellow)';
      return 'var(--red)';
    }

    async function toggleKnowledge(enabled) {
      try {
        await fetch('/api/knowledge/enabled', { method: 'PUT', headers: {'Content-Type':'application/json'}, body: JSON.stringify({enabled}) });
        if (enabled) _kbSetupView = 'none';
        if (!enabled) _kbSetupView = 'choose';
        await fetchKnowledgeStats();
      } catch (e) { console.error('toggle knowledge failed', e); }
    }

    let _beadSynthStatus = null;
    async function toggleBeadSynth(enabled) {
      try {
        await fetch('/api/knowledge/bead-synthesizer/enabled', { method: 'PUT', headers: {'Content-Type':'application/json'}, body: JSON.stringify({enabled}) });
        await fetchBeadSynthStatus();
        await fetchKnowledgeStats();
      } catch (e) { console.error('toggle bead synth failed', e); }
    }
    async function fetchBeadSynthStatus() {
      try {
        const r = await fetch('/api/knowledge/bead-synthesizer');
        if (r.ok) _beadSynthStatus = await r.json();
      } catch (e) { _beadSynthStatus = null; }
    }

    let _kbInitialLoad = true;
    let _kbRendered = false;
    let _kbGraphView = false;
    let _kbGraphData = null;

    function setSidebarCountBadge(el, count, title) {
      if (!el) return;
      const n = Number(count) || 0;
      el.hidden = false;
      el.textContent = n;
      if (n === 0) el.setAttribute('data-zero', '');
      else el.removeAttribute('data-zero');
      if (title) el.title = title;
      else el.removeAttribute('title');
    }

    /* Sidebar "x/y" pills. Overview mirrors the Overview tile's "Total open
       issues" / "Total open PRs" (overviewSidebarPill in renderOverviewKPIs,
       same /api/status snapshot and the same render pass as the tiles);
       Governor shows the shared Actionable now split from /api/status
       (actionableNow.issues / .prs, the same numbers the Governor tiles
       print). Both stay hidden until their feed has answered once.
       sidebarPairBadgeText() is pure so sidebar_pair_badge_test.go can
       execute it under node. The optional prefix names the pair — only
       Overview passes one ("BP = " for board pressure, #17240); the
       Governor and Contributors pills stay a bare "x/y". */
    function sidebarPairBadgeText(issues, prs, prefix) {
      return (prefix || '') + (Number(issues) || 0) + '/' + (Number(prs) || 0);
    }

    function setSidebarPairBadge(el, issues, prs, title, prefix) {
      if (!el) return;
      const i = Number(issues) || 0;
      const p = Number(prs) || 0;
      el.hidden = false;
      el.textContent = sidebarPairBadgeText(i, p, prefix);
      if (i === 0 && p === 0) el.setAttribute('data-zero', '');
      else el.removeAttribute('data-zero');
      if (title) el.title = title;
      else el.removeAttribute('title');
    }

    async function fetchKnowledgeStats() {
      try {
        const [r, , , docsR] = await Promise.all([fetch('/api/knowledge/stats'), fetchBeadSynthStatus(), fetchFactHistory(), fetch('/api/knowledge/documents')]);
        // Header "syncing: <name> n/m" indicator; the tick switches itself to
        // a 2.5s poll while a connector sync runs and back off when it ends.
        kbSyncProgressTick().catch(() => {});
        if (!r.ok) return;
        const prev = _kbCache;
        _kbCache = await r.json();
        _kbCache._documents = (docsR && docsR.ok) ? await docsR.json() : [];
        setSectionSummary('knowledge-section', kbFactsLabel(kbTotalFactsFromStats(_kbCache)));
        const kbBadge = document.getElementById('knowledge-sidebar-badge');
        if (kbBadge) {
          const kbFacts = kbSidebarFactCount(_kbCache);
          const kbShown = kbTotalFactsFromStats(_kbCache);
          const hasLayers = Array.isArray(_kbCache.layers);
          const title = kbFacts !== null ? kbFactsLabel(kbShown) : hasLayers
            ? kbFactsLabel(kbShown) + ' known; some layer counts unavailable'
            : 'Knowledge fact count unavailable';
          // Toggling priming changes what the badge shows, not stored facts.
          if (prev && prev.enabled !== _kbCache.enabled) navCountDelta(kbBadge, null);
          // Partial layer stats still refresh the section and its known
          // facts. Only the delta baseline needs a complete numeric total.
          if (kbFacts !== null || hasLayers) setSidebarCountBadge(kbBadge, _kbCache.enabled ? kbShown : 0, title);
          else kbBadge.title = title;
          navCountDelta(kbBadge, kbFacts, title);
        }
        if (_kbInitialLoad && _kbCache && _kbCache.enabled) {
          _kbSetupView = 'none';
        }
        _kbInitialLoad = false;
        const enabledChanged = !prev || prev.enabled !== _kbCache.enabled || JSON.stringify(prev.primer || null) !== JSON.stringify(_kbCache.primer || null);
        const layersChanged = !prev || JSON.stringify((prev.layers||[]).map(l=>l.type)) !== JSON.stringify((_kbCache.layers||[]).map(l=>l.type));
        const docsChanged = !prev || JSON.stringify((prev._documents||[]).map(d=>d.slug)) !== JSON.stringify((_kbCache._documents||[]).map(d=>d.slug));
        if (!_kbRendered || enabledChanged || layersChanged || docsChanged) {
          _kbRendered = true;
          renderKnowledge();
        }
      } catch (e) { /* knowledge unavailable */ }
    }

    function kbSelectType(type) {
      _kbSelectedType = type;
      const row = document.getElementById('kb-type-pills');
      if (row) {
        for (const pill of row.children) {
          pill.classList.toggle('active', pill.textContent === (type || 'All'));
        }
      }
      kbSearch();
    }

    function kbSelectLayer(layer) {
      _kbSelectedLayer = layer;
      const list = document.getElementById('kb-layer-btns');
      if (list) {
        for (const btn of list.children) {
          const isAll = btn.textContent.includes('All Layers');
          const btnLayer = btn.dataset.action === 'gh14' ? 'vault:' + btn.dataset.arg0 : btn.dataset.arg0;
          btn.classList.toggle('active', layer === 'all' ? isAll : btnLayer === layer);
        }
      }
      kbSearch();
    }

    function kbSearch() {
      return kbLoadFacts(0);
    }

    function kbChangePage(direction) {
      const offset = Math.max(0, _kbPageOffset + direction * _kbPageSize);
      if (_kbServerPaged) return kbLoadFacts(offset);
      _kbPageOffset = offset;
      renderKnowledgeFacts();
    }

    // Auto-memory review (#17058): drafts the CLI-memory drain wrote. The
    // search API is an operator read, so include_states=draft lists them;
    // no agent read ever does until one is approved.
    const KB_AUTO_MEMORY_VIEW = 'auto-memory';
    const KB_AUTO_MEMORY_TAG = 'cli-memory';

    async function kbLoadFacts(offset) {
      const request = ++_kbRequest;
      const query = _kbSearchQuery;
      const layer = _kbSelectedLayer;
      const typeFilter = _kbSelectedType;
      let url;
      if (query) {
        url = '/api/knowledge/search?q=' + encodeURIComponent(query) + '&limit=50';
        if (typeFilter) url += '&type=' + encodeURIComponent(typeFilter);
      } else if (layer === KB_AUTO_MEMORY_VIEW) {
        url = '/api/knowledge/search?tag=' + KB_AUTO_MEMORY_TAG + '&include_states=draft&limit=200';
        if (typeFilter) url += '&type=' + encodeURIComponent(typeFilter);
      } else if (layer && layer !== 'all') {
        // Check if this is a vault, document, or standard layer type
        const standardLayers = ['personal','project','org','community'];
        if (layer.startsWith('doc:')) {
          const docSlug = layer.slice(4);
          url = '/api/knowledge/search?tag=' + encodeURIComponent(docSlug) + '&limit=50';
        } else if (!standardLayers.includes(layer) && !layer.startsWith('git_source:')) {
          url = '/api/knowledge/vaults/' + encodeURIComponent(layer) + '/facts';
        } else {
          url = '/api/knowledge/' + encodeURIComponent(layer);
        }
        if (typeFilter) url += (url.includes('?') ? '&' : '?') + 'type=' + encodeURIComponent(typeFilter);
      } else {
        url = '/api/knowledge';
        const params = ['limit=' + _kbPageSize, 'offset=' + offset];
        if (typeFilter) params.push('type=' + encodeURIComponent(typeFilter));
        if (params.length) url += '?' + params.join('&');
      }
      try {
        const r = await fetch(url);
        if (!r.ok) throw new Error('Knowledge request failed');
        const data = await r.json();
        if (request !== _kbRequest) return;
        _kbServerPaged = !query && (!layer || layer === 'all');
        _kbPageOffset = offset;
        _kbHasMore = !!data.has_more;
        _kbFacts = Array.isArray(data) ? data : (data.results || data.facts || []);
        if (!query && layer === KB_AUTO_MEMORY_VIEW) _kbFacts = _kbFacts.filter(f => f.state === 'draft');
        _kbSelectedFact = null;
        renderKnowledgeFacts();
      } catch (e) {
        if (request !== _kbRequest) return;
        _kbFacts = [];
        _kbHasMore = false;
        _kbPageOffset = 0;
        renderKnowledgeFacts();
      }
    }

    async function kbReadFact(slug, layer) {
      if (!layer) {
        const match = _kbFacts.find(f => f.slug === slug);
        layer = match ? match.layer : 'project';
      }
      try {
        const r = await fetch('/api/knowledge/' + encodeURIComponent(layer) + '/' + encodeURIComponent(slug));
        if (!r.ok) return;
        _kbSelectedFact = await r.json();
        renderKnowledgeDetail();
      } catch (e) { /* ignore */ }
    }

    function kbToggleGraph() {
      _kbGraphView = !_kbGraphView;
      renderKnowledge();
    }

    async function fetchKnowledgeGraph() {
      try {
        const r = await fetch('/api/knowledge/graph');
        if (!r.ok) return;
        _kbGraphData = await r.json();
        renderKnowledgeGraphCanvas();
      } catch (e) { console.error('graph fetch failed', e); }
    }

    let _kbGraphAnimFrame = null;
    let _kbGraphCleanup = null;
    function renderKnowledgeGraphCanvas() {
      if (_kbGraphAnimFrame) {
        cancelAnimationFrame(_kbGraphAnimFrame);
        _kbGraphAnimFrame = null;
      }
      if (_kbGraphCleanup) {
        _kbGraphCleanup();
        _kbGraphCleanup = null;
      }
      const container = document.getElementById('kb-graph-container');
      if (!container || !_kbGraphData) return;
      const nodes = _kbGraphData.nodes || [];
      const edges = _kbGraphData.edges || [];
      if (nodes.length === 0) {
        container.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%;color:var(--muted);font-size:0.85rem">No graph data yet. Facts need <code>related:</code> frontmatter or <code>[[wikilinks]]</code> to form edges.</div>';
        return;
      }

      const canvas = document.createElement('canvas');
      const dpr = window.devicePixelRatio || 1;
      let w = container.clientWidth;
      let h = container.clientHeight;
      canvas.width = w * dpr;
      canvas.height = h * dpr;
      canvas.style.width = w + 'px';
      canvas.style.height = h + 'px';
      canvas.style.position = 'absolute';
      canvas.style.inset = '0';
      canvas.style.zIndex = '1';
      container.innerHTML = '';
      container.appendChild(canvas);

      // Loading overlay while graph simulation runs
      const loadingOverlay = document.createElement('div');
      loadingOverlay.style.cssText = 'position:absolute;inset:0;display:flex;align-items:center;justify-content:center;z-index:15;background:rgba(0,0,0,0.3);transition:opacity 0.3s';
      loadingOverlay.innerHTML = '<div style="display:flex;flex-direction:column;align-items:center;gap:var(--sp-4)"><div style="width:32px;height:32px;border:3px solid rgba(255,255,255,0.2);border-top-color:var(--blue);border-radius:50%;animation:kbGraphSpin 0.8s linear infinite"></div><span style="color:var(--text);font-size:var(--fs-sm);opacity:0.8">Laying out graph…</span></div>';
      container.appendChild(loadingOverlay);
      if (!document.getElementById('kb-graph-spin-style')) {
        const spinStyle = document.createElement('style');
        spinStyle.id = 'kb-graph-spin-style';
        spinStyle.textContent = '@keyframes kbGraphSpin { to { transform: rotate(360deg) } }';
        document.head.appendChild(spinStyle);
      }
      function hideLoadingOverlay() {
        loadingOverlay.style.opacity = '0';
        setTimeout(() => loadingOverlay.remove(), 300);
      }
      const ctx = canvas.getContext('2d');

      const GRAPH_NODE_RADIUS_MIN = 6;
      const GRAPH_NODE_RADIUS_MAX = 18;
      const GRAPH_REPULSION_STRENGTH = 500;
      const GRAPH_ATTRACTION_STRENGTH = 0.05;
      const GRAPH_SPRING_REST_LENGTH = 100;
      const GRAPH_GRAVITY_STRENGTH = 0.02;
      const GRAPH_LABEL_TRUNCATE_LEN = 24;
      const GRAPH_DAMPING = 0.9;
      const GRAPH_MIN_VELOCITY = 0.1;
      const GRAPH_EDGE_HOVER_DIST = 20;

      const layerColors = {
        personal: getComputedStyle(document.documentElement).getPropertyValue('--blue').trim() || '#58a6ff',
        project: getComputedStyle(document.documentElement).getPropertyValue('--green').trim() || '#3fb950',
        org: getComputedStyle(document.documentElement).getPropertyValue('--yellow').trim() || '#d29922',
        community: getComputedStyle(document.documentElement).getPropertyValue('--purple').trim() || '#bc8cff',
      };

      // Zoom/pan state
      let zoomLevel = 1;
      let panX = 0, panY = 0;
      const ZOOM_MIN = 0.2;
      const ZOOM_MAX = 5;
      const ZOOM_STEP = 0.1;

      function screenToWorld(sx, sy) {
        return { x: (sx - panX) / zoomLevel, y: (sy - panY) / zoomLevel };
      }
      function worldToScreen(wx, wy) {
        return { x: wx * zoomLevel + panX, y: wy * zoomLevel + panY };
      }

      const slugIdx = {};
      const maxUsage = Math.max(1, ...nodes.map(x => x.usage_count || 0));
      const simNodes = nodes.map((n, i) => {
        slugIdx[n.slug] = i;
        const ratio = (n.usage_count || 0) / maxUsage;
        return {
          x: w / 2 + (Math.cos(i * 2.399) * w * 0.3),
          y: h / 2 + (Math.sin(i * 2.399) * h * 0.3),
          vx: 0, vy: 0,
          r: GRAPH_NODE_RADIUS_MIN + ratio * (GRAPH_NODE_RADIUS_MAX - GRAPH_NODE_RADIUS_MIN),
          color: layerColors[n.layer] || layerColors.personal,
          label: (kbCleanFactTitle(n.title) || n.slug).length > GRAPH_LABEL_TRUNCATE_LEN
            ? (kbCleanFactTitle(n.title) || n.slug).slice(0, GRAPH_LABEL_TRUNCATE_LEN) + '...'
            : (kbCleanFactTitle(n.title) || n.slug),
          slug: n.slug,
          data: n,
          pinned: false,
        };
      });

      const simEdges = (edges || []).filter(e => slugIdx[e.from] !== undefined && slugIdx[e.to] !== undefined)
        .map(e => ({ from: slugIdx[e.from], to: slugIdx[e.to], pred: e.predicate }));

      let simRunning = true;
      let simSteps = 0;
      const GRAPH_MAX_SIM_STEPS = 300;
      let dragNode = null;
      let hoveredNode = null;
      let isPanning = false;
      let panStartX = 0, panStartY = 0;
      let panStartPanX = 0, panStartPanY = 0;

      function simulationStep() {
        let totalKE = 0;
        for (let i = 0; i < simNodes.length; i++) {
          if (simNodes[i].pinned) continue;
          for (let j = i + 1; j < simNodes.length; j++) {
            let dx = simNodes[j].x - simNodes[i].x;
            let dy = simNodes[j].y - simNodes[i].y;
            let dist = Math.sqrt(dx * dx + dy * dy) || 1;
            let force = GRAPH_REPULSION_STRENGTH / (dist * dist);
            let fx = (dx / dist) * force;
            let fy = (dy / dist) * force;
            if (!simNodes[i].pinned) { simNodes[i].vx -= fx; simNodes[i].vy -= fy; }
            if (!simNodes[j].pinned) { simNodes[j].vx += fx; simNodes[j].vy += fy; }
          }
        }
        for (const e of simEdges) {
          let a = simNodes[e.from], b = simNodes[e.to];
          let dx = b.x - a.x;
          let dy = b.y - a.y;
          let dist = Math.sqrt(dx * dx + dy * dy) || 1;
          let force = (dist - GRAPH_SPRING_REST_LENGTH) * GRAPH_ATTRACTION_STRENGTH;
          let fx = (dx / dist) * force;
          let fy = (dy / dist) * force;
          if (!a.pinned) { a.vx += fx; a.vy += fy; }
          if (!b.pinned) { b.vx -= fx; b.vy -= fy; }
        }
        for (const n of simNodes) {
          if (n.pinned) { n.vx = 0; n.vy = 0; continue; }
          n.vx += (w / 2 - n.x) * GRAPH_GRAVITY_STRENGTH;
          n.vy += (h / 2 - n.y) * GRAPH_GRAVITY_STRENGTH;
          n.vx *= GRAPH_DAMPING; n.vy *= GRAPH_DAMPING;
          n.x += n.vx; n.y += n.vy;
          totalKE += n.vx * n.vx + n.vy * n.vy;
        }
        return totalKE;
      }

      function draw() {
        ctx.save();
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        ctx.clearRect(0, 0, w, h);
        ctx.translate(panX, panY);
        ctx.scale(zoomLevel, zoomLevel);

        // Edges
        for (const e of simEdges) {
          const a = simNodes[e.from], b = simNodes[e.to];
          ctx.strokeStyle = 'rgba(88,166,255,0.2)';
          ctx.lineWidth = 1 / zoomLevel;
          ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke();
          const mx = (a.x + b.x) / 2, my = (a.y + b.y) / 2;
          ctx.fillStyle = 'rgba(139,148,158,0.5)';
          ctx.font = (9 / zoomLevel) + 'px monospace';
          ctx.textAlign = 'center';
          ctx.fillText(e.pred, mx, my - 4 / zoomLevel);
        }

        // Nodes
        for (const n of simNodes) {
          const isHovered = n === hoveredNode;
          const isDragged = n === dragNode;
          ctx.beginPath(); ctx.arc(n.x, n.y, n.r, 0, Math.PI * 2);
          ctx.fillStyle = n.color;
          ctx.globalAlpha = isHovered || isDragged ? 1 : 0.85;
          ctx.fill();
          ctx.globalAlpha = 1;
          ctx.strokeStyle = isHovered || isDragged ? '#ffffff' : 'rgba(255,255,255,0.3)';
          ctx.lineWidth = (isHovered || isDragged ? 2 : 1) / zoomLevel;
          ctx.stroke();
          if (isDragged) {
            ctx.beginPath(); ctx.arc(n.x, n.y, n.r + 4, 0, Math.PI * 2);
            ctx.strokeStyle = 'rgba(255,255,255,0.2)'; ctx.lineWidth = 2 / zoomLevel; ctx.stroke();
          }
          ctx.fillStyle = getComputedStyle(document.documentElement).getPropertyValue('--text').trim() || '#e6edf3';
          ctx.font = (10 / zoomLevel) + 'px monospace';
          ctx.textAlign = 'center';
          ctx.fillText(n.label, n.x, n.y + n.r + 12 / zoomLevel);
        }
        ctx.restore();
      }

      let loadingHidden = false;
      function tick() {
        simSteps++;
        const ke = simulationStep();
        draw();
        if (simRunning && (ke > GRAPH_MIN_VELOCITY || dragNode) && simSteps < GRAPH_MAX_SIM_STEPS) {
          _kbGraphAnimFrame = requestAnimationFrame(tick);
        } else {
          simRunning = false;
          _kbGraphAnimFrame = null;
          draw();
          if (!loadingHidden) { loadingHidden = true; hideLoadingOverlay(); }
        }
      }

      function restartSim() {
        if (!simRunning) {
          simRunning = true;
          simSteps = 0;
          tick();
        }
      }

      // Tooltip
      const tooltip = document.createElement('div');
      tooltip.style.cssText = 'position:absolute;background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:var(--sp-4) var(--sp-5);font-size:0.72rem;color:var(--text);pointer-events:none;display:none;z-index:25;max-width:420px;box-shadow:0 4px 12px rgba(0,0,0,0.3);overflow-y:auto;max-height:350px';
      container.appendChild(tooltip);

      function findNodeAt(sx, sy) {
        const world = screenToWorld(sx, sy);
        for (let i = simNodes.length - 1; i >= 0; i--) {
          const n = simNodes[i];
          const dx = world.x - n.x, dy = world.y - n.y;
          const hitR = n.r + 4;
          if (dx * dx + dy * dy < hitR * hitR) return n;
        }
        return null;
      }

      // Mouse: hover + tooltip
      canvas.addEventListener('mousemove', (evt) => {
        const rect = canvas.getBoundingClientRect();
        const sx = evt.clientX - rect.left;
        const sy = evt.clientY - rect.top;

        if (isPanning) {
          panX = panStartPanX + (evt.clientX - panStartX);
          panY = panStartPanY + (evt.clientY - panStartY);
          draw();
          return;
        }

        if (dragNode) {
          const world = screenToWorld(sx, sy);
          dragNode.x = world.x;
          dragNode.y = world.y;
          dragNode.vx = 0; dragNode.vy = 0;
          return;
        }

        hoveredNode = findNodeAt(sx, sy);
        if (hoveredNode) {
          canvas.style.cursor = 'grab';
          const d = hoveredNode.data;
          const confPct = Math.round((d.confidence || 0) * 100);
          const tags = (d.tags || []).slice(0, 5).map(t => escapeHtml(t)).join(', ');
          const bodyText = d.body ? escapeHtml(d.body).replace(/\n/g, '<br>') : '';
          tooltip.innerHTML = `<strong>${escapeHtml(kbCleanFactTitle(d.title) || d.slug)}</strong><br>` +
            `<span class="text-muted">Type:</span> ${escapeHtml(d.type || 'pattern')} &middot; ` +
            `<span class="text-muted">Layer:</span> ${escapeHtml(d.layer || 'personal')} &middot; ` +
            `<span class="text-muted">Confidence:</span> ${confPct}%` +
            (tags ? `<br><span class="text-muted">Tags:</span> ${tags}` : '') +
            (d.usage_count ? ` &middot; <span class="text-muted">Used:</span> ${d.usage_count}x` : '') +
            (bodyText ? `<div style="margin-top:var(--sp-3);padding-top:var(--sp-3);border-top:1px solid var(--border);line-height:1.4;white-space:pre-wrap;word-break:break-word">${bodyText}</div>` : '');
          tooltip.style.display = 'block';
          tooltip.style.left = (sx + 12) + 'px';
          tooltip.style.top = (sy - 10) + 'px';
        } else {
          canvas.style.cursor = 'default';
          tooltip.style.display = 'none';
        }
        draw();
      });

      // Mouse: drag start
      canvas.addEventListener('mousedown', (evt) => {
        const rect = canvas.getBoundingClientRect();
        const sx = evt.clientX - rect.left;
        const sy = evt.clientY - rect.top;
        const node = findNodeAt(sx, sy);
        if (node) {
          dragNode = node;
          dragNode.pinned = true;
          canvas.style.cursor = 'grabbing';
          tooltip.style.display = 'none';
          restartSim();
        } else {
          isPanning = true;
          panStartX = evt.clientX;
          panStartY = evt.clientY;
          panStartPanX = panX;
          panStartPanY = panY;
          canvas.style.cursor = 'move';
        }
        evt.preventDefault();
      });

      // Mouse: drag end
      const onMouseUp = () => {
        if (dragNode) {
          dragNode.pinned = false;
          dragNode = null;
          canvas.style.cursor = hoveredNode ? 'grab' : 'default';
        }
        if (isPanning) {
          isPanning = false;
          canvas.style.cursor = 'default';
        }
      };
      canvas.addEventListener('mouseup', onMouseUp);
      canvas.addEventListener('mouseleave', () => {
        onMouseUp();
        tooltip.style.display = 'none';
        hoveredNode = null;
        draw();
      });

      // Click: open fact detail
      canvas.addEventListener('click', (evt) => {
        if (isPanning) return;
        const rect = canvas.getBoundingClientRect();
        const node = findNodeAt(evt.clientX - rect.left, evt.clientY - rect.top);
        if (node && !dragNode) {
          _kbGraphView = false;
          _kbSelectedLayer = 'all';
          kbReadFact(node.slug);
          renderKnowledge();
        }
      });

      // Zoom: mouse wheel
      canvas.addEventListener('wheel', (evt) => {
        evt.preventDefault();
        const rect = canvas.getBoundingClientRect();
        const sx = evt.clientX - rect.left;
        const sy = evt.clientY - rect.top;
        const oldZoom = zoomLevel;
        const delta = evt.deltaY > 0 ? -ZOOM_STEP : ZOOM_STEP;
        zoomLevel = Math.max(ZOOM_MIN, Math.min(ZOOM_MAX, zoomLevel + delta * zoomLevel));
        // Zoom toward cursor position
        panX = sx - (sx - panX) * (zoomLevel / oldZoom);
        panY = sy - (sy - panY) * (zoomLevel / oldZoom);
        draw();
      }, { passive: false });

      // Zoom controls overlay
      const controlBtnStyle = 'width:28px;height:28px;border:1px solid var(--border);border-radius:var(--r-sm);background:var(--surface);color:var(--fg);cursor:pointer;display:flex;align-items:center;justify-content:center';
      const zoomControls = document.createElement('div');
      zoomControls.style.cssText = 'position:absolute;top:10px;right:10px;display:flex;flex-direction:column;gap:var(--sp-2);z-index:20';
      zoomControls.innerHTML = `
        <button class="hv-btn btn-primary" data-action="gh6" style="${controlBtnStyle};font-size:var(--fs-lg)" title="Zoom in">+</button>
        <button class="hv-btn btn-danger" data-action="gh7" style="${controlBtnStyle};font-size:10px" title="Reset zoom">&#x27F2;</button>
        <button class="hv-btn btn-secondary" data-action="gh8" style="${controlBtnStyle};font-size:var(--fs-lg)" title="Zoom out">&minus;</button>
        <button class="hv-btn btn-secondary" data-action="gh9" style="${controlBtnStyle};font-size:13px" title="Toggle fullscreen">&#x26F6;</button>
      `;
      zoomControls._zoomIn = () => { zoomLevel = Math.min(ZOOM_MAX, zoomLevel * 1.25); draw(); };
      zoomControls._zoomOut = () => { zoomLevel = Math.max(ZOOM_MIN, zoomLevel / 1.25); draw(); };
      zoomControls._zoomReset = () => { zoomLevel = 1; panX = 0; panY = 0; draw(); };
      zoomControls._toggleFullscreen = () => {
        if (document.fullscreenElement === container) {
          document.exitFullscreen();
        } else {
          container.requestFullscreen().catch(() => {});
        }
      };
      container.appendChild(zoomControls);

      // Resize canvas on fullscreen change
      function resizeCanvas() {
        const newW = container.clientWidth;
        const newH = container.clientHeight;
        if (newW === 0 || newH === 0) return;
        canvas.width = newW * dpr;
        canvas.height = newH * dpr;
        canvas.style.width = newW + 'px';
        canvas.style.height = newH + 'px';
        w = newW; h = newH;
        draw();
      }
      const FULLSCREEN_RESIZE_DELAY_MS = 100;
      const FULLSCREEN_RESIZE_RETRY_MS = 300;
      const onFullscreenChange = () => {
        setTimeout(resizeCanvas, FULLSCREEN_RESIZE_DELAY_MS);
        setTimeout(resizeCanvas, FULLSCREEN_RESIZE_RETRY_MS);
      };
      document.addEventListener('fullscreenchange', onFullscreenChange);
      container._cleanupFullscreen = () => document.removeEventListener('fullscreenchange', onFullscreenChange);

      _kbGraphCleanup = () => {
        if (container._cleanupFullscreen) container._cleanupFullscreen();
      };

      const GRAPH_STATIC_THRESHOLD = 150;
      if (simNodes.length > GRAPH_STATIC_THRESHOLD) {
        const GRAPH_PRECOMPUTE_STEPS = 80;
        for (let s = 0; s < GRAPH_PRECOMPUTE_STEPS; s++) simulationStep();
        simRunning = false;
        draw();
        hideLoadingOverlay();
      } else {
        tick();
      }
    }

    // Setup view state: 'choose' (default), 'git', 'webhook', or 'none' (skip setup, show full KB)
    let _kbSetupView = 'choose';

    function renderKnowledgeSetup(panel, badge) {
      if (badge) {
        badge.textContent = 'setup';
        badge.style.background = 'var(--yellow)';
        badge.style.color = 'white';
      }

      const hiveHost = window.location.host;

      if (_kbSetupView === 'git') {
        panel.innerHTML = renderGitSetupInstructions(hiveHost);
        return;
      }
      if (_kbSetupView === 'webhook') {
        panel.innerHTML = renderWebhookSetupInstructions(hiveHost);
        return;
      }

      // Default: show the two-card chooser
      let html = '<div style="text-align:center;padding:var(--sp-6) var(--sp-0)">';
      html += '<div class="fs-2xl mb-4">📚</div>';
      html += '<div style="font-size:0.88rem;font-weight:600;color:var(--fg);margin-bottom:var(--sp-2)">Connect Your Obsidian Vault</div>';
      html += '<div style="font-size:0.74rem;color:var(--muted);margin-bottom:var(--sp-2);max-width:500px;margin-left:auto;margin-right:auto">Sync your notes as knowledge facts. Choose a connection method below.</div>';
      html += '</div>';

      html += '<div class="kb-setup-grid">';

      // Git Repo card
      html += '<div class="kb-setup-card" data-action="gh10">';
      html += '<div class="kb-setup-card-icon">🔗</div>';
      html += '<div class="kb-setup-card-title">Connect via Git Repo</div>';
      html += '<div class="kb-setup-card-desc">Use the Obsidian Git plugin to push your vault to a repo. Hive clones it and auto-syncs every 60 seconds.</div>';
      html += '</div>';

      // Webhook card
      html += '<div class="kb-setup-card" data-action="gh11">';
      html += '<div class="kb-setup-card-icon">⚡</div>';
      html += '<div class="kb-setup-card-title">Connect via Webhook</div>';
      html += '<div class="kb-setup-card-desc">Use the Obsidian Post Webhook plugin to send individual notes instantly. No git required.</div>';
      html += '</div>';

      html += '</div>';

      // Skip link to go straight to the full KB view
      html += '<div style="text-align:center;margin-top:var(--sp-6)">';
      html += '<button class="hv-btn btn-icon btn-sm" data-action="gh12">Skip setup and use Knowledge Base without Obsidian</button>';
      html += '</div>';

      panel.innerHTML = html;
    }

    function renderGitSetupInstructions(hiveHost, opts) {
      const showBack = !(opts && opts.noBack);
      const showForm = !(opts && opts.noForm);
      let html = '<div class="kb-setup-instructions">';
      if (showBack) html += '<button class="kb-setup-back" data-action="gh13">&larr; Back to connection methods</button>';
      html += '<div style="font-size:var(--fs-md);font-weight:700;color:var(--fg);margin-bottom:var(--sp-5)">🔗 Connect via Git Repo</div>';
      html += '<ol>';
      html += '<li><strong>Install the "Obsidian Git" community plugin</strong><br>';
      html += '<span class="muted">Settings &rarr; Community Plugins &rarr; Browse &rarr; search "Obsidian Git"</span></li>';
      html += '<li><strong>Initialize your vault as a git repo</strong> (or use an existing one)<br>';
      html += '<span class="muted">The plugin can auto-commit and push on a configurable interval.</span></li>';
      html += '<li><strong>Set the git repo URL on your Hive server:</strong><br>';
      html += '<div class="mt-3"><em>Option A:</em> Set the <code>HIVE_WIKI_GIT_URL</code> environment variable before starting Hive</div>';
      html += '<div class="mt-3"><em>Option B:</em> Use the API:</div>';
      html += '<pre>curl -X POST http://' + escapeHtml(hiveHost) + '/api/knowledge/vaults \\\n';
      html += '  -H "Content-Type: application/json" \\\n';
      html += '  -H "Authorization: Bearer &lt;token&gt;" \\\n';
      html += '  -d \'{"path": "/data/vaults/hive-wiki", "name": "hive-wiki"}\'</pre></li>';
      html += '<li><strong>Hive auto-syncs every 60 seconds</strong> via <code>git pull</code><br>';
      html += '<span class="muted">Changes appear in the Knowledge Base within ~30s of pushing.</span></li>';
      html += '</ol>';
      html += '<div class="kb-tip"><strong>Recommended plugin settings:</strong><br>';
      html += '&bull; Auto pull interval: 1 minute<br>';
      html += '&bull; Auto push interval: 1 minute<br>';
      html += '&bull; Pull on startup: enabled</div>';
      html += '</div>';

      // Inline vault connect form
      if (showForm) {
        html += '<div style="border-top:1px solid var(--border);padding-top:14px;margin-top:var(--sp-6)">';
        html += '<div style="font-size:var(--fs-base);font-weight:600;color:var(--fg);margin-bottom:var(--sp-4)">Connect vault now</div>';
        html += '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-4)">Mount the cloned repo into the container (e.g. <code>-v /path/to/vault:/data/vaults/my-vault</code>), then enter the container path below.</div>';
        html += '<div style="display:grid;grid-template-columns:2fr 1fr auto;gap:var(--sp-4);align-items:center">';
        html += '<input id="kb-setup-vault-path" type="text" class="kb-search-box" placeholder="/data/vaults/hive-wiki">';
        html += '<input id="kb-setup-vault-name" type="text" class="kb-search-box" placeholder="Display name">';
        html += '<button class="hv-btn btn-secondary nous-btn btn-primary" data-action="kbSetupConnectVault">Connect</button>';
        html += '</div></div>';
      }

      return html;
    }

    function renderWebhookSetupInstructions(hiveHost, opts) {
      const showBack = !(opts && opts.noBack);
      const showDone = !(opts && opts.noDone);
      let html = '<div class="kb-setup-instructions">';
      if (showBack) html += '<button class="kb-setup-back" data-action="gh13">&larr; Back to connection methods</button>';
      html += '<div style="font-size:var(--fs-md);font-weight:700;color:var(--fg);margin-bottom:var(--sp-5)">⚡ Connect via Webhook</div>';
      html += '<ol>';
      html += '<li><strong>Install the "Post Webhook" community plugin</strong><br>';
      html += '<span class="muted">Settings &rarr; Community Plugins &rarr; Browse &rarr; search "Post Webhook"</span></li>';
      html += '<li><strong>Add a webhook in the plugin settings:</strong>';
      html += '<div style="margin-top:var(--sp-3);padding:10px 14px;background:var(--bg);border:1px solid var(--border);border-radius:var(--r);font-size:0.74rem">';
      html += '<div><strong>Name:</strong> Hive Knowledge Sync</div>';
      html += '<div><strong>URL:</strong> <code>http://' + escapeHtml(hiveHost) + '/api/knowledge/obsidian/sync</code></div>';
      html += '<div><strong>Method:</strong> POST</div>';
      html += '<div><strong>Custom Headers</strong> (paste into the plugin\'s Custom Headers field):</div>';
      html += '<div style="position:relative;margin-top:var(--sp-2)">';
      html += '<pre id="kb-headers-block" style="margin:var(--sp-0);font-size:0.72rem;padding:8px 10px;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);white-space:pre;overflow-x:auto">{\n  "Authorization": "Bearer <span id="kb-token-display">&bull;&bull;&bull;&bull;&bull;&bull;&bull;&bull;</span>",\n  "Content-Type": "application/json"\n}</pre>';
      html += '<div style="position:absolute;top:4px;right:6px;display:flex;gap:3px">';
      html += '<button class="hv-btn btn-primary btn-sm" data-action="kbToggleToken" id="kb-token-toggle">show</button>';
      html += '<button class="hv-btn btn-primary btn-sm" data-action="kbCopyHeaders" id="kb-headers-copy">copy</button>';
      html += '</div></div>';
      html += '</div></li>';
      html += '<li><strong>Use it:</strong><br>';
      html += '<span class="muted">Command palette &rarr; "Post Webhook: Send current note"<br>';
      html += 'Or bind to a hotkey (e.g., Ctrl+Shift+S)</span></li>';
      html += '<li><strong>Your note\'s frontmatter controls how it\'s stored:</strong>';
      html += '<pre>---\n';
      html += 'title: My Pattern\n';
      html += 'tags: [scanner, pattern]\n';
      html += 'type: pattern       # pattern, gotcha, decision, etc.\n';
      html += 'layer: project      # personal, project, org, community\n';
      html += 'confidence: 0.85\n';
      html += '---</pre>';
      html += '<span class="muted">Notes are upserted by filename -- saving the same note updates it.</span></li>';
      html += '</ol>';
      html += '<div class="kb-tip"><strong>Tip:</strong> The webhook endpoint is always available at <code>http://' + escapeHtml(hiveHost) + '/api/knowledge/obsidian/sync</code>. You can start sending notes immediately -- no vault connection required. Facts will appear in the Knowledge Base as soon as they arrive.</div>';
      html += '</div>';

      // Enable knowledge button for webhook (webhook works without vault connection)
      if (showDone) {
        html += '<div style="border-top:1px solid var(--border);padding-top:14px;margin-top:var(--sp-6);text-align:center">';
        html += '<button data-action="gh12" class="hv-btn btn-secondary nous-btn btn-primary">Done -- go to Knowledge Base</button>';
        html += '</div>';
      }

      return html;
    }

    let _kbTokenValue = null;
    let _kbTokenVisible = false;
    async function kbFetchToken() {
      if (_kbTokenValue) return _kbTokenValue;
      try {
        const r = await fetch('/api/auth/token');
        const d = await r.json();
        _kbTokenValue = d.token || '(not set)';
      } catch { _kbTokenValue = '(error)'; }
      return _kbTokenValue;
    }
    async function kbToggleToken() {
      const el = document.getElementById('kb-token-display');
      const btn = document.getElementById('kb-token-toggle');
      if (!el || !btn) return;
      if (_kbTokenVisible) {
        el.textContent = '•'.repeat(8);
        btn.textContent = 'show';
        _kbTokenVisible = false;
      } else {
        const t = await kbFetchToken();
        el.textContent = t;
        btn.textContent = 'hide';
        _kbTokenVisible = true;
      }
    }
    async function kbCopyToken() {
      const t = await kbFetchToken();
      if (t && t !== '(not set)' && t !== '(error)') {
        navigator.clipboard.writeText(t);
        const btn = event.target;
        btn.textContent = 'copied!';
        setTimeout(() => { btn.textContent = 'copy'; }, 1500);
      }
    }
    async function kbCopyHeaders() {
      const t = await kbFetchToken();
      const block = JSON.stringify({"Authorization": "Bearer " + t, "Content-Type": "application/json"}, null, 2);
      navigator.clipboard.writeText(block);
      const btn = document.getElementById('kb-headers-copy');
      if (btn) { btn.textContent = 'copied!'; setTimeout(() => { btn.textContent = 'copy'; }, 1500); }
    }

    async function kbSetupConnectVault() {
      const path = document.getElementById('kb-setup-vault-path')?.value?.trim();
      const name = document.getElementById('kb-setup-vault-name')?.value?.trim();
      if (!path) { await hiveAlert('Path is required'); return; }

      try {
        const r = await fetch('/api/knowledge/vaults', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ path, name: name || '' }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to connect vault'); return; }
        _kbSetupView = 'none';
        await fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    function renderKnowledge() {
      refreshSectionCardShell('knowledge-section');
      renderPublicKnowledgeControls();
      const panel = document.getElementById('knowledge-panel');
      const badge = document.getElementById('kb-status-badge');
      if (!panel) return;

      // `enabled` is primer registration — whether kicks receive knowledge —
      // not whether the knowledge API exists (#9231). The API is always up
      // for the bead synthesizer and vault browsing, so stored facts stay
      // browsable below while the panel says they are not being primed.
      const kbPrimer = (_kbCache && _kbCache.primer) || {};
      const kbPrimed = !!(_kbCache && _kbCache.enabled);
      const kbHasFacts = !!_kbCache && ((_kbCache.layers || []).length > 0 || (_kbCache.vaults || []).length > 0);
      if (!_kbCache || (!kbPrimed && !kbHasFacts)) {
        if (badge) { badge.textContent = 'disabled'; badge.style.background = 'var(--muted)'; badge.style.color = 'white'; }
        panel.innerHTML = '<div class="kb-disabled"><div class="fs-3xl mb-4">📚</div><div>Knowledge system not enabled</div><div style="font-size:0.72rem;margin-top:var(--sp-2);color:var(--muted);margin-bottom:var(--sp-5)">Enable the knowledge base to store and retrieve facts across agents.</div><button class="hv-btn btn-primary" data-action="toggleKnowledge" data-arg0="true" data-arg-types="j" title="Enable the knowledge base system. Agents will be able to store and retrieve facts (patterns, gotchas, regressions, decisions) across sessions and layers (personal, project, org, community).">Enable Knowledge</button></div>';
        return;
      }

      // Check if any vaults are connected — if not, show the Obsidian setup panel
      const connectedVaults = _kbCache.vaults || [];
      if (connectedVaults.length === 0 && _kbSetupView !== 'none') {
        renderKnowledgeSetup(panel, badge);
        return;
      }

      const layers = _kbCache.layers || [];
      const totalPages = kbTotalFactsFromStats(_kbCache);
      const healthyCount = layers.filter(l => l.healthy).length;
      setSectionSummary('knowledge-section', kbFactsLabel(totalPages));
      if (badge) {
        if (!kbPrimed) {
          badge.textContent = 'not primed';
          badge.style.background = 'var(--muted)';
        } else if (kbPrimer.restart_required) {
          badge.textContent = 'restart required';
          badge.style.background = 'var(--yellow)';
        } else {
          badge.textContent = kbFactsLabel(totalPages);
          badge.style.background = healthyCount === layers.length ? 'var(--green)' : (healthyCount > 0 ? 'var(--yellow)' : 'var(--red)');
        }
        badge.style.color = 'white';
      }

      const synthEnabled = _beadSynthStatus && _beadSynthStatus.enabled;
      const synthRunning = _beadSynthStatus && _beadSynthStatus.running;
      let html = '<div style="display:flex;justify-content:flex-end;gap:var(--sp-4);margin-bottom:var(--sp-4)">';
      if (synthEnabled) {
        const synthStatus = statusMeta(synthRunning ? 'active' : 'idle');
        html += `<span class="badge-status" data-status="${synthStatus.status}" title="Bead synthesizer is ${synthRunning ? 'running' : 'enabled but not running'}. Schedule: ${escapeHtml((_beadSynthStatus||{}).schedule||'hourly')}"><span class="status-dot" data-status="${synthStatus.status}" style="width:6px;height:6px;min-width:6px"></span> Synth ${synthStatus.label}</span>`;
        html += '<button class="hv-btn btn-danger btn-sm" data-action="toggleBeadSynth" data-arg0="false" data-arg-types="j" title="Disable the bead synthesizer. Completed beads will no longer be automatically converted to wiki facts.">Pause Synth</button>';
      } else {
        html += '<button class="hv-btn btn-primary btn-sm" data-action="toggleBeadSynth" data-arg0="true" data-arg-types="j" title="Enable the bead synthesizer. Completed beads will be automatically scanned and converted to wiki facts on an hourly schedule.">Enable Synth</button>';
      }
      if (kbPrimed) {
        html += '<button class="hv-btn btn-danger btn-sm" data-action="toggleKnowledge" data-arg0="false" data-arg-types="j" title="Disable the knowledge base. Agents will no longer inject facts into prompts. Stored facts are preserved for when re-enabled.">Disable</button>';
      } else {
        html += '<button class="hv-btn btn-primary btn-sm" data-action="toggleKnowledge" data-arg0="true" data-arg-types="j" title="Enable the knowledge base. Agent kicks will be primed with relevant facts from the configured layers, vaults, git sources and the bead synthesizer.">Enable Knowledge</button>';
      }
      html += '</div>';
      if (!kbPrimed) {
        html += '<div class="kb-notice">Knowledge is disabled: no agent kick receives these facts. They are stored and browsable (the bead synthesizer keeps writing here), but they are only injected into kicks once knowledge is enabled.</div>';
      } else if (kbPrimer.restart_required) {
        html += '<div class="kb-notice">' + escapeHtml(kbPrimer.restart_reason || 'Restart the hive to finish enabling knowledge.') + '</div>';
      }

      // Stats row
      const staleTotal = layers.reduce((sum, l) => sum + (l.stale || 0), 0);
      const orphanedTotal = layers.reduce((sum, l) => sum + (l.orphaned || 0), 0);
      html += '<div class="kb-stat-grid kb-summary-stats">';
      html += `<div class="kb-stat"><div class="val">${totalPages}</div><div class="lbl">Total Facts</div>${factSparkSvg()}</div>`;
      html += `<div class="kb-stat" title="Layers are knowledge sources (personal, project, org, community, vaults, or imported docs) that scope where facts come from and where new facts are written."><div class="val">${healthyCount}/${layers.length}</div><div class="lbl">Layers Online</div></div>`;
      html += `<div class="kb-stat"><div class="val">${escapeHtml(_kbCache.engine || 'n/a')}</div><div class="lbl">Engine</div></div>`;
      if (staleTotal > 0) html += `<div class="kb-stat"><div class="val text-warn">${staleTotal}</div><div class="lbl">Stale</div></div>`;
      if (orphanedTotal > 0) html += `<div class="kb-stat"><div class="val text-danger">${orphanedTotal}</div><div class="lbl">Orphaned</div></div>`;
      html += '</div>';
      html += '<div class="dash-card-subtitle kb-stats-help">Layers are knowledge-source scopes; facts are the reusable items agents search and inject into prompts.</div>';

      // Action buttons
      html += '<div class="kb-actions" style="display:flex;flex-wrap:wrap;gap:var(--sp-4);margin-bottom:14px">';
      html += '<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbOpenCreate" title="Create a new knowledge fact manually. Choose a layer (personal, project, org, community), type, and enter the fact content.">+ New Fact</button>';
      html += '<button class="hv-btn btn-secondary nous-btn btn-surface" data-action="kbOpenSubscriptions" title="Manage knowledge sources: git repos, wiki subscriptions, document imports, and fact imports.">🔗 Knowledge Sources</button>';
      html += '<button class="hv-btn btn-secondary nous-btn btn-surface" data-action="kbOpenVaults" title="Manage knowledge vaults — collections of facts organized by topic or project. Vaults can be shared across hive instances.">📂 Vaults</button>';
      html += '<button class="hv-btn btn-secondary nous-btn btn-surface" data-action="kbOpenHowItWorks">📖 How it works</button>';
      html += '<button class="hv-btn btn-secondary nous-btn btn-surface" data-action="kbOpenIntegrations" title="Configure knowledge integrations and connectors: Obsidian, SharePoint, Confluence, Notion, Google Drive, GitHub Wiki, git, repo wiki, and documents.">🔌 Integrations</button>';
      html += `<button class="hv-btn btn-secondary nous-btn" style="background:${_kbGraphView ? 'var(--cyan)' : 'var(--surface)'};color:${_kbGraphView ? 'var(--bg)' : 'var(--fg)'};border:1px solid ${_kbGraphView ? 'var(--cyan)' : 'var(--border)'}" data-action="kbToggleGraph" title="Toggle knowledge graph visualization showing relationships between facts.">🕸️ Graph</button>`;
      html += '</div>';

      // Main layout: sidebar + content
      html += '<div class="kb-layout">';

      // Left sidebar: one consolidated Knowledge Sources list + search + filters.
      // Every source is ONE row here: health dot (tooltip = health detail) +
      // name + type pill (vault / doc) + count pill. The backend already
      // reports each connected vault as a layer (knowledge.Stats appends
      // vaults to the layers list), so the separate vaults array is only
      // rendered for entries NOT already present as a layer.
      html += '<div class="kb-sidebar">';
      html += '<div class="section-label m-0">Knowledge Sources</div>';
      html += '<div class="kb-layer-list" id="kb-layer-btns">';
      // Aggregate row stays on top as the filter reset (kbSelectLayer keys on the 'All Layers' text)
      html += `<button class="hv-btn btn-secondary btn-sm kb-layer-btn${_kbSelectedLayer === 'all' ? ' active' : ''}" data-action="kbSelectLayer" data-arg0="all"><span class="kb-layer-label">📋 All Layers</span><span class="kb-layer-count">${totalPages}</span></button>`;
      html += `<button class="hv-btn btn-secondary btn-sm kb-layer-btn${_kbSelectedLayer === KB_AUTO_MEMORY_VIEW ? ' active' : ''}" data-action="kbSelectLayer" data-arg0="${KB_AUTO_MEMORY_VIEW}" title="Drafts drained from the coding CLIs' own memory stores. No agent sees one until it is approved."><span class="kb-layer-label">🧠 Auto-memory drafts</span></button>`;
      for (const l of layers) {
        const isGitSource = l.type && l.type.startsWith('git_source:');
        const icon = KB_LAYER_ICONS[l.type] || (isGitSource ? '📄' : '📂');
        const label = KB_LAYER_LABELS[l.type] || l.type;
        const active = _kbSelectedLayer === l.type ? ' active' : '';
        const count = l.total_pages || 0;
        const layerStatus = statusMeta(l.healthy ? 'ready' : 'error');
        const healthTip = `${escapeHtml(label)}: ${l.healthy ? 'healthy' : 'unreachable'}`;
        const repoUrl = isGitSource && l.url ? (l.subpath ? l.url + '/tree/' + 'main/' + l.subpath : l.url) : '';
        html += `<button class="hv-btn btn-secondary btn-sm kb-layer-btn${active}" data-action="kbSelectLayer" data-arg0="${escapeHtml(l.type)}" title="${healthTip}">`;
        html += `<span class="kb-layer-label"><span class="kb-health-dot status-dot" data-status="${layerStatus.status}" title="${healthTip}"></span>${icon} ${escapeHtml(label)}</span>`;
        if (isGitSource && repoUrl) {
          html += `<a href="${escapeHtml(repoUrl)}" target="_blank" rel="noopener" data-action="gh3" style="color:var(--accent);text-decoration:none;font-size:var(--fs-base);flex-shrink:0" title="Open source repo in new tab">↗</a>`;
        }
        html += `<span class="kb-src-type vault">vault</span>`;
        html += `<span class="kb-layer-count">${count}</span>`;
        html += `</button>`;
      }
      // Vaults not already mirrored into the layers list (dedupe by name);
      // same row shape, click behavior unchanged
      const layerTypes = new Set(layers.map(l => l.type));
      for (const v of connectedVaults.filter(v => !layerTypes.has(v.name))) {
        const vTip = `${escapeHtml(v.name)}: connected — ${escapeHtml(v.root_dir)}`;
        html += `<button class="hv-btn btn-secondary btn-sm kb-layer-btn${_kbSelectedLayer === 'vault:'+v.name ? ' active' : ''}" data-action="gh14" data-arg0="${escapeHtml(v.name)}" title="${vTip}">`;
        html += `<span class="kb-layer-label"><span class="kb-health-dot status-dot" data-status="ok" title="${vTip}"></span>📂 ${escapeHtml(v.name)}</span>`;
        html += `<span class="kb-src-type vault">vault</span>`;
        html += `<span class="kb-layer-count">${v.pages}</span>`;
        html += `</button>`;
      }
      // Imported documents — rows of type 'doc' in the same list; click behavior unchanged
      for (const d of (_kbCache._documents || [])) {
        const dIcon = DOC_CONTENT_ICONS[d.content_type] || '📄';
        const dFacts = d.fact_count || 0;
        const dTip = `${escapeHtml(d.title || d.slug)}: imported — ${escapeHtml(d.source_url || d.source_file || d.slug)}`;
        html += `<button class="hv-btn btn-secondary btn-sm kb-layer-btn${_kbSelectedLayer === 'doc:'+d.slug ? ' active' : ''}" data-action="kbSelectLayer" data-arg0="doc:${escapeHtml(d.slug)}" title="${dTip}">`;
        html += `<span class="kb-layer-label"><span class="kb-health-dot status-dot" data-status="ok" title="${dTip}"></span>${dIcon} ${escapeHtml(d.title || d.slug)}</span>`;
        html += `<span class="kb-src-type doc">doc</span>`;
        html += `<span class="kb-layer-count">${dFacts}</span>`;
        html += `</button>`;
      }
      html += '</div>';

      // Search
      html += `<input type="text" class="kb-search-box" placeholder="Search facts..." value="${escapeHtml(_kbSearchQuery)}" data-keyup-action="gh15" data-keys="Enter" data-change-action="gh16" title="Search across all knowledge layers by keyword. Press Enter to search. Results are filtered by the selected layer and type.">`;

      // Type filter pills
      html += '<div class="kb-filter-row" id="kb-type-pills">';
      html += `<span class="kb-filter-pill${_kbSelectedType === '' ? ' active' : ''}" data-action="kbSelectType" data-arg0="">All</span>`;
      for (const t of KB_FACT_TYPES) {
        const active = _kbSelectedType === t ? ' active' : '';
        html += `<span class="kb-filter-pill${active}" data-action="kbSelectType" data-arg0="${escapeHtml(t)}">${escapeHtml(t)}</span>`;
      }
      html += '</div>';

      html += '</div>'; // kb-sidebar

      // Right: fact list + detail OR graph view
      html += '<div class="kb-main">';
      if (_kbGraphView) {
        html += '<div id="kb-graph-container" style="width:100%;height:500px;position:relative;border:1px solid var(--border);border-radius:10px;overflow:hidden;background:var(--surface)"></div>';
      } else {
        html += '<div id="kb-fact-list" class="kb-fact-list"></div>';
        html += '<div id="kb-fact-detail"></div>';
      }
      html += '</div>';

      html += '</div>'; // kb-layout
      html += '<div id="kb-effective-panel" class="kb-effective"></div>';
      html += '<div id="kb-advisor-panel" class="kb-effective"></div>';
      panel.innerHTML = html;
      renderKbEffective();
      renderKbAdvisor();
      if (!_kbAdvisorData && !_kbAdvisorError && !_kbAdvisorLoading) kbAdvisorRefresh();
      if (_kbGraphView) {
        fetchKnowledgeGraph();
      } else {
        kbSearch();
      }
    }

    // Effective knowledge (#11202): what one agent would receive under its
    // knowledge.agent_scopes entry, and why the rest is filtered out.
    let _kbEffectiveAgent = '';
    let _kbEffectiveData = null;
    let _kbEffectiveError = '';
    const KB_EFFECTIVE_REASONS = {
      lifecycle: 'lifecycle state not admitted',
      layer: 'layer outside scope',
      repo: 'repo outside scope',
      type: 'type outside scope',
      tag: 'no matching tag',
    };

    function kbEffectiveAgentNames() {
      const names = new Set(((window._lastStatus || {}).agents || []).map(a => a && a.name).filter(Boolean));
      if (_kbEffectiveAgent) names.add(_kbEffectiveAgent);
      return Array.from(names).sort();
    }

    function kbEffectiveEntryHTML(e, reason) {
      let meta = [e.layer, e.type, e.status].filter(Boolean).map(escapeHtml).join(' · ');
      if (e.repo) meta += ' · repo ' + escapeHtml(e.repo);
      let html = '<li class="kb-effective-item"><code>' + escapeHtml(e.id) + '</code><span>' + escapeHtml(e.title || '') + '</span><span class="kb-effective-meta">' + meta + '</span>';
      if (reason) html += '<span class="kb-effective-reason" title="' + escapeHtml(KB_EFFECTIVE_REASONS[reason] || reason) + '">' + escapeHtml(reason) + '</span>';
      return html + '</li>';
    }

    function renderKbEffective() {
      const el = document.getElementById('kb-effective-panel');
      if (!el) return;
      let html = '<div class="kb-effective-head"><div class="section-label m-0">Effective knowledge</div>';
      html += '<select class="hv-btn btn-secondary btn" data-change-action="kbEffectiveSelectAgent" data-arg-types="v" title="Show the knowledge this agent receives under its knowledge.agent_scopes entry">';
      html += '<option value="">Choose an agent…</option>';
      for (const name of kbEffectiveAgentNames()) {
        html += '<option value="' + escapeHtml(name) + '"' + (name === _kbEffectiveAgent ? ' selected' : '') + '>' + escapeHtml(name) + '</option>';
      }
      html += '</select>';
      if (_kbEffectiveAgent) html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbEffectiveRefresh" title="Reload the effective knowledge for this agent">Refresh</button>';
      html += '</div>';
      const d = _kbEffectiveData;
      if (_kbEffectiveError) {
        html += '<div class="kb-notice">' + escapeHtml(_kbEffectiveError) + '</div>';
      } else if (!_kbEffectiveAgent) {
        html += '<div class="dash-card-subtitle">Pick an agent to see which knowledge entries it receives and why the others are filtered out.</div>';
      } else if (d) {
        const sc = d.scope || {};
        const dims = [['layers', sc.layers], ['repos', sc.repos], ['types', sc.types], ['tags', sc.tags]]
          .map(([k, v]) => k + ': ' + ((v && v.length) ? v.join(', ') : 'any'));
        dims.push('states: ' + (sc.include_states || []).join(', '));
        html += '<div class="kb-effective-scope">' + (sc.configured ? 'Scope from knowledge.agent_scopes' : 'No knowledge.agent_scopes entry — unrestricted') + ' · ' + escapeHtml(dims.join(' · ')) + '</div>';
        if (d.truncated) html += '<div class="kb-notice">Lists are capped; showing ' + (d.included_returned || 0) + ' of ' + (d.included_total || 0) + ' included and ' + (d.excluded_returned || 0) + ' of ' + (d.excluded_total || 0) + ' excluded entries.</div>';
        html += '<div class="kb-effective-cols">';
        html += '<div><div class="section-label m-0">Included (' + (d.included_total || 0) + ')</div><ul class="kb-effective-list">';
        html += (d.included || []).map(e => kbEffectiveEntryHTML(e, '')).join('') || '<li class="kb-effective-meta">Nothing in scope.</li>';
        html += '</ul></div>';
        html += '<div><div class="section-label m-0">Excluded (' + (d.excluded_total || 0) + ')</div><ul class="kb-effective-list">';
        html += (d.excluded || []).map(e => kbEffectiveEntryHTML(e, e.reason)).join('') || '<li class="kb-effective-meta">Nothing filtered out.</li>';
        html += '</ul></div></div>';
      } else {
        html += '<div class="dash-card-subtitle">Loading…</div>';
      }
      el.innerHTML = html;
    }

    async function kbEffectiveSelectAgent(name) {
      _kbEffectiveAgent = (name || '').trim();
      _kbEffectiveData = null;
      _kbEffectiveError = '';
      renderKbEffective();
      if (!_kbEffectiveAgent) return;
      const agent = _kbEffectiveAgent;
      try {
        const r = await fetch('/api/knowledge/effective?agent=' + encodeURIComponent(agent));
        const data = await r.json().catch(() => ({}));
        if (agent !== _kbEffectiveAgent) return;
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbEffectiveData = data;
      } catch (e) {
        if (agent !== _kbEffectiveAgent) return;
        _kbEffectiveError = 'Could not load effective knowledge: ' + e.message;
      }
      renderKbEffective();
    }

    function kbEffectiveRefresh() {
      return kbEffectiveSelectAgent(_kbEffectiveAgent);
    }

    // ── Advisor card (#18897) ──
    // Read-only rank-versus-holdout comparison from GET /api/knowledge/advisor/summary:
    // counts and means only, plus the advisor settings that produced them.
    let _kbAdvisorData = null;
    let _kbAdvisorError = '';
    let _kbAdvisorLoading = false;

    function kbAdvisorNum(v, digits) {
      return (typeof v === 'number' && isFinite(v)) ? v.toFixed(digits) : '–';
    }

    function kbAdvisorCountsHTML(label, counts) {
      const keys = Object.keys(counts || {}).sort();
      if (!keys.length) return '<div><div class="section-label m-0">' + escapeHtml(label) + '</div><div class="kb-effective-meta">none recorded</div></div>';
      return '<div><div class="section-label m-0">' + escapeHtml(label) + '</div><ul class="kb-effective-list">'
        + keys.map(k => '<li class="kb-effective-item"><span>' + escapeHtml(k || 'unknown') + '</span><span class="kb-effective-meta">' + escapeHtml(String(counts[k])) + '</span></li>').join('')
        + '</ul></div>';
    }

    // Why model calls got no answer (#17054): error classes and the most
    // frequent redacted reasons (HTTP status, provider code, message).
    function kbAdvisorErrorsHTML(errors) {
      const e = errors || {};
      const top = Array.isArray(e.top_reasons) ? e.top_reasons : [];
      if (!top.length) return '';
      return '<div class="section-label m-0" style="margin-top:var(--sp-4)">Top error reasons</div><ul class="kb-effective-list">'
        + top.map(r => '<li class="kb-effective-item"><span>' + escapeHtml(String(r.class || 'unknown')) + ': ' + escapeHtml(String(r.reason || '')) + '</span><span class="kb-effective-meta">' + escapeHtml(String(r.count)) + '</span></li>').join('')
        + '</ul>';
    }

    function kbAdvisorArmRowHTML(a, note) {
      return '<tr><td>' + escapeHtml(a.arm) + (note ? ' <span class="kb-effective-meta">' + escapeHtml(note) + '</span>' : '') + '</td><td>' + escapeHtml(String(a.beads)) + '</td><td>'
        + kbAdvisorNum(a.mean_fix_attempts, 2) + '</td><td>' + kbAdvisorNum(a.mean_ci_failure_count, 2) + '</td><td>' + kbAdvisorNum(a.mean_kicks_received, 2) + '</td></tr>';
    }

    function kbAdvisorCardHTML(d, error, loading) {
      let html = '<div class="kb-effective-head"><div class="section-label m-0">Advisor</div>'
        + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbAdvisorRefresh" title="Reload the advisor rank-versus-holdout summary">Refresh</button></div>';
      if (error) return html + '<div class="kb-notice">' + escapeHtml(error) + '</div>';
      if (!d) return html + '<div class="dash-card-subtitle">' + (loading ? 'Loading…' : 'No advisor summary yet.') + '</div>';
      const st = d.settings || {};
      const w = d.window || {};
      const cost = d.cost || {};
      const counts = d.counts || {};
      html += '<div class="kb-effective-scope">Settings (read-only): mode ' + escapeHtml(String(st.mode || 'off'))
        + ' · holdout ' + escapeHtml(String(st.holdout_percent || 0)) + '%'
        + ' · model ' + escapeHtml(String(st.model || 'not set'))
        + ' · budget ' + escapeHtml(String(st.max_calls_per_day == null ? '–' : st.max_calls_per_day)) + '/day, '
        + escapeHtml(String(st.max_calls_per_agent_hour == null ? '–' : st.max_calls_per_agent_hour)) + '/agent/hour'
        + (st.enabled ? '' : ' · advisor is turned off') + '</div>';
      if (st.problem) html += '<div class="kb-notice">Advisor misconfigured: ' + escapeHtml(String(st.problem)) + '</div>';
      html += '<div class="kb-effective-scope">' + escapeHtml(String(w.decisions || 0)) + ' decisions'
        + (w.first_decision ? ' from ' + escapeHtml(w.first_decision) + ' to ' + escapeHtml(w.last_decision || '') : '')
        + (w.rank_mode_start ? ' · rank mode since ' + escapeHtml(w.rank_mode_start) : '') + '</div>';
      if (d.caveat) html += '<div class="kb-notice">' + escapeHtml(d.caveat) + '</div>';
      html += '<div class="kb-effective-cols">'
        + kbAdvisorCountsHTML('Decisions by mode', counts.by_mode)
        + kbAdvisorCountsHTML('Decisions by arm', counts.by_arm)
        + kbAdvisorCountsHTML('Decisions by outcome', counts.by_outcome) + '</div>';
      html += kbAdvisorErrorsHTML(d.errors);
      const arms = d.arms || [];
      html += '<div class="section-label m-0" style="margin-top:var(--sp-4)">Closed beads by arm</div>';
      if (!arms.length && !d.mixed && !d.none) {
        html += '<div class="kb-effective-meta">No closed beads with a closed PR yet.</div>';
      } else {
        html += '<table class="kb-advisor-table"><thead><tr><th>Arm</th><th>Beads</th><th>Mean fix attempts</th><th>Mean CI failures</th><th>Mean kicks</th></tr></thead><tbody>';
        html += arms.map(a => kbAdvisorArmRowHTML(a, '')).join('');
        if (d.mixed) html += kbAdvisorArmRowHTML(d.mixed, 'in more than one arm; not comparable');
        if (d.none) html += kbAdvisorArmRowHTML(d.none, 'no advisor decision');
        html += '</tbody></table>';
        html += '<div class="kb-effective-meta">Bead means cover every closed bead with a closed PR; the since window does not narrow them.</div>';
      }
      html += '<div class="kb-effective-meta" style="margin-top:var(--sp-3)">Mean model latency ' + kbAdvisorNum(cost.mean_latency_ms, 0) + ' ms (' + escapeHtml(String(cost.latency_samples || 0)) + ' calls) · mean tokens '
        + kbAdvisorNum(cost.mean_prompt_tokens, 0) + ' prompt / ' + kbAdvisorNum(cost.mean_completion_tokens, 0) + ' completion (' + escapeHtml(String(cost.token_samples || 0)) + ' calls)</div>';
      return html;
    }

    function renderKbAdvisor() {
      const el = document.getElementById('kb-advisor-panel');
      if (!el) return;
      el.innerHTML = kbAdvisorCardHTML(_kbAdvisorData, _kbAdvisorError, _kbAdvisorLoading);
    }

    async function kbAdvisorRefresh() {
      _kbAdvisorLoading = true;
      _kbAdvisorError = '';
      renderKbAdvisor();
      try {
        const r = await fetch('/api/knowledge/advisor/summary');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbAdvisorData = data;
      } catch (e) {
        _kbAdvisorError = 'Could not load the advisor summary: ' + e.message;
      }
      _kbAdvisorLoading = false;
      renderKbAdvisor();
    }
    // ── Advisor card end ──

    // Fact hygiene: synthesized/imported facts can carry raw markdown heading
    // markers in titles ("### Configure build...") or be nothing but a
    // horizontal-rule line ("--------"). Clean both at render time.
    const KB_HEADING_MARKER_RE = /^\s*#{1,6}\s+/;   // leading markdown heading syntax
    const KB_DIVIDER_ONLY_RE = /^[\s\-=_*]+$/;      // content made only of divider chars

    function kbCleanFactTitle(t) {
      return String(t || '').replace(KB_HEADING_MARKER_RE, '').trim();
    }

    function kbCleanFactPreview(b) {
      return String(b || '').replace(KB_HEADING_MARKER_RE, '').trim();
    }

    function kbIsDividerOnlyFact(f) {
      const title = String(f.title || '').trim();
      const body = String(f.body || '').trim();
      const titleJunk = title === '' || KB_DIVIDER_ONLY_RE.test(title);
      const bodyJunk = body === '' || KB_DIVIDER_ONLY_RE.test(body);
      return titleJunk && bodyJunk;
    }

    function kbConfidenceBadge(f) {
      if (!f || f.confidence_scored === false) {
        const reason = (f && f.confidence_reason) || 'No confidence, validation, provenance, usage, or corroboration signal is available yet.';
        return `<span class="kb-fact-conf" title="${escapeHtml(reason)}">unscored</span>`;
      }
      const conf = typeof f.confidence === 'number' ? f.confidence : 0;
      const confPct = Math.round(conf * 100);
      const reason = f.confidence_reason || ('Confidence score: ' + confPct + '%');
      return `<span class="kb-fact-conf" style="color:${confColor(conf)}" title="${escapeHtml(reason)}">${confPct}%</span>`;
    }

    function renderKnowledgeFacts() {
      const el = document.getElementById('kb-fact-list');
      if (!el) return;

      const visibleFacts = _kbFacts.filter(f => !kbIsDividerOnlyFact(f));
      const start = _kbServerPaged ? 0 : _kbPageOffset;
      const page = visibleFacts.slice(start, start + _kbPageSize);
      const hasNext = _kbServerPaged ? _kbHasMore : start + _kbPageSize < visibleFacts.length;
      let html = page.length ? '' : '<div class="kb-empty"><div class="kb-empty-icon">🔍</div>No facts found. Try a different search or layer.</div>';
      for (const f of page) {
        const conf = typeof f.confidence === 'number' ? f.confidence : 0;
        const confPct = Math.round(conf * 100);
        const typeClass = 'kb-type-' + (f.type || 'pattern');
        const layerClass = 'kb-layer-' + (f.layer || 'project');
        const selected = _kbSelectedFact && _kbSelectedFact.slug === f.slug ? ' selected' : '';
        html += `<div class="kb-fact-item${selected}" data-action="gh17" data-arg0="${escapeHtml(f.slug || '')}">`;
        html += '<div class="kb-fact-meta">';
        html += kbConfidenceBadge(f);
        if (f.confidence_scored !== false) html += `<div class="kb-fact-conf-bar" title="${escapeHtml(f.confidence_reason || '')}"><div class="kb-fact-conf-fill" style="width:${confPct}%;background:${confColor(conf)}"></div></div>`;
        html += '</div>';
        html += '<div class="kb-fact-body">';
        html += `<div class="kb-fact-title">${escapeHtml(kbCleanFactTitle(f.title) || f.slug || 'Untitled')}</div>`;
        html += `<div class="kb-fact-snippet">${escapeHtml(kbCleanFactPreview(f.body))}</div>`;
        html += '<div class="kb-fact-tags">';
        html += `<span class="kb-type-badge ${typeClass}">${escapeHtml(f.type || 'unknown')}</span>`;
        html += `<span class="kb-layer-badge ${layerClass}">${escapeHtml(f.layer || '')}</span>`;
        if (f.status) html += `<span class="kb-layer-badge" style="background:var(--bg);color:var(--muted)">${escapeHtml(f.status)}</span>`;
        html += kbAutoMemoryRowHTML(f);
        html += '</div></div></div>';
      }
      if (_kbPageOffset > 0 || hasNext) {
        html += '<div class="kb-pagination" role="group" aria-label="Knowledge fact pages">';
        html += '<button class="hv-btn btn-secondary btn-sm" data-action="kbPreviousPage"' + (_kbPageOffset === 0 ? ' disabled' : '') + '>Previous</button>';
        html += '<span> Page ' + (Math.floor(_kbPageOffset / _kbPageSize) + 1) + ' </span>';
        html += '<button class="hv-btn btn-secondary btn-sm" data-action="kbNextPage"' + (!hasNext ? ' disabled' : '') + '>Next</button></div>';
      }
      el.innerHTML = html;
    }

    function kbIsAutoMemory(f) {
      return (f.tags || []).some(t => String(t).trim().toLowerCase() === KB_AUTO_MEMORY_TAG);
    }

    // Agent tags of a drained CLI-memory fact, plus approve and discard for
    // an owner while it is still a draft. Discard deprecates rather than
    // deletes, so the drain's content hash keeps it from being re-drafted.
    function kbAutoMemoryRowHTML(f) {
      if (!kbIsAutoMemory(f)) return '';
      let html = '';
      for (const t of (f.tags || [])) {
        const m = /^agent:(.+)$/i.exec(String(t).trim());
        if (m) html += `<span class="kb-layer-badge" title="Drained from this agent's CLI memory">agent ${escapeHtml(m[1])}</span>`;
      }
      if (f.state === 'draft' && viewerIsOwner()) {
        const slug = escapeHtml(f.slug || '');
        html += `<button class="hv-btn btn-primary btn-sm" data-action="kbAutoMemoryReview" data-arg0="${slug}" data-arg1="approved" data-stop="1" title="Approve: agents whose scope includes this entry receive it in their kicks">Approve</button>`;
        html += `<button class="hv-btn btn-danger btn-sm" data-action="kbAutoMemoryReview" data-arg0="${slug}" data-arg1="deprecated" data-stop="1" title="Discard: mark it deprecated so no agent receives it and the drain never re-drafts it">Discard</button>`;
      }
      return html;
    }

    async function kbAutoMemoryReview(slug, state) {
      const question = state === 'approved'
        ? 'Approve "' + slug + '"? It was written by an agent, and once approved it is injected into the kicks of every agent whose scope includes it.'
        : 'Discard "' + slug + '"? It is marked deprecated, no agent receives it, and the drain will not re-draft it.';
      if (!(await hiveConfirm(question))) return;
      try {
        const r = await fetch('/api/knowledge/entry/' + encodeURIComponent(slug) + '/state', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ state, reason: 'auto-memory review (dashboard)' }),
        });
        if (!r.ok) { const e = await r.json().catch(() => ({})); await hiveAlert(e.error || 'Failed to change state'); return; }
        fetchKnowledgeStats();
        kbSearch();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    function renderKnowledgeDetail() {
      const el = document.getElementById('kb-fact-detail');
      if (!el || !_kbSelectedFact) { if (el) el.innerHTML = ''; return; }
      const f = _kbSelectedFact;
      const conf = typeof f.confidence === 'number' ? f.confidence : 0;
      const confPct = Math.round(conf * 100);
      const typeClass = 'kb-type-' + (f.type || 'pattern');
      const layerClass = 'kb-layer-' + (f.layer || 'project');

      let html = '<div class="kb-detail">';
      html += `<div class="kb-detail-title">${escapeHtml(kbCleanFactTitle(f.title) || f.slug || 'Untitled')}</div>`;
      html += '<div style="display:flex;gap:var(--sp-3);margin-bottom:10px">';
      html += `<span class="kb-type-badge ${typeClass}">${escapeHtml(f.type || 'unknown')}</span>`;
      html += `<span class="kb-layer-badge ${layerClass}">${escapeHtml(f.layer || '')}</span>`;
      if (f.status) html += `<span class="kb-layer-badge" style="background:var(--bg);color:var(--muted)">${escapeHtml(f.status)}</span>`;
      html += '</div>';
      html += `<div class="kb-detail-body">${escapeHtml(f.body || 'No content')}</div>`;
      html += '<div class="kb-detail-meta">';
      if (f.confidence_scored === false) {
        html += `<div><span class="kb-detail-label">Confidence:</span> <span title="${escapeHtml(f.confidence_reason || '')}" class="kb-conf-unscored">unscored</span></div>`;
      } else {
        html += `<div><span class="kb-detail-label">Confidence:</span> <span title="${escapeHtml(f.confidence_reason || '')}" style="color:${confColor(conf)};font-weight:600">${confPct}%</span></div>`;
      }
      html += `<div><span class="kb-detail-label">Slug:</span> ${escapeHtml(f.slug || '')}</div>`;
      if (f.tags && f.tags.length) html += `<div><span class="kb-detail-label">Tags:</span> ${f.tags.map(t => escapeHtml(t)).join(', ')}</div>`;
      html += '</div>';

      // Action buttons
      html += '<div style="display:flex;gap:var(--sp-3);margin-top:var(--sp-5);flex-wrap:wrap">';
      html += `<button class="hv-btn btn-secondary nous-btn btn-primary btn-sm" data-action="kbOpenEdit">✏️ Edit</button>`;
      const promoteTargets = { personal: 'project', project: 'org', org: 'community' };
      const promoteTo = promoteTargets[f.layer];
      if (promoteTo) {
        html += `<button class="hv-btn btn-secondary nous-btn btn-primary btn-sm" data-action="kbPromote" data-arg0="${escapeHtml(f.slug)}" data-arg1="${escapeHtml(f.layer)}" data-arg2="${promoteTo}">⬆ Promote to ${promoteTo}</button>`;
      }
      html += `<button class="hv-btn btn-danger nous-btn btn-sm" data-action="kbDelete" data-arg0="${escapeHtml(f.slug)}" data-arg1="${escapeHtml(f.layer)}">🗑 Delete</button>`;
      html += '</div>';

      html += '</div>';
      el.innerHTML = html;

      // Highlight selected fact in list
      document.querySelectorAll('.kb-fact-item').forEach(item => item.classList.remove('selected'));
      const items = document.querySelectorAll('.kb-fact-item');
      for (const item of items) {
        if (item.querySelector('.kb-fact-title')?.textContent === (kbCleanFactTitle(f.title) || f.slug)) {
          item.classList.add('selected');
          break;
        }
      }
    }

    // ── Knowledge modals and actions ──

    function kbShowModal(title, bodyHtml, opts) {
      // opts.dialogClass hands sizing to a stylesheet recipe (e.g.
      // .kb-sources-dialog) instead of the inline width, so the mobile
      // full-screen tiers and desktop sizing live in one place.
      const dialogClass = (opts && opts.dialogClass) ? ' ' + opts.dialogClass : '';
      const dialogWidth = (opts && opts.width) || '600px';
      const dialogStyle = dialogClass ? '' : ` style="width:${dialogWidth}"`;
      let overlay = document.getElementById('kb-modal-overlay');
      if (!overlay) {
        overlay = document.createElement('div');
        overlay.id = 'kb-modal-overlay';
        overlay.className = 'nous-config-overlay';
        document.body.appendChild(overlay);
      }
      overlay.style.display = 'flex';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = `<div class="nous-config-dialog${dialogClass}"${dialogStyle}>
        <h2 style="font-size:1rem;margin:var(--sp-0) var(--sp-0) var(--sp-0) var(--sp-0);padding:var(--sp-8) var(--sp-8) var(--sp-6);display:flex;align-items:center;justify-content:space-between;flex-shrink:0">
          ${title}
          <button class="hv-btn btn-icon btn-sm" data-action="kbCloseModal">✕</button>
        </h2>
        <div class="nous-config-body modal-scroll">${bodyHtml}</div>
      </div>`;
    }

    function kbCloseModal() {
      const overlay = document.getElementById('kb-modal-overlay');
      if (overlay) overlay.style.display = 'none';
    }

    function kbOpenCreate() {
      const layers = (_kbCache || {}).layers || [];
      let layerOpts = '';
      for (const l of layers) {
        const sel = l.type === 'project' ? ' selected' : '';
        layerOpts += `<option value="${escapeHtml(l.type)}"${sel}>${escapeHtml(KB_LAYER_LABELS[l.type] || l.type)}</option>`;
      }
      if (!layerOpts) layerOpts = '<option value="project">Project</option>';

      let typeOpts = '';
      for (const t of KB_FACT_TYPES) {
        typeOpts += `<option value="${escapeHtml(t)}">${escapeHtml(t)}</option>`;
      }

      const html = `
        <div class="stack">
          <label class="muted-note">Title
            <input id="kb-create-title" type="text" class="kb-search-box mt-2" placeholder="Guard .join() against undefined">
          </label>
          <label class="muted-note">Body
            <textarea id="kb-create-body" rows="6" style="width:100%;margin-top:var(--sp-2);padding:var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg);font-size:0.8rem;font-family:inherit;resize:vertical" placeholder="Always use (arr || []).join(',') — hooks can return undefined when endpoints fail."></textarea>
          </label>
          <div style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:10px">
            <label class="muted-note">Layer
              <select id="kb-create-layer" class="field-input field-input-block">${layerOpts}</select>
            </label>
            <label class="muted-note">Type
              <select id="kb-create-type" class="field-input field-input-block">${typeOpts}</select>
            </label>
            <label class="muted-note">Confidence
              <input id="kb-create-conf" type="number" min="0" max="1" step="0.1" value="0.7" class="field-input field-input-block">
            </label>
          </div>
          <label class="muted-note">Tags (comma-separated)
            <input id="kb-create-tags" type="text" class="kb-search-box mt-2" placeholder="react, testing, hooks">
          </label>
          <div class="row-end mt-4">
            <button class="hv-btn btn-secondary nous-btn nous-config-btn-cancel" data-action="kbCloseModal">Cancel</button>
            <button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbSubmitCreate">Create Fact</button>
          </div>
        </div>`;
      kbShowModal('New Fact', html);
    }

    async function kbSubmitCreate() {
      const title = document.getElementById('kb-create-title')?.value?.trim();
      const body = document.getElementById('kb-create-body')?.value?.trim();
      const layer = document.getElementById('kb-create-layer')?.value;
      const type = document.getElementById('kb-create-type')?.value;
      const conf = parseFloat(document.getElementById('kb-create-conf')?.value || '0.7');
      const tagsRaw = document.getElementById('kb-create-tags')?.value || '';
      const tags = tagsRaw.split(',').map(t => t.trim()).filter(Boolean);

      if (!title || !body) { await hiveAlert('Title and body are required'); return; }

      try {
        const r = await fetch('/api/knowledge/create', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ title, body, layer, type, confidence: conf, tags }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to create'); return; }
        kbCloseModal();
        fetchKnowledgeStats();
        kbSearch();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    function kbOpenEdit() {
      if (!_kbSelectedFact) return;
      const f = _kbSelectedFact;

      let typeOpts = '';
      for (const t of KB_FACT_TYPES) {
        const sel = t === f.type ? ' selected' : '';
        typeOpts += `<option value="${escapeHtml(t)}"${sel}>${escapeHtml(t)}</option>`;
      }

      const html = `
        <div class="stack">
          <label class="muted-note">Title
            <input id="kb-edit-title" type="text" class="kb-search-box mt-2" value="${escapeHtml(f.title || '')}">
          </label>
          <label class="muted-note">Body
            <textarea id="kb-edit-body" rows="8" style="width:100%;margin-top:var(--sp-2);padding:var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg);font-size:0.8rem;font-family:inherit;resize:vertical">${escapeHtml(f.body || '')}</textarea>
          </label>
          <div style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:10px">
            <label class="muted-note">Type
              <select id="kb-edit-type" class="field-input field-input-block">${typeOpts}</select>
            </label>
            <label class="muted-note">Status
              <select id="kb-edit-status" class="field-input field-input-block">
                <option value="draft"${f.status === 'draft' ? ' selected' : ''}>Draft</option>
                <option value="verified"${f.status === 'verified' ? ' selected' : ''}>Verified</option>
                <option value="stale"${f.status === 'stale' ? ' selected' : ''}>Stale</option>
                <option value="archived"${f.status === 'archived' ? ' selected' : ''}>Archived</option>
              </select>
            </label>
            <label class="muted-note">Confidence
              <input id="kb-edit-conf" type="number" min="0" max="1" step="0.1" value="${f.confidence || 0.7}" class="field-input field-input-block">
            </label>
          </div>
          <label class="muted-note">Tags (comma-separated)
            <input id="kb-edit-tags" type="text" class="kb-search-box mt-2" value="${escapeHtml((f.tags || []).join(', '))}">
          </label>
          <div class="muted-xs">Layer: ${escapeHtml(f.layer || '')} · Slug: ${escapeHtml(f.slug || '')}</div>
          <div class="row-end mt-4">
            <button class="hv-btn btn-secondary nous-btn nous-config-btn-cancel" data-action="kbCloseModal">Cancel</button>
            <button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbSubmitEdit">Save Changes</button>
          </div>
        </div>`;
      kbShowModal('Edit Fact', html);
    }

    async function kbSubmitEdit() {
      if (!_kbSelectedFact) return;
      const f = _kbSelectedFact;
      const title = document.getElementById('kb-edit-title')?.value?.trim();
      const body = document.getElementById('kb-edit-body')?.value?.trim();
      const type = document.getElementById('kb-edit-type')?.value;
      const status = document.getElementById('kb-edit-status')?.value;
      const conf = parseFloat(document.getElementById('kb-edit-conf')?.value || '0.7');
      const tagsRaw = document.getElementById('kb-edit-tags')?.value || '';
      const tags = tagsRaw.split(',').map(t => t.trim()).filter(Boolean);

      try {
        const r = await fetch('/api/knowledge/' + encodeURIComponent(f.layer) + '/' + encodeURIComponent(f.slug), {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ title, body, type, status, confidence: conf, tags }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to update'); return; }
        kbCloseModal();
        kbReadFact(f.slug, f.layer);
        kbSearch();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbDelete(slug, layer) {
      if (!(await hiveConfirm('Delete fact "' + slug + '" from ' + layer + '?'))) return;
      try {
        const r = await fetch('/api/knowledge/' + encodeURIComponent(layer) + '/' + encodeURIComponent(slug), { method: 'DELETE' });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to delete'); return; }
        _kbSelectedFact = null;
        const detail = document.getElementById('kb-fact-detail');
        if (detail) detail.innerHTML = '';
        fetchKnowledgeStats();
        kbSearch();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbPromote(slug, fromLayer, toLayer) {
      const reason = await hivePrompt({title: 'Promote to ' + toLayer, label: 'Reason for promoting to ' + toLayer + ':', defaultValue: 'Useful for the broader team'});
      if (reason === null) return;
      try {
        const r = await fetch('/api/knowledge/promote', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ slug, from_layer: fromLayer, to_layer: toLayer, reason, promoter: 'dashboard' }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to promote'); return; }
        await hiveAlert('Fact promoted to ' + toLayer);
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    function kbOpenImport() {
      // Writable channels only — exclude the reserved automation vault and
      // read-only git sources so import never targets a non-writable layer (#3581).
      const layers = ((_kbCache || {}).layers || []).filter(l =>
        l && l.type && l.type !== RESERVED_KB_CHANNEL && !String(l.type).startsWith('git_source:'));
      let layerOpts = '';
      layers.forEach((l, i) => {
        const sel = i === 0 ? ' selected' : '';
        layerOpts += `<option value="${escapeHtml(l.type)}"${sel}>${escapeHtml(KB_LAYER_LABELS[l.type] || l.type)}</option>`;
      });
      if (!layerOpts) layerOpts = '<option value="" disabled selected>No channel yet — create one first</option>';

      const html = `
        <div class="stack">
          <p style="font-size:0.78rem;color:var(--muted);margin:var(--sp-0)">Paste markdown or JSON content. Markdown headings (# or ##) become separate facts. JSON should be an array of {title, body, type, tags} objects.</p>
          <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
            <label class="muted-note">Layer
              <select id="kb-import-layer" class="field-input field-input-block">${layerOpts}</select>
            </label>
            <label class="muted-note">Format
              <select id="kb-import-format" class="field-input field-input-block">
                <option value="markdown" selected>Markdown</option>
                <option value="json">JSON</option>
              </select>
            </label>
          </div>
          <label class="muted-note">Content <span aria-hidden="true">*</span>
            <textarea id="kb-import-content" required aria-required="true" rows="24" style="width:100%;margin-top:var(--sp-2);padding:var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg);font-size:0.78rem;font-family:var(--font-mono);resize:vertical;min-height:320px" placeholder="# Guard .join() against undefined\n\nAlways use (arr || []).join(',')..."></textarea>
          </label>
          <div style="display:flex;align-items:center;gap:10px;margin-top:var(--sp-2)">
            <label style="font-size:0.72rem;color:var(--muted);cursor:pointer;display:flex;align-items:center;gap:var(--sp-2)">
              <span>or load from file:</span>
              <input type="file" accept=".md,.json,.txt" style="font-size:0.72rem" data-change-action="kbLoadFile" data-arg-types="t">
            </label>
          </div>
          <div class="row-end mt-4">
            <button class="hv-btn btn-secondary nous-btn nous-config-btn-cancel" data-action="kbCloseModal">Cancel</button>
            <button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbSubmitImport">Import</button>
          </div>
        </div>`;
      kbShowModal('📥 Import Facts', html, { width: 'min(700px, 80vw)', maxHeight: '85vh' });
    }

    function kbLoadFile(input) {
      const file = input.files[0];
      if (!file) return;
      const reader = new FileReader();
      reader.onload = function(e) {
        const textarea = document.getElementById('kb-import-content');
        if (textarea) textarea.value = e.target.result;
        const formatSel = document.getElementById('kb-import-format');
        if (formatSel && file.name.endsWith('.json')) formatSel.value = 'json';
      };
      reader.readAsText(file);
    }

    async function kbSubmitImport() {
      const content = document.getElementById('kb-import-content')?.value?.trim();
      const layer = document.getElementById('kb-import-layer')?.value;
      const format = document.getElementById('kb-import-format')?.value;

      if (!content) { kbFieldError(document.getElementById('kb-import-content'), 'Content is required'); return; }
      if (!layer) { kbFieldError(document.getElementById('kb-import-layer'), 'Pick a channel to import into, or create one with "+ New channel".'); return; }
      if (layer === RESERVED_KB_CHANNEL) { kbFieldError(document.getElementById('kb-import-layer'), RESERVED_KB_CHANNEL + ' is reserved for automation. Choose or create another channel.'); return; }

      try {
        const r = await fetch('/api/knowledge/import', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ content, layer, format }),
        });
        const data = await r.json();
        if (!r.ok) { await hiveAlert(data.error || 'Failed to import'); return; }
        kbCloseModal();
        await hiveAlert('Imported ' + (data.imported || 0) + ' facts to ' + layer);
        fetchKnowledgeStats();
        kbSearch();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    // kbNewChannel reveals an inline text input to name a new knowledge channel
    // (rather than a blocking the in-app prompt, which clashes with the dashboard's
    // modal conventions). This is the path for adding knowledge on channels other
    // than the reserved automation vault (issue #3581).
    function kbNewChannel() {
      const row = document.getElementById('kb-newchannel-row');
      if (row) { row.style.display = 'flex'; document.getElementById('kb-newchannel-name')?.focus(); }
    }

    // kbCreateChannel POSTs the new channel, then adds + selects it in the import
    // dropdown without a full re-render.
    async function kbCreateChannel() {
      const input = document.getElementById('kb-newchannel-name');
      const name = input?.value?.trim();
      if (!name) { await hiveAlert('Enter a channel name.'); return; }
      if (name === RESERVED_KB_CHANNEL) { await hiveAlert(RESERVED_KB_CHANNEL + ' is reserved for automation.'); return; }
      try {
        const r = await fetch('/api/knowledge/channels', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ name }),
        });
        const data = await r.json();
        if (!r.ok) { await hiveAlert(data.error || 'Failed to create channel'); return; }
        const created = (data.channel && data.channel.name) || name;
        await fetchKnowledgeStats();
        const sel = document.getElementById('kb-import-layer');
        if (sel) {
          let opt = Array.from(sel.options).find(o => o.value === created);
          if (!opt) {
            opt = document.createElement('option');
            opt.value = created;
            opt.textContent = KB_LAYER_LABELS[created] || created;
            const ph = Array.from(sel.options).find(o => o.disabled);
            if (ph) ph.remove();
            sel.appendChild(opt);
          }
          sel.value = created;
        }
        if (input) input.value = '';
        const row = document.getElementById('kb-newchannel-row');
        if (row) row.style.display = 'none';
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    // kbRemoveChannel disconnects the currently-selected channel: it stops feeding
    // agents (un-indexed), leaving its files on disk. Resolves the channel's
    // on-disk path from the channels list, since disconnect keys on path.
    async function kbRemoveChannel() {
      const sel = document.getElementById('kb-import-layer');
      const name = sel?.value;
      if (!name) { await hiveAlert('No channel selected.'); return; }
      if (name === RESERVED_KB_CHANNEL) { await hiveAlert(RESERVED_KB_CHANNEL + ' is reserved and cannot be removed.'); return; }
      // Only user-created channels (vaults) are removable; the static writable
      // layers (project/org/community/personal) are not channels we can disconnect.
      if (['project', 'org', 'community', 'personal'].includes(name)) {
        await hiveAlert('"' + name + '" is a built-in layer, not a removable channel.');
        return;
      }
      if (!await hiveConfirm('Remove channel "' + name + '"? It stops feeding agents. By default the imported files stay on disk (you can re-add the channel later).', 'Remove channel')) return;
      // Offer the destructive option separately, defaulting to keep-files.
      const purge = await hiveConfirm('Also DELETE the imported files permanently? This cannot be undone.\n\nOK = delete files · Cancel = keep files on disk', 'Delete files?');
      try {
        // Resolve the channel name → on-disk path from the channels list.
        const listR = await fetch('/api/knowledge/channels');
        const channels = listR.ok ? await listR.json() : [];
        const match = (channels || []).find(c => c.name === name);
        if (!match || !match.root_dir) { await hiveAlert('Could not resolve the channel path to remove.'); return; }
        const r = await fetch('/api/knowledge/vaults', {
          method: 'DELETE', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ path: match.root_dir, purge }),
        });
        const data = await r.json();
        if (!r.ok) { await hiveAlert(data.error || 'Failed to remove channel'); return; }
        // Drop it from the dropdown and refresh stats.
        const opt = Array.from(sel.options).find(o => o.value === name);
        if (opt) opt.remove();
        await fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    function kbOpenSubscriptions() {
      // The dialog body (.nous-config-body) is the one scroll container —
      // a nested fixed-height scroller here left two competing scrollers and
      // the bottom of the form unreachable (#11786).
      // Sizing lives in the .kb-sources-dialog recipe: a wide, fixed-height
      // dialog on desktop so the two columns get breathing room, full-screen
      // on the phone tiers.
      kbShowModal('🔗 Knowledge Sources', '<div id="kb-subs-content"><div class="kb-integrations-loading">Loading...</div></div>', { dialogClass: 'kb-sources-dialog' });
      kbLoadSubscriptions();
    }

    async function kbLoadSubscriptions() {
      const el = document.getElementById('kb-subs-content');
      if (!el) return;

      try {
        const [subsR, gsR, docsR] = await Promise.all([
          fetch('/api/knowledge/subscriptions'),
          fetch('/api/knowledge/git-sources'),
          fetch('/api/knowledge/documents'),
        ]);
        const subs = await subsR.json();
        const gitSources = await gsR.json();
        const documents = docsR.ok ? await docsR.json() : [];
        const layers = (_kbCache || {}).layers || [];

        let html = '';

        // Explainer
        html += '<div class="kb-sources-intro">';
        html += '<strong class="text-default">How agents use knowledge sources</strong><br>';
        html += 'Before each kick, the primer queries all connected sources and injects the most relevant facts into the agent\'s dialog. Agents never talk to sources directly &mdash; knowledge is primed deterministically by file paths and keywords.';
        html += '</div>';

        // ── Two-column grid ──
        html += '<div class="kb-sources-grid">';

        // ════ LEFT COLUMN: Connections ════
        html += '<div class="kb-sources-col">';

        // ── Git Sources section ──
        html += '<div class="kb-sources-section">';
        html += '<div class="row mb-4">';
        html += '<span class="title-sm">Git Sources</span>';
        html += '<span class="tag-chip">cloned &amp; indexed locally</span>';
        html += '</div>';
        html += '<div class="kb-sources-note">Clone a GitHub repo locally. Hive indexes the markdown with Fisher-Rao scored search and syncs every 5 minutes.</div>';

        _kbFailedGitSources.forEach((fs, i) => {
          html += '<div class="card-inset row-between mb-3" data-kb-gs-error="1">';
          html += '<div class="kb-gs-error-main">';
          html += '<div class="kb-gs-error-title">';
          html += '<span class="status-dot" data-status="error"></span>';
          html += `<span class="kb-gs-error-name">${escapeHtml(fs.name)}</span></div>`;
          html += `<div class="kb-gs-error-meta">${escapeHtml(fs.url)}`;
          if (fs.subpath) html += ` / ${escapeHtml(fs.subpath)}`;
          html += ` &middot; ${escapeHtml(fs.branch || 'repo default')}</div>`;
          html += `<div role="alert" class="kb-gs-error-reason">status: error &mdash; ${escapeHtml(fs.error)}</div>`;
          html += '</div>';
          html += `<button class="hv-btn nous-btn btn-sm" data-action="kbRetryGitSource" data-arg0="${i}">Retry</button>`;
          html += `<button class="hv-btn btn-danger nous-btn btn-sm" data-action="kbDismissGitSourceError" data-arg0="${i}">Dismiss</button>`;
          html += '</div>';
        });

        if (gitSources && gitSources.length > 0) {
          for (const gs of gitSources) {
            const gsStatus = statusMeta(gs.ready ? 'ready' : 'initializing');
            const statusText = gsStatus.label;
            html += '<div class="card-inset row-between mb-3">';
            html += '<div style="min-width:0;flex:1">';
            html += `<div style="font-size:var(--fs-base);font-weight:600;color:var(--fg);display:flex;align-items:center;gap:var(--sp-3);white-space:nowrap;overflow:hidden">`;
            html += `<span class="status-dot" data-status="${gsStatus.status}" style="width:7px;height:7px;min-width:7px"></span>`;
            html += `<span style="overflow:hidden;text-overflow:ellipsis">${escapeHtml(gs.name)}</span></div>`;
            const repoHref = gs.subpath ? gs.url + '/tree/' + (gs.branch || 'main') + '/' + gs.subpath : gs.url;
            html += `<div style="font-size:0.7rem;color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">`;
            html += `<a href="${escapeHtml(repoHref)}" target="_blank" rel="noopener" style="color:var(--accent);text-decoration:none" title="Open repo in new tab">${escapeHtml(gs.url)}`;
            if (gs.subpath) html += ` / ${escapeHtml(gs.subpath)}`;
            html += ` ↗</a>`;
            html += ` &middot; ${escapeHtml(gs.layer)} &middot; ${gs.pages} pages &middot; ${statusText}</div>`;
            html += '</div>';
            html += `<button class="hv-btn btn-danger nous-btn btn-sm" data-action="gh18" data-arg0="${escapeHtml(gs.url)}" data-arg1="${escapeHtml(gs.subpath || '')}">Remove</button>`;
            html += '</div>';
          }
        } else if (_kbFailedGitSources.length === 0) {
          html += '<div class="empty-dashed">No git sources yet.</div>';
        }

        html += '<details open class="kb-sources-add"><summary class="summary-label">Add Git Source</summary>';
        html += '<div class="stack kb-sources-form">';
        html += '<input id="kb-gs-url" type="text" class="kb-search-box" required aria-required="true" placeholder="Repository URL * (https://github.com/org/repo)">';
        html += '<input id="kb-gs-name" type="text" class="kb-search-box" required aria-required="true" placeholder="Display name *">';
        html += '<input id="kb-gs-subpath" type="text" class="kb-search-box" placeholder="Subpath (e.g. docs/skills)">';
        html += '<input id="kb-gs-branch" type="text" class="kb-search-box" placeholder="Branch (blank = repo default)">';
        html += `<select id="kb-gs-layer" class="field-input">
          <option value="project">Project</option>
          <option value="org">Org</option>
          <option value="community">Community</option>
          <option value="personal">Personal</option>
        </select>`;
        html += '<div class="row-end">';
        html += '<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbAddGitSource">Clone &amp; Index</button>';
        html += '</div></div></details>';
        html += '</div>';

        // ── Wiki Subscriptions section ──
        html += '<div class="kb-sources-section">';
        html += '<div class="row mb-4">';
        html += '<span class="title-sm">Wiki Subscriptions</span>';
        html += '<span class="tag-chip">remote llm-wiki endpoint</span>';
        html += '</div>';
        html += '<div class="kb-sources-note">A running llm-wiki HTTP service. Hive queries it via MCP at prime time &mdash; facts stay remote, only search results are fetched.</div>';

        if (subs && subs.length > 0) {
          for (const s of subs) {
            html += '<div class="card-inset row-between mb-3">';
            html += `<div><div style="font-size:var(--fs-base);font-weight:600;color:var(--fg)">${escapeHtml(s.name || s.url)}</div>`;
            html += `<div class="muted-xs">${escapeHtml(s.url)} &middot; ${escapeHtml(s.layer || 'org')}</div></div>`;
            html += `<button class="hv-btn btn-danger nous-btn btn-sm" data-action="kbRemoveSub" data-arg0="${escapeHtml(s.url)}">Remove</button>`;
            html += '</div>';
          }
        } else {
          html += '<div class="empty-dashed">No subscriptions yet.</div>';
        }

        html += '<details open class="kb-sources-add"><summary class="summary-label">Add Subscription</summary>';
        html += '<div class="stack kb-sources-form">';
        html += '<input id="kb-sub-url" type="text" class="kb-search-box" required aria-required="true" placeholder="URL * (https://wiki.example.com/mcp)">';
        html += `<input id="kb-sub-name" type="text" class="kb-search-box" placeholder="Display name">`;
        html += `<select id="kb-sub-layer" class="field-input">
          <option value="project">Project</option>
          <option value="org">Org</option>
          <option value="community">Community</option>
          <option value="personal">Personal</option>
        </select>`;
        html += '<div class="row-end">';
        html += '<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbAddSub">Add</button>';
        html += '</div></div></details>';
        html += '</div>';

        html += '</div>'; // end left column

        // ════ RIGHT COLUMN: Imports ════
        html += '<div class="kb-sources-col">';

        // ── Import Documents section ──
        html += '<div class="kb-sources-section">';
        html += '<div class="row mb-4">';
        html += '<span class="title-sm">Documents</span>';
        html += '<span class="tag-chip">PDF, HTML, DOCX, text</span>';
        html += '</div>';
        html += '<div class="kb-sources-note">Import external documents from a URL. Content is parsed into chunks and stored as knowledge facts that agents can search and cite.</div>';

        if (documents && documents.length > 0) {
          for (const d of documents) {
            const dIcon = DOC_CONTENT_ICONS[d.content_type] || '📄';
            const dSource = d.source_url || d.source_file || '';
            const dFacts = d.fact_count || 0;
            html += '<div class="card-inset row-between mb-3">';
            html += '<div style="min-width:0;flex:1">';
            html += `<div style="font-size:var(--fs-base);font-weight:600;color:var(--fg);display:flex;align-items:center;gap:var(--sp-3)">${dIcon} <span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${escapeHtml(d.slug || d.title || 'unnamed')}</span></div>`;
            html += `<div style="font-size:0.7rem;color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${escapeHtml(dSource)} &middot; ${dFacts} facts</div>`;
            html += '</div>';
            html += '<div style="display:flex;gap:var(--sp-2);flex-shrink:0;margin-left:var(--sp-4)">';
            html += `<button class="hv-btn btn-secondary nous-btn btn-primary btn-sm" data-action="kbReimportDoc" data-arg0="${escapeHtml(d.slug)}" title="Re-fetch and re-extract facts">🔄</button>`;
            html += `<button class="hv-btn btn-danger nous-btn btn-sm" data-action="kbDeleteDoc" data-arg0="${escapeHtml(d.slug)}">Remove</button>`;
            html += '</div></div>';
          }
        } else {
          html += '<div class="empty-dashed">No documents imported yet.</div>';
        }

        html += '<details open class="kb-sources-add"><summary class="summary-label">Import Document</summary>';
        html += '<div class="stack kb-sources-form">';
        html += '<input id="kb-doc-url" type="text" class="kb-search-box" required aria-required="true" placeholder="URL * (https://arxiv.org/pdf/2309.06180)">';
        html += '<input id="kb-doc-name" type="text" class="kb-search-box" placeholder="Name (optional, derived from URL)">';
        html += `<select id="kb-doc-layer" class="field-input">
          <option value="project">Project</option>
          <option value="org">Org</option>
          <option value="community">Community</option>
          <option value="personal">Personal</option>
        </select>`;
        html += '<div class="row-end">';
        html += '<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbImportDocFromModal">Import</button>';
        html += '</div></div></details>';

        html += '<details class="kb-sources-add"><summary class="summary-label">🎙️ Plaud voice notes</summary>';
        html += '<div class="stack kb-sources-form">';
        html += '<div class="muted-xs">Make your Plaud Note recordings (Plaud summary + transcript) searchable knowledge via the free official Plaud MCP server. $0 beyond your Plaud plan; sign-in is Plaud OAuth in your browser.</div>';
        html += '<div><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbOpenPlaudIntegration">Configure in Integrations</button></div>';
        html += '</div></details>';

        html += '<details class="kb-sources-add"><summary class="summary-label">🗣️ Wispr Flow dictation</summary>';
        html += '<div class="stack kb-sources-form">';
        html += '<div class="muted-xs">Dictate straight into any field here — put the cursor in <strong>+ New Fact</strong> and hold the Flow key; no connector needed. To also pull your past dictations in, the Wispr Flow connector reads the app\'s local history (flow.sqlite or an export folder). $0 beyond your Wispr Flow plan.</div>';
        html += '<div><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbOpenWisprFlowIntegration">Configure in Integrations</button></div>';
        html += '</div></details>';

        html += '<details open class="kb-sources-add"><summary class="summary-label">📚 Import from Context7</summary>';
        html += '<div class="stack kb-sources-form">';
        html += '<div class="muted-xs">Search Context7 for library documentation (e.g. "vllm", "next.js", "kubernetes").</div>';
        html += '<div><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbOpenContext7Integration">Configure in Integrations</button></div>';
        html += '<div class="kb-sources-inline">';
        html += '<input id="kb-ctx7-query" type="text" class="kb-search-box" placeholder="Search libraries...">';
        html += '<button class="hv-btn btn-secondary nous-btn btn-primary btn-sm" data-action="kbCtx7Search">Search</button>';
        html += '</div>';
        html += '<div id="kb-ctx7-results" style="max-height:200px;overflow-y:auto"></div>';
        html += '</div></details>';
        html += '</div>';

        // ── Import Facts section ──
        // Build the import-target list from user-WRITABLE channels only. The
        // reserved automation vault (RESERVED_KB_CHANNEL) and read-only git-source
        // pseudo-layers are excluded — offering them caused #3581, where importing
        // into bead-synth-wiki failed with "has no configured endpoint". Users can
        // also create a new channel inline; if there is nothing to import into yet,
        // the select shows a disabled prompt rather than a bad target.
        const importChannels = (layers || []).filter(l =>
          l && l.type && l.type !== RESERVED_KB_CHANNEL && !String(l.type).startsWith('git_source:'));
        const layerOpts2 = importChannels.map((l, i) => {
          const sel = i === 0 ? ' selected' : '';
          return `<option value="${escapeHtml(l.type)}"${sel}>${escapeHtml(KB_LAYER_LABELS[l.type] || l.type)}</option>`;
        }).join('') || '<option value="" disabled selected>No channel yet — create one below</option>';

        html += '<div class="kb-sources-section">';
        html += '<div class="row mb-4">';
        html += '<span class="title-sm">Import Facts</span>';
        html += '<span class="tag-chip">markdown or JSON</span>';
        html += '</div>';
        html += '<div class="kb-sources-note">Paste markdown or JSON directly. Headings (# or ##) become separate facts. JSON should be an array of {title, body, type, tags} objects.</div>';

        html += '<details open class="kb-sources-add"><summary class="summary-label">Paste Content</summary>';
        html += '<div class="stack kb-sources-form">';
        html += `<div class="kb-sources-inline">`;
        html += `<select id="kb-import-layer" class="field-input">${layerOpts2}</select>`;
        html += `<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbNewChannel" title="Create a new knowledge channel to import into">+ New channel</button>`;
        html += `<button class="hv-btn btn-danger nous-btn" data-action="kbRemoveChannel" title="Remove the selected channel (stops it feeding agents; files remain on disk)">Remove</button>`;
        html += `</div>`;
        html += `<div id="kb-newchannel-row" class="kb-sources-inline" style="display:none">`;
        html += `<input id="kb-newchannel-name" type="text" class="kb-search-box" placeholder="New channel name (e.g. Runbooks)" data-keydown-action="kbCreateChannel" data-prevent="1" data-keys="Enter">`;
        html += `<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbCreateChannel">Create</button>`;
        html += `</div>`;
        html += `<select id="kb-import-format" class="field-input">
          <option value="markdown" selected>Markdown</option>
          <option value="json">JSON</option>
        </select>`;
        html += '<textarea id="kb-import-content" rows="8" class="kb-sources-textarea" required aria-required="true" placeholder="# Guard .join() against undefined\n\nAlways use (arr || []).join(\',\')..."></textarea>';
        html += '<div class="row-between kb-sources-actions">';
        html += '<label class="kb-sources-file"><span>or load from file:</span><input type="file" accept=".md,.json,.txt" data-change-action="kbLoadFile" data-arg-types="t"></label>';
        html += '<button class="hv-btn btn-primary nous-btn btn-blue" data-action="kbSubmitImport">Import</button>';
        html += '</div></div></details>';
        html += '</div>';

        html += '</div>'; // end right column
        html += '</div>'; // end two-column grid

        el.innerHTML = html;
      } catch (e) {
        el.innerHTML = '<div style="color:var(--red);padding:var(--sp-5)">Failed to load knowledge sources: ' + escapeHtml(e.message) + '</div>';
      }
    }

    function kbClearFieldError(el) {
      if (!el) return;
      el.classList.remove('field-invalid');
      el.removeAttribute('aria-invalid');
      const next = el.nextElementSibling;
      if (next && next.classList.contains('field-error')) next.remove();
    }

    // Marks a required input invalid, shows msg directly under it and focuses it; clears on the next edit.
    function kbFieldError(el, msg) {
      if (!el) { showToast(msg, 'error'); return; }
      kbClearFieldError(el);
      el.classList.add('field-invalid');
      el.setAttribute('aria-invalid', 'true');
      const err = document.createElement('div');
      err.className = 'field-error';
      err.setAttribute('role', 'alert');
      err.textContent = msg;
      el.insertAdjacentElement('afterend', err);
      el.addEventListener('input', () => kbClearFieldError(el), { once: true });
      el.addEventListener('change', () => kbClearFieldError(el), { once: true });
      el.focus();
    }

    // Connect attempts that failed, kept so the Sources panel shows them as
    // error rows (with the reason) instead of only a transient alert.
    var _kbFailedGitSources = [];

    function _kbForgetFailedGitSource(url, subpath) {
      _kbFailedGitSources = _kbFailedGitSources.filter(f => !(f.url === url && f.subpath === subpath));
    }

    async function kbAddGitSource() {
      const url = document.getElementById('kb-gs-url')?.value?.trim();
      const name = document.getElementById('kb-gs-name')?.value?.trim();
      const subpath = document.getElementById('kb-gs-subpath')?.value?.trim() || '';
      const branch = document.getElementById('kb-gs-branch')?.value?.trim() || '';
      const layer = document.getElementById('kb-gs-layer')?.value;

      if (!url) { kbFieldError(document.getElementById('kb-gs-url'), 'Repository URL is required'); return; }
      if (!name) { kbFieldError(document.getElementById('kb-gs-name'), 'Display name is required'); return; }

      const btn = document.querySelector('#kb-subs-content [data-action="kbAddGitSource"]');
      if (btn) { btn.disabled = true; btn.textContent = 'Cloning…'; }

      let failure = '';
      try {
        // A blank branch is resolved server-side to the remote's default branch.
        const r = await fetch('/api/knowledge/git-sources', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ url, name, subpath, branch, layer }),
        });
        if (!r.ok) {
          const e = await r.json().catch(() => ({}));
          failure = e.error || ('HTTP ' + r.status);
        }
      } catch (e) {
        failure = e.message || String(e);
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Clone & Index'; }
      }
      _kbForgetFailedGitSource(url, subpath);
      if (failure) _kbFailedGitSources.push({ url, name, subpath, branch, layer, error: failure });
      kbLoadSubscriptions();
      if (failure) { await hiveAlert('Failed to add git source: ' + failure); return; }
      fetchKnowledgeStats();
    }

    function kbRetryGitSource(idx) {
      const f = _kbFailedGitSources[Number(idx)];
      if (!f) return;
      const set = (id, v) => { const el = document.getElementById(id); if (el) el.value = v || ''; };
      set('kb-gs-url', f.url);
      set('kb-gs-name', f.name);
      set('kb-gs-subpath', f.subpath);
      set('kb-gs-branch', f.branch);
      set('kb-gs-layer', f.layer || 'project');
      return kbAddGitSource();
    }

    function kbDismissGitSourceError(idx) {
      _kbFailedGitSources.splice(Number(idx), 1);
      kbLoadSubscriptions();
    }

    async function kbRemoveGitSource(url, subpath) {
      if (!(await hiveConfirm('Remove git source ' + url + (subpath ? ' (' + subpath + ')' : '') + '?'))) return;
      try {
        const r = await fetch('/api/knowledge/git-sources', {
          method: 'DELETE', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ url, subpath }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to remove'); return; }
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbAddSub() {
      const url = document.getElementById('kb-sub-url')?.value?.trim();
      const name = document.getElementById('kb-sub-name')?.value?.trim();
      const layer = document.getElementById('kb-sub-layer')?.value;

      if (!url) { kbFieldError(document.getElementById('kb-sub-url'), 'URL is required'); return; }

      try {
        const r = await fetch('/api/knowledge/subscriptions', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ url, name: name || url, layer }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to add'); return; }
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbRemoveSub(url) {
      if (!(await hiveConfirm('Remove subscription to ' + url + '?'))) return;
      try {
        const r = await fetch('/api/knowledge/subscriptions', {
          method: 'DELETE', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ url }),
        });
        if (!r.ok) { const e = await r.json(); await hiveAlert(e.error || 'Failed to remove'); return; }
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }


    async function kbImportDocFromModal() {
      const url = document.getElementById('kb-doc-url')?.value?.trim();
      const name = document.getElementById('kb-doc-name')?.value?.trim();
      const layer = document.getElementById('kb-doc-layer')?.value;

      if (!url) { kbFieldError(document.getElementById('kb-doc-url'), 'Document URL is required'); return; }

      const btn = document.querySelector('#kb-subs-content [data-action="kbImportDocFromModal"]');
      if (btn) { btn.disabled = true; btn.textContent = 'Importing...'; }

      try {
        const r = await fetch('/api/knowledge/documents', {
          method: 'POST', headers: {'Content-Type':'application/json'},
          body: JSON.stringify({ url, name: name || '', layer: layer || 'project' }),
        });
        if (!r.ok) { const t = await r.text(); await hiveAlert('Import failed: ' + t); return; }
        const meta = await r.json();
        await hiveAlert('Imported "' + (meta.slug || url) + '" → ' + (meta.fact_count || 0) + ' facts');
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) {
        await hiveAlert('Error: ' + e.message);
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Import'; }
      }
    }

    async function kbReimportDoc(slug) {
      const btn = document.querySelector('[onclick="kbReimportDoc(\'' + slug + '\')"]');
      if (btn) { btn.disabled = true; btn.textContent = '⏳'; }
      try {
        const r = await fetch('/api/knowledge/documents/' + encodeURIComponent(slug) + '/reimport', { method: 'POST' });
        if (!r.ok) { await hiveAlert('Reimport failed'); return; }
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function kbCtx7Search() {
      const query = document.getElementById('kb-ctx7-query')?.value?.trim();
      if (!query) return;
      const el = document.getElementById('kb-ctx7-results');
      if (!el) return;
      el.innerHTML = '<div style="padding:var(--sp-4);color:var(--muted);font-size:0.72rem">Searching...</div>';
      try {
        const r = await fetch('/api/knowledge/context7/search?q=' + encodeURIComponent(query));
        if (!r.ok) { el.innerHTML = '<div style="color:var(--red);font-size:0.72rem;padding:var(--sp-4)">Search failed</div>'; return; }
        const results = await r.json();
        if (!results || results.length === 0) { el.innerHTML = '<div style="padding:var(--sp-4);color:var(--muted);font-size:0.72rem">No libraries found.</div>'; return; }
        let html = '';
        for (const lib of results.slice(0, 8)) {
          const id = lib.id || '';
          const name = lib.title || id;
          const desc = lib.description || '';
          const trust = lib.trustScore ? lib.trustScore.toFixed(1) : '';
          const snippets = lib.totalSnippets || 0;
          html += '<div style="display:flex;align-items:center;justify-content:space-between;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r);margin-bottom:var(--sp-2);font-size:0.72rem">';
          html += '<div style="flex:1;min-width:0">';
          html += '<div style="font-weight:600;color:var(--fg)">' + escapeHtml(name) + '</div>';
          html += '<div style="color:var(--muted);font-size:var(--fs-xs);white-space:nowrap;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(id);
          if (trust) html += ' · trust ' + trust;
          if (lib.stars) html += ' · ' + formatNumber(lib.stars) + ' ★';
          if (snippets > 0) html += ' · ' + snippets + ' snippets';
          html += '</div>';
          if (desc) html += '<div style="color:var(--muted);font-size:var(--fs-xs);margin-top:var(--sp-1);overflow:hidden;text-overflow:ellipsis;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical">' + escapeHtml(desc) + '</div>';
          html += '</div>';
          html += '<button class="hv-btn btn-secondary nous-btn btn-primary btn-sm" data-action="kbImportCtx7" data-arg0="' + escapeHtml(id) + '" data-arg1="' + escapeHtml(name) + '">Import</button>';
          html += '</div>';
        }
        el.innerHTML = html;
      } catch (e) {
        el.innerHTML = '<div style="color:var(--red);font-size:0.72rem;padding:var(--sp-4)">Error: ' + escapeHtml(e.message) + '</div>';
      }
    }

    async function kbImportCtx7(libraryId, name) {
      const btn = event?.target;
      if (btn) { btn.disabled = true; btn.textContent = 'Importing...'; }
      try {
        const docName = name || libraryId.replace(/^\//, '').replace(/\//g, '-');
        const r = await fetch('/api/knowledge/documents', {
          method: 'POST', headers: {'Content-Type':'application/json'},
          body: JSON.stringify({ name: docName, context7_id: libraryId, layer: 'community' }),
        });
        if (!r.ok) { const t = await r.text(); await hiveAlert('Import failed: ' + t); return; }
        const meta = await r.json();
        await hiveAlert('Imported Context7 docs for "' + (meta.slug || libraryId) + '" → ' + (meta.fact_count || 0) + ' facts');
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) {
        await hiveAlert('Error: ' + e.message);
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Import'; }
      }
    }

    async function kbDeleteDoc(slug) {
      if (!(await hiveConfirm('Delete document "' + slug + '" and all extracted facts?'))) return;
      try {
        const r = await fetch('/api/knowledge/documents/' + encodeURIComponent(slug), { method: 'DELETE' });
        if (!r.ok) { await hiveAlert('Delete failed'); return; }
        kbLoadSubscriptions();
        fetchKnowledgeStats();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    // ── Knowledge: How it works modal ──

    function kbOpenHowItWorks() {
      const body = `
        <div style="font-size:var(--fs-base);line-height:1.6;color:var(--fg)">
          <div style="margin-bottom:18px">
            <h3 class="modal-h">How Knowledge Works</h3>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-3) var(--sp-0)">Hive organizes knowledge into <strong>4 layers</strong>, listed from highest to lowest priority:</p>
            <div style="display:flex;gap:var(--sp-3);margin:var(--sp-4) var(--sp-0);flex-wrap:wrap">
              <span style="background:var(--surface);border:1px solid var(--border);border-radius:var(--r-sm);padding:3px 8px;font-size:var(--fs-sm)">personal (highest)</span>
              <span class="muted">&rarr;</span>
              <span style="background:var(--surface);border:1px solid var(--border);border-radius:var(--r-sm);padding:3px 8px;font-size:var(--fs-sm)">project</span>
              <span class="muted">&rarr;</span>
              <span style="background:var(--surface);border:1px solid var(--border);border-radius:var(--r-sm);padding:3px 8px;font-size:var(--fs-sm)">org</span>
              <span class="muted">&rarr;</span>
              <span style="background:var(--surface);border:1px solid var(--border);border-radius:var(--r-sm);padding:3px 8px;font-size:var(--fs-sm)">community (lowest)</span>
            </div>
            <p class="muted-note m-0">When the same page exists in multiple layers, the higher-priority layer wins. Each page is a Markdown file with optional tags.</p>
          </div>

          <div style="margin-bottom:18px">
            <h3 class="modal-h">Knowledge Sources</h3>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-3) var(--sp-0)">Knowledge comes from manually-created facts, legacy knowledge sources, vaults, and external knowledge connectors. Use <strong>Integrations</strong> to configure Obsidian setup paths and connector-backed sources such as SharePoint, Confluence, Notion, Google Drive, GitHub Wiki, git, repo wiki, and documents.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>Wiki pages</strong> — Created directly in Hive with <strong>+ New Fact</strong>. Give each page a title, layer, tags, and content. Bulk-add with the <strong>Import</strong> button.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>Obsidian vaults</strong> — Connect a vault or any directory of markdown files. Hive reads all <code class="code-chip">.md</code> files, picks up <code class="code-chip">#tags</code>, and re-scans every 30 seconds.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>Git sources</strong> — Clone a GitHub repo (or a subdirectory within it) and index its markdown files locally. Configure git sources in <code class="code-chip">hive.yaml</code> or add them through the Knowledge Sources modal. Git sources sync every 5 minutes to pick up upstream changes.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>Wiki subscriptions</strong> — Follow a remote Hive instance or knowledge feed. New content syncs automatically on a periodic schedule.</p>
            <p class="muted-note m-0"><strong>Knowledge connectors</strong> — Connector entries under <code class="code-chip">knowledge.connectors</code> sync external systems into the vault on a schedule. Open <strong>Integrations</strong> to save connector config, validate credential references, enable/disable entries, and start a sync when the runtime is available.</p>
          </div>

          <div style="margin-bottom:18px">
            <h3 class="modal-h">Linting & Promotion</h3>
            <p class="m-0">Every page starts as a <strong>draft</strong>. The system automatically checks quality (title, tags, body length) and promotes pages through <strong>draft</strong> &rarr; <strong>reviewed</strong> &rarr; <strong>canonical</strong>. No action needed on your part — just write good content and the system handles the rest.</p>
          </div>

          <div style="margin-bottom:18px">
            <h3 class="modal-h">Fisher-Rao Scoring</h3>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-3) var(--sp-0)">Search uses <strong>Fisher-Rao geodesic distance</strong> on diagonal Gaussian embeddings to find the most relevant facts. This replaces simple substring matching with a richer geometric similarity measure.</p>
            <p class="muted-note m-0">Cold facts (fewer than 10 accesses) use cosine similarity. As facts are accessed more frequently, they graduate to the full Fisher-Rao distance, which captures both the direction and uncertainty of the embedding for more accurate ranking.</p>
          </div>

          <div style="margin-bottom:18px">
            <h3 class="modal-h">How Sources Combine</h3>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-3) var(--sp-0)">Pages from all sources (wiki pages, vaults, git sources, and subscriptions) are merged together. If two pages share the same title, the one from the higher-priority layer is used.</p>
            <p class="muted-note m-0">Priority order: personal &gt; project &gt; org &gt; community.</p>
          </div>

          <div style="margin-bottom:18px">
            <h3 class="modal-h">Sharing Knowledge</h3>
            <p class="m-0">Point multiple Hive instances at the same vault folder, use wiki subscriptions to share across teams, or add shared git sources so every instance indexes the same upstream docs. Everyone sees the same facts without copying files around.</p>
          </div>

          <div class="mb-2">
            <h3 class="modal-h">How Agents Use Knowledge (Priming)</h3>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-3) var(--sp-0)">Before each agent kick, the <strong>knowledge primer</strong> runs a pipeline to inject relevant facts into the agent's context:</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>1. Keyword extraction</strong> — The primer extracts keywords from the issue title assigned to the agent.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>2. Fisher-Rao search</strong> — Those keywords are searched across all connected sources (wiki layers, vaults, git sources) using Fisher-Rao scoring to find the most relevant facts.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-2) var(--sp-0)"><strong>3. Injection</strong> — Matching facts are injected into the kick message via the <code class="code-chip">\${KNOWLEDGE}</code> template variable. The agent receives a "Relevant Knowledge" section containing patterns, gotchas, and decisions relevant to its current work.</p>
            <p class="muted-note m-0">This means your agents get smarter as you add knowledge — no prompt editing required. Add a fact about a common mistake and every agent working on a related issue will see it automatically.</p>
          </div>
        </div>
      `;
      kbShowModal('📖 How Knowledge Works', body, { width: 'min(800px, 85vw)', maxHeight: '85vh' });
    }

    let _kbIntegrationSelected = 'obsidian';
    let _kbIntegrationEditName = '';

    function kbOpenIntegrations() {
      kbShowModal('🔌 Knowledge Integrations', '<div id="kb-integrations-content" class="kb-integrations-modal"><div class="kb-integrations-loading">Loading integrations…</div></div>', { width: 'min(1120px, 94vw)', maxHeight: '88vh' });
      kbLoadIntegrationsModal(_kbIntegrationSelected || 'obsidian');
    }

    async function kbLoadIntegrationsModal(selected) {
      _kbIntegrationSelected = selected || _kbIntegrationSelected || 'obsidian';
      try {
        const [cfgRes, stRes] = await Promise.all([fetch('/api/config/knowledge/connectors'), fetch('/api/config/knowledge/connectors/status')]);
        const cfg = await cfgRes.json().catch(() => ({}));
        if (!cfgRes.ok) throw new Error(cfg.error || ('HTTP ' + cfgRes.status));
        const st = stRes.ok ? await stRes.json().catch(() => ({})) : {};
        _kcState = Object.assign(_kcState, { loaded: true, error: '', connectors: cfg.connectors || [], types: cfg.types || [], ctx7AutoImport: cfg.context7_auto_import || [], layers: cfg.layers || _kcState.layers, rows: st.connectors || [], runtime: st.runtime !== false, publish: st.publish || null });
      } catch (e) {
        _kcState = Object.assign(_kcState, { loaded: true, error: e.message });
      }
      kbRenderIntegrationsModal();
      kbSyncProgressArm();
    }
    // After a status load: refresh the header indicator and, when a sync is
    // already running (interval or another owner), start the fast poll.
    function kbSyncProgressArm() {
      kbSyncIndicatorRender();
      if ((_kcState.rows || []).some(kcProgressActive) && !DASHBOARD_INTERVALS['kb-sync-progress']) dashboardSetInterval('kb-sync-progress', kbSyncProgressTick, KC_SYNC_POLL_MS);
    }

    function kbIntegrationTypes() {
      const configured = new Set(_kcState.types || []);
      const ordered = KB_CONNECTOR_TYPE_ORDER.filter(t => configured.has(t));
      (_kcState.types || []).forEach(t => { if (!ordered.includes(t)) ordered.push(t); });
      return ['obsidian'].concat(ordered);
    }

    function kbIntegrationLabel(type) {
      if (type === 'obsidian') return 'Obsidian';
      return (KC_TYPE_INFO[type] && KC_TYPE_INFO[type].label) || type;
    }

    function kbRenderIntegrationsModal() {
      const el = document.getElementById('kb-integrations-content');
      if (!el) return;
      if (_kcState.error) {
        el.innerHTML = '<div class="kb-notice">Failed to load integrations: ' + escapeHtml(_kcState.error) + '</div>';
        return;
      }
      const types = kbIntegrationTypes();
      if (!types.includes(_kbIntegrationSelected)) _kbIntegrationSelected = types[0] || 'obsidian';
      let html = '<div class="kb-integrations-shell">';
      html += '<div class="kb-integrations-sidebar">';
      html += '<div class="section-label m-0">Integrations</div>';
      html += '<div class="kb-layer-list kb-integrations-list">';
      types.forEach(t => {
        const active = t === _kbIntegrationSelected ? ' active' : '';
        const count = t === 'obsidian' ? '' : String((_kcState.connectors || []).filter(c => c.type === t).length || 0);
        const attr = t === 'obsidian' ? 'data-integration-type="obsidian"' : 'data-connector-type="' + escapeHtml(t) + '"';
        html += '<button class="hv-btn btn-secondary btn-sm kb-layer-btn' + active + '" data-action="kbSelectIntegration" data-arg0="' + escapeHtml(t) + '" ' + attr + '>';
        html += '<span class="kb-layer-label">' + kbIntegrationIcon(t) + escapeHtml(kbIntegrationLabel(t)) + '</span>';
        if (count) html += '<span class="kb-layer-count">' + count + '</span>';
        html += '</button>';
      });
      html += '</div><p class="muted-xs kb-integrations-note">Connector credentials are stored as environment-variable names or secret file paths only; pasted secrets are rejected.</p></div>';
      html += '<div class="kb-integrations-main">' + (_kbIntegrationSelected === 'obsidian' ? kbRenderObsidianIntegration() : kbRenderConnectorIntegration(_kbIntegrationSelected)) + '</div></div>';
      el.innerHTML = html;
    }

    function kbRenderObsidianIntegration() {
      const hiveHost = window.location.host;
      const modalOpts = { noBack: true, noForm: true, noDone: true };
      let body = '<div class="card-inset mb-5"><div class="title-sm kb-integration-title">' + kbIntegrationIcon('obsidian') + 'Obsidian</div><p class="muted-note m-0">Connect an Obsidian vault by mounting a Git-synced vault folder, or post individual notes through the webhook endpoint. This preserves the previous Obsidian setup guide inside the generic Integrations modal.</p></div>';
      body += renderGitSetupInstructions(hiveHost, modalOpts);
      body += '<div style="border-top:1px solid var(--border);margin:var(--sp-7) var(--sp-0)"></div>';
      body += renderWebhookSetupInstructions(hiveHost, modalOpts);
      return body;
    }

    // Badge shown when the hive's connector syncer is not running (status
    // response `runtime: false`): the entry is saved but this process never
    // constructed it, so sign-in, Check and Sync would all 404. Distinct from
    // `pending`, which is a loaded connector awaiting sign-in or first sync.
    // Only an explicit `runtime: false` counts: a failed or absent status
    // response must not tell the owner to restart the hive
    // (hivecommons/hive#18093).
    const KC_STATUS_NOT_LOADED = 'not loaded';
    const KC_NOT_LOADED_HINT = 'Connector runtime is not running on this hive — restart the hive after enabling knowledge connectors.';
    function kbConnectorNotLoaded(c) { return !!c.enabled && !_kcState.runtime; }
    function kbConnectorNotLoadedHtml() {
      return '<div class="muted-xs" data-kb-not-loaded>' + escapeHtml(KC_NOT_LOADED_HINT) + ' <a href="/docs/knowledge-connectors.md" target="_blank" rel="noopener">Docs ↗</a></div>';
    }

    // ── Connector sync progress (live) ──
    // Pure helpers over one status row (`/api/config/knowledge/connectors/status`
    // `connectors[]`, whose `progress` block the syncer updates as it goes).
    // Rendered on the Integrations card and the Knowledge header while a sync
    // runs; covered by knowledge_sync_ui.test.js.
    const KC_SYNC_POLL_MS = 2500;
    const KC_PROGRESS_ACTIVE_PHASES = ['listing', 'fetching', 'writing'];
    const KC_PROGRESS_TITLE_MAX = 60;
    function kcProgressActive(r) {
      if (!r) return false;
      const p = r.progress || {};
      return !!r.running || KC_PROGRESS_ACTIVE_PHASES.includes(p.phase);
    }
    function kcProgressStartMs(p) {
      const t = p && p.started_at ? Date.parse(p.started_at) : NaN;
      return isNaN(t) || t <= 0 ? 0 : t;
    }
    function kcElapsedText(startMs, nowMs) {
      if (!startMs) return '';
      const s = Math.max(0, Math.round(((nowMs || Date.now()) - startMs) / 1000));
      if (s < 60) return s + 's';
      const m = Math.floor(s / 60);
      if (m < 60) return m + 'm ' + (s % 60) + 's';
      return Math.floor(m / 60) + 'h ' + (m % 60) + 'm';
    }
    function kcProgressCount(p) {
      const done = (p && p.items_done) || 0;
      const total = (p && p.items_total) || 0;
      if (total > 0) return done + ' of ' + total;
      return done > 0 ? String(done) : '';
    }
    function kcProgressLabel(p) {
      p = p || {};
      const phase = KC_PROGRESS_ACTIVE_PHASES.includes(p.phase) ? p.phase : 'listing';
      const count = kcProgressCount(p);
      let text = phase + (count ? ' ' + count : (phase === 'listing' ? ' items…' : '…'));
      const title = (p.current || '').trim();
      if (title) text += ' · ' + (title.length > KC_PROGRESS_TITLE_MAX ? title.slice(0, KC_PROGRESS_TITLE_MAX) + '…' : title);
      return text;
    }
    function kcProgressHtml(r, nowMs) {
      if (!kcProgressActive(r)) return '';
      const p = (r && r.progress) || {};
      const total = p.items_total || 0;
      const done = Math.min(p.items_done || 0, total);
      const bar = total > 0
        ? '<progress class="kc-progress-bar" max="' + total + '" value="' + done + '"></progress>'
        : '<progress class="kc-progress-bar"></progress>';
      const elapsed = kcElapsedText(kcProgressStartMs(p), nowMs);
      return '<div class="kc-progress" data-kc-progress data-phase="' + escapeHtml(p.phase || 'listing') + '">' + bar
        + '<div class="kc-progress-copy"><span class="kc-progress-label">' + escapeHtml(kcProgressLabel(p)) + '</span>'
        + (elapsed ? '<span class="kc-progress-elapsed" title="elapsed">' + escapeHtml(elapsed) + '</span>' : '') + '</div></div>';
    }
    // Error of the sync that just failed, shown inline under the bar's place
    // until the next sync starts (the row's last_error line covers older ones).
    function kcProgressFailedHtml(r) {
      const p = (r && r.progress) || {};
      if (p.phase !== 'failed' || !p.last_error || (r && r.last_error === p.last_error)) return '';
      return '<div class="kc-error" data-kc-progress-error title="' + escapeHtml(p.last_error) + '">sync failed: ' + escapeHtml(p.last_error.length > 140 ? p.last_error.slice(0, 140) + '…' : p.last_error) + '</div>';
    }
    // Compact header text: "syncing: plaud 12/200" (+N when several run).
    function kcSyncIndicatorText(rows) {
      const active = (rows || []).filter(kcProgressActive);
      if (!active.length) return '';
      const r = active[0];
      const p = r.progress || {};
      let text = 'syncing: ' + (r.name || '?');
      if (p.items_total > 0) text += ' ' + (p.items_done || 0) + '/' + p.items_total;
      else if (p.items_done > 0) text += ' ' + p.items_done;
      if (active.length > 1) text += ' +' + (active.length - 1);
      return text;
    }
    // ── Connector sync progress end ──

    function kbSyncIndicatorRender() {
      const el = document.getElementById('kb-sync-badge');
      if (!el) return;
      const text = kcSyncIndicatorText(_kcState.rows);
      el.hidden = !text;
      el.textContent = text;
      el.title = text ? 'A knowledge connector is syncing; open Integrations for the progress bar.' : '';
    }
    // Re-render only the live parts of a connector row (pill, meta line,
    // progress bar, Sync now button) so an owner's half-typed form survives.
    function kbSyncProgressPatchRows() {
      const byName = {};
      (_kcState.rows || []).forEach(r => { byName[r.name] = r; });
      document.querySelectorAll('[data-kb-connector-name]').forEach(el => {
        const name = el.getAttribute('data-kb-connector-name');
        const c = (_kcState.connectors || []).find(x => x.name === name);
        if (!c) return;
        const r = byName[name] || {};
        const status = kbConnectorStatusOf(c, r);
        const pill = el.querySelector('.kc-pill');
        if (pill) { pill.textContent = status; pill.setAttribute('data-status', status); }
        const meta = el.querySelector('[data-kc-meta]');
        if (meta) meta.innerHTML = kbConnectorMetaHtml(c, r);
        const slot = el.querySelector('[data-kc-progress-slot]');
        if (slot) slot.innerHTML = kcProgressHtml(r) + kcProgressFailedHtml(r);
        const btn = el.querySelector('[data-action="kbIntegrationSync"]');
        if (btn && !kbConnectorNotLoaded(c)) btn.disabled = status === 'syncing';
      });
      document.querySelectorAll('tr[data-connector] [data-kc-progress-slot]').forEach(slot => {
        const row = slot.closest('tr');
        const name = row.getAttribute('data-connector');
        const r = byName[name] || {};
        slot.innerHTML = kcProgressHtml(r);
        const pill = row.querySelector('.kc-pill');
        if (pill && r.status) { pill.textContent = r.status; pill.setAttribute('data-status', r.status); }
      });
    }
    // One poll of the connector status; keeps polling every KC_SYNC_POLL_MS
    // while any connector is syncing and stops (with one full refresh of the
    // open Integrations modal / Settings table) when the last one finishes.
    async function kbSyncProgressTick() {
      let st;
      try {
        const r = await fetch('/api/config/knowledge/connectors/status');
        if (!r.ok) return;
        st = await r.json().catch(() => null);
      } catch (e) { return; }
      if (!st || !Array.isArray(st.connectors)) return;
      _kcState.rows = st.connectors;
      if (st.publish) _kcState.publish = st.publish;
      if (typeof st.runtime === 'boolean') _kcState.runtime = st.runtime;
      kbSyncIndicatorRender();
      kbSyncProgressPatchRows();
      const active = _kcState.rows.some(kcProgressActive);
      const polling = !!DASHBOARD_INTERVALS['kb-sync-progress'];
      if (active && !polling) dashboardSetInterval('kb-sync-progress', kbSyncProgressTick, KC_SYNC_POLL_MS);
      if (!active && polling) {
        dashboardClearInterval('kb-sync-progress');
        if (document.getElementById('kb-integrations-content')) kbLoadIntegrationsModal(_kbIntegrationSelected);
        if (document.getElementById('kc-connectors-root') && typeof renderKnowledgeConnectorsInto === 'function') renderKnowledgeConnectorsInto();
      }
    }

    function kbConnectorStatusOf(c, r) {
      return kbConnectorNotLoaded(c) ? KC_STATUS_NOT_LOADED : (r.status || (c.enabled ? 'pending' : 'disabled'));
    }
    function kbConnectorMetaHtml(c, r) {
      const publish = _kcState.publish && _kcState.publish.connector === c.name ? ' · publish ' + (_kcState.publish.running ? 'syncing' : _kcState.publish.last_error ? 'error' : _kcState.publish.last_success ? 'ok' : 'pending') : '';
      return escapeHtml(c.layer || 'project') + ' · ' + escapeHtml(c.interval || '15m') + ' · last sync ' + escapeHtml(kcAgo(r.last_sync)) + ' · pages/facts ' + (r.pages || 0) + '/' + (r.facts || 0) + escapeHtml(publish);
    }

    function kbConnectorRow(type, c, owner) {
      const byName = {};
      (_kcState.rows || []).forEach(r => { byName[r.name] = r; });
      const r = byName[c.name] || {};
      const notLoaded = kbConnectorNotLoaded(c);
      const status = kbConnectorStatusOf(c, r);
      let html = '<div class="card-inset row-between mb-3" data-kb-connector-name="' + escapeHtml(c.name) + '"><div class="kb-integration-row-main">';
      html += '<div class="kb-integration-connector-title"><code>' + escapeHtml(c.name) + '</code> <span class="kc-pill" data-status="' + escapeHtml(status) + '">' + escapeHtml(status) + '</span></div>';
      html += '<div class="muted-xs" data-kc-meta>' + kbConnectorMetaHtml(c, r) + '</div>';
      html += '<div data-kc-progress-slot>' + kcProgressHtml(r) + kcProgressFailedHtml(r) + '</div>';
      if (r.last_error) html += '<div class="kc-error" title="' + escapeHtml(r.last_error) + '">' + escapeHtml(r.last_error.length > 140 ? r.last_error.slice(0, 140) + '…' : r.last_error) + '</div>';
      if (notLoaded) html += kbConnectorNotLoadedHtml();
      else if (type === 'plaud') html += kbPlaudAuthLine(c.name, type) + kbPlaudPasteHtml(c.name);
      else if (type === 'zoom') html += kbPlaudAuthLine(c.name, type) + kbZoomPasteHtml(c.name, c);
      else if (type === 'teams') html += kbPlaudAuthLine(c.name, type) + kbTeamsConsentHtml(c.name, c);
      else if (type === 'discord') html += kbPlaudAuthLine(c.name, type) + kbDiscordHelpHtml(c.name, c);
      else if (type === 'google-meet') html += kbPlaudAuthLine(c.name, type) + kbGoogleMeetPasteHtml(c.name, c);
      else if (type === 'webex') html += kbPlaudAuthLine(c.name, type) + kbWebexPasteHtml(c.name, c);
      else if (type === 'slack-huddles') html += kbPlaudAuthLine(c.name, type) + kbSlackHuddlesHelpHtml(c.name, c);
      else if (type === 'otter') html += kbPlaudAuthLine(c.name, type) + kbOtterHelpHtml(c.name, c);
      else if (type === 'granola') html += kbPlaudAuthLine(c.name, type) + kbGranolaHelpHtml(c.name, c);
      else if (type === 'limitless') html += kbPlaudAuthLine(c.name, type) + kbLimitlessHelpHtml(c.name, c);
      else if (type === 'fathom') html += kbPlaudAuthLine(c.name, type) + kbFathomHelpHtml(c.name, c);
      else if (type === 'fireflies') html += kbPlaudAuthLine(c.name, type) + kbFirefliesHelpHtml(c.name, c);
      if (type === 'wisprflow') html += kbWisprFlowSourceLine(c);
      html += '</div>';
      if (owner) {
        const disabledAttr = ' disabled title="' + escapeHtml(KC_NOT_LOADED_HINT) + '"';
        html += '<div class="kb-integration-actions">';
        if (KB_AUTH_TYPES[type]) html += kbPlaudAuthButtons(c.name, notLoaded ? disabledAttr : '', type);
        html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbIntegrationSync" data-arg0="' + escapeHtml(c.name) + '"' + (notLoaded ? disabledAttr : status === 'syncing' ? ' disabled' : '') + '>Sync now</button>';
        html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbIntegrationEdit" data-arg0="' + escapeHtml(c.name) + '">Edit</button>';
        html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbIntegrationToggle" data-arg0="' + escapeHtml(c.name) + '">' + (c.enabled ? 'Disable' : 'Enable') + '</button>';
        html += '</div>';
      }
      html += '</div>';
      return html;
    }

    function kbRenderConnectorIntegration(type) {
      const info = KC_TYPE_INFO[type] || { label: type, description: 'External knowledge connector.' };
      const owner = viewerIsOwner();
      const connectors = (_kcState.connectors || []).filter(c => c.type === type);
      if (!_kbIntegrationEditName || !connectors.some(c => c.name === _kbIntegrationEditName)) _kbIntegrationEditName = connectors[0]?.name || '';
      const edited = connectors.find(c => c.name === _kbIntegrationEditName) || { name: '', type, enabled: true, layer: 'project', interval: '', scope: {}, auth: {} };
      let html = '<div class="card-inset mb-5"><div class="row-between"><div><div class="title-sm kb-integration-title">' + kbIntegrationIcon(type) + escapeHtml(info.label) + '</div><p class="muted-note m-0">' + escapeHtml(info.description) + '</p></div><span class="row">' + (KB_AUTH_TYPES[type] ? '<a href="/docs/inbox-voice-sources.md" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm">Inbox quickstart ↗</a>' : '') + '<a href="/docs/knowledge-connectors.md" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm">Docs ↗</a></span></div></div>';
      html += '<div class="row-between mb-4"><div class="section-label m-0">Configured entries</div>' + (owner ? '<button type="button" class="hv-btn btn-primary btn-sm" data-action="kbIntegrationNew" data-arg0="' + escapeHtml(type) + '">Add ' + escapeHtml(info.label) + '</button>' : '') + '</div>';
      html += connectors.length ? connectors.map(c => kbConnectorRow(type, c, owner)).join('') : '<div class="empty-dashed mb-4">No ' + escapeHtml(info.label) + ' connector configured yet.</div>';
      html += owner ? kbConnectorFormHTML(type, edited, !!edited.name) : '<p class="muted-note">Only the hive owner can edit connector configuration.</p>';
      return html;
    }

    function kbConnectorFormHTML(type, c, existing) {
      const opts = (arr, sel) => arr.map(v => '<option value="' + escapeHtml(v) + '"' + (v === sel ? ' selected' : '') + '>' + escapeHtml(v) + '</option>').join('');
      const authKind = c.auth && c.auth.env ? 'env' : (c.auth && c.auth.file ? 'file' : '');
      const authVal = (c.auth && (c.auth.env || c.auth.file)) || '';
      let html = '<div class="card-inset" data-kb-integration-form data-kb-type="' + escapeHtml(type) + '"><div class="title-sm mb-4">' + (existing ? 'Edit connector' : 'Add connector') + '</div>';
      html += '<div class="kb-integration-form-grid-3">';
      html += '<label class="muted-note">Name<input class="kb-search-box mt-2" type="text" data-kb-kc-name value="' + escapeHtml(c.name || '') + '"' + (existing ? ' readonly' : '') + ' placeholder="team-wiki" autocomplete="off"></label>';
      html += '<label class="muted-note">Layer<select class="field-input field-input-block mt-2" data-kb-kc-layer>' + opts(_kcState.layers || ['personal', 'project', 'org', 'community'], c.layer || 'project') + '</select></label>';
      html += '<label class="muted-note">Interval<input class="kb-search-box mt-2" type="text" data-kb-kc-interval value="' + escapeHtml(c.interval || '') + '" placeholder="15m" autocomplete="off"></label></div>';
      html += '<div class="kb-integration-form-grid-2">';
      html += '<label class="muted-note">Credential source<select class="field-input field-input-block mt-2" data-kb-kc-auth-kind><option value=""' + (authKind === '' ? ' selected' : '') + '>None</option><option value="env"' + (authKind === 'env' ? ' selected' : '') + '>Environment variable name</option><option value="file"' + (authKind === 'file' ? ' selected' : '') + '>Secret file path</option></select></label>';
      html += '<label class="muted-note">Credential reference<input class="kb-search-box mt-2" type="text" data-kb-kc-auth-value value="' + escapeHtml(authVal) + '" placeholder="TOKEN_ENV or /run/secrets/token" autocomplete="off"></label></div>';
      html += '<div class="kb-integration-scope-grid">' + kcScopeFieldsHTML(type, c.scope || {}) + '</div>';
      if (type === 'context7') html += kbContext7FormExtras(c);
      if (type === 'wisprflow') html += '<p class="muted-xs" data-kb-wisprflow-note>Leave the credential source as None: the connector reads Wispr Flow\'s local history read-only with the sqlite3 CLI. Leave the path empty for the default app location on the hive host (macOS: ~/Library/Application Support/Wispr Flow/flow.sqlite; Windows: %APPDATA%\\Wispr Flow\\flow.sqlite), or point it at a copy of flow.sqlite or a folder of exported .json/.ndjson/.csv/.md files. New dictations need no connector at all — dictate into + New Fact directly.</p>';
      if (type === 'plaud') html += '<p class="muted-xs" data-kb-plaud-note>Leave the credential source as None: Plaud signs in with its own OAuth flow (Connect on the entry above) and keeps the token in the MCP server\'s own store. Requires Node.js ≥ 20 on the hive host and costs nothing beyond your Plaud plan.</p>';
      if (type === 'zoom' || type === 'teams') html += kbMeetingAuthFormHTML(type, c);
      if (type === 'google-meet') html += kbMeetingAuthFormHTML(type, c);
      if (type === 'webex') html += kbMeetingAuthFormHTML(type, c);
      if (type === 'slack-huddles') html += '<p class="muted-xs" data-kb-slack-huddles-note>Set the credential source to <strong>Environment variable</strong> (or a secret file) holding a Slack token: a bot token (<code>xoxb-</code>) of an app installed in the workspace and invited to each channel, or a user token (<code>xoxp-</code>) for DMs. Scopes: <code>channels:history</code> and <code>groups:history</code> (plus <code>im:history</code> / <code>mpim:history</code> for DMs), <code>canvases:read</code> and <code>files:read</code> for the notes canvas, <code>users:read</code> for names and <code>channels:read</code> / <code>groups:read</code> for channel names. List the channel ids (C…, G…, D…) under Channels.</p>';
      if (type === 'otter') html += '<p class="muted-xs" data-kb-otter-note>Set the credential source to <strong>Environment variable</strong> (or a secret file) holding an Otter.ai API key: in Otter open <strong>Integrations → Developer</strong> and click Create key (Enterprise workspaces; the key is shown once, two per user); enter the NAME of the variable (for example <code>OTTER_API_KEY</code>), never the key itself.</p>';
      if (type === 'granola') html += '<p class="muted-xs" data-kb-granola-note>Set the credential source to <strong>Environment variable</strong> (or a secret file) holding a Granola API key (<code>grn_…</code>): create it in Granola\'s API settings (Business and Enterprise plans) and enter the NAME of the variable (for example <code>GRANOLA_API_KEY</code>), never the key itself.</p>';
      if (type === 'limitless') html += '<p class="muted-xs" data-kb-limitless-note>Set the credential source to <strong>Environment variable</strong> (or a secret file) holding a Limitless API key: in the Limitless desktop or web app open the <strong>Developer</strong> settings and create a key; enter the NAME of the variable (for example <code>LIMITLESS_API_KEY</code>), never the key itself. The Developer API is in beta and reads Pendant data only.</p>';
      if (type === 'fathom') html += '<p class="muted-xs" data-kb-fathom-note>Set the credential source to <strong>Environment variable</strong> (or a secret file) holding a Fathom API key: in Fathom open <strong>User Settings → API Access</strong> and generate a key; enter the NAME of the variable (for example <code>FATHOM_API_KEY</code>), never the key itself. The key sees the meetings its user recorded or that were shared with them or their team.</p>';
      if (type === 'fireflies') html += '<p class="muted-xs" data-kb-fireflies-note>Set the credential source to <strong>Environment variable</strong> (or a secret file) holding a Fireflies API key: in Fireflies open <strong>Integrations → Fireflies API</strong> and copy the key; enter the NAME of the variable (for example <code>FIREFLIES_API_KEY</code>), never the key itself. Audio playback and Deepgram fallback use the meeting audio link, which Fireflies serves on Pro plans and higher.</p>';
      if (type === 'discord') html += '<p class="muted-xs" data-kb-discord-note>Set the credential source to <strong>Environment variable</strong> and enter the NAME of the variable that holds the bot token (for example <code>DISCORD_BOT_TOKEN</code>); the value never passes through the dashboard. List the server id for scheduled events and the ids of the channels where finished recordings are posted.</p>';
      html += '<label class="muted-note kb-integration-enabled"><input type="checkbox" data-kb-kc-enabled' + (c.enabled ? ' checked' : '') + '> Enabled</label>';
      html += '<div class="hive-dialog-error" role="alert" data-kb-kc-msg></div>';
      html += '<div class="row-end kb-integration-form-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbIntegrationValidate">Validate</button><button type="button" class="hv-btn btn-primary btn-sm" data-action="kbIntegrationSave">Save</button></div></div>';
      return html;
    }

    // Context7 extras: key set/unset (reference only) and the legacy
    // knowledge.context7.auto_import list, edited here instead of a second control.
    function kbContext7FormExtras(c) {
      const set = c.credential_set === true;
      const lines = (_kcState.ctx7AutoImport || []).map(it => it.id + (it.query ? ' ' + it.query : '')).join('\n');
      let html = '<div class="muted-note" data-kb-ctx7-key-state="' + (set ? 'set' : 'unset') + '">API key: <strong>' + (set ? 'set' : 'unset') + '</strong> — enter an env var name or secret file path above (without one, <code>CONTEXT7_API_KEY</code> is used). The key value is never shown.</div>';
      html += '<label class="muted-note">Auto-import at startup (one library per line, optional topic after the ID)<textarea class="field-input field-input-block mt-2" rows="3" data-kb-ctx7-auto-import data-setting-key="knowledge.context7.auto_import[].id" data-setting-key-query="knowledge.context7.auto_import[].query" placeholder="/vercel/next.js routing" autocomplete="off">' + escapeHtml(lines) + '</textarea></label>';
      return html;
    }

    function kbReadContext7AutoImport() {
      const el = document.querySelector('[data-kb-ctx7-auto-import]');
      if (!el) return undefined;
      return el.value.split('\n').map(l => l.trim()).filter(Boolean).map(l => {
        const i = l.search(/\s/);
        return i < 0 ? { id: l } : { id: l.slice(0, i), query: l.slice(i).trim() };
      });
    }

    function kbReadIntegrationForm() {
      const box = document.querySelector('[data-kb-integration-form]');
      if (!box) return null;
      const val = sel => { const el = box.querySelector(sel); return el ? el.value.trim() : ''; };
      const scope = {};
      box.querySelectorAll('[data-kc-scope]').forEach(el => { if (el.value.trim()) scope[el.dataset.kcScope] = el.value.trim(); });
      const authKind = val('[data-kb-kc-auth-kind]');
      const authVal = val('[data-kb-kc-auth-value]');
      const auth = authKind === 'env' ? { env: authVal } : authKind === 'file' ? { file: authVal } : {};
      // OAuth client registration references (zoom, teams): variable NAMES only.
      box.querySelectorAll('[data-kb-kc-auth-field]').forEach(el => { const v = el.value.trim(); if (v) auth[el.dataset.kbKcAuthField] = v; });
      return { name: val('[data-kb-kc-name]'), type: box.dataset.kbType, enabled: !box.querySelector('[data-kb-kc-enabled]') || box.querySelector('[data-kb-kc-enabled]').checked, layer: val('[data-kb-kc-layer]'), interval: val('[data-kb-kc-interval]'), scope, auth };
    }

    // Zoom / Teams app registration: which environment variables hold the
    // client id, secret and account/tenant id. Values never pass through the
    // dashboard; the hive reads the variables at sync time.
    // Settings keys of the auth.*_env references, spelled out so the Settings
    // parity guard sees each hive.yaml leaf referenced by the dashboard.
    const KB_MEETING_AUTH_SETTING_KEYS = {
      client_id_env: 'knowledge.connectors[].auth.client_id_env',
      client_secret_env: 'knowledge.connectors[].auth.client_secret_env',
      account_id_env: 'knowledge.connectors[].auth.account_id_env',
      tenant_id_env: 'knowledge.connectors[].auth.tenant_id_env'
    };
    function kbMeetingAuthFormHTML(type, c) {
      const a = (c && c.auth) || {};
      const field = (key, label, placeholder) => '<label class="muted-note">' + escapeHtml(label) + '<input class="kb-search-box mt-2" type="text" data-kb-kc-auth-field="' + key + '" data-setting-key="' + KB_MEETING_AUTH_SETTING_KEYS[key] + '" value="' + escapeHtml(a[key] || '') + '" placeholder="' + escapeHtml(placeholder) + '" autocomplete="off"></label>';
      let html = '<div class="kb-integration-form-grid-2" data-kb-meeting-auth="' + escapeHtml(type) + '">';
      if (type === 'zoom') {
        const mode = a.mode || 'user_oauth';
        html += '<label class="muted-note">OAuth grant<select class="field-input field-input-block mt-2" data-kb-kc-auth-field="mode" data-setting-key="knowledge.connectors[].auth.mode"><option value="user_oauth"' + (mode === 'user_oauth' ? ' selected' : '') + '>User-level OAuth (my own recordings)</option><option value="server_to_server"' + (mode === 'server_to_server' ? ' selected' : '') + '>Server-to-Server (account; list users above)</option></select></label>';
        html += field('client_id_env', 'Client ID env var', 'ZOOM_CLIENT_ID');
        html += field('client_secret_env', 'Client secret env var', 'ZOOM_CLIENT_SECRET');
        html += field('account_id_env', 'Account ID env var (Server-to-Server only)', 'ZOOM_ACCOUNT_ID');
        html += '</div>';
        html += '<p class="muted-xs" data-kb-zoom-note>Leave the credential source as None. Register an OAuth app on the Zoom App Marketplace (General app for user-level, Server-to-Server OAuth for the account) with the scopes <code>cloud_recording:read:list_user_recordings</code>, <code>cloud_recording:read:list_recording_files</code>, <code>user:read:user</code> (classic: <code>recording:read</code>, <code>user:read</code>), plus <code>meeting_summary:read</code> and <code>meeting:read:list_past_participants</code> for the AI summary and participants. For user-level add the redirect URL above to the app\'s OAuth allow list; put the client id/secret in the hive\'s environment under the variable names you enter here. Tokens are kept at <code>/data/knowledge/connector-state/&lt;name&gt;/home/.zoom/tokens.json</code> (0600).</p>';
      } else if (type === 'google-meet') {
        const mode = a.mode || 'user_oauth';
        html += '<label class="muted-note">OAuth grant<select class="field-input field-input-block mt-2" data-kb-kc-auth-field="mode" data-setting-key="knowledge.connectors[].auth.mode"><option value="user_oauth"' + (mode === 'user_oauth' ? ' selected' : '') + '>User-level OAuth (my own meetings)</option><option value="server_to_server"' + (mode === 'server_to_server' ? ' selected' : '') + '>Service account with domain-wide delegation (list users above)</option></select></label>';
        html += field('client_id_env', 'OAuth client ID env var (user-level only)', 'GOOGLE_MEET_CLIENT_ID');
        html += field('client_secret_env', 'OAuth client secret env var (user-level only)', 'GOOGLE_MEET_CLIENT_SECRET');
        html += '</div>';
        html += '<p class="muted-xs" data-kb-google-meet-note>User-level OAuth: leave the credential source as None. In Google Cloud enable the <strong>Google Meet REST API</strong> and the <strong>Google Drive API</strong>, configure the OAuth consent screen with the scopes <code>meetings.space.readonly</code> and <code>drive.meet.readonly</code>, create an OAuth client (Web application) whose authorized redirect URI is the redirect URL above, and put its id and secret in the hive\'s environment under the variable names you enter here. Tokens are kept at <code>/data/knowledge/connector-state/&lt;name&gt;/home/.google-meet/tokens.json</code> (0600). Service account: leave the client id/secret fields empty, set the credential source to the environment variable or file holding the service-account key JSON, list the Workspace users to read under Users, and have a Workspace super admin authorize the service account\'s client id for <code>https://www.googleapis.com/auth/meetings.space.readonly</code> and <code>https://www.googleapis.com/auth/drive.meet.readonly</code> in Admin console → Security → API controls → Domain-wide delegation.</p>';
      } else if (type === 'webex') {
        html += field('client_id_env', 'Integration client ID env var', 'WEBEX_CLIENT_ID');
        html += field('client_secret_env', 'Integration client secret env var', 'WEBEX_CLIENT_SECRET');
        html += '</div>';
        html += '<p class="muted-xs" data-kb-webex-note>Leave the credential source as None. On developer.webex.com create an <strong>Integration</strong> with the redirect URL above and the scopes <code>meeting:recordings_read</code>, <code>meeting:transcripts_read</code>, <code>meeting:summaries_read</code> (needs Webex AI Assistant), <code>meeting:participants_read</code> and <code>spark:people_read</code>, and put its client id and secret in the hive\'s environment under the variable names you enter here (narrow the request with the <code>oauth_scopes</code> scope key when the integration has fewer). Tokens are kept at <code>/data/knowledge/connector-state/&lt;name&gt;/home/.webex/tokens.json</code> (0600).</p>';
      } else {
        html += field('tenant_id_env', 'Tenant ID env var', 'TEAMS_TENANT_ID');
        html += field('client_id_env', 'Client (application) ID env var', 'TEAMS_CLIENT_ID');
        html += field('client_secret_env', 'Client secret env var', 'TEAMS_CLIENT_SECRET');
        html += '</div>';
        html += '<p class="muted-xs" data-kb-teams-note>Leave the credential source as None. Register an app in Microsoft Entra (app registrations), add a client secret, and grant the <strong>application</strong> permissions <code>OnlineMeetingRecording.Read.All</code> and <code>OnlineMeetingTranscript.Read.All</code> (optionally <code>OnlineMeetings.Read.All</code> for meeting subject and attendees, <code>User.Read.All</code> to list organizers by UPN). A tenant administrator must grant admin consent and create an application access policy for the organizers (<code>New-CsApplicationAccessPolicy</code> / <code>Grant-CsApplicationAccessPolicy</code>); Connect shows the consent link when it is missing. Graph endpoints used: <code>/users/{id}/onlineMeetings/getAllRecordings</code>, <code>/getAllTranscripts</code> and the transcript <code>/content</code> as VTT.</p>';
      }
      return html;
    }

    function kbSetIntegrationMsg(text, ok) {
      const msg = document.querySelector('[data-kb-kc-msg]');
      if (msg) { msg.textContent = text; msg.dataset.ok = ok ? 'true' : 'false'; }
    }

    function kbSelectIntegration(type) { _kbIntegrationSelected = type || 'obsidian'; _kbIntegrationEditName = ''; kbRenderIntegrationsModal(); }
    function kbIntegrationEdit(name) { _kbIntegrationEditName = name || ''; kbRenderIntegrationsModal(); }
    function kbOpenContext7Integration() { _kbIntegrationSelected = 'context7'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbIntegrationNew(type) { _kbIntegrationSelected = type || _kbIntegrationSelected; _kbIntegrationEditName = ''; kbRenderIntegrationsModal(); }

    async function kbIntegrationValidate() {
      const next = kbReadIntegrationForm();
      if (!next) return;
      kbSetIntegrationMsg('Validating…', true);
      try {
        const r = await fetch('/api/config/knowledge/connectors/validate', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(next) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        kbSetIntegrationMsg(data.ok ? 'Configuration is valid.' : (data.error || 'Invalid configuration'), !!data.ok);
      } catch (e) { kbSetIntegrationMsg('Validation failed: ' + e.message, false); }
    }

    async function kbIntegrationSave() {
      const next = kbReadIntegrationForm();
      if (!next) return;
      const exists = (_kcState.connectors || []).some(c => c.name === next.name);
      const list = exists ? _kcState.connectors.map(c => c.name === next.name ? next : c) : (_kcState.connectors || []).concat([next]);
      try {
        const autoImport = next.type === 'context7' ? kbReadContext7AutoImport() : undefined;
        await kcSave(list, autoImport ? { context7_auto_import: autoImport } : undefined);
        _kbIntegrationSelected = next.type;
        _kbIntegrationEditName = next.name;
        await kbLoadIntegrationsModal(next.type);
        showToast('Connector ' + next.name + ' saved', 'success');
      } catch (e) { kbSetIntegrationMsg(e.message, false); }
    }

    async function kbIntegrationToggle(name) {
      const list = (_kcState.connectors || []).map(c => c.name === name ? Object.assign({}, c, { enabled: !c.enabled }) : c);
      try { await kcSave(list); await kbLoadIntegrationsModal(_kbIntegrationSelected); showToast('Connector ' + name + ' updated', 'success'); }
      catch (e) { showToast('Connector update failed: ' + e.message, 'error'); }
    }

    async function kbIntegrationSync(name) {
      await kcSyncNow(name);
      setTimeout(() => kbLoadIntegrationsModal(_kbIntegrationSelected), 1500);
    }

    // ── Interactive connector sign-in (Plaud OAuth through its MCP server;
    // Zoom OAuth app; Teams Graph application permissions) ──
    // Cached per connector name; loaded lazily when the card renders. The
    // kbPlaud* functions serve every KB_AUTH_TYPES type; the label follows
    // the connector type.
    const KB_AUTH_TYPES = { plaud: 'Plaud', zoom: 'Zoom', teams: 'Teams', discord: 'Discord', 'google-meet': 'Google Meet', webex: 'Webex', 'slack-huddles': 'Slack', fireflies: 'Fireflies', fathom: 'Fathom', limitless: 'Limitless', granola: 'Granola', otter: 'Otter' };
    const _kbPlaudAuth = {};
    function kbAuthLabel(name, type) {
      if (!type) { const c = (_kcState.connectors || []).find(x => x.name === name); type = c && c.type; }
      return KB_AUTH_TYPES[type] || 'Plaud';
    }
    function kbPlaudAuthLine(name, type) {
      const label = kbAuthLabel(name, type);
      const a = _kbPlaudAuth[name];
      if (!a) {
        setTimeout(() => kbPlaudAuthLoad(name, false), 0);
        return '<div class="muted-xs" data-kb-plaud-auth="' + escapeHtml(name) + '">' + label + ': checking sign-in…</div>';
      }
      const link = a.auth_url ? ' — <a href="' + escapeHtml(a.auth_url) + '" target="_blank" rel="noopener">open the ' + label + (type === 'teams' ? ' admin consent' : type === 'discord' ? ' bot invite' : ' authorization') + ' page ↗</a>' : '';
      let text;
      if (a.error) text = label + ': ' + escapeHtml(a.error);
      else if (a.pending) text = label + ': waiting for authorization' + (link || ' (URL pending…)');
      else if (a.connected) text = label + ': connected' + (a.account ? ' as <strong>' + escapeHtml(a.account) + '</strong>' : '');
      else text = label + ': not connected' + (a.message ? ' — ' + escapeHtml(a.message) : '') + link;
      return '<div class="muted-xs" data-kb-plaud-auth="' + escapeHtml(name) + '" data-connected="' + (a.connected ? '1' : '0') + '">' + text + '</div>';
    }
    // disabledAttr, when set, is appended to every button (connector not loaded, #18093).
    function kbPlaudAuthButtons(name, disabledAttr, type) {
      const label = kbAuthLabel(name, type);
      const a = _kbPlaudAuth[name] || {};
      const n = escapeHtml(name);
      const off = disabledAttr || '';
      let html = '';
      if (a.connected) html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudDisconnect" data-arg0="' + n + '"' + off + '>Disconnect</button>';
      else html += '<button type="button" class="hv-btn btn-primary btn-sm" data-action="kbPlaudConnect" data-arg0="' + n + '"' + (off || (a.pending ? ' disabled' : '')) + '>Connect ' + label + '</button>';
      html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudRefreshAuth" data-arg0="' + n + '"' + (off || ' title="Re-check sign-in with ' + label + '"') + '>Check</button>';
      return html;
    }
    // Zoom user-level OAuth on a hosted hive: the app's redirect URL is a
    // localhost address the browser cannot reach, so the operator pastes the
    // URL it landed on; the hive exchanges the code and keeps the refresh
    // token in the connector's token store (same endpoint as Plaud).
    function kbZoomPasteHtml(name, c) {
      const n = escapeHtml(name);
      const a = _kbPlaudAuth[name] || {};
      const mode = (c && c.auth && c.auth.mode) || 'user_oauth';
      const redirect = (c && c.scope && c.scope.redirect_uri) || 'http://localhost:8199/zoom/callback';
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Recordings are triaged in the Inbox, not here.</span></div>';
      if (mode === 'server_to_server') return '<p class="muted-xs" data-kb-zoom-s2s>Server-to-Server OAuth: Connect requests an account token with the client id, secret and account id from the environment variables named in the form; no browser step. Scopes to add on the Zoom app: <code>cloud_recording:read:list_user_recordings:admin</code>, <code>cloud_recording:read:list_recording_files:admin</code>, <code>user:read:user:admin</code>, optionally <code>meeting_summary:read:admin</code> and <code>meeting:read:list_past_participants:admin</code>.</p>';
      return '<details class="kb-plaud-paste mt-2" data-kb-zoom-paste><summary class="muted-xs">Finish Zoom sign-in by pasting the redirect URL</summary>' +
        '<p class="muted-xs">Registration: on the Zoom App Marketplace create a <strong>General</strong> (user-managed) OAuth app, add <code>' + escapeHtml(redirect) + '</code> to its OAuth Redirect URL and allow list, add the scopes <code>cloud_recording:read:list_user_recordings</code>, <code>cloud_recording:read:list_recording_files</code>, <code>user:read:user</code> (optionally <code>meeting_summary:read</code>, <code>meeting:read:list_past_participants</code>), and put the client id and secret in the hive environment under the variable names in the form. Then click <strong>Connect Zoom</strong>, approve in the browser, and paste the <code>' + escapeHtml(redirect) + '?code=…&amp;state=…</code> URL the browser lands on (a connection error page is expected) within 10 minutes.</p>' +
        '<textarea class="kb-search-box" id="kb-plaud-paste-' + n + '" rows="2" placeholder="' + escapeHtml(redirect) + '?code=…&amp;state=…"></textarea>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudPasteComplete" data-arg0="' + n + '">Submit</button>' + openInbox + '</div></details>';
    }
    // Teams: client credentials have no browser step; what can be missing
    // is the tenant administrator's consent (and application access policy).
    function kbTeamsConsentHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const users = (c && c.scope && (c.scope.users || c.scope.group)) || '';
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Recordings are triaged in the Inbox, not here.</span></div>';
      const consent = /admin consent/i.test(a.message || '') ? '<div class="kc-error" data-kb-teams-consent>Requires Entra admin consent for OnlineMeetingRecording.Read.All and OnlineMeetingTranscript.Read.All' + (a.auth_url ? ' — <a href="' + escapeHtml(a.auth_url) + '" target="_blank" rel="noopener">grant consent ↗</a>' : '') + '</div>' : '';
      return consent + '<p class="muted-xs" data-kb-teams-help>Microsoft Graph application permissions: Connect fetches a client-credentials token with the tenant id, client id and secret from the environment variables named in the form and probes the recordings of ' + (users ? '<code>' + escapeHtml(users) + '</code>' : 'the configured organizers') + '. A tenant administrator must grant admin consent to <code>OnlineMeetingRecording.Read.All</code> and <code>OnlineMeetingTranscript.Read.All</code> and assign an application access policy covering those organizers; without it Graph answers 403 and the card shows the consent link. Playback of Teams recordings in the Inbox is not available yet (Graph serves them behind a bearer token); transcripts and metadata sync.</p>';
    }
    // Discord: a bot token has no browser step; what can be missing is the
    // bot's membership in the server or its channel permissions (the card
    // then offers the bot invite link), or the Message Content intent.
    function kbDiscordHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const scope = (c && c.scope) || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Recordings and event notes are triaged in the Inbox, not here.</span></div>';
      const where = [scope.guild ? 'server <code>' + escapeHtml(scope.guild) + '</code>' : '', scope.channels ? 'channels <code>' + escapeHtml(scope.channels) + '</code>' : ''].filter(Boolean).join(' and ') || 'the configured server and channels';
      const access = /invite it/i.test(a.message || '') ? '<div class="kc-error" data-kb-discord-access>The bot cannot read ' + where + (a.auth_url ? ' — <a href="' + escapeHtml(a.auth_url) + '" target="_blank" rel="noopener">invite the bot ↗</a>' : '') + '</div>' : '';
      return access + '<p class="muted-xs" data-kb-discord-help>Discord has no cloud recordings or transcripts, so the hive reads what a meeting leaves behind: finished recordings posted to the recording channels (an audio upload, or a recording bot\'s download link such as Craig\'s, forwarded there), and scheduled events with their channel chat. Create an application in the Discord Developer Portal, add a bot, enable the <strong>Message Content</strong> intent, put the bot token in the hive environment, and invite the bot with View Channel and Read Message History. Connect probes ' + where + ' with that token. Audio uploads play in the Inbox; items without a transcript stay marked as not transcribed.</p>';
    }
    // Google Meet user-level OAuth on a hosted hive: the OAuth client's
    // redirect is a localhost address the browser cannot reach, so the
    // operator pastes the URL it landed on (same endpoint as Plaud and Zoom).
    // A service account has no browser step.
    function kbGoogleMeetPasteHtml(name, c) {
      const n = escapeHtml(name);
      const a = _kbPlaudAuth[name] || {};
      const mode = (c && c.auth && c.auth.mode) || 'user_oauth';
      const redirect = (c && c.scope && c.scope.redirect_uri) || 'http://localhost:8199/google-meet/callback';
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      const playback = '<span class="muted-xs" data-kb-google-meet-playback>Meet recordings are Drive files served behind a bearer token, so Inbox items link the Drive page instead of playing inline; transcripts and Gemini notes sync as text.</span>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + playback + '</div>';
      if (mode === 'server_to_server') return '<p class="muted-xs" data-kb-google-meet-sa>Service account with domain-wide delegation: Connect signs a token request as the first listed user with the key named in the form; no browser step. A Workspace super admin must authorize the service account\'s client id for <code>meetings.space.readonly</code> and <code>drive.meet.readonly</code> (Admin console → Security → API controls → Domain-wide delegation).</p>';
      return '<details class="kb-plaud-paste mt-2" data-kb-google-meet-paste><summary class="muted-xs">Finish Google sign-in by pasting the redirect URL</summary>' +
        '<p class="muted-xs">Registration: in Google Cloud enable the Google Meet REST API and the Google Drive API, add the scopes <code>meetings.space.readonly</code> and <code>drive.meet.readonly</code> to the OAuth consent screen, create an OAuth client (Web application) with <code>' + escapeHtml(redirect) + '</code> as an authorized redirect URI, and put its id and secret in the hive environment under the variable names in the form. Then click <strong>Connect Google Meet</strong>, approve in the browser, and paste the <code>' + escapeHtml(redirect) + '?state=…&amp;code=…</code> URL the browser lands on (a connection error page is expected) within 10 minutes.</p>' +
        '<textarea class="kb-search-box" id="kb-plaud-paste-' + n + '" rows="2" placeholder="' + escapeHtml(redirect) + '?state=…&amp;code=…"></textarea>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudPasteComplete" data-arg0="' + n + '">Submit</button>' + openInbox + '</div></details>';
    }
    // Webex integration on a hosted hive: the registered redirect is a
    // localhost address the browser cannot reach, so the operator pastes the
    // URL it landed on (same endpoint as Plaud, Zoom and Google Meet).
    function kbWebexPasteHtml(name, c) {
      const n = escapeHtml(name);
      const a = _kbPlaudAuth[name] || {};
      const redirect = (c && c.scope && c.scope.redirect_uri) || 'http://localhost:8199/webex/callback';
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs" data-kb-webex-playback>Recordings play in the Inbox through Webex\'s temporary direct download link (minted per click, valid 3 hours); transcripts and AI Assistant summaries sync as text.</span></div>';
      return '<details class="kb-plaud-paste mt-2" data-kb-webex-paste><summary class="muted-xs">Finish Webex sign-in by pasting the redirect URL</summary>' +
        '<p class="muted-xs">Registration: on developer.webex.com → My Webex Apps create an <strong>Integration</strong> with <code>' + escapeHtml(redirect) + '</code> as its redirect URI and the scopes <code>meeting:recordings_read</code>, <code>meeting:transcripts_read</code>, <code>meeting:summaries_read</code>, <code>meeting:participants_read</code>, <code>spark:people_read</code>, and put its client id and secret in the hive environment under the variable names in the form. Then click <strong>Connect Webex</strong>, approve in the browser, and paste the <code>' + escapeHtml(redirect) + '?code=…&amp;state=…</code> URL the browser lands on (a connection error page is expected) within 10 minutes.</p>' +
        '<textarea class="kb-search-box" id="kb-plaud-paste-' + n + '" rows="2" placeholder="' + escapeHtml(redirect) + '?code=…&amp;state=…"></textarea>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="kbPlaudPasteComplete" data-arg0="' + n + '">Submit</button>' + openInbox + '</div></details>';
    }
    // Slack huddles: a bot or user token has no browser step; the card says
    // what Slack keeps for a huddle (the AI notes canvas, with its transcript
    // when allowed; never audio) and what the token needs.
    function kbSlackHuddlesHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const chans = (c && c.scope && c.scope.channels) || '';
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Huddle notes are triaged in the Inbox, not here.</span></div>';
      const scopeHint = /needs scope/i.test(a.message || '') ? '<div class="kc-error" data-kb-slack-huddles-scope>The Slack token is missing a scope: add it on the app (OAuth &amp; Permissions), reinstall the app and click Check.</div>' : '';
      const channelHint = /\/invite/.test(a.message || '') ? '<div class="kc-error" data-kb-slack-huddles-invite>The app is not in ' + (chans ? '<code>' + escapeHtml(chans) + '</code>' : 'the channel') + ': run <code>/invite @your-app</code> there and click Check.</div>' : '';
      return scopeHint + channelHint + '<p class="muted-xs" data-kb-slack-huddles-help>Slack never records huddle audio. What it keeps is the <strong>AI notes canvas</strong> Slack posts in the huddle thread when someone turns on AI notes (paid plans), with the transcript embedded when the workspace allows it. The hive reads the huddle messages of ' + (chans ? '<code>' + escapeHtml(chans) + '</code>' : 'the configured channels') + ' (<code>conversations.history</code>), finds the notes canvas in the thread and reads it with <code>canvases.getContent</code>; huddles without notes are skipped. Inbox items are text only, with no playback.</p>';
    }
    // Fireflies: an API key has no browser step; the card says where the key
    // comes from and that playback needs a Pro plan (audio_url).
    function kbFirefliesHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs" data-kb-fireflies-playback>Meetings are triaged in the Inbox. ▶ Play uses Fireflies\' 24-hour audio link (Pro plan or higher).</span></div>';
      const paid = /paid_required|Pro plan/i.test(a.message || '') ? '<div class="kc-error" data-kb-fireflies-paid>Fireflies answered that this needs a paid plan (Pro or higher).</div>' : '';
      const limited = /too_many_requests|rate limit/i.test(a.message || '') ? '<div class="kc-error" data-kb-fireflies-ratelimit>Fireflies rate-limited the API key; the next sync retries (Free plan: 500 requests a day).</div>' : '';
      return paid + limited + '<p class="muted-xs" data-kb-fireflies-help>Fireflies.ai keeps a transcript, speakers and an AI summary for every meeting its notetaker joined. The hive reads them with the <a href="https://docs.fireflies.ai/graphql-api/query/transcripts" target="_blank" rel="noopener">Fireflies GraphQL API</a> (<code>transcripts</code>, 50 per page) using the API key from <strong>Fireflies → Integrations → Fireflies API</strong>. ' + (c && c.scope && /^true$/i.test(c.scope.mine || '') ? 'Only meetings you organized are read.' : 'Every meeting the key\'s owner can see is read.') + ' Click Check to verify the key.</p>';
    }
    // Fathom: an API key has no browser step; the card says which meetings
    // the key sees and how playback works (a recording download Fathom
    // prepares on request).
    function kbFathomHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs" data-kb-fathom-playback>Meetings are triaged in the Inbox. ▶ Play asks Fathom for a recording download; video can take a minute to prepare, so click again if it is not ready.</span></div>';
      const limited = /rate limit/i.test(a.message || '') ? '<div class="kc-error" data-kb-fathom-ratelimit>Fathom rate-limited the API key (60 calls a minute, 30 for transcripts and summaries); the next sync retries.</div>' : '';
      const recorders = (c && c.scope && c.scope.recorded_by) || '';
      return limited + '<p class="muted-xs" data-kb-fathom-help>Fathom keeps a transcript, an AI summary and action items for every call its notetaker recorded. The hive reads them with the <a href="https://developers.fathom.ai/api-reference/meetings/list-meetings" target="_blank" rel="noopener">Fathom API</a> (<code>GET /meetings</code>) using an API key from <strong>Fathom → User Settings → API Access</strong>. The key sees the meetings its user recorded and the ones shared with them or their team' + (recorders ? ', limited here to <code>' + escapeHtml(recorders) + '</code>' : '') + '. Click Check to verify the key.</p>';
    }
    // Limitless: an API key has no browser step; the card says the API reads
    // Pendant lifelogs only and that audio stays behind the key (no playback).
    function kbLimitlessHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Pendant conversations are triaged in the Inbox, not here.</span></div>';
      const limited = /rate limit/i.test(a.message || '') ? '<div class="kc-error" data-kb-limitless-ratelimit>Limitless rate-limited the API key (180 requests a minute); the next sync retries.</div>' : '';
      const starred = c && c.scope && /^true$/i.test(c.scope.starred_only || '');
      return limited + '<p class="muted-xs" data-kb-limitless-help>The Limitless Pendant records conversations and Limitless transcribes them into <strong>lifelogs</strong>. The hive reads them with the <a href="https://www.limitless.ai/developers" target="_blank" rel="noopener">Limitless Developer API</a> (<code>GET /v1/lifelogs</code>, Pendant data only) using an API key from the Limitless app\'s Developer settings' + (starred ? ', starred lifelogs only' : '') + '. Pendant audio is served only behind the key, so Inbox items are text only, with no playback. Click Check to verify the key.</p>';
    }
    // Granola: an API key has no browser step; the card says which notes the
    // key can read and that Granola keeps no audio.
    function kbGranolaHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs">Meeting notes are triaged in the Inbox, not here.</span></div>';
      const limited = /rate limit/i.test(a.message || '') ? '<div class="kc-error" data-kb-granola-ratelimit>Granola rate-limited the API key; the next sync retries.</div>' : '';
      const where = (c && c.scope && (c.scope.folder || c.scope.space)) || '';
      return limited + '<p class="muted-xs" data-kb-granola-help>Granola writes AI notes and a transcript for each meeting on your device and keeps no audio. The hive reads the notes with the <a href="https://docs.granola.ai/introduction" target="_blank" rel="noopener">Granola API</a> (<code>GET /v1/notes</code>) using an API key (<code>grn_…</code>, Business and Enterprise plans) from Granola\'s API settings' + (where ? ', limited here to <code>' + escapeHtml(where) + '</code>' : '') + '. A personal key reads your notes; a workspace key reads public notes and spaces with API access. Inbox items are text only, with no playback. Click Check to verify the key.</p>';
    }
    // Otter.ai: an API key has no browser step; the card says the Public API
    // needs an Enterprise workspace and how playback works.
    function kbOtterHelpHtml(name, c) {
      const a = _kbPlaudAuth[name] || {};
      const openInbox = '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="ocNavigate" data-arg0="inbox-section">📥 Open Inbox</button>';
      if (a.connected) return '<div class="kc-actions mt-2">' + openInbox + '<span class="muted-xs" data-kb-otter-playback>Conversations are triaged in the Inbox. ▶ Play uses Otter\'s MP3 download link, minted per click.</span></div>';
      const enterprise = /Public API/i.test(a.message || '') ? '<div class="kc-error" data-kb-otter-enterprise>Otter rejected the key. The Public API is available to Enterprise workspaces only; if Integrations → Developer is missing, ask your Otter account manager to turn it on.</div>' : '';
      const scope = (c && c.scope && c.scope.channel) ? 'the channel <code>' + escapeHtml(c.scope.channel) + '</code>' : (c && c.scope && /^true$/i.test(c.scope.include_shared || '') ? 'your conversations and the ones shared with you' : 'your own conversations');
      return enterprise + '<p class="muted-xs" data-kb-otter-help>Otter.ai keeps a transcript, an AI summary, an outline and action items for every conversation. The hive reads ' + scope + ' with the <a href="https://help.otter.ai/hc/en-us/articles/36130822688279-Otter-ai-Public-API" target="_blank" rel="noopener">Otter.ai Public API</a> (<code>GET /conversations</code>, Enterprise workspaces) using an API key from <strong>Integrations → Developer</strong>. Click Check to verify the key.</p>';
    }
    async function kbPlaudAuthLoad(name, refresh) {
      try {
        const r = await fetch('/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth' + (refresh ? '?refresh=1' : ''));
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbPlaudAuth[name] = data;
      } catch (e) {
        _kbPlaudAuth[name] = { error: e.message };
      }
      if (document.querySelector('[data-kb-plaud-auth="' + CSS.escape(name) + '"]')) kbRenderIntegrationsModal();
      return _kbPlaudAuth[name];
    }
    // One-line summary of a sign-in state for the Check toast (hivecommons/hive#18092).
    function kbPlaudAuthSummary(a) {
      if (a.connected) return 'connected' + (a.account ? ' as ' + a.account : '');
      if (a.pending) return 'waiting for authorization' + (a.auth_url ? ' — open the link on the card' : '');
      return 'not connected' + (a.message ? ' — ' + a.message : '');
    }
    // Check: re-query the sign-in state and always report the outcome, so a
    // 404 "no knowledge connectors are configured" reads as an error toast
    // with the server's message verbatim, not as silence (hivecommons/hive#18092).
    async function kbPlaudRefreshAuth(name) {
      const label = kbAuthLabel(name);
      showToast('Checking ' + label + ' sign-in…', 'info');
      const a = (await kbPlaudAuthLoad(name, true)) || {};
      if (a.error) showToast('Check failed: ' + a.error, 'error');
      else showToast(label + ' ' + name + ': ' + kbPlaudAuthSummary(a), a.connected ? 'success' : 'info');
    }
    async function kbPlaudConnect(name) {
      const label = kbAuthLabel(name);
      try {
        const r = await fetch('/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/connect', { method: 'POST' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbPlaudAuth[name] = data;
        kbRenderIntegrationsModal();
        if (data.connected) showToast(label + ' connected' + (data.account ? ' as ' + data.account : ''), 'success');
        else if (data.pending && data.auth_url) { showToast('Open the ' + label + ' authorization link shown on the card, then ' + (label === 'Zoom' ? 'paste the redirect URL into the card.' : 'click Check.'), 'info'); kbPlaudPollAuth(name, 0); }
        else if (data.auth_url) showToast(label + ': ' + (data.message || 'not connected') + ' — the consent link is on the card.', 'info');
        else showToast(label + ' login started; the authorization URL will appear on the card shortly.', 'info');
        if (!data.connected && !data.auth_url) kbPlaudPollAuth(name, 0);
      } catch (e) {
        showToast(label + ' connect failed: ' + e.message, 'error');
      }
    }
    // Poll the cached state while a login is pending (URL arrival, completion).
    function kbPlaudPollAuth(name, attempt) {
      if (attempt > 60) return;
      setTimeout(async () => {
        await kbPlaudAuthLoad(name, false);
        const a = _kbPlaudAuth[name] || {};
        if (a.connected) { showToast(kbAuthLabel(name) + ' connected' + (a.account ? ' as ' + a.account : ''), 'success'); return; }
        if (a.pending) kbPlaudPollAuth(name, attempt + 1);
      }, 5000);
    }
    async function kbPlaudDisconnect(name) {
      const label = kbAuthLabel(name);
      if (!(await hiveConfirm('Sign out of ' + label + '? Synced recordings stay in the vault until you remove the connector.'))) return;
      try {
        const r = await fetch('/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/disconnect', { method: 'POST' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kbPlaudAuth[name] = data;
        kbRenderIntegrationsModal();
        showToast(label + ' disconnected', 'success');
      } catch (e) {
        showToast(label + ' disconnect failed: ' + e.message, 'error');
      }
    }
    function kbOpenPlaudIntegration() { _kbIntegrationSelected = 'plaud'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenZoomIntegration() { _kbIntegrationSelected = 'zoom'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenTeamsIntegration() { _kbIntegrationSelected = 'teams'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenDiscordIntegration() { _kbIntegrationSelected = 'discord'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenGoogleMeetIntegration() { _kbIntegrationSelected = 'google-meet'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenWebexIntegration() { _kbIntegrationSelected = 'webex'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenSlackHuddlesIntegration() { _kbIntegrationSelected = 'slack-huddles'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenOtterIntegration() { _kbIntegrationSelected = 'otter'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenGranolaIntegration() { _kbIntegrationSelected = 'granola'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenLimitlessIntegration() { _kbIntegrationSelected = 'limitless'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenFathomIntegration() { _kbIntegrationSelected = 'fathom'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
    function kbOpenFirefliesIntegration() { _kbIntegrationSelected = 'fireflies'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }

    // ── Wispr Flow (local dictation history, no sign-in) ──
    function kbWisprFlowSourceLine(c) {
      const path = (c.scope && c.scope.path) || '';
      const group = (c.scope && c.scope.group) === 'day' ? 'one fact per day' : 'one fact per dictation';
      return '<div class="muted-xs" data-kb-wisprflow-source>Wispr Flow: ' + (path ? '<code>' + escapeHtml(path) + '</code>' : 'default app location on the hive host') + ' · ' + group + '</div>';
    }
    function kbOpenWisprFlowIntegration() { _kbIntegrationSelected = 'wisprflow'; _kbIntegrationEditName = ''; kbOpenIntegrations(); }
