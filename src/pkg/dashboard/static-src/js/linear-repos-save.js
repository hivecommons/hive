    // ── Linear agent (RFC #4492 Part 2) ─────────────────────────────────────
    // loadLinearAgentStatus paints install/workspace/session state from the
    // owner-only status endpoint into the Features tab's Linear Agent panel.
    async function loadLinearAgentStatus() {
      const out = document.getElementById('linear-agent-panel');
      if (!out) return;
      out.textContent = 'Loading…';
      try {
        const res = await fetch('/api/linear/agent/status');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const st = await res.json();
        const rows = [];
        if (!st.configured) {
          rows.push('<div class="text-warn">Not configured — set <code>LINEAR_CLIENT_ID</code>, <code>LINEAR_CLIENT_SECRET</code> and <code>LINEAR_WEBHOOK_SECRET</code> on the hive (see docs/linear-agent.md).</div>');
        }
        if (st.store_error) rows.push(`<div class="text-danger">${esc(st.store_error)}</div>`);
        if (st.connected) {
          const ws = st.workspace || {};
          rows.push(`<div>Connected to <strong>${esc(ws.name || ws.url_key || ws.id || 'workspace')}</strong> as app user <code>${esc(st.viewer_id || '')}</code>${st.connected_at ? ` since ${esc(formatDashboardDateTime(st.connected_at))}` : ''}.</div>`);
        } else {
          rows.push('<div>No Linear workspace connected.</div>');
        }
        rows.push(`<div>Session agent: ${st.session_agent ? `<strong>${esc(st.session_agent)}</strong>` : `<span class="text-warn">${esc(st.session_agent_error || 'not resolved')}</span>`}</div>`);
        const credLabel = { oauth: 'connected app OAuth token (writes authored by the Hive app user)', api_key: 'work-source API key (no workspace connected — writes authored by the key owner)', none: 'none — ISSUES_ONLY+ agents cannot write to Linear' }[st.agent_credential || 'none'];
        rows.push(`<div>Agent credential: <span style="color:${st.agent_credential === 'none' ? 'var(--yellow)' : 'inherit'}">${esc(credLabel)}</span></div>`);
        rows.push(`<div>Webhook endpoint: <code>${esc(st.webhook_path || '/api/linear/webhook')}</code> · OAuth callback: <code>${esc(st.callback_path || '/linear/callback')}</code></div>`);
        const sessions = Array.isArray(st.sessions) ? st.sessions : [];
        if (sessions.length) {
          const cells = sessions.map(s => `<tr><td style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">${s.issue_url ? `<a class="text-accent" href="${esc(s.issue_url)}" target="_blank" rel="noopener">${esc(s.issue_identifier || s.id)}</a>` : esc(s.issue_identifier || s.id)}</td><td style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">${esc(s.issue_title || '')}</td><td style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">${esc(s.agent || '—')}</td><td style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">${esc(s.state || '')}</td><td style="padding:var(--sp-1) var(--sp-0)">${s.last_event_at ? esc(formatDashboardDateTime(s.last_event_at)) : ''}</td></tr>`).join('');
          rows.push(`<table style="margin-top:var(--sp-3);border-collapse:collapse;font-size:0.72rem"><thead><tr style="text-align:left;color:var(--muted)"><th style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">Issue</th><th style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">Title</th><th style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">Agent</th><th style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)">State</th><th style="padding:var(--sp-1) var(--sp-0)">Last event</th></tr></thead><tbody>${cells}</tbody></table>`);
        } else {
          rows.push('<div>No agent sessions yet — delegate a Linear issue to the app to start one.</div>');
        }
        out.innerHTML = rows.join('');
      } catch (err) {
        out.textContent = `Failed to load Linear agent status: ${err.message}`;
      }
    }

    // ── Advisor lane Settings (#9725, phase 4 of #9638) ──
    // Owner-only editor for the fleet advisor block, the named model_roles map
    // and each agent's override/opt-out, over /api/config/advisor*. Every edit
    // is validated server-side with the hive.yaml checks and persisted; it
    // takes effect at the affected agent's next launch. The per-agent table
    // shows the value in force and where it comes from (agent/fleet/default).
    let _advisorSettings = null;
    const ADVISOR_NEXT_LAUNCH = 'takes effect at each affected agent’s next launch';

    async function loadAdvisorSettings() {
      const out = document.getElementById('advisor-settings-panel');
      if (!out) return;
      out.textContent = 'Loading…';
      try {
        const res = await fetch('/api/config/advisor', { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        _advisorSettings = await res.json();
        out.innerHTML = renderAdvisorSettings(_advisorSettings);
      } catch (err) {
        out.textContent = 'Failed to load advisor settings: ' + err.message;
      }
    }

    function advisorSourceBadge(src) {
      return `<span class="advisor-src advisor-src-${esc(src || 'default')}">${esc(src || 'default')}</span>`;
    }

    function renderAdvisorSettings(st) {
      const f = st.fleet || {};
      const d = st.defaults || {};
      const roles = st.model_roles || {};
      const agents = Array.isArray(st.agents) ? st.agents : [];
      const fleetOn = f.enabled === true;
      const roleNames = Object.keys(roles).sort();
      const roleRows = roleNames.map(n => {
        const r = roles[n] || {};
        return `<tr><td><code>@${esc(n)}</code></td><td>${esc(r.backend || '—')}</td><td><code>${esc(r.model || '')}</code></td><td>${esc(r.reasoning_effort || '—')}</td>
          <td><button class="hv-btn btn-secondary btn-sm" data-action="removeAdvisorRole" data-arg0="${esc(n)}">Remove</button></td></tr>`;
      }).join('');
      const agentRows = agents.map(a => {
        const e = a.effective || {};
        const s = e.sources || {};
        const model = e.resolved_model && e.resolved_model !== e.model ? `${esc(e.model || '')} → ${esc(e.resolved_model)}` : esc(e.resolved_model || e.model || '—');
        const active = a.active ? '<span class="text-success">active</span>' : `<span class="advisor-src" title="${esc(a.active_reason || '')}">not active</span>`;
        return `<tr><td><strong>${esc(a.name)}</strong>${a.override ? ' <span class="advisor-src advisor-src-agent">override</span>' : ''}</td><td>${esc(a.backend || '—')}</td><td>${active}</td>
          <td>${e.enabled ? 'on' : 'off'} ${advisorSourceBadge(s.enabled)}</td>
          <td><code>${model}</code> ${advisorSourceBadge(s.model)}</td>
          <td>${e.daily_budget_tokens ? formatNumber(Number(e.daily_budget_tokens)) : 'unlimited'} ${advisorSourceBadge(s.daily_budget_tokens)}</td>
          <td>${esc(String(e.timeout_s || ''))}s ${advisorSourceBadge(s.timeout_s)}</td>
          <td>${esc(String(e.max_consecutive_blocks || ''))} ${advisorSourceBadge(s.max_consecutive_blocks)}</td>
          <td><button class="hv-btn btn-secondary btn-sm" data-action="editAdvisorAgent" data-arg0="${esc(a.name)}">Edit</button></td></tr>`;
      }).join('');
      return `<div class="advisor-set-grid">
          <div><label class="advisor-set-label" for="adv-f-enabled">Fleet default</label>
            <select id="adv-f-enabled" class="advisor-set-input"><option value="on" ${fleetOn ? 'selected' : ''}>On — every supported agent is reviewed</option><option value="off" ${fleetOn ? '' : 'selected'}>Off — only agents that opt in</option></select></div>
          <div><label class="advisor-set-label" for="adv-f-model">Advisor model (id or @role)</label>
            <input id="adv-f-model" class="advisor-set-input" type="text" value="${esc(f.model || '')}" placeholder="trajectory reviewer model"></div>
          <div><label class="advisor-set-label" for="adv-f-budget">Daily budget (tokens, 0 = unlimited)</label>
            <input id="adv-f-budget" class="advisor-set-input" type="number" min="0" value="${f.daily_budget_tokens || ''}" placeholder="0"></div>
          <div><label class="advisor-set-label" for="adv-f-timeout">Review timeout (sec)</label>
            <input id="adv-f-timeout" class="advisor-set-input" type="number" min="0" value="${f.timeout_s || ''}" placeholder="${esc(String(d.timeout_s || ''))}"></div>
          <div><label class="advisor-set-label" for="adv-f-blocks">Consecutive-block limit (below ${esc(String(d.block_bound || ''))})</label>
            <input id="adv-f-blocks" class="advisor-set-input" type="number" min="0" value="${f.max_consecutive_blocks || ''}" placeholder="${esc(String(d.max_consecutive_blocks || ''))}"></div>
          <div class="advisor-set-wide"><label class="advisor-set-label" for="adv-f-instructions">Instructions for the advisor</label>
            <textarea id="adv-f-instructions" class="advisor-set-input" rows="3">${esc(f.instructions || '')}</textarea></div>
        </div>
        <div class="advisor-set-actions"><button class="hv-btn btn-primary btn" data-action="saveAdvisorFleet">Save fleet default</button></div>

        <div class="advisor-set-h">Model roles</div>
        <div class="advisor-note">Named models referenced as <code>@role</code> by the advisor block or an agent override. A role still referenced cannot be removed.</div>
        <table class="advisor-set-table"><thead><tr><th>Role</th><th>Backend</th><th>Model</th><th>Reasoning effort</th><th></th></tr></thead>
          <tbody>${roleRows || '<tr><td colspan="5" class="advisor-note">No roles defined.</td></tr>'}
          <tr><td><input id="adv-role-name" class="advisor-set-input" type="text" placeholder="name"></td><td><input id="adv-role-backend" class="advisor-set-input" type="text" placeholder="optional"></td>
            <td><input id="adv-role-model" class="advisor-set-input" type="text" placeholder="model id"></td><td><input id="adv-role-effort" class="advisor-set-input" type="text" placeholder="optional"></td>
            <td><button class="hv-btn btn-secondary btn-sm" data-action="addAdvisorRole">Add</button></td></tr></tbody></table>

        <div class="advisor-set-h">Per-agent value in force</div>
        <div class="advisor-note">Each value is marked with where it comes from: the agent's own <em>agent</em> override, the <em>fleet</em> default above, or the built-in <em>default</em>.</div>
        <table class="advisor-set-table"><thead><tr><th>Agent</th><th>Backend</th><th>Advisor</th><th>Enabled</th><th>Model</th><th>Budget</th><th>Timeout</th><th>Blocks</th><th></th></tr></thead>
          <tbody>${agentRows || '<tr><td colspan="9" class="advisor-note">No agents configured.</td></tr>'}</tbody></table>`;
    }

    async function putAdvisorSettings(path, body, okMsg) {
      try {
        const res = await fetch(path, { method: 'PUT', headers: _inceptionAuthHeaders(), body: JSON.stringify(body) });
        if (!res.ok) { showToast('Advisor settings not saved: ' + await saveErrorMessage(res), 'error'); return false; }
        _advisorSettings = await res.json();
        const out = document.getElementById('advisor-settings-panel');
        if (out) out.innerHTML = renderAdvisorSettings(_advisorSettings);
        showToast(okMsg + ' — ' + ADVISOR_NEXT_LAUNCH, 'success');
        return true;
      } catch (err) {
        showToast('Advisor settings not saved: ' + err.message, 'error');
        return false;
      }
    }

    function advisorIntField(id) {
      const el = document.getElementById(id);
      const v = el ? el.value.trim() : '';
      return v === '' ? null : Math.max(0, parseInt(v, 10) || 0);
    }
    function advisorTextField(id) {
      const el = document.getElementById(id);
      return el ? el.value.trim() : '';
    }

    function saveAdvisorFleet() {
      const enabledEl = document.getElementById('adv-f-enabled');
      return putAdvisorSettings('/api/config/advisor', {
        enabled: !!enabledEl && enabledEl.value === 'on',
        model: advisorTextField('adv-f-model'),
        instructions: advisorTextField('adv-f-instructions'),
        daily_budget_tokens: advisorIntField('adv-f-budget') || 0,
        timeout_s: advisorIntField('adv-f-timeout') || 0,
        max_consecutive_blocks: advisorIntField('adv-f-blocks') || 0,
      }, 'Advisor fleet default saved');
    }

    function addAdvisorRole() {
      const name = advisorTextField('adv-role-name').replace(/^@/, '');
      const model = advisorTextField('adv-role-model');
      if (!name || !model) { showToast('A role needs a name and a model', 'error'); return; }
      const roles = Object.assign({}, (_advisorSettings || {}).model_roles || {});
      roles[name] = { backend: advisorTextField('adv-role-backend'), model, reasoning_effort: advisorTextField('adv-role-effort') };
      return putAdvisorSettings('/api/config/advisor/roles', { model_roles: roles }, `Role @${name} saved`);
    }

    async function removeAdvisorRole(name) {
      const ok = await hiveConfirm({ title: 'Remove model role', message: `Remove the @${name} role? Anything still referencing it must be changed first.`, confirmLabel: 'Remove' });
      if (!ok) return;
      const roles = Object.assign({}, (_advisorSettings || {}).model_roles || {});
      delete roles[name];
      return putAdvisorSettings('/api/config/advisor/roles', { model_roles: roles }, `Role @${name} removed`);
    }

    function editAdvisorAgent(name) {
      const a = ((_advisorSettings || {}).agents || []).find(x => x.name === name);
      if (!a) return;
      const o = a.override || {};
      const e = a.effective || {};
      const en = o.enabled === true ? 'on' : (o.enabled === false ? 'off' : 'inherit');
      const opt = (v, label) => `<option value="${v}" ${en === v ? 'selected' : ''}>${label}</option>`;
      const num = (id, label, v, ph) => `<div><label class="advisor-set-label" for="${id}">${label}</label><input id="${id}" class="advisor-set-input" type="number" min="0" value="${v == null ? '' : esc(String(v))}" placeholder="inherit (${esc(String(ph))})"></div>`;
      kbShowModal(`Advisor — ${esc(name)}`, `
        <div class="advisor-note">Blank fields inherit the fleet default. ${a.active ? '' : esc(a.active_reason || '')}</div>
        <div class="advisor-set-grid">
          <div><label class="advisor-set-label" for="adv-a-enabled">Advisor for this agent</label>
            <select id="adv-a-enabled" class="advisor-set-input">${opt('inherit', 'Inherit fleet default')}${opt('on', 'On')}${opt('off', 'Off — opt out')}</select></div>
          <div><label class="advisor-set-label" for="adv-a-model">Model (id or @role)</label>
            <input id="adv-a-model" class="advisor-set-input" type="text" value="${esc(o.model || '')}" placeholder="inherit (${esc(e.resolved_model || e.model || 'default')})"></div>
          ${num('adv-a-budget', 'Daily budget (tokens)', o.daily_budget_tokens, e.daily_budget_tokens || 'unlimited')}
          ${num('adv-a-timeout', 'Review timeout (sec)', o.timeout_s, e.timeout_s)}
          ${num('adv-a-blocks', 'Consecutive-block limit', o.max_consecutive_blocks, e.max_consecutive_blocks)}
          <div class="advisor-set-wide"><label class="advisor-set-label" for="adv-a-instructions">Instructions</label>
            <textarea id="adv-a-instructions" class="advisor-set-input" rows="3" placeholder="inherit">${esc(o.instructions || '')}</textarea></div>
        </div>
        <div class="advisor-set-actions">
          <button class="hv-btn btn-primary btn" data-action="saveAdvisorAgent" data-arg0="${esc(name)}">Save override</button>
          <button class="hv-btn btn-secondary btn" data-action="clearAdvisorAgent" data-arg0="${esc(name)}">Clear override</button>
          <button class="hv-btn btn-secondary btn" data-action="kbCloseModal">Cancel</button>
        </div>`, { width: '640px' });
    }

    async function saveAdvisorAgent(name) {
      const override = {};
      const en = (document.getElementById('adv-a-enabled') || {}).value || 'inherit';
      if (en !== 'inherit') override.enabled = en === 'on';
      const model = advisorTextField('adv-a-model');
      if (model) override.model = model;
      const instructions = advisorTextField('adv-a-instructions');
      if (instructions) override.instructions = instructions;
      for (const [id, key] of [['adv-a-budget', 'daily_budget_tokens'], ['adv-a-timeout', 'timeout_s'], ['adv-a-blocks', 'max_consecutive_blocks']]) {
        const v = advisorIntField(id);
        if (v != null) override[key] = v;
      }
      const empty = Object.keys(override).length === 0;
      if (await putAdvisorSettings('/api/config/advisor/agent/' + encodeURIComponent(name), { override: empty ? null : override },
        empty ? `${name} now inherits the fleet default` : `Advisor override for ${name} saved`)) kbCloseModal();
    }

    async function clearAdvisorAgent(name) {
      const ok = await hiveConfirm({ title: 'Clear advisor override', message: `Clear ${name}'s advisor override? It will inherit the fleet default.`, confirmLabel: 'Clear' });
      if (!ok) return;
      if (await putAdvisorSettings('/api/config/advisor/agent/' + encodeURIComponent(name), { override: null }, `${name} now inherits the fleet default`)) kbCloseModal();
    }

    // linearAgentConnect starts the actor=app OAuth flow: the install endpoint
    // returns Linear's authorize URL with a single-use state; approving it in
    // the opened tab lands on /linear/callback and persists the grant.
    async function linearAgentConnect() {
      try {
        const res = await fetch('/api/linear/agent/install', { method: 'POST' });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
        if (!data.authorize_url) throw new Error('no authorize URL returned');
        hiveExportViewer({title:'Linear authorization URL', text:data.authorize_url, sourceURL:data.authorize_url, allowOpen:true});
        showToast('Open the Linear authorization link from the in-app dialog, then press Refresh here.', 'info');
      } catch (err) {
        showToast(`Linear connect failed: ${err.message}`, 'error');
      }
    }

    // linearAgentDisconnect forgets the stored grant. Revoking the app itself
    // happens in Linear's workspace settings, not here.
    async function linearAgentDisconnect() {
      if (!await hiveConfirm({title: 'Forget Linear workspace connection?', message: 'The app must also be revoked from Linear\'s settings to fully remove it.', confirmLabel: 'Forget', danger: true})) return;
      try {
        const res = await fetch('/api/linear/agent/disconnect', { method: 'POST' });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
        showToast('Linear workspace disconnected', 'success');
        loadLinearAgentStatus();
      } catch (err) {
        showToast(`Linear disconnect failed: ${err.message}`, 'error');
      }
    }

    // markPromptSourceDirty reads the four GitHub prompt-source inputs and records
    // them as a nested promptSource object on the general section (or null when
    // fully blank, which clears the source server-side). Owner+repo+path must all
    // be present for the source to take effect; a partial fill is sent as-is and
    // the server treats an incomplete source as cleared.
    function markPromptSourceDirty() {
      const owner = (document.getElementById('cfg-agent-ps-owner') || {}).value || '';
      const repo = (document.getElementById('cfg-agent-ps-repo') || {}).value || '';
      const path = (document.getElementById('cfg-agent-ps-path') || {}).value || '';
      const ref = (document.getElementById('cfg-agent-ps-ref') || {}).value || '';
      if (!owner.trim() && !repo.trim() && !path.trim() && !ref.trim()) {
        markDirty('general', 'promptSource', null);
        return;
      }
      markDirty('general', 'promptSource', {
        owner: owner.trim(),
        repo: repo.trim(),
        path: path.trim(),
        ref: ref.trim(),
      });
    }

    // collectPromptSource returns the current prompt-source object from the form,
    // or null when incomplete. Used by the create-agent submit path (which builds
    // its own agent payload rather than reading _configState.dirty for this field).
    function collectPromptSource() {
      const owner = ((document.getElementById('cfg-agent-ps-owner') || {}).value || '').trim();
      const repo = ((document.getElementById('cfg-agent-ps-repo') || {}).value || '').trim();
      const path = ((document.getElementById('cfg-agent-ps-path') || {}).value || '').trim();
      const ref = ((document.getElementById('cfg-agent-ps-ref') || {}).value || '').trim();
      if (!owner || !repo || !path) return null;
      return { type: 'github', owner: owner, repo: repo, path: path, ref: ref };
    }

    // onchange handler for the CLI Pin Value select — ensures the selected
    // gateway method is actually configured (prompting for the LiteLLM
    // endpoint, or routing to Model Gateways for the others) and reverts the
    // selection when it is not, before recording the change.
    async function onCliPinChange(sel) {
      if (!(await ensureGatewayMethodConfigured(sel.value))) {
        sel.value = sel.dataset.prev || 'claude';
        return;
      }
      sel.dataset.prev = sel.value;
      markDirty('general', 'cliPinValue', sel.value);
      updateLaunchCmd(sel.value);
      refreshModelOptions(sel.value);
      updateBobSessionLabelVisibility();
    }

    function bobSessionEffectiveLabel() {
      const explicit = ((document.getElementById('cfg-bob-session-label') || {}).value || '').trim();
      if (explicit) return explicit;
      const general = (_configState.data || {}).general || {};
      const prefix = (general.bobSessionPrefix || '').trim();
      const name = _configState.agent || '';
      return prefix ? prefix + name : '(none — Bob shows the workdir name)';
    }

    function updateBobSessionLabelVisibility() {
      const wrap = document.getElementById('cfg-bob-session-label-wrap');
      if (!wrap) return;
      const backend = ((document.getElementById('cfg-pin-value') || {}).value || '').trim();
      wrap.hidden = backend !== 'bob';
      const hint = document.getElementById('cfg-bob-session-effective');
      if (hint) hint.textContent = bobSessionEffectiveLabel();
    }

    function onBobSessionLabelChange(el) {
      const v = (el && el.value !== undefined ? el.value : this.value || '').trim();
      markDirty('general', 'bobSessionLabel', v);
      updateBobSessionLabelVisibility();
    }

    // refreshModelOptions rebuilds the Model select for a newly picked CLI Pin
    // Value. renderAgentGeneral fills it once, for the backend the dialog
    // opened with, so without this an operator switching claude -> codex was
    // offered Claude models until they saved and reopened the dialog. The
    // current model stays selected (as "(current)" when the new backend does
    // not offer it) so the switch never silently changes the model.
    function refreshModelOptions(backend) {
      const sel = document.getElementById('cfg-model');
      if (!sel) return;
      const current = sel.value;
      const opts = modelsForBackend(backend) || [];
      const match = opts.find(m => _modelsEqual(current, m.value));
      sel.innerHTML = modelSelectOptionsHtml(backend, current, opts);
      sel.title = MODEL_PRECEDENCE_TITLE;
      sel.value = match ? match.value : current;
    }

    // MODEL_PRECEDENCE_TITLE is the Model select's tooltip: which model an
    // agent really sends, in precedence order (#9945 — an operator could not
    // tell that "(from launch command)" meant "no per-agent model").
    const MODEL_PRECEDENCE_TITLE = 'Which model this agent sends: 1) the per-agent model chosen here, if any; 2) otherwise the backend default — a gateway\'s default_model (Settings → Model Gateways) or, for a CLI backend, the model the CLI itself picks. A model the gateway\'s team is not allowed fails every call with a 403, so prefer the backend default unless you need a specific allowed model.';

    // fullModelLabel shows the complete gateway model id (e.g.
    // rits/zai-org/glm-5-3, not just glm-5-3) with any discovery suffix, so
    // the option is recognisable as the id the gateway card shows (#9945).
    // Lists built without a suffix (static CLI catalogs) keep their label.
    function fullModelLabel(m) {
      return m.suffix !== undefined ? m.value + m.suffix : m.label;
    }

    // inferenceBackendDefaultModel is the model an agent on backend uses when
    // it names none, as reported by /api/inference/models ("" when unknown).
    function inferenceBackendDefaultModel(backend) {
      const c = _inferenceModelCache[canonicalInferenceBackendName(backend)] || _inferenceModelCache[backend];
      return (c && c.defaultModel) || '';
    }

    // modelDisallowedByGateway is true when the gateway's last entitlement
    // signal (key-info probe or a "team not allowed" 403) is known and does
    // not include model — the agent will 403 on every call with it.
    function modelDisallowedByGateway(backend, model) {
      if (!model) return false;
      const c = _inferenceModelCache[canonicalInferenceBackendName(backend)] || _inferenceModelCache[backend];
      if (!c || !c.entitled) return false;
      const entitled = c.entitledModels || (c.models || []).map(m => m.value);
      return !entitled.some(e => _modelIdMatches(e, model));
    }

    // modelDefaultOptionLabel names the "no per-agent model" choice by what
    // it resolves to, replacing the opaque "(from launch command)".
    function modelDefaultOptionLabel(backend) {
      const def = inferenceBackendDefaultModel(backend);
      if (def) return `(backend default — ${def})`;
      if (isInferenceBackendName(backend)) return '(backend default — the gateway\'s default_model, Settings → Model Gateways)';
      return `(backend default — chosen by the ${backend || 'CLI'} CLI)`;
    }

    // modelSelectOptionsHtml builds the agent config dialog's Model options:
    // the backend-default choice first, every offered model with its FULL id,
    // and — when the current model is not offered — a "(current)" option that
    // says when the gateway does not allow it, so an operator looking at
    // "aws/claude-sonnet-4-6 (current — not allowed by gateway)" knows why
    // the agent is failing and what to pick instead (#9945).
    function modelSelectOptionsHtml(backend, current, opts) {
      const models = opts || modelsForBackend(backend) || [];
      let html = `<option value="">${esc(modelDefaultOptionLabel(backend))}</option>`;
      html += models.map(m => `<option value="${esc(m.value)}"${_modelsEqual(current, m.value) ? ' selected' : ''}>${esc(fullModelLabel(m))}</option>`).join('');
      if (current && !models.some(m => _modelsEqual(current, m.value))) {
        const note = modelDisallowedByGateway(backend, current) ? ' (current — not allowed by gateway)' : ' (current)';
        html += `<option value="${esc(current)}" selected>${esc(current)}${note}</option>`;
      }
      return html;
    }

    // ensureModelOptionsForBackend refreshes the dialog's Model select once
    // live discovery for an inference backend has answered, so the default
    // label and the "not allowed" flag reflect the gateway rather than a cold
    // cache on first open.
    function ensureModelOptionsForBackend(backend) {
      if (!isInferenceBackendName(backend) || typeof fetchInferenceModels !== 'function') return;
      fetchInferenceModels(canonicalInferenceBackendName(backend)).then(() => {
        const sel = document.getElementById('cfg-model');
        if (sel) refreshModelOptions(backend);
      }).catch(() => {});
    }

    function updateLaunchCmd(cli) {
      const input = document.getElementById('cfg-launch-cmd');
      if (!input) return;
      const current = input.value;
      // A direct "/usr/bin/<cli> <flags>" command cannot be retargeted by
      // swapping the path: the image installs most CLIs under /usr/local/bin
      // (so "/usr/bin/bob" does not exist, #10509) and the old CLI's flags
      // are wrong for the new one. Clear it so hive builds the launch command.
      const updated = /^\s*\/usr\/bin\/\S+/.test(current)
        ? ''
        : current.replace(/agent-launch\.sh\s+--backend\s+\S+/, `agent-launch.sh --backend ${cli}`);
      if (updated !== current) {
        input.value = updated;
        markDirty('general', 'launchCmd', updated);
      }
    }

    function addRestriction() {
      const patternEl = document.getElementById('cfg-restriction-pattern');
      const reasonEl = document.getElementById('cfg-restriction-reason');
      const pattern = (patternEl ? patternEl.value.trim() : '');
      const reason = (reasonEl ? reasonEl.value.trim() : '');
      if (!pattern) return;
      if (!_configState.data.restrictions) _configState.data.restrictions = { agent: [], global: [], policy: [] };
      _configState.data.restrictions.agent.push({ pattern, reason, enabled: true });
      markDirty('restrictions', 'rules', _configState.data.restrictions.agent);
      if (patternEl) patternEl.value = '';
      if (reasonEl) reasonEl.value = '';
      renderConfigTab(_configState.tab);
    }

    function removeRestriction(index) {
      if (!_configState.data.restrictions || !_configState.data.restrictions.agent) return;
      _configState.data.restrictions.agent.splice(index, 1);
      markDirty('restrictions', 'rules', _configState.data.restrictions.agent);
      renderConfigTab(_configState.tab);
    }

    function toggleRestriction(index, enabled) {
      if (!_configState.data.restrictions || !_configState.data.restrictions.agent) return;
      _configState.data.restrictions.agent[index].enabled = enabled;
      markDirty('restrictions', 'rules', _configState.data.restrictions.agent);
    }

    function normalizeRepoInput(raw) {
      return raw.trim().replace(/\/+$/, '');
    }

    // hiveForgeHostLabel returns this hive's bare GitHub host ("github.com" or a
    // GHE hostname). Mirrors the server's hiveForgeHost() and renderGovRepos so
    // the single-host rule matches everywhere.
    function hiveForgeHostLabel() {
      const ghBase = window._githubBaseUrl || 'https://github.com';
      return ghBase.replace(/^https?:\/\//, '').replace(/\/$/, '') || 'github.com';
    }

    // repoRefHostLabel extracts the GitHub host a pasted repo carries, or '' when
    // it names none (bare "repo"/"owner/repo" belong to the hive's forge).
    // Mirrors the server's repoRefHostLabel (a leading dotted segment or full URL
    // is an explicit host).
    function repoRefHostLabel(s) {
      s = (s || '').trim().replace(/^https?:\/\//, '').replace(/^\/+|\/+$/g, '');
      if (!s) return '';
      const parts = s.split('/');
      if (parts.length > 1 && parts[0].indexOf('.') !== -1) return parts[0].toLowerCase();
      return '';
    }

    function sameForgeHost(a, b) {
      const norm = h => { h = (h || '').trim().toLowerCase(); return (!h || h === 'github.com') ? 'github.com' : h; };
      return norm(a) === norm(b);
    }

    // addRepoWithChecks handles the Repos-tab "+ Add": it enforces the
    // single-host-per-spoke rule client-side (requirement #2), then asks the
    // server whether the hive's GitHub App can access the new repo's org and, if
    // not, guides the user through installing/authorizing the App before the repo
    // is accepted (requirement #3). Only on a clean check is the repo appended.
    async function addRepoWithChecks() {
      const input = document.getElementById('cfg-repo-input');
      if (!input) return;
      const raw = input.value.trim();
      if (!raw) return;
      const statusEl = document.getElementById('cfg-repo-add-status');
      const setStatus = (html, color) => { if (statusEl) { statusEl.innerHTML = html; statusEl.style.color = color || 'var(--muted)'; } };

      // Requirement #2: reject a paste on a different forge before any round-trip.
      const spokeHost = hiveForgeHostLabel();
      const refHost = repoRefHostLabel(raw);
      if (refHost && !sameForgeHost(refHost, spokeHost)) {
        setStatus('✕ ' + esc(raw) + ' is on <strong>' + esc(refHost) + '</strong>, but this hive is on <strong>' + esc(spokeHost) + '</strong>. A hive\'s repos must all live on one GitHub host — remove the host or use a repo on ' + esc(spokeHost) + '.', 'var(--red)');
        return;
      }

      const val = normalizeRepoInput(raw);
      if (!_configState.data.repos) _configState.data.repos = [];
      if (_configState.data.repos.includes(val)) {
        setStatus('This repo is already in the list.', 'var(--muted)');
        return;
      }

      // Requirement #3: verify the App can access the repo's org.
      setStatus('Checking GitHub App access…', 'var(--muted)');
      try {
        const res = await fetch('/api/config/governor/repos/check-access', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ repo: val })
        });
        const d = await res.json().catch(() => ({}));
        if (res.status === 409 && d && d.needsInstall) {
          // App not installed on the new org: surface the correct per-forge
          // install URL wired to the existing pending-install/recheck plumbing.
          // Clicking Install flags the heartbeat (handleGitHubAppInstallClicked)
          // exactly like the App-required banner; Re-check re-probes and, on
          // success, completes the add.
          const installBtn = (d.installUrl)
            ? '<a href="' + esc(d.installUrl) + '" target="_blank" rel="noopener" data-action="markGitHubAppInstallClicked" style="color:var(--accent);font-weight:600">Install/authorize the App for ' + esc(d.org || '') + ' ↗</a>'
            : '<span class="text-brand">This hive\'s GitHub Enterprise App slug is not configured, so an install link cannot be built — ask your operator to install the Hive App for ' + esc(d.org || '') + '.</span>';
          setStatus('⚠ ' + esc(d.error || ('The App needs access to ' + val)) + '<br>' + installBtn + ' &nbsp;·&nbsp; <a href="#" data-action="recheckRepoAccessThenAdd" data-arg-types="e" style="color:var(--accent);font-weight:600">Re-check &amp; add</a>', 'var(--amber)');
          return;
        }
        if (!res.ok) {
          setStatus('✕ ' + esc((d && d.error) || ('HTTP ' + res.status)), 'var(--red)');
          return;
        }
      } catch (e) {
        // Network error probing access is non-fatal: the server re-validates the
        // single-host rule and the default-repo rule on save, so let the add
        // proceed rather than hard-block on a transient dashboard hiccup.
        _appendRepoAccepted(val);
        setStatus('Added (App-access check could not be reached; the server will re-validate on save).', 'var(--muted)');
        return;
      }
      _appendRepoAccepted(val);
      setStatus('✓ Added — App has access.', 'var(--green)');
    }

    // _appendRepoAccepted appends a validated repo, seeds the first repo as the
    // default (a hive must always have one), and re-renders.
    function _appendRepoAccepted(val) {
      if (!_configState.data.repos) _configState.data.repos = [];
      _configState.data.repos.push(val);
      markDirty('repos', 'repos', _configState.data.repos);
      // First repo added to an empty list becomes the default automatically, so
      // there is always exactly one default (requirement #4).
      if (!_configState.data.primaryRepo || !_configState.data.repos.includes(_configState.data.primaryRepo)) {
        _configState.data.primaryRepo = val;
        markDirty('repos', 'primaryRepo', val);
      }
      const input = document.getElementById('cfg-repo-input');
      if (input) input.value = '';
      renderConfigTab(_configState.tab);
    }

    // markGitHubAppInstallClicked flags the heartbeat that an install link was
    // clicked, reusing the existing pending-install endpoint. Fire-and-forget.
    function markGitHubAppInstallClicked() {
      fetch('/api/github-app/install-clicked', { method: 'POST', headers: _inceptionAuthHeaders() }).catch(() => {});
    }

    // recheckRepoAccessThenAdd re-runs the App-access probe after the user
    // installs/authorizes the App, and completes the add when access is now
    // confirmed — the same re-check pattern the App-required banner uses.
    async function recheckRepoAccessThenAdd(ev) {
      if (ev) ev.preventDefault();
      await addRepoWithChecks();
    }

    // syncRepoSaveGuard disables the config dialog Save button while the Repos
    // tab has repos but no default selected, so a hive can never be saved without
    // exactly one default (requirement #4). Called after every config tab render.
    const CONFIG_NOT_LOADED_MESSAGE = 'Configuration did not load — there is nothing to save.';
    function syncRepoSaveGuard() {
      const saveBtn = document.querySelector('.config-footer .config-save');
      if (!saveBtn) return;
      let block = false;
      let why = '';
      const d = _configState.data || {};
      if (_configState.loadError) {
        // Nothing loaded, so there is nothing real to save; a Save here would
        // persist the placeholder's zeros (#7405).
        block = true;
        why = CONFIG_NOT_LOADED_MESSAGE;
      } else if (_configState.type === 'governor' && Array.isArray(d.repos) && d.repos.length > 0) {
        block = !d.primaryRepo || !d.repos.includes(d.primaryRepo);
        why = 'Set a default repo before saving — click a ☆ star in the Repos tab.';
      }
      saveBtn.disabled = block;
      saveBtn.style.opacity = block ? '0.5' : '';
      saveBtn.style.cursor = block ? 'not-allowed' : '';
      saveBtn.title = block ? why : '';
    }

    function addListItem(section, key, inputId) {
      const input = document.getElementById(inputId);
      let val = input.value.trim();
      if (!val) return;
      if (section === 'hooks') {
        if (!_configState.data.hooks) _configState.data.hooks = {};
        if (!_configState.data.hooks[key]) _configState.data.hooks[key] = [];
        _configState.data.hooks[key].push(val);
        markDirty('hooks', key, _configState.data.hooks[key]);
      } else if (section === 'labels') {
        if (String(key).startsWith('hard:')) {
          const bucket = String(key).slice(5);
          const st = hardSuppressState();
          stageHardSuppressLabels({ [bucket]: (st[bucket] || []).concat([val]) });
        } else if (key === 'require') {
          if (!_configState.data.requireLabels) _configState.data.requireLabels = [];
          _configState.data.requireLabels.push(val);
          markDirty('labels', 'require_labels', _configState.data.requireLabels);
        } else if (key === 'reporter_login') {
          const st = reporterTrustState();
          stageReporterTrust({ trustedLogins: st.trustedLogins.concat([val.replace(/^@/, '')]) });
        } else if (key === 'reporter_label') {
          const st = reporterTrustState();
          stageReporterTrust({ untrustedRequireLabels: st.untrustedRequireLabels.concat([val]) });
        } else {
          if (!_configState.data.labels) _configState.data.labels = [];
          _configState.data.labels.push(val);
          markDirty('labels', 'labels', _configState.data.labels);
        }
      } else if (section === 'repos') {
        // Repos go through the async host-check + App-access workflow, which
        // owns input clearing and re-render on success. Return here so the
        // synchronous tail below does not double-add or clear prematurely.
        addRepoWithChecks();
        return;
      }
      input.value = '';
      renderConfigTab(_configState.tab);
    }

    function removeListItem(section, key, index) {
      if (section === 'hooks') {
        _configState.data.hooks[key].splice(index, 1);
        markDirty('hooks', key, _configState.data.hooks[key]);
      } else if (section === 'labels') {
        if (String(key).startsWith('hard:')) {
          const bucket = String(key).slice(5);
          const st = hardSuppressState();
          stageHardSuppressLabels({ [bucket]: (st[bucket] || []).filter((_, i) => i !== index) });
        } else if (key === 'require') {
          _configState.data.requireLabels.splice(index, 1);
          markDirty('labels', 'require_labels', _configState.data.requireLabels);
        } else if (key === 'reporter_login') {
          const st = reporterTrustState();
          stageReporterTrust({ trustedLogins: st.trustedLogins.filter((_, i) => i !== index) });
        } else if (key === 'reporter_label') {
          const st = reporterTrustState();
          stageReporterTrust({ untrustedRequireLabels: st.untrustedRequireLabels.filter((_, i) => i !== index) });
        } else {
          _configState.data.labels.splice(index, 1);
          markDirty('labels', 'labels', _configState.data.labels);
        }
      } else if (section === 'repos') {
        _configState.data.repos.splice(index, 1);
        markDirty('repos', 'repos', _configState.data.repos);
      }
      renderConfigTab(_configState.tab);
    }

    function addAgent() {
      const input = document.getElementById('cfg-new-agent');
      const name = input.value.trim().toLowerCase();
      if (!name) return;
      if (_configState.data.agents && _configState.data.agents.includes(name)) {
        showToast(`Agent "${name}" already exists`, 'error');
        return;
      }
      const copySelect = document.getElementById('cfg-copy-from');
      const copyFrom = copySelect ? copySelect.value : '';
      const payload = copyFrom ? { name, copyFrom } : { name };
      fetch('/api/config/governor/agents', {
        method: 'POST', headers: _inceptionAuthHeaders(),
        body: JSON.stringify(payload)
      }).then(r => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.json(); })
        .then(() => {
          showToast(copyFrom ? `Agent "${name}" created from ${copyFrom}` : `Agent "${name}" added`, 'success');
          if (!_configState.data.agents) _configState.data.agents = [];
          _configState.data.agents.push(name);
          input.value = '';
          renderConfigTab('Agents');
        })
        .catch(err => showToast(`Failed: ${err.message}`, 'error'));
    }

    // _packAgentNote returns an extra confirm-dialog line when the agent is part
    // of the active ACMM pack. Deleting a pack agent used to look like it had
    // silently failed — the pack re-added it on the next config reload — so the
    // dialog now states up front that the deletion is recorded and permanent.
    function _packAgentNote(name) {
      const packAgents = (window._lastStatus && window._lastStatus.acmmPackAgents) || [];
      if (!packAgents.includes(name)) return '';
      return `\n\n"${name}" is part of this hive's ACMM pack. The deletion will be recorded, so no restart or ACMM level change will bring it back. Add it again from the Agents tab if you change your mind.`;
    }

    async function deleteAgentFromConfig(name) {
      if (!(await hiveConfirm(`Delete agent "${name}"?\n\nThis will remove its env file, governor config, and sidebar entry. The tmux session (if running) will NOT be killed.${_packAgentNote(name)}`))) return;
      fetch(`/api/config/governor/agents/${name}`, { method: 'DELETE' })
        .then(r => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.json(); })
        .then(d => {
          // Say out loud when the agent belongs to an ACMM pack. The pack used
          // to re-add it on the next reload/restart with no explanation, which
          // is why this was reported as a bug rather than asked about.
          showToast(((d || {}).note) || `Agent "${name}" deleted`, 'success');
          closeConfigDialog();
          // Remove from cached agents so cards disappear immediately
          window._lastAgents = (window._lastAgents || []).filter(a => a.name !== name);
          // Remove from sidebar layout
          if (_sidebarLayout && _sidebarLayout.groups) {
            for (const g of _sidebarLayout.groups) {
              g.agents = (g.agents || []).filter(a => a !== name);
            }
            _saveSidebarLayout(_sidebarLayout.groups);
          }
          ocUpdateSidebarAgents();
          renderAgents(window._lastAgents);
        })
        .catch(err => showToast(`Failed: ${err.message}`, 'error'));
    }

    async function removeAgent(name) {
      if (!(await hiveConfirm(`Remove agent "${name}"? This will delete its configuration.${_packAgentNote(name)}`))) return;
      fetch(`/api/config/governor/agents/${name}`, { method: 'DELETE' })
        .then(r => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return r.json(); })
        .then(d => {
          showToast(((d || {}).note) || `Agent "${name}" removed`, 'success');
          _configState.data.agents = (_configState.data.agents || []).filter(a => a !== name);
          renderConfigTab('Agents');
        })
        .catch(err => showToast(`Failed: ${err.message}`, 'error'));
    }

    // saveErrorMessage turns a failed save Response into a user-facing
    // message. 403 means the hub proxied this user in with read-only
    // access — explain that instead of a bare HTTP status.
    async function saveErrorMessage(res) {
      // Prefer the server's own reason. A 403 is NOT always "you're read-only":
      // the owner-role gate also 403s when the role is fine but the hub's
      // owner-verification proof is missing (owner reached the dashboard without
      // the proxy-proof header). Hard-coding "read-only" for every 403 misleads
      // an actual owner into thinking they lack permissions. Surface the real
      // message when the body carries one; fall back to the read-only wording
      // only for a bare, message-less 403.
      let serverMsg = '';
      try {
        const d = await res.json();
        if (d && d.error) serverMsg = d.error;
      } catch (e) { /* non-JSON body */ }
      if (serverMsg) return serverMsg;
      if (res.status === 403) {
        return 'your permissions on this hive are read-only, so changes are not allowed. If you are the owner, sign out and back in to refresh your access; otherwise contact the owner of this hive to ask for write permissions.';
      }
      return 'HTTP ' + res.status;
    }

    async function saveConfig() {
      const dirty = _configState.dirty;

      // Guard: the dialog never loaded a config (#7405). Save is disabled in
      // this state, but a keyboard/racy click could still reach here — refuse
      // rather than PUT values that were never the agent's.
      if (_configState.loadError) {
        showToast(CONFIG_NOT_LOADED_MESSAGE, 'error');
        return;
      }

      // Guard: a governor config with repos must always name a default that is
      // one of them (requirement #4). The Save button is disabled in this state,
      // but a keyboard/racy click could still reach here — refuse and point the
      // user at the fix rather than letting the server 400 opaquely.
      if (_configState.type === 'governor') {
        const d = _configState.data || {};
        if (Array.isArray(d.repos) && d.repos.length > 0 && (!d.primaryRepo || !d.repos.includes(d.primaryRepo))) {
          showToast('Set a default repo before saving — click a ☆ star in the Repos tab.', 'error');
          renderConfigTab('Repos');
          return;
        }
        const wfErrors = dirty['work-source'] ? wsWavefrontErrors((d.work_source || {}).wavefront || {}) : [];
        if (wfErrors.length) {
          showToast(wfErrors[0], 'error');
          renderConfigTab('Work Source');
          return;
        }
      }

      if (_configState.isCreate) {
        const createData = dirty._create || {};
        const generalData = dirty.general || {};
        const cadenceData = dirty.cadences || {};
        const name = (createData.name || '').trim().toLowerCase().replace(/\s+/g, '-');
        if (!name) {
          showToast('Agent name is required', 'error');
          return;
        }
        if (!/^[a-z0-9][a-z0-9-]*$/.test(name)) {
          showToast('Name must be lowercase letters, numbers, and hyphens', 'error');
          return;
        }
        if (!(await ensureGatewayMethodConfigured(generalData.cliPinValue))) return;
        const agent = {
          backend: generalData.cliPinValue || 'copilot',
          model: generalData.model || '',
          enabled: true,
          display_name: generalData.displayName || '',
          description: generalData.description || '',
          emoji: generalData.emoji || '',
          color: generalData.color || '#3498db',
          sort_order: generalData.sortOrder || 50,
          replicas: generalData.replicas || 1,
          bead_role: generalData.beadRole || 'worker',
          role: generalData.role || '',
          kick_template: generalData.kickTemplate || '',
          launch_cmd: generalData.launchCmd || '',
          restart_strategy: generalData.restartStrategy || 'immediate',
          stale_timeout: generalData.staleTimeout || 0,
          clear_on_kick: generalData.clearOnKick !== false,
          include_repos: generalData.includeRepos !== false,
          repos: generalData.repos || [], // #6204: empty = every repo
          lane_keywords: generalData.laneKeywords || [],
          detect_keywords: generalData.detectKeywords || [],
          aliases: generalData.aliases || [],
          caveman_mode: generalData.cavemanMode || '',
          explain_mode: generalData.explainMode || '',
        };
        if (generalData.bobSessionLabel) agent.bob = { session_label: generalData.bobSessionLabel };
        // Create mode has a Prompt Template tab but no agent to PUT to yet, so
        // the edited template rides along in the create payload's kick_template.
        capturePromptTemplate();
        if (_configState.promptTemplateDirty && _configState.promptTemplateValue) {
          agent.kick_template = _configState.promptTemplateValue;
        }
        if (generalData.sandboxEnabled !== undefined) agent.sandbox = { enabled: generalData.sandboxEnabled };
        const ps = collectPromptSource();
        if (ps) agent.prompt_source = ps;
        try {
          const res = await fetch('/api/agents', {
            method: 'POST',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify({ name, agent })
          });
          const data = await res.json();
          if (!res.ok) {
            showToast(data.error || 'Failed to create agent', 'error');
            return;
          }
          showToast(`Agent "${name}" created`, 'success');
          _configState.dirty = {};
          closeConfigDialog();
          fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
        } catch (err) {
          showToast('Network error: ' + err.message, 'error');
        }
        return;
      }

      // The kick template is not a _configState.dirty section — it has its own
      // endpoint — so it is checked separately here (#2766: the footer Save is
      // the single commit point now that "Save Template" is gone).
      capturePromptTemplate();
      const templateDirty = !!_configState.promptTemplateDirty;
      if (Object.keys(dirty).length === 0 && !templateDirty) {
        showToast('No changes to save', 'info');
        closeConfigDialog();
        return;
      }
      // Pinning an agent to a gateway method requires that gateway to be
      // configured WITH a key — ask (and abort the save when the operator
      // backs out or is routed to Model Gateways) before PUTting the section.
      if (dirty.general && dirty.general.cliPinValue !== undefined &&
          !(await ensureGatewayMethodConfigured(dirty.general.cliPinValue))) return;
      const base = _configState.type === 'governor' ? '/api/config/governor' : `/api/config/agent/${_configState.agent}`;
      let success = true;
      // Set once a cadences section has been accepted by the server, so the
      // success path knows to repaint the governor cadence matrix even when
      // no `general` field changed (#5492).
      let savedCadences = false;
      for (const [section, values] of Object.entries(dirty)) {
        try {
          if (_configState.type === 'governor' && section === 'general' && values.hiveId !== undefined) {
            const idRes = await fetch('/api/hive-id', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify({ id: values.hiveId })
            });
            if (!idRes.ok) throw new Error(await saveErrorMessage(idRes));
          } else if (_configState.type === 'governor' && section === 'health' && hasWatchdogKeys(values)) {
            // The Watchdog controls render inside the Health tab, but they save
            // to their own endpoint (governor.watchdog is its own config block).
            // Split the dirty bag: watchdog keys go one way, the rest stay on
            // the health endpoint, so editing both in one visit saves both.
            const { watchdog, health } = splitWatchdogKeys(values);
            const wdRes = await fetch('/api/config/governor/watchdog', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(watchdog)
            });
            if (!wdRes.ok) throw new Error(await saveErrorMessage(wdRes));
            // The activity strip's headline ("would have taken" vs "taken")
            // and promotion hint depend on the mode just saved; drop the
            // cached readout so the next Health render fetches it afresh.
            invalidateWatchdogActivity();
            if (Object.keys(health).length > 0) {
              const hRes = await fetch(`${base}/health`, {
                method: 'PUT',
                headers: _inceptionAuthHeaders(),
                body: JSON.stringify(health)
              });
              if (!hRes.ok) throw new Error(await saveErrorMessage(hRes));
            }
          } else if (_configState.type === 'governor' && section === 'channels') {
            // Inbound trigger surfaces (Settings → Channels) span github.*,
            // notifications.* bots and escalation sinks; they save through
            // their own owner-only /api/config/channels endpoint.
            const chRes = await fetch('/api/config/channels', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!chRes.ok) throw new Error(await saveErrorMessage(chRes));
            _configState.data.channels = await chRes.json().catch(() => _configState.data.channels);
          } else if (_configState.type === 'governor' && section === 'escalation') {
            // Escalation is a top-level Config field, not under /governor.
            const escRes = await fetch('/api/config/escalation', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!escRes.ok) throw new Error(await saveErrorMessage(escRes));
            _configState.data.escalation = await escRes.json().catch(() => _configState.data.escalation);
          } else if (_configState.type === 'governor' && section === 'auto-merge') {
            // auto_merge is a TOP-LEVEL config block, not under governor —
            // it saves through its own /api/config/auto-merge endpoint.
            const amRes = await fetch('/api/config/auto-merge', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!amRes.ok) throw new Error(await saveErrorMessage(amRes));
          } else if (_configState.type === 'governor' && section === 'sla') {
            // sla is a TOP-LEVEL config block (Delivery SLA targets); it
            // saves through its own owner-only /api/config/sla endpoint.
            const current = ((_configState.data || {}).sla || {}).targets || {};
            const slaRes = await fetch('/api/config/sla', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify({ targets: Object.assign({}, current, values) })
            });
            if (!slaRes.ok) throw new Error(await saveErrorMessage(slaRes));
            _configState.data.sla = await slaRes.json().catch(() => _configState.data.sla);
          } else if (_configState.type === 'governor' && section === 'convergence') {
            // convergence is a TOP-LEVEL config block, not under governor —
            // it saves through its own owner-only /api/config/convergence
            // endpoint (#4263 off/shadow/enforce rollout).
            const cvRes = await fetch('/api/config/convergence', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!cvRes.ok) throw new Error(await saveErrorMessage(cvRes));
            _configState.data.convergence = await cvRes.json().catch(() => _configState.data.convergence);
          } else if (_configState.type === 'governor' && section === 'review') {
            // review is a TOP-LEVEL config block, not under governor —
            // it saves through its own /api/config/review endpoint.
            const rvRes = await fetch('/api/config/review', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!rvRes.ok) throw new Error(await saveErrorMessage(rvRes));
          } else if (_configState.type === 'governor' && section === 'dashboard') {
            const dashRes = await fetch('/api/config/dashboard', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!dashRes.ok) throw new Error(await saveErrorMessage(dashRes));
            _configState.data.dashboard = await dashRes.json().catch(() => _configState.data.dashboard);
          } else if (_configState.type === 'governor' && section === 'backends') {
            const backendsRes = await fetch('/api/config/backends', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!backendsRes.ok) throw new Error(await saveErrorMessage(backendsRes));
            _configState.data.backends = await backendsRes.json().catch(() => _configState.data.backends);
          } else if (_configState.type === 'governor' && section === 'classifier') {
            const clsRes = await fetch('/api/config/classifier', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!clsRes.ok) throw new Error(await saveErrorMessage(clsRes));
            _configState.data.classifier = await clsRes.json().catch(() => _configState.data.classifier);
          } else {
            const res = await fetch(`${base}/${section}`, {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(values)
            });
            if (!res.ok) throw new Error(await saveErrorMessage(res));
            if (section === 'cadences') {
              // A cadence write kicks an ASYNC status rebuild server-side, so
              // the refetch below can be served the cached pre-write snapshot
              // and repaint the OLD cadence — the operator waits for a later
              // broadcast to see their own change (#5492). Raise the
              // stale-snapshot floor from the write ack so any snapshot built
              // before this mutation is discarded instead of rendered.
              // Reached only after the !res.ok throw above, so a failed write
              // never advances the floor.
              const cadenceAck = await res.json().catch(() => ({}));
              noteStatusMutation(cadenceAck.minStatusSeq);
              savedCadences = true;
            }
            if (_configState.type === 'governor' && section === 'features') {
              // Spektacular runner/executor settings are rewired live; say so
              // when they could not be, instead of implying they are (#9172).
              const ack = await res.json().catch(() => ({}));
              const spekMsg = spektacularApplyMessage(ack.spektacularApply);
              if (spekMsg) showToast(spekMsg.msg, spekMsg.level);
            }
            if (_configState.type === 'governor' && section === 'dashboard') {
              // #12076: the write succeeded, but below ACMM L3 the Experiment
              // Lab stays hidden — say so instead of letting a saved toggle
              // look like a no-op.
              noteExperimentLabSaved(values);
            }
            if (_configState.type === 'governor' && section === 'litellm') {
              // Saving the LiteLLM tab auto-runs the live connection test
              // server-side; surface its result exactly like the Test
              // Connection button would (inline line + toast).
              const data = await res.json().catch(() => null);
              if (data && data.probe) {
                const r = showLiteLLMTestResult(data.probe);
                showToast(r.msg, r.ok ? 'success' : 'error');
              }
              // A saved key can unlock (or change) the entitlement-filtered
              // model set — force a fresh discovery and re-render the
              // Default Model dropdown so the new models appear immediately.
              fetchInferenceModels('litellm', true).then(refreshLitellmDefaultModelField).catch(() => {});
            }
          }
        } catch (err) {
          showToast(`Failed to save ${section}: ${err.message}`, 'error');
          success = false;
        }
      }
      // Persist the Prompt Template tab as part of the same click. It raises
      // its own error toast and returns false on failure, which keeps the
      // dialog open rather than silently discarding the edit.
      if (templateDirty && !(await savePromptTemplate())) success = false;
      if (success) {
        showToast('Configuration saved', 'success');
        // Optimistically reflect agent-level identity fields that render
        // outside this dialog (sidebar rows, agent cards, token panel,
        // focused detail header) before the confirming refetch lands —
        // the same optimistic-update + refetch pattern switchCli/switchModel
        // use. _lastAgents and _lastStatus.agents share object references,
        // so one mutation covers every render site.
        const savedGeneral = dirty.general;
        const savedAgentUpdate = _configState.type === 'agent' && savedGeneral && Array.isArray(window._lastAgents);
        if (savedAgentUpdate) {
          const liveAgent = window._lastAgents.find(x => x.name === _configState.agent);
          if (liveAgent) {
            if (savedGeneral.displayName !== undefined) liveAgent.displayName = savedGeneral.displayName;
            if (savedGeneral.emoji !== undefined) liveAgent.emoji = savedGeneral.emoji;
            if (savedGeneral.color !== undefined) liveAgent.color = savedGeneral.color;
            if (savedGeneral.sortOrder !== undefined) liveAgent.sortOrder = savedGeneral.sortOrder;
            if (savedGeneral.sandboxEnabled !== undefined) liveAgent.sandboxed = savedGeneral.sandboxEnabled && (((window._lastStatus || {}).security || {}).sandboxEnabled !== false);
            if (savedGeneral.replicas !== undefined) liveAgent.replicaCount = savedGeneral.replicas;
          }
        }
        _configState.dirty = {};
        closeConfigDialog(); // resets _configState — do not read it below
        if (savedAgentUpdate) {
          renderAgents(window._lastAgents);
          ocUpdateSidebarAgents();
          injectAgentStyles(window._lastAgents);
          if (typeof ocRenderAgentDetail === 'function') ocRenderAgentDetail();
          const lastData = window._lastStatus || {};
          renderGovernor(lastData.governor || {}, lastData.cadenceMatrix || [], lastData);
          renderTokens(lastData.tokens || {});
        }
        if (savedCadences) {
          // A cadence-only save has no `general` block, so the optimistic
          // block above is skipped entirely and nothing repaints the governor
          // cadence matrix. The matrix is server-derived (cadenceMatrix), so
          // there is no confirmed value to patch in locally — instead pull the
          // post-mutation snapshot, which the raised floor guarantees is not a
          // pre-write one (#5492).
          refreshStatusUntilFresh();
        } else {
          fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
        }
      }
    }

    // Poll /api/status until a snapshot at or above the stale-snapshot floor
    // arrives, then render it. The server rebuild kicked by a config write is
    // asynchronous, so the first refetch usually still returns the cached
    // pre-write snapshot; render() drops it via the floor guard and this
    // retries briefly rather than leaving the panel stale until the next
    // broadcast (#5492).
    //
    // This never invents a value: every render is a real server snapshot. If
    // the rebuild never produces a fresh one, we simply stop retrying and the
    // regular SSE stream repaints whenever it next delivers — the operator is
    // never shown an unconfirmed value.
    var STATUS_FRESH_RETRY_MS = 400;
    var STATUS_FRESH_MAX_ATTEMPTS = 12;
    function refreshStatusUntilFresh(attempt) {
      const n = attempt || 0;
      fetch('/api/status')
        .then(r => r.json())
        .then(data => {
          // render() itself enforces the floor, but we need to know whether it
          // took, so test the same condition here before delegating.
          const seq = (data && data.statusSeq != null) ? Number(data.statusSeq) : null;
          const stale = seq != null && seq < _statusSeqFloor;
          if (!stale) { render(data); return; }
          if (n + 1 < STATUS_FRESH_MAX_ATTEMPTS) {
            setTimeout(() => refreshStatusUntilFresh(n + 1), STATUS_FRESH_RETRY_MS);
          }
        })
        .catch(() => {});
    }

