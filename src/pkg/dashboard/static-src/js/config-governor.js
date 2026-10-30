    // ── Governor Tab Renderers ──
    function renderGovGeneral() {
      const currentId = (window._lastStatus || {}).hiveId || '';
      // Hive ID edit lock (#7247). The server decides; the browser cannot tell
      // whether this hive is hub-managed. Default to LOCKED when the field is
      // absent so an older spoke that predates this flag fails safe (offering
      // an edit that always 409s is worse than hiding an available one).
      const hiveIdEditable = (window._lastStatus || {}).hiveIdEditable !== false;
      const hiveIdLockReason = (window._lastStatus || {}).hiveIdLockReason || '';
      const currentACMMLevel = (window._lastStatus || {}).acmmLevel || 0;
      const attrib = (_configState.data || {}).attribution || {};
      const genAdv = (_configState.data || {}).general_advanced || {};
      const attribOn = genAdv.attribution_trailer !== undefined ? genAdv.attribution_trailer !== false : attrib.attributionTrailer !== false; // default on
      const evalInterval = genAdv.eval_interval_s || 0;
      const explainMode = genAdv.explain_mode || '';
      return `
        <h3 style="margin:var(--sp-0) var(--sp-0) var(--sp-2);font-size:0.9rem;color:var(--text)">Governor</h3>
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">General hive instance settings. The governor's controls — agents, thresholds, budget, notifications, and more — live in the tabs above.</p>
        <div class="config-field">
          <label>Hive ID ${settingHelpMark('settings', 'general', 'Hive ID', `Unique identifier for this hive instance. Stamped into beads and available as \${HIVE_ID} in agent CLAUDE.md templates. Persists across container restarts.`)}</label>
          <div style="display:flex;gap:var(--sp-4);align-items:center">
            <input type="text" id="cfg-hive-id" value="${esc(currentId)}" placeholder="hive-xxxxxxxx" style="flex:1" ${hiveIdEditable ? '' : 'readonly disabled'} data-input-action="markDirty" data-arg0="general" data-arg1="hiveId" data-arg-types="s,s,v">
            ${hiveIdEditable ? '<button class="hv-btn btn-primary btn-sm" data-action="saveHiveId">Save</button>' : '<span style="font-size:0.7rem;color:var(--muted);white-space:nowrap">&#128274; Hub-managed</span>'}
          </div>
          <span style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:block">${hiveIdEditable ? 'Format: hive-adjective-noun (e.g. hive-bold-hawk). Lowercase letters, digits and hyphens only. Changes take effect immediately.' : esc(hiveIdLockReason)}</span>
        </div>
        <div class="config-field" data-setting-key="acmm_level">
          <label>Maturity Level ${settingHelpMark('agent-configuration', 'acmm-levels-agent-rosters-as-packs', 'Maturity Level', `ACMM (AI Codebase Maturity Model) level — controls which agents run and how autonomous they are, from advisory-only (L1) to autonomous (L6).`)}</label>
          <div style="display:flex;gap:10px;align-items:center">
            <span style="display:inline-block;padding:3px 12px;border-radius:var(--r-pill);font-size:var(--fs-sm);font-weight:700;background:color-mix(in srgb, var(--amber) 14%, transparent);border:1px solid color-mix(in srgb, var(--amber) 45%, transparent);color:var(--amber)">${currentACMMLevel > 0 ? acmmLevelLabel(currentACMMLevel) : 'Not set'}</span>
            <button class="hv-btn btn-secondary btn" data-action="gh41" title="Open the ACMM maturity level selector">Change&hellip;</button>
          </div>
          <span style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:block">Each level applies a curated agent pack. Change&hellip; opens the level selector.</span>
          <div class="mt-4">${securityPostureStripHtml()}</div>
        </div>
        <div class="config-field" style="margin-top:14px">
          <label>Eval interval (seconds) ${yamlHint('governor.eval_interval_s')} ${settingHelpMark('settings', 'general', 'Eval interval (seconds)', `How often the governor evaluates the hive (sensing, mode ladder, agent kicks). Default 300. Lower = more responsive, more API calls.`)}</label>
          <input type="number" min="10" max="86400" value="${evalInterval || ''}" placeholder="300" data-change-action="gh70" style="width:140px">
          <span style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:block">Applies from the next eval cycle.</span>
        </div>
        <div class="config-field" style="margin-top:14px">
          <label>Default explain mode ${yamlHint('governor.explain_mode')} ${settingHelpMark('agent-configuration', 'fleet-wide-default', 'Default explain mode', `Hive-wide default for agents that leave their own Explain Mode unset. Asks agents to say WHY they made each tool call, on EXPLAIN: lines alongside the work &mdash; they still act, they do not narrate instead. Costs extra tokens on every kick, so leave it off unless you are debugging. An agent with its own explicit value, including &quot;off&quot;, keeps it. Takes precedence over the HIVE_EXPLAIN_MODE deployment environment variable.`)}</label>
          <select data-change-action="gh75" style="width:320px">
            ${HIVE_EXPLAIN_MODES.map(m => `<option value="${m.value}" ${explainMode === m.value ? 'selected' : ''}>${esc(m.label)}</option>`).join('')}
          </select>
          <span id="gov-explain-default-note" style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:block">${explainDefaultNoteHtml(genAdv)}</span>
        </div>
        <div class="config-toggle" style="margin-top:14px">
          <div class="config-toggle-switch ${attribOn ? 'on' : ''}" data-action="toggleAttributionTrailer" data-arg-types="t"></div>
          <span class="config-toggle-label">Show attribution trailer on PRs/issues ${settingHelpMark('settings', 'general', 'Show attribution trailer on PRs/issues', `Append attribution trailer (agent, backend, model) to agent-created PRs and issues &mdash; audit log always records regardless. Hive-wide: one setting for all agents.`)}</span>
        </div>
        ${renderGovLoopPacing(genAdv)}`;
    }

    // renderGovLoopPacing is the governor eval-loop pacing block on the General
    // tab. The fields stage into the general-advanced section, so the footer
    // Save sends them through PUT /api/config/governor/general-advanced. The
    // loop recomputes its interval and sweep cadence every cycle, so a saved
    // change applies from the next eval. 0 or empty restores the default.
    function renderGovLoopPacing(genAdv) {
      const d = genAdv.loop_defaults || {};
      const owner = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const num = (key, label, path, help, tip) => `
          <div class="config-field">
            <label for="gov-loop-${key}">${esc(label)} ${yamlHint(path)} ${settingHelpMark('settings', 'general', label, tip)}</label>
            <input id="gov-loop-${key}" type="number" min="0" step="1" value="${genAdv[key] ? esc(String(genAdv[key])) : ''}" placeholder="${esc(String(d[key] || ''))}" data-change-action="markDirty" data-arg0="general-advanced" data-arg1="${key}" data-arg-types="s,s,N" ${owner ? '' : 'disabled'}>
            <div class="config-help">${help} Applies from the next eval cycle. 0 restores the default.</div>
          </div>`;
      const src = genAdv.thresholds_source || '';
      const srcText = src === 'pack'
        ? 'pack — seeded by an ACMM pack, so the thresholds still scale with repo count'
        : 'operator — set by hand (or never seeded), so explicit thresholds are used verbatim';
      return `
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Eval loop pacing</div>
          <div class="config-field-row">
            ${num('eval_interval_max_s', 'Max eval interval (seconds)', 'governor.eval_interval_max_s', 'Cap when API budget mode stretches evals.', 'Upper bound on the eval interval after the API-budget stretch and the webhook interval are applied. Must be at least the eval interval. Default 1800.')}
            ${num('eval_interval_webhook_s', 'Webhook eval interval (seconds)', 'governor.eval_interval_webhook_s', 'Base interval while GitHub webhooks are healthy.', 'While signed GitHub webhooks keep arriving the base eval interval rises to this value, because webhooks already trigger the work polling would find. Capped by the max eval interval. Default 900.')}
          </div>
          <div class="config-field-row">
            ${num('webhook_wake_debounce_s', 'Webhook wake debounce (seconds)', 'governor.webhook_wake_debounce_s', 'How long a verified webhook delivery waits for more before it wakes an early eval cycle.', 'A verified GitHub webhook that changes a PR or issue schedules an early eval cycle; deliveries arriving within this window are coalesced into that one cycle, which re-enriches only the items the webhooks marked dirty. Default 10.')}
            ${num('webhook_wake_min_gap_s', 'Webhook wake minimum gap (seconds)', 'governor.webhook_wake_min_gap_s', 'Shortest gap between two webhook-triggered eval cycles.', 'Caps how often webhook deliveries can wake the loop: a wake that would land sooner than this after the previous webhook-triggered cycle is deferred to the gap. Default 30.')}
          </div>
          <div class="config-field-row">
            ${num('conserve_interval_multiplier', 'Conserve-mode multiplier', 'governor.conserve_interval_multiplier', 'Eval interval multiplier in API budget conserve mode.', 'When remaining GitHub API quota drops below the reserve, the eval interval is multiplied by this factor. Default 2.')}
            ${num('optional_sweep_every_n_cycles', 'Optional sweeps every N cycles', 'governor.optional_sweep_every_n_cycles', 'Run optional sweeps every Nth normal cycle (1 = every cycle).', 'The optional enrichment steps of an eval cycle run only on every Nth cycle while the API budget is normal; in conserve or critical mode they are skipped regardless. Default 1, every cycle.')}
          </div>
          <div class="config-field-row">
            ${num('provider_budget_probe_interval_s', 'Provider spend-limit probe (seconds)', 'governor.provider_budget.probe_interval_s', 'How long a provider spending-limit refusal suppresses kicks before one cycle probes again.', 'After the inference provider refuses on a spending limit, kicks are withheld; once the last refusal is older than this, one cycle&#39;s kicks go through to test whether the provider serves again. Shorter recovers sooner but burns a run on a still-clipped provider. Default 1800.')}
          </div>
          <div class="config-field">
            <label>Threshold ownership ${yamlHint('governor.thresholds_source')}</label>
            <div class="fs-sm">${esc(srcText)} <span class="text-muted">— read-only; editing any threshold on the Thresholds tab makes them operator-owned, applying a pack re-seeds them.</span></div>
          </div>
          ${owner ? '<div class="config-help">Save with the dialog Save button.</div>' : '<div class="config-help">Owner role required to change these settings.</div>'}
        </div>`;
    }

    // opts.note replaces the default "next restart" wording for a field that
    // does NOT need one — the explain-mode default is re-read on every kick, and
    // telling an operator to restart for it would be wrong. opts.refresh is a
    // callback that repaints whatever derived read-out the field owns, given the
    // server's response.
    async function saveGovGeneralAdvanced(field, value, opts) {
      if (value === null) return; // empty input — leave the setting untouched
      opts = opts || {};
      try {
        const body = {}; body[field] = value;
        const res = await fetch('/api/config/governor/general-advanced', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify(body),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        _configState.data.general_advanced = data;
        if (opts.refresh) opts.refresh(data);
        showToast(opts.note || 'Governor settings updated — effective on next restart');
      } catch (err) {
        showToast('Failed to update governor settings: ' + err.message, 'error');
      }
    }

    async function toggleAttributionTrailer(el) {
      const turningOn = !el.classList.contains('on');
      el.classList.toggle('on');
      try {
        const res = await fetch('/api/config/governor/general-advanced', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ attribution_trailer: turningOn }),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const data = await res.json();
        _configState.data.general_advanced = data;
        if (!_configState.data.attribution) _configState.data.attribution = {};
        _configState.data.attribution.attributionTrailer = turningOn;
        showToast('Attribution trailer ' + (turningOn ? 'enabled' : 'disabled'));
      } catch (err) {
        el.classList.toggle('on'); // revert on failure
        showToast('Failed to update attribution trailer: ' + err.message, 'error');
      }
    }

    async function toggleTrajectoryReview(el) {
      const turningOn = !el.classList.contains('on');
      el.classList.toggle('on');
      try {
        const res = await fetch('/api/config/governor/trajectory', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ enabled: turningOn }),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        if (!_configState.data.trajectory) _configState.data.trajectory = {};
        _configState.data.trajectory.enabled = turningOn;
        showToast('Trajectory review ' + (turningOn ? 'enabled' : 'disabled') + ' — effective on next restart');
      } catch (err) {
        el.classList.toggle('on'); // revert on failure
        showToast('Failed to update trajectory review: ' + err.message, 'error');
      }
    }

    async function saveTrajectoryField(field, value) {
      try {
        const body = {}; body[field] = value;
        const res = await fetch('/api/config/governor/trajectory', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify(body),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        if (!_configState.data.trajectory) _configState.data.trajectory = {};
        _configState.data.trajectory[field] = value;
        // Re-render so the status chip / resolved-endpoint line reflect the change.
        renderConfigTab('General');
        showToast('Trajectory review updated — effective on next restart');
      } catch (err) {
        showToast('Failed to update trajectory review: ' + err.message, 'error');
      }
    }

    async function saveHiveId() {
      const input = document.getElementById('cfg-hive-id');
      if (!input) return;
      // Second line of defence (#7247): the control is not rendered when the
      // ID is hub-managed, but a stale DOM or a console call could still reach
      // here. The server refuses with 409 regardless; this just gives the
      // operator the reason instead of a generic failure toast.
      if ((window._lastStatus || {}).hiveIdEditable === false) {
        showToast((window._lastStatus || {}).hiveIdLockReason || 'Hive ID is managed by the hub', 'error');
        return;
      }
      const newId = input.value.trim();
      if (!newId) { showToast('Hive ID cannot be empty', 'error'); return; }
      try {
        const res = await fetch('/api/hive-id', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ id: newId }),
        });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        showToast('Hive ID updated to ' + newId);
      } catch (err) {
        showToast('Failed to save Hive ID: ' + err.message, 'error');
      }
    }

    function renderGovAgents(agents) {
      const sec = (_configState.data || {}).security || {};
      const backendPolicy = (_configState.data || {}).backends || {};
      const live = window._lastAgents || [];
      const liveMap = {};
      live.forEach(a => { liveMap[a.name] = a; });
      const list = agents.map(a => {
        const name = typeof a === 'string' ? a : (a && a.name) || '';
        const sandboxed = (liveMap[name] && liveMap[name].sandboxed) || false;
        const badge = sandboxed ? ' <span class="status-badge done" title="This agent is opted into the sandbox and the global sandbox gate is on">sandboxed</span>' : '';
        return `<li><span class="agent-link" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(name)}">${esc(name)}</span>${badge}<button class="remove-agent" data-action="removeAgent" data-agent="${esc(name)}">✕</button></li>`;
      }).join('');
      const sandboxNote = sec.agentSandboxEnabled
        ? `${sec.sandboxedAgents || 0}/${sec.totalAgents || agents.length} agents sandboxed`
        : 'Sandbox global gate is off — per-agent opt-ins are staged but not active.';
      return `
        <p class="fs-sm text-muted mb-4">Managed agents. Click name to configure.</p>
        <div style="font-size:0.72rem;color:var(--muted);margin-bottom:10px">${esc(sandboxNote)}</div>
        <ul class="config-agent-list">${list}</ul>
        <div class="config-list-add mt-5">
          <input type="text" id="cfg-new-agent" placeholder="agent name" title="Internal name for the new agent. Used as the tmux session name and config file name. Use lowercase with no spaces (e.g. scanner, code-reviewer).">
          <select id="cfg-copy-from" style="margin-left:var(--sp-3);font-size:var(--fs-sm);background:var(--surface);color:var(--text);border:1px solid var(--border);border-radius:var(--r-sm);padding:var(--sp-2) var(--sp-3)" title="Optionally copy configuration from an existing agent. Select 'blank' to start with default settings.">
            <option value="">blank</option>
            ${agents.map(a => { const name = typeof a === 'string' ? a : (a && a.name) || ''; return '<option value="' + esc(name) + '">copy ' + esc(name) + '</option>'; }).join('')}
          </select>
          <button class="hv-btn btn-primary" data-action="addAgent" title="Create the new agent with the specified name and optional copied config">+ Add Agent</button>
        </div>
        <div class="config-section mt-6">
          <h3>Backend placement policy</h3>
          <p class="text-muted fs-base">Restrict which CLI or inference backends agents may use. Deny wins over allow; an empty allow list permits every supported backend.</p>
          <div class="config-field-row">
            <div class="config-field"><label>Allowed backends ${yamlHint('backends.allow[]')}</label><textarea rows="4" placeholder="copilot&#10;claude" data-change-action="markBackendsPolicy" data-arg0="allow" data-arg-types="s,v">${esc((backendPolicy.allow || []).join('\n'))}</textarea></div>
            <div class="config-field"><label>Denied backends ${yamlHint('backends.deny[]')}</label><textarea rows="4" placeholder="codex" data-change-action="markBackendsPolicy" data-arg0="deny" data-arg-types="s,v">${esc((backendPolicy.deny || []).join('\n'))}</textarea></div>
          </div>
          <div class="config-help">Use backend IDs such as <code>copilot</code>, <code>claude</code>, <code>gemini</code>, <code>goose</code>, <code>codex</code>, <code>bob</code>, <code>vllm</code>, <code>llm-d</code>, or a configured gateway name.</div>
        </div>`;
    }

    const GOVERNOR_MODE_COLORS = { idle: 'var(--green)', quiet: 'var(--blue)', busy: 'var(--yellow)', surge: 'var(--red)' };

    function governorTrackMax(effectiveSurge, pressure) {
      const surge = Math.max(0, Number(effectiveSurge) || 0);
      const current = Math.max(0, Number(pressure) || 0);
      return Math.max(surge + 10, current + 5, 1);
    }

    function governorPct(value, max) {
      const n = Math.max(0, Number(value) || 0);
      const cap = Math.max(1, Number(max) || 1);
      return Math.min((n / cap) * 100, 100);
    }

    function governorSegmentBoundaries(quiet, busy, surge, pressure) {
      const max = governorTrackMax(surge, pressure);
      return {
        max,
        quietPct: governorPct(quiet, max),
        busyPct: governorPct(busy, max),
        surgePct: governorPct(surge, max),
        pressurePct: governorPct(pressure, max),
      };
    }

    function governorPressureBarBoundaries(effectiveThresholds, pressure) {
      const t = effectiveThresholds || {};
      return governorSegmentBoundaries(t.quiet || 2, t.busy || 10, t.surge || 20, pressure);
    }

    function governorThresholdSliderBoundaries(effectiveThresholds, pressure) {
      return governorPressureBarBoundaries(effectiveThresholds, pressure);
    }

    function governorModeGradient(bounds) {
      return `linear-gradient(to right,
        ${GOVERNOR_MODE_COLORS.idle} 0%,${GOVERNOR_MODE_COLORS.idle} ${bounds.quietPct}%,
        ${GOVERNOR_MODE_COLORS.quiet} ${bounds.quietPct}%,${GOVERNOR_MODE_COLORS.quiet} ${bounds.busyPct}%,
        ${GOVERNOR_MODE_COLORS.busy} ${bounds.busyPct}%,${GOVERNOR_MODE_COLORS.busy} ${bounds.surgePct}%,
        ${GOVERNOR_MODE_COLORS.surge} ${bounds.surgePct}%,${GOVERNOR_MODE_COLORS.surge} 100%)`;
    }

    function governorScaleThreshold(base, repoCount, scaling) {
      const n = Number(base) || 0;
      if (n <= 0) return n;
      const repos = Math.max(1, Number(repoCount) || 1);
      if (scaling === 'none') return n;
      if (scaling === 'sqrt') return n * Math.ceil(Math.sqrt(repos));
      return n * repos;
    }

    function governorScalingFactor(repoCount, scaling, activeScope) {
      const repos = Math.max(1, Number(repoCount) || 1);
      if (activeScope === 'per_repo' || scaling === 'none' || repos <= 1) return 1;
      if (scaling === 'sqrt') return Math.ceil(Math.sqrt(repos));
      return repos;
    }

    function governorScalingSummary(repoCount, scaling, activeScope) {
      const repos = Math.max(1, Number(repoCount) || 1);
      const factor = governorScalingFactor(repos, scaling, activeScope);
      if (factor <= 1) return '';
      if (scaling === 'sqrt') return `Scaling: Square-root × ceil(sqrt(${repos} watched repos)) = ×${factor}`;
      return `Scaling: Linear × ${repos} watched repos`;
    }

    function governorThresholdIsPinned(d, key) {
      const dirty = (_configState.dirty && _configState.dirty.thresholds) || {};
      if (Object.prototype.hasOwnProperty.call(dirty, key)) return true;
      const pinned = (d && d.pinnedThresholds) || {};
      return !!pinned[key];
    }

    function governorStaggerLabels(labels, minGapPct) {
      const gap = Number(minGapPct) || 8;
      const sorted = (labels || []).map((label, idx) => Object.assign({ idx, pct: 0 }, label)).sort((a, b) => a.pct - b.pct);
      const laneEnds = [];
      for (const label of sorted) {
        let lane = laneEnds.findIndex(end => (label.pct - end) >= gap);
        if (lane < 0) lane = laneEnds.length;
        laneEnds[lane] = label.pct;
        label.lane = lane;
      }
      return sorted.sort((a, b) => a.idx - b.idx).map(({ idx, ...label }) => label);
    }

    function governorPressureFromStatus(status) {
      const gov = (status && status.governor) || {};
      return (Number(gov.issues) || 0) + (Number(gov.prs) || 0);
    }

    function governorEffectiveThresholds(d, base, activeScope, activeScaling) {
      const repoCount = (d && d.repoCount) || 1;
      const dirty = (_configState.dirty && _configState.dirty.thresholds) || {};
      const hasDirtyThreshold = Object.prototype.hasOwnProperty.call(dirty, 'quiet') ||
        Object.prototype.hasOwnProperty.call(dirty, 'busy') ||
        Object.prototype.hasOwnProperty.call(dirty, 'surge');
      const scalingDirty = _configState.dirty && _configState.dirty['threshold-scaling'];
      const scopeDirty = _configState.dirty && _configState.dirty['cadence-scope'];
      const serverEff = (d && d.effectiveThresholds) || {};
      if (!hasDirtyThreshold && !scalingDirty && !scopeDirty && serverEff.quiet && serverEff.busy && serverEff.surge) {
        return { quiet: serverEff.quiet, busy: serverEff.busy, surge: serverEff.surge };
      }
      const scaleRepos = activeScope === 'per_repo' ? 1 : repoCount;
      const out = {
        quiet: governorScaleThreshold(base.quiet, scaleRepos, activeScaling),
        busy: governorScaleThreshold(base.busy, scaleRepos, activeScaling),
        surge: governorScaleThreshold(base.surge, scaleRepos, activeScaling),
      };
      for (const key of ['quiet', 'busy', 'surge']) {
        if (governorThresholdIsPinned(d, key)) out[key] = base[key];
      }
      return out;
    }

    function governorThresholdLabel(baseValue, effectiveValue, pinned) {
      if (pinned) return `${baseValue} pinned`;
      return Number(baseValue) === Number(effectiveValue) ? String(baseValue) : `${baseValue} → ${effectiveValue}`;
    }

    function governorThresholdHintText(baseValue, effectiveValue, pinned, repoCount, scaling, activeScope) {
      const repos = Math.max(1, Number(repoCount) || 1);
      const factor = governorScalingFactor(repos, scaling, activeScope);
      if (factor <= 1) return '';
      if (pinned) return `${effectiveValue} (pinned, scaling off)`;
      if (scaling === 'sqrt') return `${baseValue} × ceil(sqrt(${repos} repos)) = ${effectiveValue} in force`;
      return `${baseValue} × ${repos} repos = ${effectiveValue} in force`;
    }

    // renderGovSLATargets: the Delivery SLA budgets (hours) the Throughput panel
    // colours against. Saved through PUT /api/config/sla (top-level block).
    function renderGovSLATargets(d) {
      const sla = (d && d.sla) || {};
      const eff = sla.effective_targets || {};
      const defaults = sla.defaults || { start_to_pr_h: 4, pr_to_merge_h: 24, open_to_merge_h: 72 };
      const dirty = _configState.dirty.sla || {};
      const fields = [
        ['start_to_pr_h', 'Start → PR', 'Hours from the stage/030-pr-in-progress start mark to the fixing PR opening.'],
        ['pr_to_merge_h', 'PR → merge', 'Hours from the fixing PR opening to its merge.'],
        ['open_to_merge_h', 'Open → merge', 'Hours from the issue opening on GitHub to the fixing PR merging.'],
      ];
      return `<div class="config-field mb-7">
          <label>Delivery SLA targets (hours) ${settingHelpMark('operator-reference', 'delivery-sla', 'Delivery SLA targets', `Per-span budgets the Throughput section's Delivery SLA tiles and sparklines are coloured against: green within target, amber up to ${PRT_SLA_WARN_RATIO}× target, red beyond. 0 restores the default (${defaults.start_to_pr_h} / ${defaults.pr_to_merge_h} / ${defaults.open_to_merge_h} h).`)}</label>
          <div class="config-field-row">${fields.map(([key, label, tip]) => {
            const v = dirty[key] !== undefined ? dirty[key] : (eff[key] !== undefined ? eff[key] : defaults[key]);
            return `<div class="config-field" data-setting-key="sla.targets.${esc(key)}"><label for="cfg-sla-${esc(key)}" title="${esc(tip)}">${esc(label)}</label><input type="number" min="0" max="8760" step="0.5" id="cfg-sla-${esc(key)}" value="${esc(String(v))}" data-input-action="markDirty" data-arg0="sla" data-arg1="${esc(key)}" data-arg-types="s,s,N" data-setting-path="sla.targets.${esc(key)}" aria-label="${esc(label)} target in hours"></div>`;
          }).join('')}</div>
        </div>`;
    }

    function renderGovThresholds(d) {
      const t = (d && d.thresholds) || {};
      const merged = Object.assign({}, t, _configState.dirty.thresholds || {});
      const q = merged.quiet || 2, b = merged.busy || 10, s = merged.surge || 20;
      const base = { quiet: q, busy: b, surge: s };
      // The handles edit BASE thresholds; the governor ladders on the effective
      // ones, which for an unset mode are the base scaled by repo count (#3498).
      // Show both — a 39-repo hive surges at 780, far past this bar's range, and
      // a slider reading 20 with no further explanation would be a lie.
      const repoCount = (d && d.repoCount) || 1;
      const scope = ((d && d.cadenceScope) || 'aggregate');
      const scopeDirty = (_configState.dirty['cadence-scope'] || {}).cadenceScope;
      const activeScope = scopeDirty !== undefined ? scopeDirty : scope;
      const scaling = ((d && d.thresholdScaling) || 'linear');
      const scalingDirty = (_configState.dirty['threshold-scaling'] || {}).thresholdScaling;
      const activeScaling = scalingDirty !== undefined ? scalingDirty : scaling;
      const coverageTargetDirty = (_configState.dirty['coverage-target'] || {}).coverageTarget;
      const activeCoverageTarget = coverageTargetDirty !== undefined ? coverageTargetDirty : ((d && d.coverageTarget) || 91);
      const scaled = activeScaling !== 'none' && repoCount > 1 && activeScope !== 'per_repo';
      const effective = governorEffectiveThresholds(d, base, activeScope, activeScaling);
      const scalingSummary = governorScalingSummary(repoCount, activeScaling, activeScope);
      const pinned = {
        quiet: governorThresholdIsPinned(d, 'quiet'),
        busy: governorThresholdIsPinned(d, 'busy'),
        surge: governorThresholdIsPinned(d, 'surge'),
      };
      const pressure = governorPressureFromStatus(window._lastStatus || {});
      const bounds = governorThresholdSliderBoundaries(effective, pressure);
      const maxValue = bounds.max;
      const labels = governorStaggerLabels([
        { key: 'quiet', pct: bounds.quietPct, text: governorThresholdLabel(q, effective.quiet, pinned.quiet), color: GOVERNOR_MODE_COLORS.quiet },
        { key: 'busy', pct: bounds.busyPct, text: governorThresholdLabel(b, effective.busy, pinned.busy), color: GOVERNOR_MODE_COLORS.busy },
        { key: 'surge', pct: bounds.surgePct, text: governorThresholdLabel(s, effective.surge, pinned.surge), color: GOVERNOR_MODE_COLORS.surge },
      ], 11);
      const hints = {
        quiet: scaled ? governorThresholdHintText(q, effective.quiet, pinned.quiet, repoCount, activeScaling, activeScope) : '',
        busy: scaled ? governorThresholdHintText(b, effective.busy, pinned.busy, repoCount, activeScaling, activeScope) : '',
        surge: scaled ? governorThresholdHintText(s, effective.surge, pinned.surge, repoCount, activeScaling, activeScope) : '',
      };
      const zoneLabels = governorStaggerLabels([
        { key: 'idle', pct: bounds.quietPct / 2, color: GOVERNOR_MODE_COLORS.idle },
        { key: 'quiet', pct: (bounds.quietPct + bounds.busyPct) / 2, color: GOVERNOR_MODE_COLORS.quiet },
        { key: 'busy', pct: (bounds.busyPct + bounds.surgePct) / 2, color: GOVERNOR_MODE_COLORS.busy },
        { key: 'surge', pct: (bounds.surgePct + 100) / 2, color: GOVERNOR_MODE_COLORS.surge },
      ], 14);
      const effNote = scaled && effective.surge
        ? `<p style="font-size:var(--fs-sm);color:var(--muted);margin:-12px 0 20px">
             In force for <strong>${repoCount}</strong> watched repos:
             quiet <strong>${effective.quiet}</strong> · busy <strong>${effective.busy}</strong> · surge <strong>${effective.surge}</strong>.
             Dragging a handle pins that threshold to an absolute value and opts it out of scaling.
           </p>`
        : activeScope === 'per_repo'
          ? `<p style="font-size:var(--fs-sm);color:var(--muted);margin:-12px 0 20px">
               In force per repo:
               quiet <strong>${q}</strong> · busy <strong>${b}</strong> · surge <strong>${s}</strong>.
               Multi-repo threshold scaling does not apply to per-repo mode resolution.
             </p>`
        : '';
      return `
        <div class="config-field mb-7">
          <label>Cadence mode scope ${settingHelpMark('governor-thresholds', '', 'Cadence mode scope', `Aggregate preserves the current one-mode behavior. Per repo computes each successfully scanned repo's mode from its own actionable queue; hive-wide agents continue using the aggregate mode.`)} ${yamlHint('governor.cadence_scope')}</label>
          <select id="cfg-cadence-scope" data-change-action="markDirty" data-arg0="cadence-scope" data-arg1="cadenceScope" data-arg-types="s,s,v">
            ${[['aggregate', 'Aggregate (default)'], ['per_repo', 'Per repo']]
              .map(([v, label]) => `<option value="${v}" ${activeScope === v ? 'selected' : ''}>${label}</option>`).join('')}
          </select>
        </div>
        <div class="config-field mb-7">
          <label>Multi-repo threshold scaling ${settingHelpMark('governor-thresholds', '', 'Multi-repo threshold scaling', `How issue/PR thresholds scale as repo count grows. Square-root is recommended for large hives.`)} ${yamlHint('governor.threshold_scaling')}</label>
          <select id="cfg-threshold-scaling" data-change-action="markDirty" data-arg0="threshold-scaling" data-arg1="thresholdScaling" data-arg-types="s,s,v" ${activeScope === 'per_repo' ? 'disabled style="opacity:0.6"' : ''}>
            ${[['linear', 'Linear'], ['sqrt', 'Square-root (default)'], ['none', 'None']]
              .map(([v, label]) => `<option value="${v}" ${activeScaling === v ? 'selected' : ''}>${label}</option>`).join('')}
          </select>
          ${activeScope === 'per_repo' ? '<p style="font-size:var(--fs-sm);color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Scaling is ignored when cadence mode scope is per repo.</p>' : ''}
        </div>
        <div class="config-field mb-7">
          <label>Coverage goal (%) ${settingHelpMark('governor-thresholds', '', 'Coverage goal', `Test-coverage percentage the quality card, ci-maintainer bar, agent coverage preamble, and ACMM L6 gate measure against (1-100, default 91).`)}</label>
          <input type="number" min="1" max="100" id="cfg-coverage-target" value="${activeCoverageTarget}" data-input-action="markDirty" data-arg0="coverage-target" data-arg1="coverageTarget" data-arg-types="s,s,N">
        </div>
        <p class="fs-sm text-muted mb-7">Drag the handles to set issue-count thresholds for governor mode transitions.</p>
        ${effNote}
        ${scalingSummary ? `<p class="fs-sm text-muted mb-4" id="thresh-scaling-summary"><strong>${scalingSummary}</strong></p>` : ''}
        <div class="thresh-bar-wrap">
          <div class="thresh-bar-track" id="thresh-track"
               data-max="${maxValue}" style="background:${governorModeGradient(bounds)}">
            <div class="thresh-pressure-marker" style="left:${bounds.pressurePct}%" data-val="${pressure}" title="Current pressure: ${pressure}"></div>
            <div class="thresh-handle" id="thresh-quiet" style="left:${bounds.quietPct}%" data-key="quiet" data-val="${q}"></div>
            <div class="thresh-handle" id="thresh-busy" style="left:${bounds.busyPct}%" data-key="busy" data-val="${b}"></div>
            <div class="thresh-handle" id="thresh-surge" style="left:${bounds.surgePct}%" data-key="surge" data-val="${s}"></div>
          </div>
          <div class="thresh-bar-labels">
            <span style="left:0;top:0;transform:none;color:${GOVERNOR_MODE_COLORS.idle}">0</span>
            ${labels.map(l => `<span style="left:${l.pct}%;top:${l.lane * 1.35}em;color:${l.color}" id="thresh-lbl-${l.key}">${l.text}</span>`).join('')}
            <span style="right:0;left:auto;top:0;transform:none;color:var(--muted)">${maxValue}</span>
          </div>
          <div class="thresh-zone-labels">
            ${zoneLabels.map(l => `<span style="left:${l.pct}%;top:${l.lane * 1.15}em;color:${l.color}">${l.key}</span>`).join('')}
          </div>
        </div>
        <div class="config-field-row" style="margin-top:var(--sp-8)">
          <div class="config-field" data-setting-key="governor.modes.quiet.threshold"><label title="Number of actionable issues at which the governor transitions from idle to quiet mode. Below this count, agents run at their idle cadence.">Quiet</label><input type="number" min="1" max="${maxValue}" id="thresh-in-quiet" value="${q}" data-input-action="setThreshFromInput" data-arg0="quiet" data-arg-types="s,v">${hints.quiet ? `<span class="muted-xs" id="thresh-hint-quiet">${hints.quiet}</span>` : ''}</div>
          <div class="config-field" data-setting-key="governor.modes.busy.threshold"><label title="Number of actionable issues at which the governor transitions from quiet to busy mode. Agents increase their kick frequency to handle the growing workload.">Busy</label><input type="number" min="1" max="${maxValue}" id="thresh-in-busy" value="${b}" data-input-action="setThreshFromInput" data-arg0="busy" data-arg-types="s,v">${hints.busy ? `<span class="muted-xs" id="thresh-hint-busy">${hints.busy}</span>` : ''}</div>
          <div class="config-field" data-setting-key="governor.modes.surge.threshold"><label title="Number of actionable issues at which the governor transitions from busy to surge mode. Maximum agent activity — monitor your token budget closely at this level.">Surge</label><input type="number" min="1" max="${maxValue}" id="thresh-in-surge" value="${s}" data-input-action="setThreshFromInput" data-arg0="surge" data-arg-types="s,v">${hints.surge ? `<span class="muted-xs" id="thresh-hint-surge">${hints.surge}</span>` : ''}</div>

        </div>`;
    }

    function setThreshFromInput(key, val) {
      const track = document.getElementById('thresh-track');
      const maxValue = track ? (parseInt(track.dataset.max, 10) || governorTrackMax(((_configState.data.effectiveThresholds || {}).surge || 20), governorPressureFromStatus(window._lastStatus || {}))) : governorTrackMax(((_configState.data.effectiveThresholds || {}).surge || 20), governorPressureFromStatus(window._lastStatus || {}));
      const v = Math.max(1, Math.min(maxValue, parseInt(val) || 1));
      markDirty('thresholds', key, v);
      updateThreshBarGradient();
    }

    (function initThreshDrag() {
      let active = null, trackRect = null;
      document.addEventListener('pointerdown', (e) => {
        if (!e.target.classList.contains('thresh-handle')) return;
        active = e.target;
        trackRect = document.getElementById('thresh-track').getBoundingClientRect();
        active.setPointerCapture(e.pointerId);
        e.preventDefault();
      });
      document.addEventListener('pointermove', (e) => {
        if (!active || !trackRect) return;
        const pct = Math.max(0, Math.min(100, ((e.clientX - trackRect.left) / trackRect.width) * 100));
        const track = document.getElementById('thresh-track');
        const maxValue = track ? (parseInt(track.dataset.max, 10) || 1) : 1;
        const val = Math.round((pct / 100) * maxValue);
        const key = active.dataset.key;
        const d = _configState.data.thresholds || {};
        const merged = Object.assign({}, d, _configState.dirty.thresholds || {});
        let clamped = val;
        if (key === 'quiet') clamped = Math.max(1, Math.min(clamped, (merged.busy || 10) - 1));
        if (key === 'busy') clamped = Math.max((merged.quiet || 2) + 1, Math.min(clamped, (merged.surge || 20) - 1));
        if (key === 'surge') clamped = Math.max((merged.busy || 10) + 1, Math.min(clamped, maxValue - 1));
        active.dataset.val = clamped;
        const inp = document.getElementById('thresh-in-' + key);
        if (inp) inp.value = clamped;
        markDirty('thresholds', key, clamped);
        updateThreshBarGradient();
      });
      document.addEventListener('pointerup', () => { active = null; trackRect = null; });
    })();

    function updateThreshBarGradient() {
      const track = document.getElementById('thresh-track');
      if (!track) return;
      const d = _configState.data.thresholds || {};
      const m = Object.assign({}, d, _configState.dirty.thresholds || {});
      const base = { quiet: m.quiet || 2, busy: m.busy || 10, surge: m.surge || 20 };
      const scope = ((_configState.data && _configState.data.cadenceScope) || 'aggregate');
      const scopeDirty = (_configState.dirty['cadence-scope'] || {}).cadenceScope;
      const activeScope = scopeDirty !== undefined ? scopeDirty : scope;
      const scaling = ((_configState.data && _configState.data.thresholdScaling) || 'linear');
      const scalingDirty = (_configState.dirty['threshold-scaling'] || {}).thresholdScaling;
      const activeScaling = scalingDirty !== undefined ? scalingDirty : scaling;
      const effective = governorEffectiveThresholds(_configState.data || {}, base, activeScope, activeScaling);
      const repoCount = ((_configState.data && _configState.data.repoCount) || 1);
      const factor = governorScalingFactor(repoCount, activeScaling, activeScope);
      const scaled = factor > 1;
      const pinned = {
        quiet: governorThresholdIsPinned(_configState.data || {}, 'quiet'),
        busy: governorThresholdIsPinned(_configState.data || {}, 'busy'),
        surge: governorThresholdIsPinned(_configState.data || {}, 'surge'),
      };
      const bounds = governorThresholdSliderBoundaries(effective, governorPressureFromStatus(window._lastStatus || {}));
      const labels = governorStaggerLabels([
        { key: 'quiet', pct: bounds.quietPct },
        { key: 'busy', pct: bounds.busyPct },
        { key: 'surge', pct: bounds.surgePct },
      ], 11);
      const labelLanes = {};
      labels.forEach(l => { labelLanes[l.key] = l.lane; });
      track.dataset.max = String(bounds.max);
      track.style.background = governorModeGradient(bounds);
      for (const item of [
        ['quiet', base.quiet, effective.quiet, bounds.quietPct],
        ['busy', base.busy, effective.busy, bounds.busyPct],
        ['surge', base.surge, effective.surge, bounds.surgePct],
      ]) {
        const [key, raw, effVal, pct] = item;
        const handle = document.getElementById('thresh-' + key);
        const lbl = document.getElementById('thresh-lbl-' + key);
        const hint = document.getElementById('thresh-hint-' + key);
        if (handle) { handle.style.left = pct + '%'; handle.dataset.val = raw; }
        if (lbl) { lbl.textContent = governorThresholdLabel(raw, effVal, pinned[key]); lbl.style.left = pct + '%'; lbl.style.top = ((labelLanes[key] || 0) * 1.35) + 'em'; }
        if (hint) hint.textContent = scaled ? governorThresholdHintText(raw, effVal, pinned[key], repoCount, activeScaling, activeScope) : '';
      }
      const pressureMarker = track.querySelector('.thresh-pressure-marker');
      if (pressureMarker) pressureMarker.style.left = bounds.pressurePct + '%';
      const zones = track.parentElement.querySelector('.thresh-zone-labels');
      if (zones) {
        const spans = zones.querySelectorAll('span');
        const zoneLabels = governorStaggerLabels([
          { pct: bounds.quietPct / 2 },
          { pct: (bounds.quietPct + bounds.busyPct) / 2 },
          { pct: (bounds.busyPct + bounds.surgePct) / 2 },
          { pct: (bounds.surgePct + 100) / 2 },
        ], 14);
        zoneLabels.forEach((label, i) => {
          if (spans[i]) { spans[i].style.left = label.pct + '%'; spans[i].style.top = (label.lane * 1.15) + 'em'; }
        });
      }
    }

    const OUTSIDE_BUCKET_DEFS = [
      { key: 'needsDirection', saveKey: 'needs_direction', label: 'needs-direction (legacy)', setting: 'project.issue_filter.hard_suppress_labels.needs_direction', fixed: false, definition: 'Legacy spelling of the decision park; the hive migrates these onto stage/045-needs-decision and no longer writes them.' },
      { key: 'needsDecision', saveKey: 'needs_decision', label: 'needs human decision', setting: 'project.issue_filter.hard_suppress_labels.needs_decision', fixed: false, definition: 'Issues parked for a maintainer ruling. stage/045-needs-decision (and the PR-only stage/085-needs-decision) is always recognised; add extra spellings here.' },
      { key: 'needsSpec', saveKey: 'needs_spec', label: 'needs-spec', setting: 'project.issue_filter.hard_suppress_labels.needs_spec', fixed: false, definition: 'Issues waiting for specification or acceptance criteria.' },
      { key: 'labels', saveKey: 'labels', label: 'exempt labels', setting: 'governor.labels.exempt', fixed: false, definition: 'Issues or PRs that must never be agent work; permanent do-not-merge and hold labels also apply.' },
      { key: 'reporterTrust', saveKey: 'reporter_trust', label: 'reporter triage', setting: 'project.issue_filter.reporter_trust', fixed: false, definition: 'When enabled, issues from untrusted reporters wait for a maintainer triage label.' },
      { key: 'requireLabels', saveKey: 'require_labels', label: 'project issue filter', setting: 'project.issue_filter.require_labels', fixed: false, definition: 'When non-empty, issues must carry at least one required label before agents may work them.' },
      { key: 'standingMeta', label: 'standing meta/advisory', setting: 'fixed: advisory title or hive/advisory label', fixed: true, definition: 'Hive advisory reports are standing coordination issues, not agent tasks.' },
      { key: 'dependencyDashboard', label: 'dependency dashboard', setting: 'fixed: bot author + Dependency/Renovate Dashboard title', fixed: true, definition: 'Renovate/Dependabot dashboard issues are bot control panels, not discrete work.' },
      { key: 'holdAdjacent', label: 'hold-adjacent/other', setting: 'fixed: scanner safety remainder', fixed: true, definition: 'Open items not assigned to a named bucket; kept outside until a dedicated classifier exists.' }
    ];

    function hardSuppressState() {
      const hs = _configState.data.hardSuppressLabels || {};
      return {
        needsHuman: Array.isArray(hs.needsHuman) ? hs.needsHuman : ['needs-human'],
        needsDirection: Array.isArray(hs.needsDirection) ? hs.needsDirection : ['needs-direction'],
        needsDecision: Array.isArray(hs.needsDecision) ? hs.needsDecision : ['stage/045-needs-decision'],
        needsSpec: Array.isArray(hs.needsSpec) ? hs.needsSpec : ['needs-spec']
      };
    }

    function stageHardSuppressLabels(next) {
      _configState.data.hardSuppressLabels = Object.assign({}, hardSuppressState(), next);
      const st = hardSuppressState();
      markDirty('labels', 'hard_suppress_labels', {
        needs_human: st.needsHuman,
        needs_direction: st.needsDirection,
        needs_decision: st.needsDecision,
        needs_spec: st.needsSpec
      });
    }

    function renderHardSuppressEditor(key, label, setting) {
      const st = hardSuppressState();
      const list = (st[key] || []).map((l, i) => `<div class="config-list-item"><span>${esc(l)}</span><button class="remove-item" data-action="removeListItem" data-arg0="labels" data-arg1="hard:${key}" data-arg2="${i}" data-arg-types="s,s,j" title="Remove ${esc(l)} from ${esc(label)} labels">✕</button></div>`).join('');
      const inputId = `cfg-hard-${key}-input`;
      return `<div class="config-list mt-2">
        ${list || '<div class="fs-sm text-muted mb-3">No labels — add at least one to keep this bucket active</div>'}
        <div class="config-list-add">
          <input type="text" id="${esc(inputId)}" placeholder="label name" title="Label counted as ${esc(label)} outside work (${esc(setting)}).">
          <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="labels" data-arg1="hard:${key}" data-arg2="${esc(inputId)}" title="Add a ${esc(label)} label">+ Add</button>
        </div>
      </div>`;
    }

    function renderOutsideBucketsPanel(labels, requireLabels) {
      const st = hardSuppressState();
      const rt = reporterTrustState();
      const currentValue = (def) => {
        if (def.key === 'needsDirection') return st.needsDirection.join(', ');
        if (def.key === 'needsDecision') return st.needsDecision.join(', ');
        if (def.key === 'needsSpec') return st.needsSpec.join(', ');
        if (def.key === 'labels') return (labels || []).concat(['do-not-merge']).join(', ') || 'none';
        if (def.key === 'reporterTrust') return rt.enabled ? `enabled; triage labels: ${rt.untrustedRequireLabels.join(', ')}` : 'disabled';
        if (def.key === 'requireLabels') return (requireLabels || []).join(', ') || 'disabled (no required labels)';
        return def.setting;
      };
      const rows = OUTSIDE_BUCKET_DEFS.map(def => `<tr><td><b>${esc(def.label)}</b></td><td>${esc(def.definition)}</td><td><code>${esc(currentValue(def))}</code></td><td>${def.fixed ? 'Fixed scanner rule' : `Edit here in Settings → Labels<br><code>${esc(def.setting)}</code>`}</td></tr>`).join('');
      return `<h4 id="outside-buckets" class="mt-6 mb-3">Triage buckets: what counts as outside</h4>
        <p class="fs-sm text-muted mb-4">The Overview/Governor <b>outside</b> number is open work excluded before it can enter the actionable band. Use this panel to see and change the owner-controlled rules.</p>
        <div class="config-table-wrap mb-4">
          <table class="config-table"><thead><tr><th>Bucket</th><th>Definition</th><th>Current rule</th><th>How to change</th></tr></thead><tbody>${rows}</tbody></table>
        </div>
        <div class="config-grid mb-5">
          <div><div class="fs-sm mb-2"><b>needs-direction labels</b></div><p class="fs-xs text-muted mb-2">Maintainer direction required.</p>${renderHardSuppressEditor('needsDirection', 'needs-direction', 'project.issue_filter.hard_suppress_labels.needs_direction')}</div>
          <div><div class="fs-sm mb-2"><b>needs-decision labels</b></div><p class="fs-xs text-muted mb-2">Maintainer decision required.</p>${renderHardSuppressEditor('needsDecision', 'needs-decision', 'project.issue_filter.hard_suppress_labels.needs_decision')}</div>
          <div><div class="fs-sm mb-2"><b>needs-spec labels</b></div><p class="fs-xs text-muted mb-2">Spec or acceptance criteria required.</p>${renderHardSuppressEditor('needsSpec', 'needs-spec', 'project.issue_filter.hard_suppress_labels.needs_spec')}</div>
        </div>`;
    }

    function renderGovLabels(labels, holdLabels, requireLabels) {
      const permanentLabels = (holdLabels || []).concat(['do-not-merge']);
      const staticList = permanentLabels.map(l =>
        `<div class="config-list-item" style="opacity:0.7"><span>${esc(l)}</span><span style="font-size:0.65rem;color:var(--muted);margin-left:auto;margin-right:var(--sp-4)">permanent</span></div>`
      ).join('');
      const list = labels.map((l, i) =>
        `<div class="config-list-item"><span>${esc(l)}</span><button class="remove-item" data-action="removeListItem" data-arg0="labels" data-arg1="list" data-arg2="${i}" data-arg-types="s,s,j">✕</button></div>`
      ).join('');
      // Required labels (allow-list) — the OPPOSITE polarity from the exempt
      // list above: exempt = "never touch these", required = "ONLY touch
      // these". Stored as project.issue_filter.require_labels and enforced at
      // the actionable scan, not just in prompts.
      const reqList = (requireLabels || []).map((l, i) =>
        `<div class="config-list-item"><span>${esc(l)}</span><button class="remove-item" data-action="removeListItem" data-arg0="labels" data-arg1="require" data-arg2="${i}" data-arg-types="s,s,j">✕</button></div>`
      ).join('');
      const automergeLabel = (_configState.dirty.labels && _configState.dirty.labels.automerge !== undefined)
        ? _configState.dirty.labels.automerge
        : (_configState.data.automergeLabel || '');
      const writingGuide = (_configState.dirty.labels && _configState.dirty.labels.writing_guide !== undefined)
        ? _configState.dirty.labels.writing_guide
        : (_configState.data.writingGuide || '');
      const stageLabelsOn = (_configState.dirty.labels && _configState.dirty.labels.stage_enabled !== undefined)
        ? _configState.dirty.labels.stage_enabled
        : (_configState.data.stageLabelsEnabled !== false);
      const stageDwell = (_configState.dirty.labels && _configState.dirty.labels.stage_dwell !== undefined)
        ? _configState.dirty.labels.stage_dwell
        : (_configState.data.stageLabelsDwell || '');
      const stageNudgeOn = (_configState.dirty.labels && _configState.dirty.labels.stage_nudge_enabled !== undefined)
        ? _configState.dirty.labels.stage_nudge_enabled
        : (_configState.data.stageNudgeEnabled !== false);
      const stageNudgeRenudge = (_configState.dirty.labels && _configState.dirty.labels.stage_nudge_renudge_interval !== undefined)
        ? _configState.dirty.labels.stage_nudge_renudge_interval
        : (_configState.data.stageNudgeRenudge || '');
      const stagePresenceWindow = (_configState.dirty.labels && _configState.dirty.labels.stage_presence_window !== undefined)
        ? _configState.dirty.labels.stage_presence_window
        : (_configState.data.stagePresenceWindow || '');
      const stageAdoptionOn = (_configState.dirty.labels && _configState.dirty.labels.stage_adoption_enabled !== undefined)
        ? _configState.dirty.labels.stage_adoption_enabled
        : (_configState.data.stageAdoptionEnabled !== false);
      const stageAdoptionMaintainerPolicy = (_configState.dirty.labels && _configState.dirty.labels.stage_adoption_maintainer_policy !== undefined)
        ? _configState.dirty.labels.stage_adoption_maintainer_policy
        : (_configState.data.stageAdoptionMaintainerPolicy || 'adopt');
      const stageAdoptionMaintainerOptions = [
        ['adopt', 'adopt — a clanker PR whose hive disconnected is adopted whoever wrote it, repo role ignored (default)'],
        ['ask', 'ask — never adopt a maintainer\'s PR on its own; mark it for an owner decision and offer Adopt in the activity monitor'],
        ['never', 'never — the hive never takes over a maintainer\'s PR and releases any adoption of one'],
      ].map(([v, label]) => `<option value="${v}" ${stageAdoptionMaintainerPolicy === v ? 'selected' : ''}>${esc(label)}</option>`).join('');
      const stageStalledQuietOn = (_configState.dirty.labels && _configState.dirty.labels.stage_stalled_require_quiet !== undefined)
        ? _configState.dirty.labels.stage_stalled_require_quiet
        : (_configState.data.stageStalledRequireQuiet !== false);
      const stageEngineMode = (_configState.dirty.labels && _configState.dirty.labels.stage_engine_mode !== undefined)
        ? _configState.dirty.labels.stage_engine_mode
        : (_configState.data.stageEngineMode || 'enforce');
      const stageEngineModeOptions = [['shadow', 'shadow — engine derives, writes nothing (dry run)'], ['enforce', 'enforce — engine writes (default)']]
        .map(([v, label]) => `<option value="${v}" ${stageEngineMode === v ? 'selected' : ''}>${esc(label)}</option>`).join('');
      const stageGraceOnline = (_configState.dirty.labels && _configState.dirty.labels.stage_grace_online !== undefined)
        ? _configState.dirty.labels.stage_grace_online
        : (_configState.data.stageGraceOnline || '');
      const stageGraceOffline = (_configState.dirty.labels && _configState.dirty.labels.stage_grace_offline !== undefined)
        ? _configState.dirty.labels.stage_grace_offline
        : (_configState.data.stageGraceOffline || '');
      return `
        ${renderOutsideBucketsPanel(labels, requireLabels)}
        <h4 style="margin:var(--sp-0) var(--sp-0) var(--sp-3)">Exempt labels (deny-list)</h4>
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">Labels that filter issues <b>out</b> of the actionable issues list and PRs out of the mergeable PRs list — issues carrying one of these are never agent work. Agents receive the filtered lists as variable substitutions in their kick prompts.</p>
        <p style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-4);font-style:italic">Permanent labels cannot be removed. Hold labels also appear in the On Hold section of the dashboard.</p>
        <div class="config-list">
          ${staticList}
          <div style="border-top:1px solid var(--border);margin:var(--sp-3) var(--sp-0)"></div>
          ${list || '<div class="fs-sm text-muted mb-3">No additional exempt labels</div>'}
          <div class="config-list-add">
            <input type="text" id="cfg-label-input" placeholder="label name" title="GitHub label name to exempt from actionable issues and mergeable PRs.">
            <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="labels" data-arg1="list" data-arg2="cfg-label-input" title="Add this label to the exempt list">+ Add</button>
          </div>
        </div>
        <h4 class="mt-6 mb-3">Required labels (allow-list)</h4>
        <p class="fs-sm text-muted mb-4">The opposite polarity: when this list is non-empty, agents may <b>only</b> initiate work on issues carrying at least one of these labels — "only work approved issues". Empty = every issue is eligible (the default). Exempt labels above still win on conflict; PRs and the Hold list are unaffected. Matching is exact (case-insensitive).</p>
        <div class="config-list">
          ${reqList || '<div class="fs-sm text-muted mb-3">No required labels — agents may work every actionable issue</div>'}
          <div class="config-list-add">
            <input type="text" id="cfg-require-label-input" placeholder="label name" title="GitHub label an issue must carry before agents may work it (e.g. an approval/queue label).">
            <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="labels" data-arg1="require" data-arg2="cfg-require-label-input" title="Add this label to the required (allow) list">+ Add</button>
          </div>
        </div>
        <h4 class="mt-6 mb-3">Auto-merge queue label</h4>
        <div class="config-field">
          <label for="cfg-automerge-label">Queue label ${yamlHint('governor.labels.automerge')} ${settingHelpMark('settings', 'labels', 'Auto-merge queue label', `The label a merger or owner queue action applies to a PR to queue it for auto-merge. Configurable because the label lives in the managed repository, where a convention may already exist (Prow-style projects use lgtm). Empty uses the default.`)}</label>
          <input type="text" id="cfg-automerge-label" value="${esc(automergeLabel)}" placeholder="${esc(_configState.data.automergeLabelDefault || 'lgtm')}" data-change-action="markDirty" data-arg0="labels" data-arg1="automerge" data-arg-types="s,s,w">
          <div class="config-help">Label applied by the merger queue; default is ${esc(_configState.data.automergeLabelDefault || 'lgtm')}. Requires restart.</div>
        </div>
        <h4 class="mt-6 mb-3">Stage labels</h4>
        <div class="config-toggle"><div class="config-toggle-switch ${stageLabelsOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="labels" data-key="stage_enabled"></div><span class="config-toggle-label">Hive-owned stage/NN labels ${yamlHint('governor.labels.stage.enabled')} ${settingHelpMark('settings', 'labels', 'Stage labels', `Each eval cycle the hive writes exactly one stage/NN-* label per open issue/PR (review-pending, reviewed, fixing, returned-to-contributor, adopted-by-hive, awaiting-merge-admission, needs-decision, ready-for-human-merge, pre-merge-soak, merge-queue), a role/* admission-tier label, an author/* label saying who wrote the PR (human, clanker = a contributor's own agent, hive = this hive), and stage/stalled when nobody has touched the item for the stage's dwell. Backward moves are a one-line stage comment and the dashboard's transition history, not a label. Writes happen only on change. See docs/labels-and-control-signals.md.`)}</span></div>
        <div class="config-toggle"><div class="config-toggle-switch ${stageStalledQuietOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="labels" data-key="stage_stalled_require_quiet"></div><span class="config-toggle-label">stage/stalled means no activity ${yamlHint('governor.labels.stage.stalled.require_quiet')} ${settingHelpMark('settings', 'labels', 'Stalled requires quiet', `On (default): stage/stalled is applied only when nobody but the hive has touched the item (head push, comment, review, CI state, label) for the stage's dwell, and never while required checks are running or the reviewer verdict for the current head is pending — those waits are on the hive. Any activity clears it at once. Off: the dwell is measured from entering the stage. See docs/labels-and-control-signals.md.`)}</span></div>
        <div class="config-field">
          <label for="cfg-stage-engine-mode">Central stage engine mode ${yamlHint('governor.labels.stage.engine.mode')} ${settingHelpMark('settings', 'labels', 'Central stage engine', `ADR-0025: one reconciler derives every stage/NN label through one writer. enforce (default): the engine's derivation is written and its admission table counts out-of-order transitions (Activity tracker strip, /api/status.stageEngine). shadow: a pause — the engine derives and counts but writes nothing, so stage labels stay exactly as found until the mode returns to enforce. Takes effect on the next sweep, no restart.`)}</label>
          <select id="cfg-stage-engine-mode" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_engine_mode" data-arg-types="s,s,v">${stageEngineModeOptions}</select>
          <div class="config-help"><code>enforce</code> is the default. <code>shadow</code> pauses every stage write (dry run). No restart needed.</div>
        </div>
        <div class="config-field">
          <label for="cfg-stage-dwell">Dwell overrides ${yamlHint('governor.labels.stage.dwell.<key>')}</label>
          <input type="text" id="cfg-stage-dwell" value="${esc(stageDwell)}" placeholder="review-pending=1h, awaiting-merge-admission=30m, needs-author=24h, returned-to-contributor=24h" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_dwell" data-arg-types="s,s,w">
          <div class="config-help">How long an item may sit in a stage before it gets <code>stage/stalled</code>; comma-separated <code>slug=duration</code>, <code>0</code> = never. Unset stages use the built-in table.</div>
        </div>
        <div class="config-toggle"><div class="config-toggle-switch ${stageNudgeOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="labels" data-key="stage_nudge_enabled"></div><span class="config-toggle-label">Contributor nudge on returned-to-contributor ${yamlHint('governor.labels.stage.contributor_nudge.enabled')} ${settingHelpMark('settings', 'labels', 'Contributor nudge', `When a contributor's PR enters stage/072-returned-to-contributor (red CI, changes requested, conflicts) the hive posts ONE @-mention comment per head with the failing checks / review asks and, for relay contributors, pushes a fix-pr task to their relay session (parallel sub-session when the relay advertises parallel_sessions, otherwise preempting the current task). Re-nudged once at the stall threshold; cap 2 per head. See docs/labels-and-control-signals.md.`)}</span></div>
        <div class="config-field">
          <label for="cfg-stage-nudge-renudge">Re-nudge interval ${yamlHint('governor.labels.stage.contributor_nudge.renudge_interval')}</label>
          <input type="text" id="cfg-stage-nudge-renudge" value="${esc(stageNudgeRenudge)}" placeholder="24h (stage dwell)" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_nudge_renudge_interval" data-arg-types="s,s,w">
          <div class="config-help">Go duration before the single re-nudge; empty = the returned-to-contributor dwell.</div>
        </div>
        <div class="config-field">
          <label for="cfg-stage-presence-window">Contributor presence window ${yamlHint('governor.labels.stage.presence_window')}</label>
          <input type="text" id="cfg-stage-presence-window" value="${esc(stagePresenceWindow)}" placeholder="15m" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_presence_window" data-arg-types="s,s,w">
          <div class="config-help">A contributor counts as online when their relay heartbeat is within this window; online contributors get the fix-pr interrupt immediately, offline ones only the comment (their fix waits in the queue).</div>
        </div>
        <div class="config-toggle"><div class="config-toggle-switch ${stageAdoptionOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="labels" data-key="stage_adoption_enabled"></div><span class="config-toggle-label">Adopt red contributor PRs after the grace ${yamlHint('governor.labels.stage.adoption.enabled')} ${settingHelpMark('settings', 'labels', 'Adopt red contributor PRs', `When a nudged contributor PR is still red after its grace (online 2h / offline 1h, below) the hive moves it to stage/074-adopted-by-hive: it pushes hive/adopt-N at the contributor's head (commits and sign-off preserved), opens a superseding App PR carrying the original Closes lines, comments once on the original, and the scanner's red-PR fix-first kick rebases and fixes CI on that branch. The original closes automatically when the superseding PR merges. Off: the PR stays in 072 and just stalls. See docs/labels-and-control-signals.md.`)}</span></div>
        <div class="config-field">
          <label for="cfg-stage-adoption-maintainer-policy">When the PR author maintains the repository ${yamlHint('governor.labels.stage.adoption.maintainer_policy')} ${settingHelpMark('settings', 'labels', 'Adoption of a maintainer\'s PR', `What adoption does when the author of a nudged author/clanker PR holds admin or maintain on the repository. adopt (default): the role is ignored — clanker PRs are adoptable on disconnect regardless of the owner's repo role (hivecommons/hive#14524: a maintainer's own hive PR sat at 072 for 26h with its hive offline). ask: the hive never adopts such a PR by itself; the activity monitor row reads "waiting on @author (maintainer, offline Nh) — adopt?" and its menu offers Adopt, which queues the same adoption as a contributor going offline. never: the pre-#14524 rule — a maintainer's PR is never taken over and an existing adoption of one is released. Human-authored PRs are never adopted under any setting.`)}</label>
          <select id="cfg-stage-adoption-maintainer-policy" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_adoption_maintainer_policy" data-arg-types="s,s,v">${stageAdoptionMaintainerOptions}</select>
          <div class="config-help">adopt: repo role ignored, adopt on disconnect like any clanker PR. ask: park it for an owner decision with an Adopt action. never: hands-off and release.</div>
        </div>
        <div class="config-field">
          <label for="cfg-stage-grace-online">Online contributor grace ${yamlHint('governor.labels.stage.contributor_grace_online')}</label>
          <input type="text" id="cfg-stage-grace-online" value="${esc(stageGraceOnline)}" placeholder="2h" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_grace_online" data-arg-types="s,s,w">
          <div class="config-help">Go duration after the nudge before an online contributor's red PR is adopted (the relay interrupt went unanswered, no new head).</div>
        </div>
        <div class="config-field">
          <label for="cfg-stage-grace-offline">Offline contributor grace ${yamlHint('governor.labels.stage.contributor_grace_offline')}</label>
          <input type="text" id="cfg-stage-grace-offline" value="${esc(stageGraceOffline)}" placeholder="1h" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_grace_offline" data-arg-types="s,s,w">
          <div class="config-help">Go duration after the comment-only nudge before an offline or external author's red PR is adopted. With live hub presence the hive instead waits ${yamlHint('governor.labels.stage.adoption.after_offline')} (default 30m) after a registered relay's last heartbeat, or ${yamlHint('governor.labels.stage.adoption.after_unregistered')} (default 2h) for an author with no profile here, and adopts in place on the PR's own branch when GitHub permits ${yamlHint('governor.labels.stage.adoption.in_place')} (default on) rather than opening a superseding PR. An in-place adoption keeps a changed head only when GitHub links the commit to the App bot ${yamlHint('governor.labels.stage.adoption.strict_attribution')} (default on); otherwise the hive hands the PR back to the contributor. Once the hive owns the head, the contributor lane merges it under the hive's own authority when the hive-authored gates pass (CI green on the adopted head, reviewer verdict, DCO, diff fences, soak), without requiring the original author's trust tier ${yamlHint('governor.labels.stage.adoption.auto_merge')} (default on). When the hive supersedes instead, it closes the original in the same pass that opens its copy — one note naming the copy, <code>hive/superseded</code> — and reopens it if the copy is closed unmerged or red at two heads ${yamlHint('governor.labels.stage.adoption.supersede.close_first')} (default on; off keeps the original open at stage 074 until the copy merges). A clanker- or hive-authored original closes in that pass by policy even without the repo's write-back opt-in label, since the copy carries its commits and sign-off ${yamlHint('governor.labels.stage.adoption.close_superseded')} (default <code>on_adopt</code>; <code>on_merge</code> waits for the copy to merge; a person's PR always waits unless the label is present). A copy that conflicts with the base is handed to the fixer's rebase-resolve episode (two attempts, then stage 085 <code>rebase-failed-twice</code>); an adoption attempt that opened nothing is retried once the author's clanker has stayed offline for ${yamlHint('governor.labels.stage.adoption.offline_grace')} (default 30m).</div>
        </div>
        ${renderReporterTrustBlock()}
        <h4>Writing guide</h4>
        <div class="config-field">
          <label for="cfg-writing-guide">Project writing guide</label>
          <textarea id="cfg-writing-guide" rows="7" placeholder="No writing guide is set." data-input-action="markDirty" data-change-action="markDirty" data-arg0="labels" data-arg1="writing_guide" data-arg-types="s,s,v">${esc(writingGuide)}</textarea>
          <div class="fs-xs text-muted mt-2">
            Lands as <code>${'${WRITING_GUIDE}'}</code> before the body template in every issue/PR-filing kick and now in review-swarm comments.
            See <a class="text-accent" href="https://github.com/hivecommons/hive/blob/v5/src/docs/agent-configuration.md#writing-guide-how-issues-and-prs-should-read-projectwriting_guide" target="_blank" rel="noopener">Writing guide docs ↗</a>.
          </div>
        </div>`;
    }

    // Reporter trust (hivecommons/hive#9665): gate agent work on WHO filed the
    // issue. Trusted reporters (by GitHub association or explicit login) are
    // admitted by the ordinary rules; anyone else's issue needs a triage label
    // first. The merge-side hold for the same reporters lives on the Repos tab.
    // State: _configState.data.reporterTrust as served by GET /api/config/governor;
    // every edit re-stages the WHOLE block under dirty.labels.reporter_trust,
    // because the server merges absent fields as "unchanged" and we want one
    // coherent save.
    const REPORTER_ASSOCIATION_HELP = {
      OWNER: 'Repository or organization owner',
      MEMBER: 'Member of the organization that owns the repo',
      COLLABORATOR: 'Invited as a collaborator on the repo',
      CONTRIBUTOR: 'Has had a commit merged before — one typo fix counts, so this is NOT trusted by default',
      FIRST_TIME_CONTRIBUTOR: 'Opening their first contribution to this repo',
      FIRST_TIMER: 'Opening their first contribution on GitHub at all',
      NONE: 'No relationship to the repo'
    };

    function reporterTrustState() {
      const d = _configState.data.reporterTrust || {};
      return {
        enabled: !!d.enabled,
        trustedAssociations: Array.isArray(d.trustedAssociations) ? d.trustedAssociations : ['OWNER', 'MEMBER', 'COLLABORATOR'],
        trustedLogins: Array.isArray(d.trustedLogins) ? d.trustedLogins : [],
        untrustedRequireLabels: Array.isArray(d.untrustedRequireLabels) ? d.untrustedRequireLabels : ['triage/accepted'],
        knownAssociations: Array.isArray(d.knownAssociations) && d.knownAssociations.length
          ? d.knownAssociations
          : ['OWNER', 'MEMBER', 'COLLABORATOR', 'CONTRIBUTOR', 'FIRST_TIME_CONTRIBUTOR', 'FIRST_TIMER', 'NONE'],
        clankerRequested: !!d.clankerRequested,
        clankerRequestedLabel: typeof d.clankerRequestedLabel === 'string' ? d.clankerRequestedLabel : 'clanker-requested',
        clankerRequestedAddendum: typeof d.clankerRequestedAddendum === 'string' ? d.clankerRequestedAddendum : '',
        clankerStaged: !!d.clankerStaged
      };
    }

    // Live preview of the clanker-requested explanation, shared by the render
    // and the input handlers so typing updates it without re-rendering.
    function clankerRequestedPreviewText(label, addendum) {
      const name = (label || '').trim() || 'clanker-requested';
      let text = 'This PR carries the `' + name + '` label, so Hive is holding it for a maintainer.';
      const extra = (addendum || '').trim();
      if (extra) text += '\n\n' + extra;
      return text;
    }

    function updateClankerRequestedPreview() {
      const lbl = document.getElementById('cfg-clanker-label');
      const add = document.getElementById('cfg-clanker-addendum');
      const prev = document.getElementById('cfg-clanker-preview');
      if (prev) prev.textContent = clankerRequestedPreviewText(lbl ? lbl.value : '', add ? add.value : '');
    }

    function setClankerRequestedField(key, value) {
      stageReporterTrust({ [key]: value });
      updateClankerRequestedPreview();
    }

    function toggleClankerRequested(el) {
      const isOn = el.classList.toggle('on');
      stageReporterTrust({ clankerRequested: isOn });
      renderConfigTab('Labels');
    }

    function stageReporterTrust(next) {
      _configState.data.reporterTrust = Object.assign({}, _configState.data.reporterTrust || {}, next);
      const st = reporterTrustState();
      const block = {
        enabled: st.enabled,
        trusted_associations: st.trustedAssociations,
        trusted_logins: st.trustedLogins,
        untrusted_require_labels: st.untrustedRequireLabels
      };
      // Only send the clanker-requested keys once the operator touched them, so
      // an unrelated edit never writes the defaults into config.
      if ('clankerRequested' in next || 'clankerRequestedLabel' in next || 'clankerRequestedAddendum' in next || st.clankerStaged) {
        _configState.data.reporterTrust.clankerStaged = true;
        block.clanker_requested = st.clankerRequested;
        block.clanker_requested_label = st.clankerRequestedLabel;
        block.clanker_requested_addendum = st.clankerRequestedAddendum;
      }
      markDirty('labels', 'reporter_trust', block);
    }

    function toggleReporterTrustEnabled(el) {
      const isOn = el.classList.toggle('on');
      stageReporterTrust({ enabled: isOn });
      renderConfigTab('Labels');
    }

    function toggleReporterTrustAssociation(assoc, checked) {
      const st = reporterTrustState();
      const set = new Set(st.trustedAssociations.map(a => String(a).toUpperCase()));
      if (checked) set.add(String(assoc).toUpperCase()); else set.delete(String(assoc).toUpperCase());
      // Keep GitHub's order so the saved list is stable across edits.
      stageReporterTrust({ trustedAssociations: st.knownAssociations.filter(a => set.has(a)) });
      renderConfigTab('Labels');
    }

    function renderReporterTrustBlock() {
      const st = reporterTrustState();
      const trusted = new Set(st.trustedAssociations.map(a => String(a).toUpperCase()));
      const assocRows = st.knownAssociations.map(a => `
            <label class="rt-assoc" title="${esc(REPORTER_ASSOCIATION_HELP[a] || a)}">
              <input type="checkbox" ${trusted.has(a) ? 'checked' : ''} ${st.enabled ? '' : 'disabled'} data-change-action="toggleReporterTrustAssociation" data-arg0="${esc(a)}" data-arg-types="s,c"> <code>${esc(a)}</code>
            </label>`).join('');
      const loginList = st.trustedLogins.map((l, i) =>
        `<div class="config-list-item"><span>@${esc(l)}</span><button class="remove-item" data-action="removeListItem" data-arg0="labels" data-arg1="reporter_login" data-arg2="${i}" data-arg-types="s,s,j">✕</button></div>`
      ).join('');
      const labelList = st.untrustedRequireLabels.map((l, i) =>
        `<div class="config-list-item"><span>${esc(l)}</span><button class="remove-item" data-action="removeListItem" data-arg0="labels" data-arg1="reporter_label" data-arg2="${i}" data-arg-types="s,s,j">✕</button></div>`
      ).join('');
      const dim = st.enabled ? '' : ' rt-dim';
      return `
        <h4 class="rt-h4">Reporter trust</h4>
        <p class="fs-sm text-muted mb-4">Gate work on <b>who filed the issue</b>, not only on its labels. Issues from trusted reporters are worked under the rules above. Issues from anyone else wait until a maintainer adds one of the triage labels below — and the PR that follows is held for a maintainer at every autonomy level (see the Repos tab).</p>
        <div class="config-toggle rt-toggle">
          <div class="config-toggle-switch ${st.enabled ? 'on' : ''}" data-action="toggleReporterTrustEnabled" data-arg-types="t" title="Require maintainer triage for issues from untrusted reporters"></div>
          <span class="config-toggle-label">Require triage for issues from untrusted reporters</span>
        </div>
        <div class="rt-body${dim}">
          <div class="fs-sm mb-2 rt-subhead">Trusted GitHub associations</div>
          <p class="fs-xs text-muted mb-3">GitHub reports one of these for every issue author. Checked = trusted. <code>CONTRIBUTOR</code>, <code>FIRST_TIME_CONTRIBUTOR</code> and <code>FIRST_TIMER</code> are assigned automatically from commit history (one commit to this repo makes anyone a <code>CONTRIBUTOR</code>), and <code>NONE</code> means no relationship at all. Check any of those and people become trusted without your say. <code>OWNER</code>, <code>MEMBER</code> and <code>COLLABORATOR</code> change only when someone is given org membership or repo access. With the default (those three checked), nobody gains trust just by getting PRs merged. <code>MEMBER</code> includes any member of the owning org, even one with no access to this repo. To trust a specific outside person, add their login under Always-trusted logins below; that trusts them without giving them repo access.</p>
          <div class="rt-assoc-grid">${assocRows}</div>
          <div class="fs-sm mb-2 rt-subhead">Always-trusted logins</div>
          <p class="fs-xs text-muted mb-3">People to trust whatever GitHub says about them — an external maintainer of a dependency, for example.</p>
          <div class="config-list">
            ${loginList || '<div class="fs-sm text-muted mb-3">No extra logins</div>'}
            <div class="config-list-add">
              <input type="text" id="cfg-reporter-login-input" placeholder="github login" ${st.enabled ? '' : 'disabled'} title="GitHub login to trust as a reporter regardless of association">
              <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="labels" data-arg1="reporter_login" data-arg2="cfg-reporter-login-input" ${st.enabled ? '' : 'disabled'} title="Trust this login">+ Add</button>
            </div>
          </div>
          <div class="fs-sm mb-2 rt-subhead">Triage labels for everyone else</div>
          <p class="fs-xs text-muted mb-3">An issue from an untrusted reporter becomes actionable once it carries at least one of these. Exact, case-insensitive match. The ordinary required-labels list above still applies afterwards.</p>
          <div class="config-list">
            ${labelList || '<div class="fs-sm text-muted mb-3">No triage labels — untrusted reporters can never be admitted</div>'}
            <div class="config-list-add">
              <input type="text" id="cfg-reporter-label-input" placeholder="label name" ${st.enabled ? '' : 'disabled'} title="Label a maintainer adds to admit an untrusted reporter's issue">
              <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="labels" data-arg1="reporter_label" data-arg2="cfg-reporter-label-input" ${st.enabled ? '' : 'disabled'} title="Add this triage label">+ Add</button>
            </div>
          </div>
        </div>
        <h4 class="rt-h4">Clanker-requested PRs</h4>
        <div class="config-toggle rt-toggle">
          <div class="config-toggle-switch ${st.clankerRequested ? 'on' : ''}" data-action="toggleClankerRequested" data-arg-types="t" title="Hold PRs that carry the clanker-requested label"></div>
          <span class="config-toggle-label">Hold PRs carrying the clanker-requested label ${settingHelpMark('agent-configuration', 'reporter-trust-who-filed-it-not-only-what-it-is-labelled', 'Hold PRs carrying the clanker-requested label', `Off by default. While on, a PR carrying this label is treated as held for a maintainer; Hive never removes the hold.`)}</span>
        </div>
        <div class="rt-body${st.clankerRequested ? '' : ' rt-dim'}">
          <div class="config-field">
            <label for="cfg-clanker-label">Label name</label>
            <input type="text" id="cfg-clanker-label" value="${esc(st.clankerRequestedLabel)}" placeholder="clanker-requested" ${st.clankerRequested ? '' : 'disabled'} data-input-action="setClankerRequestedField" data-arg0="clankerRequestedLabel" data-arg-types="s,v" title="Label that marks a PR as clanker-requested; must not be blank">
          </div>
          <div class="config-field">
            <label for="cfg-clanker-addendum">Comment addendum</label>
            <textarea id="cfg-clanker-addendum" rows="3" maxlength="1000" placeholder="Optional text appended to the explanation (max 1000 bytes)" ${st.clankerRequested ? '' : 'disabled'} data-input-action="setClankerRequestedField" data-arg0="clankerRequestedAddendum" data-arg-types="s,v" title="Optional free text appended to the explanation comment">${esc(st.clankerRequestedAddendum)}</textarea>
          </div>
          <div class="fs-sm mb-2 rt-subhead">Comment preview</div>
          <pre id="cfg-clanker-preview" class="fs-xs" style="white-space:pre-wrap">${esc(clankerRequestedPreviewText(st.clankerRequestedLabel, st.clankerRequestedAddendum))}</pre>
        </div>`;
    }

    // Kick-list cap bounds, mirroring MinKickListCap / MaxKickListCap /
    // DefaultMaxIssuesPerKick / DefaultMaxPRsPerKick in pkg/config/kick_limits.go.
    // The server re-validates, so these only shape the input and the help text.
    const KICK_LIST_CAP_MAX = 500;
    const KICK_LIST_CAP_ISSUE_DEFAULT = 100;
    const KICK_LIST_CAP_PR_DEFAULT = 50;

    function renderGovRepos(repos) {
      const primary = _configState.data.primaryRepo || '';
      const selfAuthDefault = _configState.data.selfAuthorizationHold !== false;
      const selfAuthEnvLocked = !!_configState.data.selfAuthorizationHoldEnvLocked;
      const repoSelfAuth = _configState.data.repoSelfAuthorizationHold || {};
      // #9665 reporter-trust merge hold: same three-way shape as #5117
      // (all-repos default, per-repo override, env lock).
      const rtHoldDefault = !!_configState.data.reporterTrustHold;
      const rtHoldEnvLocked = !!_configState.data.reporterTrustHoldEnvLocked;
      const repoRtHold = _configState.data.repoReporterTrustHold || {};
      const rtGateOn = !!((_configState.data.reporterTrust || {}).enabled);
      // #15335 current-base merge gate: hive-wide default lives on the
      // Features tab ("Merge only on current base"); each repo row carries
      // the inherit / on / off override.
      const cbDefault = !!_configState.data.currentBaseDefault;
      const repoCurrentBase = _configState.data.repoCurrentBase || {};
      // #11313 label-driven approval: per-repo opt-in plus project-wide trust.
      const repoLabelDriven = _configState.data.repoLabelDriven || {};
      const ldApproval = _configState.data.labelDrivenApproval || {};
      const ldDefaultLabels = Array.isArray(ldApproval.approvalLabels) ? ldApproval.approvalLabels : ['triage/accepted'];
      const ldLocked = !dashboardRoleAtLeast(window._hiveRole || 'read', 'write');
      const anyLabelDriven = repos.some(r => !!(repoLabelDriven[r] || {}).enabled);
      // The API sends EFFECTIVE caps (defaults already resolved), so these
      // boxes always show the number actually in force rather than a blank.
      // Dirty edits win so a value the operator just typed survives a re-render
      // (adding a repo re-renders this whole tab before Save).
      const dirtyCaps = _configState.dirty.repos || {};
      const caps = {
        issues: dirtyCaps.maxIssuesPerKick !== undefined
          ? dirtyCaps.maxIssuesPerKick
          : (_configState.data.maxIssuesPerKick || KICK_LIST_CAP_ISSUE_DEFAULT),
        prs: dirtyCaps.maxPRsPerKick !== undefined
          ? dirtyCaps.maxPRsPerKick
          : (_configState.data.maxPRsPerKick || KICK_LIST_CAP_PR_DEFAULT),
      };
      // ghHost is the hive's forge, derived server-side by ResolvedBaseURL()
      // (base_url, or the api_url host for pooled GHE hives that carry
      // base_url: ""). A hive is single-forge — validateSingleRepoHost rejects
      // off-host repos — so every repo below lives on THIS host. We surface it
      // both in the header and as a per-row pill so an org/repo can never be
      // mistaken for github.com when it actually lives on github.ibm.com.
      const ghBase = window._githubBaseUrl || 'https://github.com';
      const ghHost = ghBase.replace(/^https?:\/\//, '').replace(/\/$/, '');
      const isGHE = ghHost !== 'github.com';
      // GHE hosts get an indigo "Enterprise" pill so an operator instantly sees
      // this is an Enterprise hive; public github.com gets a muted host pill.
      // Inline styles mirror the pill recipe (--pill-c tint + border) and the
      // ACMM badge pattern — self-contained, no external resources (CSP-safe).
      const pillColor = isGHE ? 'var(--indigo)' : 'var(--muted)';
      const pillBase = 'display:inline-flex;align-items:center;gap:var(--sp-2);flex-shrink:0;'
        + 'font-size:var(--fs-2xs);font-weight:700;letter-spacing:0.02em;padding:1px 7px;'
        + 'border-radius:var(--r-pill);white-space:nowrap;'
        + 'background:color-mix(in srgb, ' + pillColor + ' 12%, transparent);'
        + 'border:1px solid color-mix(in srgb, ' + pillColor + ' 40%, transparent);'
        + 'color:' + pillColor;
      const pillLabel = isGHE ? esc(ghHost) + ' · Enterprise' : esc(ghHost);
      const pillTitle = isGHE
        ? 'This repo lives on the GitHub Enterprise host ' + esc(ghHost) + ' — NOT public github.com'
        : 'This repo lives on public github.com';
      const list = repos.map((r, i) => {
        const isDefault = r === primary;
        const starStyle = isDefault
          ? 'color:var(--amber);cursor:pointer;font-size:1rem;margin-right:6px'
          : 'color:var(--muted);cursor:pointer;font-size:1rem;margin-right:var(--sp-3);opacity:0.4';
        const starTitle = isDefault
          ? 'Default repo (advisory issue lives here)'
          : 'Set as default repo for advisory issue';
        const policy = repoSelfAuth[r] || {};
        const hasOverride = Object.prototype.hasOwnProperty.call(policy, 'value');
        const effectiveHold = selfAuthEnvLocked && Object.prototype.hasOwnProperty.call(policy, 'effective') ? !!policy.effective : (hasOverride ? !!policy.value : selfAuthDefault);
        const holdTitle = hasOverride
          ? 'This repo has an explicit #5117 self-authorization hold override.'
          : 'Inherits the all-repos #5117 self-authorization hold default.';
        const inheritButton = hasOverride
          ? '<button class="remove-item" data-action="clearRepoSelfAuthHoldOverride" data-arg0="' + esc(r) + '" data-arg-types="s" title="Clear this repo override and inherit the all-repos default">inherit</button>'
          : '<span style="font-size:var(--fs-xs);color:var(--muted);white-space:nowrap">inherits</span>';
        const rtPolicy = repoRtHold[r] || {};
        const rtHasOverride = Object.prototype.hasOwnProperty.call(rtPolicy, 'value');
        const rtEffective = rtHoldEnvLocked && Object.prototype.hasOwnProperty.call(rtPolicy, 'effective') ? !!rtPolicy.effective : (rtHasOverride ? !!rtPolicy.value : rtHoldDefault);
        const rtTitle = rtHasOverride
          ? 'This repo has an explicit #9665 reporter-trust hold override.'
          : 'Inherits the all-repos #9665 reporter-trust hold default.';
        const rtInherit = rtHasOverride
          ? '<button class="remove-item" data-action="clearRepoReporterTrustHoldOverride" data-arg0="' + esc(r) + '" data-arg-types="s" title="Clear this repo override and inherit the all-repos default">inherit</button>'
          : '<span class="rt-inherit">inherits</span>';
        const rtRow = '<label title="' + rtTitle + '" class="rt-check"><input type="checkbox" ' + (rtEffective ? 'checked' : '') + ' ' + (rtHoldEnvLocked ? 'disabled' : '') + ' data-change-action="toggleRepoReporterTrustHold" data-arg0="' + esc(r) + '" data-arg-types="s,c"> Hold PRs for untrusted reporters (#9665)</label>' + rtInherit;
        const cbPolicy = repoCurrentBase[r] || {};
        const cbHasOverride = Object.prototype.hasOwnProperty.call(cbPolicy, 'value');
        const cbEffective = cbHasOverride ? !!cbPolicy.value : (Object.prototype.hasOwnProperty.call(cbPolicy, 'effective') ? !!cbPolicy.effective : cbDefault);
        const cbTitle = cbHasOverride
          ? 'This repo has an explicit "merge only on current base" override (auto_merge.' + (cbPolicy.value ? 'require_current_base' : 'skip_current_base') + ').'
          : 'Inherits the Features tab\'s "Merge only on current base" default (currently ' + (cbDefault ? 'on' : 'off') + ').';
        const cbInherit = cbHasOverride
          ? '<button class="remove-item" data-action="clearRepoCurrentBaseOverride" data-arg0="' + esc(r) + '" data-arg-types="s" title="Clear this repo override and inherit the Features default">inherit</button>'
          : '<span class="rt-inherit">inherits</span>';
        const cbRow = '<label title="' + cbTitle + '" class="rt-check"><input type="checkbox" ' + (cbEffective ? 'checked' : '') + ' data-change-action="toggleRepoCurrentBase" data-arg0="' + esc(r) + '" data-arg-types="s,c"> Merge only on current base</label>' + cbInherit;
        // #11313 label-driven approvals (Prow-style): opt-in per repo, with the
        // sub-row (approval labels + help) only while the toggle is on.
        const ld = repoLabelDriven[r] || {};
        const ldOn = !!ld.enabled;
        const ldRow = '<label class="rt-check" title="Opt-in: on this repo Hive leaves triage labels alone; a trusted triager applying ' + esc(ldDefaultLabels.join(', ') || 'the approval label') + ' releases parked issues and counts as human sign-off (#11313)"><input type="checkbox" ' + (ldOn ? 'checked' : '') + ' ' + (ldLocked ? 'disabled' : '') + ' data-change-action="toggleRepoLabelDriven" data-arg0="' + esc(r) + '" data-arg-types="s,c"> Label-driven approvals (Prow-style)</label>';
        const ldSub = `<div class="config-list-item ld-sub" data-ld-repo="${esc(r)}">${ldRow}${ldOn ? `
            <div class="config-field"><label>Approval labels ${settingHelpMark('maintainer-commands', 'label-driven-repositories', 'Approval labels', 'Comma-separated labels whose trusted addition approves an issue on this repo. Empty inherits the all-repos default (' + esc(ldDefaultLabels.join(', ')) + ').')}</label><input type="text" ${ldLocked ? 'disabled' : ''} value="${esc((ld.approvalLabels || []).join(', '))}" placeholder="${esc(ldDefaultLabels.join(', '))}" data-change-action="setRepoLabelDrivenLabels" data-arg0="${esc(r)}" data-arg-types="s,v"></div>
            <p class="config-help">Opt-in: <code>${esc((ld.effectiveLabels || ldDefaultLabels).join('</code>, <code>'))}</code> applied by a trusted triager/org member releases parked issues and counts as human sign-off. <a href="${esc(dashboardDocsHref('maintainer-commands', 'label-driven-repositories'))}" target="_blank" rel="noopener noreferrer" data-docs-page="maintainer-commands" data-docs-anchor="label-driven-repositories" data-action="gh3" data-stop="1">How label-driven repositories work</a></p>` : ''}
          </div>`;
        // Fully forge-qualified so the row is unambiguous: github.ibm.com/org/repo.
        const displayName = ghHost + '/' + r;
        return `<div class="config-list-item config-repo-row" style="align-items:center;gap:var(--sp-3)"><span data-action="setDefaultRepo" data-arg0="${esc(r)}" style="${starStyle}" title="${starTitle}">${isDefault ? '★' : '☆'}</span><span class="config-repo-name" title="${esc(displayName)}">${esc(displayName)}</span><label class="rt-check" title="${holdTitle}"><input type="checkbox" ${effectiveHold ? 'checked' : ''} ${selfAuthEnvLocked ? 'disabled' : ''} data-change-action="toggleRepoSelfAuthHold" data-arg0="${esc(r)}" data-arg-types="s,c"> Hold hive-authored PRs for human sign-off (#5117)</label>${inheritButton}${rtRow}${cbRow}<span style="${pillBase}" title="${pillTitle}">${pillLabel}</span><button class="remove-item" data-action="removeListItem" data-arg0="repos" data-arg1="list" data-arg2="${i}" data-arg-types="s,s,j">&#x2715;</button></div>` + ldSub;
      }).join('');
      const hostNote = isGHE
        ? ` on <strong style="color:var(--indigo)">${esc(ghHost)}</strong> (GitHub Enterprise)`
        : ` on <strong>${esc(ghHost)}</strong>`;
      // The forge is shown as a fixed, non-editable prefix before the input so
      // the user only types org/repo and the GitHub host can never be
      // mistaken (requirement #1). The prefix color matches the per-row pill
      // (indigo for GHE, muted for public github.com). All host machinery is
      // client-side + CSP-safe (inline styles, no external resources).
      const prefixColor = isGHE ? 'var(--indigo)' : 'var(--muted)';
      // needDefault is true when repos exist but none is the current default —
      // the Save-guard state. renderGovRepos surfaces it inline; syncRepoSaveGuard
      // (called on every tab render) is what actually disables the Save button.
      const needDefault = repos.length > 0 && !repos.includes(primary);
      const defaultWarn = needDefault
        ? '<div id="cfg-repo-default-warn" style="font-size:0.72rem;color:var(--amber);margin-top:var(--sp-3)">⚠ Set a default repo before saving — click a ☆ star to choose the repo where the advisory issue is maintained.</div>'
        : '';
      return `
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">GitHub repositories monitored by this hive${hostNote}. Click the star to set the default repo where the advisory issue is maintained.</p>
        <div class="config-toggle" style="margin:var(--sp-0) var(--sp-0) var(--sp-5)">
          <div class="config-toggle-switch ${selfAuthDefault ? 'on' : ''} ${selfAuthEnvLocked ? 'disabled' : ''}" data-action="${selfAuthEnvLocked ? '' : 'toggleSelfAuthDefault'}" data-arg-types="t" title="${selfAuthEnvLocked ? 'HIVE_SELF_AUTHORIZATION_HOLD is set, so the process environment controls this value.' : 'All-repos default for the #5117 self-authorization hold.'}"></div>
          <span class="config-toggle-label">All repos: hold hive-authored PRs for human sign-off (#5117) <span style="color:var(--muted);font-weight:400">— repo rows can override or inherit this default</span></span>
        </div>
        <div class="config-toggle rt-toggle-lg">
          <div class="config-toggle-switch ${rtHoldDefault ? 'on' : ''} ${rtHoldEnvLocked ? 'disabled' : ''}" data-action="${rtHoldEnvLocked ? '' : 'toggleReporterTrustHoldDefault'}" data-arg-types="t" title="${rtHoldEnvLocked ? 'HIVE_REPORTER_TRUST_HOLD is set; the environment controls this value.' : 'Hold, at every ACMM level, any PR whose rationale traces to an issue from a reporter this hive does not trust'}"></div>
          <span class="config-toggle-label">All repos: hold PRs for untrusted reporters at every level (#9665) ${yamlHint('github.reporter_trust_hold')} <span class="rt-sub">— follows the Labels tab's reporter-trust switch${rtGateOn ? '' : ' (currently off)'} unless set here; a human removes the hold, Hive never does</span></span>
        </div>
        <div class="config-list">
          ${list || '<div class="fs-sm text-muted mb-3">No repos configured</div>'}
          <div class="config-list-add" style="align-items:stretch">
            <div style="display:flex;flex:1;min-width:0;align-items:stretch;border:1px solid var(--border);border-radius:var(--r);overflow:hidden">
              <span style="display:inline-flex;align-items:center;padding:var(--sp-0) var(--sp-4);background:color-mix(in srgb, ${prefixColor} 12%, var(--surface));color:${prefixColor};font-size:0.78rem;font-weight:600;white-space:nowrap;border-right:1px solid var(--border)" title="This hive's forge — every repo must live on ${esc(ghHost)}">${esc(ghHost)}/</span>
              <input type="text" id="cfg-repo-input" style="flex:1;min-width:0;border:none;border-radius:0" placeholder="org/repo or GitHub URL" data-keydown-action="addListItem" data-arg0="repos" data-arg1="list" data-arg2="cfg-repo-input" data-prevent="1" data-keys="Enter" title="GitHub repository in org/repo format (e.g. kubestellar/console) or a full GitHub URL. It must live on this hive's forge, ${esc(ghHost)} — saving a repo from a different org migrates this hive to that org. It will be monitored for issues, PRs, and CI status.">
            </div>
            <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="repos" data-arg1="list" data-arg2="cfg-repo-input" title="Add this repository to the monitored list">+ Add</button>
          </div>
          <div id="cfg-repo-add-status" style="font-size:0.72rem;margin-top:var(--sp-3)"></div>
          ${defaultWarn}
        </div>
        ${anyLabelDriven ? `<div class="ld-defaults" id="cfg-label-driven-defaults">
          <div class="config-toggle">
            <div class="config-toggle-switch ${ldApproval.trustOrgMembers !== false ? 'on' : ''} ${ldLocked ? 'disabled' : ''}" data-action="${ldLocked ? '' : 'toggleLabelDrivenTrustOrgMembers'}" data-arg-types="t" title="Prow's own rule: public members of the repo's org may apply the approval label. Off: only triage/write/maintain/admin collaborators and trusted bots."></div>
            <span class="config-toggle-label">Label-driven repos: trust public org members to apply approval labels ${settingHelpMark('operator-reference', 'label-driven-approval', 'Trust org members', 'When on (default), a public member of the repository owner org who applies the approval label is trusted, matching Prow. Collaborators with the triage role or higher are always trusted.')}</span>
          </div>
          <div class="config-field-row">
            <div class="config-field">
              <label>Default approval labels ${settingHelpMark('operator-reference', 'label-driven-approval', 'Default approval labels', 'Comma-separated. Applies to every label-driven repo without its own list. Empty restores triage/accepted.')}</label>
              <input type="text" id="cfg-label-driven-approval-labels" ${ldLocked ? 'disabled' : ''} value="${esc(ldDefaultLabels.join(', '))}" placeholder="triage/accepted" data-change-action="setLabelDrivenApprovalDefault" data-arg-types="v">
            </div>
            <div class="config-field">
              <label>Trusted bots ${settingHelpMark('operator-reference', 'label-driven-approval', 'Trusted bots', 'Comma-separated bot logins (for example kubestellar-prow[bot]) whose label additions count as a trusted triager. Empty restores the built-in list.')}</label>
              <input type="text" id="cfg-label-driven-trusted-bots" ${ldLocked ? 'disabled' : ''} value="${esc((ldApproval.trustedBots || []).join(', '))}" placeholder="kubestellar-prow[bot]" data-change-action="setLabelDrivenTrustedBots" data-arg-types="v">
            </div>
          </div>
          <p class="config-help">Only the last <em>labeled</em> event counts; an unreadable timeline fails closed. <code>hold</code> and <code>do-not-merge</code> still park. Nothing changes for repos without the toggle.</p>
        </div>` : ''}
        <div style="margin-top:22px;padding-top:var(--sp-6);border-top:1px solid var(--border)">
          <div style="font-size:0.8rem;font-weight:600;margin-bottom:var(--sp-2)">Kick prompt list caps</div>
          <p class="fs-sm text-muted mb-5">
            These caps limit how many issues and pull requests are listed in an agent's kick
            dialog. They exist to keep the prompt a manageable size for the agents that read
            those lists — <strong>scanner</strong> above all, which receives both in full. The
            lists are pasted into the prompt as-is, so without a cap the prompt grows with your
            backlog: a hive with 302 open PRs produced a 69.5&nbsp;KiB kick against a budget of
            about 25&nbsp;KiB, and because a kick is typed into a terminal, an oversized prompt
            also takes minutes to deliver. Lower them if your backlog is large; raise them if
            your hive is small and you want agents to see everything.
          </p>
          <div class="config-field-row">
            <div class="config-field">
              <label>Max issues per kick ${settingHelpMark('settings', 'repos', 'Max issues per kick', `How many issues at most are listed in a kick dialog. Issues are listed oldest first, so the cap keeps the oldest (most overdue) work and drops the newest — the agent is told how many were omitted. Range 1-${KICK_LIST_CAP_MAX}. Default ${KICK_LIST_CAP_ISSUE_DEFAULT}.`)}</label>
              <input type="number" min="1" max="${KICK_LIST_CAP_MAX}" value="${caps.issues}" data-change-action="gh79">
            </div>
            <div class="config-field">
              <label>Max PRs per kick ${settingHelpMark('settings', 'repos', 'Max PRs per kick', `How many pull requests at most are listed in a kick dialog. PRs are listed in review-priority order (fixes before refactors and docs before tests, oldest first within each class), so the cap keeps the highest-priority PRs and drops the rest — the agent is told how many were omitted. Range 1-${KICK_LIST_CAP_MAX}. Default ${KICK_LIST_CAP_PR_DEFAULT}.`)}</label>
              <input type="number" min="1" max="${KICK_LIST_CAP_MAX}" value="${caps.prs}" data-change-action="gh80">
            </div>
          </div>
        </div>`;
    }

    function setDefaultRepo(repo) {
      _configState.data.primaryRepo = repo;
      markDirty('repos', 'primaryRepo', repo);
      markDirty('repos', 'repos', _configState.data.repos || []);
      renderConfigTab('Repos');
    }

    function toggleSelfAuthDefault(el) {
      if (_configState.data.selfAuthorizationHoldEnvLocked) {
        showToast('HIVE_SELF_AUTHORIZATION_HOLD is set; the environment controls this value.', 'error');
        return;
      }
      const isOn = el.classList.toggle('on');
      _configState.data.selfAuthorizationHold = isOn;
      markDirty('repos', 'selfAuthorizationHold', isOn);
      renderConfigTab('Repos');
    }

    function toggleRepoSelfAuthHold(repo, checked) {
      if (_configState.data.selfAuthorizationHoldEnvLocked) {
        showToast('HIVE_SELF_AUTHORIZATION_HOLD is set; repo overrides are disabled.', 'error');
        renderConfigTab('Repos');
        return;
      }
      if (!_configState.data.repoSelfAuthorizationHold) _configState.data.repoSelfAuthorizationHold = {};
      _configState.data.repoSelfAuthorizationHold[repo] = { value: !!checked, effective: !!checked };
      markDirty('repos', 'repoSelfAuthorizationHold', Object.fromEntries(Object.entries(_configState.data.repoSelfAuthorizationHold).map(([k, v]) => [k, Object.prototype.hasOwnProperty.call(v, 'value') ? v.value : null])));
      renderConfigTab('Repos');
    }

    function toggleReporterTrustHoldDefault(el) {
      if (_configState.data.reporterTrustHoldEnvLocked) {
        showToast('HIVE_REPORTER_TRUST_HOLD is set; the environment controls this value.', 'error');
        return;
      }
      const isOn = el.classList.toggle('on');
      _configState.data.reporterTrustHold = isOn;
      markDirty('repos', 'reporterTrustHold', isOn);
      renderConfigTab('Repos');
    }

    function repoReporterTrustHoldPayload() {
      return Object.fromEntries(Object.entries(_configState.data.repoReporterTrustHold || {}).map(([k, v]) => [k, Object.prototype.hasOwnProperty.call(v, 'value') ? v.value : null]));
    }

    function toggleRepoReporterTrustHold(repo, checked) {
      if (_configState.data.reporterTrustHoldEnvLocked) {
        showToast('HIVE_REPORTER_TRUST_HOLD is set; repo overrides are disabled.', 'error');
        renderConfigTab('Repos');
        return;
      }
      if (!_configState.data.repoReporterTrustHold) _configState.data.repoReporterTrustHold = {};
      _configState.data.repoReporterTrustHold[repo] = { value: !!checked, effective: !!checked };
      markDirty('repos', 'repoReporterTrustHold', repoReporterTrustHoldPayload());
      renderConfigTab('Repos');
    }

    function clearRepoReporterTrustHoldOverride(repo) {
      if (_configState.data.reporterTrustHoldEnvLocked) {
        showToast('HIVE_REPORTER_TRUST_HOLD is set; repo overrides are disabled.', 'error');
        return;
      }
      if (!_configState.data.repoReporterTrustHold) _configState.data.repoReporterTrustHold = {};
      _configState.data.repoReporterTrustHold[repo] = { effective: !!_configState.data.reporterTrustHold };
      const payload = repoReporterTrustHoldPayload();
      payload[repo] = null;
      markDirty('repos', 'repoReporterTrustHold', payload);
      renderConfigTab('Repos');
    }

    // #15335 current-base gate per-repo override (Repos tab): true writes the
    // repo into auto_merge.require_current_base, false into
    // auto_merge.skip_current_base, null clears it from both.
    function repoCurrentBasePayload() {
      return Object.fromEntries(Object.entries(_configState.data.repoCurrentBase || {}).map(([k, v]) => [k, Object.prototype.hasOwnProperty.call(v, 'value') ? v.value : null]));
    }

    function toggleRepoCurrentBase(repo, checked) {
      if (!_configState.data.repoCurrentBase) _configState.data.repoCurrentBase = {};
      _configState.data.repoCurrentBase[repo] = { value: !!checked, effective: !!checked };
      markDirty('repos', 'repoCurrentBase', repoCurrentBasePayload());
      renderConfigTab('Repos');
    }

    function clearRepoCurrentBaseOverride(repo) {
      if (!_configState.data.repoCurrentBase) _configState.data.repoCurrentBase = {};
      _configState.data.repoCurrentBase[repo] = { effective: !!_configState.data.currentBaseDefault };
      const payload = repoCurrentBasePayload();
      payload[repo] = null;
      markDirty('repos', 'repoCurrentBase', payload);
      renderConfigTab('Repos');
    }

    // #11313 label-driven approval controls (Repos tab). Per-repo entries are
    // sent whole so the server can clear or set the opt-in and its labels in
    // one PUT; project-wide knobs travel in their own bag.
    function hiveSplitLabelList(value) {
      return String(value || '').split(',').map(v => v.trim()).filter(Boolean);
    }
    function repoLabelDrivenPayload() {
      return Object.fromEntries(Object.entries(_configState.data.repoLabelDriven || {}).map(([k, v]) => [k, v && v.enabled ? { enabled: true, approvalLabels: v.approvalLabels || [] } : null]));
    }
    function toggleRepoLabelDriven(repo, checked) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'write')) {
        showToast('Write or owner role is required to change label-driven approvals.', 'error');
        renderConfigTab('Repos');
        return;
      }
      if (!_configState.data.repoLabelDriven) _configState.data.repoLabelDriven = {};
      const prev = _configState.data.repoLabelDriven[repo] || {};
      _configState.data.repoLabelDriven[repo] = checked ? { enabled: true, approvalLabels: prev.approvalLabels || [] } : { enabled: false };
      markDirty('repos', 'repoLabelDriven', repoLabelDrivenPayload());
      renderConfigTab('Repos');
    }
    function setRepoLabelDrivenLabels(repo, value) {
      if (!_configState.data.repoLabelDriven) _configState.data.repoLabelDriven = {};
      const prev = _configState.data.repoLabelDriven[repo] || {};
      const labels = hiveSplitLabelList(value);
      _configState.data.repoLabelDriven[repo] = { enabled: true, approvalLabels: labels, effectiveLabels: labels.length ? labels : undefined };
      markDirty('repos', 'repoLabelDriven', repoLabelDrivenPayload());
      renderConfigTab('Repos');
    }
    function labelDrivenApprovalDirty() {
      const d = _configState.dirty.repos || {};
      return Object.assign({}, d.labelDrivenApproval || {});
    }
    function toggleLabelDrivenTrustOrgMembers(el) {
      const isOn = el.classList.toggle('on');
      if (!_configState.data.labelDrivenApproval) _configState.data.labelDrivenApproval = {};
      _configState.data.labelDrivenApproval.trustOrgMembers = isOn;
      markDirty('repos', 'labelDrivenApproval', Object.assign(labelDrivenApprovalDirty(), { trustOrgMembers: isOn }));
    }
    function setLabelDrivenApprovalDefault(value) {
      if (!_configState.data.labelDrivenApproval) _configState.data.labelDrivenApproval = {};
      const labels = hiveSplitLabelList(value);
      _configState.data.labelDrivenApproval.approvalLabels = labels.length ? labels : ['triage/accepted'];
      markDirty('repos', 'labelDrivenApproval', Object.assign(labelDrivenApprovalDirty(), { approvalLabels: labels }));
      renderConfigTab('Repos');
    }
    function setLabelDrivenTrustedBots(value) {
      if (!_configState.data.labelDrivenApproval) _configState.data.labelDrivenApproval = {};
      const bots = hiveSplitLabelList(value);
      _configState.data.labelDrivenApproval.trustedBots = bots;
      markDirty('repos', 'labelDrivenApproval', Object.assign(labelDrivenApprovalDirty(), { trustedBots: bots }));
    }

    function clearRepoSelfAuthHoldOverride(repo) {
      if (_configState.data.selfAuthorizationHoldEnvLocked) {
        showToast('HIVE_SELF_AUTHORIZATION_HOLD is set; repo overrides are disabled.', 'error');
        return;
      }
      const effective = _configState.data.selfAuthorizationHold !== false;
      if (!_configState.data.repoSelfAuthorizationHold) _configState.data.repoSelfAuthorizationHold = {};
      _configState.data.repoSelfAuthorizationHold[repo] = { effective: effective };
      const payload = Object.fromEntries(Object.entries(_configState.data.repoSelfAuthorizationHold).map(([k, v]) => [k, Object.prototype.hasOwnProperty.call(v, 'value') ? v.value : null]));
      payload[repo] = null;
      markDirty('repos', 'repoSelfAuthorizationHold', payload);
      renderConfigTab('Repos');
    }

    function renderGovBudget(b, h) {
      loadBudgetExemptions();
      h = h || {};
      const bobCoin = ((b.coins || {}).bob) || {};
      const coinLabel = bobCoin.label || 'BC';
      return `
        <div class="config-field-row">
          <div class="config-field"><label>Total Tokens ${settingHelpMark('settings', 'budget', 'Total Tokens', `Maximum token budget for the period. When usage approaches this limit, the governor will downgrade models or skip low-priority kicks. Set to 0 to disable budget tracking.`)}</label><input type="number" value="${b.totalTokens || 0}" data-change-action="gh43"></div>
          <div class="config-field"><label>Period (days) ${settingHelpMark('settings', 'budget', 'Period (days)', `Rolling window length in days for token budget accounting. Default is 7 (weekly). The budget resets at the end of each period.`)}</label><input type="number" value="${b.periodDays || 7}" data-change-action="gh44"></div>
        </div>
        <div class="config-field-row">
          <div class="config-field"><label>USD Budget ${yamlHint('governor.budget.usd')} ${settingHelpMark('settings', 'budget', 'USD Budget', `Optional Bob budget cap in dollars for the period. Set to 0 to disable this dollar cap.`)}</label><input id="gov-budget-usd" type="number" step="0.01" min="0" value="${b.usd || 0}" data-change-action="gh85"></div>
          <div class="config-field"><label>${esc(coinLabel)} Budget ${settingHelpMark('settings', 'budget', coinLabel + ' Budget', `Optional Bob coin cap for the period. Uses the configured Bob coin conversion; confirm the conversion with your Bob team.`)}</label><input id="gov-budget-coin" type="number" step="0.01" min="0" value="${bobCoin.budget || 0}" data-change-action="gh86"></div>
        </div>
        <div class="config-field">
          <label>Bob budget entry unit</label>
          <select data-change-action="gh87"><option value="usd">USD</option><option value="coins">${esc(coinLabel)}</option></select>
          <div style="font-size:var(--fs-xs);color:var(--muted);margin-top:4px">USD and ${esc(coinLabel)} caps are both first-class; set either or both. The governor stops kicks when the first configured cap is exhausted.</div>
        </div>
        <div class="config-field">
          <label>Critical Percent ${settingHelpMark('settings', 'budget', 'Critical Percent', `When token usage exceeds this percentage of the total budget, the governor enters budget-critical mode: downgrading models to cheaper alternatives and potentially skipping non-essential kicks. Default is 90%.`)}</label>
          <input type="number" value="${b.criticalPct || 90}" data-change-action="gh45">
        </div>
        <div class="config-field" style="margin-top:14px">
          <label>Per-Agent Exemptions ${settingHelpMark('settings', 'budget', 'Per-Agent Exemptions', `Exempt agents keep getting kicked even while an exhausted budget suppresses everyone else. Toggles apply immediately (no Save needed). The governor panel's global "ignore budget" checkbox bypasses enforcement for ALL agents; these exemptions are per-agent.`)}</label>
          <div id="budget-exempt-list"><div class="var-empty">Loading exemptions...</div></div>
        </div>
        <div class="config-toggle" style="margin-top:14px">
          <div class="config-toggle-switch ${h.modelLock ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="health" data-key="modelLock"></div>
          <span class="config-toggle-label">Model Lock ${yamlHint('governor.health.model_lock')} ${settingHelpMark('settings', 'budget', 'Model Lock', `When enabled, the governor will not automatically change any agent's model (e.g. downgrading from Opus to Sonnet during budget pressure). Agents keep their configured model regardless of budget status. Does NOT prevent manual model changes or agent kicks.`)}</span>
        </div>`;
    }

    // ── Budget per-agent exemptions (governor IgnoredAgents) ──
    // Toggles POST the array form of /api/budget-ignore ({ignored: [names]}),
    // which sets the per-agent list; the bool form remains the global toggle.
    let _budgetExemptAgents = new Set();

    async function loadBudgetExemptions() {
      try {
        const r = await fetch('/api/budget-ignore');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        _budgetExemptAgents = new Set(d.agents || []);
        renderBudgetExemptList(d.by_agent || {});
      } catch (e) {
        const el = document.getElementById('budget-exempt-list');
        if (el) el.innerHTML = '<div class="text-muted fs-sm">Could not load exemptions.</div>';
      }
    }

    function renderBudgetExemptList(byAgent) {
      const el = document.getElementById('budget-exempt-list');
      if (!el) return;
      const agents = [...(window._lastAgents || [])].sort((a, b) => (a.sortOrder || 100) - (b.sortOrder || 100));
      if (agents.length === 0) {
        el.innerHTML = '<div class="text-muted fs-sm">No agents configured.</div>';
        return;
      }
      const rows = agents.map(a => {
        const burn = byAgent[a.name] || 0;
        const exempt = _budgetExemptAgents.has(a.name);
        return `<div class="config-list-item var-row">
          <span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${a.emoji ? a.emoji + ' ' : ''}${esc(a.displayName || a.name)}</span>
          <span style="color:var(--muted);font-size:0.7rem;white-space:nowrap" title="Tokens recorded for this agent by the governor's budget tracker">${burn > 0 ? formatNumber(burn) + ' tok' : '—'}</span>
          <label style="display:flex;align-items:center;gap:var(--sp-2);font-size:0.72rem;color:var(--muted);cursor:pointer;white-space:nowrap">
            <input type="checkbox" ${exempt ? 'checked' : ''} data-change-action="toggleBudgetExempt" data-arg0="${esc(a.name)}" data-arg-types="s,c" style="cursor:pointer"> exempt
          </label>
        </div>`;
      }).join('');
      el.innerHTML = rows;
    }

    async function toggleBudgetExempt(name, checked) {
      if (checked) _budgetExemptAgents.add(name);
      else _budgetExemptAgents.delete(name);
      try {
        const r = await fetch('/api/budget-ignore', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ ignored: [..._budgetExemptAgents] })
        });
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        _budgetExemptAgents = new Set(d.agents || []);
        showToast(checked ? `${name} exempted from budget suppression` : `${name} no longer exempt`, 'success');
      } catch (e) {
        // Revert the optimistic flip and re-render so the checkbox matches reality.
        if (checked) _budgetExemptAgents.delete(name);
        else _budgetExemptAgents.add(name);
        showToast('Failed to update budget exemptions: ' + e.message, 'error');
        loadBudgetExemptions();
      }
    }

    const HIVE_NOTIFICATION_EVENTS = [
      ['sweep_completed', 'Sweep completed'],
      ['escalation_red', 'Escalation red'],
      ['stage_completed', 'Run stage completed'],
      ['issue_claimed', 'Issue claimed'],
      ['issue_released', 'Issue released']
    ];

    function selectedNotificationEvents() {
      return Array.from(document.querySelectorAll('.notification-event-checkbox:checked')).map(el => el.value);
    }

    function markNotificationEventsDirty() {
      markDirty('notifications', 'events', selectedNotificationEvents());
    }

    function renderGovNotifications(n) {
      const ntfyHint = n.hasNtfy ? '(configured — enter new value to change)' : '';
      const discordHint = n.hasDiscord ? '(configured — enter new value to change)' : '';
      const slackHint = n.hasSlack ? '(configured — enter new value to change)' : '';
      const selected = new Set(n.events || []);
      const eventBoxes = HIVE_NOTIFICATION_EVENTS.map(([value, label]) => `
        <label class="config-toggle-label">
          <input type="checkbox" class="notification-event-checkbox" value="${esc(value)}" ${selected.has(value) ? 'checked' : ''} data-change-action="gh96"> ${esc(label)}
        </label>`).join('');
      return `
        <div class="config-field">
          <label>Ntfy Server ${ntfyHint} ${yamlHint('notifications.ntfy.server')} ${settingHelpMark('notifications', 'configuration', 'Ntfy Server', `URL of the ntfy.sh server for push notifications. The governor sends alerts for mode changes, budget warnings, agent crashes, and rate limits. Leave empty to disable ntfy notifications. Default: https://ntfy.sh`)}</label>
          <input type="text" value="${esc(n.ntfyServer || '')}" data-change-action="markDirty" data-arg0="notifications" data-arg1="ntfyServer" data-arg-types="s,s,v" placeholder="https://ntfy.sh">
        </div>
        <div class="config-field">
          <label>Ntfy Topic ${yamlHint('notifications.ntfy.topic')} ${settingHelpMark('notifications', 'configuration', 'Ntfy Topic', `Topic name on the ntfy server. Subscribe to this topic in the ntfy app or web UI to receive governor notifications on your phone or desktop.`)}</label>
          <input type="text" value="${esc(n.ntfyTopic || '')}" data-change-action="markDirty" data-arg0="notifications" data-arg1="ntfyTopic" data-arg-types="s,s,v" placeholder="my-topic">
        </div>
        <div class="config-field">
          <label>Discord Webhook ${discordHint} ${yamlHint('notifications.discord.webhook')} ${settingHelpMark('notifications', 'configuration', 'Discord Webhook', `Discord webhook URL for posting selected hive notifications to a channel. Create a webhook in your Discord server settings under Integrations.`)}</label>
          <input type="text" value="${n.hasDiscord ? '••••••••' : ''}" data-change-action="markDirty" data-arg0="notifications" data-arg1="discordWebhook" data-arg-types="s,s,v" placeholder="https://discord.com/api/webhooks/...">
        </div>
        <div class="config-field">
          <label>Slack Webhook ${slackHint} ${yamlHint('notifications.slack.webhook')} ${settingHelpMark('notifications', 'configuration', 'Slack Webhook', `Slack incoming webhook URL. It uses the same notify hook fanout as Discord and ntfy.`)}</label>
          <input type="text" value="${n.hasSlack ? '••••••••' : ''}" data-change-action="markDirty" data-arg0="notifications" data-arg1="slackWebhook" data-arg-types="s,s,v" placeholder="https://hooks.slack.com/services/...">
        </div>
        <div class="config-field">
          <label>Hive notification events ${yamlHint('notifications.events[]')} ${settingHelpMark('notifications', 'configuration', 'Hive notification events', `Select which hive transitions are fanned out through the notify hooks to Discord, Slack, and ntfy.`)}</label>
          <div class="config-grid notification-event-checkboxes">${eventBoxes}</div>
        </div>
        ${renderGovNotificationsDiscordBot(n)}
        ${renderGovNotificationsGithubActivity(n)}`;
    }

    function markNotificationsList(key, value) {
      markDirty('notifications', key, splitConfigLines(value));
    }

    function githubActivityState() {
      const n = ((_configState.data || {}).notifications) || {};
      n.githubActivity = n.githubActivity || {};
      return n.githubActivity;
    }

    function markGithubActivity(key, value) {
      githubActivityState()[key] = value;
      const prev = (_configState.dirty.notifications || {}).githubActivity || {};
      markDirty('notifications', 'githubActivity', Object.assign({}, prev, { [key]: value }));
    }

    function markGithubActivityList(key, value) {
      markGithubActivity(key, splitConfigLines(value));
    }

    function toggleGithubActivityFlag(el, key) {
      if (!el || el.classList.contains('disabled')) return;
      const isOn = el.classList.toggle('on');
      el.setAttribute('aria-checked', isOn ? 'true' : 'false');
      markGithubActivity(key, isOn);
    }

    function renderGovNotificationsDiscordBot(n) {
      const tokenText = n.hasDiscordBotToken ? 'Configured — enter a new token to rotate, or save an empty value to clear.' : 'Not configured.';
      const factoryText = n.hasDiscordFactoryWebhook ? 'Configured — enter a new URL to change, or save an empty value to clear.' : 'https://discord.com/api/webhooks/...';
      const requestsText = n.hasDiscordRequestsWebhook ? 'Configured — enter a new URL to change, or save an empty value to clear.' : 'https://discord.com/api/webhooks/...';
      return `
        <div class="config-subgroup">Discord bot ${settingHelpMark('notifications', 'configuration', 'Discord bot', `Optional Discord bot integration; separate from webhook notifications. The bot posts status to one channel and accepts commands only from allowed users.`)}</div>
        <div class="config-field">
          <label>Bot token ${yamlHint('notifications.discord.bot_token')} ${settingHelpMark('notifications', 'configuration', 'Discord bot token', `Discord bot token. The bot starts only when both the token and the channel id are set.`)}</label>
          <input type="password" autocomplete="new-password" placeholder="${esc(tokenText)}" data-change-action="markDirty" data-arg0="notifications" data-arg1="discordBotToken" data-arg-types="s,s,v">
          <div class="config-help">Write-only; the current token is never echoed. Requires restart.</div>
        </div>
        <div class="config-field">
          <label>Channel id ${yamlHint('notifications.discord.channel_id')} ${settingHelpMark('notifications', 'configuration', 'Discord channel id', `Discord channel the bot posts to and listens on, for example 1234567890.`)}</label>
          <input type="text" value="${esc(n.discordChannelId || '')}" placeholder="1234567890" data-change-action="markDirty" data-arg0="notifications" data-arg1="discordChannelId" data-arg-types="s,s,w">
          <div class="config-help">Requires restart.</div>
        </div>
        <div class="config-field">
          <label>RFC channel id ${yamlHint('notifications.discord.rfc_channel_id')} ${settingHelpMark('notifications', 'configuration', 'Discord RFC channel id', `Discord channel that receives RFC lifecycle notices: an RFC issue (rfc label or RFC-prefixed title) opened, closed, or taken off hold so it can be implemented. Posts with the bot token only; the command bot and the channel id above are not required. Empty turns the notices off.`)}</label>
          <input type="text" value="${esc(n.discordRfcChannelId || '')}" placeholder="1234567890" data-change-action="markDirty" data-arg0="notifications" data-arg1="discordRfcChannelId" data-arg-types="s,s,w">
          <div class="config-help">Empty disables RFC notices. Env HIVE_DISCORD_RFC_CHANNEL_ID overrides.</div>
        </div>
        <div class="config-field">
          <label>Allowed users ${yamlHint('notifications.discord.allowed_users[]')} ${settingHelpMark('notifications', 'configuration', 'Discord allowed users', `Discord user IDs permitted to issue bot commands (!kick, !pause, agent actions). When empty, command handling is disabled (fail closed): the bot still posts status but accepts no commands.`)}</label>
          <textarea rows="3" placeholder="One Discord user id per line" data-change-action="markNotificationsList" data-arg0="discordAllowedUsers" data-arg-types="s,v">${esc((n.discordAllowedUsers || []).join('\n'))}</textarea>
          <div class="config-help">Empty disables bot commands. Requires restart.</div>
        </div>
        <div class="config-field">
          <label>Factory webhook ${yamlHint('notifications.discord.factory_webhook')} ${settingHelpMark('notifications', 'configuration', 'Discord factory webhook', `Discord webhook the hub posts the org-wide GitHub activity feed to. Required when the GitHub activity feed is enabled.`)}</label>
          <input type="password" autocomplete="new-password" placeholder="${esc(factoryText)}" data-change-action="markDirty" data-arg0="notifications" data-arg1="discordFactoryWebhook" data-arg-types="s,s,v">
          <div class="config-help">Write-only; the current URL is never echoed. Used by the hub process.</div>
        </div>
        <div class="config-field">
          <label>Hosted-hive requests webhook ${yamlHint('notifications.discord.requests_webhook_url')} ${settingHelpMark('notifications', 'configuration', 'Discord requests webhook', `Discord webhook the hub posts new hosted-hive provision requests to (the community request channel). A hub-level requests webhook takes precedence when set.`)}</label>
          <input type="password" autocomplete="new-password" placeholder="${esc(requestsText)}" data-change-action="markDirty" data-arg0="notifications" data-arg1="discordRequestsWebhookUrl" data-arg-types="s,s,v">
          <div class="config-help">Write-only; the current URL is never echoed. Used by the hub process; may require restart.</div>
        </div>`;
    }

    function renderGovNotificationsGithubActivity(n) {
      const ga = n.githubActivity || {};
      const toggle = (key, on, label, yamlKey, tip) => `<div class="config-toggle mt-3">
          <div class="config-toggle-switch ${on ? 'on' : ''}" data-action="toggleGithubActivityFlag" data-arg0="${key}" data-arg-types="t,s" role="switch" aria-checked="${on ? 'true' : 'false'}"></div>
          <span class="config-toggle-label">${label} ${yamlHint(yamlKey)} ${settingHelpMark('notifications', 'configuration', label, tip)}</span>
        </div>`;
      const list = (key, label, yamlKey, placeholder, tip) => `<div class="config-field">
          <label>${label} ${yamlHint(yamlKey)} ${settingHelpMark('notifications', 'configuration', label, tip)}</label>
          <textarea rows="3" placeholder="${esc(placeholder)}" data-change-action="markGithubActivityList" data-arg0="${key}" data-arg-types="s,v">${esc((ga[key] || []).join('\n'))}</textarea>
        </div>`;
      return `
        <div class="config-subgroup">GitHub activity feed ${settingHelpMark('notifications', 'configuration', 'GitHub activity feed', `Default off. When enabled, the hub polls org repos, diffs issue and PR state, and posts concise lines to the Discord factory webhook. Runs in the hub process; changes apply after a hub restart.`)}</div>
        ${toggle('enabled', !!ga.enabled, 'Enable GitHub activity feed', 'notifications.github_activity.enabled', 'Requires a Discord factory webhook and an org (or project.org). Requires restart.')}
        <div class="config-field-row">
          <div class="config-field">
            <label>Org ${yamlHint('notifications.github_activity.org')} ${settingHelpMark('notifications', 'configuration', 'GitHub activity org', `GitHub org to watch. Empty falls back to project.org.`)}</label>
            <input type="text" value="${esc(ga.org || '')}" placeholder="${esc(ga.effectiveOrg || 'project.org')}" data-change-action="markGithubActivity" data-arg0="org" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Poll interval (seconds) ${yamlHint('notifications.github_activity.poll_interval_s')} ${settingHelpMark('notifications', 'configuration', 'GitHub activity poll interval', `Seconds between polls. 0 uses the built-in default.`)}</label>
            <input type="number" min="0" value="${esc(String(ga.pollIntervalS || 0))}" data-change-action="markGithubActivity" data-arg0="pollIntervalS" data-arg-types="s,N">
          </div>
        </div>
        <div class="config-field">
          <label>API URL ${yamlHint('notifications.github_activity.api_url')} ${settingHelpMark('notifications', 'configuration', 'GitHub activity API URL', `GitHub API base URL for GitHub Enterprise. Empty uses https://api.github.com.`)}</label>
          <input type="text" value="${esc(ga.apiUrl || '')}" placeholder="https://api.github.com" data-change-action="markGithubActivity" data-arg0="apiUrl" data-arg-types="s,w">
          <div class="config-help">Requires restart.</div>
        </div>
        ${list('repos', 'Repos', 'notifications.github_activity.repos[]', 'One repo per line; empty watches every org repo', 'Limit the feed to these repos. Empty watches every repo in the org.')}
        ${list('events', 'Events', 'notifications.github_activity.events[]', 'issue_opened', 'Activity events to post, for example issue_opened, issue_closed, issue_reopened. Empty posts every event.')}
        <div class="config-field-row">
          ${list('allowAuthors', 'Allow authors', 'notifications.github_activity.allow_authors[]', 'One login per line', 'When set, only activity by these authors is posted.')}
          ${list('denyAuthors', 'Deny authors', 'notifications.github_activity.deny_authors[]', 'One login per line', 'Activity by these authors is never posted.')}
        </div>
        ${toggle('filterBots', ga.filterBots !== false, 'Filter bot authors', 'notifications.github_activity.filter_bots', 'Default on: skip activity authored by bot accounts.')}
        ${toggle('filterDependabot', ga.filterDependabot !== false, 'Filter Dependabot', 'notifications.github_activity.filter_dependabot', 'Default on: skip Dependabot activity.')}`;
    }

    // ── Stats tab for per-agent stat config ──
    let _statSources = null;
    async function ensureStatSources() {
      if (_statSources) return _statSources;
      try {
        const r = await fetch('/api/config/stat-sources');
        _statSources = await r.json();
      } catch (_) {
        _statSources = { sources: {}, styles: [] };
      }
      return _statSources;
    }
    ensureStatSources();

    /* statSourcesForDialog returns the source catalogue for the agent the
       config dialog is showing. The agent config response carries a catalogue
       scoped to that agent (#7411: an ADVISORY/on-demand agent is never
       offered the CI "health" strip — those checks describe the primary
       repo's pipelines, not the agent); the page-wide unscoped catalogue is
       the fallback for older servers and the create dialog. */
    function statSourcesForDialog() {
      const scoped = _configState.data && _configState.data.statSources;
      if (scoped && scoped.sources) return scoped;
      return _statSources || { sources: {}, styles: [] };
    }

    function renderAgentStatsTab(stats) {
      const catalogue = statSourcesForDialog();
      const sources = catalogue.sources || {};
      const styles = catalogue.styles && catalogue.styles.length ? catalogue.styles : ['number', 'dot', 'pct', 'pct-bar', 'spark'];
      const sourceKeys = Object.keys(sources);

      const rows = (stats || []).map((s, i) => {
        const fieldOptions = (sources[s.source] || {}).fields || [];
        // A row whose source is no longer offered to this agent (e.g. a
        // health stat left on an advisory agent) still shows its own source,
        // so the operator can see what it is and remove it.
        const rowSourceKeys = s.source && !sourceKeys.includes(s.source) ? sourceKeys.concat([s.source]) : sourceKeys;
        return `<div class="config-stat-row" data-idx="${i}">
          <div class="config-field-row" style="margin-bottom:var(--sp-2);align-items:center">
            <div class="config-field" style="flex:0.7"><label title="Display name shown on the agent card for this metric">Label</label><input type="text" value="${esc(s.label || '')}" data-change-action="updateStat" data-arg0="${i}" data-arg1="label" data-arg-types="j,s,v"></div>
            <div class="config-field" style="flex:0.8"><label title="Data source to pull this metric from (e.g. agentMetrics, github, budget)">Source</label>
              <select data-change-action="updateStatSource" data-arg0="${i}" data-arg-types="j,v" title="Changing the source resets the field to the first available option">
                ${rowSourceKeys.map(k => `<option value="${esc(k)}" ${s.source === k ? 'selected' : ''}>${esc((sources[k] || {}).label || k)}${sources[k] ? '' : ' (not offered to this agent)'}</option>`).join('')}
              </select>
            </div>
            <div class="config-field" style="flex:0.8"><label title="Specific data field from the selected source to display">Field</label>
              <select id="stat-field-${i}" data-change-action="updateStat" data-arg0="${i}" data-arg1="field" data-arg-types="j,s,v">
                ${fieldOptions.map(f => `<option value="${f}" ${s.field === f ? 'selected' : ''}>${f}</option>`).join('')}
              </select>
            </div>
            <div class="config-field" style="flex:0.6"><label title="Visual display style: number (raw value), dot (colored indicator), pct (percentage), pct-bar (progress bar), spark (sparkline chart)">Style</label>
              <select data-change-action="updateStat" data-arg0="${i}" data-arg1="style" data-arg-types="j,s,v">
                ${styles.map(st => `<option value="${st}" ${s.style === st ? 'selected' : ''}>${st}</option>`).join('')}
              </select>
            </div>
            <div class="config-field" style="flex:0.4"><label title="Emoji or symbol shown next to this stat on the agent card">Icon</label><input type="text" value="${esc(s.icon || '')}" data-change-action="updateStat" data-arg0="${i}" data-arg1="icon" data-arg-types="j,s,v" style="width:40px"></div>
            <div class="config-field" style="flex:0.4"><label title="Target value for percentage and progress bar styles. The stat is shown as (value/target). Leave empty for raw values.">Target</label><input type="number" value="${s.target || ''}" data-change-action="updateStat" data-arg0="${i}" data-arg1="target" data-arg-types="j,s,V" style="width:50px"></div>
            <div style="display:flex;gap:var(--sp-1);align-items:flex-end;padding-bottom:var(--sp-1)">
              <button class="hv-btn btn-secondary btn-sm" data-action="moveStatUp" data-arg0="${i}" data-arg-types="j" title="Move up" ${i === 0 ? 'disabled' : ''}>▲</button>
              <button class="hv-btn btn-secondary btn-sm" data-action="moveStatDown" data-arg0="${i}" data-arg-types="j" title="Move down" ${i === stats.length - 1 ? 'disabled' : ''}>▼</button>
              <button class="hv-btn btn-danger btn-sm" data-action="removeStat" data-arg0="${i}" data-arg-types="j" title="Remove">✕</button>
            </div>
          </div>
        </div>`;
      }).join('');

      return `<div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px">
          <b>What this is.</b> Custom metrics shown on this agent's dashboard card, each pulling a <code>field</code> from a named <code>source</code> such as the agent's metrics collector or stats.json.
          <b>Why use it.</b> It surfaces the agent-specific KPIs you care about right on the card, without opening the full detail view.
          <b>Where it shows up.</b> Each configured entry renders as a metric on this agent's card in the chosen style.
          <b>How to configure.</b> Add a row and set its label, source, field, and style (plus an optional icon and target); reorder or remove rows as needed.
        </div>
        <div class="config-stats-list">${rows}</div>
        <button class="hv-btn btn-secondary btn-add btn-primary" data-action="addStat">+ Add Stat</button>
        <p class="text-muted fs-sm mt-4">Configure which metrics appear on this agent's card. Changes take effect after saving.</p>`;
    }

    function updateStat(idx, key, value) {
      const stats = _configState.data.stats || [];
      if (!stats[idx]) return;
      if (value === undefined) { delete stats[idx][key]; } else { stats[idx][key] = value; }
      markDirty('stats', 'stats', stats);
    }

    function updateStatSource(idx, newSource) {
      const stats = _configState.data.stats || [];
      if (!stats[idx]) return;
      stats[idx].source = newSource;
      const sources = statSourcesForDialog().sources || {};
      const fields = (sources[newSource] || {}).fields || [];
      stats[idx].field = fields[0] || '';
      markDirty('stats', 'stats', stats);
      renderConfigTab('Stats');
    }

    function moveStatUp(idx) {
      const stats = _configState.data.stats || [];
      if (idx <= 0) return;
      [stats[idx - 1], stats[idx]] = [stats[idx], stats[idx - 1]];
      markDirty('stats', 'stats', stats);
      renderConfigTab('Stats');
    }

    function moveStatDown(idx) {
      const stats = _configState.data.stats || [];
      if (idx >= stats.length - 1) return;
      [stats[idx], stats[idx + 1]] = [stats[idx + 1], stats[idx]];
      markDirty('stats', 'stats', stats);
      renderConfigTab('Stats');
    }

    function removeStat(idx) {
      const stats = _configState.data.stats || [];
      stats.splice(idx, 1);
      _configState.data.stats = stats;
      markDirty('stats', 'stats', stats);
      renderConfigTab('Stats');
    }

    function addStat() {
      if (!_configState.data.stats) _configState.data.stats = [];
      const sources = statSourcesForDialog().sources || {};
      const firstSource = Object.keys(sources)[0] || 'agentMetrics';
      const firstField = ((sources[firstSource] || {}).fields || [])[0] || '';
      const newKey = 'stat_' + Date.now();
      _configState.data.stats.push({ key: newKey, label: firstField || 'New Stat', source: firstSource, field: firstField, style: 'number' });
      markDirty('stats', 'stats', _configState.data.stats);
      renderConfigTab('Stats');
    }

    // Health tab (#7251). "Healthcheck Interval" and "Restart Cooldown" used to
    // head this tab; both were DEAD -- fully plumbed through config, validation
    // and persistence, but read by no runtime loop. An operator lowering the
    // interval to 60s got no change, while the cadence that actually governs
    // liveness sweeps sat further down the same tab under Agent Watchdog.
    // They are gone, and the watchdog -- the system that really runs -- now
    // leads the tab, with the escalation breaker after it.
    //
    // Model Lock moved to the Budget tab: it disables budget-pressure model
    // downgrades, which is a budget behaviour, not a health one.
    function renderGovHealth(h) {
      return `
        ${renderWatchdogSection(h.watchdog)}
        ${renderEscalationSection(_configState.data.escalation)}`;
    }

    // Watchdog section on the Health tab (RFC #4665). It lives beside the
    // escalation breaker deliberately: both answer the same operator question —
    // what happens when an agent misbehaves, and when does the system stop
    // trying? Splitting them across two tabs would split one question.
    //
    // The backoff ladder is rendered read-only: it is a derived progression,
    // and letting an operator type an arbitrary one invites a 1-second cap.
    function renderWatchdogSection(w) {
      if (!w) return '';
      const modes = [
        ['observe', 'Observe', 'Watch and report only. Publishes conditions and records what it WOULD have done, but never restarts or pauses an agent.'],
        ['heal', 'Heal', 'Full self-healing. Restarts dead agents with backoff, and pauses an agent that will not recover.'],
        ['off', 'Off', 'The watchdog does not run at all.'],
      ];
      const mode = w.mode || 'observe';
      const opts = modes.map(([v, label]) =>
        `<option value="${esc(v)}"${v === mode ? ' selected' : ''}>${esc(label)}</option>`).join('');
      const modeHelp = modes.map(([v, label, desc]) =>
        `<div style="margin-bottom:3px${v === mode ? ';color:var(--text)' : ''}"><strong>${esc(label)}</strong> — ${esc(desc)}</div>`).join('');

      // The kill switch is fleet-wide and outranks this hive's saved mode, so
      // the banner must say the stored value is not the value in force.
      const pausedBanner = w.paused ? `
        <div style="border:1px solid var(--line);border-left:3px solid var(--accent);padding:8px 10px;margin-bottom:10px;font-size:var(--fs-sm);color:var(--muted)">
          <strong class="text-default">Fleet-wide pause active.</strong>
          ${esc(w.pausedEnv || 'HIVE_WATCHDOG_PAUSE')} is set on this deployment, so healing is downgraded to observe no matter what is saved here.
        </div>` : '';

      const healBanner = mode === 'heal' && !w.paused ? `
        <div style="border:1px solid var(--line);border-left:3px solid var(--accent);padding:8px 10px;margin-bottom:10px;font-size:var(--fs-sm);color:var(--muted)">
          <strong class="text-default">Healing is active.</strong>
          This watchdog can restart agents and pause one that will not recover. Every action it takes is recorded in the Audit Log.
        </div>` : '';

      const observeBanner = mode === 'observe' && !w.paused ? `
        <div style="border:1px solid var(--line);border-left:3px solid var(--accent);padding:8px 10px;margin-bottom:10px;font-size:var(--fs-sm);color:var(--muted)">
          <strong class="text-default">Observe only — no agent is being restarted or paused.</strong>
          Conditions below and <code>watchdog-*-observed</code> entries in the Audit Log show what healing WOULD have done. Review those, then switch to Heal.
        </div>` : '';

      const ladder = (w.backoff || []).map(b => esc(b)).join(' → ') || '—';

      return `
        <h4 style="margin:18px 0 8px">Agent Watchdog</h4>
        <p style="font-size:var(--fs-sm);color:var(--muted);margin:0 0 10px">
          Observes each agent's actual terminal rather than its configured state, and can restart agents that are running but dead.
        </p>
        <p class="config-help">YAML keys: <code>governor.watchdog.enabled</code>, <code>governor.watchdog.mode</code>, <code>governor.watchdog.auth_probe</code>, <code>governor.watchdog.probe_interval_s</code>, <code>governor.watchdog.liveness.shell_prompt_after</code>, <code>governor.watchdog.liveness.stuck_overlay_after</code>, <code>governor.watchdog.readiness.no_production_for</code>, <code>governor.watchdog.restart.backoff[]</code>, <code>governor.watchdog.restart.crash_loop_after</code>, <code>governor.watchdog.restart.healthy_reset</code>.</p>
        ${pausedBanner}${healBanner}${observeBanner}
        ${renderWatchdogActivityShell()}
        <div class="config-field-row">
          <div class="config-field" data-setting-key="governor.watchdog.mode">
            <label>Mode ${settingHelpMark('agent-watchdog', 'configuration', 'Mode', `How much authority the watchdog has over your agents. Start in Observe, read the Audit Log, then move to Heal.`)}</label>
            <select data-change-action="markDirty" data-arg0="health" data-arg1="watchdogMode" data-arg-types="s,s,s">${opts}</select>
            <div id="watchdog-promotion-hint" style="font-size:0.7rem;margin-top:var(--sp-2);min-height:1em"></div>
          </div>
          <div class="config-field" data-setting-key="governor.watchdog.probe_interval_s">
            <label>Probe interval (sec) ${settingHelpMark('agent-watchdog', 'configuration', 'Probe interval (sec)', `Minimum seconds between watchdog sweeps. Each sweep captures one terminal per agent, so this is the main throttle on its cost.`)}</label>
            <input type="number" min="30" max="86400" value="${esc(String(w.probeIntervalS || 300))}" data-change-action="markDirty" data-arg0="health" data-arg1="watchdogProbeIntervalS" data-arg-types="s,s,N">
          </div>
        </div>
        <div style="font-size:0.7rem;color:var(--muted);margin:-4px 0 12px;line-height:1.5">${modeHelp}</div>
        <div class="config-field-row">
          <div class="config-field">
            <label>Give up after (failed restarts) ${settingHelpMark('agent-watchdog', 'configuration', 'Give up after (failed restarts)', `How many consecutive failed restarts before the watchdog stops trying and pauses the agent for a human. Restarting forever is never the answer.`)}</label>
            <input type="number" min="1" max="50" value="${esc(String(w.crashLoopAfter || 5))}" data-change-action="markDirty" data-arg0="health" data-arg1="watchdogCrashLoopAfter" data-arg-types="s,s,N">
          </div>
          <div class="config-field">
            <label>Healthy reset window ${settingHelpMark('agent-watchdog', 'configuration', 'Healthy reset window', `How long an agent must stay continuously healthy before its failure count clears. One good check is not enough — that would let a flapping agent launder its record.`)}</label>
            <input type="text" placeholder="30m" value="${esc(w.healthyReset || '30m')}" data-change-action="markDirty" data-arg0="health" data-arg1="watchdogHealthyReset" data-arg-types="s,s,s">
          </div>
        </div>
        <div style="font-size:0.72rem;color:var(--muted);margin-bottom:var(--sp-3)">
          Restart backoff ladder: <code style="font-family:var(--font-mono)">${ladder}</code>
          <span style="opacity:0.8">— fixed progression; the last step is the cap.</span>
        </div>`;
    }

