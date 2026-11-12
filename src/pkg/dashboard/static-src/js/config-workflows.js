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
    const _workflowState = { loaded: false, loading: false, enabled: false, workflows: [], bindings: [] };

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
      return `<div class="config-list-item ld-sub" data-wf-repo="${esc(repo)}"><label class="rt-check" title="Which workflow model this repository follows (hivecommons/hive#17620). hive-default is the built-in lifecycle; a custom model changes the monitor's columns and, when workflow.enabled is on, how stages are derived.">Workflow <select ${locked ? 'disabled' : ''} data-change-action="setRepoWorkflow" data-arg0="${esc(repo)}" data-arg-types="s,v">${options}</select></label>${flag}${bound ? `<span class="rt-inherit" title="Bound to ${esc(current)}">bound</span>` : '<span class="rt-inherit">default</span>'}</div>`;
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
        showToast(body.name ? `${repo} now follows ${key}` : `${repo} back on hive-default`);
      } catch (err) {
        showToast('Failed to bind workflow: ' + err.message, 'error');
      }
      renderConfigTab('Repos');
    }
