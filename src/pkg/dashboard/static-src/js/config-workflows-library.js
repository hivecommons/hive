// Settings → Workflows (hivecommons/hive#17620, RFC phase 3).
    //
    // The spoke's home for workflow models: a Library of every stored model
    // (built-in hive-default, locally saved, ingested), a read-only detail
    // view with the compiled column/state diagram, diagnostics and bound
    // repositories, and an Ingest form that turns a repository's pipeline
    // files into a draft lifecycle model through POST /api/workflows/ingest.
    // Binding a repository to a model stays on Settings → Repos
    // (config-workflows.js); this tab never binds. Editing is phase 4.
    //
    // All HTML builders are pure functions of their inputs so the node
    // harness (workflows_library_ui_test.go) can run them without a DOM.
    const WF_LIB_VIEWS = ['library', 'detail', 'ingest', 'editor', 'marketplace'];
    const WF_PROVENANCE_LABELS = {
      'built-in': 'built-in',
      'curated': 'curated',
      'local': 'local',
      'ingested-unreviewed': 'ingested · unreviewed',
    };
    const WF_DIAGRAM = { colW: 172, colGap: 28, chipH: 30, chipGap: 10, headH: 30, pad: 14, textPad: 10, maxLabel: 22 };

    // ---- Edge routing, shared by the library diagram and the editor canvas ----
    // Quiet by default, detail on focus: transitions are orthogonal polylines
    // that leave a chip's port, run into the gutter between lanes, travel a
    // per-lane-pair channel and enter the target chip. Edges that share a
    // lane pair share one trunk (bundling). Backward edges go around the
    // outside above the lane headers; forward edges spanning two or more
    // lanes go around below. Labels are only drawn on hover/selection. All
    // functions here are pure and never mutate their inputs.
    const WF_EDGE = { chan: 8, radius: 6, fanMax: 4, selfR: 12 };

    // wfEdgeCommonEvent returns the event carried by the most transitions when
    // at least two share it; labels for that event are noise and never drawn.
    function wfEdgeCommonEvent(transitions) {
      const n = {};
      let best = '', bc = 0;
      (transitions || []).forEach(t => {
        const k = (t && t.on) || '';
        if (!k) return;
        n[k] = (n[k] || 0) + 1;
        if (n[k] > bc || (n[k] === bc && k < best)) { best = k; bc = n[k]; }
      });
      return bc >= 2 ? best : '';
    }

    // wfEdgeChannels counts the corridor channels a transition set needs above
    // (backward lane pairs) and below (forward pairs spanning 2+ lanes) the
    // lanes, so a layout can reserve the space before placing chips. colOf
    // maps a state id to its lane index or null when unplaced.
    function wfEdgeChannels(transitions, colOf) {
      const top = {}, bottom = {};
      (transitions || []).forEach(t => {
        const a = colOf(t.from), b = colOf(t.to);
        if (a == null || b == null || a === b) return;
        if (b < a - 1) top[a + '>' + b] = true;
        else if (b > a + 1) bottom[a + '>' + b] = true;
      });
      return { top: Object.keys(top).length, bottom: Object.keys(bottom).length };
    }
    function wfEdgeCorridor(n) { return n ? (n + 1) * WF_EDGE.chan : 0; }

    // wfOrthoPath turns an orthogonal polyline into a path with rounded corners.
    function wfOrthoPath(points, radius) {
      const R = v => Math.round(v * 10) / 10;
      const p = [];
      points.forEach(q => { const l = p[p.length - 1]; if (!l || l[0] !== q[0] || l[1] !== q[1]) p.push(q); });
      for (let i = 1; i < p.length - 1;) {
        const a = p[i - 1], b = p[i], c = p[i + 1];
        if ((a[0] === b[0] && b[0] === c[0]) || (a[1] === b[1] && b[1] === c[1])) p.splice(i, 1); else i++;
      }
      if (p.length < 2) return '';
      let d = `M${R(p[0][0])} ${R(p[0][1])}`;
      for (let i = 1; i < p.length - 1; i++) {
        const a = p[i - 1], b = p[i], c = p[i + 1];
        const r = Math.min(radius, Math.hypot(b[0] - a[0], b[1] - a[1]) / 2, Math.hypot(c[0] - b[0], c[1] - b[1]) / 2);
        const ux = Math.sign(b[0] - a[0]), uy = Math.sign(b[1] - a[1]), vx = Math.sign(c[0] - b[0]), vy = Math.sign(c[1] - b[1]);
        d += ` L${R(b[0] - ux * r)} ${R(b[1] - uy * r)} Q${R(b[0])} ${R(b[1])} ${R(b[0] + vx * r)} ${R(b[1] + vy * r)}`;
      }
      const e = p[p.length - 1];
      return d + ` L${R(e[0])} ${R(e[1])}`;
    }

    // wfRouteEdges routes transitions through the gutters of a lane layout.
    // geom = { cols: [{x,y,w,h}] in lane order, chips: {id: {x,y,w,h,colIndex,index}},
    // gap, pad }. opts = { dedupe, fanMax, expanded: {fanKey: true} }.
    // Returns { edges, fans, common } where each edge carries its path `d`,
    // a label anchor (lx, ly), kind (self|same|fwd|fwdlong|back), back,
    // common (its `on` is the model's most common event), and collapsed/fan
    // when it is folded into a fan-out trunk. Fans group >fanMax forward
    // edges from one state into one lane.
    function wfRouteEdges(transitions, geom, opts) {
      const o = opts || {}, K = WF_EDGE, fanMax = o.fanMax || K.fanMax, expanded = o.expanded || {};
      const cols = geom.cols || [], chips = geom.chips || {}, gap = geom.gap || 28, pad = geom.pad || 14;
      const common = wfEdgeCommonEvent(transitions);
      if (!cols.length) return { edges: [], fans: [], common: common };
      const laneTop = cols[0].y, laneBottom = cols[0].y + cols[0].h;
      const recs = [], seen = {};
      (transitions || []).forEach((t, i) => {
        if (!t) return;
        const a = chips[t.from], b = chips[t.to];
        if (!a || !b) return;
        const key = t.from + '>' + t.to;
        if (o.dedupe) { if (seen[key]) return; seen[key] = true; }
        const r = { index: i, from: t.from, to: t.to, on: t.on || '', note: t.note || '', key: key, self: t.from === t.to, back: false, kind: 'self', span: 0, d: '', lx: 0, ly: 0, common: !!t.on && t.on === common, collapsed: false, fan: '' };
        if (!r.self) {
          r.span = b.colIndex - a.colIndex;
          r.back = r.span < 0 || (r.span === 0 && b.index < a.index);
          r.kind = r.span === 0 ? 'same' : r.span === 1 ? 'fwd' : r.span > 1 ? 'fwdlong' : 'back';
        }
        recs.push(r);
      });
      // Channel reservations: gutter g sits between cols[g] and cols[g+1];
      // g = -1 is the margin left of the first lane, g = cols.length-1 the
      // margin right of the last.
      const gutters = {}, top = {}, bottom = {};
      const reserve = (g, key) => { (gutters[g] = gutters[g] || {})[key] = true; };
      recs.forEach(r => {
        const a = chips[r.from], b = chips[r.to], pair = a.colIndex + '>' + b.colIndex;
        if (r.kind === 'same') reserve(a.colIndex, 'S' + String(Math.abs(b.index - a.index)).padStart(3, '0'));
        else if (r.kind === 'fwd') reserve(a.colIndex, 'F' + pair);
        else if (r.kind === 'fwdlong') { reserve(a.colIndex, 'F' + pair + 'x'); reserve(b.colIndex - 1, 'F' + pair + 'e'); bottom[pair] = Math.abs(r.span); }
        else if (r.kind === 'back' && r.span === -1) reserve(b.colIndex, 'B' + pair);
        else if (r.kind === 'back') { reserve(a.colIndex - 1, 'B' + pair + 'x'); reserve(b.colIndex, 'B' + pair + 'e'); top[pair] = Math.abs(r.span); }
      });
      const chanX = {};
      Object.keys(gutters).forEach(gs => {
        const g = Number(gs);
        let left, width;
        if (g < 0) { left = cols[0].x - pad; width = pad; }
        else if (g >= cols.length - 1) { left = cols[cols.length - 1].x + cols[cols.length - 1].w; width = pad; }
        else { left = cols[g].x + cols[g].w; width = cols[g + 1].x - left; }
        // Same-lane bows nearest their lane, then forward trunks, then backward.
        const order = k => (k[0] === 'S' ? '0' : k[0] === 'F' ? '1' : '2') + k;
        const keys = Object.keys(gutters[gs]).sort((p, q) => order(p) < order(q) ? -1 : 1);
        keys.forEach((k, i) => { chanX[g + ':' + k] = left + width * (i + 1) / (keys.length + 1); });
      });
      // Corridor rows: shorter spans sit nearest the lanes so longer ones
      // pass outside them without crossing.
      const rows = (set, sign, base) => {
        const out = {};
        Object.keys(set).sort((p, q) => set[p] - set[q] || (p < q ? -1 : 1)).forEach((k, i) => { out[k] = base + sign * K.chan * (i + 1); });
        return out;
      };
      const topY = rows(top, -1, laneTop), bottomY = rows(bottom, 1, laneBottom);
      const mid = pts => {
        let best = 0, bl = -1;
        for (let i = 1; i < pts.length; i++) { const l = Math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1]); if (l > bl) { bl = l; best = i; } }
        return [(pts[best][0] + pts[best - 1][0]) / 2, (pts[best][1] + pts[best - 1][1]) / 2];
      };
      recs.forEach(r => {
        const a = chips[r.from], b = chips[r.to], pair = a.colIndex + '>' + b.colIndex;
        const y1 = a.y + a.h / 2, y2 = b.y + b.h / 2;
        let pts;
        if (r.self) {
          const px = a.x + a.w, R = K.selfR;
          r.d = `M${px} ${y1 - 6} C${px + R * 2} ${y1 - R * 2}, ${px + R * 2} ${y1 + R * 2}, ${px} ${y1 + 6}`;
          r.lx = px + R * 1.6; r.ly = y1;
          return;
        }
        if (r.kind === 'same') {
          const x = chanX[a.colIndex + ':S' + String(Math.abs(b.index - a.index)).padStart(3, '0')];
          pts = [[a.x + a.w, y1], [x, y1], [x, y2], [b.x + b.w, y2]];
        } else if (r.kind === 'fwd') {
          const x = chanX[a.colIndex + ':F' + pair];
          pts = [[a.x + a.w, y1], [x, y1], [x, y2], [b.x, y2]];
        } else if (r.kind === 'fwdlong') {
          const x1 = chanX[a.colIndex + ':F' + pair + 'x'], x2 = chanX[(b.colIndex - 1) + ':F' + pair + 'e'], cy = bottomY[pair];
          pts = [[a.x + a.w, y1], [x1, y1], [x1, cy], [x2, cy], [x2, y2], [b.x, y2]];
        } else if (r.span === -1) {
          // Adjacent lanes share one gutter: run straight down it.
          const x = chanX[b.colIndex + ':B' + pair];
          pts = [[a.x, y1], [x, y1], [x, y2], [b.x + b.w, y2]];
        } else {
          const x1 = chanX[(a.colIndex - 1) + ':B' + pair + 'x'], x2 = chanX[b.colIndex + ':B' + pair + 'e'], cy = topY[pair];
          pts = [[a.x, y1], [x1, y1], [x1, cy], [x2, cy], [x2, y2], [b.x + b.w, y2]];
        }
        r.d = wfOrthoPath(pts, K.radius);
        const m = mid(pts); r.lx = m[0]; r.ly = m[1];
      });
      // Fan-out: more than fanMax forward edges from one state into one lane
      // collapse to a trunk with a +N badge unless expanded.
      const groups = {};
      recs.forEach(r => { if (r.kind === 'fwd' || r.kind === 'fwdlong') (groups[r.from + '>' + chips[r.to].colIndex] = groups[r.from + '>' + chips[r.to].colIndex] || []).push(r); });
      const fans = [];
      Object.keys(groups).sort().forEach(key => {
        const list = groups[key];
        if (list.length <= fanMax) return;
        const a = chips[list[0].from], y1 = a.y + a.h / 2;
        const px = a.x + a.w, firstKind = list[0].kind, pair = a.colIndex + '>' + chips[list[0].to].colIndex;
        const x = chanX[a.colIndex + ':F' + pair + (firstKind === 'fwdlong' ? 'x' : '')];
        const ys = list.map(r => chips[r.to].y + chips[r.to].h / 2);
        const yEnd = firstKind === 'fwdlong' ? bottomY[pair] : Math.max.apply(null, ys);
        const yStart = firstKind === 'fwdlong' ? y1 : Math.min.apply(null, ys);
        const open = !!expanded[key];
        list.forEach(r => { r.collapsed = !open; r.fan = key; });
        fans.push({ key: key, from: list[0].from, col: chips[list[0].to].colIndex, count: list.length, open: open, x: px, y: y1, edges: list.map(r => r.index),
          d: wfOrthoPath([[px, y1], [x, y1], [x, Math.min(yStart, y1)]], K.radius) + ' ' + wfOrthoPath([[x, y1], [x, Math.max(yEnd, y1)]], K.radius) });
      });
      return { edges: recs, fans: fans, common: common };
    }

    // wfEdgeLabelSvg draws a floating label pill at an edge's anchor.
    function wfEdgeLabelSvg(text, lx, ly, cls) {
      const t = wfTrim(text, 18), w = Math.max(16, t.length * 6 + 10);
      return `<g class="${cls}" pointer-events="none"><rect x="${Math.round(lx - w / 2)}" y="${Math.round(ly - 9)}" width="${w}" height="16" rx="8" class="${cls}-bg"/><text x="${Math.round(lx)}" y="${Math.round(ly + 3)}" text-anchor="middle">${esc(t)}</text></g>`;
    }
    // Simulate (RFC phase 3c): the facts form is generated from this table
    // so the HTML builder, the form reader and the node tests share one
    // source of truth. `path` is the dotted key inside the simulate facts
    // DTO; `kind` restricts a field to issues or PRs; enumerations mirror
    // the stageengine name helpers the API parses.
    const WF_SIM_KINDS = ['issue', 'pr'];
    const WF_SIM_FIELDS = [
      { path: 'labels', label: 'Labels', type: 'list', hint: 'space or comma separated' },
      { path: 'open', label: 'Open', type: 'bool' },
      { path: 'merged', label: 'Merged', type: 'bool', kind: 'pr' },
      { path: 'draft', label: 'Draft', type: 'bool', kind: 'pr' },
      { path: 'author.login', label: 'Author', type: 'text' },
      { path: 'author.kind', label: 'Author kind', type: 'select', options: ['human', 'hive', 'clanker', 'unknown'] },
      { path: 'author.role', label: 'Author role', type: 'select', options: ['', 'trusted', 'contributor', 'newcomer', 'untrusted'] },
      { path: 'author.selfAuthored', label: 'Self-authored (hive\u2019s own PR)', type: 'bool', kind: 'pr' },
      { path: 'issue.bucket', label: 'Issue bucket', type: 'select', kind: 'issue', options: ['', 'actionable', 'claimed', 'in_progress', 'claimed_by_pr', 'filtered', 'reporter_triage'] },
      { path: 'pr.checks', label: 'Checks', type: 'select', kind: 'pr', options: ['unknown', 'green', 'red', 'pending'] },
      { path: 'pr.review', label: 'Review', type: 'select', kind: 'pr', options: ['pending', 'approved', 'changes_requested', 'requires_human', 'unavailable'] },
      { path: 'pr.dco', label: 'DCO', type: 'select', kind: 'pr', options: ['unknown', 'yes', 'no'] },
      { path: 'pr.conflicting', label: 'Merge conflict', type: 'bool', kind: 'pr' },
      { path: 'park.holds', label: 'Hold labels', type: 'list', hint: 'blank = derived from labels' },
      { path: 'park.needsHuman', label: 'Needs human', type: 'bool' },
    ];
    function wfSimDefaultFacts() {
      return { labels: [], open: true, merged: false, draft: false, origin: '',
        author: { login: '', kind: 'human', selfAuthored: false, role: '' },
        park: { holds: null, needsHuman: false },
        issue: { bucket: '' },
        pr: { checks: 'unknown', review: 'pending', dco: 'unknown', conflicting: false } };
    }
    function wfSimDefaultState() { return { kind: 'pr', repo: '', facts: wfSimDefaultFacts(), result: null, error: '', busy: false, open: false }; }
    const _wfLib = { view: 'library', selected: null, detail: null, detailError: '', loadingKey: '', pending: null, enabledBusy: false,
      ingest: { repo: '', name: '', busy: false, result: null, error: '' },
      sim: wfSimDefaultState() };

    function wfProvenanceOf(entry) {
      if (!entry) return 'local';
      if (entry.readOnly || entry.name === 'hive-default') return 'built-in';
      return entry.provenance || 'local';
    }

    function wfProvenanceBadge(entry) {
      const p = wfProvenanceOf(entry);
      const label = WF_PROVENANCE_LABELS[p] || p;
      const tips = {
        'built-in': 'Generated from the engine\'s stage table; read-only.',
        'curated': 'Reviewed and vouched for by a human.',
        'local': 'Saved on this hive by an operator.',
        'ingested-unreviewed': 'Derived from a repository\'s pipeline files; nobody has reviewed it yet. Inferred states and events compile with warnings.',
      };
      return `<span class="wf-prov wf-prov-${esc(p)}" title="${esc(tips[p] || '')}">${esc(label)}</span>`;
    }

    function wfCompiledBadge(entry) {
      const diags = Array.isArray(entry.diagnostics) ? entry.diagnostics : [];
      const warnings = diags.filter(d => d.severity !== 'error').length;
      if (entry.compiled === false) {
        return `<span class="wf-compiled wf-compiled-no" title="${esc(diags.map(d => d.code + ': ' + d.message).join('\n') || 'Does not compile')}">does not compile</span>`;
      }
      if (warnings) return `<span class="wf-compiled wf-compiled-warn" title="${esc(diags.map(d => d.code + ': ' + d.message).join('\n'))}">compiles · ${warnings} warning${warnings === 1 ? '' : 's'}</span>`;
      return '<span class="wf-compiled wf-compiled-ok">compiles</span>';
    }

    // Repositories bound to name@version, from the bindings the library
    // call returned.
    function wfBoundRepos(bindings, name, version) {
      return (Array.isArray(bindings) ? bindings : []).filter(b => b && b.name === name && b.version === version).map(b => b.repo);
    }

    function wfLibNav(view) {
      const items = [['library', 'Library'], ['ingest', 'Ingest'], ['editor', 'Editor'], ['marketplace', 'Marketplace']];
      return '<div class="wf-nav" role="tablist">' + items.map(([id, label]) =>
        `<button type="button" role="tab" class="wf-nav-item${view === id || (id === 'library' && view === 'detail') ? ' active' : ''}" aria-selected="${view === id ? 'true' : 'false'}" data-action="wfLibShow" data-arg0="${id}">${label}</button>`
      ).join('') + '</div>';
    }

    function renderWorkflowLibraryRows(workflows, bindings, owner) {
      const list = Array.isArray(workflows) ? workflows.slice() : [];
      list.sort((a, b) => {
        const pa = wfProvenanceOf(a) === 'built-in' ? 0 : 1, pb = wfProvenanceOf(b) === 'built-in' ? 0 : 1;
        if (pa !== pb) return pa - pb;
        return (a.name + '@' + a.version).localeCompare(b.name + '@' + b.version);
      });
      if (!list.length) return '<tr><td colspan="7" class="text-muted">No workflow models stored yet. Ingest one from a repository, or wait for hive-default to register.</td></tr>';
      return list.map(e => {
        const key = e.name + '@' + e.version;
        const bound = wfBoundRepos(bindings, e.name, e.version);
        const builtin = wfProvenanceOf(e) === 'built-in';
        const canDelete = owner && !builtin && !e.readOnly && !bound.length;
        const actions = [
          `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">View</button>`,
          owner ? (builtin ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdCopyOf" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" title="Open a copy in the editor under a new name.">Edit a copy</button>`
            : `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" title="Open in the visual editor.">Edit</button>`) : '',
          owner ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibDuplicate" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" title="Save a copy under a new name as version 1 (provenance: local).">Duplicate</button>` : '',
          `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibExport" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" title="Show the model as JSON to copy.">Export</button>`,
          typeof wfMktPublishButton === 'function' ? wfMktPublishButton(e, owner) : '',
          canDelete ? `<button type="button" class="hv-btn btn-danger btn-sm" data-action="wfLibDelete" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Delete</button>`
            : (owner && !builtin && bound.length ? `<span class="rt-inherit" title="Unbind it on Settings → Repos first.">bound</span>` : ''),
        ].join('');
        return `<tr data-wf-entry="${esc(key)}">` +
          `<td><a href="#" class="wf-key mono" data-action="wfLibOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" data-prevent="1">${esc(key)}</a>${e.description ? `<div class="wf-desc text-muted">${esc(e.description)}</div>` : ''}</td>` +
          `<td>${wfProvenanceBadge(e)}</td>` +
          `<td>${esc(e.source || '')}</td>` +
          `<td>${wfCompiledBadge(e)}</td>` +
          `<td class="wf-num">${esc(String(e.states || 0))} / ${esc(String(e.columns || 0))}</td>` +
          `<td>${bound.length ? `<span title="${esc(bound.join('\n'))}">${bound.length} repo${bound.length === 1 ? '' : 's'}</span>` : '<span class="text-muted">—</span>'}</td>` +
          `<td class="kc-actions">${actions}</td></tr>`;
      }).join('');
    }

    // renderWorkflowEnabledToggle is the Library header's owner switch for
    // workflow.enabled (PUT /api/workflows/enabled). Pure: enabled is the
    // live flag from GET /api/workflows, owner gates the control, busy dims
    // it while a PUT is in flight.
    function renderWorkflowEnabledToggle(enabled, owner, busy) {
      const on = !!enabled;
      const locked = !owner || !!busy;
      const explain = on
        ? 'On: a bound repository\'s stages are derived by its model\'s rules and the model\'s labels are provisioned in the repository.'
        : 'Off: a binding only changes the Activity Monitor\'s columns; stages are still derived by the built-in engine.';
      return '<div class="config-toggle wf-enabled-toggle" data-wf-enabled="' + (on ? 'on' : 'off') + '">' +
        '<div class="config-toggle-switch' + (on ? ' on' : '') + (locked ? ' disabled' : '') + '" role="switch" aria-checked="' + (on ? 'true' : 'false') + '" ' +
        (locked ? 'aria-disabled="true" ' : 'data-action="wfToggleEnabled" data-arg-types="t" ') +
        'title="' + (owner ? 'Turn model-based stage derivation ' + (on ? 'off' : 'on') : 'Owner role is required to change workflow.enabled') + '"></div>' +
        '<span class="config-toggle-label">Derive stages from bound models <span class="text-muted">(' + yamlHint('workflow.enabled') + ')</span></span>' +
        '</div>' +
        '<p class="text-muted fs-sm wf-enabled-explain">' + explain + '</p>';
    }

    function renderWorkflowLibraryTable(workflows, bindings, owner, enabled, busy) {
      return '<div class="config-section" data-section="workflows-library">' +
        '<h3>Workflow library</h3>' +
        renderWorkflowEnabledToggle(enabled, owner, busy) +
        '<p class="text-muted fs-base">Every workflow model this hive knows: the built-in <span class="mono">hive-default</span> lifecycle, models saved here, and drafts ingested from repository pipelines. A repository follows a model only once it is bound on Settings → Repos; binding changes the Activity Monitor\'s columns for that repository and, when the switch above is on, how its stages are derived.</p>' +
        '<table class="kc-table wf-table"><thead><tr><th>Model</th><th>Provenance</th><th>Source</th><th>Compile</th><th title="states / columns">S / C</th><th>Bound</th><th></th></tr></thead>' +
        '<tbody>' + renderWorkflowLibraryRows(workflows, bindings, owner) + '</tbody></table>' +
        '</div>';
    }

    function wfTrim(label, max) {
      const s = String(label || '');
      return s.length > max ? s.slice(0, max - 1) + '…' : s;
    }

    // Column/state diagram as inline SVG, laid out from the compiled
    // profile's columns (display order) and the model's transitions. Pure.
    function workflowDiagramSvg(model, profile) {
      const spec = (model && model.spec) || {};
      const cols = (profile && Array.isArray(profile.columns) && profile.columns.length) ? profile.columns : (spec.columns || []);
      if (!cols.length) return '<p class="text-muted">This model declares no columns, so there is nothing to draw.</p>';
      const D = WF_DIAGRAM;
      const states = {};
      (function walk(list) { (list || []).forEach(s => { states[s.id] = s; walk(s.regions); }); })(spec.states);
      const colOf = {};
      cols.forEach((c, ci) => (Array.isArray(c.states) ? c.states : []).forEach(id => { if (colOf[id] == null) colOf[id] = ci; }));
      const transitions = Array.isArray(spec.transitions) ? spec.transitions : [];
      const channels = wfEdgeChannels(transitions, id => (colOf[id] == null ? null : colOf[id]));
      const topCorr = wfEdgeCorridor(channels.top), bottomCorr = wfEdgeCorridor(channels.bottom);
      const laneY = D.pad + topCorr;
      let maxRows = 1;
      cols.forEach(c => { maxRows = Math.max(maxRows, (Array.isArray(c.states) ? c.states : []).length); });
      const laneH = D.headH + maxRows * (D.chipH + D.chipGap);
      const geomCols = [], chips = {};
      cols.forEach((c, ci) => {
        const x = D.pad + ci * (D.colW + D.colGap);
        geomCols.push({ x: x, y: laneY, w: D.colW, h: laneH });
        (Array.isArray(c.states) ? c.states : []).forEach((id, ri) => {
          if (chips[id]) return;
          chips[id] = { id: id, x: x + 8, y: laneY + D.headH + ri * (D.chipH + D.chipGap), w: D.colW - 16, h: D.chipH, colIndex: ci, index: ri };
        });
      });
      const width = D.pad * 2 + cols.length * D.colW + (cols.length - 1) * D.colGap;
      const height = D.pad * 2 + topCorr + laneH + bottomCorr;
      const out = [`<svg class="wf-diagram" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="Workflow columns and states" data-wf-focus-root="1">`];
      out.push('<defs><marker id="wf-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M0 0 L8 4 L0 8 z" class="wf-dg-arrowhead" fill="context-stroke"/></marker></defs>');
      cols.forEach((c, ci) => {
        const g = geomCols[ci];
        out.push(`<g class="wf-dg-col${c.terminal ? ' terminal' : ''}"><rect x="${g.x}" y="${g.y}" width="${g.w}" height="${g.h}" rx="6" class="wf-dg-lane"/>` +
          `<text x="${g.x + D.colW / 2}" y="${g.y + D.headH / 2 + 4}" text-anchor="middle" class="wf-dg-colname">${esc(wfTrim(c.label || c.id, D.maxLabel))}</text></g>`);
      });
      const routed = wfRouteEdges(transitions, { cols: geomCols, chips: chips, gap: D.colGap, pad: D.pad }, { dedupe: true });
      const edgeSvg = e => {
        const title = esc(e.from + ' \u2192 ' + e.to + (e.on ? ' on ' + e.on : '') + (e.note ? ' \u2014 ' + e.note : ''));
        return `<g class="wf-dg-edge${e.back ? ' back' : ''}${e.collapsed ? ' wf-fan-edge' : ''}" data-wf-from="${esc(e.from)}" data-wf-to="${esc(e.to)}"><path d="${e.d}" class="wf-dg-edge-hit"/><path d="${e.d}" class="wf-dg-edge-line" marker-end="url(#wf-arrow)"/>` +
          (e.on ? wfEdgeLabelSvg(e.on, e.lx, e.ly, 'wf-dg-edge-label') : '') + `<title>${title}</title></g>`;
      };
      const fanned = {};
      routed.fans.forEach(f => { f.edges.forEach(i => { fanned[i] = f.key; }); });
      out.push('<g class="wf-edge-layer">');
      routed.edges.forEach(e => { if (e.self || fanned[e.index]) return; out.push(edgeSvg(e)); });
      routed.fans.forEach(f => {
        out.push(`<g class="wf-fan" data-wf-fan="${esc(f.key)}" data-wf-from="${esc(f.from)}"><path d="${f.d}" class="wf-dg-edge-line wf-fan-trunk"/>` +
          routed.edges.filter(e => fanned[e.index] === f.key).map(edgeSvg).join('') +
          `<g class="wf-fan-badge"><rect x="${f.x + 4}" y="${f.y - 8}" width="26" height="16" rx="8"/><text x="${f.x + 17}" y="${f.y + 4}" text-anchor="middle">+${f.count}</text><title>${f.count} transitions into one lane; hover to expand</title></g></g>`);
      });
      out.push('</g>');
      cols.forEach(c => (c.states || []).forEach(id => {
        const p = chips[id], s = states[id] || { id: id, label: id };
        if (!p || p.id !== id) return;
        const lc = s.lifecycle || {};
        const inferred = lc.confidence === 'inferred';
        const num = lc.number ? String(lc.number).padStart(3, '0') : '';
        const cls = 'wf-dg-state' + (inferred ? ' inferred' : '') + (spec.initial === id ? ' initial' : '') + (profile && profile.terminal && profile.terminal[id] ? ' terminal' : '');
        const tip = esc(s.label || id) + (num ? ' · stage/' + num : '') + (inferred ? ' · inferred' : '') + (lc.column ? '\ncolumn ' + esc(lc.column) : '');
        out.push(`<g class="${cls}" data-state="${esc(id)}" data-wf-state="${esc(id)}"><title>${tip}</title>` +
          `<rect x="${p.x}" y="${p.y}" width="${p.w}" height="${p.h}" rx="5" class="wf-dg-chip"/>` +
          (num ? `<text x="${p.x + D.textPad - 4}" y="${p.y + D.chipH / 2 + 4}" class="wf-dg-num mono">${num}</text>` : '') +
          `<text x="${p.x + D.textPad + (num ? 26 : -4)}" y="${p.y + D.chipH / 2 + 4}" class="wf-dg-label">${esc(wfTrim(s.label || id, num ? D.maxLabel - 5 : D.maxLabel))}</text></g>`);
      }));
      out.push('</svg>');
      return out.join('');
    }

    // wfEdgeFocusHover implements detail-on-focus for both the library diagram
    // and the editor canvas: hovering a state dims every edge not incident to
    // it, hovering an edge highlights it and both end chips. Focused edges are
    // moved to a front layer so they draw over the chips. Pure DOM, no state.
    function wfEdgeFocusHover(ev) {
      const t = ev.target && ev.target.closest ? ev.target.closest('[data-wf-state],[data-wf-from]') : null;
      const root = t ? t.closest('[data-wf-focus-root]') : (ev.relatedTarget && ev.relatedTarget.closest ? ev.relatedTarget.closest('[data-wf-focus-root]') : null);
      if (!root) return;
      wfEdgeFocusClear(root);
      if (!t || ev.type === 'pointerout') return;
      const edges = root.querySelectorAll('[data-wf-from]');
      let states = [];
      if (t.hasAttribute('data-wf-state')) {
        const id = t.getAttribute('data-wf-state');
        edges.forEach(e => { if (e.getAttribute('data-wf-from') === id || e.getAttribute('data-wf-to') === id) { e.classList.add('focus'); states.push(e.getAttribute('data-wf-from'), e.getAttribute('data-wf-to')); } });
      } else {
        t.classList.add('focus');
        states = [t.getAttribute('data-wf-from'), t.getAttribute('data-wf-to')];
      }
      states.forEach(id => { if (id == null) return; const c = root.querySelector('[data-wf-state="' + id.replace(/"/g, '') + '"]'); if (c) c.classList.add('focus'); });
      root.classList.add('wf-focus');
      const layer = root.querySelector('.wf-edge-layer');
      if (!layer) return;
      let front = layer.parentNode.querySelector(':scope > .wf-edge-front');
      if (!front) { front = document.createElementNS('http://www.w3.org/2000/svg', 'g'); front.setAttribute('class', 'wf-edge-front'); layer.parentNode.appendChild(front); }
      layer.querySelectorAll(':scope > [data-wf-from].focus').forEach(e => front.appendChild(e));
    }
    function wfEdgeFocusClear(root) {
      root.classList.remove('wf-focus');
      const layer = root.querySelector('.wf-edge-layer');
      root.querySelectorAll('.focus').forEach(e => e.classList.remove('focus'));
      if (layer) root.querySelectorAll('.wf-edge-front > [data-wf-from]').forEach(e => layer.appendChild(e));
    }
    if (typeof document !== 'undefined' && document.addEventListener) {
      document.addEventListener('pointerover', wfEdgeFocusHover);
      document.addEventListener('pointerout', wfEdgeFocusHover);
    }

    function renderWorkflowDiagnostics(diags) {
      const list = Array.isArray(diags) ? diags : [];
      if (!list.length) return '<p class="text-muted fs-sm">No diagnostics: the model compiles clean.</p>';
      return '<ul class="wf-diags">' + list.map(d =>
        `<li class="wf-diag wf-diag-${esc(d.severity || 'warning')}"><span class="wf-diag-code mono">${esc(d.code || '')}</span> ${esc(d.message || '')}${d.path ? ` <span class="text-muted mono">${esc(d.path)}</span>` : ''}</li>`
      ).join('') + '</ul>';
    }

    function renderWorkflowDetail(detail, bindings, owner, sim) {
      const e = detail.entry || {}, m = detail.model || {}, p = detail.profile || null;
      if (sim === undefined) sim = _wfLib.sim;
      const key = e.name + '@' + e.version;
      const bound = wfBoundRepos(bindings, e.name, e.version);
      const meta = (m.spec && m.spec.meta) || {};
      const sources = (m.spec && Array.isArray(m.spec.sources)) ? m.spec.sources : [];
      const citations = [];
      (function walk(list) { (list || []).forEach(s => { ((s.lifecycle || {}).citations || []).forEach(c => citations.push(c)); walk(s.regions); }); })(m.spec && m.spec.states);
      const builtin = wfProvenanceOf(e) === 'built-in';
      const facts = [
        ['Provenance', wfProvenanceBadge(e)],
        ['Source', esc(e.source || meta.source || '—')],
        ['Derivation', esc((m.spec && m.spec.derivation && m.spec.derivation.mode) || (p && p.mode) || '—')],
        ['Compile', wfCompiledBadge(e)],
        ['States / columns', esc(String(e.states || 0)) + ' / ' + esc(String(e.columns || 0))],
        ['Hash', `<span class="mono" title="${esc(e.hash || '')}">${esc(String(e.hash || '').slice(0, 12))}</span>`],
        ['Saved', e.savedAt && e.savedAt !== '0001-01-01T00:00:00Z' ? esc(formatDashboardDateTime(e.savedAt)) + (e.savedBy ? ' by ' + esc(e.savedBy) : '') : '—'],
        ['Bound repositories', bound.length ? bound.map(r => `<span class="mono">${esc(r)}</span>`).join(', ') : '<span class="text-muted">none — bind on Settings → Repos</span>'],
      ];
      if (sources.length) facts.push(['Ingested from', sources.map(s => `<span class="mono">${esc(s.repo || s.ref || s.path || JSON.stringify(s))}</span>`).join(', ')]);
      const actions = [
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibShow" data-arg0="library">← Library</button>',
        owner ? (builtin ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdCopyOf" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Edit a copy</button>`
          : `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfEdOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Edit</button>`) : '',
        owner ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibDuplicate" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Duplicate</button>` : '',
        `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibExport" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Export JSON</button>`,
        typeof wfMktPublishButton === 'function' ? wfMktPublishButton(e, owner) : '',
        owner && !builtin && !bound.length ? `<button type="button" class="hv-btn btn-danger btn-sm" data-action="wfLibDelete" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Delete</button>` : '',
      ].join('');
      return '<div class="config-section wf-detail" data-section="workflows-detail" data-wf-key="' + esc(key) + '">' +
        `<div class="wf-detail-head"><h3 class="mono">${esc(key)}</h3><div class="kc-actions">${actions}</div></div>` +
        (e.description ? `<p class="fs-base">${esc(e.description)}</p>` : '') +
        '<dl class="wf-facts">' + facts.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('') + '</dl>' +
        '<h4>Columns and states</h4>' +
        '<p class="text-muted fs-sm">Left to right as the Activity Monitor draws them. Arrows are the model\'s transitions; hover one for its event. Dashed states were inferred by ingest and have not been confirmed by a human.</p>' +
        '<div class="wf-diagram-wrap">' + wfSimMarkDiagram(workflowDiagramSvg(m, p), sim && sim.result) + '</div>' +
        renderWorkflowSimulate(sim, owner) +
        '<h4>Diagnostics</h4>' + renderWorkflowDiagnostics(e.diagnostics || (p && p.diagnostics)) +
        (citations.length ? '<h4>Citations</h4><ul class="wf-citations">' + citations.slice(0, 40).map(c => `<li class="mono">${esc(c.path || '')}${c.line ? ':' + esc(String(c.line)) : ''}${c.note ? ` <span class="text-muted">${esc(c.note)}</span>` : ''}</li>`).join('') + (citations.length > 40 ? `<li class="text-muted">… ${citations.length - 40} more</li>` : '') + '</ul>' : '') +
        '<details class="wf-json"><summary>Model JSON</summary><pre class="mono">' + esc(JSON.stringify(m, null, 2)) + '</pre></details>' +
        '</div>';
    }

    // --- Simulate -----------------------------------------------------------

    function wfSimGet(obj, path) {
      return path.split('.').reduce((o, k) => (o && o[k] !== undefined && o[k] !== null) ? o[k] : undefined, obj);
    }

    function wfSimSetPath(obj, path, value) {
      const keys = path.split('.');
      let cur = obj;
      keys.slice(0, -1).forEach(k => { if (!cur[k] || typeof cur[k] !== 'object') cur[k] = {}; cur = cur[k]; });
      cur[keys[keys.length - 1]] = value;
      return obj;
    }

    function wfSimSplitList(raw) {
      return String(raw || '').split(/[\s,]+/).map(s => s.trim()).filter(Boolean);
    }

    // wfSimFieldsFor returns the form fields that apply to a kind.
    function wfSimFieldsFor(kind) {
      return WF_SIM_FIELDS.filter(f => !f.kind || f.kind === kind);
    }

    // wfSimFormFromFacts maps facts to the raw form value of one field:
    // strings for text/select, booleans for bool, a joined string for lists.
    function wfSimFormFromFacts(facts, field) {
      const v = wfSimGet(facts || {}, field.path);
      if (field.type === 'bool') return !!v;
      if (field.type === 'list') return Array.isArray(v) ? v.join(' ') : String(v || '');
      return v === undefined ? (field.options ? field.options[0] : '') : String(v);
    }

    // wfSimFactsFromForm folds raw form values back into a facts object.
    // `read(field)` returns the raw value for a field (DOM value / checked
    // in the page, a plain lookup in tests); fields it returns undefined
    // for keep their current value.
    function wfSimFactsFromForm(facts, kind, read) {
      const out = JSON.parse(JSON.stringify(facts || wfSimDefaultFacts()));
      wfSimFieldsFor(kind).forEach(f => {
        const raw = read(f);
        if (raw === undefined) return;
        let v;
        if (f.type === 'bool') v = !!raw;
        else if (f.type === 'list') v = wfSimSplitList(raw);
        else v = String(raw == null ? '' : raw).trim();
        if (f.path === 'park.holds' && v.length === 0) v = null;
        wfSimSetPath(out, f.path, v);
      });
      return out;
    }

    function wfSimParseItem(raw) {
      const m = /^\s*(?:https?:\/\/[^/]+\/)?([^/\s#]+)\/([^/\s#]+?)(?:\/(?:issues|pull)\/|#)(\d+)\s*$/.exec(String(raw || ''));
      return m ? { repo: m[1] + '/' + m[2], number: parseInt(m[3], 10) } : null;
    }

    function wfSimFieldHtml(field, facts, disabled) {
      const id = 'wf-sim-' + field.path.replace(/\./g, '-');
      const val = wfSimFormFromFacts(facts, field);
      const common = `id="${id}" data-sim-field="${esc(field.path)}" ${disabled}`;
      let input;
      if (field.type === 'bool') {
        input = `<input type="checkbox" ${common} ${val ? 'checked' : ''} data-change-action="wfSimSet" data-arg0="${esc(field.path)}" data-arg-types="s,c">`;
        return `<label class="config-field wf-sim-check">${input} ${esc(field.label)}</label>`;
      }
      if (field.type === 'select') {
        input = `<select ${common} data-change-action="wfSimSet" data-arg0="${esc(field.path)}" data-arg-types="s,v">` +
          field.options.map(o => `<option value="${esc(o)}" ${o === val ? 'selected' : ''}>${esc(o || '—')}</option>`).join('') + '</select>';
      } else {
        input = `<input type="text" ${common} value="${esc(val)}" placeholder="${esc(field.hint || '')}" data-change-action="wfSimSet" data-arg0="${esc(field.path)}" data-arg-types="s,v">`;
      }
      return `<label class="config-field">${esc(field.label)}${input}</label>`;
    }

    function renderWorkflowSimulate(state, owner) {
      const st = state || _wfLib.sim;
      const kind = WF_SIM_KINDS.indexOf(st.kind) >= 0 ? st.kind : 'pr';
      const disabled = st.busy ? 'disabled' : '';
      const kinds = WF_SIM_KINDS.map(k =>
        `<label class="wf-sim-kind"><input type="radio" name="wf-sim-kind" value="${k}" ${k === kind ? 'checked' : ''} ${disabled} data-change-action="wfSimSet" data-arg0="kind" data-arg-types="s,v"> ${k === 'pr' ? 'Pull request' : 'Issue'}</label>`).join('');
      const fields = wfSimFieldsFor(kind).map(f => wfSimFieldHtml(f, st.facts, disabled)).join('');
      return '<details class="config-section wf-sim" data-section="workflows-simulate" ' + (st.open || st.result || st.error ? 'open' : '') + '>' +
        '<summary><h4>Simulate</h4></summary>' +
        '<p class="text-muted fs-sm">Paste a real issue or PR, or describe one, and see which state this model puts it in and why — the same facts the engine would use. This is the confidence check before binding; nothing is written.</p>' +
        '<div class="wf-sim-pick">' +
        `<label class="config-field">Pick a real item<input type="text" id="wf-sim-repo" value="${esc(st.repo)}" placeholder="owner/repo#123" ${disabled} data-input-action="wfSimSet" data-arg0="repo" data-arg-types="s,v"></label>` +
        `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfSimFetch" ${disabled}>${st.busy ? 'Working…' : 'Fetch facts'}</button>` +
        '</div>' +
        '<div class="wf-sim-kinds">' + kinds + '</div>' +
        '<div class="wf-sim-form">' + fields + '</div>' +
        `<div class="kc-actions"><button type="button" class="hv-btn btn-primary btn-sm" data-action="wfSimRun" ${disabled}>${st.busy ? 'Working…' : 'Run'}</button></div>` +
        (st.error ? `<p class="wf-ingest-error">${esc(st.error)}</p>` : '') +
        (st.result ? renderWorkflowSimulateResult(st.result) : '') +
        '</details>';
    }

    function wfSimVerdict(p) {
      if (!p.known) return '<span class="wf-sim-unknown" title="fact unknown">?</span>';
      return p.holds ? '<span class="wf-sim-ok">✓</span>' : '<span class="wf-sim-fail">✗</span>';
    }

    function renderWorkflowSimulateResult(result) {
      const r = result || {}, d = r.decision || {};
      const stage = d.stage ? 'stage/' + String(d.stage).padStart(3, '0') : '';
      let chip;
      if (d.abstain && !d.covered) chip = `<span class="wf-sim-chip wf-sim-chip-abstain">Not covered</span> <span class="text-muted">${esc(d.reason || '')}</span>`;
      else if (d.abstain) chip = `<span class="wf-sim-chip wf-sim-chip-abstain">Abstain</span> <span class="text-muted">${esc(d.reason || '')}</span>`;
      else if (!d.state) chip = `<span class="wf-sim-chip wf-sim-chip-none">No stage</span> <span class="text-muted">${esc(d.reason || '')}</span>`;
      else chip = `<span class="wf-sim-chip wf-sim-chip-state mono">${esc(d.state)}</span>` +
        (d.column ? ` <span class="wf-sim-sep">·</span> ${esc(d.column)}` : '') +
        (stage ? ` <span class="wf-sim-sep">·</span> <span class="mono">${esc(stage)}</span>` : '') +
        (d.reason ? ` <span class="wf-sim-sep">·</span> <span class="text-muted">${esc(d.reason)}</span>` : '');
      const trace = Array.isArray(r.trace) ? r.trace : [];
      const items = trace.map(t => {
        const cls = t.matched ? 'matched' : (t.skipped ? 'skipped' : (t.evaluated ? 'missed' : 'unevaluated'));
        const head = t.builtin
          ? `<span class="mono">${esc(t.state || '—')}</span> <span class="text-muted">built-in deriver${t.reason ? ' · ' + esc(t.reason) : ''}</span>`
          : `<span class="mono">${esc(t.id || '')}</span> → <span class="mono">${esc(t.state || '')}</span>` +
            (t.skipped ? ` <span class="text-muted">skipped: ${esc(t.skipped)}</span>` : '') +
            (!t.evaluated && !t.skipped ? ' <span class="text-muted">not evaluated</span>' : '');
        const preds = (t.predicates || []).map(p =>
          `<li class="mono"><span class="wf-sim-pred">${esc(p.fact)} ${esc(p.op)} ${esc(p.value || '')}</span> → ${wfSimVerdict(p)}` +
          (p.actual ? ` <span class="text-muted">(${esc(p.actual)})</span>` : '') + '</li>').join('');
        return `<li class="wf-sim-rule ${cls}" data-rule="${esc(t.id || '')}">${head}${preds ? '<ul class="wf-sim-preds">' + preds + '</ul>' : ''}</li>`;
      }).join('');
      const fill = [];
      if (Array.isArray(r.filled) && r.filled.length) fill.push('<span class="text-muted">fetched:</span> ' + r.filled.map(esc).join(', '));
      if (Array.isArray(r.unknown) && r.unknown.length) fill.push('<span class="text-muted">unknown, defaulted:</span> ' + r.unknown.map(esc).join(', '));
      (r.notes || []).forEach(n => fill.push(esc(n)));
      return '<div class="wf-sim-result" data-sim-state="' + esc(d.state || '') + '">' +
        '<div class="wf-sim-decision">' + chip + '</div>' +
        (items ? '<ol class="wf-sim-trace">' + items + '</ol>' : '') +
        (fill.length ? '<p class="fs-sm wf-sim-fill">' + fill.join('<br>') + '</p>' : '') +
        '</div>';
    }

    // wfSimMarkDiagram tags the simulated state's chip in the diagram SVG.
    function wfSimMarkDiagram(svg, result) {
      const id = result && result.decision && result.decision.state;
      if (!id) return svg;
      const marker = 'data-state="' + esc(id) + '"';
      return svg.split(marker).map((part, i, all) => {
        if (i === all.length - 1) return part;
        return part.replace(/class="([^"]*)"\s*$/, 'class="$1 simulated" ');
      }).join(marker);
    }

    function wfSimSet(field, value) {
      const st = _wfLib.sim;
      st.open = true;
      if (field === 'kind') { st.kind = WF_SIM_KINDS.indexOf(value) >= 0 ? value : 'pr'; st.result = null; st.error = ''; wfLibRerender(); return; }
      if (field === 'repo') { st.repo = String(value || ''); return; }
      const def = WF_SIM_FIELDS.find(f => f.path === field);
      if (!def) return;
      st.facts = wfSimFactsFromForm(st.facts, st.kind, f => f.path === field ? value : undefined);
    }

    function wfSimReadForm() {
      const st = _wfLib.sim;
      st.open = true;
      const repoEl = document.getElementById('wf-sim-repo');
      if (repoEl) st.repo = repoEl.value.trim();
      st.facts = wfSimFactsFromForm(st.facts, st.kind, f => {
        const el = document.querySelector('[data-sim-field="' + f.path + '"]');
        if (!el) return undefined;
        return f.type === 'bool' ? el.checked : el.value;
      });
    }

    async function wfSimCall(body) {
      const st = _wfLib.sim, e = (_wfLib.detail && _wfLib.detail.entry) || {};
      if (!e.name) { st.error = 'Open a model first.'; wfLibRerender(); return null; }
      st.busy = true; st.error = ''; wfLibRerender();
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(e.name) + '/' + encodeURIComponent(e.version) + '/simulate',
          { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(body) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        return await res.json();
      } catch (err) {
        st.error = 'Simulate failed: ' + err.message;
        st.result = null;
        return null;
      } finally {
        st.busy = false;
      }
    }

    // wfSimFetch fills the facts form from a real item so the operator can
    // tweak them before running; the fetched facts are also run as-is.
    async function wfSimFetch() {
      const st = _wfLib.sim;
      wfSimReadForm();
      const item = wfSimParseItem(st.repo);
      if (!item) { st.error = 'Enter an item as owner/repo#123.'; st.result = null; wfLibRerender(); return; }
      const out = await wfSimCall({ repo: item.repo, number: item.number });
      if (out) {
        if (out.facts) {
          st.facts = Object.assign(wfSimDefaultFacts(), out.facts);
          if (!st.facts.pr) st.facts.pr = wfSimDefaultFacts().pr;
          if (!st.facts.issue) st.facts.issue = wfSimDefaultFacts().issue;
        }
        if (out.facts && out.facts.pr && !out.facts.issue) st.kind = 'pr';
        else if (out.facts && out.facts.issue) st.kind = 'issue';
        st.result = out;
      }
      wfLibRerender();
    }

    async function wfSimRun() {
      const st = _wfLib.sim;
      wfSimReadForm();
      const body = { kind: st.kind, facts: st.facts };
      const item = wfSimParseItem(st.repo);
      if (item) { body.repo = item.repo; body.number = item.number; }
      const out = await wfSimCall(body);
      if (out) st.result = out;
      wfLibRerender();
    }

    function wfRepoChoices() {
      const d = (_configState && _configState.data) || {};
      const org = String(d.org || '').trim();
      const repos = Array.isArray(d.repos) ? d.repos : [];
      return repos.map(r => {
        const name = typeof r === 'string' ? r : (r && (r.name || r.repo)) || '';
        if (!name) return '';
        return name.indexOf('/') >= 0 || !org ? name : org + '/' + name;
      }).filter(Boolean);
    }

    function renderWorkflowIngest(state, owner) {
      const st = state || _wfLib.ingest;
      const choices = wfRepoChoices();
      const opts = ['<option value="">— choose a watched repository —</option>'].concat(choices.map(r => `<option value="${esc(r)}" ${r === st.repo ? 'selected' : ''}>${esc(r)}</option>`)).join('');
      const r = st.result;
      let result = '';
      if (st.error) result = `<div class="wf-ingest-error">${esc(st.error)}</div>`;
      else if (r) {
        const e = r.entry || null;
        const cols = (r.profile && r.profile.columns) || (r.model && r.model.spec && r.model.spec.columns) || [];
        const pipelines = Array.isArray(r.pipelines) ? r.pipelines : [];
        result = '<div class="wf-ingest-result" data-wf-ingest-result="1">' +
          `<div class="wf-ingest-summary">${r.compiled ? '<span class="wf-compiled wf-compiled-ok">compiles</span>' : '<span class="wf-compiled wf-compiled-no">does not compile</span>'}` +
          ` · ${esc(String(r.files || 0))} pipeline file${r.files === 1 ? '' : 's'} · ${pipelines.length} pipeline${pipelines.length === 1 ? '' : 's'} · ${cols.length} column${cols.length === 1 ? '' : 's'}` +
          (r.ref ? ` · <span class="mono">${esc(String(r.ref).slice(0, 12))}</span>` : '') +
          (e ? ` · saved as <a href="#" class="mono" data-action="wfLibOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" data-prevent="1">${esc(e.name + '@' + e.version)}</a>` : '') +
          (owner && r.model ? ` <button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdOpenIngestResult" title="Open this draft on the editor canvas.">Open in editor</button>` : '') +
          '</div>' +
          (pipelines.length ? '<ul class="wf-pipelines">' + pipelines.map(p => `<li><span class="mono">${esc(p.system || p.kind || '')}</span> ${esc(p.name || p.id || '')}${p.path ? ` <span class="text-muted mono">${esc(p.path)}</span>` : ''}</li>`).join('') + '</ul>' : '') +
          '<h4>Diagnostics</h4>' + renderWorkflowDiagnostics(r.diagnostics) +
          '<h4>Proposed columns</h4><div class="wf-diagram-wrap">' + workflowDiagramSvg(r.model, r.profile) + '</div>' +
          '</div>';
      }
      const disabled = !owner || st.busy ? 'disabled' : '';
      return '<div class="config-section" data-section="workflows-ingest">' +
        '<h3>Ingest a repository\'s pipelines</h3>' +
        '<p class="text-muted fs-base">Reads the repository\'s GitHub Actions workflows and Tekton pipelines from its default branch and proposes a lifecycle model: hive-default\'s review flow with the repository\'s PR-verify pipelines bound in, plus post-merge states for its main-branch CI, release and deploy pipelines. Everything inferred is marked <em>inferred</em> and the model is saved with provenance <em>ingested · unreviewed</em>. Nothing is bound until you choose to on Settings → Repos.</p>' +
        (owner ? '' : '<p class="text-muted fs-sm">Owner role is required to ingest.</p>') +
        '<div class="wf-ingest-form">' +
          `<label class="config-field">Repository<select id="wf-ingest-repo-select" ${disabled} data-change-action="wfIngestPickRepo" data-arg-types="v">${opts}</select></label>` +
          `<label class="config-field">or owner/repo<input type="text" id="wf-ingest-repo" value="${esc(st.repo)}" placeholder="cncf/techdocs" ${disabled} data-input-action="wfIngestSetRepo" data-arg-types="v"></label>` +
          `<label class="config-field">Save as<input type="text" id="wf-ingest-name" value="${esc(st.name)}" placeholder="${esc(wfIngestDefaultName(st.repo) || 'derived from the repository')}" ${disabled} data-input-action="wfIngestSetName" data-arg-types="v"></label>` +
          `<div class="kc-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfIngestRun" data-arg0="false" data-arg-types="j" ${disabled}>${st.busy ? 'Working…' : 'Preview'}</button>` +
          `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfIngestRun" data-arg0="true" data-arg-types="j" ${disabled}>Ingest and save</button></div>` +
        '</div>' + result + '</div>' +
        (typeof renderWorkflowIngestSources === 'function' ? renderWorkflowIngestSources(state, owner) : '');
    }

    function wfIngestDefaultName(repo) {
      const r = String(repo || '').trim().toLowerCase();
      if (!r) return '';
      return r.replace(/[^a-z0-9/._-]+/g, '-').replace(/\//g, '-').replace(/^-+|-+$/g, '');
    }

    function renderGovWorkflows() {
      const owner = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      if (!_workflowState.loaded) {
        loadWorkflowLibrary(() => { if (_configState.tab === 'Workflows') renderConfigTab('Workflows'); });
        return wfLibNav(_wfLib.view) + '<div class="config-section"><p class="text-muted">Loading the workflow library…</p></div>';
      }
      if (_wfLib.pending) {
        const p = _wfLib.pending; _wfLib.pending = null;
        wfLibOpen(p.name, p.version);
      }
      let body;
      if (_wfLib.view === 'detail') {
        if (_wfLib.detailError) body = `<div class="config-section"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibShow" data-arg0="library">← Library</button><p class="wf-ingest-error">${esc(_wfLib.detailError)}</p></div>`;
        else if (!_wfLib.detail) body = '<div class="config-section"><p class="text-muted">Loading ' + esc(_wfLib.loadingKey || 'model') + '…</p></div>';
        else body = renderWorkflowDetail(_wfLib.detail, _workflowState.bindings, owner);
      } else if (_wfLib.view === 'ingest') {
        body = renderWorkflowIngest(_wfLib.ingest, owner);
      } else if (_wfLib.view === 'editor') {
        body = renderWorkflowEditor(owner);
      } else if (_wfLib.view === 'marketplace') {
        body = renderWorkflowMarketplaceView(owner);
      } else {
        body = renderWorkflowLibraryTable(_workflowState.workflows, _workflowState.bindings, owner, _workflowState.enabled, _wfLib.enabledBusy);
      }
      return wfLibNav(_wfLib.view) + body;
    }

    function wfLibRerender() { if (_configState && _configState.tab === 'Workflows') renderConfigTab('Workflows'); }

    function wfLibShow(view) {
      if (_wfLib.view === 'editor' && view !== 'editor' && typeof wfEdGuardLeave === 'function' && _wfEd.model && wfEdDirty()) {
        wfEdGuardLeave().then(ok => { if (ok) { _wfEd.model = null; _wfEd.saved = null; wfLibShow(view); } });
        return;
      }
      _wfLib.view = WF_LIB_VIEWS.indexOf(view) >= 0 ? view : 'library';
      if (_wfLib.view !== 'detail') { _wfLib.detail = null; _wfLib.detailError = ''; }
      wfLibRerender();
    }

    async function wfLibOpen(name, version) {
      const key = name + '@' + version;
      _wfLib.view = 'detail'; _wfLib.detail = null; _wfLib.detailError = ''; _wfLib.loadingKey = key; _wfLib.selected = key;
      _wfLib.sim.result = null; _wfLib.sim.error = '';
      wfLibRerender();
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version), { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        if (_wfLib.loadingKey !== key) return;
        _wfLib.detail = data;
      } catch (err) {
        if (_wfLib.loadingKey !== key) return;
        _wfLib.detailError = 'Could not load ' + key + ': ' + err.message;
      }
      wfLibRerender();
    }

    // Opens Settings → Workflows on a model; the Activity Monitor badge and
    // the Repos binding row deep-link here.
    function openWorkflowLibraryEntry(name, version) {
      _wfLib.pending = { name: name, version: version };
      if (typeof _configModalOpen !== 'undefined' && _configModalOpen && _configState.type === 'governor') {
        switchConfigTab('Workflows');
        return;
      }
      openConfigDialog('governor', null, 'Workflows');
    }

    // wfToggleEnabled flips workflow.enabled through PUT /api/workflows/enabled.
    // Turning it on changes how every bound repository's stages are derived
    // and creates labels in those repositories, so it asks first; turning it
    // off is immediate (columns stay, derivation reverts to the built-in
    // engine).
    async function wfToggleEnabled(el) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to change workflow.enabled.', 'error'); return; }
      if (_wfLib.enabledBusy) return;
      const next = !_workflowState.enabled;
      if (next) {
        const bound = (_workflowState.bindings || []).length;
        const ok = await hiveConfirm({
          title: 'Derive stages from bound models?',
          message: 'Stages for ' + (bound ? bound + ' bound repositor' + (bound === 1 ? 'y' : 'ies') : 'any repository you bind') + ' will be derived by the bound model\'s rules instead of the built-in engine, and each model\'s labels are created in the repository now. Unbound repositories are unaffected. You can turn this off again at any time.',
          confirmLabel: 'Turn on',
        });
        if (!ok) return;
      }
      _wfLib.enabledBusy = true;
      wfLibRerender();
      try {
        const res = await fetch('/api/workflows/enabled', { method: 'PUT', headers: _inceptionAuthHeaders(), body: JSON.stringify({ enabled: next }) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        _workflowState.enabled = !!data.enabled;
        if (Array.isArray(data.bindings)) _workflowState.bindings = data.bindings;
        if (Array.isArray(data.warnings) && data.warnings.length) {
          showToast('Derivation is on, but some labels could not be provisioned yet (the next sweep retries): ' + data.warnings.join('; '), 'error');
        } else {
          showToast(_workflowState.enabled ? 'Stages are now derived from bound models.' : 'Stage derivation is back on the built-in engine; bound columns stay.');
        }
      } catch (err) {
        showToast('Could not change workflow.enabled: ' + err.message, 'error');
      } finally {
        _wfLib.enabledBusy = false;
        wfLibRerender();
      }
    }

    async function wfLibReload() {
      _workflowState.loaded = false;
      await loadWorkflowLibrary();
      wfLibRerender();
    }

    async function wfLibDuplicate(name, version) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to save a workflow.', 'error'); return; }
      const newName = await hivePrompt({ title: 'Duplicate ' + name + '@' + version, message: 'Name for the copy (lower-case, digits, dots, dashes). It is saved as version 1 with provenance local; the original is untouched.', defaultValue: name === 'hive-default' ? 'my-lifecycle' : name + '-copy', confirmLabel: 'Duplicate' });
      if (!newName) return;
      const target = String(newName).trim().toLowerCase();
      if (!/^[a-z0-9][a-z0-9._-]*$/.test(target)) { showToast('Name must be lower-case letters, digits, dots, dashes or underscores.', 'error'); return; }
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version), { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        const m = data.model || {};
        m.metadata = Object.assign({}, m.metadata, { name: target });
        m.spec = Object.assign({}, m.spec);
        m.spec.meta = Object.assign({}, m.spec.meta, { version: '1', provenance: 'local', description: (m.spec.meta && m.spec.meta.description ? m.spec.meta.description + ' ' : '') + '(copy of ' + name + '@' + version + ')' });
        const save = await fetch('/api/workflows', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(m) });
        if (!save.ok) throw new Error(await saveErrorMessage(save));
        showToast('Saved ' + target + '@1');
        await wfLibReload();
        wfLibOpen(target, '1');
      } catch (err) {
        showToast('Duplicate failed: ' + err.message, 'error');
      }
    }

    async function wfLibDelete(name, version) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to delete a workflow.', 'error'); return; }
      const ok = await hiveConfirm({ title: 'Delete ' + name + '@' + version + '?', message: 'The model file is removed from this hive. Nothing happens to repositories: a bound model cannot be deleted, so none follows it.', confirmLabel: 'Delete', danger: true });
      if (!ok) return;
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version), { method: 'DELETE', headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        showToast('Deleted ' + name + '@' + version);
        _wfLib.view = 'library'; _wfLib.detail = null;
        await wfLibReload();
      } catch (err) {
        showToast('Delete failed: ' + err.message, 'error');
      }
    }

    async function wfLibExport(name, version) {
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version), { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        hiveExportViewer({ title: name + '@' + version + '.workflow.json', text: JSON.stringify(data.model || {}, null, 2), mime: 'application/json' });
      } catch (err) {
        showToast('Export failed: ' + err.message, 'error');
      }
    }

    function wfIngestPickRepo(value) { _wfLib.ingest.repo = String(value || ''); _wfLib.ingest.result = null; _wfLib.ingest.error = ''; wfLibRerender(); }
    function wfIngestSetRepo(value) { _wfLib.ingest.repo = String(value || ''); }
    function wfIngestSetName(value) { _wfLib.ingest.name = String(value || ''); }

    async function wfIngestRun(save) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to ingest.', 'error'); return; }
      const st = _wfLib.ingest;
      const repoEl = document.getElementById('wf-ingest-repo'), nameEl = document.getElementById('wf-ingest-name');
      if (repoEl) st.repo = repoEl.value.trim();
      if (nameEl) st.name = nameEl.value.trim();
      if (!/^[^/\s]+\/[^/\s]+$/.test(st.repo)) { st.error = 'Enter a repository as owner/repo.'; st.result = null; wfLibRerender(); return; }
      st.busy = true; st.error = ''; st.result = null; wfLibRerender();
      try {
        const body = { repo: st.repo, save: !!save };
        if (st.name) body.name = st.name;
        const res = await fetch('/api/workflows/ingest', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(body) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        st.result = await res.json();
        if (save && st.result.entry) {
          showToast('Saved ' + st.result.entry.name + '@' + st.result.entry.version);
          _workflowState.loaded = false;
          await loadWorkflowLibrary();
        }
      } catch (err) {
        st.error = 'Ingest failed: ' + err.message;
      } finally {
        st.busy = false;
        wfLibRerender();
      }
    }
