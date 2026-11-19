    // ── Review Gate: linked perspectives + agent sandbox settings (#18797) ──
    // Both used to be hand edits of the config files on the PVC. The Review
    // Gate fields save through PUT /api/config/review (perspectives_source);
    // the agent Sandbox fields through the agent General section.

    const PERSPECTIVES_SOURCE_FIELDS = ['owner', 'repo', 'path', 'ref'];
    const SANDBOX_RUNTIME_CHOICES = ['podman', 'job', 'openshell', 'substrate'];

    function reviewSourceFieldHtml(field, label, value, placeholder, canEdit) {
      return `<div class="config-field"><label for="rv-psrc-${field}">${label}</label>`
        + `<input type="text" id="rv-psrc-${field}" value="${esc(value || '')}" placeholder="${esc(placeholder)}" spellcheck="false" autocomplete="off"${canEdit ? '' : ' disabled'} data-change-action="markDirtyPerspectivesSource"></div>`;
    }

    /* The link controls and the resolver status. Shown whether or not a source
       is set: an unlinked hive links here, a linked one relinks or unlinks. */
    function reviewPerspectivesSourceHtml(rv, canEdit) {
      rv = rv || {};
      const src = rv.perspectives_source || {};
      const linked = rv.perspectives_linked === true;
      const status = rv.perspectives_source_status || '';
      const warning = rv.perspectives_source_warning || '';
      const approved = rv.perspectives_source_approved !== false;
      const securityLink = '<a href="#" class="text-accent" data-action="switchConfigTab" data-prevent="1" data-tab="Security">Settings, Security, Prompt sources</a>';
      let statusHtml = '';
      if (linked && status === 'unresolved') {
        statusHtml = '<div class="rv-perspectives-linked-status" role="status">Not resolved yet. The repo may be unreachable, the file invalid, or the repo not approved. The inline perspectives below are in effect.</div>';
      } else if (linked) {
        statusHtml = `<div class="rv-perspectives-linked-status" role="status">Resolved from <code>${esc(status)}</code></div>`;
      }
      const approvalHtml = linked && !approved
        ? `<div class="rv-perspectives-linked-warning" role="alert">&#9888; ${esc(src.owner || '')}/${esc(src.repo || '')} is not approved as a prompt source, so the link cannot resolve. An owner can approve it under ${securityLink}.</div>`
        : '';
      const warningHtml = warning ? `<div class="rv-perspectives-linked-warning">&#9888; ${esc(warning)}</div>` : '';
      const unlink = linked && canEdit
        ? '<button type="button" class="hv-btn btn-secondary btn-sm btn-small" id="rv-psrc-unlink" data-action="unlinkReviewPerspectivesSource">Unlink</button>'
        : '';
      return `<details class="settings-subsection-details" id="rv-psrc"${linked ? ' open' : ''}>
          <summary class="settings-subsection-summary">Link perspectives to a repo file ${yamlHint('review.perspectives_source')}</summary>
          <p class="config-help">Read the perspective names and focus text from a file in a GitHub repo, so a merged change there applies without editing this hive. The repo must be approved under ${securityLink}.</p>
          <div class="rv-psrc-fields">
            ${reviewSourceFieldHtml('owner', 'Owner', src.owner, 'org or user', canEdit)}
            ${reviewSourceFieldHtml('repo', 'Repo', src.repo, 'repo name', canEdit)}
            ${reviewSourceFieldHtml('path', 'Path', src.path, '.hive/perspectives.yaml', canEdit)}
            ${reviewSourceFieldHtml('ref', 'Ref (optional)', src.ref, 'branch, tag or SHA; default branch if blank', canEdit)}
          </div>
          <div class="rv-psrc-actions">${unlink}</div>
          ${statusHtml}${approvalHtml}${warningHtml}
        </details>`;
    }

    /* Reads the four inputs. Blank owner, repo or path clears the link on
       save, matching the server, where a partial source counts as unset. */
    function markDirtyPerspectivesSource() {
      const val = {};
      PERSPECTIVES_SOURCE_FIELDS.forEach(f => {
        const el = document.getElementById('rv-psrc-' + f);
        val[f] = el ? el.value.trim() : '';
      });
      const empty = PERSPECTIVES_SOURCE_FIELDS.every(f => !val[f]);
      markDirty('review', 'perspectives_source', empty ? null : val);
    }

    /* Unlink saves at once, not on the dialog's Save: it is the action the
       409 message sends the operator to, and it must work with the inline
       perspective edits still unsaved. */
    async function unlinkReviewPerspectivesSource() {
      try {
        const res = await fetch('/api/config/review', {
          method: 'PUT', headers: _inceptionAuthHeaders(), body: JSON.stringify({ perspectives_source: null })
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const fresh = await fetch('/api/config/review');
        if (fresh.ok) _configState.data.review = await fresh.json();
        if (_configState.dirty.review) delete _configState.dirty.review.perspectives_source;
        showToast('Perspectives unlinked. They are editable here now.', 'success');
        renderConfigTab('Features');
      } catch (err) {
        showToast('Failed to unlink perspectives: ' + err.message, 'error');
      }
    }

    // ── Agent sandbox settings ──────────────────────────────────────────

    function sandboxFieldHtml(label, id, value, placeholder, onchange) {
      return `<div class="config-field"><label for="${id}">${label}</label>`
        + `<input type="text" id="${id}" value="${esc(value == null ? '' : String(value))}" placeholder="${esc(placeholder)}" spellcheck="false" autocomplete="off" ${onchange}></div>`;
    }

    function sandboxJobFieldHtml(label, key, value, placeholder) {
      return sandboxFieldHtml(label, 'cfg-agent-sandbox-job-' + key, value, placeholder,
        `data-change-action="markDirtySandboxJob" data-arg0="${key}" data-arg-types="s,v"`);
    }

    /* Image, runtime and Job fields of this agent's own sandbox block. A blank
       field inherits the hive default (Settings, Security), shown as the
       placeholder; the warning names the field and links to the controls. */
    function agentSandboxSettingsHtml(g) {
      g = g || {};
      const f = g.sandboxFields || {};
      const job = f.sandboxJob || {};
      const sources = Array.isArray(g.sandboxCopySources) ? g.sandboxCopySources : [];
      const problem = g.sandboxDefaultsProblem || '';
      const defaultLink = '<a href="#" class="text-accent" data-action="openConfigDialog" data-prevent="1" data-config-type="governor" data-tab="Security">Set the default image in Settings, Security</a>';
      const problemHtml = problem
        ? `<div class="config-field-hint" id="cfg-agent-sandbox-problem" role="alert" style="color:var(--red)">${esc(problem)}. Set the image below${sources.length ? ', copy the settings from another agent' : ''}, or ${defaultLink}.</div>`
        : '';
      const runtimeOpts = ['<option value="">Inherit' + (f.sandboxRuntimeEffective ? ' (' + esc(f.sandboxRuntimeEffective) + ')' : '') + '</option>']
        .concat(SANDBOX_RUNTIME_CHOICES.map(r => `<option value="${r}"${f.sandboxRuntime === r ? ' selected' : ''}>${r}</option>`)).join('');
      const copyHtml = sources.length
        ? `<div class="config-field"><label for="cfg-agent-sandbox-copy">Copy sandbox settings from...</label>
            <div class="cfg-inline-row"><select id="cfg-agent-sandbox-copy">${sources.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('')}</select>
            <button type="button" class="hv-btn btn-secondary btn-sm btn-small" id="cfg-agent-sandbox-copy-btn" data-action="copySandboxSettingsFrom">Copy settings</button></div>
            <div class="config-field-hint">Copies the image, runtime and Job settings, including Secret names. No secret values are stored in a sandbox block. This agent's publish checks are kept. Turns the sandbox on for this agent.</div></div>`
        : '';
      return `<details class="settings-subsection-details" id="cfg-agent-sandbox-settings"${problem || g.sandboxEnabled ? ' open' : ''}>
          <summary class="settings-subsection-summary">Sandbox settings: image, runtime and Job</summary>
          ${problemHtml}
          ${copyHtml}
          ${sandboxFieldHtml('Image ' + yamlHint('sandbox.image'), 'cfg-agent-sandbox-image', f.sandboxImage, f.sandboxImageEffective ? 'inherits ' + f.sandboxImageEffective : 'container image', 'data-change-action="markDirty" data-arg0="general" data-arg1="sandboxImage" data-arg-types="s,s,v"')}
          <div class="config-field"><label for="cfg-agent-sandbox-runtime">Runtime ${yamlHint('sandbox.runtime')}</label>
            <select id="cfg-agent-sandbox-runtime" data-change-action="markDirty" data-arg0="general" data-arg1="sandboxRuntime" data-arg-types="s,s,v">${runtimeOpts}</select></div>
          ${sandboxJobFieldHtml('Job workspace claim ' + yamlHint('sandbox.job.workspace_claim'), 'workspaceClaim', job.workspaceClaim, 'PersistentVolumeClaim name (runtime job)')}
          ${sandboxJobFieldHtml('Job workspace claim mount', 'workspaceClaimMount', job.workspaceClaimMount, 'mount path')}
          ${sandboxJobFieldHtml('Job service account', 'serviceAccount', job.serviceAccount, 'service account name')}
          ${sandboxJobFieldHtml('Job scheduler name', 'schedulerName', job.schedulerName, 'cluster default')}
          ${sandboxJobFieldHtml('Job TTL (seconds)', 'ttlSeconds', job.ttlSeconds || '', '0 uses the default')}
          <div class="config-field"><label for="cfg-agent-sandbox-job-envFromSecrets">Job secrets (names) ${yamlHint('sandbox.job.env_from_secrets[]')}</label>
            <textarea id="cfg-agent-sandbox-job-envFromSecrets" rows="2" class="mono" placeholder="one Secret name per line" data-change-action="markDirtySandboxJob" data-arg0="envFromSecrets" data-arg-types="s,v">${esc((job.envFromSecrets || []).join('\n'))}</textarea>
            <div class="config-field-hint">Secret names only. Never paste a value.</div></div>
        </details>`;
    }

    function markDirtySandboxJob(key, value) {
      let v = value;
      if (key === 'ttlSeconds') v = value === '' || value == null ? 0 : Number(value);
      if (key === 'envFromSecrets') v = String(value || '').split('\n').map(s => s.trim()).filter(Boolean);
      const prev = (_configState.dirty.general || {}).sandboxJob || {};
      markDirty('general', 'sandboxJob', Object.assign({}, prev, { [key]: v }));
    }

    /* Copies the chosen agent's complete block into this agent at once, then
       reloads the dialog so the fields and the warning show the result. */
    async function copySandboxSettingsFrom() {
      const sel = document.getElementById('cfg-agent-sandbox-copy');
      const agent = _configState.agent;
      if (!sel || !sel.value || !agent) return;
      try {
        const res = await fetch('/api/config/agent/' + encodeURIComponent(agent) + '/general', {
          method: 'PUT', headers: _inceptionAuthHeaders(), body: JSON.stringify({ sandboxCopyFrom: sel.value })
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        await loadConfigData('agent', agent);
        if (_configState.dirty.general) {
          ['sandboxImage', 'sandboxRuntime', 'sandboxJob', 'sandboxCopyFrom', 'sandboxEnabled'].forEach(k => { delete _configState.dirty.general[k]; });
        }
        showToast('Sandbox settings copied from ' + sel.value, 'success');
        renderConfigTab('General');
      } catch (err) {
        showToast('Failed to copy sandbox settings: ' + err.message, 'error');
      }
    }
