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
    const WF_LIB_VIEWS = ['library', 'detail', 'ingest'];
    const WF_PROVENANCE_LABELS = {
      'built-in': 'built-in',
      'curated': 'curated',
      'local': 'local',
      'ingested-unreviewed': 'ingested · unreviewed',
    };
    const WF_DIAGRAM = { colW: 172, colGap: 28, chipH: 30, chipGap: 10, headH: 30, pad: 14, textPad: 10, maxLabel: 22 };
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
    const _wfLib = { view: 'library', selected: null, detail: null, detailError: '', loadingKey: '', pending: null,
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
      const items = [['library', 'Library'], ['ingest', 'Ingest']];
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
          owner ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibDuplicate" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" title="Save a copy under a new name as version 1 (provenance: local).">Duplicate</button>` : '',
          `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibExport" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" title="Show the model as JSON to copy.">Export</button>`,
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

    function renderWorkflowLibraryTable(workflows, bindings, owner) {
      return '<div class="config-section" data-section="workflows-library">' +
        '<h3>Workflow library</h3>' +
        '<p class="text-muted fs-base">Every workflow model this hive knows: the built-in <span class="mono">hive-default</span> lifecycle, models saved here, and drafts ingested from repository pipelines. A repository follows a model only once it is bound on Settings → Repos; binding changes the Activity Monitor\'s columns for that repository and, when ' + yamlHint('workflow.enabled') + ' is on, how its stages are derived.</p>' +
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
      const pos = {};
      let maxRows = 1;
      cols.forEach((c, ci) => {
        const ids = Array.isArray(c.states) ? c.states : [];
        maxRows = Math.max(maxRows, ids.length);
        ids.forEach((id, ri) => {
          pos[id] = { x: D.pad + ci * (D.colW + D.colGap), y: D.pad + D.headH + ri * (D.chipH + D.chipGap), col: ci };
        });
      });
      const width = D.pad * 2 + cols.length * D.colW + (cols.length - 1) * D.colGap;
      const height = D.pad * 2 + D.headH + maxRows * (D.chipH + D.chipGap);
      const out = [`<svg class="wf-diagram" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="Workflow columns and states">`];
      out.push('<defs><marker id="wf-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="6" markerHeight="6" orient="auto-start-reverse"><path d="M0 0 L8 4 L0 8 z" class="wf-dg-arrowhead"/></marker></defs>');
      cols.forEach((c, ci) => {
        const x = D.pad + ci * (D.colW + D.colGap);
        out.push(`<g class="wf-dg-col${c.terminal ? ' terminal' : ''}"><rect x="${x}" y="${D.pad}" width="${D.colW}" height="${height - D.pad * 2}" rx="6" class="wf-dg-lane"/>` +
          `<text x="${x + D.colW / 2}" y="${D.pad + D.headH / 2 + 4}" text-anchor="middle" class="wf-dg-colname">${esc(wfTrim(c.label || c.id, D.maxLabel))}</text></g>`);
      });
      const edges = Array.isArray(spec.transitions) ? spec.transitions : [];
      const seen = {};
      edges.forEach(t => {
        const a = pos[t.from], b = pos[t.to];
        if (!a || !b || t.from === t.to) return;
        const k = t.from + '>' + t.to;
        if (seen[k]) return;
        seen[k] = true;
        const title = esc((t.on || '') + (t.note ? ' — ' + t.note : ''));
        let d;
        if (a.col === b.col) {
          // Same lane: bow out to the right of the chips.
          const x = a.x + D.colW, y1 = a.y + D.chipH / 2, y2 = b.y + D.chipH / 2, bow = D.colGap * 0.6;
          d = `M${x} ${y1} C${x + bow} ${y1}, ${x + bow} ${y2}, ${x} ${y2}`;
        } else {
          const fwd = b.col > a.col;
          const x1 = fwd ? a.x + D.colW : a.x, x2 = fwd ? b.x : b.x + D.colW;
          const y1 = a.y + D.chipH / 2, y2 = b.y + D.chipH / 2, dx = (x2 - x1) / 2;
          d = `M${x1} ${y1} C${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`;
        }
        out.push(`<path d="${d}" class="wf-dg-edge${a.col > b.col ? ' back' : ''}" marker-end="url(#wf-arrow)"><title>${title}</title></path>`);
      });
      cols.forEach(c => (c.states || []).forEach(id => {
        const p = pos[id], s = states[id] || { id: id, label: id };
        const lc = s.lifecycle || {};
        const inferred = lc.confidence === 'inferred';
        const num = lc.number ? String(lc.number).padStart(3, '0') : '';
        const cls = 'wf-dg-state' + (inferred ? ' inferred' : '') + (spec.initial === id ? ' initial' : '') + (profile && profile.terminal && profile.terminal[id] ? ' terminal' : '');
        const tip = esc(s.label || id) + (num ? ' · stage/' + num : '') + (inferred ? ' · inferred' : '') + (lc.column ? '\ncolumn ' + esc(lc.column) : '');
        out.push(`<g class="${cls}" data-state="${esc(id)}"><title>${tip}</title>` +
          `<rect x="${p.x + 8}" y="${p.y}" width="${D.colW - 16}" height="${D.chipH}" rx="5" class="wf-dg-chip"/>` +
          (num ? `<text x="${p.x + D.textPad + 4}" y="${p.y + D.chipH / 2 + 4}" class="wf-dg-num mono">${num}</text>` : '') +
          `<text x="${p.x + D.textPad + (num ? 34 : 4)}" y="${p.y + D.chipH / 2 + 4}" class="wf-dg-label">${esc(wfTrim(s.label || id, num ? D.maxLabel - 5 : D.maxLabel))}</text></g>`);
      }));
      out.push('</svg>');
      return out.join('');
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
        owner ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibDuplicate" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Duplicate</button>` : '',
        `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfLibExport" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}">Export JSON</button>`,
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
        '</div>' + result + '</div>';
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
      } else {
        body = renderWorkflowLibraryTable(_workflowState.workflows, _workflowState.bindings, owner);
      }
      return wfLibNav(_wfLib.view) + body;
    }

    function wfLibRerender() { if (_configState && _configState.tab === 'Workflows') renderConfigTab('Workflows'); }

    function wfLibShow(view) {
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
