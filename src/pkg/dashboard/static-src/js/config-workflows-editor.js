// Settings → Workflows → Editor (hivecommons/hive#17620, RFC phase 4 UI).
    //
    // A design surface for workflow models: columns are vertical lanes on an
    // SVG canvas, states are chips inside them, transitions are edges drawn
    // by dragging from a chip's port. Everything that changes the model goes
    // through the pure functions in the first half of this file (wfEd* that
    // take a model and return a new one) so the node harness
    // (workflows_editor_ui_test.go) can exercise them without a DOM. The
    // second half is the runtime: _wfEd state, render, pointer handling,
    // live compile, the YAML/Diff/Simulate drawers and persistence.
    const WF_ED = { colW: 200, colGap: 32, chipH: 34, chipGap: 12, headH: 36, pad: 16, grid: 8, minRows: 4, portR: 5, minColW: 120, maxColW: 480 };
    const WF_ED_HISTORY_CAP = 200;
    const WF_ED_COMPILE_DEBOUNCE_MS = 300;
    const WF_ED_YAML_DEBOUNCE_MS = 500;
    const WF_ED_UNASSIGNED = '__unassigned';
    const WF_ED_API_VERSION = 'workflow.hivecommons.dev/v1alpha1';
    const WF_ED_KINDS = ['issue', 'pr'];
    const WF_ED_OWNERS = ['human', 'registered-agent', 'external-service', 'external-observation'];
    const WF_ED_STATE_TYPES = ['', 'parallel', 'final'];
    const WF_ED_DERIVATIONS = ['builtin', 'rules'];
    const WF_ED_DRAWERS = ['problems', 'yaml', 'diff', 'simulate'];
    // Palette: common states and columns. Numbers follow hive-default's
    // stage ladder so a palette-built model lines up with builtin derivation.
    const WF_ED_PALETTE_STATES = [
      { id: 'open', label: 'Open', number: 10 },
      { id: 'triage', label: 'Triage', number: 20 },
      { id: 'needs-decision', label: 'Needs decision', number: 25, kinds: ['issue'] },
      { id: 'claimed', label: 'Claimed', number: 30, kinds: ['issue'] },
      { id: 'in-progress', label: 'Work in progress', number: 40 },
      { id: 'in-review', label: 'In review', number: 60, kinds: ['pr'] },
      { id: 'ci-failing', label: 'CI failing', number: 65, kinds: ['pr'] },
      { id: 'merge-queue', label: 'Merge queue', number: 80, kinds: ['pr'] },
      { id: 'merged', label: 'Merged', number: 90, kinds: ['pr'] },
      { id: 'released', label: 'Released', number: 95 },
      { id: 'deployed-staging', label: 'Deployed · staging', number: 96 },
      { id: 'deployed-production', label: 'Deployed · production', number: 97 },
      { id: 'done', label: 'Done', number: 100 },
      { id: 'blocked', label: 'Blocked', number: 50 },
    ];
    const WF_ED_PALETTE_COLUMNS = [
      { id: 'intake', label: 'Intake' },
      { id: 'analysis', label: 'Analysis' },
      { id: 'writing', label: 'Writing' },
      { id: 'review', label: 'Review' },
      { id: 'publish', label: 'Publish' },
      { id: 'done', label: 'Done', terminal: true },
    ];

    // --- Pure model core ----------------------------------------------------

    function wfEdClone(v) { return v === undefined ? undefined : JSON.parse(JSON.stringify(v)); }

    function wfEdSlug(s) {
      return String(s || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 48) || 'item';
    }

    function wfEdSpec(model) { return (model && model.spec) || {}; }

    // wfEdStates flattens states including regions (the compiler does too).
    function wfEdStates(model) {
      const out = [];
      (function walk(list) { (list || []).forEach(s => { if (s && s.id) { out.push(s); walk(s.regions); } }); })(wfEdSpec(model).states);
      return out;
    }

    function wfEdFindState(model, id) { return wfEdStates(model).find(s => s.id === id) || null; }
    function wfEdFindColumn(model, id) { return (wfEdSpec(model).columns || []).find(c => c.id === id) || null; }

    function wfEdUniqueId(model, base) {
      const taken = {};
      wfEdStates(model).forEach(s => { taken[s.id] = true; });
      (wfEdSpec(model).columns || []).forEach(c => { taken[c.id] = true; });
      const root = wfEdSlug(base);
      if (!taken[root]) return root;
      for (let i = 2; ; i++) { const cand = root + '-' + i; if (!taken[cand]) return cand; }
    }

    function wfEdNewModel(name) {
      return {
        apiVersion: WF_ED_API_VERSION, kind: 'WorkflowModel',
        metadata: { name: wfEdSlug(name || 'my-lifecycle') },
        spec: {
          meta: { version: '1', provenance: 'local', source: 'hand', description: '' },
          scope: { forge: 'github.com', repository: '*' },
          initial: 'open',
          columns: [{ id: 'intake', label: 'Intake', states: ['open'] }],
          states: [{ id: 'open', label: 'Open', owner: 'human', lifecycle: { column: 'intake', number: 10 } }],
          transitions: [],
          derivation: { mode: 'builtin' },
          labels: { prefix: 'stage/' },
        },
      };
    }

    // wfEdNormalize makes the two column/state links agree: every column's
    // states list is rebuilt from membership plus lifecycle.column fallbacks,
    // and each state's lifecycle.column names the lane it sits in.
    function wfEdNormalize(model) {
      const m = wfEdClone(model) || {};
      m.spec = m.spec || {};
      const spec = m.spec;
      spec.columns = Array.isArray(spec.columns) ? spec.columns : [];
      spec.states = Array.isArray(spec.states) ? spec.states : [];
      spec.transitions = Array.isArray(spec.transitions) ? spec.transitions : [];
      const states = wfEdStates(m);
      const known = {}; states.forEach(s => { known[s.id] = s; });
      const placed = {};
      spec.columns.forEach(c => {
        c.states = (Array.isArray(c.states) ? c.states : []).filter(id => known[id] && !placed[id]);
        c.states.forEach(id => { placed[id] = c.id; });
      });
      states.forEach(s => {
        if (placed[s.id]) { s.lifecycle = Object.assign({}, s.lifecycle, { column: placed[s.id] }); return; }
        const want = s.lifecycle && s.lifecycle.column;
        const col = want && spec.columns.find(c => c.id === want);
        if (col) { col.states.push(s.id); placed[s.id] = col.id; }
        else if (s.lifecycle && s.lifecycle.column) { s.lifecycle = Object.assign({}, s.lifecycle); delete s.lifecycle.column; }
      });
      return m;
    }

    function wfEdAddColumn(model, col, index) {
      const m = wfEdNormalize(model);
      const c = Object.assign({ label: '' }, col || {});
      c.id = wfEdUniqueId(m, c.id || c.label || 'column');
      if (!c.label) c.label = c.id;
      c.states = [];
      if (!c.terminal) delete c.terminal;
      if (!c.hint) delete c.hint;
      const at = (index === undefined || index === null || index < 0 || index > m.spec.columns.length) ? m.spec.columns.length : index;
      m.spec.columns.splice(at, 0, c);
      return m;
    }

    function wfEdMoveColumn(model, id, toIndex) {
      const m = wfEdNormalize(model);
      const from = m.spec.columns.findIndex(c => c.id === id);
      if (from < 0) return m;
      const col = m.spec.columns.splice(from, 1)[0];
      const to = Math.max(0, Math.min(toIndex, m.spec.columns.length));
      m.spec.columns.splice(to, 0, col);
      return m;
    }

    function wfEdRenameColumn(model, oldId, newId) {
      const m = wfEdNormalize(model);
      const id = wfEdSlug(newId);
      if (!id || id === oldId) return m;
      if (m.spec.columns.some(c => c.id === id) || wfEdStates(m).some(s => s.id === id)) return m;
      m.spec.columns.forEach(c => { if (c.id === oldId) c.id = id; });
      wfEdStates(m).forEach(s => { if (s.lifecycle && s.lifecycle.column === oldId) s.lifecycle.column = id; });
      return m;
    }

    // wfEdRemoveColumn drops a lane. Its states move to moveStatesTo when
    // given, otherwise they become unassigned (drawn in the trailing lane).
    function wfEdRemoveColumn(model, id, moveStatesTo) {
      const m = wfEdNormalize(model);
      const idx = m.spec.columns.findIndex(c => c.id === id);
      if (idx < 0) return m;
      const gone = m.spec.columns.splice(idx, 1)[0];
      const target = moveStatesTo && moveStatesTo !== id ? m.spec.columns.find(c => c.id === moveStatesTo) : null;
      (gone.states || []).forEach(sid => {
        const s = wfEdFindState(m, sid);
        if (target) { target.states.push(sid); if (s) s.lifecycle = Object.assign({}, s.lifecycle, { column: target.id }); }
        else if (s && s.lifecycle) { delete s.lifecycle.column; }
      });
      return m;
    }

    function wfEdAddState(model, state, columnId, index) {
      const m = wfEdNormalize(model);
      const s = Object.assign({ label: '', owner: 'human' }, state || {});
      s.id = wfEdUniqueId(m, s.id || s.label || 'state');
      if (!s.label) s.label = s.id;
      if (!s.owner) s.owner = 'human';
      s.lifecycle = Object.assign({}, s.lifecycle || {});
      const col = columnId ? m.spec.columns.find(c => c.id === columnId) : null;
      if (col) {
        s.lifecycle.column = col.id;
        const at = (index === undefined || index === null || index < 0 || index > col.states.length) ? col.states.length : index;
        col.states.splice(at, 0, s.id);
      } else {
        delete s.lifecycle.column;
      }
      m.spec.states.push(s);
      if (!m.spec.initial) m.spec.initial = s.id;
      return m;
    }

    // wfEdMoveState puts a state into a lane at a position (columnId '' or
    // unknown → unassigned). Moving within the same lane re-orders it.
    function wfEdMoveState(model, id, columnId, index) {
      const m = wfEdNormalize(model);
      const s = wfEdFindState(m, id);
      if (!s) return m;
      m.spec.columns.forEach(c => { c.states = c.states.filter(x => x !== id); });
      const col = columnId && columnId !== WF_ED_UNASSIGNED ? m.spec.columns.find(c => c.id === columnId) : null;
      s.lifecycle = Object.assign({}, s.lifecycle || {});
      if (col) {
        const at = (index === undefined || index === null || index < 0 || index > col.states.length) ? col.states.length : index;
        col.states.splice(at, 0, id);
        s.lifecycle.column = col.id;
      } else {
        delete s.lifecycle.column;
      }
      return m;
    }

    // wfEdRenameState changes an id and every reference to it: column
    // membership, transitions, initial, label entries, derivation rules,
    // lanes and agent grants.
    function wfEdRenameState(model, oldId, newId) {
      const m = wfEdNormalize(model);
      const id = wfEdSlug(newId);
      if (!id || id === oldId || !wfEdFindState(m, oldId)) return m;
      if (wfEdFindState(m, id) || m.spec.columns.some(c => c.id === id)) return m;
      const spec = m.spec;
      wfEdStates(m).forEach(s => { if (s.id === oldId) s.id = id; });
      spec.columns.forEach(c => { c.states = c.states.map(x => x === oldId ? id : x); });
      spec.transitions.forEach(t => { if (t.from === oldId) t.from = id; if (t.to === oldId) t.to = id; });
      if (spec.initial === oldId) spec.initial = id;
      ((spec.labels && spec.labels.entries) || []).forEach(l => { if (l.state === oldId) l.state = id; });
      ((spec.derivation && spec.derivation.rules) || []).forEach(r => { if (r.state === oldId) r.state = id; });
      (spec.lanes || []).forEach(l => { if (l.stateRef === oldId) l.stateRef = id; });
      (spec.agents || []).forEach(a => { if (Array.isArray(a.states)) a.states = a.states.map(x => x === oldId ? id : x); });
      return m;
    }

    function wfEdRemoveState(model, id) {
      const m = wfEdNormalize(model);
      const spec = m.spec;
      (function prune(list) {
        for (let i = list.length - 1; i >= 0; i--) {
          if (list[i].id === id) list.splice(i, 1);
          else if (Array.isArray(list[i].regions)) prune(list[i].regions);
        }
      })(spec.states);
      spec.columns.forEach(c => { c.states = c.states.filter(x => x !== id); });
      spec.transitions = spec.transitions.filter(t => t.from !== id && t.to !== id);
      if (spec.labels && Array.isArray(spec.labels.entries)) spec.labels.entries = spec.labels.entries.filter(l => l.state !== id);
      if (spec.derivation && Array.isArray(spec.derivation.rules)) spec.derivation.rules = spec.derivation.rules.filter(r => r.state !== id);
      if (spec.initial === id) { const rest = wfEdStates(m); spec.initial = rest.length ? rest[0].id : ''; }
      return m;
    }

    function wfEdAddTransition(model, from, to, on) {
      const m = wfEdNormalize(model);
      if (!wfEdFindState(m, from) || !wfEdFindState(m, to)) return m;
      const ev = on == null ? '' : String(on);
      if (m.spec.transitions.some(t => t.from === from && t.to === to && (t.on || '') === ev)) return m;
      m.spec.transitions.push({ from: from, to: to, on: ev });
      return m;
    }

    function wfEdRemoveTransition(model, index) {
      const m = wfEdNormalize(model);
      if (index >= 0 && index < m.spec.transitions.length) m.spec.transitions.splice(index, 1);
      return m;
    }

    // wfEdPath splits "spec.states[id=open].lifecycle.number" or
    // "spec.transitions[2].on" into segments; [k=v] selects an array item by
    // field, [n] by index.
    function wfEdPath(path) {
      const out = [];
      String(path || '').split('.').forEach(part => {
        const re = /([^[\]]+)|\[([^\]]+)\]/g;
        let mt;
        while ((mt = re.exec(part)) !== null) {
          if (mt[1] !== undefined) out.push({ key: mt[1] });
          else if (mt[2].indexOf('=') > 0) { const kv = mt[2].split('='); out.push({ field: kv[0], value: kv.slice(1).join('=') }); }
          else out.push({ index: parseInt(mt[2], 10) });
        }
      });
      return out;
    }

    // wfEdUpdate sets one field by path and returns a new model. Empty
    // string / null / undefined deletes optional scalars so the saved JSON
    // stays tidy. Fields that re-link structure (state column, ids) have
    // their own functions; this one never touches links.
    function wfEdUpdate(model, path, value) {
      const m = wfEdClone(model);
      const segs = wfEdPath(path);
      if (!segs.length) return m;
      let cur = m;
      for (let i = 0; i < segs.length - 1; i++) {
        const s = segs[i];
        let next;
        if (s.key !== undefined) { if (cur[s.key] === undefined || cur[s.key] === null) cur[s.key] = (segs[i + 1].key !== undefined) ? {} : []; next = cur[s.key]; }
        else if (s.index !== undefined) { next = cur[s.index]; }
        else { next = Array.isArray(cur) ? cur.find(x => x && String(x[s.field]) === s.value) : undefined; }
        if (next === undefined || next === null) return m;
        cur = next;
      }
      const last = segs[segs.length - 1];
      const remove = value === undefined || value === null || value === '' || (Array.isArray(value) && !value.length);
      if (last.key !== undefined) { if (remove) delete cur[last.key]; else cur[last.key] = value; }
      else if (last.index !== undefined) { if (remove) cur.splice(last.index, 1); else cur[last.index] = value; }
      return m;
    }

    // wfEdDepths is the BFS distance of every state from the initial state
    // along transitions; unreachable states get Infinity.
    function wfEdDepths(model) {
      const spec = wfEdSpec(model);
      const depth = {};
      wfEdStates(model).forEach(s => { depth[s.id] = Infinity; });
      const start = spec.initial && depth[spec.initial] !== undefined ? spec.initial : (wfEdStates(model)[0] || {}).id;
      if (!start) return depth;
      depth[start] = 0;
      const queue = [start];
      while (queue.length) {
        const cur = queue.shift();
        (spec.transitions || []).forEach(t => {
          if (t.from === cur && depth[t.to] !== undefined && depth[t.to] === Infinity) { depth[t.to] = depth[cur] + 1; queue.push(t.to); }
        });
      }
      return depth;
    }

    // wfEdAutoLayout orders columns by the depth of their states from the
    // initial state (terminal lanes last), places unassigned states by
    // depth into the nearest lane, and sorts each lane by stage number then
    // label. Pure: returns a new model.
    function wfEdAutoLayout(model) {
      const m = wfEdNormalize(model);
      const depth = wfEdDepths(m);
      const states = wfEdStates(m);
      const byId = {}; states.forEach(s => { byId[s.id] = s; });
      const cols = m.spec.columns;
      if (!cols.length) return m;
      const colDepth = cols.map((c, i) => {
        const ds = c.states.map(id => depth[id]).filter(d => isFinite(d));
        return { col: c, i: i, d: ds.length ? Math.min.apply(null, ds) : Infinity };
      });
      colDepth.sort((a, b) => {
        if (!!a.col.terminal !== !!b.col.terminal) return a.col.terminal ? 1 : -1;
        if (a.d !== b.d) return (isFinite(a.d) ? a.d : 1e9) - (isFinite(b.d) ? b.d : 1e9);
        return a.i - b.i;
      });
      m.spec.columns = colDepth.map(x => x.col);
      // Unassigned states join the lane whose depth is closest.
      const placed = {}; m.spec.columns.forEach(c => c.states.forEach(id => { placed[id] = true; }));
      states.forEach(s => {
        if (placed[s.id]) return;
        const d = depth[s.id];
        let best = null, bestGap = Infinity;
        colDepth.forEach(x => { const gap = Math.abs((isFinite(x.d) ? x.d : 1e9) - (isFinite(d) ? d : 1e9)); if (gap < bestGap) { bestGap = gap; best = x.col; } });
        if (!best) best = m.spec.columns[m.spec.columns.length - 1];
        best.states.push(s.id);
        s.lifecycle = Object.assign({}, s.lifecycle, { column: best.id });
      });
      m.spec.columns.forEach(c => {
        c.states.sort((a, b) => {
          const na = (byId[a].lifecycle || {}).number || 0, nb = (byId[b].lifecycle || {}).number || 0;
          if (na !== nb) return (na || 1e9) - (nb || 1e9);
          const da = depth[a], db = depth[b];
          if (da !== db) return (isFinite(da) ? da : 1e9) - (isFinite(db) ? db : 1e9);
          return String(byId[a].label || a).localeCompare(String(byId[b].label || b));
        });
      });
      return m;
    }

    function wfEdSnap(v) { return Math.round(v / WF_ED.grid) * WF_ED.grid; }

    // wfEdLayout computes lane and chip geometry in model space (before
    // pan/zoom). view.laneWidths may override a lane's width by id. States
    // not in any lane are drawn in a trailing dashed "unassigned" lane.
    function wfEdLayout(model, profile, view) {
      const D = WF_ED;
      const spec = wfEdSpec(model);
      const widths = (view && view.laneWidths) || {};
      const states = wfEdStates(model);
      const byId = {}; states.forEach(s => { byId[s.id] = s; });
      const lanes = (spec.columns || []).map(c => ({ id: c.id, label: c.label || c.id, hint: c.hint || '', terminal: !!c.terminal, states: (c.states || []).filter(id => byId[id]), virtual: false }));
      const placed = {}; lanes.forEach(l => l.states.forEach(id => { placed[id] = true; }));
      const loose = states.filter(s => !placed[s.id]).map(s => s.id);
      if (loose.length || !lanes.length) lanes.push({ id: WF_ED_UNASSIGNED, label: 'Unassigned', hint: 'drag states into a lane', terminal: false, states: loose, virtual: true });
      let maxRows = D.minRows;
      lanes.forEach(l => { maxRows = Math.max(maxRows, l.states.length + 1); });
      const laneH = D.headH + maxRows * (D.chipH + D.chipGap) + D.chipGap;
      const laneIndex = {}; lanes.forEach((l, ci) => l.states.forEach(id => { laneIndex[id] = ci; }));
      const channels = wfEdgeChannels(spec.transitions || [], id => (laneIndex[id] == null ? null : laneIndex[id]));
      const topCorr = wfEdgeCorridor(channels.top), bottomCorr = wfEdgeCorridor(channels.bottom);
      const laneY = D.pad + topCorr;
      let x = D.pad;
      const cols = [], chips = {};
      lanes.forEach((l, ci) => {
        const w = Math.max(D.minColW, Math.min(D.maxColW, wfEdSnap(widths[l.id] || D.colW)));
        cols.push({ id: l.id, label: l.label, hint: l.hint, terminal: l.terminal, virtual: l.virtual, index: ci, x: x, y: laneY, w: w, h: laneH });
        l.states.forEach((id, ri) => {
          chips[id] = { id: id, col: l.id, colIndex: ci, index: ri, x: x + D.grid, y: laneY + D.headH + D.chipGap + ri * (D.chipH + D.chipGap), w: w - D.grid * 2, h: D.chipH };
        });
        x += w + D.colGap;
      });
      const width = x - D.colGap + D.pad, height = D.pad * 2 + topCorr + laneH + bottomCorr;
      const routed = wfRouteEdges(spec.transitions || [], { cols: cols, chips: chips, gap: D.colGap, pad: D.pad }, { expanded: (view && view.fansOpen) || {} });
      const edges = routed.edges, fans = routed.fans;
      return { width: width, height: height, cols: cols, chips: chips, edges: edges, fans: fans, commonEvent: routed.common, laneH: laneH };
    }

    // wfEdLaneAt / wfEdSlotAt map a model-space point onto a lane and a chip
    // slot index, for drops.
    function wfEdLaneAt(layout, x) {
      for (const c of layout.cols) { if (x >= c.x - WF_ED.colGap / 2 && x < c.x + c.w + WF_ED.colGap / 2) return c; }
      return null;
    }
    function wfEdSlotAt(layout, lane, y) {
      const top = lane.y + WF_ED.headH + WF_ED.chipGap;
      const n = Object.keys(layout.chips).filter(id => layout.chips[id].col === lane.id).length;
      return Math.max(0, Math.min(n, Math.round((y - top) / (WF_ED.chipH + WF_ED.chipGap))));
    }

    // wfEdLocalProblems mirrors the compiler's refusals so the canvas can
    // flag them before the server answers. Codes match workflowmodel.
    function wfEdLocalProblems(model) {
      const spec = wfEdSpec(model);
      const out = [];
      const push = (code, severity, subject, detail) => out.push({ code: code, severity: severity, subject: subject, detail: detail });
      const states = wfEdStates(model);
      const byId = {}; states.forEach(s => { byId[s.id] = s; });
      const cols = spec.columns || [];
      const colById = {}; cols.forEach(c => { colById[c.id] = c; });
      if (!(model && model.metadata && model.metadata.name)) push('invalid', 'error', '', 'metadata.name is required');
      if (!states.length) push('invalid', 'error', '', 'the model has no states');
      if (!spec.initial) push('invalid', 'error', '', 'spec.initial is empty');
      else if (!byId[spec.initial]) push('unknown-state', 'error', spec.initial, 'spec.initial names a state that does not exist');
      const seenIds = {};
      states.forEach(s => { if (seenIds[s.id]) push('invalid', 'error', s.id, 'duplicate state id'); seenIds[s.id] = true; });
      const terminalStates = {};
      cols.forEach(c => { if (c.terminal) (c.states || []).forEach(id => { terminalStates[id] = true; }); });
      const hasExit = {};
      (spec.transitions || []).forEach((t, i) => {
        const subj = t.from + '->' + t.to;
        if (!byId[t.from]) push('unknown-state', 'error', subj, 'transition from unknown state ' + t.from);
        if (!byId[t.to]) push('unknown-state', 'error', subj, 'transition to unknown state ' + t.to);
        if (t.from !== t.to) hasExit[t.from] = true;
        if (Array.isArray(spec.events) && spec.events.length && t.on && !spec.events.some(e => e.id === t.on)) push('unknown-event', 'error', subj, 'event ' + t.on + ' is not declared in spec.events');
      });
      const memberOf = {};
      cols.forEach(c => {
        if (!(c.states || []).length) push('column-empty', 'warning', c.id, 'column has no states');
        (c.states || []).forEach(id => {
          if (!byId[id]) push('unknown-state', 'error', c.id, 'column lists unknown state ' + id);
          if (memberOf[id]) push('state-no-column', 'error', id, 'state is listed in both ' + memberOf[id] + ' and ' + c.id);
          memberOf[id] = c.id;
        });
      });
      if (cols.length && !cols.some(c => c.terminal)) push('no-terminal-column', 'error', '', 'no column is marked terminal');
      const numbers = {};
      states.forEach(s => {
        const lc = s.lifecycle || {};
        if (!s.owner) push('invalid', 'error', s.id, 'state has no owner');
        if (s.type === 'parallel' && !(Array.isArray(s.regions) && s.regions.length)) push('invalid', 'error', s.id, 'parallel state has no regions');
        if (cols.length && !memberOf[s.id]) push('state-no-column', 'error', s.id, 'state is not in any column');
        if (lc.column && !colById[lc.column]) push('unknown-column', 'error', s.id, 'lifecycle.column names unknown column ' + lc.column);
        if (lc.column && memberOf[s.id] && lc.column !== memberOf[s.id]) push('state-no-column', 'error', s.id, 'lifecycle.column is ' + lc.column + ' but the state sits in ' + memberOf[s.id]);
        if (!terminalStates[s.id] && s.type !== 'final' && s.type !== 'parallel' && !hasExit[s.id]) push('state-no-exit', 'error', s.id, 'non-terminal state has no outgoing transition');
        if (lc.number) {
          const kinds = Array.isArray(lc.kinds) && lc.kinds.length ? lc.kinds : WF_ED_KINDS;
          kinds.forEach(k => {
            const key = k + ':' + lc.number;
            if (numbers[key] && numbers[key] !== s.id) push('duplicate-stage-number', 'error', s.id, 'stage ' + lc.number + ' for ' + k + ' is also used by ' + numbers[key]);
            numbers[key] = s.id;
          });
        }
        if (lc.confidence === 'inferred') push('inferred-element', 'warning', s.id, 'state was inferred and not confirmed');
      });
      return out;
    }

    // wfEdDiff is the client-side twin of workflowmodel.Diff for the unsaved
    // working copy: same keyed collections and the same flat field changes.
    function wfEdDiff(a, b) {
      const flat = v => v === undefined || v === null ? '' : (Array.isArray(v) ? v.map(x => typeof x === 'object' ? JSON.stringify(x) : String(x)).join(',') : (typeof v === 'object' ? JSON.stringify(v) : String(v)));
      const diffItems = (ia, ib) => {
        const out = { added: [], removed: [], changed: [] };
        Object.keys(ib).forEach(k => { if (!(k in ia)) out.added.push(k); });
        Object.keys(ia).forEach(k => { if (!(k in ib)) out.removed.push(k); });
        Object.keys(ia).forEach(k => {
          if (!(k in ib)) return;
          const fields = [];
          const keys = {}; Object.keys(ia[k]).concat(Object.keys(ib[k])).forEach(f => { keys[f] = true; });
          Object.keys(keys).sort().forEach(f => { const fa = flat(ia[k][f]), fb = flat(ib[k][f]); if (fa !== fb) fields.push({ field: f, from: fa, to: fb }); });
          if (fields.length) out.changed.push({ key: k, fields: fields });
        });
        out.added.sort(); out.removed.sort(); out.changed.sort((x, y) => x.key.localeCompare(y.key));
        return out;
      };
      const strip = (o, drop) => { const c = Object.assign({}, o); drop.forEach(k => { delete c[k]; }); return c; };
      const colItems = m => { const o = {}; (wfEdSpec(m).columns || []).forEach(c => { o[c.id] = strip(c, ['id']); }); return o; };
      const stateItems = m => { const o = {}; wfEdStates(m).forEach(s => { const c = strip(s, ['id', 'regions', 'lifecycle']); Object.keys(s.lifecycle || {}).forEach(k => { c['lifecycle.' + k] = s.lifecycle[k]; }); o[s.id] = c; }); return o; };
      const transItems = m => {
        const o = {}, count = {};
        (wfEdSpec(m).transitions || []).forEach(t => { const k = t.from + '>' + t.to; count[k] = (count[k] || 0) + 1; });
        (wfEdSpec(m).transitions || []).forEach(t => { const k = t.from + '>' + t.to; o[count[k] > 1 ? k + '@' + (t.on || '') : k] = strip(t, ['from', 'to']); });
        return o;
      };
      const labelItems = m => { const o = {}; ((wfEdSpec(m).labels || {}).entries || []).forEach(l => { o[l.name] = strip(l, ['name']); }); return o; };
      const ruleItems = m => { const o = {}; ((wfEdSpec(m).derivation || {}).rules || []).forEach((r, i) => { o[r.id || 'rules[' + i + ']'] = strip(r, ['id']); }); return o; };
      const d = {
        columns: diffItems(colItems(a), colItems(b)),
        states: diffItems(stateItems(a), stateItems(b)),
        transitions: diffItems(transItems(a), transItems(b)),
        labels: diffItems(labelItems(a), labelItems(b)),
        rules: diffItems(ruleItems(a), ruleItems(b)),
        meta: [],
      };
      const ma = wfEdSpec(a).meta || {}, mb = wfEdSpec(b).meta || {};
      const mkeys = {}; Object.keys(ma).concat(Object.keys(mb)).forEach(k => { mkeys[k] = true; });
      Object.keys(mkeys).sort().forEach(k => { if (flat(ma[k]) !== flat(mb[k])) d.meta.push({ field: k, from: flat(ma[k]), to: flat(mb[k]) }); });
      const parts = [];
      let changed = d.meta.length;
      ['columns', 'states', 'transitions', 'labels', 'rules'].forEach(k => {
        const cs = d[k];
        if (cs.added.length) parts.push('+' + cs.added.length + ' ' + k);
        if (cs.removed.length) parts.push('\u2212' + cs.removed.length + ' ' + k);
        changed += cs.changed.length;
      });
      if (changed) parts.push(changed + ' changed');
      d.empty = !parts.length;
      d.summary = d.empty ? 'no changes' : parts.join(', ');
      return d;
    }

    // wfEdSvg renders the canvas. `state` carries selection, diagnostics by
    // subject, a simulate hit, profile and the view transform. Pure.
    function wfEdSvg(model, layout, state) {
      const st = state || {};
      const D = WF_ED;
      const spec = wfEdSpec(model);
      const view = st.view || { panX: 0, panY: 0, zoom: 1 };
      const sel = st.selection || { type: '', ids: [] };
      const isSel = (type, id) => sel.type === type && sel.ids.indexOf(id) >= 0;
      const diags = {};
      (st.diags || []).forEach(d => { const k = d.subject || ''; if (!diags[k] || d.severity === 'error') diags[k] = Object.assign({}, diags[k] || {}, { severity: diags[k] && diags[k].severity === 'error' ? 'error' : d.severity, text: ((diags[k] && diags[k].text) ? diags[k].text + '\n' : '') + d.code + ': ' + d.detail }); });
      const states = {}; wfEdStates(model).forEach(s => { states[s.id] = s; });
      const profile = st.profile || null;
      const out = [];
      const vw = st.viewportW || layout.width, vh = st.viewportH || layout.height;
      out.push(`<svg class="wf-ed-svg${sel.type === 'state' && sel.ids.length === 1 ? ' wf-focus' : ''}" width="${vw}" height="${vh}" viewBox="0 0 ${vw} ${vh}" role="application" aria-label="Workflow model canvas: ${esc(String(spec.columns ? spec.columns.length : 0))} columns, ${esc(String(Object.keys(states).length))} states" tabindex="0" data-wf-ed-canvas="1" data-wf-focus-root="1">`);
      out.push('<defs><marker id="wf-ed-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0 0 L8 4 L0 8 z" class="wf-ed-arrowhead" fill="context-stroke"/></marker>' +
        `<pattern id="wf-ed-grid" width="${D.grid * 4}" height="${D.grid * 4}" patternUnits="userSpaceOnUse"><path d="M ${D.grid * 4} 0 L 0 0 0 ${D.grid * 4}" class="wf-ed-gridline"/></pattern></defs>`);
      out.push(`<rect class="wf-ed-bg" x="0" y="0" width="${vw}" height="${vh}" fill="url(#wf-ed-grid)" data-wf-ed-bg="1"/>`);
      out.push(`<g class="wf-ed-world" transform="translate(${view.panX} ${view.panY}) scale(${view.zoom})">`);
      layout.cols.forEach(c => {
        const d = diags[c.id];
        const cls = 'wf-ed-col' + (c.terminal ? ' terminal' : '') + (c.virtual ? ' virtual' : '') + (isSel('column', c.id) ? ' selected' : '') + (st.dropLane === c.id ? ' drop' : '');
        out.push(`<g class="${cls}" data-wf-col="${esc(c.id)}">` +
          `<rect x="${c.x}" y="${c.y}" width="${c.w}" height="${c.h}" rx="8" class="wf-ed-lane"/>` +
          `<rect x="${c.x}" y="${c.y}" width="${c.w}" height="${D.headH}" rx="8" class="wf-ed-lanehead" data-wf-colhead="${esc(c.id)}"${c.virtual ? '' : ' tabindex="0" role="button"'} aria-label="Column ${esc(c.label)}"/>` +
          `<text x="${c.x + c.w / 2}" y="${c.y + D.headH / 2 + 4}" text-anchor="middle" class="wf-ed-colname">${esc(wfTrim(c.label, Math.floor(c.w / 9)))}${c.terminal ? ' \u25A0' : ''}</text>` +
          (d ? `<g class="wf-ed-badge ${esc(d.severity)}" data-wf-problem="${esc(c.id)}"><circle cx="${c.x + c.w - 12}" cy="${c.y + 12}" r="7"/><text x="${c.x + c.w - 12}" y="${c.y + 15}" text-anchor="middle">!</text><title>${esc(d.text)}</title></g>` : '') +
          (c.virtual ? '' : `<rect x="${c.x + c.w - 6}" y="${c.y}" width="12" height="${c.h}" class="wf-ed-resize" data-wf-resize="${esc(c.id)}"/>`) +
          '</g>');
      });
      // Edges: quiet by default. Labels render for every edge but CSS only
      // shows them on hover/selection, or (Labels toggle) for events other
      // than the model's most common one. A selected state puts the canvas
      // in focus mode so only its incident edges stay at full strength.
      const focusState = sel.type === 'state' && sel.ids.length === 1 ? sel.ids[0] : '';
      const fanned = {};
      (layout.fans || []).forEach(f => { f.edges.forEach(i => { fanned[i] = f.key; }); });
      const edgeSvg = e => {
        const subj = e.from + '->' + e.to, d = diags[subj];
        const selected = isSel('transition', String(e.index));
        const focus = focusState && (e.from === focusState || e.to === focusState);
        const cls = 'wf-ed-edge' + (e.back ? ' back' : '') + (e.self ? ' self' : '') + (selected ? ' selected' : '') + (focus ? ' focus' : '') + (e.collapsed ? ' wf-fan-edge' : '') +
          (e.common && !d ? ' common' : '') + (st.simHit && (st.simHit === e.to) ? ' sim-path' : '') + (d ? ' ' + d.severity : '');
        return `<g class="${cls}" data-wf-edge="${e.index}" data-wf-from="${esc(e.from)}" data-wf-to="${esc(e.to)}">` +
          `<path d="${e.d}" class="wf-ed-edge-hit"/>` +
          `<path d="${e.d}" class="wf-ed-edge-line" marker-end="url(#wf-ed-arrow)"/>` +
          (e.on || d ? wfEdgeLabelSvg((e.on || '(no event)') + (d ? ' !' : ''), e.lx, e.ly, 'wf-ed-edge-label') : '') +
          `<title>${esc(e.from)} \u2192 ${esc(e.to)}${e.on ? ' on ' + esc(e.on) : ' (no event)'}${e.note ? '\n' + esc(e.note) : ''}${d ? '\n' + esc(d.text) : ''}</title></g>`;
      };
      out.push(`<g class="wf-edge-layer${st.showLabels ? ' show-labels' : ''}">`);
      layout.edges.forEach(e => { if (!fanned[e.index]) out.push(edgeSvg(e)); });
      (layout.fans || []).forEach(f => {
        out.push(`<g class="wf-fan${f.open ? ' open' : ''}" data-wf-fan="${esc(f.key)}" data-wf-from="${esc(f.from)}"><path d="${f.d}" class="wf-ed-edge-line wf-fan-trunk"/>` +
          layout.edges.filter(e => fanned[e.index] === f.key).map(edgeSvg).join('') +
          `<g class="wf-fan-badge" role="button" tabindex="0" aria-label="${f.count} transitions from ${esc(f.from)} into one lane; ${f.open ? 'collapse' : 'expand'}"><rect x="${f.x + 6}" y="${f.y - 8}" width="28" height="16" rx="8"/><text x="${f.x + 20}" y="${f.y + 4}" text-anchor="middle">${f.open ? '\u2212' : '+'}${f.count}</text><title>${f.count} transitions into one lane \u00b7 click to ${f.open ? 'collapse' : 'expand'}</title></g></g>`);
      });
      out.push('</g>');
      Object.keys(layout.chips).forEach(id => {
        const p = layout.chips[id], s = states[id] || { id: id, label: id };
        const lc = s.lifecycle || {};
        const inferred = lc.confidence === 'inferred';
        const num = lc.number ? String(lc.number).padStart(3, '0') : '';
        const d = diags[id];
        const terminal = (profile && profile.terminal && profile.terminal[id]) || (layout.cols.find(c => c.id === p.col) || {}).terminal;
        const focus = focusState && (id === focusState || layout.edges.some(e => (e.from === focusState && e.to === id) || (e.to === focusState && e.from === id)));
        const cls = 'wf-ed-state' + (inferred ? ' inferred' : '') + (spec.initial === id ? ' initial' : '') + (terminal ? ' terminal' : '') + (isSel('state', id) ? ' selected' : '') + (focus ? ' focus' : '') +
          (st.simHit ? (st.simHit === id ? ' wf-ed-sim-hit' : ' wf-ed-sim-dim') : '') + (d ? ' ' + d.severity : '') + (st.dragging && st.dragging.indexOf(id) >= 0 ? ' dragging' : '');
        const kinds = Array.isArray(lc.kinds) && lc.kinds.length ? lc.kinds.join('+') : '';
        const tip = (s.label || id) + (num ? ' \u00b7 stage/' + num : '') + (kinds ? ' \u00b7 ' + kinds : '') + (inferred ? ' \u00b7 inferred' : '') + (d ? '\n' + d.text : '');
        out.push(`<g class="${cls}" data-wf-state="${esc(id)}" tabindex="0" role="button" aria-label="State ${esc(s.label || id)}${d ? ', ' + esc(d.severity) : ''}">` +
          `<title>${esc(tip)}</title>` +
          `<rect x="${p.x}" y="${p.y}" width="${p.w}" height="${p.h}" rx="6" class="wf-ed-chip"/>` +
          (spec.initial === id ? `<circle cx="${p.x - 1}" cy="${p.y + p.h / 2}" r="4" class="wf-ed-initial"/>` : '') +
          (num ? `<text x="${p.x + 10}" y="${p.y + p.h / 2 + 4}" class="wf-ed-num">${num}</text>` : '') +
          `<text x="${p.x + (num ? 42 : 10)}" y="${p.y + p.h / 2 + 4}" class="wf-ed-label">${esc(wfTrim(s.label || id, Math.floor((p.w - (num ? 56 : 24)) / 7.2)))}</text>` +
          (d ? `<g class="wf-ed-badge ${esc(d.severity)}" data-wf-problem="${esc(id)}"><circle cx="${p.x + p.w - 22}" cy="${p.y + 9}" r="7"/><text x="${p.x + p.w - 22}" y="${p.y + 12}" text-anchor="middle">!</text></g>` : '') +
          (inferred ? `<g class="wf-ed-confirm" data-wf-confirm="${esc(id)}" role="button" aria-label="Confirm ${esc(s.label || id)}"><rect x="${p.x + p.w - 42}" y="${p.y + p.h - 16}" width="30" height="14" rx="7"/><text x="${p.x + p.w - 27}" y="${p.y + p.h - 5}" text-anchor="middle">\u2713</text><title>Mark as confirmed</title></g>` : '') +
          `<circle cx="${p.x + p.w}" cy="${p.y + p.h / 2}" r="${D.portR}" class="wf-ed-port" data-wf-port="${esc(id)}"><title>Drag to another state to add a transition</title></circle>` +
          '</g>');
      });
      if (st.pendingEdge) out.push(`<path d="M${st.pendingEdge.x1} ${st.pendingEdge.y1} L${st.pendingEdge.x2} ${st.pendingEdge.y2}" class="wf-ed-edge-pending" marker-end="url(#wf-ed-arrow)"/>`);
      out.push('</g>');
      if (st.marquee) out.push(`<rect class="wf-ed-marquee" x="${Math.min(st.marquee.x1, st.marquee.x2)}" y="${Math.min(st.marquee.y1, st.marquee.y2)}" width="${Math.abs(st.marquee.x2 - st.marquee.x1)}" height="${Math.abs(st.marquee.y2 - st.marquee.y1)}"/>`);
      out.push('</svg>');
      return out.join('');
    }

    // wfEdMinimapSvg draws lanes and chips as small rectangles with the
    // viewport outlined. Pure.
    function wfEdMinimapSvg(layout, view, viewportW, viewportH) {
      const W = 160, H = Math.max(40, Math.round(W * layout.height / Math.max(1, layout.width)));
      const k = Math.min(W / Math.max(1, layout.width), H / Math.max(1, layout.height));
      const out = [`<svg class="wf-ed-minimap-svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="Minimap of the canvas" data-wf-ed-minimap="1" data-k="${k}">`];
      layout.cols.forEach(c => out.push(`<rect x="${c.x * k}" y="${c.y * k}" width="${c.w * k}" height="${c.h * k}" class="wf-ed-mm-lane${c.terminal ? ' terminal' : ''}"/>`));
      Object.keys(layout.chips).forEach(id => { const p = layout.chips[id]; out.push(`<rect x="${p.x * k}" y="${p.y * k}" width="${p.w * k}" height="${p.h * k}" class="wf-ed-mm-chip"/>`); });
      const vx = -view.panX / view.zoom * k, vy = -view.panY / view.zoom * k, vw = viewportW / view.zoom * k, vh = viewportH / view.zoom * k;
      out.push(`<rect x="${vx}" y="${vy}" width="${vw}" height="${vh}" class="wf-ed-mm-view"/>`);
      out.push('</svg>');
      return out.join('');
    }

    // wfEdFitView returns a view that fits the layout into a viewport.
    function wfEdFitView(layout, viewportW, viewportH) {
      const zoom = Math.max(0.2, Math.min(2, Math.min(viewportW / Math.max(1, layout.width), viewportH / Math.max(1, layout.height))));
      return { zoom: zoom, panX: Math.max(0, (viewportW - layout.width * zoom) / 2), panY: Math.max(0, (viewportH - layout.height * zoom) / 2) };
    }

    // wfEdCompileModel is the model as sent to compile/simulate: a draft
    // version stands in for the empty one the editor saves with.
    function wfEdCompileModel(model) {
      const m = wfEdClone(model);
      m.spec = m.spec || {};
      m.spec.meta = Object.assign({}, m.spec.meta);
      if (!m.spec.meta.version) m.spec.meta.version = 'draft';
      return m;
    }

    // wfEdSelectedProblem picks the compile status pill text from diagnostics.
    function wfEdStatus(diags, busy, compiledFlag) {
      const list = Array.isArray(diags) ? diags : [];
      const errors = list.filter(d => d.severity === 'error').length;
      const warnings = list.filter(d => d.severity !== 'error').length;
      if (errors || compiledFlag === false) return { cls: 'error', text: 'does not compile' + (errors ? ' \u00b7 ' + errors + ' error' + (errors === 1 ? '' : 's') : ''), errors: errors, warnings: warnings, busy: !!busy };
      if (warnings) return { cls: 'warn', text: warnings + ' warning' + (warnings === 1 ? '' : 's'), errors: 0, warnings: warnings, busy: !!busy };
      return { cls: 'ok', text: 'compiles', errors: 0, warnings: 0, busy: !!busy };
    }

    // --- Runtime state --------------------------------------------------------

    function wfEdDefaultState() {
      return {
        model: null, saved: null, source: null, readOnly: false, rev: 0,
        history: [], future: [], selection: { type: '', ids: [] },
        view: { panX: 0, panY: 0, zoom: 1, laneWidths: {}, showLabels: false, fansOpen: {} },
        compile: { busy: false, rev: -1, profile: null, diags: [], compiled: null, error: '' },
        drawer: { tab: 'problems', open: true },
        yaml: { text: '', rev: -1, error: '', busy: false, focused: false },
        diff: { versions: [], against: '', remote: null, error: '', busy: false, loadedFor: '' },
        sim: { kind: 'pr', facts: wfSimDefaultFacts(), result: null, error: '', busy: false, hit: '' },
        palette: { hiveDefault: null, loading: false, section: 'states' },
        exportMenu: false, spaceHeld: false, viewport: { w: 900, h: 520 }, timers: {}, drag: null, marquee: null, pendingEdge: null, dropLane: '',
      };
    }
    const _wfEd = wfEdDefaultState();

    function wfEdOwner() { return dashboardRoleAtLeast(window._hiveRole || 'read', 'owner'); }
    function wfEdDirty() { return !!_wfEd.model && JSON.stringify(_wfEd.model) !== JSON.stringify(_wfEd.saved); }
    function wfEdCanEdit() { return !!_wfEd.model && !_wfEd.readOnly && wfEdOwner(); }

    // wfEdDiags is what the canvas and Problems drawer show: the server's
    // diagnostics when they are for the current revision, local ones until.
    function wfEdDiags() {
      if (!_wfEd.model) return [];
      if (_wfEd.compile.rev === _wfEd.rev && !_wfEd.compile.error) return _wfEd.compile.diags || [];
      return wfEdLocalProblems(_wfEd.model);
    }

    function wfEdLoad(model, source, readOnly) {
      const m = wfEdNormalize(model);
      Object.assign(_wfEd, wfEdDefaultState(), { model: m, saved: wfEdClone(m), source: source || null, readOnly: !!readOnly, palette: _wfEd.palette });
      _wfEd.rev = 1;
      _wfLib.view = 'editor';
      wfEdScheduleCompile(0);
      wfLibRerender();
    }

    function wfEdCommit(next, opts) {
      if (!_wfEd.model) return;
      if (!wfEdCanEdit()) { showToast(_wfEd.readOnly ? 'This model is read-only. Use "Edit a copy".' : 'Owner role is required to edit.', 'error'); return; }
      const o = opts || {};
      _wfEd.history.push(wfEdClone(_wfEd.model));
      _wfEd.history = _wfEd.history.slice(-WF_ED_HISTORY_CAP);
      _wfEd.future = [];
      _wfEd.model = wfEdNormalize(next);
      _wfEd.rev++;
      _wfEd.sim.hit = '';
      wfEdScheduleCompile();
      wfEdRefresh(o.parts || ['toolbar', 'canvas', 'inspector', 'drawer']);
    }

    function wfEdUndo() {
      if (!_wfEd.history.length) return;
      _wfEd.future.push(wfEdClone(_wfEd.model));
      _wfEd.future = _wfEd.future.slice(-WF_ED_HISTORY_CAP);
      _wfEd.model = _wfEd.history.pop();
      _wfEd.rev++; wfEdPruneSelection(); wfEdScheduleCompile(); wfEdRefresh();
    }
    function wfEdRedo() {
      if (!_wfEd.future.length) return;
      _wfEd.history.push(wfEdClone(_wfEd.model));
      _wfEd.history = _wfEd.history.slice(-WF_ED_HISTORY_CAP);
      _wfEd.model = _wfEd.future.pop();
      _wfEd.rev++; wfEdPruneSelection(); wfEdScheduleCompile(); wfEdRefresh();
    }

    function wfEdPruneSelection() {
      const sel = _wfEd.selection;
      if (sel.type === 'state') sel.ids = sel.ids.filter(id => wfEdFindState(_wfEd.model, id));
      else if (sel.type === 'column') sel.ids = sel.ids.filter(id => wfEdFindColumn(_wfEd.model, id));
      else if (sel.type === 'transition') sel.ids = sel.ids.filter(i => _wfEd.model.spec.transitions[parseInt(i, 10)]);
      if (!sel.ids.length) { sel.type = ''; }
    }

    function wfEdSelect(type, id, additive) {
      const sel = _wfEd.selection;
      if (!type) { sel.type = ''; sel.ids = []; }
      else if (additive && sel.type === type) { const i = sel.ids.indexOf(id); if (i >= 0) sel.ids.splice(i, 1); else sel.ids.push(id); if (!sel.ids.length) sel.type = ''; }
      else { sel.type = type; sel.ids = [id]; }
      wfEdRefresh(['canvas', 'inspector']);
    }

    // --- Live compile ---------------------------------------------------------

    function wfEdScheduleCompile(delay) {
      clearTimeout(_wfEd.timers.compile);
      _wfEd.timers.compile = setTimeout(wfEdCompileNow, delay === undefined ? WF_ED_COMPILE_DEBOUNCE_MS : delay);
    }

    async function wfEdCompileNow() {
      if (!_wfEd.model) return;
      const rev = _wfEd.rev, c = _wfEd.compile;
      c.busy = true; wfEdRefresh(['toolbar']);
      try {
        const res = await fetch('/api/workflows/compile', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(wfEdCompileModel(_wfEd.model)) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        if (rev !== _wfEd.rev) return;
        c.profile = data.profile || null; c.diags = Array.isArray(data.diagnostics) ? data.diagnostics : []; c.compiled = !!data.compiled; c.error = ''; c.rev = rev;
      } catch (err) {
        if (rev !== _wfEd.rev) return;
        c.error = err.message; c.rev = rev;
      } finally {
        if (rev === _wfEd.rev) { c.busy = false; wfEdRefresh(['toolbar', 'canvas', 'drawer']); }
      }
    }

    // --- Render ---------------------------------------------------------------

    function renderWorkflowEditor(owner) {
      if (!_wfEd.model) return renderWorkflowEditorStart(owner);
      return '<div class="wf-ed" data-section="workflows-editor" data-wf-ed-root="1">' +
        '<div id="wf-ed-toolbar" class="wf-ed-toolbar-wrap">' + renderWfEdToolbar(_wfEd) + '</div>' +
        '<div class="wf-ed-main">' +
          '<aside id="wf-ed-palette" class="wf-ed-palette" aria-label="Templates">' + renderWfEdPalette(_wfEd) + '</aside>' +
          '<div id="wf-ed-canvas" class="wf-ed-canvas" aria-label="Canvas">' + renderWfEdCanvas(_wfEd) + '</div>' +
          '<aside id="wf-ed-inspector" class="wf-ed-inspector" aria-label="Inspector">' + renderWfEdInspector(_wfEd) + '</aside>' +
        '</div>' +
        '<div id="wf-ed-drawer" class="wf-ed-drawer' + (_wfEd.drawer.open ? ' open' : '') + '">' + renderWfEdDrawer(_wfEd) + '</div>' +
        '</div>';
    }

    function renderWorkflowEditorStart(owner) {
      const list = Array.isArray(_workflowState.workflows) ? _workflowState.workflows.slice().sort((a, b) => (a.name + '@' + a.version).localeCompare(b.name + '@' + b.version)) : [];
      const dis = owner ? '' : 'disabled';
      const ingest = _wfLib.ingest && _wfLib.ingest.result && _wfLib.ingest.result.model;
      return '<div class="config-section wf-ed-start" data-section="workflows-editor-start">' +
        '<h3>Workflow editor</h3>' +
        '<p class="text-muted fs-base">Design a lifecycle on a canvas: columns are lanes, states are chips, transitions are drawn by dragging from a chip\u2019s port. The model compiles live as you edit; the YAML, Diff and Simulate drawers work on the unsaved copy.</p>' +
        (owner ? '' : '<p class="text-muted fs-sm">Owner role is required to edit or save; you can still open models to look at them.</p>') +
        '<div class="wf-ed-start-grid">' +
          `<button type="button" class="wf-ed-start-card" data-action="wfEdNew" ${dis}><strong>New blank model</strong><span>One column and one initial state; add lanes and chips from the palette.</span></button>` +
          `<button type="button" class="wf-ed-start-card" data-action="wfEdDuplicateDefault" ${dis}><strong>Duplicate hive-default</strong><span>Start from the built-in lifecycle under a new name.</span></button>` +
          `<button type="button" class="wf-ed-start-card" data-action="wfLibShow" data-arg0="ingest"><strong>From Ingest</strong><span>Derive a draft from a repository\u2019s pipelines, then open it here.</span></button>` +
          (ingest ? `<button type="button" class="wf-ed-start-card" data-action="wfEdOpenIngestResult"><strong>Open the last ingest result</strong><span class="mono">${esc((ingest.metadata || {}).name || 'draft')}</span></button>` : '') +
          '<button type="button" class="wf-ed-start-card" disabled title="Coming in phase 7"><strong>Marketplace</strong><span>Browse shared models \u2014 coming in phase 7.</span></button>' +
        '</div>' +
        '<h4>Open from Library</h4>' +
        (list.length ? '<ul class="wf-ed-start-list">' + list.map(e => {
          const builtin = wfProvenanceOf(e) === 'built-in';
          return `<li><a href="#" class="mono wf-key" data-action="wfEdOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" data-prevent="1">${esc(e.name + '@' + e.version)}</a> ${wfProvenanceBadge(e)} ${wfCompiledBadge(e)}` +
            (builtin ? ` <span class="text-muted fs-xs">opens read-only</span>` : '') + '</li>';
        }).join('') + '</ul>' : '<p class="text-muted fs-sm">No stored models yet.</p>') +
        '</div>';
    }

    function renderWfEdToolbar(st) {
      const can = wfEdCanEdit();
      const dis = can ? '' : 'disabled';
      const status = st.compile.error ? { cls: 'error', text: 'compile failed: ' + st.compile.error } : wfEdStatus(wfEdDiags(), st.compile.busy, st.compile.rev === st.rev ? st.compile.compiled : null);
      const name = (st.model.metadata || {}).name || 'untitled';
      const ver = st.source ? '@' + st.source.version : (st.model.spec.meta && st.model.spec.meta.version ? '@' + st.model.spec.meta.version + ' (unsaved)' : ' (unsaved)');
      const dirty = wfEdDirty();
      const inferred = wfEdStates(st.model).filter(s => (s.lifecycle || {}).confidence === 'inferred').length;
      const btn = (action, label, title, extra) => `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="${action}" title="${esc(title || '')}" ${extra || ''}>${label}</button>`;
      return '<div class="wf-ed-toolbar" role="toolbar" aria-label="Editor toolbar">' +
        `<div class="wf-ed-title"><span class="mono">${esc(name)}</span><span class="text-muted">${esc(ver)}</span>${dirty ? '<span class="wf-ed-dirty" title="Unsaved changes">\u25CF</span>' : ''}${st.readOnly ? '<span class="wf-ed-ro">read-only</span>' : ''}</div>` +
        `<span class="wf-ed-status ${status.cls}${status.busy ? ' busy' : ''}" data-action="wfEdDrawerTab" data-arg0="problems" role="status" title="${esc(status.text)}">${status.busy ? '\u23F3 ' : ''}${esc(status.text)}</span>` +
        '<span class="wf-ed-sep"></span>' +
        btn('wfEdUndo', '\u21B6', 'Undo (Ctrl/Cmd+Z)', (st.history.length && can) ? '' : 'disabled') +
        btn('wfEdRedo', '\u21B7', 'Redo (Shift+Ctrl/Cmd+Z)', (st.future.length && can) ? '' : 'disabled') +
        '<span class="wf-ed-sep"></span>' +
        btn('wfEdAddColumnAction', '+ Column', 'Add a column (lane)', dis) +
        btn('wfEdAddStateAction', '+ State', 'Add a state to the selected (or first) column', dis) +
        btn('wfEdAutoLayoutAction', 'Auto-layout', 'Order columns by transition depth and sort states by stage', dis) +
        (inferred ? btn('wfEdConfirmAll', '\u2713 Confirm all inferred (' + inferred + ')', 'Mark every inferred state confirmed; after a save, also records the confirm server-side', dis) : '') +
        '<span class="wf-ed-sep"></span>' +
        btn('wfEdZoom', '\u2212', 'Zoom out', 'data-arg0="-1" data-arg-types="j"') +
        `<span class="wf-ed-zoom mono">${Math.round(st.view.zoom * 100)}%</span>` +
        btn('wfEdZoom', '+', 'Zoom in', 'data-arg0="1" data-arg-types="j"') +
        btn('wfEdFit', 'Fit', 'Fit the whole model in view') +
        btn('wfEdToggleLabels', st.view.showLabels ? 'Labels \u2713' : 'Labels', 'Show event labels on edges (the most common event stays unlabelled; hover an edge to see its label)', st.view.showLabels ? 'aria-pressed="true"' : 'aria-pressed="false"') +
        '<span class="wf-ed-sep"></span>' +
        `<span class="wf-ed-menu">${btn('wfEdToggleExport', 'Export \u25BE', 'Export the canvas or the model')}${st.exportMenu ? '<div class="wf-ed-menu-items" role="menu">' +
          ['svg', 'SVG', 'png', 'PNG', 'json', 'Model JSON', 'yaml', 'Model YAML'].reduce((acc, v, i, arr) => i % 2 ? acc : acc + `<button type="button" role="menuitem" data-action="wfEdExport" data-arg0="${v}">${arr[i + 1]}</button>`, '') + '</div>' : ''}</span>` +
        '<span class="wf-ed-grow"></span>' +
        (st.readOnly ? btn('wfEdEditCopy', 'Edit a copy', 'Duplicate under a new name and edit that') : '') +
        (can ? btn('wfEdRevert', 'Revert', 'Discard unsaved changes', dirty ? '' : 'disabled') : '') +
        (can ? btn('wfEdSaveAs', 'Save as\u2026', 'Save under a new name as version 1') : '') +
        (can ? `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfEdSave" title="Save as the next version" ${dirty || !st.source ? '' : 'disabled'}>Save</button>` : '') +
        btn('wfEdClose', 'Close', 'Back to the library') +
        '</div>';
    }

    function renderWfEdPalette(st) {
      const can = wfEdCanEdit();
      const item = (kind, payload, label, sub) => `<div class="wf-ed-pal-item${can ? '' : ' disabled'}" ${can ? 'draggable="true"' : ''} data-wf-pal="${esc(JSON.stringify(Object.assign({ kind: kind }, payload)))}" data-action="wfEdPaletteClick" data-arg0="${esc(JSON.stringify(Object.assign({ kind: kind }, payload)))}" data-arg-types="j" role="button" tabindex="0" title="Drag onto a lane, or click to add to the selected lane"><span>${esc(label)}</span>${sub ? `<span class="wf-ed-pal-sub mono">${esc(sub)}</span>` : ''}</div>`;
      const hd = st.palette.hiveDefault;
      const hdStates = hd ? wfEdStates(hd).filter(s => (s.lifecycle || {}).number).sort((a, b) => a.lifecycle.number - b.lifecycle.number) : [];
      const sections = [
        ['states', 'States', WF_ED_PALETTE_STATES.map(s => item('state', s, s.label, String(s.number).padStart(3, '0') + (s.kinds ? ' ' + s.kinds.join('+') : ''))).join('')],
        ['columns', 'Columns', WF_ED_PALETTE_COLUMNS.map(c => item('column', c, c.label, c.terminal ? 'terminal' : '')).join('')],
        ['hive', 'hive-default stages', hd ? (hdStates.map(s => item('state', { id: s.id, label: s.label, number: s.lifecycle.number, kinds: s.lifecycle.kinds || [], owner: s.owner, replies: s.lifecycle.replies || [] }, s.label, String(s.lifecycle.number).padStart(3, '0'))).join('') || '<p class="text-muted fs-xs">No numbered stages.</p>') : (st.palette.loading ? '<p class="text-muted fs-xs">Loading\u2026</p>' : '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdLoadHiveDefault">Load</button>')],
      ];
      return '<h4>Templates</h4>' + sections.map(([id, label, body]) =>
        `<details class="wf-ed-pal-section" ${st.palette.section === id || id === 'states' ? 'open' : ''}><summary>${label}</summary><div class="wf-ed-pal-list">${body}</div></details>`).join('');
    }

    function wfEdLayoutNow() { return wfEdLayout(_wfEd.model, _wfEd.compile.rev === _wfEd.rev ? _wfEd.compile.profile : null, _wfEd.view); }

    function renderWfEdCanvas(st) {
      const layout = wfEdLayoutNow();
      const svg = wfEdSvg(st.model, layout, {
        selection: st.selection, diags: wfEdDiags(), profile: st.compile.rev === st.rev ? st.compile.profile : null, view: st.view,
        viewportW: st.viewport.w, viewportH: st.viewport.h, simHit: st.sim.hit, marquee: st.marquee, pendingEdge: st.pendingEdge, dropLane: st.dropLane, showLabels: !!st.view.showLabels,
        dragging: st.drag && st.drag.type === 'chip' && st.drag.moved ? st.drag.ids : null,
      });
      const big = layout.width * st.view.zoom > st.viewport.w || layout.height * st.view.zoom > st.viewport.h;
      return svg + (big ? '<div class="wf-ed-minimap" aria-hidden="false">' + wfEdMinimapSvg(layout, st.view, st.viewport.w, st.viewport.h) + '</div>' : '') +
        '<div class="wf-ed-hint text-muted">drag chips between lanes \u00b7 drag a port to connect \u00b7 wheel pans, Ctrl+wheel zooms \u00b7 dbl-click renames \u00b7 Del removes</div>';
    }

    // esc() folds newlines; textarea bodies need them kept.
    function wfEdEscText(s) { return esc(String(s == null ? '' : s).replace(/\n/g, '\u0001')).replace(/\u0001/g, '\n'); }
    function wfEdField(label, input, hint) { return `<label class="config-field wf-ed-field">${esc(label)}${input}${hint ? `<span class="text-muted fs-xs">${hint}</span>` : ''}</label>`; }
    function wfEdText(path, value, extra) { return `<input type="text" value="${esc(value == null ? '' : String(value))}" data-change-action="wfEdFieldSet" data-arg0="${esc(path)}" data-arg-types="s,v" ${extra || ''}>`; }
    function wfEdSelectHtml(action, arg0, value, options, extra) {
      return `<select data-change-action="${action}" ${arg0 !== null ? `data-arg0="${esc(arg0)}" data-arg-types="s,v"` : 'data-arg-types="v"'} ${extra || ''}>` +
        options.map(o => { const v = Array.isArray(o) ? o[0] : o, l = Array.isArray(o) ? o[1] : (o || '\u2014'); return `<option value="${esc(v)}" ${v === value ? 'selected' : ''}>${esc(l)}</option>`; }).join('') + '</select>';
    }

    function renderWfEdInspector(st) {
      const m = st.model, spec = m.spec, sel = st.selection;
      const can = wfEdCanEdit();
      const dis = can ? '' : 'disabled';
      const events = (spec.events || []).map(e => e.id);
      let body;
      if (sel.type === 'state' && sel.ids.length > 1) {
        body = `<h4>${sel.ids.length} states</h4><p class="text-muted fs-sm">${sel.ids.map(id => `<span class="mono">${esc(id)}</span>`).join(', ')}</p>` +
          `<div class="kc-actions"><button type="button" class="hv-btn btn-danger btn-sm" data-action="wfEdDeleteSelection" ${dis}>Delete</button></div>`;
      } else if (sel.type === 'state') {
        const s = wfEdFindState(m, sel.ids[0]);
        if (!s) return '<p class="text-muted fs-sm">Nothing selected.</p>';
        const lc = s.lifecycle || {}, base = 'spec.states[id=' + s.id + ']';
        const cols = [[WF_ED_UNASSIGNED, '\u2014 unassigned']].concat(spec.columns.map(c => [c.id, c.label || c.id]));
        const dupNumber = lc.number && wfEdStates(m).some(o => o.id !== s.id && (o.lifecycle || {}).number === lc.number);
        body = `<h4>State <span class="mono">${esc(s.id)}</span></h4>` +
          wfEdField('Id', `<div class="wf-ed-inline"><input type="text" value="${esc(s.id)}" readonly><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdRenameStateAction" data-arg0="${esc(s.id)}" ${dis}>Rename\u2026</button></div>`, 'Renaming updates transitions, initial, labels and rules.') +
          wfEdField('Label', wfEdText(base + '.label', s.label, dis)) +
          wfEdField('Column', wfEdSelectHtml('wfEdSetStateColumn', s.id, lc.column || WF_ED_UNASSIGNED, cols, dis)) +
          wfEdField('Stage number', `<input type="number" min="0" step="1" value="${esc(lc.number ? String(lc.number) : '')}" data-change-action="wfEdFieldSetNumber" data-arg0="${esc(base + '.lifecycle.number')}" data-arg-types="s,v" ${dis}>`, dupNumber ? '<span class="wf-ed-warn">another state uses this number</span>' : 'unique per kind; blank = no stage label') +
          wfEdField('Owner', wfEdSelectHtml('wfEdFieldSet', base + '.owner', s.owner || 'human', WF_ED_OWNERS, dis)) +
          wfEdField('Type', wfEdSelectHtml('wfEdFieldSet', base + '.type', s.type || '', [['', 'sequential'], ['parallel', 'parallel'], ['final', 'final']], dis)) +
          `<div class="config-field wf-ed-field"><span>Kinds</span><div class="wf-ed-checks">${WF_ED_KINDS.map(k => `<label><input type="checkbox" ${(lc.kinds || []).indexOf(k) >= 0 ? 'checked' : ''} data-change-action="wfEdSetKind" data-arg0="${esc(s.id)}" data-arg1="${k}" data-arg-types="s,s,c" ${dis}> ${k}</label>`).join('')}<span class="text-muted fs-xs">none = both</span></div></div>` +
          wfEdField('Confidence', wfEdSelectHtml('wfEdFieldSet', base + '.lifecycle.confidence', lc.confidence || '', [['', 'confirmed'], ['inferred', 'inferred']], dis)) +
          wfEdField('Replies / verbs', wfEdText(base + '.lifecycle.replies', (lc.replies || []).join(', '), dis + ' data-change-action="wfEdSetList" placeholder="comma separated"'), 'reply verbs that move an item here') +
          wfEdField('Waiting on', wfEdText(base + '.lifecycle.waiting', (lc.waiting || []).join(', '), dis + ' data-change-action="wfEdSetList" placeholder="comma separated"')) +
          `<div class="config-field wf-ed-field"><span>Initial</span><label><input type="radio" name="wf-ed-initial" ${spec.initial === s.id ? 'checked' : ''} data-change-action="wfEdSetInitial" data-arg0="${esc(s.id)}" data-arg-types="s" ${dis}> this is the initial state</label></div>` +
          (Array.isArray(lc.citations) && lc.citations.length ? '<div class="config-field wf-ed-field"><span>Citations</span><ul class="wf-citations">' + lc.citations.map(c => `<li class="mono">${esc(c.path || '')}${c.line ? ':' + esc(String(c.line)) : ''}${c.text ? ` <span class="text-muted">${esc(c.text)}</span>` : ''}</li>`).join('') + '</ul></div>' : '') +
          '<h5>Transitions</h5><ul class="wf-ed-translist">' + (spec.transitions.map((t, i) => ({ t: t, i: i })).filter(x => x.t.from === s.id || x.t.to === s.id).map(x =>
            `<li><a href="#" data-action="wfEdSelectTransition" data-arg0="${x.i}" data-arg-types="j" data-prevent="1" class="mono">${esc(x.t.from)} \u2192 ${esc(x.t.to)}</a> <span class="text-muted">${esc(x.t.on || '(no event)')}</span></li>`).join('') || '<li class="text-muted">none</li>') + '</ul>' +
          `<div class="kc-actions"><button type="button" class="hv-btn btn-danger btn-sm" data-action="wfEdDeleteSelection" ${dis}>Delete state</button></div>`;
      } else if (sel.type === 'transition') {
        const i = parseInt(sel.ids[0], 10), t = spec.transitions[i];
        if (!t) return '<p class="text-muted fs-sm">Nothing selected.</p>';
        const base = 'spec.transitions[' + i + ']';
        body = `<h4>Transition <span class="mono">${esc(t.from)} \u2192 ${esc(t.to)}</span></h4>` +
          wfEdField('On (event)', `<input type="text" list="wf-ed-events" value="${esc(t.on || '')}" data-change-action="wfEdFieldSet" data-arg0="${esc(base + '.on')}" data-arg-types="s,v" ${dis} id="wf-ed-focus-on"><datalist id="wf-ed-events">${events.map(e => `<option value="${esc(e)}">`).join('')}</datalist>`, events.length ? 'declared events: ' + esc(events.join(', ')) : 'free text; declare events in spec.events to validate') +
          wfEdField('Guard ref', wfEdText(base + '.guardRef', t.guardRef, dis + ' list="wf-ed-guards"') + `<datalist id="wf-ed-guards">${(spec.guards || []).map(g => `<option value="${esc(g.id)}">`).join('')}</datalist>`) +
          wfEdField('Gate ref', wfEdText(base + '.gateRef', t.gateRef, dis)) +
          wfEdField('Note', wfEdText(base + '.note', t.note, dis)) +
          `<div class="kc-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdReverseTransition" data-arg0="${i}" data-arg-types="j" ${dis}>Reverse</button><button type="button" class="hv-btn btn-danger btn-sm" data-action="wfEdDeleteSelection" ${dis}>Delete</button></div>`;
      } else if (sel.type === 'column') {
        const c = wfEdFindColumn(m, sel.ids[0]);
        if (!c) return '<p class="text-muted fs-sm">Nothing selected.</p>';
        const base = 'spec.columns[id=' + c.id + ']';
        const others = spec.columns.filter(o => o.id !== c.id);
        body = `<h4>Column <span class="mono">${esc(c.id)}</span></h4>` +
          wfEdField('Id', `<div class="wf-ed-inline"><input type="text" value="${esc(c.id)}" readonly><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdRenameColumnAction" data-arg0="${esc(c.id)}" ${dis}>Rename\u2026</button></div>`) +
          wfEdField('Label', wfEdText(base + '.label', c.label, dis)) +
          wfEdField('Hint', wfEdText(base + '.hint', c.hint, dis), 'shown as the lane\u2019s tooltip in the Activity Monitor') +
          `<div class="config-field wf-ed-field"><label><input type="checkbox" ${c.terminal ? 'checked' : ''} data-change-action="wfEdFieldSetBool" data-arg0="${esc(base + '.terminal')}" data-arg-types="s,c" ${dis}> Terminal column</label><span class="text-muted fs-xs">states here need no exit</span></div>` +
          wfEdField('Width', `<input type="range" min="${WF_ED.minColW}" max="${WF_ED.maxColW}" step="${WF_ED.grid}" value="${st.view.laneWidths[c.id] || WF_ED.colW}" data-input-action="wfEdLaneWidth" data-arg0="${esc(c.id)}" data-arg-types="s,v">`) +
          `<p class="text-muted fs-sm">${(c.states || []).length} state${(c.states || []).length === 1 ? '' : 's'}</p>` +
          '<div class="kc-actions">' +
            `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdMoveColumnBy" data-arg0="${esc(c.id)}" data-arg1="-1" data-arg-types="s,j" ${dis}>\u2190 Move</button>` +
            `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdMoveColumnBy" data-arg0="${esc(c.id)}" data-arg1="1" data-arg-types="s,j" ${dis}>Move \u2192</button>` +
            `<button type="button" class="hv-btn btn-danger btn-sm" data-action="wfEdRemoveColumnAction" data-arg0="${esc(c.id)}" ${dis}>Delete column</button>` +
          '</div>' + (others.length && (c.states || []).length ? '<p class="text-muted fs-xs">Deleting asks where to move its states.</p>' : '');
      } else {
        const meta = spec.meta || {}, lab = spec.labels || {};
        const states = wfEdStates(m);
        body = '<h4>Model</h4>' +
          wfEdField('Name', wfEdText('metadata.name', (m.metadata || {}).name, dis + ' pattern="[a-z0-9][a-z0-9._-]*"'), 'lower-case, digits, dots, dashes') +
          wfEdField('Description', `<textarea rows="3" data-change-action="wfEdFieldSet" data-arg0="spec.meta.description" data-arg-types="s,v" ${dis}>${wfEdEscText(meta.description || '')}</textarea>`) +
          wfEdField('Source', wfEdSelectHtml('wfEdFieldSet', 'spec.meta.source', meta.source || 'hand', ['hand', 'spec', 'github-actions', 'tekton', 'issue-templates', 'built-in'], dis)) +
          wfEdField('Provenance', `<input type="text" value="${esc(meta.provenance || 'local')}" readonly>`, 'set by the store on save') +
          wfEdField('Version', `<input type="text" value="${esc(st.source ? st.source.version : (meta.version || ''))}" readonly>`, 'Save assigns the next free integer') +
          wfEdField('Derivation', wfEdSelectHtml('wfEdFieldSet', 'spec.derivation.mode', (spec.derivation || {}).mode || 'builtin', WF_ED_DERIVATIONS, dis), 'builtin = hive\u2019s stage engine by number; rules = spec.derivation.rules') +
          wfEdField('Initial state', wfEdSelectHtml('wfEdFieldSet', 'spec.initial', spec.initial || '', states.map(s => [s.id, s.label || s.id]), dis)) +
          wfEdField('Label prefix', wfEdText('spec.labels.prefix', lab.prefix, dis + ' placeholder="stage/"')) +
          wfEdField('Scope repository', wfEdText('spec.scope.repository', (spec.scope || {}).repository, dis + ' placeholder="*"')) +
          `<p class="text-muted fs-sm">${spec.columns.length} column${spec.columns.length === 1 ? '' : 's'} \u00b7 ${states.length} state${states.length === 1 ? '' : 's'} \u00b7 ${spec.transitions.length} transition${spec.transitions.length === 1 ? '' : 's'}</p>`;
      }
      return body;
    }

    function wfEdProblemsHtml(diags) {
      const list = Array.isArray(diags) ? diags : [];
      if (!list.length) return '<p class="text-muted fs-sm">No problems: the model compiles clean.</p>';
      return '<ul class="wf-ed-problems">' + list.map(d =>
        `<li class="wf-ed-problem ${esc(d.severity || 'warning')}"><a href="#" data-action="wfEdSelectSubject" data-arg0="${esc(d.subject || '')}" data-prevent="1"><span class="wf-diag-code mono">${esc(d.code || '')}</span>${d.subject ? `<span class="mono">${esc(d.subject)}</span> ` : ''}${esc(d.detail || d.message || '')}</a></li>`).join('') + '</ul>';
    }

    function wfEdChangeSetHtml(label, cs) {
      if (!cs || !(cs.added || []).length && !(cs.removed || []).length && !(cs.changed || []).length) return '';
      return `<div class="wf-ed-diff-group"><h5>${esc(label)}</h5>` +
        (cs.added || []).map(k => `<div class="wf-ed-diff-row added">+ <span class="mono">${esc(k)}</span></div>`).join('') +
        (cs.removed || []).map(k => `<div class="wf-ed-diff-row removed">\u2212 <span class="mono">${esc(k)}</span></div>`).join('') +
        (cs.changed || []).map(c => `<div class="wf-ed-diff-row changed">~ <span class="mono">${esc(c.key)}</span><ul>${(c.fields || []).map(f => `<li><span class="mono">${esc(f.field)}</span>: <s>${esc(f.from || '\u2205')}</s> \u2192 ${esc(f.to || '\u2205')}</li>`).join('')}</ul></div>`).join('') + '</div>';
    }

    function wfEdDiffHtml(d) {
      if (!d) return '';
      return `<p class="fs-sm"><strong>${esc(d.summary || '')}</strong></p>` +
        wfEdChangeSetHtml('Columns', d.columns) + wfEdChangeSetHtml('States', d.states) + wfEdChangeSetHtml('Transitions', d.transitions) + wfEdChangeSetHtml('Labels', d.labels) + wfEdChangeSetHtml('Rules', d.rules) +
        ((d.meta || []).length ? '<div class="wf-ed-diff-group"><h5>Meta</h5>' + d.meta.map(f => `<div class="wf-ed-diff-row changed">~ <span class="mono">${esc(f.field)}</span>: <s>${esc(f.from || '\u2205')}</s> \u2192 ${esc(f.to || '\u2205')}</div>`).join('') + '</div>' : '');
    }

    function wfEdSimFieldHtml(field, facts, disabled) {
      const val = wfSimFormFromFacts(facts, field);
      const common = `data-wf-sim-field="${esc(field.path)}" ${disabled}`;
      if (field.type === 'bool') return `<label class="config-field wf-sim-check"><input type="checkbox" ${common} ${val ? 'checked' : ''} data-change-action="wfEdSimSet" data-arg0="${esc(field.path)}" data-arg-types="s,c"> ${esc(field.label)}</label>`;
      if (field.type === 'select') return `<label class="config-field">${esc(field.label)}<select ${common} data-change-action="wfEdSimSet" data-arg0="${esc(field.path)}" data-arg-types="s,v">${field.options.map(o => `<option value="${esc(o)}" ${o === val ? 'selected' : ''}>${esc(o || '\u2014')}</option>`).join('')}</select></label>`;
      return `<label class="config-field">${esc(field.label)}<input type="text" ${common} value="${esc(val)}" placeholder="${esc(field.hint || '')}" data-change-action="wfEdSimSet" data-arg0="${esc(field.path)}" data-arg-types="s,v"></label>`;
    }

    function renderWfEdDrawer(st) {
      const tabs = [['problems', 'Problems' + (wfEdDiags().length ? ' (' + wfEdDiags().length + ')' : '')], ['yaml', 'YAML'], ['diff', 'Diff'], ['simulate', 'Simulate']];
      let body = '';
      const tab = st.drawer.tab;
      if (!st.drawer.open) body = '';
      else if (tab === 'problems') {
        body = (st.compile.error ? `<p class="wf-ingest-error">Compile request failed: ${esc(st.compile.error)}</p>` : '') +
          (st.compile.rev !== st.rev ? '<p class="text-muted fs-xs">Showing local checks until the server answers\u2026</p>' : '') + wfEdProblemsHtml(wfEdDiags());
      } else if (tab === 'yaml') {
        body = `<div class="wf-ed-yaml-bar"><span class="text-muted fs-xs">${st.yaml.busy ? 'Working\u2026' : (st.yaml.rev === st.rev ? 'In sync with the canvas.' : 'Edit, then Apply (or leave the field) to push changes to the canvas.')}</span>` +
          `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdYamlRefresh">Refresh from canvas</button>` +
          `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfEdYamlApply" ${wfEdCanEdit() ? '' : 'disabled'}>Apply</button></div>` +
          (st.yaml.error ? `<p class="wf-ingest-error">${esc(st.yaml.error)}</p>` : '') +
          `<textarea id="wf-ed-yaml" class="wf-ed-yaml mono" spellcheck="false" ${wfEdCanEdit() ? '' : 'readonly'} data-change-action="wfEdYamlApply" aria-label="Model YAML"></textarea>`;
      } else if (tab === 'diff') {
        const local = wfEdDiff(st.saved || {}, st.model);
        const versions = st.diff.versions || [];
        const opts = [['', '\u2014 saved version \u2014']].concat(versions.map(v => [v.version, v.version + (v.savedAt && v.savedAt !== '0001-01-01T00:00:00Z' ? ' \u00b7 ' + formatDashboardDateTime(v.savedAt) : '')]));
        body = '<h5>Working copy vs last saved</h5>' + (local.empty ? '<p class="text-muted fs-sm">No unsaved changes.</p>' : wfEdDiffHtml(local)) +
          (st.source ? '<h5>Saved versions</h5>' +
            `<div class="wf-ed-inline"><label class="config-field">Compare <span class="mono">${esc(st.source.name + '@' + st.source.version)}</span> against ${wfEdSelectHtml('wfEdDiffAgainst', null, st.diff.against, opts)}</label></div>` +
            (st.diff.busy ? '<p class="text-muted fs-sm">Loading\u2026</p>' : '') + (st.diff.error ? `<p class="wf-ingest-error">${esc(st.diff.error)}</p>` : '') +
            (st.diff.remote ? wfEdDiffHtml(st.diff.remote) : '') : '<p class="text-muted fs-sm">Save the model to compare stored versions.</p>');
      } else if (tab === 'simulate') {
        const sim = st.sim;
        const dis = sim.busy ? 'disabled' : '';
        const kinds = WF_ED_KINDS.map(k => `<label class="wf-sim-kind"><input type="radio" name="wf-ed-sim-kind" value="${k}" ${k === sim.kind ? 'checked' : ''} ${dis} data-change-action="wfEdSimSet" data-arg0="kind" data-arg-types="s,v"> ${k === 'pr' ? 'Pull request' : 'Issue'}</label>`).join('');
        body = `<div class="wf-sim-kinds">${kinds}<span class="wf-ed-grow"></span><button type="button" class="hv-btn btn-primary btn-sm" data-action="wfEdSimRun" ${dis}>${sim.busy ? 'Simulating\u2026' : 'Simulate'}</button>${sim.hit ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfEdSimClear">Clear highlight</button>' : ''}</div>` +
          `<div class="wf-sim-form">${wfSimFieldsFor(sim.kind).map(f => wfEdSimFieldHtml(f, sim.facts, dis)).join('')}</div>` +
          (sim.error ? `<p class="wf-ingest-error">${esc(sim.error)}</p>` : '') +
          (sim.result ? renderWorkflowSimulateResult(sim.result) : '');
      }
      return '<div class="wf-ed-drawer-tabs" role="tablist">' + tabs.map(([id, label]) =>
        `<button type="button" role="tab" class="wf-ed-drawer-tab${tab === id && st.drawer.open ? ' active' : ''}" aria-selected="${tab === id && st.drawer.open}" data-action="wfEdDrawerTab" data-arg0="${id}">${label}</button>`).join('') +
        `<span class="wf-ed-grow"></span><button type="button" class="wf-ed-drawer-tab" data-action="wfEdDrawerToggle" aria-label="${st.drawer.open ? 'Collapse drawer' : 'Expand drawer'}">${st.drawer.open ? '\u25BE' : '\u25B4'}</button></div>` +
        `<div class="wf-ed-drawer-body" role="tabpanel">${body}</div>`;
    }

    // wfEdRefresh re-renders only the named regions so inputs elsewhere keep
    // focus; a full re-render goes through the config tab.
    function wfEdRefresh(parts) {
      const root = document.querySelector('[data-wf-ed-root]');
      if (!root) { if (_wfLib.view === 'editor') wfLibRerender(); return; }
      const want = parts || ['toolbar', 'palette', 'canvas', 'inspector', 'drawer'];
      const map = { toolbar: renderWfEdToolbar, palette: renderWfEdPalette, canvas: renderWfEdCanvas, inspector: renderWfEdInspector, drawer: renderWfEdDrawer };
      // Re-rendering the canvas replaces the focused SVG node; remember where
      // keyboard focus was so shortcuts keep working after every edit.
      const active = document.activeElement;
      const canvasEl = document.getElementById('wf-ed-canvas');
      const focusInCanvas = active && canvasEl && canvasEl.contains(active) && want.indexOf('canvas') >= 0;
      let focusChip = '';
      if (focusInCanvas && active.closest) { const c = active.closest('[data-wf-state]'); if (c) focusChip = c.getAttribute('data-wf-state') || ''; }
      want.forEach(p => {
        const el = document.getElementById('wf-ed-' + p);
        if (!el) return;
        if (p === 'drawer' && _wfEd.drawer.tab === 'yaml' && _wfEd.yaml.focused) return;
        if (p === 'canvas') wfEdMeasure(el);
        el.innerHTML = map[p](_wfEd);
        if (p === 'drawer') { el.classList.toggle('open', _wfEd.drawer.open); wfEdFillYaml(el); }
      });
      wfEdAttach(root);
      if (focusInCanvas) {
        const again = (focusChip && root.querySelector('[data-wf-state="' + focusChip.replace(/"/g, '') + '"]')) || root.querySelector('.wf-ed-svg');
        if (again && again.focus) again.focus({ preventScroll: true });
      }
      if (want.indexOf('drawer') >= 0 && _wfEd.drawer.open && _wfEd.drawer.tab === 'yaml') wfEdYamlSync();
    }

    function wfEdMeasure(el) {
      const r = el.getBoundingClientRect();
      if (r.width > 50) _wfEd.viewport.w = Math.floor(r.width);
      if (r.height > 50) _wfEd.viewport.h = Math.floor(r.height);
    }

    // wfEdAfterRender runs after the Workflows tab is drawn.
    function wfEdAfterRender() {
      const root = document.querySelector('[data-wf-ed-root]');
      if (!root || !_wfEd.model) return;
      const canvas = document.getElementById('wf-ed-canvas');
      if (canvas) {
        const before = _wfEd.viewport.w + 'x' + _wfEd.viewport.h;
        wfEdMeasure(canvas);
        if (before !== _wfEd.viewport.w + 'x' + _wfEd.viewport.h) canvas.innerHTML = renderWfEdCanvas(_wfEd);
      }
      wfEdAttach(root);
      wfEdFillYaml(root);
      if (_wfEd.drawer.open && _wfEd.drawer.tab === 'yaml') wfEdYamlSync();
      if (!_wfEd.palette.hiveDefault && !_wfEd.palette.loading) wfEdLoadHiveDefault();
      if (_wfEd.source && _wfEd.diff.loadedFor !== _wfEd.source.name) wfEdLoadVersions();
    }

    // --- Pointer interactions -------------------------------------------------

    function wfEdCanvasPoint(ev) {
      const svg = document.querySelector('#wf-ed-canvas svg[data-wf-ed-canvas]');
      if (!svg) return { sx: 0, sy: 0, x: 0, y: 0 };
      const r = svg.getBoundingClientRect();
      const sx = ev.clientX - r.left, sy = ev.clientY - r.top, v = _wfEd.view;
      return { sx: sx, sy: sy, x: (sx - v.panX) / v.zoom, y: (sy - v.panY) / v.zoom };
    }

    // wfEdAttach wires the canvas, palette and keyboard once per root.
    function wfEdAttach(root) {
      if (!root) return;
      const canvas = document.getElementById('wf-ed-canvas');
      if (canvas && !canvas._wfEdWired) {
        canvas._wfEdWired = true;
        canvas.addEventListener('pointerdown', wfEdPointerDown);
        canvas.addEventListener('pointermove', wfEdPointerMove);
        canvas.addEventListener('pointerup', wfEdPointerUp);
        canvas.addEventListener('pointercancel', wfEdPointerUp);
        canvas.addEventListener('wheel', wfEdWheel, { passive: false });
        canvas.addEventListener('dblclick', wfEdDblClick);
        canvas.addEventListener('click', wfEdCanvasClick);
        canvas.addEventListener('dragover', ev => { if (!wfEdCanEdit()) return; ev.preventDefault(); const p = wfEdCanvasPoint(ev); const lane = wfEdLaneAt(wfEdLayoutNow(), p.x); const id = lane && !lane.virtual ? lane.id : ''; if (id !== _wfEd.dropLane) { _wfEd.dropLane = id; wfEdRefresh(['canvas']); } });
        canvas.addEventListener('dragleave', () => { if (_wfEd.dropLane) { _wfEd.dropLane = ''; wfEdRefresh(['canvas']); } });
        canvas.addEventListener('drop', wfEdDrop);
        canvas.addEventListener('keydown', wfEdCanvasKey);
      }
      const palette = document.getElementById('wf-ed-palette');
      if (palette && !palette._wfEdWired) {
        palette._wfEdWired = true;
        palette.addEventListener('dragstart', ev => {
          const item = ev.target.closest && ev.target.closest('[data-wf-pal]');
          if (!item || !wfEdCanEdit()) { ev.preventDefault(); return; }
          ev.dataTransfer.setData('text/plain', item.getAttribute('data-wf-pal'));
          ev.dataTransfer.effectAllowed = 'copy';
        });
      }
      if (!root._wfEdWired) {
        root._wfEdWired = true;
        root.addEventListener('keydown', wfEdRootKey);
        root.addEventListener('keyup', ev => { if (ev.key === ' ') _wfEd.spaceHeld = false; });
        root.addEventListener('focusin', ev => { if (ev.target && ev.target.id === 'wf-ed-yaml') _wfEd.yaml.focused = true; });
        root.addEventListener('focusout', ev => { if (ev.target && ev.target.id === 'wf-ed-yaml') _wfEd.yaml.focused = false; });
        root.addEventListener('click', ev => { if (_wfEd.exportMenu && !(ev.target.closest && ev.target.closest('.wf-ed-menu'))) { _wfEd.exportMenu = false; wfEdRefresh(['toolbar']); } });
      }
      if (!window._wfEdResizeWired) {
        window._wfEdResizeWired = true;
        window.addEventListener('resize', () => { if (document.getElementById('wf-ed-canvas')) wfEdRefresh(['canvas']); });
      }
    }

    function wfEdPointerDown(ev) {
      if (ev.button === 2) return;
      const t = ev.target;
      const closest = sel => t.closest ? t.closest(sel) : null;
      const p = wfEdCanvasPoint(ev);
      const can = wfEdCanEdit();
      const layout = wfEdLayoutNow();
      ev.currentTarget.setPointerCapture && ev.currentTarget.setPointerCapture(ev.pointerId);
      if (ev.button === 1 || _wfEd.spaceHeld) { _wfEd.drag = { type: 'pan', sx: p.sx, sy: p.sy, panX: _wfEd.view.panX, panY: _wfEd.view.panY }; ev.preventDefault(); return; }
      const mm = closest('[data-wf-ed-minimap]');
      if (mm) { wfEdMinimapJump(ev, mm); _wfEd.drag = { type: 'minimap', el: mm }; return; }
      if (closest('[data-wf-confirm]') || closest('[data-wf-problem]')) return;
      const port = closest('[data-wf-port]');
      if (port) {
        if (!can) return;
        const from = port.getAttribute('data-wf-port'), c = layout.chips[from];
        _wfEd.drag = { type: 'port', from: from, x1: c.x + c.w, y1: c.y + c.h / 2 };
        _wfEd.pendingEdge = { x1: _wfEd.drag.x1, y1: _wfEd.drag.y1, x2: p.x, y2: p.y };
        ev.preventDefault(); return;
      }
      const rs = closest('[data-wf-resize]');
      if (rs) { const id = rs.getAttribute('data-wf-resize'); _wfEd.drag = { type: 'resize', id: id, sx: p.x, w: _wfEd.view.laneWidths[id] || WF_ED.colW }; ev.preventDefault(); return; }
      const chip = closest('[data-wf-state]');
      if (chip) {
        const id = chip.getAttribute('data-wf-state');
        const sel = _wfEd.selection;
        if (ev.shiftKey) wfEdSelect('state', id, true);
        else if (!(sel.type === 'state' && sel.ids.indexOf(id) >= 0)) wfEdSelect('state', id, false);
        const ids = _wfEd.selection.type === 'state' ? _wfEd.selection.ids.slice() : [id];
        if (can) _wfEd.drag = { type: 'chip', ids: ids, sx: p.x, sy: p.y, moved: false };
        ev.preventDefault(); return;
      }
      const head = closest('[data-wf-colhead]');
      if (head) {
        const id = head.getAttribute('data-wf-colhead');
        if (id !== WF_ED_UNASSIGNED) { wfEdSelect('column', id, false); if (can) _wfEd.drag = { type: 'col', id: id, sx: p.x, moved: false }; }
        ev.preventDefault(); return;
      }
      const fanBadge = closest('.wf-fan-badge');
      if (fanBadge) { const fan = fanBadge.closest('[data-wf-fan]'); if (fan) wfEdToggleFan(fan.getAttribute('data-wf-fan')); ev.preventDefault(); return; }
      const edge = closest('[data-wf-edge]');
      if (edge) { wfEdSelect('transition', edge.getAttribute('data-wf-edge'), ev.shiftKey); ev.preventDefault(); return; }
      if (closest('[data-wf-col]')) {
        // Clicking lane body: select lane but allow marquee drag.
        const id = closest('[data-wf-col]').getAttribute('data-wf-col');
        if (!ev.shiftKey) { if (id !== WF_ED_UNASSIGNED) wfEdSelect('column', id, false); else wfEdSelect('', null); }
      } else if (!ev.shiftKey) wfEdSelect('', null);
      _wfEd.drag = { type: 'marquee', sx: p.sx, sy: p.sy, additive: ev.shiftKey };
      _wfEd.marquee = null;
      ev.preventDefault();
    }

    function wfEdPointerMove(ev) {
      const d = _wfEd.drag;
      if (!d) return;
      const p = wfEdCanvasPoint(ev);
      if (d.type === 'pan') { _wfEd.view.panX = d.panX + (p.sx - d.sx); _wfEd.view.panY = d.panY + (p.sy - d.sy); wfEdRefresh(['canvas']); return; }
      if (d.type === 'minimap') { wfEdMinimapJump(ev, d.el); return; }
      if (d.type === 'port') { _wfEd.pendingEdge = { x1: d.x1, y1: d.y1, x2: p.x, y2: p.y }; wfEdRefresh(['canvas']); return; }
      if (d.type === 'resize') { _wfEd.view.laneWidths[d.id] = Math.max(WF_ED.minColW, Math.min(WF_ED.maxColW, wfEdSnap(d.w + (p.x - d.sx)))); wfEdRefresh(['canvas', 'inspector']); return; }
      if (d.type === 'chip' || d.type === 'col') {
        if (!d.moved && Math.abs(p.x - d.sx) + Math.abs(p.y - (d.sy || p.y)) < 4) return;
        d.moved = true;
        const lane = wfEdLaneAt(wfEdLayoutNow(), p.x);
        _wfEd.dropLane = lane ? lane.id : '';
        d.x = p.x; d.y = p.y;
        wfEdRefresh(['canvas']); return;
      }
      if (d.type === 'marquee') { _wfEd.marquee = { x1: d.sx, y1: d.sy, x2: p.sx, y2: p.sy }; wfEdRefresh(['canvas']); }
    }

    function wfEdPointerUp(ev) {
      const d = _wfEd.drag;
      if (!d) return;
      _wfEd.drag = null;
      const p = wfEdCanvasPoint(ev);
      const layout = wfEdLayoutNow();
      const t = ev.target, closest = sel => t && t.closest ? t.closest(sel) : null;
      if (d.type === 'port') {
        _wfEd.pendingEdge = null;
        const chip = closest('[data-wf-state]') || document.elementFromPoint(ev.clientX, ev.clientY)?.closest?.('[data-wf-state]');
        if (chip) {
          const to = chip.getAttribute('data-wf-state');
          const before = _wfEd.model.spec.transitions.length;
          const next = wfEdAddTransition(_wfEd.model, d.from, to, '');
          if (next.spec.transitions.length > before) {
            _wfEd.selection = { type: 'transition', ids: [String(before)] };
            wfEdCommit(next);
            const on = document.getElementById('wf-ed-focus-on'); if (on) on.focus();
            return;
          }
          showToast('That transition already exists.', 'info');
        }
        wfEdRefresh(['canvas']); return;
      }
      if (d.type === 'chip') {
        _wfEd.dropLane = '';
        if (!d.moved) { wfEdRefresh(['canvas']); return; }
        const lane = wfEdLaneAt(layout, p.x);
        if (!lane) { wfEdRefresh(['canvas']); return; }
        let slot = lane.virtual ? 0 : wfEdSlotAt(layout, lane, p.y);
        let m = _wfEd.model;
        d.ids.forEach(id => {
          const cur = layout.chips[id];
          if (cur && cur.col === lane.id && cur.index < slot) slot = Math.max(0, slot - 1);
          m = wfEdMoveState(m, id, lane.virtual ? '' : lane.id, slot);
          slot++;
        });
        wfEdCommit(m); return;
      }
      if (d.type === 'col') {
        _wfEd.dropLane = '';
        if (!d.moved) { wfEdRefresh(['canvas']); return; }
        const lane = wfEdLaneAt(layout, p.x);
        if (!lane || lane.virtual || lane.id === d.id) { wfEdRefresh(['canvas']); return; }
        const cols = _wfEd.model.spec.columns;
        const from = cols.findIndex(c => c.id === d.id), to = cols.findIndex(c => c.id === lane.id);
        wfEdCommit(wfEdMoveColumn(_wfEd.model, d.id, to > from ? to : to)); return;
      }
      if (d.type === 'marquee') {
        const mq = _wfEd.marquee; _wfEd.marquee = null;
        if (mq && Math.abs(mq.x2 - mq.x1) > 4 && Math.abs(mq.y2 - mq.y1) > 4) {
          const v = _wfEd.view;
          const x1 = (Math.min(mq.x1, mq.x2) - v.panX) / v.zoom, x2 = (Math.max(mq.x1, mq.x2) - v.panX) / v.zoom;
          const y1 = (Math.min(mq.y1, mq.y2) - v.panY) / v.zoom, y2 = (Math.max(mq.y1, mq.y2) - v.panY) / v.zoom;
          const hit = Object.keys(layout.chips).filter(id => { const c = layout.chips[id]; return c.x < x2 && c.x + c.w > x1 && c.y < y2 && c.y + c.h > y1; });
          const sel = _wfEd.selection;
          if (d.additive && sel.type === 'state') hit.forEach(id => { if (sel.ids.indexOf(id) < 0) sel.ids.push(id); });
          else _wfEd.selection = hit.length ? { type: 'state', ids: hit } : { type: '', ids: [] };
          wfEdRefresh(['canvas', 'inspector']); return;
        }
        wfEdRefresh(['canvas']); return;
      }
      if (d.type === 'resize') { wfEdRefresh(['canvas', 'inspector']); }
    }

    function wfEdMinimapJump(ev, mm) {
      const k = parseFloat(mm.getAttribute('data-k')) || 1;
      const r = mm.getBoundingClientRect();
      const mx = (ev.clientX - r.left) / k, my = (ev.clientY - r.top) / k, v = _wfEd.view;
      v.panX = -(mx * v.zoom - _wfEd.viewport.w / 2); v.panY = -(my * v.zoom - _wfEd.viewport.h / 2);
      wfEdRefresh(['canvas']);
    }

    function wfEdWheel(ev) {
      ev.preventDefault();
      const v = _wfEd.view;
      if (ev.ctrlKey || ev.metaKey) {
        const p = wfEdCanvasPoint(ev);
        const factor = ev.deltaY < 0 ? 1.1 : 1 / 1.1;
        const zoom = Math.max(0.2, Math.min(3, v.zoom * factor));
        v.panX = p.sx - (p.sx - v.panX) * (zoom / v.zoom); v.panY = p.sy - (p.sy - v.panY) * (zoom / v.zoom); v.zoom = zoom;
        wfEdRefresh(['canvas', 'toolbar']);
      } else {
        v.panX -= ev.shiftKey ? ev.deltaY : ev.deltaX; v.panY -= ev.shiftKey ? 0 : ev.deltaY;
        wfEdRefresh(['canvas']);
      }
    }

    function wfEdCanvasClick(ev) {
      const t = ev.target, closest = sel => t.closest ? t.closest(sel) : null;
      const confirm = closest('[data-wf-confirm]');
      if (confirm) { wfEdConfirmState(confirm.getAttribute('data-wf-confirm')); return; }
      const prob = closest('[data-wf-problem]');
      if (prob) { _wfEd.drawer.open = true; _wfEd.drawer.tab = 'problems'; wfEdRefresh(['drawer']); }
    }

    async function wfEdDblClick(ev) {
      const t = ev.target, closest = sel => t.closest ? t.closest(sel) : null;
      const chip = closest('[data-wf-state]'), head = closest('[data-wf-colhead]');
      if (chip) {
        const id = chip.getAttribute('data-wf-state'), s = wfEdFindState(_wfEd.model, id);
        if (!s || !wfEdCanEdit()) return;
        const label = await hivePrompt({ title: 'Rename state', message: 'Display label for ' + id + '.', defaultValue: s.label || id, confirmLabel: 'Rename' });
        if (label && label !== s.label) wfEdCommit(wfEdUpdate(_wfEd.model, 'spec.states[id=' + id + '].label', String(label).trim()));
      } else if (head) {
        const id = head.getAttribute('data-wf-colhead'), c = wfEdFindColumn(_wfEd.model, id);
        if (!c || !wfEdCanEdit()) return;
        const label = await hivePrompt({ title: 'Rename column', message: 'Display label for ' + id + '.', defaultValue: c.label || id, confirmLabel: 'Rename' });
        if (label && label !== c.label) wfEdCommit(wfEdUpdate(_wfEd.model, 'spec.columns[id=' + id + '].label', String(label).trim()));
      }
    }

    function wfEdDrop(ev) {
      ev.preventDefault();
      _wfEd.dropLane = '';
      if (!wfEdCanEdit()) return;
      let payload = null;
      try { payload = JSON.parse(ev.dataTransfer.getData('text/plain')); } catch (e) { payload = null; }
      if (!payload) { wfEdRefresh(['canvas']); return; }
      const p = wfEdCanvasPoint(ev), layout = wfEdLayoutNow(), lane = wfEdLaneAt(layout, p.x);
      wfEdPaletteAdd(payload, lane && !lane.virtual ? lane.id : '', lane && !lane.virtual ? wfEdSlotAt(layout, lane, p.y) : undefined, lane ? lane.index : undefined);
    }

    function wfEdPaletteAdd(payload, laneId, slot, laneIndex) {
      if (!payload || !_wfEd.model) return;
      if (payload.kind === 'column') {
        const m = wfEdAddColumn(_wfEd.model, { id: payload.id, label: payload.label, terminal: !!payload.terminal, hint: payload.hint }, laneIndex);
        _wfEd.selection = { type: 'column', ids: [m.spec.columns[laneIndex === undefined ? m.spec.columns.length - 1 : Math.min(laneIndex, m.spec.columns.length - 1)].id] };
        wfEdCommit(m); return;
      }
      const lc = { number: payload.number || undefined };
      if (Array.isArray(payload.kinds) && payload.kinds.length) lc.kinds = payload.kinds.slice();
      if (Array.isArray(payload.replies) && payload.replies.length) lc.replies = payload.replies.slice();
      const target = laneId || (_wfEd.selection.type === 'column' ? _wfEd.selection.ids[0] : (_wfEd.selection.type === 'state' ? ((wfEdFindState(_wfEd.model, _wfEd.selection.ids[0]) || {}).lifecycle || {}).column : '')) || (_wfEd.model.spec.columns[0] || {}).id || '';
      const m = wfEdAddState(_wfEd.model, { id: payload.id, label: payload.label, owner: payload.owner || 'human', lifecycle: lc }, target, slot);
      const added = m.spec.states[m.spec.states.length - 1];
      _wfEd.selection = { type: 'state', ids: [added.id] };
      wfEdCommit(m);
    }

    function wfEdPaletteClick(payload) { if (wfEdCanEdit()) wfEdPaletteAdd(payload, '', undefined, undefined); }

    function wfEdRootKey(ev) {
      const tag = (ev.target.tagName || '').toLowerCase();
      const inField = tag === 'input' || tag === 'textarea' || tag === 'select' || ev.target.isContentEditable;
      if ((ev.metaKey || ev.ctrlKey) && (ev.key === 'z' || ev.key === 'Z')) {
        if (inField) return;
        ev.preventDefault();
        if (ev.shiftKey) wfEdRedo(); else wfEdUndo();
      } else if ((ev.metaKey || ev.ctrlKey) && (ev.key === 's' || ev.key === 'S')) {
        ev.preventDefault(); if (wfEdCanEdit()) wfEdSave();
      }
    }

    function wfEdCanvasKey(ev) {
      const t = ev.target, closest = sel => t.closest ? t.closest(sel) : null;
      if (ev.key === ' ') { _wfEd.spaceHeld = true; ev.preventDefault(); return; }
      if (ev.key === 'Escape') { _wfEd.exportMenu = false; wfEdSelect('', null); return; }
      if (ev.key === 'Enter') {
        const chip = closest('[data-wf-state]'), head = closest('[data-wf-colhead]');
        if (chip) { wfEdSelect('state', chip.getAttribute('data-wf-state'), ev.shiftKey); ev.preventDefault(); }
        else if (head && head.getAttribute('data-wf-colhead') !== WF_ED_UNASSIGNED) { wfEdSelect('column', head.getAttribute('data-wf-colhead'), false); ev.preventDefault(); }
        return;
      }
      if (ev.key === 'Delete' || ev.key === 'Backspace') {
        const chip = closest('[data-wf-state]');
        if (chip && !(_wfEd.selection.type === 'state' && _wfEd.selection.ids.indexOf(chip.getAttribute('data-wf-state')) >= 0)) _wfEd.selection = { type: 'state', ids: [chip.getAttribute('data-wf-state')] };
        ev.preventDefault(); wfEdDeleteSelection(); return;
      }
      if (ev.key.indexOf('Arrow') === 0 && _wfEd.selection.type === 'state' && _wfEd.selection.ids.length === 1 && wfEdCanEdit()) {
        ev.preventDefault();
        const id = _wfEd.selection.ids[0], layout = wfEdLayoutNow(), c = layout.chips[id];
        if (!c) return;
        if (ev.key === 'ArrowUp' || ev.key === 'ArrowDown') {
          const idx = c.index + (ev.key === 'ArrowUp' ? -1 : 1);
          if (idx < 0) return;
          wfEdCommit(wfEdMoveState(_wfEd.model, id, c.col === WF_ED_UNASSIGNED ? '' : c.col, idx));
        } else {
          const ci = c.colIndex + (ev.key === 'ArrowLeft' ? -1 : 1);
          const lane = layout.cols[ci];
          if (!lane) return;
          wfEdCommit(wfEdMoveState(_wfEd.model, id, lane.virtual ? '' : lane.id, c.index));
        }
      }
    }

    // --- Actions (delegated) --------------------------------------------------

    function wfEdOpen(name, version) { return wfEdOpenEntry(name, version); }
    async function wfEdOpenEntry(name, version) {
      if (!(await wfEdGuardLeave())) return;
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version), { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        const ro = wfProvenanceOf(data.entry || { name: name }) === 'built-in' || !!(data.entry && data.entry.readOnly);
        wfEdLoad(data.model, { name: name, version: version }, ro);
      } catch (err) {
        showToast('Could not open ' + name + '@' + version + ': ' + err.message, 'error');
      }
    }

    async function wfEdNew() {
      if (!wfEdOwner()) { showToast('Owner role is required to create a model.', 'error'); return; }
      if (!(await wfEdGuardLeave())) return;
      const name = await hivePrompt({ title: 'New workflow model', message: 'Name (lower-case, digits, dots, dashes).', defaultValue: 'my-lifecycle', confirmLabel: 'Create' });
      if (!name) return;
      const slug = wfEdSlug(name);
      if (!/^[a-z0-9][a-z0-9._-]*$/.test(slug)) { showToast('Name must be lower-case letters, digits, dots, dashes or underscores.', 'error'); return; }
      wfEdLoad(wfEdNewModel(slug), null, false);
    }

    async function wfEdDuplicateDefault() { return wfEdCopyOf('hive-default', '1'); }
    async function wfEdEditCopy() { if (_wfEd.source) return wfEdCopyOf(_wfEd.source.name, _wfEd.source.version, _wfEd.model); }

    // wfEdCopyOf opens a copy of name@version (or the given model) as an
    // unsaved working copy under a new name.
    async function wfEdCopyOf(name, version, model) {
      if (!wfEdOwner()) { showToast('Owner role is required to edit.', 'error'); return; }
      if (_wfEd.model && !_wfEd.readOnly && !(await wfEdGuardLeave())) return;
      const newName = await hivePrompt({ title: 'Edit a copy of ' + name + '@' + version, message: 'Name for the copy. It opens unsaved; Save stores it as version 1 with provenance local.', defaultValue: name === 'hive-default' ? 'my-lifecycle' : name + '-copy', confirmLabel: 'Open copy' });
      if (!newName) return;
      const target = wfEdSlug(newName);
      try {
        let m = model;
        if (!m) {
          const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/' + encodeURIComponent(version), { headers: _inceptionAuthHeaders() });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          m = (await res.json()).model;
        }
        m = wfEdClone(m);
        m.metadata = Object.assign({}, m.metadata, { name: target });
        m.spec = Object.assign({}, m.spec);
        m.spec.meta = Object.assign({}, m.spec.meta, { version: '', provenance: 'local', description: ((m.spec.meta && m.spec.meta.description) ? m.spec.meta.description + ' ' : '') + '(copy of ' + name + '@' + version + ')' });
        wfEdLoad(m, null, false);
        _wfEd.saved = null;
        wfEdRefresh(['toolbar']);
      } catch (err) {
        showToast('Could not copy: ' + err.message, 'error');
      }
    }

    function wfEdOpenIngestResult() {
      const r = _wfLib.ingest && _wfLib.ingest.result;
      if (!r || !r.model) { showToast('No ingest result to open.', 'error'); return; }
      wfEdOpenModel(r.model, r.entry ? { name: r.entry.name, version: r.entry.version } : null);
    }
    async function wfEdOpenModel(model, source) {
      if (!(await wfEdGuardLeave())) return;
      wfEdLoad(model, source, false);
      if (!source) { _wfEd.saved = null; wfEdRefresh(['toolbar']); }
    }

    // wfEdGuardLeave asks before discarding unsaved work; true = go ahead.
    async function wfEdGuardLeave() {
      if (!_wfEd.model || !wfEdDirty() || _wfEd.readOnly) return true;
      return hiveConfirm({ title: 'Discard unsaved changes?', message: 'The working copy of ' + ((_wfEd.model.metadata || {}).name || 'this model') + ' has changes that were not saved.', confirmLabel: 'Discard', danger: true });
    }

    async function wfEdClose() {
      if (!(await wfEdGuardLeave())) return;
      Object.assign(_wfEd, wfEdDefaultState(), { palette: _wfEd.palette });
      _wfLib.view = 'library';
      wfLibRerender();
    }

    function wfEdDrawerTab(tab) { _wfEd.drawer.tab = WF_ED_DRAWERS.indexOf(tab) >= 0 ? tab : 'problems'; _wfEd.drawer.open = true; wfEdRefresh(['drawer']); }
    function wfEdDrawerToggle() { _wfEd.drawer.open = !_wfEd.drawer.open; wfEdRefresh(['drawer', 'canvas']); }
    function wfEdToggleExport() { _wfEd.exportMenu = !_wfEd.exportMenu; wfEdRefresh(['toolbar']); }
    function wfEdZoom(dir) { const v = _wfEd.view, z = Math.max(0.2, Math.min(3, v.zoom * (dir > 0 ? 1.2 : 1 / 1.2))); const cx = _wfEd.viewport.w / 2, cy = _wfEd.viewport.h / 2; v.panX = cx - (cx - v.panX) * (z / v.zoom); v.panY = cy - (cy - v.panY) * (z / v.zoom); v.zoom = z; wfEdRefresh(['canvas', 'toolbar']); }
    function wfEdToggleLabels() { _wfEd.view.showLabels = !_wfEd.view.showLabels; wfEdRefresh(['canvas', 'toolbar']); }
    function wfEdToggleFan(key) { const o = _wfEd.view.fansOpen || (_wfEd.view.fansOpen = {}); if (o[key]) delete o[key]; else o[key] = true; wfEdRefresh(['canvas']); }
    function wfEdFit() { const fit = wfEdFitView(wfEdLayoutNow(), _wfEd.viewport.w, _wfEd.viewport.h); Object.assign(_wfEd.view, fit); wfEdRefresh(['canvas', 'toolbar']); }
    function wfEdLaneWidth(id, value) { _wfEd.view.laneWidths[id] = wfEdSnap(parseInt(value, 10) || WF_ED.colW); wfEdRefresh(['canvas']); }

    function wfEdSelectTransition(i) { wfEdSelect('transition', String(i), false); }
    function wfEdSelectSubject(subject) {
      const s = String(subject || '');
      if (!s) return;
      if (wfEdFindState(_wfEd.model, s)) { wfEdSelect('state', s, false); return; }
      if (wfEdFindColumn(_wfEd.model, s)) { wfEdSelect('column', s, false); return; }
      const arrow = s.indexOf('->');
      if (arrow > 0) {
        const from = s.slice(0, arrow), to = s.slice(arrow + 2);
        const i = _wfEd.model.spec.transitions.findIndex(t => t.from === from && t.to === to);
        if (i >= 0) wfEdSelect('transition', String(i), false);
      }
    }

    function wfEdFieldSet(path, value) {
      if (!_wfEd.model) return;
      let v = value == null ? '' : String(value);
      if (path === 'metadata.name') { v = wfEdSlug(v); }
      const parts = path === 'spec.initial' || path === 'spec.derivation.mode' ? undefined : ['toolbar', 'canvas', 'inspector', 'drawer'];
      wfEdCommit(wfEdUpdate(_wfEd.model, path, v), { parts: parts });
    }
    function wfEdFieldSetNumber(path, value) {
      const n = parseInt(value, 10);
      wfEdCommit(wfEdUpdate(_wfEd.model, path, isNaN(n) || n <= 0 ? '' : n));
    }
    function wfEdFieldSetBool(path, checked) { wfEdCommit(wfEdUpdate(_wfEd.model, path, checked ? true : '')); }
    function wfEdSetList(path, value) { wfEdCommit(wfEdUpdate(_wfEd.model, path, String(value || '').split(/[,\s]+/).map(s => s.trim()).filter(Boolean))); }
    function wfEdSetStateColumn(id, columnId) { wfEdCommit(wfEdMoveState(_wfEd.model, id, columnId === WF_ED_UNASSIGNED ? '' : columnId)); }
    function wfEdSetInitial(id) { wfEdCommit(wfEdUpdate(_wfEd.model, 'spec.initial', id)); }
    function wfEdSetKind(id, kind, checked) {
      const s = wfEdFindState(_wfEd.model, id); if (!s) return;
      const kinds = ((s.lifecycle || {}).kinds || []).filter(k => k !== kind);
      if (checked) kinds.push(kind);
      wfEdCommit(wfEdUpdate(_wfEd.model, 'spec.states[id=' + id + '].lifecycle.kinds', WF_ED_KINDS.filter(k => kinds.indexOf(k) >= 0)));
    }
    async function wfEdRenameStateAction(id) {
      const v = await hivePrompt({ title: 'Rename state id', message: 'New id for ' + id + '. Transitions, initial, labels and rules follow.', defaultValue: id, confirmLabel: 'Rename' });
      if (!v) return;
      const next = wfEdRenameState(_wfEd.model, id, v);
      const nid = wfEdSlug(v);
      if (!wfEdFindState(next, nid)) { showToast('That id is taken or invalid.', 'error'); return; }
      _wfEd.selection = { type: 'state', ids: [nid] };
      wfEdCommit(next);
    }
    async function wfEdRenameColumnAction(id) {
      const v = await hivePrompt({ title: 'Rename column id', message: 'New id for ' + id + '.', defaultValue: id, confirmLabel: 'Rename' });
      if (!v) return;
      const next = wfEdRenameColumn(_wfEd.model, id, v), nid = wfEdSlug(v);
      if (!wfEdFindColumn(next, nid)) { showToast('That id is taken or invalid.', 'error'); return; }
      _wfEd.selection = { type: 'column', ids: [nid] };
      wfEdCommit(next);
    }
    function wfEdMoveColumnBy(id, delta) {
      const i = _wfEd.model.spec.columns.findIndex(c => c.id === id);
      if (i < 0) return;
      wfEdCommit(wfEdMoveColumn(_wfEd.model, id, i + delta));
    }
    function wfEdReverseTransition(i) {
      const t = _wfEd.model.spec.transitions[i]; if (!t) return;
      const m = wfEdUpdate(wfEdUpdate(_wfEd.model, 'spec.transitions[' + i + '].from', t.to), 'spec.transitions[' + i + '].to', t.from);
      wfEdCommit(m);
    }

    async function wfEdAddColumnAction() {
      const label = await hivePrompt({ title: 'New column', message: 'Column label (the id is derived).', defaultValue: 'Column ' + (_wfEd.model.spec.columns.length + 1), confirmLabel: 'Add' });
      if (!label) return;
      const sel = _wfEd.selection.type === 'column' ? _wfEd.model.spec.columns.findIndex(c => c.id === _wfEd.selection.ids[0]) : -1;
      const m = wfEdAddColumn(_wfEd.model, { label: String(label).trim() }, sel >= 0 ? sel + 1 : undefined);
      const at = sel >= 0 ? sel + 1 : m.spec.columns.length - 1;
      _wfEd.selection = { type: 'column', ids: [m.spec.columns[at].id] };
      wfEdCommit(m);
    }
    async function wfEdAddStateAction() {
      const label = await hivePrompt({ title: 'New state', message: 'State label (the id is derived).', defaultValue: 'New state', confirmLabel: 'Add' });
      if (!label) return;
      wfEdPaletteAdd({ kind: 'state', label: String(label).trim() }, '', undefined, undefined);
    }
    function wfEdAutoLayoutAction() { wfEdCommit(wfEdAutoLayout(_wfEd.model)); }

    async function wfEdDeleteSelection() {
      const sel = _wfEd.selection;
      if (!sel.type || !wfEdCanEdit()) return;
      if (sel.type === 'state') {
        const ids = sel.ids.slice();
        const touching = _wfEd.model.spec.transitions.filter(t => ids.indexOf(t.from) >= 0 || ids.indexOf(t.to) >= 0).length;
        if (ids.length > 1 || touching) {
          const ok = await hiveConfirm({ title: 'Delete ' + (ids.length === 1 ? 'state ' + ids[0] : ids.length + ' states') + '?', message: (touching ? touching + ' transition' + (touching === 1 ? '' : 's') + ' will go with ' + (ids.length === 1 ? 'it' : 'them') + '. ' : '') + 'Undo restores everything.', confirmLabel: 'Delete', danger: true });
          if (!ok) return;
        }
        let m = _wfEd.model; ids.forEach(id => { m = wfEdRemoveState(m, id); });
        _wfEd.selection = { type: '', ids: [] }; wfEdCommit(m); return;
      }
      if (sel.type === 'transition') {
        const idx = sel.ids.map(i => parseInt(i, 10)).sort((a, b) => b - a);
        let m = _wfEd.model; idx.forEach(i => { m = wfEdRemoveTransition(m, i); });
        _wfEd.selection = { type: '', ids: [] }; wfEdCommit(m); return;
      }
      if (sel.type === 'column') wfEdRemoveColumnAction(sel.ids[0]);
    }

    async function wfEdRemoveColumnAction(id) {
      const c = wfEdFindColumn(_wfEd.model, id); if (!c || !wfEdCanEdit()) return;
      let moveTo = '';
      if ((c.states || []).length) {
        const others = _wfEd.model.spec.columns.filter(o => o.id !== id);
        const choice = await hivePrompt({ title: 'Delete column ' + id + '?', message: 'It holds ' + c.states.length + ' state' + (c.states.length === 1 ? '' : 's') + '. Type the id of the column to move them to' + (others.length ? ' (' + others.map(o => o.id).join(', ') + ')' : '') + ', or leave blank to unassign them.', defaultValue: others.length ? others[0].id : '', confirmLabel: 'Delete column' });
        if (choice === null || choice === undefined || choice === false) return;
        moveTo = String(choice).trim();
        if (moveTo && !others.some(o => o.id === moveTo)) { showToast('No column ' + moveTo + '.', 'error'); return; }
      } else {
        const ok = await hiveConfirm({ title: 'Delete column ' + id + '?', message: 'The column is empty.', confirmLabel: 'Delete', danger: true });
        if (!ok) return;
      }
      _wfEd.selection = { type: '', ids: [] };
      wfEdCommit(wfEdRemoveColumn(_wfEd.model, id, moveTo));
    }

    function wfEdConfirmState(id) {
      if (!wfEdCanEdit()) return;
      wfEdCommit(wfEdUpdate(_wfEd.model, 'spec.states[id=' + id + '].lifecycle.confidence', ''));
    }
    async function wfEdConfirmAll() {
      if (!wfEdCanEdit()) return;
      let m = _wfEd.model;
      wfEdStates(m).forEach(s => { if ((s.lifecycle || {}).confidence === 'inferred') m = wfEdUpdate(m, 'spec.states[id=' + s.id + '].lifecycle.confidence', ''); });
      (m.spec.events || []).forEach((e, i) => { if (e.confidence === 'inferred') m = wfEdUpdate(m, 'spec.events[' + i + '].confidence', ''); });
      (m.spec.gates || []).forEach((g, i) => { if (g.confidence === 'inferred') m = wfEdUpdate(m, 'spec.gates[' + i + '].confidence', ''); });
      wfEdCommit(m);
      if (_wfEd.source && !_wfEd.readOnly && !_wfEd.saved) return;
      if (_wfEd.source && (_wfEd.saved && (_wfEd.saved.spec.meta || {}).provenance === 'ingested-unreviewed')) {
        const ok = await hiveConfirm({ title: 'Also confirm the stored version?', message: 'POST /confirm records that a human reviewed ' + _wfEd.source.name + '@' + _wfEd.source.version + ' and saves a confirmed copy as the next version. Your canvas edits stay unsaved until you Save.', confirmLabel: 'Confirm stored version' });
        if (!ok) return;
        try {
          const res = await fetch('/api/workflows/' + encodeURIComponent(_wfEd.source.name) + '/' + encodeURIComponent(_wfEd.source.version) + '/confirm', { method: 'POST', headers: _inceptionAuthHeaders() });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          const data = await res.json();
          showToast('Confirmed as ' + data.entry.name + '@' + data.entry.version);
          _workflowState.loaded = false; await loadWorkflowLibrary(); wfEdLoadVersions();
        } catch (err) { showToast('Confirm failed: ' + err.message, 'error'); }
      }
    }

    // --- Persistence ----------------------------------------------------------

    async function wfEdSave(asName) {
      if (!wfEdCanEdit()) return;
      const m = wfEdClone(_wfEd.model);
      if (asName) m.metadata = Object.assign({}, m.metadata, { name: asName });
      const name = (m.metadata || {}).name;
      if (!/^[a-z0-9][a-z0-9._-]*$/.test(name || '')) { showToast('Name must be lower-case letters, digits, dots, dashes or underscores.', 'error'); return; }
      m.spec.meta = Object.assign({}, m.spec.meta, { version: '' });
      if (!m.spec.meta.provenance || m.spec.meta.provenance === 'built-in') m.spec.meta.provenance = 'local';
      try {
        const res = await fetch('/api/workflows', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(m) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json(), e = data.entry || {};
        showToast('Saved ' + e.name + '@' + e.version + (e.compiled ? '' : ' (does not compile)'), e.compiled ? 'success' : 'info');
        _workflowState.loaded = false; await loadWorkflowLibrary();
        m.spec.meta.version = e.version;
        _wfEd.model = wfEdNormalize(m); _wfEd.saved = wfEdClone(_wfEd.model); _wfEd.source = { name: e.name, version: e.version }; _wfEd.readOnly = false;
        _wfEd.diff.loadedFor = ''; _wfEd.rev++; wfEdScheduleCompile(0);
        wfEdRefresh(); wfEdLoadVersions();
      } catch (err) {
        showToast('Save failed: ' + err.message, 'error');
      }
    }
    async function wfEdSaveAs() {
      const cur = (_wfEd.model.metadata || {}).name || 'model';
      const v = await hivePrompt({ title: 'Save as\u2026', message: 'New name. The model is saved as version 1 of that name; the current name is untouched.', defaultValue: cur + '-copy', confirmLabel: 'Save' });
      if (!v) return;
      return wfEdSave(wfEdSlug(v));
    }
    async function wfEdRevert() {
      if (!wfEdDirty()) return;
      const ok = await hiveConfirm({ title: 'Revert to last saved?', message: 'Unsaved canvas changes are discarded (undo history is kept).', confirmLabel: 'Revert', danger: true });
      if (!ok) return;
      wfEdCommit(_wfEd.saved || wfEdNewModel((_wfEd.model.metadata || {}).name));
    }

    async function wfEdLoadHiveDefault() {
      _wfEd.palette.loading = true;
      try {
        const res = await fetch('/api/workflows/hive-default/1', { headers: _inceptionAuthHeaders() });
        if (res.ok) _wfEd.palette.hiveDefault = (await res.json()).model || null;
      } catch (e) { /* palette section stays on its Load button */ }
      _wfEd.palette.loading = false;
      wfEdRefresh(['palette']);
    }

    async function wfEdLoadVersions() {
      if (!_wfEd.source) return;
      const name = _wfEd.source.name;
      _wfEd.diff.loadedFor = name;
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(name) + '/versions', { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        _wfEd.diff.versions = (data.versions || []).filter(v => v.version !== _wfEd.source.version);
      } catch (err) { _wfEd.diff.versions = []; }
      if (_wfEd.drawer.tab === 'diff') wfEdRefresh(['drawer']);
    }

    async function wfEdDiffAgainst(version) {
      const d = _wfEd.diff;
      d.against = String(version || ''); d.remote = null; d.error = '';
      if (!d.against || !_wfEd.source) { wfEdRefresh(['drawer']); return; }
      d.busy = true; wfEdRefresh(['drawer']);
      try {
        const res = await fetch('/api/workflows/' + encodeURIComponent(_wfEd.source.name) + '/diff?from=' + encodeURIComponent(d.against) + '&to=' + encodeURIComponent(_wfEd.source.version), { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        d.remote = (await res.json()).diff || null;
      } catch (err) { d.error = err.message; }
      d.busy = false; wfEdRefresh(['drawer']);
    }

    // --- YAML drawer ----------------------------------------------------------

    // The shared esc() folds newlines, so the YAML text is assigned as a
    // property after the drawer is in the DOM rather than embedded in HTML.
    function wfEdFillYaml(scope) {
      const ta = (scope || document).querySelector('#wf-ed-yaml');
      if (ta) ta.value = _wfEd.yaml.text || '';
    }

    function wfEdYamlSync() {
      const y = _wfEd.yaml;
      if (!_wfEd.model || y.focused || y.busy || y.rev === _wfEd.rev) return;
      clearTimeout(_wfEd.timers.yaml);
      _wfEd.timers.yaml = setTimeout(wfEdYamlRefresh, WF_ED_YAML_DEBOUNCE_MS);
    }
    async function wfEdYamlRefresh() {
      const y = _wfEd.yaml, rev = _wfEd.rev;
      if (!_wfEd.model) return;
      y.busy = true;
      try {
        const res = await fetch('/api/workflows/yaml', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ model: _wfEd.model }) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        if (rev !== _wfEd.rev) return;
        y.text = data.yaml || ''; y.rev = rev; y.error = '';
      } catch (err) { y.error = 'YAML render failed: ' + err.message; }
      y.busy = false; y.focused = false;
      if (_wfEd.drawer.tab === 'yaml') wfEdRefresh(['drawer']);
    }
    async function wfEdYamlApply() {
      const y = _wfEd.yaml, el = document.getElementById('wf-ed-yaml');
      if (!el || !wfEdCanEdit()) return;
      const text = el.value;
      if (text === y.text) return;
      y.busy = true; y.error = ''; y.focused = false;
      try {
        const res = await fetch('/api/workflows/yaml', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ yaml: text }) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        if (data.error) { y.error = data.error; y.text = text; y.busy = false; wfEdRefresh(['drawer']); return; }
        y.text = text; y.busy = false;
        wfEdPruneSelection();
        wfEdCommit(data.model);
        y.rev = _wfEd.rev;
      } catch (err) { y.error = 'YAML apply failed: ' + err.message; y.busy = false; wfEdRefresh(['drawer']); }
    }

    // --- Simulate drawer ------------------------------------------------------

    function wfEdSimSet(field, value) {
      const sim = _wfEd.sim;
      if (field === 'kind') { sim.kind = WF_ED_KINDS.indexOf(value) >= 0 ? value : 'pr'; sim.result = null; sim.error = ''; wfEdRefresh(['drawer']); return; }
      sim.facts = wfSimFactsFromForm(sim.facts, sim.kind, f => f.path === field ? value : undefined);
    }
    async function wfEdSimRun() {
      const sim = _wfEd.sim;
      sim.facts = wfSimFactsFromForm(sim.facts, sim.kind, f => { const el = document.querySelector('[data-wf-sim-field="' + f.path + '"]'); return el ? (f.type === 'bool' ? el.checked : el.value) : undefined; });
      sim.busy = true; sim.error = ''; wfEdRefresh(['drawer']);
      try {
        const res = await fetch('/api/workflows/simulate', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ model: wfEdCompileModel(_wfEd.model), kind: sim.kind, facts: sim.facts }) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        sim.result = await res.json();
        sim.hit = (sim.result.decision && !sim.result.decision.abstain && sim.result.decision.state) || '';
      } catch (err) { sim.error = 'Simulate failed: ' + err.message; sim.result = null; sim.hit = ''; }
      sim.busy = false; wfEdRefresh(['drawer', 'canvas']);
    }
    function wfEdSimClear() { _wfEd.sim.hit = ''; wfEdRefresh(['drawer', 'canvas']); }

    // --- Export ---------------------------------------------------------------

    // wfEdExportStyle resolves the CSS tokens the canvas uses so the exported
    // SVG is self-contained. The tag is built in pieces so the style ratchet,
    // which scans inline <style> blocks, does not see one here.
    function wfEdExportStyle() {
      const cs = getComputedStyle(document.querySelector('.wf-ed-canvas') || document.body);
      const tok = (name, fallback) => (cs.getPropertyValue(name) || '').trim() || fallback;
      const rules = [
        ['.wf-ed-svg', 'font-family:' + tok('--font-ui', 'sans-serif') + ';background:' + tok('--bg', 'white')],
        ['.wf-ed-gridline', 'stroke:' + tok('--border', 'silver') + ';stroke-width:0.5;fill:none'],
        ['.wf-ed-lane', 'fill:' + tok('--panel', 'whitesmoke') + ';stroke:' + tok('--border', 'silver')],
        ['.wf-ed-col.terminal .wf-ed-lane', 'stroke-dasharray:4 3;fill:' + tok('--bg-soft', 'whitesmoke')],
        ['.wf-ed-lanehead', 'fill:' + tok('--panel-strong', 'gainsboro')],
        ['.wf-ed-colname', 'fill:' + tok('--muted', 'gray') + ';font-size:' + tok('--fs-xs', '11px') + ';font-weight:700;text-transform:uppercase'],
        ['.wf-ed-chip', 'fill:' + tok('--panel-strong', 'white') + ';stroke:' + tok('--line-strong', 'gray')],
        ['.wf-ed-state.initial .wf-ed-chip', 'stroke:' + tok('--status-info', 'blue') + ';stroke-width:1.5'],
        ['.wf-ed-state.inferred .wf-ed-chip', 'stroke-dasharray:4 3;stroke:' + tok('--status-warn', 'orange')],
        ['.wf-ed-initial', 'fill:' + tok('--status-info', 'blue')],
        ['.wf-ed-label', 'fill:' + tok('--text', 'black') + ';font-size:' + tok('--fs-sm', '13px')],
        ['.wf-ed-num', 'fill:' + tok('--muted', 'gray') + ';font-size:' + tok('--fs-2xs', '10px') + ';font-family:' + tok('--font-mono', 'monospace')],
        ['.wf-ed-edge-line', 'fill:none;stroke:' + tok('--muted', 'gray') + ';stroke-width:1.25'],
        ['.wf-ed-edge.back .wf-ed-edge-line', 'stroke-dasharray:3 3'],
        ['.wf-ed-edge-hit', 'fill:none;stroke:none'],
        ['.wf-ed-edge-label, .wf-fan-edge', 'display:none'],
        ['.wf-edge-layer.show-labels .wf-ed-edge:not(.common) .wf-ed-edge-label, .wf-ed-edge.selected .wf-ed-edge-label', 'display:inline'],
        ['.wf-ed-edge-label text', 'fill:' + tok('--brand', 'blue') + ';font-size:' + tok('--fs-2xs', '10px') + ';font-weight:600'],
        ['.wf-ed-edge-label-bg', 'fill:' + tok('--panel', 'white') + ';stroke:' + tok('--brand', 'blue')],
        ['.wf-fan-trunk', 'stroke-width:2.5'],
        ['.wf-fan-badge rect', 'fill:' + tok('--panel-strong', 'white') + ';stroke:' + tok('--muted', 'gray')],
        ['.wf-fan-badge text', 'fill:' + tok('--text', 'black') + ';font-size:' + tok('--fs-2xs', '10px') + ';font-weight:700'],
        ['.wf-ed-arrowhead', 'fill:' + tok('--muted', 'gray')],
        ['.wf-ed-port, .wf-ed-resize, .wf-ed-badge, .wf-ed-confirm, .wf-ed-marquee', 'display:none'],
      ];
      return '<' + 'style>' + rules.map(r => r[0] + '{' + r[1] + '}').join('') + '</' + 'style>';
    }

    function wfEdExportSvgText() {
      const layout = wfEdLayoutNow();
      let svg = wfEdSvg(_wfEd.model, layout, { view: { panX: 0, panY: 0, zoom: 1 }, viewportW: layout.width, viewportH: layout.height, profile: _wfEd.compile.profile, diags: [], showLabels: !!_wfEd.view.showLabels });
      svg = svg.replace('<svg ', '<svg xmlns="http://www.w3.org/2000/svg" ').replace(/ tabindex="0"/g, '').replace('<defs>', wfEdExportStyle() + '<defs>');
      return svg;
    }

    async function wfEdExport(kind) {
      _wfEd.exportMenu = false; wfEdRefresh(['toolbar']);
      const name = ((_wfEd.model.metadata || {}).name || 'workflow');
      if (kind === 'json') { hiveExportViewer({ title: name + '.workflow.json', text: JSON.stringify(_wfEd.model, null, 2), mime: 'application/json' }); return; }
      if (kind === 'yaml') {
        try {
          const res = await fetch('/api/workflows/yaml', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ model: _wfEd.model }) });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          hiveExportViewer({ title: name + '.workflow.yaml', text: (await res.json()).yaml || '', mime: 'application/yaml' });
        } catch (err) { showToast('Export failed: ' + err.message, 'error'); }
        return;
      }
      const svg = wfEdExportSvgText();
      if (kind === 'svg') { hiveExportViewer({ title: name + '.svg', text: svg, mime: 'image/svg+xml', sourceURL: 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg), allowOpen: true }); return; }
      if (kind === 'png') {
        const img = new Image();
        const layout = wfEdLayoutNow(), scale = 2;
        img.onload = () => {
          const c = document.createElement('canvas'); c.width = layout.width * scale; c.height = layout.height * scale;
          const ctx = c.getContext('2d'); ctx.scale(scale, scale); ctx.drawImage(img, 0, 0);
          let url = '';
          try { url = c.toDataURL('image/png'); } catch (e) { showToast('PNG export blocked by the browser: ' + e.message, 'error'); return; }
          wfEdShowImage(name + '.png', url);
        };
        img.onerror = () => showToast('PNG export failed: the SVG did not rasterize.', 'error');
        img.src = 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
      }
    }

    // wfEdShowImage shows a rendered PNG inline with an open-in-new-tab link
    // (no forced download; see TestNoForcedBrowserDownloadsRatchet).
    function wfEdShowImage(title, dataUrl) {
      const overlay = document.createElement('div');
      overlay.className = 'hive-dialog-overlay';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = '<div class="hive-dialog hive-export-dialog" role="dialog" aria-modal="true" aria-labelledby="wf-ed-png-title">' +
        `<div id="wf-ed-png-title" class="hive-dialog-title">${esc(title)}</div>` +
        '<div class="hive-dialog-body long modal-scroll wf-ed-png-body"><p class="cmp-note">Right-click the image to copy or save it, or open it in a new tab.</p><img class="wf-ed-png" alt="Rendered workflow canvas"></div>' +
        '<div class="hive-dialog-buttons"><a class="hv-btn btn-secondary wf-ed-png-open" target="_blank" rel="noopener noreferrer">Open in new tab</a>' +
        '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-initial-focus>Close</button></div></div>';
      overlay.querySelector('.wf-ed-png').src = dataUrl;
      overlay.querySelector('.wf-ed-png-open').href = dataUrl;
      document.body.appendChild(overlay);
      hiveWireDialog(overlay, function () {}, undefined, '.primary', null, close => close());
    }
