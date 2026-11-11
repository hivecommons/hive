    // Settings → Workflows → Ingest: the two extra sources of RFC phase 6
    // (hivecommons/hive#17620) — Tekton from a live cluster context and a
    // written process spec, optionally refined by the configured model
    // gateway. Rendered by renderWorkflowIngestSources(state, owner), which
    // renderWorkflowIngest appends to its HTML. Renderers are pure over
    // _wfIngestSrc; the wfIngestSrc* handlers mutate it and rerender.
    const WF_INGEST_SPEC_MODES = [['text', 'Paste the spec'], ['url', 'Fetch from a URL'], ['repo', 'Read a repository file']];
    const WF_INGEST_SRC_CAPS_UNKNOWN = { cluster: false, clusterReason: '', contexts: [], spec: { llm: false, model: '' } };
    const _wfIngestSrc = {
      caps: null, capsRequested: false, capsError: '',
      cluster: { open: false, context: '', namespace: '', allNamespaces: false, selector: '', name: '', busy: false, result: null, error: '' },
      spec: { open: false, mode: 'text', text: '', url: '', repo: '', path: '', useLLM: true, name: '', busy: false, result: null, error: '' },
    };

    function wfIngestSrcCaps() {
      const c = _wfIngestSrc.caps || WF_INGEST_SRC_CAPS_UNKNOWN;
      return {
        cluster: !!c.cluster,
        clusterReason: String(c.clusterReason || ''),
        contexts: Array.isArray(c.contexts) ? c.contexts.filter(Boolean) : [],
        spec: { llm: !!(c.spec && c.spec.llm), model: String((c.spec && c.spec.model) || '') },
      };
    }

    // Shared result block: the same summary / diagnostics / diagram the
    // repository ingest shows, plus the per-source extras passed in.
    function renderWorkflowIngestSourceResult(r, extras) {
      const e = r.entry || null;
      const cols = (r.profile && r.profile.columns) || (r.model && r.model.spec && r.model.spec.columns) || [];
      const pipelines = Array.isArray(r.pipelines) ? r.pipelines : [];
      return '<div class="wf-ingest-result" data-wf-ingest-result="1">' +
        `<div class="wf-ingest-summary">${r.compiled ? '<span class="wf-compiled wf-compiled-ok">compiles</span>' : '<span class="wf-compiled wf-compiled-no">does not compile</span>'}` +
        (r.source ? ` · <span class="mono">${esc(r.source)}</span>` : '') +
        (typeof r.files === 'number' ? ` · ${esc(String(r.files))} file${r.files === 1 ? '' : 's'}` : '') +
        (Array.isArray(r.pipelines) ? ` · ${pipelines.length} pipeline${pipelines.length === 1 ? '' : 's'}` : '') +
        ` · ${cols.length} column${cols.length === 1 ? '' : 's'}` +
        (e ? ` · saved as <a href="#" class="mono" data-action="wfLibOpen" data-arg0="${esc(e.name)}" data-arg1="${esc(e.version)}" data-prevent="1">${esc(e.name + '@' + e.version)}</a>` : '') +
        '</div>' +
        (extras || '') +
        (pipelines.length ? '<ul class="wf-pipelines">' + pipelines.map(p => `<li><span class="mono">${esc(p.system || p.kind || '')}</span> ${esc(p.name || p.id || '')}${p.path ? ` <span class="text-muted mono">${esc(p.path)}</span>` : ''}</li>`).join('') + '</ul>' : '') +
        '<h4>Diagnostics</h4>' + renderWorkflowDiagnostics(r.diagnostics) +
        '<h4>Proposed columns</h4><div class="wf-diagram-wrap">' + workflowDiagramSvg(r.model, r.profile) + '</div>' +
        '</div>';
    }

    function renderWorkflowIngestClusterRuns(runs) {
      const list = Array.isArray(runs) ? runs : [];
      if (!list.length) return '<p class="text-muted fs-sm">No PipelineRuns observed: the model carries no run status.</p>';
      return '<h4>Observed runs</h4><p class="text-muted fs-sm">Last run per pipeline, recorded as an <em>observed</em> annotation — not an inferred lifecycle element.</p><ul class="wf-src-runs">' +
        list.map(r => `<li><span class="mono">${esc(r.namespace || '')}/${esc(r.pipeline || '')}</span> <span class="wf-src-run-status wf-src-run-${esc(String(r.status || 'unknown').toLowerCase())}">${esc(r.status || 'Unknown')}</span>${r.run ? ` <span class="text-muted mono">${esc(r.run)}</span>` : ''}${r.completed ? ` <span class="text-muted">${esc(formatDashboardDateTime(r.completed))}</span>` : ''}</li>`).join('') +
        '</ul>';
    }

    function renderWorkflowIngestSpecNotes(r) {
      const llm = r.llm || {};
      const sections = Array.isArray(r.sections) ? r.sections : [];
      let note;
      if (!llm.used) note = '<span class="wf-src-llm wf-src-llm-off">Deterministic only</span> — step lists, conditional sentences and role nouns read without a model.';
      else if (llm.accepted) note = `<span class="wf-src-llm wf-src-llm-on">LLM-assisted</span> — refined by <span class="mono">${esc(llm.model || 'the configured gateway')}</span> in ${esc(String(llm.attempts || 1))} attempt${llm.attempts === 1 ? '' : 's'}; every element is still <em>inferred</em> and cites the spec.`;
      else note = `<span class="wf-src-llm wf-src-llm-fallback">LLM fell back</span> — <span class="mono">${esc(llm.model || 'the gateway')}</span> did not return a compiling model after ${esc(String(llm.attempts || 0))} attempt${llm.attempts === 1 ? '' : 's'}${llm.error ? ` (${esc(llm.error)})` : ''}; showing the deterministic skeleton.`;
      return `<p class="wf-src-note fs-sm">${note}</p>` +
        (sections.length ? '<h4>Spec sections</h4><ul class="wf-src-sections">' + sections.map(s => `<li><span class="mono">L${esc(String(s.startLine || 1))}–L${esc(String(s.endLine || s.startLine || 1))}</span> ${esc(s.heading || '')}</li>`).join('') + '</ul>' : '');
    }

    function renderWorkflowIngestClusterPanel(caps, owner) {
      const st = _wfIngestSrc.cluster;
      const disabled = !owner || st.busy || !caps.cluster ? 'disabled' : '';
      const contexts = caps.contexts;
      const ctxField = contexts.length
        ? `<label class="config-field">Context<select id="wf-src-cluster-context" ${disabled} data-change-action="wfIngestSrcSet" data-arg0="cluster" data-arg1="context" data-arg-types="s,s,v"><option value="">— current context —</option>${contexts.map(c => `<option value="${esc(c)}" ${c === st.context ? 'selected' : ''}>${esc(c)}</option>`).join('')}</select></label>`
        : `<label class="config-field">Context<input type="text" id="wf-src-cluster-context" value="${esc(st.context)}" placeholder="current context" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="cluster" data-arg1="context" data-arg-types="s,s,v"></label>`;
      let result = '';
      if (st.error) result = `<div class="wf-ingest-error">${esc(st.error)}</div>`;
      else if (st.result) result = renderWorkflowIngestSourceResult(st.result, renderWorkflowIngestClusterRuns(st.result.runs) + (Array.isArray(st.result.namespaces) && st.result.namespaces.length > 1 ? `<p class="text-muted fs-sm">Namespaces: ${st.result.namespaces.map(n => `<span class="mono">${esc(n)}</span>`).join(', ')}</p>` : ''));
      return `<details class="wf-src-panel" data-section="workflows-ingest-cluster" ${st.open ? 'open' : ''} data-toggle-action="wfIngestSrcToggle" data-arg0="cluster" data-arg-types="s,o">` +
        '<summary><h3>From a cluster</h3><span class="text-muted fs-sm">Tekton Pipelines, Tasks, PipelineRuns and Triggers read through kubectl</span></summary>' +
        '<p class="text-muted fs-base">Lists the Tekton resources of one namespace (or every namespace, capped) in a kube context, writes them as <span class="mono">cluster/&lt;namespace&gt;/&lt;kind&gt;/&lt;name&gt;.yaml</span> and runs the same Tekton ingester as a repository, so citations point at those paths. The last PipelineRun status per pipeline is recorded as <em>observed</em>, never inferred.</p>' +
        (caps.cluster ? '' : `<p class="wf-src-unavailable fs-sm">No kube access on this spoke${caps.clusterReason ? ': ' + esc(caps.clusterReason) : ''}. Install kubectl and a kubeconfig (or run in-cluster) to enable.</p>`) +
        (owner ? '' : '<p class="text-muted fs-sm">Owner role is required to ingest.</p>') +
        '<div class="wf-ingest-form">' + ctxField +
          `<label class="config-field">Namespace<input type="text" id="wf-src-cluster-namespace" value="${esc(st.namespace)}" placeholder="tekton-pipelines" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="cluster" data-arg1="namespace" data-arg-types="s,s,v"></label>` +
          `<label class="config-field">Label selector<input type="text" id="wf-src-cluster-selector" value="${esc(st.selector)}" placeholder="app.kubernetes.io/part-of=release" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="cluster" data-arg1="selector" data-arg-types="s,s,v"></label>` +
          `<label class="config-field">Save as<input type="text" id="wf-src-cluster-name" value="${esc(st.name)}" placeholder="derived from context and namespace" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="cluster" data-arg1="name" data-arg-types="s,s,v"></label>` +
          `<label class="config-field wf-src-check"><input type="checkbox" id="wf-src-cluster-all" ${st.allNamespaces ? 'checked' : ''} ${disabled} data-change-action="wfIngestSrcSet" data-arg0="cluster" data-arg1="allNamespaces" data-arg-types="s,s,c"> All namespaces (capped at 25)</label>` +
          `<div class="kc-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfIngestSrcRun" data-arg0="cluster" data-arg1="false" data-arg-types="s,j" ${disabled}>${st.busy ? 'Working…' : 'Preview'}</button>` +
          `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfIngestSrcRun" data-arg0="cluster" data-arg1="true" data-arg-types="s,j" ${disabled}>Ingest and save</button></div>` +
        '</div>' + result + '</details>';
    }

    function renderWorkflowIngestSpecPanel(caps, owner) {
      const st = _wfIngestSrc.spec;
      const disabled = !owner || st.busy ? 'disabled' : '';
      const modes = WF_INGEST_SPEC_MODES.map(([id, label]) => `<option value="${id}" ${st.mode === id ? 'selected' : ''}>${label}</option>`).join('');
      let input;
      if (st.mode === 'url') input = `<label class="config-field wf-src-wide">URL<input type="url" id="wf-src-spec-url" value="${esc(st.url)}" placeholder="https://example.org/process.md" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="spec" data-arg1="url" data-arg-types="s,s,v"></label>`;
      else if (st.mode === 'repo') input =
        `<label class="config-field">Repository<input type="text" id="wf-src-spec-repo" value="${esc(st.repo)}" placeholder="cncf/techdocs" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="spec" data-arg1="repo" data-arg-types="s,s,v"></label>` +
        `<label class="config-field">Path<input type="text" id="wf-src-spec-path" value="${esc(st.path)}" placeholder="docs/process.md" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="spec" data-arg1="path" data-arg-types="s,s,v"></label>`;
      else input = `<label class="config-field wf-src-wide">Spec (markdown or plain text, up to 256 KiB)<textarea id="wf-src-spec-text" rows="10" class="mono" placeholder="# Release process&#10;&#10;1. A maintainer opens a release issue.&#10;2. Reviewers approve the changelog.&#10;3. The release is published when CI is green." ${disabled} data-input-action="wfIngestSrcSet" data-arg0="spec" data-arg1="text" data-arg-types="s,s,v">${esc(st.text)}</textarea></label>`;
      const llmField = caps.spec.llm
        ? `<label class="config-field wf-src-check"><input type="checkbox" id="wf-src-spec-llm" ${st.useLLM ? 'checked' : ''} ${disabled} data-change-action="wfIngestSrcSet" data-arg0="spec" data-arg1="useLLM" data-arg-types="s,s,c"> Use AI assistance <span class="text-muted">(${esc(caps.spec.model || 'configured gateway')}; the answer must parse and compile, every element stays inferred)</span></label>`
        : '<p class="text-muted fs-sm wf-src-wide">No model gateway is configured, so the spec is read deterministically. Configure one under Settings → Model Gateways to offer AI assistance.</p>';
      let result = '';
      if (st.error) result = `<div class="wf-ingest-error">${esc(st.error)}</div>`;
      else if (st.result) result = renderWorkflowIngestSourceResult(st.result, renderWorkflowIngestSpecNotes(st.result));
      return `<details class="wf-src-panel" data-section="workflows-ingest-spec" ${st.open ? 'open' : ''} data-toggle-action="wfIngestSrcToggle" data-arg0="spec" data-arg-types="s,o">` +
        '<summary><h3>From a written spec</h3><span class="text-muted fs-sm">A process document becomes a draft model, every element inferred</span></summary>' +
        '<p class="text-muted fs-base">Reads numbered steps, "if / when / after" sentences and role nouns into states, events and gates. With AI assistance the configured gateway refines that skeleton; its answer is accepted only when it parses strictly and compiles, otherwise the deterministic draft is kept. Either way everything is marked <em>inferred</em>, cites the spec section it came from and is saved as <em>ingested · unreviewed</em>.</p>' +
        (owner ? '' : '<p class="text-muted fs-sm">Owner role is required to ingest.</p>') +
        '<div class="wf-ingest-form">' +
          `<label class="config-field">Source<select id="wf-src-spec-mode" ${disabled} data-change-action="wfIngestSrcSet" data-arg0="spec" data-arg1="mode" data-arg-types="s,s,v">${modes}</select></label>` +
          `<label class="config-field">Save as<input type="text" id="wf-src-spec-name" value="${esc(st.name)}" placeholder="derived from the first heading" ${disabled} data-input-action="wfIngestSrcSet" data-arg0="spec" data-arg1="name" data-arg-types="s,s,v"></label>` +
          input + llmField +
          `<div class="kc-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="wfIngestSrcRun" data-arg0="spec" data-arg1="false" data-arg-types="s,j" ${disabled}>${st.busy ? 'Working…' : 'Preview'}</button>` +
          `<button type="button" class="hv-btn btn-primary btn-sm" data-action="wfIngestSrcRun" data-arg0="spec" data-arg1="true" data-arg-types="s,j" ${disabled}>Ingest and save</button></div>` +
        '</div>' + result + '</details>';
    }

    function renderWorkflowIngestSources(state, owner) {
      if (!_wfIngestSrc.capsRequested && typeof wfIngestSrcLoadCaps === 'function') wfIngestSrcLoadCaps();
      const caps = wfIngestSrcCaps();
      return '<div class="config-section wf-src" data-section="workflows-ingest-sources">' +
        '<h3>Other sources</h3>' +
        (_wfIngestSrc.capsError ? `<p class="text-muted fs-sm">Could not read ingest capabilities: ${esc(_wfIngestSrc.capsError)}</p>` : '') +
        renderWorkflowIngestClusterPanel(caps, owner) +
        renderWorkflowIngestSpecPanel(caps, owner) +
        '</div>';
    }

    function wfIngestSrcToggle(which, open) {
      if (_wfIngestSrc[which]) _wfIngestSrc[which].open = !!open;
    }

    function wfIngestSrcSet(which, field, value) {
      const st = _wfIngestSrc[which];
      if (!st || !(field in st) || field === 'busy' || field === 'result' || field === 'error' || field === 'open') return;
      if (typeof st[field] === 'boolean') st[field] = !!value;
      else st[field] = String(value == null ? '' : value);
      if (field === 'mode') { st.result = null; st.error = ''; wfLibRerender(); }
    }

    async function wfIngestSrcLoadCaps() {
      _wfIngestSrc.capsRequested = true;
      if (typeof fetch !== 'function') return;
      try {
        const res = await fetch('/api/workflows/ingest/capabilities', { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        _wfIngestSrc.caps = await res.json();
        _wfIngestSrc.capsError = '';
      } catch (err) {
        _wfIngestSrc.caps = WF_INGEST_SRC_CAPS_UNKNOWN;
        _wfIngestSrc.capsError = err.message;
      }
      wfLibRerender();
    }

    // Body for POST /api/workflows/ingest/{cluster,spec}, or a validation
    // message when the panel's fields do not describe a request.
    function wfIngestSrcRequest(which, st, save, caps) {
      if (which === 'cluster') {
        if (!st.namespace.trim() && !st.allNamespaces) return { error: 'Enter a namespace or tick “All namespaces”.' };
        const body = { save: !!save };
        if (st.context.trim()) body.context = st.context.trim();
        if (st.allNamespaces) body.allNamespaces = true; else body.namespace = st.namespace.trim();
        if (st.selector.trim()) body.selector = st.selector.trim();
        if (st.name.trim()) body.name = st.name.trim();
        return { body };
      }
      const body = { save: !!save, useLLM: !!(caps && caps.spec && caps.spec.llm && st.useLLM) };
      if (st.mode === 'url') {
        if (!/^https:\/\/\S+$/.test(st.url.trim())) return { error: 'Enter an https:// URL.' };
        body.url = st.url.trim();
      } else if (st.mode === 'repo') {
        if (!/^[^/\s]+\/[^/\s]+$/.test(st.repo.trim())) return { error: 'Enter a repository as owner/repo.' };
        if (!st.path.trim()) return { error: 'Enter the path of the spec file.' };
        body.repo = st.repo.trim(); body.path = st.path.trim();
      } else {
        if (!st.text.trim()) return { error: 'Paste the process spec first.' };
        body.text = st.text;
      }
      if (st.name.trim()) body.name = st.name.trim();
      return { body };
    }

    async function wfIngestSrcRun(which, save) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) { showToast('Owner role is required to ingest.', 'error'); return; }
      const st = _wfIngestSrc[which];
      if (!st) return;
      const req = wfIngestSrcRequest(which, st, save, wfIngestSrcCaps());
      if (req.error) { st.error = req.error; st.result = null; wfLibRerender(); return; }
      st.busy = true; st.error = ''; st.result = null; st.open = true; wfLibRerender();
      try {
        const res = await fetch('/api/workflows/ingest/' + which, { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(req.body) });
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
