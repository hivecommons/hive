    function renderEventTriggersSection() {
      return `<div class="settings-subsection config-field" id="event-triggers-section" data-setting-key="triggers">
        <div class="settings-subsection-title">Event triggers ${yamlHint('triggers[]')} ${settingHelpMark('cel-triggers', 'run-the-scanner-for-newly-seen-github-issues', 'Event triggers', 'Additive CEL rules select an existing agent on event evaluation. Only a persisted, compiled rule is used. Agent execution gates still apply.')}</div>
        <p class="config-help"><strong>First-seen semantics:</strong> <code>issue.opened</code> means the first time an ACTIONABLE issue is evaluated, once per hive process—not exclusively issues created after saving. Existing actionable issues may match once; a restart replays them once. Editing rules does not reset seen-item history. Pause, budget, on-demand and kick-channel gates still apply.</p>
        <p class="config-help">Signed GitHub delivery is separate setup. Saving a rule does not install a source, register a webhook, or configure its secret. The existing GitHub App/hub relay or signed receiver wakes evaluation; without delivery, rules wait for normal evaluation. No separate enabled field exists: add a rule to configure it; remove it to stop its routing.</p>
        <div id="event-triggers-editor">${renderEventTriggersEditor(_configState.eventTriggers)}</div>
      </div>`;
    }

    function renderEventTriggersEditor(model) {
      if (!viewerIsOwner()) return '<div class="config-help">Owner role required to view or change Event triggers.</div>';
      if (!model || !model.loaded) {
        return `${model && model.error ? `<div role="alert" class="config-help text-red">${esc(model.error)}</div>` : '<div class="config-help">Loading Event triggers…</div>'}
          <button class="hv-btn btn-secondary btn-sm" data-action="loadEventTriggers" data-arg0="true" data-arg-types="j"${model && model.loading ? ' disabled' : ''}>Load Event triggers</button>`;
      }
      const dis = model.busy ? ' disabled' : '';
      const listDis = model.busy || model.draft ? ' disabled' : '';
      const d = model.draft;
      const options = (values, selected) => '<option value="">Choose one</option>' + values.map(v => {
        const value = typeof v === 'string' ? v : v.value;
        const label = typeof v === 'string' ? v : v.label;
        return `<option value="${esc(value)}"${value === selected ? ' selected' : ''}>${esc(label)}</option>`;
      }).join('');
      let html = `<p class="config-help">Saved rules apply on the next event evaluation, without restarting. Saving does not kick an agent immediately or replay already-seen items.</p>`;
      if (model.error) html += `<div class="config-help text-red" role="alert">${esc(model.error)}</div>`;
      if (model.notice) html += `<div class="config-help" role="status">${esc(model.notice)}</div>`;
      html += model.rules.length ? model.rules.map((r, i) => `<div class="config-field">
        <strong>${esc(r.name || '(unnamed rule)')}</strong> → <code>${esc(r.agent)}</code> · priority ${esc(String(r.priority || 0))}
        <pre class="config-help event-trigger-expression">${esc(r.expr)}</pre>
        <button class="hv-btn btn-secondary btn-sm" data-action="editEventTrigger" data-arg0="${i}" data-arg-types="j"${listDis}>Edit</button>
        <button class="hv-btn btn-danger btn-sm" data-action="removeEventTrigger" data-arg0="${i}" data-arg-types="j"${listDis}>Remove</button>
      </div>`).join('') : '<p class="config-help">No Event triggers configured. Default governor behavior is unchanged.</p>';
      html += `<button class="hv-btn btn-primary btn-sm" data-action="newEventTrigger"${listDis}>Run scanner when an issue is first seen</button>
        <button class="hv-btn btn-secondary btn-sm" data-action="loadEventTriggers" data-arg0="true" data-arg-types="j"${dis}>Reload (discard draft)</button>`;
      if (!d) return html;
      const preset = d.preset !== 'advanced';
      html += `<div class="config-section event-trigger-form">
        <h4>${model.editIndex === null ? 'Add Event trigger' : 'Edit Event trigger'}</h4>
        <p class="config-help">Use Save Event trigger below; the dialog's Save &amp; close does not save this separate editor. Save or cancel this edit before editing another rule.</p>
        <div class="config-field"><label for="event-trigger-preset">Event / preset</label>
          <select id="event-trigger-preset" data-change-action="eventTriggerPresetChanged"${dis}>
            ${[['scanner', 'Run scanner when an issue is first seen'], ['issue.opened', 'Issue first seen'], ['pr.opened', 'Pull request first seen'], ['advanced', 'Advanced CEL (preserve or edit expression)']].map(([value,label]) => `<option value="${value}"${d.preset === value ? ' selected' : ''}>${label}</option>`).join('')}
          </select></div>
        <div class="config-field-row">
          <div class="config-field"><label for="event-trigger-name">Name</label><input id="event-trigger-name" value="${esc(d.name)}" data-input-action="eventTriggerDraftChanged"${dis}></div>
          <div class="config-field"><label for="event-trigger-agent">Target existing agent</label><select id="event-trigger-agent" data-change-action="eventTriggerDraftChanged"${dis}>${options(model.agents, d.agent)}</select></div>
          <div class="config-field"><label for="event-trigger-priority">Priority (integer; higher first)</label><input id="event-trigger-priority" type="number" step="1" value="${esc(String(d.priority))}" data-input-action="eventTriggerDraftChanged"${dis}></div>
        </div>
        ${preset ? `<div class="config-field"><label for="event-trigger-repo">Repository (required; no all-repositories default)</label><select id="event-trigger-repo" data-change-action="eventTriggerPresetChanged"${dis}>${options(model.repositories, d.repo)}</select></div>` : ''}
        <div class="config-field"><label for="event-trigger-expr">${preset ? 'Generated CEL expression (choose Advanced CEL to edit)' : 'Advanced CEL expression (must return bool)'}</label>
          <textarea id="event-trigger-expr" rows="3" data-input-action="eventTriggerDraftChanged"${preset ? ' readonly' : ''}${dis}>${esc(d.expr)}</textarea></div>
        <button class="hv-btn btn-primary btn-sm" data-action="saveEventTrigger"${dis}>${model.busy ? 'Saving…' : 'Save Event trigger'}</button>
        <button class="hv-btn btn-secondary btn-sm" data-action="cancelEventTriggerEdit"${dis}>Cancel edit</button>
      </div>`;
      return html;
    }

    function paintEventTriggers(state) {
      if (_configState !== state) return;
      const el = document.getElementById('event-triggers-editor');
      if (el) el.innerHTML = renderEventTriggersEditor(state.eventTriggers);
      if (state.focusEventTriggers) {
        const section = document.getElementById('event-triggers-section');
        if (section) {
          configScrollBodyTo(document.getElementById('config-body'), section);
          state.focusEventTriggers = false;
        }
      }
    }

    async function eventTriggerResponse(response) {
      let data;
      try { data = await response.json(); }
      catch (_) { throw new Error('Invalid server response (HTTP ' + response.status + ')'); }
      if (!response.ok) throw new Error((data && typeof data.error === 'string' && data.error) || ('HTTP ' + response.status));
      if (!data || !Array.isArray(data.rules) || typeof data.version !== 'string' || !Array.isArray(data.agents) || !Array.isArray(data.repositories)) {
        throw new Error('Invalid Event triggers response');
      }
      return data;
    }

    async function loadEventTriggers(force) {
      if (!viewerIsOwner()) return;
      const state = _configState;
      const previous = state.eventTriggers;
      if (previous && (previous.busy || previous.loading)) return;
      if (previous && previous.loaded && !force) { paintEventTriggers(state); return; }
      const model = {loading: true, loaded: false, error: '', draft: null, editIndex: null};
      state.eventTriggers = model;
      paintEventTriggers(state);
      try {
        const data = await eventTriggerResponse(await fetch('/api/config/triggers', {headers: _inceptionAuthHeaders()}));
        if (_configState !== state || state.eventTriggers !== model) return;
        Object.assign(model, data, {loaded: true});
      } catch (e) {
        if (_configState !== state) return;
        model.error = e.message;
      } finally {
        model.loading = false;
        paintEventTriggers(state);
      }
    }

    function openEventTriggerSettings() {
      openConfigDialog('governor', null, 'Features');
      _configState.focusEventTriggers = true;
    }

    function newEventTrigger() {
      const model = _configState.eventTriggers;
      if (!model || !model.loaded || model.busy || model.draft) return;
      model.editIndex = null;
      model.draft = {name: 'scanner-new-issue', agent: model.agents.includes('scanner') ? 'scanner' : '', priority: '0', preset: 'scanner', repo: '', expr: ''};
      model.error = '';
      paintEventTriggers(_configState);
    }

    function editEventTrigger(index) {
      const model = _configState.eventTriggers;
      if (!model || model.busy || model.draft || !model.rules[index]) return;
      model.editIndex = index;
      model.draft = {...model.rules[index], priority: String(model.rules[index].priority || 0), preset: 'advanced', repo: ''};
      model.error = '';
      paintEventTriggers(_configState);
    }

    function eventTriggerDraftChanged() {
      const model = _configState.eventTriggers;
      if (!model || !model.draft || model.busy) return;
      const d = model.draft;
      for (const key of ['name','agent','priority','preset','repo','expr']) {
        const el = document.getElementById('event-trigger-' + key);
        if (el) d[key] = el.value;
      }
    }

    function eventTriggerPresetChanged() {
      const model = _configState.eventTriggers;
      if (!model || !model.draft || model.busy) return;
      const oldPreset = model.draft.preset;
      eventTriggerDraftChanged();
      const d = model.draft;
      if (d.preset === 'scanner' && oldPreset !== 'scanner') d.agent = model.agents.includes('scanner') ? 'scanner' : '';
      if (d.preset !== 'advanced') {
        const kind = d.preset === 'scanner' ? 'issue.opened' : d.preset;
        d.expr = d.repo ? 'event.kind == ' + JSON.stringify(kind) + ' && event.repo == ' + JSON.stringify(d.repo) : '';
      }
      paintEventTriggers(_configState);
    }

    function cancelEventTriggerEdit() {
      const model = _configState.eventTriggers;
      if (!model || model.busy) return;
      model.draft = null;
      model.editIndex = null;
      paintEventTriggers(_configState);
    }

    function saveEventTrigger() {
      const model = _configState.eventTriggers;
      if (!model || !model.draft || model.busy) return;
      eventTriggerDraftChanged();
      const d = model.draft;
      const priority = Number(d.priority);
      if (!d.name.trim() || !model.agents.includes(d.agent) || !d.expr.trim() || !String(d.priority).trim() || !Number.isSafeInteger(priority) ||
          (d.preset !== 'advanced' && !model.repositories.some(r => r.value === d.repo))) {
        model.error = 'Choose a name, existing agent, one managed repository for a preset, and a bool CEL expression with integer priority.';
        paintEventTriggers(_configState);
        return;
      }
      const rules = model.rules.map(rule => ({...rule}));
      const index = model.editIndex === null ? rules.length : model.editIndex;
      rules[index] = {name: d.name.trim(), agent: d.agent, expr: d.expr, priority};
      const body = {rules};
      if (d.preset !== 'advanced') body.preset = {index, repo: d.repo, kind: d.preset === 'scanner' ? 'issue.opened' : d.preset};
      return persistEventTriggers(body);
    }

    function removeEventTrigger(index) {
      const model = _configState.eventTriggers;
      if (!model || !model.loaded || model.busy || model.draft || !model.rules[index]) return;
      return persistEventTriggers({rules: model.rules.filter((_, i) => i !== index)});
    }

    async function persistEventTriggers(body) {
      const state = _configState;
      const model = state.eventTriggers;
      if (!model || model.busy) return;
      model.busy = true;
      model.error = '';
      model.notice = '';
      paintEventTriggers(state);
      try {
        const headers = {..._inceptionAuthHeaders(), 'If-Match': model.version};
        const data = await eventTriggerResponse(await fetch('/api/config/triggers', {method: 'PUT', headers, body: JSON.stringify(body)}));
        if (_configState !== state) return;
        Object.assign(model, data, {draft: null, editIndex: null, notice: 'Saved. Applies on next event evaluation; no restart required. No webhook was registered.'});
      } catch (e) {
        if (_configState === state) model.error = e.message;
      } finally {
        model.busy = false;
        paintEventTriggers(state);
      }
    }
