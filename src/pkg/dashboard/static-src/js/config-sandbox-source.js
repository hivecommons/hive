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
          ${sandboxSchedulingHtml('agent', f)}
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

    // ── Sandbox Job scheduling, resources, preflight, read relay (#19553) ──
    // Workspace setup (sandbox.setup, #19574) shares the renderer and stager.
    // One renderer and one stager for both places these live: the hive-wide
    // agent_sandbox block (Settings, Security, scope 'hive') and one agent's
    // sandbox block (agent General tab, scope 'agent'). The server validates
    // every value again (api_sandbox_job_scheduling.go); the checks here only
    // explain a mistake before Save. A field with an error is left out of the
    // save, so a typo never saves half a map.

    const SANDBOX_TAINT_EFFECTS = ['NoSchedule', 'PreferNoSchedule', 'NoExecute'];
    const SANDBOX_K8S_NAME_MAX = 63;
    const SANDBOX_K8S_PREFIX_MAX = 253;
    const SANDBOX_MAP_MAX_ENTRIES = 64;
    /* Pod metadata keys the Job launcher sets itself. Mirrors the
       SandboxPod* constants in crux/config/sandbox_job_metadata.go; a Go
       test keeps the two in step. */
    const SANDBOX_RESERVED_POD_LABELS = ['job-name', 'app.kubernetes.io/managed-by'];
    const SANDBOX_RESERVED_POD_LABEL_PREFIX = 'hive.kubestellar.io/';
    const SANDBOX_TOLERATIONS_MAX = 32;
    const SANDBOX_PULL_SECRETS_MAX = 32;
    const SANDBOX_PREFLIGHT_COMMANDS_MAX = 32;
    const SANDBOX_PREFLIGHT_COMMAND_BYTES_MAX = 4096;
    const SANDBOX_K8S_NAME_RE = /^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$/;
    const SANDBOX_DNS_NAME_RE = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
    const SANDBOX_QUANTITY_RE = /^([0-9]+(\.[0-9]*)?|\.[0-9]+)([KMGTPE]i|[numkMGTPE]|[eE][+-]?[0-9]+)?$/;
    const SANDBOX_SOURCE_LABELS = { agent: "this agent's override", agent_sandbox: 'from agent_sandbox defaults', '': 'not set' };
    const SANDBOX_PREFLIGHT_FIELDS = ['preflightCommands', 'preflightTimeoutS', 'preflightTotalTimeoutS', 'preflightRetryDelayS'];
    const SANDBOX_SETUP_FIELDS = ['setupCommands', 'setupHealthCommands', 'setupTimeoutS', 'setupTotalTimeoutS'];

    /* The hive.yaml path behind each control, spelled out in full so the
       settings parity guard (settings_parity_test.go) finds every leaf. */
    const SANDBOX_SCHED_YAML = {
      hive: {
        nodeSelector: 'agent_sandbox.job.node_selector.<key>',
        podLabels: 'agent_sandbox.job.pod_labels.<key>',
        podAnnotations: 'agent_sandbox.job.pod_annotations.<key>',
        limits: 'agent_sandbox.job.resources.limits.<key>',
        requests: 'agent_sandbox.job.resources.requests.<key>',
        tolerations: 'agent_sandbox.job.tolerations[].key',
        tolerationParts: ['agent_sandbox.job.tolerations[].operator', 'agent_sandbox.job.tolerations[].value', 'agent_sandbox.job.tolerations[].effect'],
        imagePullSecrets: 'agent_sandbox.job.image_pull_secrets[]',
        scheduleTimeoutS: 'agent_sandbox.job.schedule_timeout_s',
        preflightCommands: 'agent_sandbox.preflight.commands[]',
        preflightTimeoutS: 'agent_sandbox.preflight.timeout_s',
        preflightTotalTimeoutS: 'agent_sandbox.preflight.total_timeout_s',
        preflightRetryDelayS: 'agent_sandbox.preflight.retry_delay_s',
        setupCommands: 'agent_sandbox.setup.commands[]',
        setupHealthCommands: 'agent_sandbox.setup.health_commands[]',
        setupTimeoutS: 'agent_sandbox.setup.timeout_s',
        setupTotalTimeoutS: 'agent_sandbox.setup.total_timeout_s',
      },
      agent: {
        nodeSelector: 'agents.<key>.sandbox.job.node_selector.<key>',
        podLabels: 'agents.<key>.sandbox.job.pod_labels.<key>',
        podAnnotations: 'agents.<key>.sandbox.job.pod_annotations.<key>',
        limits: 'agents.<key>.sandbox.job.resources.limits.<key>',
        requests: 'agents.<key>.sandbox.job.resources.requests.<key>',
        tolerations: 'agents.<key>.sandbox.job.tolerations[].key',
        tolerationParts: ['agents.<key>.sandbox.job.tolerations[].operator', 'agents.<key>.sandbox.job.tolerations[].value', 'agents.<key>.sandbox.job.tolerations[].effect'],
        imagePullSecrets: 'agents.<key>.sandbox.job.image_pull_secrets[]',
        scheduleTimeoutS: 'agents.<key>.sandbox.job.schedule_timeout_s',
        preflightCommands: 'agents.<key>.sandbox.preflight.commands[]',
        preflightTimeoutS: 'agents.<key>.sandbox.preflight.timeout_s',
        preflightTotalTimeoutS: 'agents.<key>.sandbox.preflight.total_timeout_s',
        preflightRetryDelayS: 'agents.<key>.sandbox.preflight.retry_delay_s',
        setupCommands: 'agents.<key>.sandbox.setup.commands[]',
        setupHealthCommands: 'agents.<key>.sandbox.setup.health_commands[]',
        setupTimeoutS: 'agents.<key>.sandbox.setup.timeout_s',
        setupTotalTimeoutS: 'agents.<key>.sandbox.setup.total_timeout_s',
      },
    };

    function sandboxQualifiedNameError(s) {
      const slash = s.lastIndexOf('/');
      const name = slash >= 0 ? s.slice(slash + 1) : s;
      if (slash >= 0) {
        const prefix = s.slice(0, slash);
        if (!prefix || prefix.length > SANDBOX_K8S_PREFIX_MAX || !SANDBOX_DNS_NAME_RE.test(prefix)) return `"${s}" has an invalid prefix (a lowercase DNS name such as example.com)`;
      }
      if (!name || name.length > SANDBOX_K8S_NAME_MAX || !SANDBOX_K8S_NAME_RE.test(name)) return `"${s}" is not a valid Kubernetes name`;
      return '';
    }

    function sandboxReservedPodKey(k) {
      const lower = k.toLowerCase();
      return SANDBOX_RESERVED_POD_LABELS.includes(lower) || lower.startsWith(SANDBOX_RESERVED_POD_LABEL_PREFIX);
    }

    /* key=value lines into a map. kind 'label' takes label values (may be
       empty), 'podLabel' is a label that must not use a reserved key, and
       kind 'quantity' takes resource quantities like 2, 500m, 16Gi. */
    function sandboxParseMapLines(text, kind) {
      const value = {};
      const errors = [];
      String(text || '').split(/\r?\n/).forEach((raw, i) => {
        const line = raw.trim();
        if (!line) return;
        const idx = line.indexOf('=');
        if (idx <= 0) { errors.push(`line ${i + 1}: write key=value`); return; }
        const k = line.slice(0, idx).trim();
        const v = line.slice(idx + 1).trim();
        const keyErr = sandboxQualifiedNameError(k);
        if (keyErr) { errors.push(`line ${i + 1}: ${keyErr}`); return; }
        if (kind === 'quantity' && !SANDBOX_QUANTITY_RE.test(v)) { errors.push(`line ${i + 1}: "${v}" is not a quantity (for example 2, 500m, 16Gi)`); return; }
        if (kind === 'podLabel' && sandboxReservedPodKey(k)) { errors.push(`line ${i + 1}: "${k}" is reserved`); return; }
        if ((kind === 'label' || kind === 'podLabel') && v && (v.length > SANDBOX_K8S_NAME_MAX || !SANDBOX_K8S_NAME_RE.test(v))) { errors.push(`line ${i + 1}: "${v}" is not a valid label value`); return; }
        value[k] = v;
      });
      if (Object.keys(value).length > SANDBOX_MAP_MAX_ENTRIES) errors.push(`at most ${SANDBOX_MAP_MAX_ENTRIES} entries`);
      return { value, errors };
    }

    function sandboxParseAnnotations(text) {
      try {
        const value = JSON.parse(String(text || '').trim() || '{}');
        if (!value || Array.isArray(value) || typeof value !== 'object' || Object.values(value).some(v => typeof v !== 'string')) {
          return { value: {}, errors: ['write a JSON object with string values'] };
        }
        return { value, errors: [] };
      } catch (_) {
        return { value: {}, errors: ['write a valid JSON object'] };
      }
    }

    function sandboxMapText(map) {
      return Object.keys(map || {}).sort().map(k => k + '=' + map[k]).join('\n');
    }

    /* Toleration lines in taint syntax: key=value:Effect (operator Equal),
       key:Effect or key (operator Exists), * or *:Effect (every taint). An
       omitted effect matches every effect. */
    function sandboxParseTolerationLines(text) {
      const value = [];
      const errors = [];
      String(text || '').split(/\r?\n/).forEach((raw, i) => {
        const line = raw.trim();
        if (!line) return;
        let spec = line;
        let effect = '';
        const colon = line.lastIndexOf(':');
        if (colon >= 0) {
          effect = line.slice(colon + 1).trim();
          spec = line.slice(0, colon).trim();
          if (!SANDBOX_TAINT_EFFECTS.includes(effect)) { errors.push(`line ${i + 1}: effect must be ${SANDBOX_TAINT_EFFECTS.join(', ')} or left out`); return; }
        }
        if (spec === '*') { value.push({ key: '', operator: 'Exists', value: '', effect }); return; }
        const eq = spec.indexOf('=');
        const key = (eq >= 0 ? spec.slice(0, eq) : spec).trim();
        const val = eq >= 0 ? spec.slice(eq + 1).trim() : '';
        const keyErr = sandboxQualifiedNameError(key);
        if (keyErr) { errors.push(`line ${i + 1}: ${keyErr}`); return; }
        if (val && (val.length > SANDBOX_K8S_NAME_MAX || !SANDBOX_K8S_NAME_RE.test(val))) { errors.push(`line ${i + 1}: "${val}" is not a valid taint value`); return; }
        value.push({ key, operator: eq >= 0 ? 'Equal' : 'Exists', value: val, effect });
      });
      if (value.length > SANDBOX_TOLERATIONS_MAX) errors.push(`at most ${SANDBOX_TOLERATIONS_MAX} tolerations`);
      return { value, errors };
    }

    function sandboxTolerationText(list) {
      return (list || []).map(t => {
        const op = t.operator || (t.value ? 'Equal' : '');
        let spec = t.key || '';
        if (!spec && op === 'Exists') spec = '*';
        else if (op !== 'Exists') spec += '=' + (t.value || '');
        return spec + (t.effect ? ':' + t.effect : '');
      }).join('\n');
    }

    function sandboxParseNameLines(text) {
      const value = [];
      const errors = [];
      String(text || '').split(/\r?\n/).forEach((raw, i) => {
        const s = raw.trim();
        if (!s) return;
        if (s.length > SANDBOX_K8S_PREFIX_MAX || !SANDBOX_DNS_NAME_RE.test(s)) { errors.push(`line ${i + 1}: "${s}" is not a Secret name. Names only, never a value`); return; }
        value.push(s);
      });
      if (value.length > SANDBOX_PULL_SECRETS_MAX) errors.push(`at most ${SANDBOX_PULL_SECRETS_MAX} names`);
      return { value, errors };
    }

    function sandboxParseCommandLines(text) {
      const value = String(text || '').split(/\r?\n/).map(s => s.trim()).filter(Boolean);
      const errors = [];
      if (value.length > SANDBOX_PREFLIGHT_COMMANDS_MAX) errors.push(`at most ${SANDBOX_PREFLIGHT_COMMANDS_MAX} commands`);
      value.forEach((c, i) => { if (c.length > SANDBOX_PREFLIGHT_COMMAND_BYTES_MAX) errors.push(`command ${i + 1} is longer than ${SANDBOX_PREFLIGHT_COMMAND_BYTES_MAX} characters`); });
      return { value, errors };
    }

    function sandboxParseSeconds(text) {
      const s = String(text == null ? '' : text).trim();
      if (!s) return { value: 0, errors: [] };
      const n = Number(s);
      if (!Number.isInteger(n) || n < 0) return { value: 0, errors: ['a whole number of seconds, 0 or more'] };
      return { value: n, errors: [] };
    }

    function sandboxParseListenAddr(text) {
      const s = String(text || '').trim();
      if (s && !/^(\[[0-9A-Fa-f:.]+\]|[^:\s]*):[0-9]+$/.test(s)) return { value: s, errors: ['host:port, for example :8443'] };
      return { value: s, errors: [] };
    }

    function sandboxParseEndpoint(text) {
      const s = String(text || '').trim();
      if (!s) return { value: s, errors: [] };
      if (!/^https:\/\/[^\s/@]+(\/.*)?$/.test(s)) return { value: s, errors: ['an https://host:port URL with no user or password'] };
      return { value: s, errors: [] };
    }

    function sandboxSchedId(scope, field) { return 'sbx-' + scope + '-' + field; }

    function sandboxSchedInputValue(scope, field) {
      const el = document.getElementById(sandboxSchedId(scope, field));
      if (!el) return null;
      return el.type === 'checkbox' ? !!el.checked : el.value;
    }

    function sandboxSchedShowErrors(scope, field, errors) {
      const el = document.getElementById(sandboxSchedId(scope, field) + '-error');
      if (el) el.textContent = errors.length ? 'Not saved until fixed: ' + errors.join('; ') : '';
    }

    function sandboxSchedTextarea(scope, field, label, yaml, text, placeholder, help, rows, disabled) {
      const id = sandboxSchedId(scope, field);
      return `<div class="config-field" data-setting-key="${esc(yaml)}"><label for="${id}">${label} ${yamlHint(yaml)}</label>`
        + `<textarea id="${id}" rows="${rows || 2}" class="mono" spellcheck="false" placeholder="${esc(placeholder)}"${disabled ? ' disabled' : ''} data-change-action="markSandboxScheduling" data-arg0="${scope}" data-arg-types="s">${esc(text)}</textarea>`
        + `<div class="config-field-hint">${help}</div>`
        + `<div class="config-field-hint" id="${id}-error" role="alert" style="color:var(--red)"></div></div>`;
    }

    function sandboxSchedInput(scope, field, label, yaml, value, placeholder, help, type, disabled) {
      const id = sandboxSchedId(scope, field);
      const t = type || 'text';
      const extra = (t === 'number' ? ' min="0" step="1"' : ' spellcheck="false" autocomplete="off"') + (disabled ? ' disabled' : '');
      return `<div class="config-field" data-setting-key="${esc(yaml)}"><label for="${id}">${label} ${yamlHint(yaml)}</label>`
        + `<input type="${t}" id="${id}"${extra} value="${esc(value == null ? '' : String(value))}" placeholder="${esc(placeholder)}" data-change-action="markSandboxScheduling" data-arg0="${scope}" data-arg-types="s">`
        + `<div class="config-field-hint">${help}</div>`
        + `<div class="config-field-hint" id="${id}-error" role="alert" style="color:var(--red)"></div></div>`;
    }

    function sandboxSchedCheckbox(scope, field, label, yaml, checked, help) {
      const id = sandboxSchedId(scope, field);
      return `<div class="config-field" data-setting-key="${esc(yaml)}"><label for="${id}"><input type="checkbox" id="${id}"${checked ? ' checked' : ''} data-change-action="markSandboxScheduling" data-arg0="${scope}" data-arg-types="s"> ${label} ${yamlHint(yaml)}</label>`
        + `<div class="config-field-hint">${help}</div>`
        + `<div class="config-field-hint" id="${id}-error" role="alert" style="color:var(--red)"></div></div>`;
    }

    function sandboxEffectiveText(field, v) {
      if (field === 'nodeSelector' || field === 'podLabels' || field === 'podAnnotations') return sandboxMapText(v).split('\n').filter(Boolean).join(', ');
      if (field === 'resources') {
        const parts = [];
        const lim = sandboxMapText((v || {}).limits).split('\n').filter(Boolean).join(', ');
        const req = sandboxMapText((v || {}).requests).split('\n').filter(Boolean).join(', ');
        if (lim) parts.push('limits ' + lim);
        if (req) parts.push('requests ' + req);
        return parts.join('; ');
      }
      if (field === 'tolerations') return sandboxTolerationText(v).split('\n').filter(Boolean).join(', ');
      if (Array.isArray(v)) return v.join(', ');
      if (field === 'ttlSeconds' || field === 'scheduleTimeoutS') return v ? String(v) + ' s' : '';
      return v == null ? '' : String(v);
    }

    /* "In effect for this agent": the resolved value of every Job field and
       where it comes from, plus a warning when the agent's own job block hides
       hive-wide pod-template settings (a per-agent block replaces them whole). */
    function sandboxEffectiveHtml(f) {
      const eff = f.sandboxEffective || {};
      const job = eff.job || {};
      const src = eff.sources || {};
      const rows = [
        ['Node selector', 'nodeSelector'], ['Resources', 'resources'], ['Tolerations', 'tolerations'],
        ['Pod labels', 'podLabels'], ['Pod annotations', 'podAnnotations'],
        ['Service account', 'serviceAccount'], ['Scheduler name', 'schedulerName'], ['Image pull secrets', 'imagePullSecrets'],
        ['Job TTL', 'ttlSeconds'], ['Schedule timeout', 'scheduleTimeoutS'],
      ].map(([label, key]) => {
        const text = sandboxEffectiveText(key, job[key]);
        const from = key === 'scheduleTimeoutS' && !src[key] ? 'built-in default' : (SANDBOX_SOURCE_LABELS[src[key] || ''] || src[key]);
        return `<li data-effective-field="${key}"><strong>${label}:</strong> ${text ? `<code>${esc(text)}</code>` : 'none'} <span class="config-field-hint">(${esc(from)})</span></li>`;
      });
      const pre = eff.preflight || {};
      const cmds = (pre.commands || []).length;
      const preFrom = SANDBOX_SOURCE_LABELS[eff.preflightSource || ''] || eff.preflightSource;
      rows.push(`<li data-effective-field="preflight"><strong>Node preflight:</strong> ${cmds ? `${cmds} command${cmds === 1 ? '' : 's'}, ${Number(pre.timeoutS || 0)} s each, ${Number(pre.totalTimeoutS || 0)} s total` : 'off'} <span class="config-field-hint">(${esc(preFrom)})</span></li>`);
      const setup = eff.setup || {};
      const setupCmds = (setup.commands || []).length;
      const setupHealth = (setup.healthCommands || []).length;
      const setupFrom = SANDBOX_SOURCE_LABELS[eff.setupSource || ''] || eff.setupSource;
      const plural = (n, word) => `${n} ${word}${n === 1 ? '' : 's'}`;
      rows.push(`<li data-effective-field="setup"><strong>Workspace setup:</strong> ${setupCmds || setupHealth ? `${plural(setupCmds, 'setup command')}, ${plural(setupHealth, 'health check')}, ${Number(setup.timeoutS || 0)} s each, ${Number(setup.totalTimeoutS || 0)} s total` : 'off'} <span class="config-field-hint">(${esc(setupFrom)})</span></li>`);
      const hidden = Array.isArray(eff.hiddenDefaults) ? eff.hiddenDefaults : [];
      const hiddenHtml = hidden.length
        ? `<div class="config-field-hint" id="cfg-agent-sandbox-hidden-defaults" role="status" style="color:var(--yellow)">&#9888; This agent has its own Job block, so these hive-wide settings do not apply to it: ${hidden.map(h => `<code>agent_sandbox.job.${esc(h)}</code>`).join(', ')}. Set them below if this agent needs them.</div>`
        : '';
      return `<div class="config-field" id="cfg-agent-sandbox-effective"><label>In effect for this agent</label><ul class="config-help">${rows.join('')}</ul>${hiddenHtml}</div>`;
    }

    /* The Job scheduling, resources, pull secrets and preflight fields, plus
       (hive scope only) the GitHub read relay. scope 'hive' reads the Security
       section (agentSandbox* keys); scope 'agent' reads the General tab's
       sandboxFields. */
    function sandboxSchedulingHtml(scope, d) {
      d = d || {};
      const agent = scope === 'agent';
      const y = SANDBOX_SCHED_YAML[agent ? 'agent' : 'hive'];
      const own = agent ? (d.sandboxJob || {}) : {
        nodeSelector: d.agentSandboxJobNodeSelector, resources: d.agentSandboxJobResources, tolerations: d.agentSandboxJobTolerations,
        podLabels: d.agentSandboxJobPodLabels, podAnnotations: d.agentSandboxJobPodAnnotations,
        imagePullSecrets: d.agentSandboxJobImagePullSecrets, scheduleTimeoutS: d.agentSandboxJobScheduleTimeoutS,
      };
      const defaults = (agent ? d.sandboxPreflightDefaults : d.agentSandboxPreflightDefaults) || {};
      const eff = agent ? ((d.sandboxEffective || {}).job || {}) : {};
      const src = agent ? ((d.sandboxEffective || {}).sources || {}) : {};
      const inherited = (key, text) => (agent && src[key] === 'agent_sandbox' && text ? 'inherits ' + text.split('\n').join(', ') : '');
      const res = own.resources || {};
      const effRes = eff.resources || {};
      const pre = agent ? d.sandboxPreflight : d.agentSandboxPreflight;
      const preOverride = agent && pre != null;
      const preShown = pre || (agent ? (d.sandboxEffective || {}).preflight : null) || {};
      const preLocked = agent && !preOverride;
      const setupDefaults = (agent ? d.sandboxSetupDefaults : d.agentSandboxSetupDefaults) || {};
      const setup = agent ? d.sandboxSetup : d.agentSandboxSetup;
      const setupOverride = agent && setup != null;
      const setupShown = setup || (agent ? (d.sandboxEffective || {}).setup : null) || {};
      const setupLocked = agent && !setupOverride;
      const wholesaleNote = agent
        ? "An agent with any Job setting of its own uses its own node selector, resources, tolerations and service account instead of the hive-wide ones."
        : 'An agent with its own Job block uses its own node selector, resources, tolerations and service account instead of these.';
      let html = `<div class="settings-subsection-title mt-4">Job scheduling and resources</div>
          <p class="config-help">Where the sandbox Job may run and what it asks the cluster for (runtime <code>job</code> only). ${wholesaleNote}</p>
          ${agent ? sandboxEffectiveHtml(d) : ''}
          ${sandboxSchedTextarea(scope, 'nodeSelector', 'Node selector', y.nodeSelector, sandboxMapText(own.nodeSelector), inherited('nodeSelector', sandboxMapText(eff.nodeSelector)) || 'accelerator=spyre', 'One Kubernetes node label per line, <code>key=value</code>. The Job only runs on nodes carrying every label. Avoid <code>kubernetes.io/hostname</code>: it pins one node and defeats the preflight retry.', 3)}
          ${sandboxSchedTextarea(scope, 'podLabels', 'Pod labels', y.podLabels, sandboxMapText(own.podLabels), 'example.com/team=platform', 'One <code>key=value</code> per line. Agent entries override matching hive-wide entries; reserved Hive and Kubernetes keys cannot be set.', 2)}
          ${sandboxSchedTextarea(scope, 'podAnnotations', 'Pod annotations', y.podAnnotations, JSON.stringify(own.podAnnotations || {}, null, 2), '{"example.com/note":"value"}', 'A JSON object of string values. Agent entries override matching hive-wide entries; reserved Hive and Kubernetes keys cannot be set.', 3)}
          ${sandboxSchedTextarea(scope, 'limits', 'Resource limits', y.limits, sandboxMapText(res.limits), inherited('resources', sandboxMapText(effRes.limits)) || 'ibm.com/spyre_pf=1\nmemory=32Gi', 'One <code>resource=quantity</code> per line, for example <code>cpu=4</code>, <code>memory=32Gi</code>, or a device such as <code>ibm.com/spyre_pf=1</code>. Device (extended) resources are requested through limits.', 3)}
          ${sandboxSchedTextarea(scope, 'requests', 'Resource requests', y.requests, sandboxMapText(res.requests), inherited('resources', sandboxMapText(effRes.requests)) || 'cpu=500m', 'One <code>resource=quantity</code> per line. Leave empty to request what the limits say.', 2)}
          ${sandboxSchedTextarea(scope, 'tolerations', 'Tolerations', y.tolerations, sandboxTolerationText(own.tolerations), inherited('tolerations', sandboxTolerationText(eff.tolerations)) || 'accelerator:NoSchedule', 'One per line in taint syntax: <code>key=value:Effect</code> (operator Equal), <code>key:Effect</code> or <code>key</code> (operator Exists), <code>*</code> for every taint. Effect is NoSchedule, PreferNoSchedule or NoExecute; leave it out to match all. Also sets ' + y.tolerationParts.map(k => '<code>' + esc(k) + '</code>').join(', ') + '.', 2)}
          ${sandboxSchedTextarea(scope, 'imagePullSecrets', 'Image pull secrets (names)', y.imagePullSecrets, (own.imagePullSecrets || []).join('\n'), inherited('imagePullSecrets', (eff.imagePullSecrets || []).join('\n')) || 'one Secret name per line', 'Names of <code>kubernetes.io/dockerconfigjson</code> Secrets in the hive namespace, for a private registry. Names only, never a value.' + (agent ? ' Empty inherits the hive-wide list.' : ''), 2)}
          ${sandboxSchedInput(scope, 'scheduleTimeoutS', 'Schedule timeout (seconds)', y.scheduleTimeoutS, own.scheduleTimeoutS || '', agent && eff.scheduleTimeoutS ? 'in effect: ' + eff.scheduleTimeoutS : String(defaults.scheduleTimeoutS || ''), 'How long the pod may stay Unschedulable before the kick fails as <code>waiting_for_node</code>. 0 or empty ' + (agent ? 'inherits the hive-wide value, then ' : 'uses ') + 'the default of ' + Number(defaults.scheduleTimeoutS || 0) + ' s.', 'number')}
          <div class="settings-subsection-title mt-4">Node preflight</div>
          <p class="config-help">Shell commands run in the Job before the agent starts, each as <code>sh -lc</code> in the same image and workspace. A failing command marks the node <code>node_unhealthy</code> and hive retries the kick once on another node. They are written to the Job manifest and the pod log, so never put a credential on a command line.</p>`;
      if (agent) {
        html += sandboxSchedCheckbox(scope, 'preflightOverride', 'Use this agent\'s own preflight', y.preflightCommands, preOverride,
          'Off: this agent uses the hive-wide <code>agent_sandbox.preflight</code>. On: it uses the list below instead; an empty list turns preflight off for this agent.');
      }
      html += sandboxSchedTextarea(scope, 'preflightCommands', 'Preflight commands', y.preflightCommands, (preShown.commands || []).join('\n'), "python -c 'import torch; torch.ones(1).to(\"spyre\")'", 'One command per line, at most ' + Number(defaults.maxCommands || SANDBOX_PREFLIGHT_COMMANDS_MAX) + '.' + (agent && !preOverride ? ' Showing the hive-wide list; turn on the override above to edit it for this agent.' : ''), 3, preLocked);
      html += sandboxSchedInput(scope, 'preflightTimeoutS', 'Per-command timeout (seconds)', y.preflightTimeoutS, (pre || {}).timeoutS || '', String(defaults.timeoutS || ''), '0 or empty uses ' + (agent ? 'the hive-wide value, then ' : '') + 'the default of ' + Number(defaults.timeoutS || 0) + ' s.', 'number', preLocked);
      html += sandboxSchedInput(scope, 'preflightTotalTimeoutS', 'Total timeout (seconds)', y.preflightTotalTimeoutS, (pre || {}).totalTimeoutS || '', String(defaults.totalTimeoutS || ''), 'Bounds all commands together. 0 or empty uses ' + (agent ? 'the hive-wide value, then ' : '') + 'the default of ' + Number(defaults.totalTimeoutS || 0) + ' s.', 'number', preLocked);
      html += sandboxSchedInput(scope, 'preflightRetryDelayS', 'Retry delay (seconds)', y.preflightRetryDelayS, (pre || {}).retryDelayS || '', '0', 'Wait before re-creating the Job on another node after <code>node_unhealthy</code>. 0 retries at once.', 'number', preLocked);
      html += `<div class="settings-subsection-title mt-4">Workspace setup</div>
          <p class="config-help">Shell commands run in the Job after the node preflight and before the agent, each as <code>sh -lc</code> in the checked-out workspace with the agent's own environment, for example a repo script that installs a pinned toolchain from a package mirror. A failing setup command fails the kick as <code>setup_failed</code>, with no retry. A failing health check marks the node <code>node_unhealthy</code> and hive retries the kick once on another node, after the preflight retry delay, running setup again. Setup time counts toward the kick timeout. Commands are written to the Job manifest and the pod log, so never put a credential on a command line.</p>`;
      if (agent) {
        html += sandboxSchedCheckbox(scope, 'setupOverride', 'Use this agent\'s own setup', y.setupCommands, setupOverride,
          'Off: this agent uses the hive-wide <code>agent_sandbox.setup</code>. On: it uses the lists below instead; empty lists turn setup off for this agent.');
      }
      const setupMax = Number(setupDefaults.maxCommands || SANDBOX_PREFLIGHT_COMMANDS_MAX);
      const setupLockedNote = agent && !setupOverride ? ' Showing the hive-wide list; turn on the override above to edit it for this agent.' : '';
      html += sandboxSchedTextarea(scope, 'setupCommands', 'Setup commands', y.setupCommands, (setupShown.commands || []).join('\n'), './scripts/install-toolchain.sh', 'One command per line, run in order, at most ' + setupMax + '. Each must be safe to run again: a node retry repeats the whole step on the same workspace.' + setupLockedNote, 3, setupLocked);
      html += sandboxSchedTextarea(scope, 'setupHealthCommands', 'Health checks', y.setupHealthCommands, (setupShown.healthCommands || []).join('\n'), "python -c 'import torch; torch.ones(1)'", 'One command per line, run after the setup commands, at most ' + setupMax + '. A failure means the node cannot run what was installed.' + setupLockedNote, 2, setupLocked);
      html += sandboxSchedInput(scope, 'setupTimeoutS', 'Per-command timeout (seconds)', y.setupTimeoutS, (setup || {}).timeoutS || '', String(setupDefaults.timeoutS || ''), '0 or empty uses ' + (agent ? 'the hive-wide value, then ' : '') + 'the default of ' + Number(setupDefaults.timeoutS || 0) + ' s.', 'number', setupLocked);
      html += sandboxSchedInput(scope, 'setupTotalTimeoutS', 'Total timeout (seconds)', y.setupTotalTimeoutS, (setup || {}).totalTimeoutS || '', String(setupDefaults.totalTimeoutS || ''), 'Bounds setup commands and health checks together. 0 or empty uses ' + (agent ? 'the hive-wide value, then ' : '') + 'the default of ' + Number(setupDefaults.totalTimeoutS || 0) + ' s.', 'number', setupLocked);
      if (!agent) {
        const rp = d.agentSandboxJobGithubReadProxy || {};
        html += `<div class="settings-subsection-title mt-4">GitHub read relay</div>
          <p class="config-help">Off by default. When on, hive serves a TLS relay that adds a read-only, repo-scoped GitHub token to each Job's GitHub reads; writes are refused and the token never enters the Job. Hive-wide only: an agent's Job block cannot change it. Takes effect when hive restarts. You expose the listener to the Job pods yourself (a Service and a NetworkPolicy).</p>
          ${sandboxSchedCheckbox(scope, 'rpEnabled', 'Enable the GitHub read relay', 'agent_sandbox.job.github_read_proxy.enabled', !!rp.enabled, 'Needs a listen address and an https endpoint.')}
          ${sandboxSchedInput(scope, 'rpListenAddr', 'Listen address', 'agent_sandbox.job.github_read_proxy.listen_addr', rp.listenAddr || '', ':8443', 'The host:port hive binds. There is no default, so hive never opens a port you did not name.')}
          ${sandboxSchedInput(scope, 'rpEndpoint', 'Endpoint', 'agent_sandbox.job.github_read_proxy.endpoint', rp.endpoint || '', 'https://hive-read-relay:8443', 'The <code>https://host:port</code> Job pods use to reach the relay, for example the in-namespace Service. No user or password: hive adds a per-kick key itself.')}
          ${sandboxSchedTextarea(scope, 'rpNoProxy', 'Bypass hosts', 'agent_sandbox.job.github_read_proxy.no_proxy[]', (rp.noProxy || []).join('\n'), 'registry.internal', 'Extra hosts or domains a Job reaches directly, one per line, on top of localhost, .svc and .cluster.local.', 2)}`;
      }
      return html;
    }

    /* Reads every scheduling input of the scope and stages the valid ones.
       An input with an error shows it and is left out of the save. */
    function markSandboxScheduling(scope) {
      const agent = scope === 'agent';
      const staged = {};
      const take = (field, parsed, put) => {
        sandboxSchedShowErrors(scope, field, parsed.errors);
        if (!parsed.errors.length) put(parsed.value);
        return parsed.errors.length === 0;
      };
      const read = field => sandboxSchedInputValue(scope, field);
      if (read('nodeSelector') != null) take('nodeSelector', sandboxParseMapLines(read('nodeSelector'), 'label'), v => { staged.nodeSelector = v; });
      if (read('podLabels') != null) take('podLabels', sandboxParseMapLines(read('podLabels'), 'podLabel'), v => { staged.podLabels = v; });
      if (read('podAnnotations') != null) take('podAnnotations', sandboxParseAnnotations(read('podAnnotations')), v => { staged.podAnnotations = v; });
      const resources = {};
      if (read('limits') != null) take('limits', sandboxParseMapLines(read('limits'), 'quantity'), v => { resources.limits = v; });
      if (read('requests') != null) take('requests', sandboxParseMapLines(read('requests'), 'quantity'), v => { resources.requests = v; });
      if (Object.keys(resources).length) staged.resources = resources;
      if (read('tolerations') != null) take('tolerations', sandboxParseTolerationLines(read('tolerations')), v => { staged.tolerations = v; });
      if (read('imagePullSecrets') != null) take('imagePullSecrets', sandboxParseNameLines(read('imagePullSecrets')), v => { staged.imagePullSecrets = v; });
      if (read('scheduleTimeoutS') != null) take('scheduleTimeoutS', sandboxParseSeconds(read('scheduleTimeoutS')), v => { staged.scheduleTimeoutS = v; });

      let preflight;
      let preflightOk = true;
      const override = agent ? read('preflightOverride') : true;
      if (agent) {
        SANDBOX_PREFLIGHT_FIELDS.forEach(field => {
          const el = document.getElementById(sandboxSchedId(scope, field));
          if (el) el.disabled = !override;
        });
      }
      if (read('preflightCommands') != null) {
        const block = {};
        preflightOk = take('preflightCommands', sandboxParseCommandLines(read('preflightCommands')), v => { block.commands = v; });
        [['preflightTimeoutS', 'timeoutS'], ['preflightTotalTimeoutS', 'totalTimeoutS'], ['preflightRetryDelayS', 'retryDelayS']].forEach(([field, key]) => {
          if (read(field) == null) return;
          if (!take(field, sandboxParseSeconds(read(field)), v => { block[key] = v; })) preflightOk = false;
        });
        preflight = override ? block : null;
      }

      let setup;
      let setupOk = true;
      const setupOverride = agent ? read('setupOverride') : true;
      if (agent) {
        SANDBOX_SETUP_FIELDS.forEach(field => {
          const el = document.getElementById(sandboxSchedId(scope, field));
          if (el) el.disabled = !setupOverride;
        });
      }
      if (read('setupCommands') != null) {
        const block = {};
        setupOk = take('setupCommands', sandboxParseCommandLines(read('setupCommands')), v => { block.commands = v; });
        if (read('setupHealthCommands') != null && !take('setupHealthCommands', sandboxParseCommandLines(read('setupHealthCommands')), v => { block.healthCommands = v; })) setupOk = false;
        [['setupTimeoutS', 'timeoutS'], ['setupTotalTimeoutS', 'totalTimeoutS']].forEach(([field, key]) => {
          if (read(field) == null) return;
          if (!take(field, sandboxParseSeconds(read(field)), v => { block[key] = v; })) setupOk = false;
        });
        setup = setupOverride ? block : null;
      }

      if (agent) {
        const prev = (_configState.dirty.general || {}).sandboxJob || {};
        markDirty('general', 'sandboxJob', Object.assign({}, prev, staged));
        if (preflight !== undefined && preflightOk) markDirty('general', 'sandboxPreflight', preflight);
        if (setup !== undefined && setupOk) markDirty('general', 'sandboxSetup', setup);
        return;
      }
      const keys = { nodeSelector: 'agentSandboxJobNodeSelector', podLabels: 'agentSandboxJobPodLabels', podAnnotations: 'agentSandboxJobPodAnnotations', resources: 'agentSandboxJobResources', tolerations: 'agentSandboxJobTolerations', imagePullSecrets: 'agentSandboxJobImagePullSecrets', scheduleTimeoutS: 'agentSandboxJobScheduleTimeoutS' };
      Object.keys(staged).forEach(k => markDirty('security', keys[k], staged[k]));
      if (preflight !== undefined && preflightOk) markDirty('security', 'agentSandboxPreflight', preflight);
      if (setup !== undefined && setupOk) markDirty('security', 'agentSandboxSetup', setup);
      if (read('rpEnabled') != null) {
        const rp = { enabled: !!read('rpEnabled') };
        let ok = take('rpListenAddr', sandboxParseListenAddr(read('rpListenAddr')), v => { rp.listenAddr = v; });
        ok = take('rpEndpoint', sandboxParseEndpoint(read('rpEndpoint')), v => { rp.endpoint = v; }) && ok;
        ok = take('rpNoProxy', { value: String(read('rpNoProxy') || '').split(/\r?\n/).map(s => s.trim()).filter(Boolean), errors: [] }, v => { rp.noProxy = v; }) && ok;
        if (rp.enabled && (!rp.listenAddr || !rp.endpoint)) {
          sandboxSchedShowErrors(scope, 'rpEnabled', ['the relay needs a listen address and an endpoint']);
          ok = false;
        }
        if (ok) markDirty('security', 'agentSandboxJobGithubReadProxy', rp);
      }
    }
