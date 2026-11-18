    // Workflow-model bindings (hivecommons/hive#17620, RFC phase 2).
    //
    // Each repo row on Settings → Repos carries a "Workflow" select: hive-default
    // (the built-in lifecycle every unbound repo follows) or any compiled model
    // from the library. Binding persists immediately through
    // PUT /api/workflows/bindings (owner-only, audited) — it is not part of the
    // tab's Save button, because a binding is validated server-side against the
    // store (the model must exist and compile) and the tracker picks the new
    // columns up on its next poll.
    const WORKFLOW_DEFAULT_KEY = 'hive-default@1';
    const _workflowState = { loaded: false, loading: false, enabled: false, workflows: [], bindings: [], dispatch: [], agents: [] };

    function workflowKey(name, version) { return (name || '') + '@' + (version || ''); }

    async function loadWorkflowLibrary(onDone) {
      if (_workflowState.loading) return;
      _workflowState.loading = true;
      try {
        const res = await fetch('/api/workflows', { headers: _inceptionAuthHeaders() });
        if (res.status === 503) { _workflowState.loaded = true; _workflowState.workflows = []; return; }
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        _workflowState.workflows = Array.isArray(data.workflows) ? data.workflows : [];
        _workflowState.bindings = Array.isArray(data.bindings) ? data.bindings : [];
        _workflowState.enabled = !!data.enabled;
        workflowDispatchApply(data);
        _workflowState.loaded = true;
      } catch (err) {
        _workflowState.loaded = true;
        showToast('Could not load the workflow library: ' + err.message, 'error');
      } finally {
        _workflowState.loading = false;
        if (typeof onDone === 'function') onDone();
      }
    }

    function workflowBindingFor(repo) {
      const want = String(repo || '').toLowerCase();
      const org = String((_configState.data && _configState.data.org) || '').toLowerCase();
      return _workflowState.bindings.find(b => {
        const r = String(b.repo || '').toLowerCase();
        return r === want || (org && (r === org + '/' + want || want === org + '/' + r));
      }) || null;
    }

    // renderRepoWorkflowRow returns the per-repo select, or '' when the hive
    // has no workflow store (the row simply does not appear; nothing else on
    // the tab changes).
    function renderRepoWorkflowRow(repo) {
      if (!_workflowState.loaded) {
        loadWorkflowLibrary(() => { if (_configState.tab === 'Repos') renderConfigTab('Repos'); });
        return '';
      }
      if (!_workflowState.workflows.length) return '';
      const bound = workflowBindingFor(repo);
      const current = bound ? workflowKey(bound.name, bound.version) : WORKFLOW_DEFAULT_KEY;
      const locked = !dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const options = _workflowState.workflows.map(w => {
        const key = workflowKey(w.name, w.version);
        const compiled = w.compiled !== false;
        const label = key + (w.description ? ' — ' + w.description : '') + (compiled ? '' : ' (draft, does not compile)');
        return `<option value="${esc(key)}" ${key === current ? 'selected' : ''} ${compiled ? '' : 'disabled'}>${esc(label)}</option>`;
      }).join('');
      const flag = _workflowState.enabled || !bound
        ? ''
        : '<span class="rt-inherit wf-derivation-off" title="workflow.enabled is off: the binding is recorded and the monitor shows the model\'s columns, but stage derivation still runs the built-in engine. Turn on &quot;Derive stages from bound models&quot; on Settings → Workflows.">columns only — turn on derivation in Workflows</span>';
      return `<div class="config-list-item ld-sub" data-wf-repo="${esc(repo)}"><label class="rt-check" title="Which workflow model this repository follows (hivecommons/hive#17620). hive-default is the built-in lifecycle; a custom model changes the monitor's columns and, when workflow.enabled is on, how stages are derived.">Workflow <select ${locked ? 'disabled' : ''} data-change-action="setRepoWorkflow" data-arg0="${esc(repo)}" data-arg-types="s,v">${options}</select></label>${flag}${bound ? `<span class="rt-inherit" title="Bound to ${esc(current)}">bound</span>` : '<span class="rt-inherit">default</span>'}</div>${bound ? renderRepoWorkflowProfileRows(repo, workflowDispatchProfilesFor(bound, _workflowState.dispatch), _workflowState.agents, locked) : ''}`;
    }

    // Which hive agent fills each profile the bound model hands items to
    // (hivecommons/hive#18690). GET /api/workflows and the pin's own
    // response carry `dispatch` (one entry per bound repository and
    // profile: the resolved agent, how it was resolved, the pin, or the
    // problem when nothing fills it, `candidates` saying which agents may
    // do the profile's work, `not_started` listing the hand-offs an agent
    // declined) and `agents` (who can be pinned).
    function workflowDispatchApply(data) {
      _workflowState.dispatch = Array.isArray(data && data.dispatch) ? data.dispatch : [];
      _workflowState.agents = Array.isArray(data && data.agents) ? data.agents : [];
    }

    // Pure: the dispatch entries of one binding.
    function workflowDispatchProfilesFor(bound, dispatch) {
      const want = String((bound && bound.repo) || '').toLowerCase();
      return (dispatch || []).filter(d => d && d.profile && String(d.repo || '').toLowerCase() === want);
    }

    const WORKFLOW_DISPATCH_SOURCE_WORD = { named: 'the agent named after the profile', role: 'the first writer, docs or feature agent', fallback: 'the hive\'s standard agent for this kind of work' };

    // Pure: what one profile's row says — the option list of its picker
    // (the default first, then every agent that can be pinned, plus a pin
    // that no longer names an enabled agent so it stays visible), the
    // warning when nothing fills the profile, and the hand-offs an agent
    // declined or did not act on. An agent whose mode does not let it do
    // what the profile needs (`candidates`, hivecommons/hive#18749) is
    // listed but cannot be picked, with the reason on hover.
    function workflowDispatchProfileView(d, agents) {
      const pinned = String(d.pinned || '');
      const agent = String(d.agent || '');
      const auto = pinned ? 'Automatic' : (agent ? 'Automatic (' + agent + ')' : 'Automatic (none available)');
      const options = [{ value: '', label: auto, selected: !pinned }];
      const names = (agents || []).map(String);
      const cand = {};
      for (const c of (Array.isArray(d.candidates) ? d.candidates : [])) if (c && c.agent) cand[String(c.agent)] = c;
      if (pinned && names.indexOf(pinned) < 0) options.push({ value: pinned, label: pinned + ' (not an enabled agent)', selected: true });
      for (const n of names) {
        const c = cand[n];
        const unfit = !!c && c.qualifies === false;
        const mode = c && c.mode ? String(c.mode) : '';
        options.push({ value: n, label: unfit ? n + ' (' + (mode || 'not allowed') + ', cannot do this)' : n, selected: n === pinned, disabled: unfit && n !== pinned, title: unfit ? n + ' ' + String(c.reason || 'may not do what this profile needs') : '' });
      }
      const warning = d.problem ? 'Not handed off: ' + d.problem + '. Items in this profile\'s states wait until an agent that may do the work is picked.' : '';
      const how = pinned ? 'pinned' : (WORKFLOW_DISPATCH_SOURCE_WORD[String(d.source || '')] || '');
      const note = warning ? '' : (agent ? 'handed to ' + agent + (how ? ' (' + how + ')' : '') : '') + (d.disabled ? ' · automatic hand-off off, manual send only' : '');
      const stalled = (Array.isArray(d.not_started) ? d.not_started : []).map(h => workflowHandoffNote(h, d.profile));
      return { profile: String(d.profile), options, warning, note, stalled };
    }

    function renderRepoWorkflowProfileRows(repo, profiles, agents, locked) {
      return (profiles || []).map(d => {
        const v = workflowDispatchProfileView(d, agents);
        const opts = v.options.map(o => `<option value="${esc(o.value)}" ${o.selected ? 'selected' : ''} ${o.disabled ? 'disabled' : ''}${o.title ? ` title="${esc(o.title)}"` : ''}>${esc(o.label)}</option>`).join('');
        const tip = 'Which hive agent the ' + v.profile + ' profile of this workflow is handed to (workflow.dispatch.profiles.' + v.profile + '.agent). Automatic picks an agent named after the profile, then a writer, docs or feature agent, then the hive\'s standard agent for the work, and only ever an agent whose mode lets it do what the profile needs.';
        const msg = v.warning
          ? `<span class="rt-inherit wf-dispatch-gap" role="alert">${esc(v.warning)}</span>`
          : (v.note ? `<span class="rt-inherit">${esc(v.note)}</span>` : '');
        const stalled = v.stalled.map(line => `<span class="rt-inherit wf-dispatch-gap" data-wf-stalled="1" role="alert">${esc(line)}</span>`).join('');
        return `<div class="config-list-item ld-sub wf-dispatch-profile" data-wf-profile="${esc(v.profile)}"><label class="rt-check" title="${esc(tip)}">${esc(v.profile)} agent <select ${locked ? 'disabled' : ''} data-change-action="setWorkflowProfileAgent" data-arg0="${esc(v.profile)}" data-arg-types="s,v">${opts}</select></label>${msg}${stalled}</div>`;
      }).join('');
    }

    // Pins (or, with '', unpins) the agent a profile is handed to through
    // PUT /api/workflows/dispatch/profiles (owner-only, audited). Like the
    // binding it persists immediately; the next scheduler pass uses it.
    async function setWorkflowProfileAgent(profile, value) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) {
        showToast('Owner role is required to pick a workflow agent.', 'error');
        renderConfigTab('Repos');
        return;
      }
      const agent = String(value || '');
      try {
        const res = await fetch('/api/workflows/dispatch/profiles', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ profile: profile, agent: agent }),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        workflowDispatchApply(await res.json());
        showToast(agent ? `${profile} is now handed to ${agent}` : `${profile} is back on the automatic pick`);
      } catch (err) {
        showToast('Failed to pick the workflow agent: ' + err.message, 'error');
      }
      renderConfigTab('Repos');
    }

    async function setRepoWorkflow(repo, value) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) {
        showToast('Owner role is required to bind a workflow.', 'error');
        renderConfigTab('Repos');
        return;
      }
      const key = String(value || '');
      const body = { repo: repo };
      if (key && key !== WORKFLOW_DEFAULT_KEY) {
        const at = key.lastIndexOf('@');
        body.name = key.slice(0, at);
        body.version = key.slice(at + 1);
      }
      try {
        const res = await fetch('/api/workflows/bindings', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify(body),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        _workflowState.bindings = Array.isArray(data.bindings) ? data.bindings : [];
        if (Array.isArray(data.dispatch)) workflowDispatchApply(data);
        showToast(body.name ? `${repo} now follows ${key}` : `${repo} back on hive-default`);
      } catch (err) {
        showToast('Failed to bind workflow: ' + err.message, 'error');
      }
      renderConfigTab('Repos');
    }
