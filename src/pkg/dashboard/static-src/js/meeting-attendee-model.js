    // ── Settings → Meeting attendee → Model (hivecommons/hive#18517) ──
    // The model the attendee and Spektacular live mode answer with:
    // meetings.llm.mode, and on the agent-backend mode meetings.llm.backend
    // and meetings.llm.model. Backends and models come from the same catalog
    // as the agents' own pickers (backendOptionList and modelsForBackend over
    // /api/config/backends); GET /api/meetings/model says which backends can
    // answer for meetings. Saved on its own through PUT /api/meetings/model;
    // applies after a restart.
    let _maModel = null; // { data, form, loading }
    const MA_MODEL_MODE_LABELS = {
      'auto': 'Automatic (Anthropic key, else the LiteLLM endpoint)',
      'openai': 'OpenAI-compatible endpoint',
      'anthropic': 'Anthropic API key',
      'agent-backend': 'An agent backend (the CLI and sign-in the agents use)',
      'claude-cli': 'Claude Code CLI (agent backend claude)',
      'none': 'None (rules only)',
    };

    function maModelBackends() {
      const d = (_maModel && _maModel.data) || {};
      const list = (typeof backendOptionList === 'function' ? backendOptionList() : []).slice();
      (d.agent_backends || []).concat(d.supported_backends || []).forEach(b => { if (list.indexOf(b) < 0) list.push(b); });
      return list;
    }

    function maModelSupported(b) {
      return (((_maModel && _maModel.data) || {}).supported_backends || []).indexOf(b) >= 0;
    }

    // Why a backend cannot answer for meetings (GET's refused_backends).
    function maModelRefusal(b) {
      const r = (((_maModel && _maModel.data) || {}).refused_backends || {})[b];
      return r || 'it has no meetings completion profile and is not a configured model gateway';
    }

    function maModelRefusalsHtml() {
      const refused = maModelBackends().filter(b => !maModelSupported(b));
      if (!refused.length) return '';
      return '<details class="muted-xs"><summary>Why some backends are unavailable for meetings</summary><ul>' +
        refused.map(b => '<li><strong>' + escapeHtml(typeof backendLabel === 'function' ? backendLabel(b) : b) + '</strong>: ' + escapeHtml(maModelRefusal(b)) + '</li>').join('') +
        '</ul></details>';
    }

    function maModelEffectiveBackend() {
      const f = _maModel.form;
      if (f.mode === 'claude-cli') return 'claude';
      return f.backend || _maModel.data.default_backend || '';
    }

    function maModelOptions(backend, current) {
      const models = typeof modelsForBackend === 'function' ? modelsForBackend(backend) : [];
      const def = ((_maModel.data.default_models) || {})[backend] || '';
      let opts = '<option value=""' + (current ? '' : ' selected') + '>Default' + (def ? ' (' + escapeHtml(def) + ')' : '') + '</option>';
      let found = false;
      opts += (models || []).map(m => {
        const sel = m.value === current;
        if (sel) found = true;
        return '<option value="' + escapeHtml(m.value) + '"' + (sel ? ' selected' : '') + '>' + escapeHtml(m.label || m.value) + '</option>';
      }).join('');
      if (current && !found) opts += '<option value="' + escapeHtml(current) + '" selected>' + escapeHtml(current + ' (current)') + '</option>';
      return opts;
    }

    function maModelHtml() {
      if (!_maModel) {
        _maModel = { loading: true };
        maLoadModel();
      }
      return '<section class="ma-section" aria-labelledby="ma-model-h"><h4 id="ma-model-h">Model</h4><div data-ma-model>' + maModelBodyHtml() + '</div></section>';
    }

    function maModelBodyHtml() {
      if (!_maModel || _maModel.loading) return '<p class="muted-xs">Loading…</p>';
      if (_maModel.error) return '<p class="kc-error muted-xs">Model settings unavailable: ' + escapeHtml(_maModel.error) + '</p>';
      const d = _maModel.data;
      const f = _maModel.form;
      const modes = (d.modes || []).slice();
      if (f.mode && modes.indexOf(f.mode) < 0) modes.push(f.mode);
      const modeOpts = modes.map(m => '<option value="' + escapeHtml(m) + '"' + (m === f.mode ? ' selected' : '') + '>' + escapeHtml(MA_MODEL_MODE_LABELS[m] || m) + '</option>').join('');
      const agentMode = f.mode === 'agent-backend' || f.mode === 'claude-cli';
      const backend = maModelEffectiveBackend();
      let picker = '';
      if (agentMode) {
        const backendOpts = '<option value=""' + (f.backend ? '' : ' selected') + '>Default (' + escapeHtml(d.default_backend || 'none') + ')</option>' +
          maModelBackends().map(b => {
            const ok = maModelSupported(b);
            const label = (typeof backendLabel === 'function' ? backendLabel(b) : b) + (ok ? '' : ' (not for meetings)');
            return '<option value="' + escapeHtml(b) + '"' + (b === f.backend ? ' selected' : '') + (ok ? '' : ' disabled title="' + escapeHtml(maModelRefusal(b)) + '"') + '>' + escapeHtml(label) + '</option>';
          }).join('');
        picker = '<label>Backend' + yamlHint('meetings.llm.backend') + '<select class="kb-search-box mt-2" data-ma-model="backend" data-change-action="maModelInput" data-arg0="backend" data-arg-types="s,v"' +
          (f.mode === 'claude-cli' ? ' disabled' : '') + '>' + backendOpts + '</select></label>' +
          '<label>Model' + yamlHint('meetings.llm.model') + '<select class="kb-search-box mt-2" data-ma-model="model" data-change-action="maModelInput" data-arg0="model" data-arg-types="s,v">' +
          maModelOptions(backend, f.model) + '</select></label>';
      }
      const env = (d.env_overrides || []).length ? '<p class="muted-xs">Set by the environment, which wins over a saved value: ' + escapeHtml(d.env_overrides.join(', ')) + '</p>' : '';
      return '<p class="muted-xs">What the attendee and Spektacular answer with. On an agent backend they use the same CLI and sign-in as the agents, with every tool off. In use now: <strong data-ma-model-label>' + escapeHtml(d.label || '') + '</strong>.</p>' +
        '<div class="meetings-settings-grid"><label>Mode' + yamlHint('meetings.llm.mode') + '<select class="kb-search-box mt-2" data-ma-model="mode" data-change-action="maModelInput" data-arg0="mode" data-arg-types="s,v">' + modeOpts + '</select></label>' +
        picker + '</div>' + (agentMode ? maModelRefusalsHtml() : '') + env +
        (_maModel.restart ? '<p class="cmp-banner" role="status">Saved. Restart the hive to apply the new model.</p>' : '') +
        '<div class="hive-dialog-error" role="alert" data-ma-model-error></div>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="maSaveModel">Save model</button>';
    }

    function maModelRender() {
      const host = document.querySelector('[data-ma-model]');
      if (host) host.innerHTML = maModelBodyHtml();
    }

    function maModelSetData(data, restart) {
      _maModel = { data: data, form: { mode: data.mode || 'auto', backend: data.backend || '', model: data.model || '' }, restart: !!restart };
    }

    async function maLoadModel() {
      try {
        if (typeof fetchBackendsConfig === 'function') await fetchBackendsConfig(false);
        const r = await fetch('/api/meetings/model');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        maModelSetData(data, false);
      } catch (e) {
        _maModel = { error: e.message };
      }
      maModelRender();
    }

    function maModelInput(key, value) {
      if (!_maModel || !_maModel.form) return;
      _maModel.form[key] = value || '';
      // A different backend offers different models: start from its default.
      if (key === 'backend' || key === 'mode') _maModel.form.model = '';
      maModelRender();
    }

    async function maSaveModel() {
      if (!_maModel || !_maModel.form) return;
      const errEl = document.querySelector('[data-ma-model-error]');
      if (errEl) errEl.textContent = '';
      const f = _maModel.form;
      const body = { mode: f.mode };
      if (f.mode === 'agent-backend' || f.mode === 'claude-cli') {
        body.backend = f.mode === 'claude-cli' ? '' : f.backend;
        body.model = f.model;
      }
      try {
        const r = await fetch('/api/meetings/model', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        maModelSetData(data, data.restart_required);
        maModelRender();
        showToast(data.restart_required ? 'Meeting model saved; restart the hive to apply' : 'Meeting model saved', 'success');
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }
