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

    // ── Sandbox Job scheduling, resources, preflight, read relay (#19553) ──
    // Shared by the hive-wide fields (Settings, Security, Advanced sandbox)
    // and an agent's own (General, Sandbox settings). The same rules run
    // again in the API (api_sandbox_job_scheduling.go); these give feedback
    // before Save.

    const SANDBOX_LABEL_NAME_RE = /^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$/;
    const SANDBOX_DNS_SUBDOMAIN_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
    const SANDBOX_QUANTITY_RE = /^\+?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+|[KMGTPE]i|[numkMGTPE])?$/;
    const SANDBOX_SECRET_NAME_RE = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
    const SANDBOX_TOLERATION_EFFECTS = ['NoSchedule', 'PreferNoSchedule', 'NoExecute'];
    const SANDBOX_MAX_PREFLIGHT_COMMANDS = 32;
    const SANDBOX_MAX_PREFLIGHT_COMMAND_BYTES = 4096;

    /* A Kubernetes qualified name: [dns-subdomain/]name, the form of a label
       key and of a resource name (ibm.com/spyre_pf). */
    function sandboxQualifiedNameOK(s) {
      const parts = String(s).split('/');
      if (parts.length > 2) return false;
      const name = parts[parts.length - 1];
      if (parts.length === 2) {
        const prefix = parts[0];
        if (!prefix || prefix.length > 253 || !SANDBOX_DNS_SUBDOMAIN_RE.test(prefix)) return false;
      }
      return name.length <= 63 && SANDBOX_LABEL_NAME_RE.test(name);
    }

    function sandboxLabelValueOK(s) {
      return s === '' || (s.length <= 63 && SANDBOX_LABEL_NAME_RE.test(s));
    }

    function sandboxSplitLines(text) {
      return String(text || '').split(/\r?\n/).map(l => l.trim()).filter(Boolean);
    }

    function sandboxMapText(m) {
      return Object.keys(m || {}).sort().map(k => k + '=' + m[k]).join('\n');
    }

    /* Parses key=value lines. kind 'labels' checks label keys and values,
       'resources' resource names and quantities (2, 500m, 4Gi). */
    function sandboxParseMap(text, kind) {
      const out = {};
      for (const line of sandboxSplitLines(text)) {
        const idx = line.indexOf('=');
        if (idx <= 0) return { error: `"${line}" is not key=value` };
        const k = line.slice(0, idx).trim();
        const v = line.slice(idx + 1).trim();
        if (!sandboxQualifiedNameOK(k)) return { error: `"${k}" is not a valid Kubernetes ${kind === 'resources' ? 'resource name' : 'label key'}` };
        if (kind === 'resources') {
          if (!v || v.length > 64 || !SANDBOX_QUANTITY_RE.test(v)) return { error: `"${v}" is not a valid quantity for ${k} (for example 2, 500m or 4Gi)` };
        } else if (!sandboxLabelValueOK(v)) {
          return { error: `"${v}" is not a valid Kubernetes label value for ${k}` };
        }
        out[k] = v;
      }
      if (Object.keys(out).length > 64) return { error: 'at most 64 entries' };
      return { value: out };
    }

    /* A toleration per line: key=value[:Effect] (operator Equal),
       key[:Effect] (operator Exists), or * for every taint. */
    function sandboxTolerationText(t) {
      t = t || {};
      let s = t.key || '*';
      if (t.operator !== 'Exists') s += '=' + (t.value || '');
      if (t.effect) s += ':' + t.effect;
      return s;
    }

    function sandboxTolerationsText(list) {
      return (Array.isArray(list) ? list : []).map(sandboxTolerationText).join('\n');
    }

    function sandboxParseTolerations(text) {
      const out = [];
      for (const line of sandboxSplitLines(text)) {
        let body = line;
        let effect = '';
        const colon = body.lastIndexOf(':');
        if (colon >= 0) {
          effect = body.slice(colon + 1).trim();
          body = body.slice(0, colon).trim();
          if (!SANDBOX_TOLERATION_EFFECTS.includes(effect)) return { error: `"${line}": effect must be ${SANDBOX_TOLERATION_EFFECTS.join(', ')}` };
        }
        const t = {};
        const eq = body.indexOf('=');
        if (eq >= 0) {
          t.key = body.slice(0, eq).trim();
          t.operator = 'Equal';
          t.value = body.slice(eq + 1).trim();
          if (!t.key || t.key === '*') return { error: `"${line}": a key=value toleration needs a key` };
          if (!sandboxLabelValueOK(t.value)) return { error: `"${line}": "${t.value}" is not a valid label value` };
        } else {
          t.key = body === '*' ? '' : body;
          t.operator = 'Exists';
        }
        if (t.key && !sandboxQualifiedNameOK(t.key)) return { error: `"${line}": "${t.key}" is not a valid label key` };
        if (effect) t.effect = effect;
        out.push(t);
      }
      if (out.length > 32) return { error: 'at most 32 tolerations' };
      return { value: out };
    }

    /* Secret NAMES only; a pasted value (TOKEN=abc, user:pass) is refused. */
    function sandboxParseSecretNames(text) {
      const out = sandboxSplitLines(text);
      for (const n of out) {
        if (n.length > 253 || !SANDBOX_SECRET_NAME_RE.test(n)) return { error: `"${n}" is not a Secret name. Names only, never a value` };
      }
      if (out.length > 32) return { error: 'at most 32 names' };
      return { value: out };
    }

    function sandboxParseCommands(text) {
      const out = sandboxSplitLines(text);
      if (out.length > SANDBOX_MAX_PREFLIGHT_COMMANDS) return { error: `at most ${SANDBOX_MAX_PREFLIGHT_COMMANDS} commands` };
      for (const c of out) {
        if (c.length > SANDBOX_MAX_PREFLIGHT_COMMAND_BYTES) return { error: `a command is longer than ${SANDBOX_MAX_PREFLIGHT_COMMAND_BYTES} bytes` };
      }
      return { value: out };
    }

    /* A whole number >= 0 from an input; NaN when it is not one. */
    function sandboxParseSeconds(value) {
      if (value === '' || value == null) return 0;
      const n = Number(value);
      return Number.isInteger(n) && n >= 0 ? n : NaN;
    }

    /* The read relay rules config load applies: enabled needs a host:port
       listen_addr and an https endpoint with no credentials. A field set
       while disabled must still be well formed. */
    function sandboxReadRelayProblem(r) {
      r = r || {};
      const listen = String(r.listenAddr || '').trim();
      const endpoint = String(r.endpoint || '').trim();
      if (r.enabled && !listen) return 'listen_addr is required when the relay is on (for example :8443)';
      if (r.enabled && !endpoint) return 'endpoint is required when the relay is on (the https://host:port Job pods use)';
      if (listen && !/^(\[[^\]]*\]|[^:\s]*):[0-9a-zA-Z]+$/.test(listen)) return 'listen_addr must be host:port (the host may be empty)';
      if (endpoint) {
        let u = null;
        try { u = new URL(endpoint); } catch (_) { u = null; }
        if (!u || !u.hostname) return 'endpoint must be an https://host:port URL';
        if (u.protocol !== 'https:') return 'endpoint must use https://';
        if (u.username || u.password) return 'endpoint must not carry credentials; hive adds a per-kick key itself';
      }
      for (const h of (r.noProxy || [])) {
        if (/[\s,/@]/.test(h)) return `no_proxy: "${h}" is not a host or domain`;
      }
      return '';
    }

    /* Shows (or clears) the inline error under a field; a toast too when the
       dashboard has one. */
    function sandboxFieldError(id, msg) {
      const el = document.getElementById(id);
      if (el) el.textContent = msg || '';
      if (msg && typeof showToast === 'function') showToast(msg, 'error');
    }

    function sandboxSourceLabel(src) {
      if (src === 'agent') return "this agent's override";
      if (src === 'default') return 'built-in default';
      return 'from agent_sandbox defaults';
    }

    function sandboxEffectiveHtml(text, src) {
      return `<div class="config-field-hint sbx-effective">In effect: ${text ? `<code>${esc(text)}</code>` : 'none'} (${esc(sandboxSourceLabel(src))})</div>`;
    }

    function sandboxErrorSlot(id) {
      return `<div class="config-field-hint sbx-field-error" id="${id}" role="alert"></div>`;
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
          ${agentSandboxSchedulingHtml(f)}
        </details>`;
    }

    /* One textarea of the agent's own Job scheduling fields, with what is in
       effect and where it comes from. */
    function agentSandboxJobAreaHtml(label, yaml, key, text, placeholder, effectiveText, src) {
      const id = 'cfg-agent-sandbox-job-' + key;
      return `<div class="config-field"><label for="${id}">${label} ${yamlHint(yaml)}</label>
            <textarea id="${id}" rows="2" class="mono" placeholder="${esc(placeholder)}" spellcheck="false" data-change-action="markDirtySandboxJob" data-arg0="${key}" data-arg-types="s,v">${esc(text)}</textarea>
            ${sandboxEffectiveHtml(effectiveText, src)}${sandboxErrorSlot(id + '-error')}</div>`;
    }

    /* Node selector, resources, tolerations, pull secrets, schedule timeout
       and the preflight override of the agent's own sandbox block (#19553). */
    function agentSandboxSchedulingHtml(f) {
      const job = f.sandboxJob || {};
      const eff = f.sandboxJobEffective || {};
      const src = f.sandboxJobSources || {};
      const hidden = Array.isArray(f.sandboxJobHiddenGlobal) ? f.sandboxJobHiddenGlobal : [];
      const hasOwnJob = ['workspaceClaim', 'workspaceClaimMount', 'serviceAccount', 'schedulerName', 'ttlSeconds', 'scheduleTimeoutS']
        .some(k => job[k]) || ['envFromSecrets', 'imagePullSecrets', 'tolerations'].some(k => (job[k] || []).length)
        || ['nodeSelector', 'resourceLimits', 'resourceRequests'].some(k => Object.keys(job[k] || {}).length);
      const hiddenHtml = hidden.length
        ? `<div class="config-field-hint sbx-job-warning" id="cfg-agent-sandbox-job-hidden" role="alert">&#9888; This agent's own Job block replaces the hive-wide pod settings, so these <code>agent_sandbox.job</code> fields do not apply to it: ${hidden.map(h => '<code>' + esc(h) + '</code>').join(', ')}. Set them here to keep them.</div>`
        : '';
      const ownNote = hasOwnJob ? '' : `<div class="config-field-hint">This agent has no Job block of its own. Setting any Job field here starts one, and it replaces the hive-wide node selector, tolerations, resources, service account, scheduler name and secrets for this agent.</div>`;
      const pf = f.sandboxPreflight;
      const pfOwn = pf != null;
      const pfShown = pf || f.sandboxPreflightEffective || {};
      const pfEff = f.sandboxPreflightEffective || {};
      const pfEffText = (pfEff.commands || []).length
        ? (pfEff.commands || []).join('; ') + ` (timeout ${Number(pfEff.timeoutS || 0)}s, total ${Number(pfEff.totalTimeoutS || 0)}s, retry delay ${Number(pfEff.retryDelayS || 0)}s)`
        : '';
      const pfNum = (key, label) => `<div class="config-field"><label for="cfg-agent-sandbox-preflight-${key}">${label}</label>
            <input type="number" min="0" id="cfg-agent-sandbox-preflight-${key}" value="${pfOwn ? Number(pfShown[key] || 0) : 0}" data-change-action="markDirtySandboxPreflight" data-arg0="${key}" data-arg-types="s"></div>`;
      return `${hiddenHtml}${ownNote}
          ${agentSandboxJobAreaHtml('Job node selector', 'agents.<key>.sandbox.job.node_selector.<key>', 'nodeSelector', sandboxMapText(job.nodeSelector), 'one key=value per line, e.g. accelerator=spyre', sandboxMapText(eff.nodeSelector).replace(/\n/g, ', '), src.nodeSelector)}
          ${agentSandboxJobAreaHtml('Job resource limits', 'agents.<key>.sandbox.job.resources.limits.<key>', 'resourceLimits', sandboxMapText(job.resourceLimits), 'one name=quantity per line, e.g. ibm.com/spyre_pf=1', sandboxMapText(eff.resourceLimits).replace(/\n/g, ', '), src.resourceLimits)}
          ${agentSandboxJobAreaHtml('Job resource requests', 'agents.<key>.sandbox.job.resources.requests.<key>', 'resourceRequests', sandboxMapText(job.resourceRequests), 'one name=quantity per line, e.g. cpu=500m', sandboxMapText(eff.resourceRequests).replace(/\n/g, ', '), src.resourceRequests)}
          ${agentSandboxJobAreaHtml('Job tolerations', 'agents.<key>.sandbox.job.tolerations[]', 'tolerations', sandboxTolerationsText(job.tolerations), 'key=value:NoSchedule, key:NoExecute (Exists) or *', sandboxTolerationsText(eff.tolerations).replace(/\n/g, ', '), src.tolerations)}
          ${agentSandboxJobAreaHtml('Job image pull secrets (names)', 'agents.<key>.sandbox.job.image_pull_secrets[]', 'imagePullSecrets', (job.imagePullSecrets || []).join('\n'), 'one Secret name per line; empty inherits', (eff.imagePullSecrets || []).join(', '), src.imagePullSecrets)}
          <div class="config-field"><label for="cfg-agent-sandbox-job-scheduleTimeoutS">Job schedule timeout (seconds) ${yamlHint('agents.<key>.sandbox.job.schedule_timeout_s')}</label>
            <input type="number" min="0" id="cfg-agent-sandbox-job-scheduleTimeoutS" value="${job.scheduleTimeoutS ? Number(job.scheduleTimeoutS) : ''}" placeholder="0 inherits" data-change-action="markDirtySandboxJob" data-arg0="scheduleTimeoutS" data-arg-types="s,v">
            ${sandboxEffectiveHtml(eff.scheduleTimeoutS ? eff.scheduleTimeoutS + 's' : '', src.scheduleTimeoutS)}${sandboxErrorSlot('cfg-agent-sandbox-job-scheduleTimeoutS-error')}
            <div class="config-field-hint">A kick whose pod stays Unschedulable this long fails as <code>waiting_for_node</code>.</div></div>
          <div class="config-field"><label for="cfg-agent-sandbox-preflight-own"><input type="checkbox" id="cfg-agent-sandbox-preflight-own"${pfOwn ? ' checked' : ''} data-change-action="markDirtySandboxPreflight" data-arg0="own" data-arg-types="s"> Use this agent's own node preflight ${yamlHint('agents.<key>.sandbox.preflight.commands[]')}</label></div>
          <div class="config-field-hint">Off inherits <code>agent_sandbox.preflight</code>. On uses the list below; an empty list turns preflight off for this agent. A failing command reports <code>node_unhealthy</code> and retries once on another node.</div>
          <div class="config-field"><label for="cfg-agent-sandbox-preflight-commands">Preflight commands</label>
            <textarea id="cfg-agent-sandbox-preflight-commands" rows="2" class="mono" placeholder="one shell command per line" spellcheck="false" data-change-action="markDirtySandboxPreflight" data-arg0="commands" data-arg-types="s">${esc(pfOwn ? (pfShown.commands || []).join('\n') : '')}</textarea>
            ${sandboxEffectiveHtml(pfEffText, f.sandboxPreflightSource)}${sandboxErrorSlot('cfg-agent-sandbox-preflight-error')}
            <div class="config-field-hint">Commands are part of the Job manifest and their output is in the pod log: never put a credential on a command line.</div></div>
          ${pfNum('timeoutS', 'Preflight timeout per command (seconds) ' + yamlHint('agents.<key>.sandbox.preflight.timeout_s'))}
          ${pfNum('totalTimeoutS', 'Preflight total timeout (seconds) ' + yamlHint('agents.<key>.sandbox.preflight.total_timeout_s'))}
          ${pfNum('retryDelayS', 'Preflight retry delay (seconds) ' + yamlHint('agents.<key>.sandbox.preflight.retry_delay_s'))}`;
    }

    function markDirtySandboxJob(key, value) {
      let v = value;
      if (key === 'ttlSeconds') v = value === '' || value == null ? 0 : Number(value);
      if (key === 'envFromSecrets') v = String(value || '').split('\n').map(s => s.trim()).filter(Boolean);
      let parsed = null;
      if (key === 'nodeSelector') parsed = sandboxParseMap(value, 'labels');
      if (key === 'resourceLimits' || key === 'resourceRequests') parsed = sandboxParseMap(value, 'resources');
      if (key === 'tolerations') parsed = sandboxParseTolerations(value);
      if (key === 'imagePullSecrets') parsed = sandboxParseSecretNames(value);
      if (key === 'scheduleTimeoutS') {
        const n = sandboxParseSeconds(value);
        parsed = Number.isNaN(n) ? { error: 'a whole number of seconds, 0 or more' } : { value: n };
      }
      if (parsed) {
        sandboxFieldError('cfg-agent-sandbox-job-' + key + '-error', parsed.error ? 'Not saved: ' + parsed.error : '');
        if (parsed.error) return;
        v = parsed.value;
      }
      const prev = (_configState.dirty.general || {}).sandboxJob || {};
      markDirty('general', 'sandboxJob', Object.assign({}, prev, { [key]: v }));
    }

    /* Stages the agent's preflight override from its inputs: null inherits
       agent_sandbox.preflight. Editing a preflight field turns the override
       on, since a value there means nothing otherwise. */
    function markDirtySandboxPreflight(field) {
      const get = id => document.getElementById('cfg-agent-sandbox-preflight-' + id);
      const own = get('own');
      if (own && field !== 'own' && !own.checked) own.checked = true;
      if (!own || !own.checked) {
        sandboxFieldError('cfg-agent-sandbox-preflight-error', '');
        markDirty('general', 'sandboxPreflight', null);
        return;
      }
      const cmds = sandboxParseCommands(get('commands') ? get('commands').value : '');
      const next = { commands: cmds.value || [] };
      let problem = cmds.error || '';
      ['timeoutS', 'totalTimeoutS', 'retryDelayS'].forEach(k => {
        const n = sandboxParseSeconds(get(k) ? get(k).value : 0);
        if (Number.isNaN(n)) problem = problem || k + ' must be a whole number of seconds, 0 or more';
        next[k] = n;
      });
      sandboxFieldError('cfg-agent-sandbox-preflight-error', problem ? 'Not saved: ' + problem : '');
      if (problem) return;
      markDirty('general', 'sandboxPreflight', next);
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
          ['sandboxImage', 'sandboxRuntime', 'sandboxJob', 'sandboxPreflight', 'sandboxCopyFrom', 'sandboxEnabled'].forEach(k => { delete _configState.dirty.general[k]; });
        }
        showToast('Sandbox settings copied from ' + sel.value, 'success');
        renderConfigTab('General');
      } catch (err) {
        showToast('Failed to copy sandbox settings: ' + err.message, 'error');
      }
    }
