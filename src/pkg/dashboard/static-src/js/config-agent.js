    // ── Agent Tab Renderers ──
    function renderAgentGeneral(g) {
      const agentName = _configState.agent || '';
      const isCreate = _configState.isCreate === true;
      // Model options must come from live discovery for THIS agent's backend, not
      // the frozen KNOWN_MODELS placeholder (#3319). Resolve the agent's actual
      // backend from the live agent list; fall back to its pinned backend or an
      // empty backend (modelsForBackend then yields the safe default list).
      const _liveAgent = (window._lastAgents || []).find(a => a.name === agentName);
      const _agentBackend = (_liveAgent && _liveAgent.cli) || g.cliPinValue || '';
      const _modelOpts = modelsForBackend(_agentBackend) || [];
      // Rendered synchronously from the cache; once live discovery for an
      // inference backend answers, the Model select is rebuilt in place so
      // its default label and "not allowed by gateway" flag are current.
      setTimeout(() => ensureModelOptionsForBackend(_agentBackend), 0);
      const _bobSessionLabel = g.bobSessionLabel || '';
      const _bobSessionPrefix = g.bobSessionPrefix || '';
      const _bobEffectiveLabel = _bobSessionLabel
        ? _bobSessionLabel
        : (_bobSessionPrefix ? _bobSessionPrefix + agentName : '(none — Bob shows the workdir name)');
      let html = '';
      if (isCreate) {
        html += `<div class="config-field">
          <label>Name <span class="text-danger">*</span> ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Name', `The agent's internal name used as its ID, config file name, and tmux session. Lowercase, hyphens allowed.`)}</label>
          <input type="text" id="cfg-create-name" value="" placeholder="e.g. docs-writer, sec-scanner" data-change-action="markDirty" data-arg0="_create" data-arg1="name" data-arg-types="s,s,v">
          <div style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-1)">Lowercase, hyphens allowed. Used as the agent ID.</div>
        </div>`;
      } else {
        html += `<div class="config-toggle">
          <div class="config-toggle-switch ${g.enabled ? 'on' : ''}" id="cfg-agent-enabled" data-action="toggleConfigSwitch" data-section="general" data-key="enabled"></div>
          <span class="config-toggle-label">Enabled ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Enabled', `Disabled agents remain configured but do not run. Enable this agent and Save to start it.`)}</span>
        </div>`;
      }
      html += `
        <div class="config-field">
          <label>Display Name ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Display Name', `The friendly name shown on the agent card and sidebar. Does not affect the agent's internal ID or tmux session name.`)}</label>
          <input type="text" id="cfg-agent-name" value="${esc(g.displayName || (isCreate ? '' : agentName))}" placeholder="e.g. Scanner, Code Reviewer" data-change-action="gh32">
        </div>
        <div class="config-field">
          <label>Description ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Description', `A short description of what this agent is responsible for. Shown as hover text on the sidebar agent list. Helps operators and collaborators understand each agent's role at a glance.`)}</label>
          <textarea id="cfg-agent-desc" rows="2" style="width:100%;resize:vertical;font-size:var(--fs-sm)" placeholder="e.g. Triages issues and dispatches fix agents" data-change-action="markDirty" data-arg0="general" data-arg1="description" data-arg-types="s,s,v">${esc(g.description || (isCreate ? '' : AGENT_DESCRIPTIONS[agentName]) || '')}</textarea>
        </div>
        <div class="config-field-row">
          <div class="config-field">
            <label>Emoji ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Emoji', `Emoji shown next to the agent name in the sidebar and agent cards.`)}</label>
            <input type="text" id="cfg-agent-emoji" value="${esc(g.emoji || '')}" placeholder="🤖" data-change-action="markDirty" data-arg0="general" data-arg1="emoji" data-arg-types="s,s,v">
          </div>
          <div class="config-field">
            <label>Color ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Color', `The agent's identity color, used to represent it in token charts and sparklines. Does not affect card or status styling.`)}</label>
            <input type="color" id="cfg-agent-color" class="config-color-input" value="${esc(g.color || '#3498db')}" data-change-action="markDirty" data-arg0="general" data-arg1="color" data-arg-types="s,s,v">
            <div style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-1)">Identity color for this agent's token charts and sparklines.</div>
          </div>
        </div>
        <div class="config-field-row">
          <div class="config-field">
            <label>Sort Order ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Sort Order', `Controls position in sidebar and agent grid. Lower numbers appear first.`)}</label>
            <input type="number" id="cfg-agent-sort" value="${g.sortOrder || 50}" data-change-action="gh33">
          </div>
          <div class="config-field">
            <label>Replicas ${settingHelpMark('agent-configuration', 'replicated-agents', 'Replicas', `Run multiple isolated copies of this agent. Replica 1 keeps this name; extra replicas appear as name-2, name-3, etc. Cap ${MAX_AGENT_REPLICAS}.`)}</label>
            <input type="number" id="cfg-agent-replicas" min="1" max="${MAX_AGENT_REPLICAS}" value="${g.replicas || 1}" data-change-action="gh34">
          </div>
          <div class="config-field">
            <label>Bead Role ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Bead Role', `Worker agents do tasks. Supervisor monitors other agents.`)}</label>
            <select id="cfg-agent-bead-role" data-change-action="markDirty" data-arg0="general" data-arg1="beadRole" data-arg-types="s,s,v">
              <option value="worker" ${(g.beadRole || 'worker') === 'worker' ? 'selected' : ''}>worker</option>
              <option value="supervisor" ${g.beadRole === 'supervisor' ? 'selected' : ''}>supervisor</option>
            </select>
          </div>
        </div>
        <div class="config-field-row">
          <div class="config-field">
            <label>Role ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Role', `Semantic role for behavioral dispatch (e.g. scanner, reviewer, architect). Used by lane routing and kick templates.`)}</label>
            <input type="text" id="cfg-agent-role" value="${esc(g.role || '')}" placeholder="e.g. scanner, reviewer" data-change-action="markDirty" data-arg0="general" data-arg1="role" data-arg-types="s,s,v">
          </div>
          <div class="config-field">
            <label>Kick Template ${settingHelpMark('agent-configuration', 'kick-templates-what-an-agent-is-told-to-do', 'Kick Template', `Prompt template file used when the governor kicks this agent. Located in the prompts directory.`)}</label>
            <input type="text" id="cfg-agent-kick-tpl" value="${esc(g.kickTemplate || '')}" placeholder="e.g. scanner-CLAUDE.md" data-change-action="markDirty" data-arg0="general" data-arg1="kickTemplate" data-arg-types="s,s,v">
          </div>
        </div>
        <div class="config-field">
          <label>Prompt from GitHub repo ${settingHelpMark('agent-configuration', 'live-linked-agent-definitions-definition_source', 'Prompt from GitHub repo', `Optionally source this agent's kick prompt from a file in a GitHub repo instead of an inline template. The prompt is fetched live at kick time using the hive's GitHub App, and falls back to the inline/baked template if the repo is unreachable. The repo must be on the operator's seed-only allowlist.`)}</label>
          <div style="font-size:0.65rem;color:var(--muted);margin:var(--sp-1) var(--sp-0) var(--sp-3)">Leave blank to use the inline Kick Template above. All three of owner, repo, and path are required to enable.</div>
          <div class="config-field-row">
            <div class="config-field">
              <label style="font-size:0.7rem">Owner</label>
              <input type="text" id="cfg-agent-ps-owner" value="${esc((g.promptSource && g.promptSource.owner) || '')}" placeholder="e.g. kubestellar" data-input-action="markPromptSourceDirty">
            </div>
            <div class="config-field">
              <label style="font-size:0.7rem">Repo</label>
              <input type="text" id="cfg-agent-ps-repo" value="${esc((g.promptSource && g.promptSource.repo) || '')}" placeholder="e.g. hive" data-input-action="markPromptSourceDirty">
            </div>
          </div>
          <div class="config-field-row">
            <div class="config-field">
              <label style="font-size:0.7rem">File path</label>
              <input type="text" id="cfg-agent-ps-path" value="${esc((g.promptSource && g.promptSource.path) || '')}" placeholder="e.g. prompts/scanner.md" data-input-action="markPromptSourceDirty">
            </div>
            <div class="config-field">
              <label style="font-size:0.7rem">Ref <span style="font-weight:400;color:var(--muted)">(optional)</span></label>
              <input type="text" id="cfg-agent-ps-ref" value="${esc((g.promptSource && g.promptSource.ref) || '')}" placeholder="branch, tag, or SHA (default branch if blank)" data-input-action="markPromptSourceDirty">
            </div>
          </div>
        </div>
        <div class="config-field">
          <label>Operating Mode ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Operating Mode', `Controls what GitHub actions this agent can take. ADVISORY: beads only. ISSUES_ONLY: open issues, no PRs. ISSUES_AND_PRS: issues + PRs. ISSUES_PRS_MERGE: full auto-merge. Leave blank to use the ACMM level default.`)}</label>
          ${(() => {
            const allModes = ['ADVISORY','ISSUES_ONLY','ISSUES_AND_PRS','ISSUES_PRS_MERGE'];
            const statusAgent = ((window._lastStatus || {}).agents || []).find(sa => sa.name === agentName);
            const dflt = statusAgent ? statusAgent.defaultMode : '';
            const current = g.mode || '';
            return '<select id="cfg-agent-mode" data-change-action="markDirty" data-arg0="general" data-arg1="mode" data-arg-types="s,s,v">'
              + '<option value=""' + (!current ? ' selected' : '') + '>— use level default' + (dflt ? ' (' + modeEmoji(dflt) + ' ' + dflt + ')' : '') + '</option>'
              + allModes.map(m => '<option value="' + m + '"' + (current === m ? ' selected' : '') + '>' + modeEmoji(m) + ' ' + m + (m === dflt ? ' (default)' : '') + '</option>').join('')
              + '</select>';
          })()}
        </div>
        <div class="config-field">
          <label>Launch Command ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Launch Command', `The shell command used to start this agent's CLI process. Typically uses agent-launch.sh with a --backend flag. Changing the CLI Pin Value will auto-update the binary path here.`)}</label>
          <input type="text" id="cfg-launch-cmd" value="${esc(g.launchCmd || '')}" data-change-action="markDirty" data-arg0="general" data-arg1="launchCmd" data-arg-types="s,s,v">
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${g.sandboxEnabled ? 'on' : ''}" id="cfg-agent-sandbox" data-action="toggleConfigSwitch" data-section="general" data-key="sandboxEnabled"></div>
          <span class="config-toggle-label">Run in sandbox ${settingHelpMark('sandbox-isolation', '', 'Run in sandbox', `Opt this agent into the credential-free sandbox runner. Requires the hive-wide sandbox gate in Governor → Security to be enabled.`)}${g.sandboxEffective ? ' ' + statusBadgeHTML('ready', 'sandboxed', '', 'done') : ''}</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${g.clearOnKick !== false ? 'on' : ''}" id="cfg-clear-on-kick" data-action="toggleConfigSwitch" data-section="general" data-key="clearOnKick"></div>
          <span class="config-toggle-label">Clear on Kick ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Clear on Kick', `Send /clear before each governor kick to reset conversation context. Prevents stale context buildup across runs. On by default for all agents.`)}</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${g.onDemand === true ? 'on' : ''}" id="cfg-on-demand" data-action="toggleConfigSwitch" data-section="general" data-key="onDemand"></div>
          <span class="config-toggle-label">On Demand ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'On Demand', `When on, this agent is never started at boot and never kicked by the governor or by event (CEL) triggers — it only runs when something summons it explicitly. Turn it OFF to make the agent run on its cadence like a normal agent. Note: an agent also needs a cadence entry in the ACTIVE governor mode before it will actually be kicked. On-demand agents are also excluded from review fan-out, so a reviewer left on-demand cannot be dispatched.`)}${g.onDemand === true ? ' ' + statusBadgeHTML('on_demand', 'on demand', '', '') : ''}</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${g.includeRepos !== false ? 'on' : ''}" id="cfg-include-repos" data-action="toggleConfigSwitch" data-section="general" data-key="includeRepos"></div>
          <span class="config-toggle-label">Include Repos in Kicks ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Include Repos in Kicks', `Append monitored repo information to the kick dialog. Disable for agents that don't need repo context (e.g. outreach).`)}</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${g.cliPinned ? 'on' : ''}" id="cfg-cli-pinned" data-action="toggleConfigSwitch" data-section="general" data-key="cliPinned"></div>
          <span class="config-toggle-label">CLI Pinned ${settingHelpMark('cli-pins', '', 'CLI Pinned', `When enabled, the agent is locked to the CLI backend specified in CLI Pin Value. The governor will not switch this agent to a different backend. Useful for agents that require a specific tool (e.g. copilot for free-tier work).`)}</span>
        </div>
        <div class="config-field">
          <label>CLI Pin Value ${settingHelpMark('cli-pins', '', 'CLI Pin Value', `Which CLI backend to pin this agent to when CLI Pinned is on. Options: claude, copilot, bob, gemini, codex, goose, aider. copilot and goose are free backends (no token cost).`)}</label>
          <select id="cfg-pin-value" data-prev="${esc(g.cliPinValue || '')}" data-change-action="onCliPinChange" data-arg-types="t">
            ${backendOptionList().map(b => `<option value="${b}" ${g.cliPinValue === b ? 'selected' : ''}>${backendLabel(b)}</option>`).join('')}
          </select>
        </div>
        <div class="config-field" id="cfg-bob-session-label-wrap" ${_agentBackend === 'bob' ? '' : 'hidden'}>
          <label>Bob session label ${settingHelpMark('backend-support-tiers', 'tagging-bob-sessions-for-bobalytics', 'Bob session label', `What Bobalytics shows for this bob agent. This does not rename the Hive agent or change its tmux session, workdir, HOME, beads, claims or token buckets.`)}</label>
          <input type="text" id="cfg-bob-session-label" value="${esc(_bobSessionLabel)}" placeholder="${esc(_bobSessionPrefix ? _bobSessionPrefix + agentName : 'optional override')}" data-change-action="onBobSessionLabelChange" data-arg-types="t">
          <div class="text-muted">Bobalytics label only; the Hive agent identity remains <code>${esc(agentName)}</code>. Leave blank to use the global prefix, if any.</div>
          <div class="text-muted">Effective label: <code id="cfg-bob-session-effective">${esc(_bobEffectiveLabel)}</code></div>
        </div>
        <div class="config-field-row">
          <div class="config-field">
            <label>Stale Timeout ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Stale Timeout', `How long an agent can be idle before the governor considers it stale and triggers a restart. Use m (minutes) or h (hours), e.g. 20m, 2.5h. Default is 20m.`)}</label>
            <input type="text" id="cfg-stale-timeout" value="${cadenceToHuman(g.staleTimeout || 1200)}" data-change-action="gh35">
          </div>
          <div class="config-field">
            <label>Restart Strategy ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Restart Strategy', `immediate: restart right away when stale or crashed. backoff: wait progressively longer between restart attempts (useful for rate-limited agents). none: do not auto-restart — operator must manually intervene.`)}</label>
            <select id="cfg-restart-strategy" data-change-action="markDirty" data-arg0="general" data-arg1="restartStrategy" data-arg-types="s,s,v">
              ${RESTART_STRATEGIES.map(s => `<option value="${s}" ${g.restartStrategy === s ? 'selected' : ''}>${s}</option>`).join('')}
            </select>
          </div>
        </div>
        <div class="config-field">
          <label>Model ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Model', `Per-agent model override. Precedence: this model if set → otherwise the backend default (a gateway's default_model from Settings → Model Gateways, or the CLI's own default). Pick "(backend default — …)" to clear the per-agent model. A model marked "not allowed by gateway" fails every call with a 403. The governor may downgrade models automatically when budget is tight, unless Model Lock is enabled in Governor Health settings.`)}</label>
          <select id="cfg-model" title="${esc(MODEL_PRECEDENCE_TITLE)}" data-change-action="markDirty" data-arg0="general" data-arg1="model" data-arg-types="s,s,v">
            ${modelSelectOptionsHtml(_agentBackend, g.model || '', _modelOpts)}
          </select>
        </div>
        <div class="config-field">
          <label>Caveman Mode ${settingHelpMark('agent-configuration', 'caveman-prompt-compression', 'Caveman Mode', `Enable Caveman output compression to reduce token usage by ~65%. Modes: lite (removes filler), full (caveman-speak), ultra (telegraphic), wenyan (classical Chinese). Leave empty to disable.`)}</label>
          <select id="cfg-caveman-mode" data-change-action="markDirty" data-arg0="general" data-arg1="cavemanMode" data-arg-types="s,s,v">
            ${CAVEMAN_MODES.map(m => `<option value="${m}" ${(g.cavemanMode || '') === m ? 'selected' : ''}>${m || '(disabled)'}</option>`).join('')}
          </select>
        </div>
        <div class="config-field">
          <label>Jev Mode ${settingHelpMark('agent-configuration', 'jev-typed-decisions-jev_mode', 'Jev Mode', `Give this agent the jev_decide tool: quick yes/no, pick-one and rubric-score judgments answered by Jev, TypeSafe AI's cheap confidence-scored typed-decision model, instead of frontier-model reasoning. Calls go through the hive (the agent never sees the key), count against the token budget and are written to the audit log. Requires JEV_API_KEY or a connected OpenRouter gateway, and per-agent UID isolation (the endpoint identifies callers by socket UID only). Saving a change restarts the agent so the skill and env apply. Leave disabled to install nothing and make no Jev calls.`)}</label>
          <select id="cfg-jev-mode" data-change-action="markDirty" data-arg0="general" data-arg1="jevMode" data-arg-types="s,s,v" ${g.jevReady ? '' : 'disabled'}>
            ${JEV_MODES.map(m => `<option value="${m.value}" ${(g.jevMode || '') === m.value ? 'selected' : ''}>${m.label}</option>`).join('')}
          </select>
          ${g.jevReady ? '' : `<div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-2)">⚠ No Jev key: set JEV_API_KEY or connect OpenRouter in Settings → Governor → Model Gateways to enable this toggle.</div>`}
        </div>
        <div class="config-field">
          <label>Explain Mode ${settingHelpMark('agent-configuration', 'explain-mode-debugging-agent-behaviour', 'Explain Mode', `Ask this agent to say WHY it made each tool call, for debugging. Reasoning is emitted on EXPLAIN: lines alongside the work — the agent still acts, it does not narrate instead. Costs extra tokens on every kick, so leave it off unless you are debugging. View the reasoning on its own with "?explain=only" on the agent's full log. "(hive default)" follows the hive-wide Default explain mode in Settings &rarr; Governor &rarr; General (or the HIVE_EXPLAIN_MODE environment variable when that is unset); "off" opts this agent out even when the hive default is on.`)}</label>
          <select id="cfg-explain-mode" data-change-action="markDirty" data-arg0="general" data-arg1="explainMode" data-arg-types="s,s,v">
            ${EXPLAIN_MODES.map(m => `<option value="${m.value}" ${(g.explainMode || '') === m.value ? 'selected' : ''}>${m.label}</option>`).join('')}
          </select>
        </div>
        <div class="config-field">
          <label>Repos ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Repos', `Which repositories this agent serves. Leave EMPTY for the whole hive — that is the default and what every agent did before this setting existed. Naming repos here makes the agent a specialist: it is only handed work on these repos, its kick lists only these repos, and writes to any other repo are refused by the proxy and by the hive-open-pr/hive-merge/hive-open-issue relays. Use it for an agent that is only relevant to one repo — a schema-migration reviewer, an image-build auditor — so it stops costing inference and filing noise on the repos that never wanted it. Comma-separated; a bare name ("console") or an owner/name reference ("laredo/cuga-agent").`)}</label>
          <input type="text" id="cfg-agent-repos" value="${esc((g.repos || []).join(', '))}" placeholder="(empty = every repo in the hive)" data-change-action="gh39repos">
          <div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-2)">${(g.repos && g.repos.length) ? `🎯 scoped to ${g.repos.length} repo(s)` : 'hive-wide — serves every repo'}${(g.projectRepos && g.projectRepos.length) ? ` · this hive watches: ${esc(g.projectRepos.join(', '))}` : ''}</div>
        </div>
        <div class="config-field">
          <label>Lane Keywords ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Lane Keywords', `Comma-separated keywords for issue classification routing. Issues matching these keywords are routed to this agent.`)}</label>
          <input type="text" id="cfg-agent-lane-kw" value="${esc((g.laneKeywords || []).join(', '))}" placeholder="e.g. bug, triage, security" data-change-action="gh36">
        </div>
        <div class="config-field">
          <label>Detect Keywords ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Detect Keywords', `Comma-separated keywords for token session detection. Used to identify which agent produced a given session.`)}</label>
          <input type="text" id="cfg-agent-detect-kw" value="${esc((g.detectKeywords || []).join(', '))}" placeholder="e.g. scanner, triage, issue" data-change-action="gh37">
        </div>
        <div class="config-field">
          <label>Aliases ${settingHelpMark('agent-configuration', 'anatomy-of-an-agent', 'Aliases', `Comma-separated short aliases for Discord commands.`)}</label>
          <input type="text" id="cfg-agent-aliases" value="${esc((g.aliases || []).join(', '))}" placeholder="e.g. sc, scan" data-change-action="gh38">
        </div>
        ${isCreate ? '' : `<div style="margin-top:var(--sp-8);padding-top:var(--sp-6);border-top:1px solid var(--red-border)">
          <label style="color:#b91c1c;font-weight:600;font-size:var(--fs-sm);margin-bottom:var(--sp-4);display:block">Danger Zone</label>
          <button class="hv-btn btn-danger btn-sm" data-action="deleteAgentFromConfig" data-agent="${esc(agentName)}" title="Permanently remove this agent from the hive. Deletes its config file, env file, and sidebar entry. Cannot be undone.">Delete Agent</button>
          <span style="color:var(--muted);font-size:0.65rem;margin-left:var(--sp-4)">Removes agent config, env file, and sidebar entry</span>
        </div>`}`;
      return html;
    }

    function updateConfigAgentName(name) {
      // Do NOT overwrite _configState.agent — it holds the agent's config key
      // used in the API URL. Changing it to the display name causes 404 on save.
      const titleEl = document.querySelector('.config-title');
      if (titleEl) titleEl.textContent = `${name || 'New Agent'} Configuration`;
    }

    const SECONDS_PER_MIN = 60;
    const SECONDS_PER_HOUR = 3600;
    const SECONDS_PER_DAY = 86400;
    function cadenceToHuman(sec) {
      const s = sec || 0;
      if (s === 0) return '0';
      if (s >= SECONDS_PER_DAY && s % SECONDS_PER_DAY === 0) return `${s / SECONDS_PER_DAY}d`;
      if (s >= SECONDS_PER_HOUR && s % SECONDS_PER_HOUR === 0) return `${s / SECONDS_PER_HOUR}h`;
      if (s >= SECONDS_PER_MIN && s % SECONDS_PER_MIN === 0) return `${s / SECONDS_PER_MIN}m`;
      return `${s}s`;
    }
    function cadenceFromHuman(val) {
      const str = String(val).trim().toLowerCase();
      if (!str || str === '0') return 0;
      const match = str.match(/^(\d+(?:\.\d+)?)\s*(d|h|m|s)?$/);
      if (!match) return 0;
      const num = parseFloat(match[1]);
      const unit = match[2] || 'm';
      if (unit === 'd') return Math.round(num * SECONDS_PER_DAY);
      if (unit === 'h') return Math.round(num * SECONDS_PER_HOUR);
      if (unit === 's') return Math.round(num);
      return Math.round(num * SECONDS_PER_MIN);
    }

    function browserTimeZone() {
      return browserDashboardTimeZone() || 'UTC';
    }
    function cadenceModeValue(v) {
      const text = String(v).trim ? String(v).trim().toLowerCase() : '';
      if (text === 'continuous') return 'continuous';
      if (text === 'off' || text === 'pause' || text === 'paused') return 'off';
      if (!v) return 'off';
      return 'interval';
    }
    function cadenceDetailKind(v) {
      if (v && typeof v === 'object') return v.cron ? 'cron' : 'times';
      return 'interval';
    }
    function cadenceSummary(v) {
      if (String(v).trim && String(v).trim().toLowerCase() === 'continuous') return '∞ continuous';
      if (!v) return 'Off';
      if (!v || typeof v !== 'object') return `Every ${cadenceToHuman(v || 0)}`;
      const tz = v.tz || browserTimeZone();
      if (v.cron) return `Cron "${v.cron}" (${tz})`;
      const times = (v.times || []).join(' and ');
      const days = (v.days || []).join(', ') || 'every day';
      return `At ${times || '—'}, ${days} (${tz})`;
    }
    function readCadenceBuilder(mode) {
      const kind = document.getElementById(`cad-${mode}-kind`)?.value || 'off';
      if (kind === 'continuous') return 'continuous';
      if (kind === 'off') return 0;
      const detail = document.getElementById(`cad-${mode}-detail-kind`)?.value || 'interval';
      if (detail === 'interval') return cadenceFromHuman(document.getElementById(`cad-${mode}-interval`)?.value || '30m');
      if (detail === 'cron') return {
        cron: (document.getElementById(`cad-${mode}-cron`)?.value || '30 9 * * 1-5').trim(),
        tz: browserTimeZone()
      };
      const days = Array.from(document.querySelectorAll(`[data-cadence-day="${mode}"]:checked`)).map(el => el.value);
      return {
        times: (document.getElementById(`cad-${mode}-times`)?.value || '09:00').split(',').map(s => s.trim()).filter(Boolean),
        days,
        tz: browserTimeZone()
      };
    }
    function cadenceChanged(mode) {
      const value = readCadenceBuilder(mode);
      if (!_configState.data.cadences) _configState.data.cadences = {};
      _configState.data.cadences[mode] = value;
      markDirty('cadences', mode, value);
      const summary = document.getElementById(`cad-${mode}-summary`);
      if (summary) summary.textContent = cadenceSummary(value);
    }
    function cadenceSetKind(arg0, arg1, arg2) {
      const mode = typeof arg0 === 'string' ? arg0 : arg1;
      const kind = typeof arg0 === 'string' ? arg1 : arg2;
      if (!_configState.data.cadences) _configState.data.cadences = {};
      const legacyContinuous = ((_configState.data && _configState.data.general) || {}).continuous === true;
      if (legacyContinuous && mode !== 'quiet' && (kind === 'interval' || kind === 'off')) {
        if (!_configState.data.general) _configState.data.general = {};
        for (const otherMode of ['surge', 'busy', 'idle']) {
          if (otherMode === mode) continue;
          const otherCurrent = Object.prototype.hasOwnProperty.call(_configState.data.cadences, otherMode)
            ? _configState.data.cadences[otherMode]
            : (otherMode === 'idle' ? undefined : _configState.data.cadences.idle);
          const otherKind = cadenceModeValue(otherCurrent);
          if (otherKind !== 'continuous' && otherKind !== 'off') {
            _configState.data.cadences[otherMode] = 'continuous';
            markDirty('cadences', otherMode, 'continuous');
          }
        }
        _configState.data.general.continuous = false;
        markDirty('general', 'continuous', false);
      }
      const current = _configState.data.cadences[mode];
      const value = kind === 'continuous' ? 'continuous' : kind === 'interval' ? (cadenceModeValue(current) === 'interval' && typeof current !== 'object' ? current : SECONDS_PER_HOUR) : 0;
      _configState.data.cadences[mode] = value;
      markDirty('cadences', mode, value);
      renderConfigTab(_configState.tab);
    }
    function cadenceSetPreset(mode, preset) {
      if (!_configState.data.cadences) _configState.data.cadences = {};
      const value = preset === 'hours'
        ? 4 * SECONDS_PER_HOUR
        : { times: ['09:00'], days: preset === 'daily' ? [] : ['mon','tue','wed','thu','fri'], tz: browserTimeZone() };
      _configState.data.cadences[mode] = value;
      markDirty('cadences', mode, value);
      renderConfigTab(_configState.tab);
    }
    async function applyCadenceTemplate(templateKey) {
      const templateDescriptions = {
        'testing': 'New Level testing (surge: pause, busy: 2h, quiet: 4h, idle: 8h)',
        'weekly': 'Weekly sprint (surge: 1h, busy: 2h, quiet: 4h, idle: 8h)',
        'biweekly': 'Biweekly sprint (surge: 2h, busy: 4h, quiet: 8h, idle: 16h)',
        'daily': 'Daily cadence (surge: 30m, busy: 1h, quiet: 2h, idle: 4h)'
      };
      const desc = templateDescriptions[templateKey] || templateKey;
      if (!(await hiveConfirm(`Apply global cadence template "${desc}" to all configured agents across all governor modes? This replaces existing interval, schedule and on-demand cadences; continuous cadences are kept.`))) return;
      try {
        const res = await fetch('/api/config/governor/cadence-templates', {
          method: 'POST',
          headers: Object.assign({ 'Content-Type': 'application/json' }, _inceptionAuthHeaders()),
          body: JSON.stringify({ template: templateKey })
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          showToast(data.error || 'Failed to apply cadence template', 'error');
          return;
        }
        noteStatusMutation(data.minStatusSeq);
        showToast(`Applied ${desc} to ${data.agentsUpdated || 0} agents`, 'success');
        if (typeof refreshStatus === 'function') refreshStatus();
      } catch (err) {
        showToast('Network error applying cadence template: ' + err.message, 'error');
      }
    }
    function switchToPerModeCadences() {
      if (!_configState.data.general) _configState.data.general = {};
      if (!_configState.data.cadences) _configState.data.cadences = {};
      _configState.data.general.continuous = false;
      for (const mode of ['surge', 'busy', 'idle']) {
        if (!Object.prototype.hasOwnProperty.call(_configState.data.cadences, mode)) {
          _configState.data.cadences[mode] = 'continuous';
          markDirty('cadences', mode, 'continuous');
          continue;
        }
        const currentKind = cadenceModeValue(_configState.data.cadences[mode]);
        if (currentKind !== 'continuous' && currentKind !== 'off') {
          _configState.data.cadences[mode] = 'continuous';
          markDirty('cadences', mode, 'continuous');
        }
      }
      markDirty('general', 'continuous', false);
      renderConfigTab(_configState.tab);
    }
    function cadenceKindKey(event, mode, kind) {
      if (event.key === 'Enter' || event.key === ' ') { cadenceSetKind(mode, kind); return; }
      const order = ['off', 'interval', 'continuous'];
      const idx = order.indexOf(kind);
      if (idx < 0) return;
      const next = (event.key === 'ArrowLeft' || event.key === 'ArrowUp')
        ? order[(idx + order.length - 1) % order.length]
        : order[(idx + 1) % order.length];
      cadenceSetKind(mode, next);
    }
    function toggleContinuousAllActiveModes(el) {
      if (!el || el.classList.contains('disabled') || el.dataset.disabled === 'true') return;
      const isOn = el.classList.toggle('on');
      el.setAttribute('aria-checked', isOn ? 'true' : 'false');
      if (!_configState.data.general) _configState.data.general = {};
      _configState.data.general.continuous = isOn;
      markDirty('general', 'continuous', isOn);
      renderConfigTab(_configState.tab);
    }
    function cadenceKindButton(mode, current, kind, label) {
      const checked = current === kind;
      return `<button class="hv-btn btn-secondary btn-sm" type="button" role="radio" aria-checked="${checked ? 'true' : 'false'}" tabindex="${checked ? '0' : '-1'}" data-action="cadenceSetKind" data-keydown-action="cadenceKindKey" data-keys="Enter, ,ArrowLeft,ArrowRight,ArrowUp,ArrowDown" data-prevent="1" data-arg1="${mode}" data-arg2="${kind}" data-arg-types="e,s,s">${label}</button>`;
    }
    function renderCadenceBuilder(mode, value, legacyContinuous) {
      const storedKind = cadenceModeValue(value);
      const legacyActive = legacyContinuous && mode !== 'quiet' && storedKind !== 'continuous';
      const kind = legacyActive ? 'continuous' : storedKind;
      const detailKind = kind === 'interval' ? cadenceDetailKind(value) : 'interval';
      const interval = detailKind === 'interval' && typeof value !== 'object' ? cadenceToHuman(value || 0) : '30m';
      const times = detailKind === 'times' ? (value.times || []).join(',') : '09:00';
      const cron = detailKind === 'cron' ? (value.cron || '') : '30 9 * * 1-5';
      const selectedDays = new Set(detailKind === 'times' ? (value.days || []) : []);
      const days = ['mon','tue','wed','thu','fri','sat','sun'].map(d => `<label class="cadence-day-label"><input type="checkbox" data-cadence-day="${mode}" value="${d}" ${selectedDays.size===0 || selectedDays.has(d) ? 'checked' : ''} data-change-action="cadenceChanged" data-arg0="${mode}">${d}</label>`).join('');
      const storedNote = legacyActive ? 'Legacy all-modes continuous is on; choosing Off or Interval here converts it to per-mode settings.' : cadenceSummary(value);
      const body = kind === 'interval'
        ? `<div class="cadence-mode-presets">
            <button class="hv-btn btn-secondary btn-sm" type="button" data-action="cadenceSetPreset" data-arg0="${mode}" data-arg1="daily">Daily at…</button>
            <button class="hv-btn btn-secondary btn-sm" type="button" data-action="cadenceSetPreset" data-arg0="${mode}" data-arg1="weekdays">Weekdays at…</button>
            <button class="hv-btn btn-secondary btn-sm" type="button" data-action="cadenceSetPreset" data-arg0="${mode}" data-arg1="hours">Every N hours</button>
          </div>
          <div class="cadence-mode-inline">
            <select id="cad-${mode}-detail-kind" data-change-action="gh39" data-arg0="${mode}">
              <option value="interval" ${detailKind==='interval'?'selected':''}>Interval</option>
              <option value="times" ${detailKind==='times'?'selected':''}>Times</option>
              <option value="cron" ${detailKind==='cron'?'selected':''}>Custom cron</option>
            </select>
            ${detailKind === 'interval' ? `<input id="cad-${mode}-interval" type="text" value="${esc(interval)}" placeholder="30m, 4h" data-change-action="cadenceChanged" data-arg0="${mode}">` : ''}
            ${detailKind === 'times' ? `<input id="cad-${mode}-times" type="text" value="${esc(times)}" placeholder="09:00,17:00" data-change-action="cadenceChanged" data-arg0="${mode}">` : ''}
            ${detailKind === 'cron' ? `<input id="cad-${mode}-cron" type="text" value="${esc(cron)}" placeholder="30 9 * * 1-5" data-change-action="cadenceChanged" data-arg0="${mode}">` : ''}
            <span class="cadence-mode-help">Interval and time-of-day modes are mutually exclusive.</span>
          </div>
          ${detailKind === 'times' ? `<div>${days}</div>` : ''}
          ${detailKind === 'cron' ? `<div class="cadence-mode-help">5-field cron. Timezone saved from this browser: ${esc(browserTimeZone())}</div>` : ''}`
        : kind === 'continuous'
        ? '<div class="cadence-mode-help">Re-kicks after Hive observes this agent’s session end plus the configured cool-down.</div>'
        : '<div class="cadence-mode-help">No governor kicks in this mode.</div>';
      return `<div class="cadence-mode-row">
        <div class="cadence-mode-label">${mode}</div>
        <div class="cadence-mode-control" role="radiogroup" aria-label="${esc(mode)} cadence mode">
          ${cadenceKindButton(mode, kind, 'off', 'Off')}
          ${cadenceKindButton(mode, kind, 'interval', '⏱ Interval')}
          ${cadenceKindButton(mode, kind, 'continuous', '∞ Continuous')}
        </div>
        <div>
          <input type="hidden" id="cad-${mode}-kind" value="${esc(kind)}">
          ${body}
          <div id="cad-${mode}-summary" class="cadence-mode-help">${esc(storedNote)}</div>
        </div>
      </div>`;
    }

    function renderAgentCadences(c) {
      // Master power sits at the TOP of this tab on purpose. Cadences are the
      // per-mode schedule; `enabled` is whether the agent is scheduled at all.
      // An operator who disabled an agent and later came here to work out why
      // it never kicks had no way to see that from the cadence grid — every
      // mode showed a healthy interval. Same switch as the card, but it commits
      // through the dialog's dirty bag (Save), which is why it carries
      // toggleConfigSwitch rather than the card's immediate action.
      const _agentName = _configState.agent || '';
      const _live = (window._lastAgents || []).find(a => a.name === _agentName);
      const _general = (_configState.data && _configState.data.general) || {};
      const _dirtyGeneral = (_configState.dirty && _configState.dirty.general) || {};
      const _enabled = Object.prototype.hasOwnProperty.call(_dirtyGeneral, 'enabled')
        ? _dirtyGeneral.enabled === true
        : _general.enabled !== false;
      const _continuous = Object.prototype.hasOwnProperty.call(_dirtyGeneral, 'continuous')
        ? _dirtyGeneral.continuous === true
        : _general.continuous === true;
      const _continuousCooldown = Number(_general.continuousCooldown || 60);
      const _continuousBudgetPct = Number(_general.continuousBudgetPct || 80);
      const _continuousBudgetNote = _live && _live.continuousBlocked === 'budget'
        ? '<span class="text-muted">Continuous is currently held by the token/spend budget guard; normal cadence resumes until spend falls under the threshold or the window resets.</span>'
        : '';
      return `
        <div style="display:flex;align-items:center;gap:var(--sp-5);margin-bottom:14px;padding:10px 12px;border:1px solid var(--border);border-radius:var(--r)">
          ${pwrSwitchHtml(_enabled, ' data-action="toggleConfigSwitch" data-section="general" data-key="enabled"', 'Agent enabled', 'Master power for this agent. 0 removes it from governor scheduling in every mode, whatever the cadences below say. Takes effect on Save.', 'agent-power-switch')}
          <div style="font-size:0.78rem;line-height:1.5">
            <b>Agent power.</b> <code>1</code> = enabled, <code>0</code> = disabled.
            <span class="text-muted">A disabled agent is never kicked in any mode, no matter what the cadences below say. This is separate from pausing, which is a temporary operator hold.</span>
          </div>
        </div>
        <div class="config-toggle">
          ${pwrSwitchHtml(_continuous, ' id="cfg-agent-continuous" data-action="toggleContinuousAllActiveModes" data-arg-types="t"', 'Continuous in all active modes', 'Shorthand for continuous in every non-QUIET governor mode. Turning it off restores each row to its stored cadence. Takes effect on Save.')}
          <span class="config-toggle-label">
            <b>Continuous in all active modes</b> — shorthand for every non-QUIET row.
            <span class="text-muted">Uses the same turn-end signal as kick delivery (the CLI input prompt); never interrupts a busy session and still respects pause, disabled, on-demand, budget/provider holds, and mode pauses. Turning this off restores the stored per-mode intervals below.</span>
            ${_continuousBudgetNote}
          </span>
        </div>
        ${_continuous ? `<div class="cadence-legacy-notice">
          <b>Legacy continuous is on.</b> SURGE, BUSY, and IDLE are using the old "continuous in all active modes" shortcut until you save per-mode cadences.
          The row buttons below are editable; choosing Off or Interval automatically switches this agent to per-mode cadences.
          <button class="hv-btn btn-secondary btn-sm cadence-legacy-action" type="button" data-action="switchToPerModeCadences">Switch to per-mode cadences</button>
        </div>` : ''}
        <div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px">
          <b>What this is.</b> How often the governor kicks this agent, set separately for each queue-depth mode (idle, quiet, busy, surge).
          <b>Continuous.</b> When enabled, cadence no longer decides the next kick while the agent is in a running sequence; the next kick is scheduled from the observed session end plus the cool-down. Turning it off returns to these cadences.
          <b>Why use it.</b> It lets you run the agent slowly when there's little to do and faster when work piles up, controlling token spend.
          <b>Where it shows up.</b> These intervals drive the governor's kick timing and appear in the Governor cadence table.
          <b>How to configure.</b> Pick Off, Interval, or Continuous for each mode. Continuous mode cells in the Governor cadence table are read-only; edit them here.
        </div>
        <div class="cadence-mode-list">
          ${renderCadenceBuilder('surge', c.surge, _continuous)}
          ${renderCadenceBuilder('busy', c.busy, _continuous)}
          ${renderCadenceBuilder('quiet', c.quiet, _continuous)}
          ${renderCadenceBuilder('idle', c.idle, _continuous)}
        </div>
        <div class="cadence-continuous-fields">
          <span class="config-inline-field">cool-down (seconds)
            <input type="number" min="1" step="1" value="${_continuousCooldown}" data-key="continuousCooldown" data-change-action="markDirty" data-arg0="general" data-arg1="continuousCooldown" data-arg-types="s,s,v">
          </span>
          <span class="config-inline-field">budget guard (%)
            <input type="number" min="1" max="100" step="1" value="${_continuousBudgetPct}" data-key="continuousBudgetPct" data-change-action="markDirty" data-arg0="general" data-arg1="continuousBudgetPct" data-arg-types="s,s,v">
          </span>
          <span class="cadence-mode-help">Cool-down and budget guard apply to every mode set to Continuous.</span>
        </div>`;
    }

    function renderAgentPipeline(p) {
      return `
        <div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px">
          <b>What this is.</b> The deterministic pre-kick pipeline stages that run for this agent before the LLM is invoked.
          <b>Why use it.</b> These stages filter and classify incoming work so the agent only sees what's relevant, saving tokens and time.
          <b>Where it shows up.</b> It affects what filtering and classification happens on each kick before the model receives any work.
          <b>How to configure.</b> Toggle a stage off to skip it for this agent; leave it on to keep that stage in the pipeline.
        </div>
        ${PIPELINE_STAGES.map(stage => `
          <div class="config-toggle">
            <div class="config-toggle-switch ${p[stage] !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="pipeline" data-key="${stage}"></div>
            <span class="config-toggle-label">${stage} ${settingHelpMark('settings', 'agent-pipeline', stage, `${PIPELINE_STAGE_TIPS[stage] || stage}`)}</span>
          </div>
        `).join('')}`;
    }

    function renderAgentHooks(h) {
      const preList = (h.preKick || []).map((s, i) =>
        `<div class="config-list-item"><span>${esc(s)}</span><button class="remove-item" data-action="removeListItem" data-section="hooks" data-key="preKick" data-index="${i}">✕</button></div>`
      ).join('');
      const postList = (h.postIdle || []).map((s, i) =>
        `<div class="config-list-item"><span>${esc(s)}</span><button class="remove-item" data-action="removeListItem" data-section="hooks" data-key="postIdle" data-index="${i}">✕</button></div>`
      ).join('');
      return `
        <div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px">
          <b>What this is.</b> Shell scripts this agent runs before each kick (pre-kick) and after it returns to idle (post-idle).
          <b>Why use it.</b> Use them for setup before work (pull code, warm caches, check prerequisites) and cleanup after (commit, notify, collect metrics).
          <b>Where it shows up.</b> Pre-kick scripts must exit 0 or the kick is skipped; post-idle scripts run once the agent goes idle.
          <b>How to configure.</b> Enter the absolute path to a shell script and add it to the pre-kick or post-idle list; they run in order.
        </div>
        <div class="config-list">
          <label style="font-size:0.7rem;color:var(--muted);text-transform:uppercase;letter-spacing:0.5px;margin-bottom:var(--sp-3);display:block">Pre-Kick Scripts ${settingHelpMark('settings', 'agent-hooks', 'Pre-Kick Scripts', `Shell scripts executed before each governor kick. Use for setup tasks like pulling latest code, clearing caches, or checking prerequisites. Scripts run in order and must exit 0 for the kick to proceed.`)}</label>
          ${preList || '<div class="fs-sm text-muted mb-3">None configured</div>'}
          <div class="config-list-add">
            <input type="text" id="cfg-pre-kick-input" placeholder="script path" title="Absolute path to a shell script to run before each kick">
            <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="hooks" data-arg1="preKick" data-arg2="cfg-pre-kick-input" title="Add this script to the pre-kick hook list">+ Add</button>
          </div>
        </div>
        <div class="config-list">
          <label style="font-size:0.7rem;color:var(--muted);text-transform:uppercase;letter-spacing:0.5px;margin-bottom:var(--sp-3);display:block">Post-Idle Scripts ${settingHelpMark('settings', 'agent-hooks', 'Post-Idle Scripts', `Shell scripts executed after the agent finishes its work and returns to idle. Use for cleanup tasks like committing progress, sending notifications, or collecting metrics.`)}</label>
          ${postList || '<div class="fs-sm text-muted mb-3">None configured</div>'}
          <div class="config-list-add">
            <input type="text" id="cfg-post-idle-input" placeholder="script path" title="Absolute path to a shell script to run after the agent returns to idle">
            <button class="hv-btn btn-primary" data-action="addListItem" data-arg0="hooks" data-arg1="postIdle" data-arg2="cfg-post-idle-input" title="Add this script to the post-idle hook list">+ Add</button>
          </div>
        </div>`;
    }

    function renderAgentRestrictions(restrictions) {
      const agentRules = (restrictions.agent || []);
      const globalRules = (restrictions.global || []);
      const policyRules = (restrictions.policy || []);

      function renderSection(title, color, rules, opts) {
        const editable = opts && opts.editable;
        const icon = opts && opts.icon || '';
        let h = `<div class="mb-6">`;
        h += `<div style="font-size:0.72rem;font-weight:600;color:${color};margin-bottom:var(--sp-3);text-transform:uppercase;display:flex;align-items:center;gap:var(--sp-2)">${icon} ${esc(title)}</div>`;
        if (rules.length === 0 && !editable) {
          h += `<div style="font-size:var(--fs-sm);color:var(--muted);padding:var(--sp-2) var(--sp-0)">None</div>`;
        }
        for (let i = 0; i < rules.length; i++) {
          const r = rules[i];
          h += `<div style="font-size:0.78rem;padding:var(--sp-3) var(--sp-4);margin-bottom:var(--sp-2);background:var(--bg);border-left:3px solid ${color};border-radius:0 4px 4px 0;display:flex;align-items:start;gap:var(--sp-4)">`;
          h += `<div style="flex:1">`;
          h += `<code class="fs-sm">${esc(r.pattern || '')}</code>`;
          if (r.reason) h += `<div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-1)">${esc(r.reason)}</div>`;
          h += '</div>';
          if (editable) {
            h += `<label style="font-size:0.7rem;display:flex;align-items:center;gap:var(--sp-2);cursor:pointer;white-space:nowrap">`;
            h += `<input type="checkbox" ${r.enabled !== false ? 'checked' : ''} data-change-action="toggleRestriction" data-arg0="${i}" data-arg-types="j,c"> on</label>`;
            h += `<button class="hv-btn btn-danger" data-action="removeRestriction" data-index="${i}">✕</button>`;
          }
          h += '</div>';
        }
        if (editable) {
          h += `<div style="display:flex;gap:var(--sp-3);margin-top:var(--sp-3)">`;
          h += `<input type="text" id="cfg-restriction-pattern" placeholder="glob pattern, e.g. gh issue list*" style="flex:2;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)" title="Glob pattern matching commands this agent is blocked from running. The gh wrapper enforces these restrictions.">`;
          h += `<input type="text" id="cfg-restriction-reason" placeholder="reason (optional)" style="flex:1;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)" title="Optional explanation shown when the agent hits this restriction">`;
          h += `<button class="hv-btn btn-danger btn-sm" data-action="addRestriction" title="Add this restriction to the agent command blocklist">+ Add</button>`;
          h += '</div>';
        }
        h += '</div>';
        return h;
      }

      let html = '<div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px"><b>What this is.</b> Allow/deny rules, layered across agent, global, and policy scopes, that gate which commands this agent may run. <b>Why use it.</b> They keep an agent inside its lane by blocking risky or out-of-scope actions before they execute. <b>Where it shows up.</b> Rules are enforced by the gh wrapper and shown here grouped by scope, indicating which actions are permitted. <b>How to configure.</b> Edit the agent-scope rules here (glob pattern plus optional reason); global and policy rules come from policy config and are read-only.</div>';
      html += renderSection('Agent restrictions', 'var(--blue)', agentRules, { editable: true, icon: '🔧' });
      html += '<div style="border-top:1px solid var(--border);padding-top:var(--sp-5)">';
      html += renderSection('Global (all agents)', 'var(--red)', globalRules, { icon: '🌐' });
      html += renderSection('Policy (CLAUDE.md)', '#7c3aed', policyRules, { icon: '📋' });
      html += '</div>';
      return html;
    }

    // --- Channels tab ---

    const CHANNEL_COLORS = {kick:'var(--blue)',webhook:'var(--green)',discord:'#7c3aed',schedule:'var(--orange)',bead:'#0891b2'};
    const CHANNEL_TYPES = ['kick'];

    function renderAgentChannels(channels) {
      const disabled = _configState.savingChannels ? ' disabled' : '';
      let html = '<div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px"><b>What this is.</b> Governor kick channels. Only <code>kick</code> has a supported channel runtime; webhook, discord, schedule, and bead channels are not supported. <b>How to configure.</b> Add a kick channel or toggle an existing channel; with no explicit channels the agent uses governor kicks by default. <b>GitHub issue triggers.</b> To select an agent for a newly seen actionable issue, configure a top-level <code>triggers:</code> CEL rule using <code>event.kind == "issue.opened"</code> and the agent name. Signed GitHub webhooks wake evaluation through the existing GitHub App/hub relay or <code>/api/webhook/github</code> receiver. Saving a channel does not register a GitHub webhook. See <code>docs/cel-triggers.md</code> and <code>docs/operator-reference.md</code> for setup and restart semantics.</div>';
      if (!channels || channels.length === 0) {
        html += '<div style="font-size:0.8rem;color:var(--muted);padding:var(--sp-6) var(--sp-0);text-align:center;border:1px dashed var(--border);border-radius:8px">No explicit channels — this agent uses governor timer kicks by default.</div>';
      } else {
        for (let i = 0; i < channels.length; i++) {
          const ch = channels[i];
          const color = CHANNEL_COLORS[ch.type] || 'var(--muted)';
          const enabled = ch.enabled !== false;
          html += '<div style="font-size:0.78rem;padding:8px 10px;margin-bottom:var(--sp-3);background:var(--bg);border-left:3px solid '+color+';border-radius:0 4px 4px 0;display:flex;align-items:center;gap:var(--sp-4);opacity:'+(enabled?'1':'0.5')+'">';
          html += '<span style="font-weight:600;color:'+color+';text-transform:uppercase;font-size:0.7rem;min-width:60px">'+esc(ch.type)+'</span>';
          html += '<div style="flex:1;font-size:var(--fs-sm);color:var(--muted)">';
          if (ch.events && ch.events.length) html += 'events: '+ch.events.map(e => '<code>'+esc(e)+'</code>').join(', ');
          if (ch.patterns && ch.patterns.length) html += 'patterns: '+ch.patterns.map(p => '<code>'+esc(p)+'</code>').join(', ');
          if (ch.schedule) html += 'schedule: <code>'+esc(ch.schedule)+'</code>';
          if (ch.match) html += 'match: '+Object.entries(ch.match).map(([k,v]) => '<code>'+esc(k)+'='+esc(v)+'</code>').join(', ');
          if (ch.repos && ch.repos.length) html += ' (repos: '+ch.repos.join(', ')+')';
          html += '</div>';
          html += '<label style="font-size:0.7rem;display:flex;align-items:center;gap:var(--sp-2);cursor:pointer"><input type="checkbox" '+(enabled?'checked':'')+disabled+' data-change-action="toggleChannel" data-arg0="'+i+'" data-arg-types="s,c"> on</label>';
          html += '<button class="hv-btn btn-danger" data-action="removeChannel" data-arg0="'+i+'"'+disabled+'>✕</button>';
          html += '</div>';
        }
      }
      html += '<div style="margin-top:var(--sp-5);padding:var(--sp-5);border:1px solid var(--border);border-radius:8px;background:var(--surface)">';
      html += '<div style="font-size:0.72rem;font-weight:600;margin-bottom:var(--sp-4);text-transform:uppercase;color:var(--muted)">+ Add Channel</div>';
      html += '<div style="display:flex;gap:var(--sp-3);flex-wrap:wrap;align-items:end">';
      html += '<div><label style="font-size:0.7rem;color:var(--muted)">Type</label><select id="ch-add-type" style="display:block;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)" data-change-action="channelTypeChanged">'+CHANNEL_TYPES.map(t => '<option value="'+t+'">'+t+'</option>').join('')+'</select></div>';
      html += '<div id="ch-add-fields" style="display:flex;gap:var(--sp-3);flex:1;flex-wrap:wrap"></div>';
      html += '<button class="hv-btn btn-primary btn-sm" data-action="addChannel"'+disabled+'>Add</button>';
      html += '</div></div>';
      setTimeout(channelTypeChanged, 0);
      return html;
    }

    function channelTypeChanged() {
      const el = document.getElementById('ch-add-fields');
      if (!el) return;
      el.textContent = 'Governor kick — no additional fields.';
    }

    function addChannel() {
      const type = (document.getElementById('ch-add-type') || {}).value;
      if (!CHANNEL_TYPES.includes(type)) {
        showToast('Only kick channels are supported. Use triggers: for GitHub issue routing.', 'error');
        return;
      }
      const channels = (_configState.data.channels || []).slice();
      channels.push({type: type});
      return saveChannels(channels);
    }

    function removeChannel(idx) {
      const channels = (_configState.data.channels || []).slice();
      channels.splice(idx, 1);
      return saveChannels(channels);
    }

    function toggleChannel(idx, enabled) {
      const channels = (_configState.data.channels || []).slice();
      if (channels[idx]) channels[idx] = {...channels[idx], enabled: enabled};
      return saveChannels(channels);
    }

    function saveChannels(channels) {
      const state = _configState;
      if (state.savingChannels) return Promise.resolve();
      const name = state.agent;
      state.savingChannels = true;
      if (state.tab === 'Channels') renderConfigTab('Channels');
      return fetch('/api/config/agent/' + encodeURIComponent(name) + '/channels', {method:'PUT', headers:_inceptionAuthHeaders(), body:JSON.stringify({channels:channels})})
        .then(async r => {
          let data;
          try {
            data = await r.json();
          } catch (_) {
            if (r.ok) throw new Error('Invalid server response (HTTP ' + r.status + ')');
            throw new Error('HTTP ' + r.status);
          }
          if (!r.ok) throw new Error((data && data.error) || ('HTTP ' + r.status));
          return data;
        })
        .then(() => {
          state.data.channels = channels;
          if (_configState === state) showToast('Channels saved', 'success');
        })
        .catch(e => { if (_configState === state) showToast('Save error: ' + e.message, 'error'); })
        .finally(() => {
          state.savingChannels = false;
          if (_configState === state && state.tab === 'Channels') renderConfigTab('Channels');
        });
    }

    // --- Tools tab ---

    const TOOL_PRESETS = ['advisory','issues-only','issues-prs','full'];
    const TOOL_PRESET_DENIES = {
      'advisory': ['mcp__github__create_pull_request','mcp__github__create_issue','mcp__github__update_issue','mcp__github__merge_pull_request'],
      'issues-only': ['mcp__github__create_pull_request','mcp__github__merge_pull_request'],
      'issues-prs': [],
      'full': []
    };

    function renderAgentTools(tools) {
      let html = '<div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px"><b>What this is.</b> The agent\'s tool-permission tier (advisory, issues-only, issues-prs, or full) plus per-tool allow/deny overrides. <b>Why use it.</b> It controls which MCP tools the agent can call, letting you grant or withhold write actions like creating PRs or merging. <b>Where it shows up.</b> When set, these declarative permissions override the Mode-based restrictions from the General tab. <b>How to configure.</b> Pick a preset, then add rules for exceptions; an explicit allow rule overrides a deny that the preset would otherwise apply.</div>';
      if (!tools) {
        html += '<div style="font-size:0.8rem;color:var(--muted);padding:var(--sp-6) var(--sp-0);text-align:center;border:1px dashed var(--border);border-radius:8px">No explicit tool config — using Mode-based restrictions from the General tab. <a class="text-accent" href="#" data-action="switchConfigTab" data-arg0="General" data-prevent="1">Go to General</a></div>';
        html += '<div style="margin-top:var(--sp-5);text-align:center"><button class="hv-btn btn-primary btn-sm" data-action="enableToolsConfig">Enable Declarative Tools</button></div>';
        return html;
      }
      const preset = tools.preset || '';
      const rules = tools.rules || [];
      html += '<div style="margin-bottom:14px"><div style="font-size:0.72rem;font-weight:600;text-transform:uppercase;color:var(--muted);margin-bottom:var(--sp-3)">Preset</div>';
      html += '<div style="display:flex;gap:var(--sp-3);flex-wrap:wrap">';
      TOOL_PRESETS.forEach(p => {
        const active = preset === p;
        html += '<button class="hv-btn btn-danger btn-sm" data-action="setToolPreset" data-arg0="'+p+'">'+p+'</button>';
      });
      html += '</div>';
      if (preset && TOOL_PRESET_DENIES[preset]) {
        const denies = TOOL_PRESET_DENIES[preset];
        if (denies.length > 0) {
          html += '<div style="margin-top:var(--sp-4);display:flex;gap:var(--sp-2);flex-wrap:wrap">';
          denies.forEach(d => {
            const overridden = rules.some(r => r.pattern === d && r.action === 'allow');
            html += '<span style="font-size:0.7rem;padding:var(--sp-1) var(--sp-4);border-radius:10px;background:'+(overridden?'var(--green)':'var(--red)')+';color:#fff;text-decoration:'+(overridden?'line-through':'none')+'">'+esc(d.replace('mcp__github__',''))+'</span>';
          });
          html += '</div>';
        }
      }
      html += '</div>';
      html += '<div class="mb-5"><div style="font-size:0.72rem;font-weight:600;text-transform:uppercase;color:var(--muted);margin-bottom:var(--sp-3)">Rules</div>';
      if (rules.length === 0) {
        html += '<div style="font-size:var(--fs-sm);color:var(--muted);padding:var(--sp-2) var(--sp-0)">No custom rules</div>';
      }
      rules.forEach((r, i) => {
        const color = r.action === 'allow' ? 'var(--green)' : 'var(--red)';
        html += '<div style="font-size:0.78rem;padding:var(--sp-3) var(--sp-4);margin-bottom:var(--sp-2);background:var(--bg);border-left:3px solid '+color+';border-radius:0 4px 4px 0;display:flex;align-items:start;gap:var(--sp-4)">';
        html += '<span style="font-size:0.7rem;padding:1px 8px;border-radius:8px;background:'+color+';color:#fff;text-transform:uppercase;font-weight:600">'+r.action+'</span>';
        html += '<div style="flex:1"><code class="fs-sm">'+esc(r.pattern)+'</code>';
        if (r.reason) html += '<div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-1)">'+esc(r.reason)+'</div>';
        html += '</div>';
        html += '<button class="hv-btn btn-danger" data-action="removeToolRule" data-arg0="+i+">✕</button>';
        html += '</div>';
      });
      html += '<div style="display:flex;gap:var(--sp-3);margin-top:var(--sp-3);flex-wrap:wrap;align-items:end">';
      html += '<input type="text" id="tool-rule-pattern" placeholder="mcp__github__delete_*" style="flex:2;min-width:180px;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)">';
      html += '<select id="tool-rule-action" style="padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)"><option value="deny">deny</option><option value="allow">allow</option></select>';
      html += '<input type="text" id="tool-rule-reason" placeholder="reason (optional)" style="flex:1;min-width:100px;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)">';
      html += '<button class="hv-btn btn-primary btn-sm" data-action="addToolRule">+ Add</button>';
      html += '</div></div>';
      return html;
    }

    function enableToolsConfig() {
      _configState.data.tools = {preset:'', rules:[]};
      renderConfigTab('Tools');
    }

    function setToolPreset(preset) {
      const tools = _configState.data.tools || {rules:[]};
      tools.preset = preset;
      _configState.data.tools = tools;
      saveTools(tools);
    }

    function addToolRule() {
      const pattern = (document.getElementById('tool-rule-pattern') || {}).value;
      const action = (document.getElementById('tool-rule-action') || {}).value;
      const reason = (document.getElementById('tool-rule-reason') || {}).value;
      if (!pattern) { showToast('Pattern is required', 'error'); return; }
      const tools = _configState.data.tools || {preset:'', rules:[]};
      if (!tools.rules) tools.rules = [];
      tools.rules.push({pattern:pattern, action:action, reason:reason||undefined});
      _configState.data.tools = tools;
      saveTools(tools);
    }

    function removeToolRule(idx) {
      const tools = _configState.data.tools || {preset:'', rules:[]};
      if (tools.rules) tools.rules.splice(idx, 1);
      _configState.data.tools = tools;
      saveTools(tools);
    }

    function saveTools(tools) {
      const name = _configState.agent;
      fetch('/api/config/agent/' + name + '/tools', {method:'PUT', headers:_inceptionAuthHeaders(), body:JSON.stringify(tools)})
        .then(r => { if (!r.ok) throw new Error('save failed'); return r.json(); })
        .then(() => { showToast('Tools saved', 'success'); renderConfigTab('Tools'); })
        .catch(e => showToast('Save error: ' + e.message, 'error'));
    }

    // --- Connections tab ---

    const CONN_ICONS = {mcp:'🔌', api:'🌐', knowledge:'📚'};
    const CONN_TYPES = ['mcp','api','knowledge'];

    function renderAgentConnections(connections) {
      let html = '<div style="font-size:0.78rem;color:var(--muted);line-height:1.55;background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:10px 12px;margin-bottom:14px"><b>What this is.</b> External integrations the agent can use — an MCP server, an API, or a knowledge source — each with a name, type, and URI. <b>Why use it.</b> They extend the agent beyond its built-in toolset, letting it reach services like a Jira API or a custom MCP server. <b>Where it shows up.</b> Configured connections are listed here and become available to the agent at runtime. <b>How to configure.</b> Enter a name, choose the type (mcp/api/knowledge), and provide the URI (e.g. <code>stdio:///usr/local/bin/mcp-github</code>), then add it.</div>';
      if (!connections || connections.length === 0) {
        html += '<div style="font-size:0.8rem;color:var(--muted);padding:var(--sp-6) var(--sp-0);text-align:center;border:1px dashed var(--border);border-radius:8px">No external connections configured.</div>';
      } else {
        connections.forEach((c, i) => {
          const icon = CONN_ICONS[c.type] || '';
          html += '<div style="font-size:0.78rem;padding:8px 10px;margin-bottom:var(--sp-3);background:var(--bg);border:1px solid var(--border);border-radius:var(--r);display:flex;align-items:center;gap:var(--sp-4)">';
          html += '<span style="font-size:1.1rem">'+icon+'</span>';
          html += '<div style="flex:1">';
          html += '<div style="font-weight:600">'+esc(c.name)+' <span style="font-size:0.65rem;padding:1px 6px;border-radius:8px;background:var(--surface);border:1px solid var(--border);color:var(--muted);text-transform:uppercase;font-weight:400;margin-left:var(--sp-2)">'+esc(c.type)+'</span></div>';
          if (c.uri) html += '<div style="font-size:0.72rem;color:var(--muted);font-family:var(--font-mono);overflow:hidden;text-overflow:ellipsis;max-width:400px" title="'+esc(c.uri)+'">'+esc(c.uri)+'</div>';
          if (c.auth) html += '<div class="fs-xs text-muted">🔒 '+esc(c.auth.type)+': '+(c.auth.env_var||c.auth.file||'')+'</div>';
          html += '</div>';
          html += '<button class="hv-btn btn-danger" data-action="removeConnection" data-arg0="+i+">✕</button>';
          html += '</div>';
        });
      }
      html += '<div style="margin-top:var(--sp-5);padding:var(--sp-5);border:1px solid var(--border);border-radius:8px;background:var(--surface)">';
      html += '<div style="font-size:0.72rem;font-weight:600;margin-bottom:var(--sp-4);text-transform:uppercase;color:var(--muted)">+ Add Connection</div>';
      html += '<div style="display:flex;gap:var(--sp-3);flex-wrap:wrap;align-items:end">';
      html += '<div><label style="font-size:0.7rem;color:var(--muted)">Name</label><input id="conn-add-name" type="text" placeholder="jira-api" style="display:block;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg);min-width:100px"></div>';
      html += '<div><label style="font-size:0.7rem;color:var(--muted)">Type</label><select id="conn-add-type" style="display:block;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg)">'+CONN_TYPES.map(t => '<option value="'+t+'">'+t+'</option>').join('')+'</select></div>';
      html += '<div><label style="font-size:0.7rem;color:var(--muted)">URI</label><input id="conn-add-uri" type="text" placeholder="stdio:///usr/local/bin/mcp-github" style="display:block;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r-sm);font-size:0.78rem;background:var(--bg);color:var(--fg);min-width:250px"></div>';
      html += '<button class="hv-btn btn-primary btn-sm" data-action="addConnection">Add</button>';
      html += '</div></div>';
      return html;
    }

    function addConnection() {
      const name = (document.getElementById('conn-add-name') || {}).value;
      const type = (document.getElementById('conn-add-type') || {}).value;
      const uri = (document.getElementById('conn-add-uri') || {}).value;
      if (!name) { showToast('Name is required', 'error'); return; }
      const connections = (_configState.data.connections || []).slice();
      connections.push({name:name, type:type, uri:uri||undefined});
      _configState.data.connections = connections;
      saveConnections(connections);
    }

    function removeConnection(idx) {
      const connections = (_configState.data.connections || []).slice();
      connections.splice(idx, 1);
      _configState.data.connections = connections;
      saveConnections(connections);
    }

    function saveConnections(connections) {
      const name = _configState.agent;
      fetch('/api/config/agent/' + name + '/connections', {method:'PUT', headers:_inceptionAuthHeaders(), body:JSON.stringify({connections:connections})})
        .then(r => { if (!r.ok) throw new Error('save failed'); return r.json(); })
        .then(() => { showToast('Connections saved', 'success'); renderConfigTab('Connections'); })
        .catch(e => showToast('Save error: ' + e.message, 'error'));
    }

    // --- Import preview extension ---

    var TEMPLATE_VARS = [
      {name: '${AGENT_NAME}', desc: 'Agent internal name'},
      {name: '${AGENT_DISPLAY_NAME}', desc: 'Friendly display name'},
      {name: '${TIMESTAMP}', desc: 'Current date/time'},
      {name: '${QUEUE_ISSUES}', desc: 'Actionable issues count'},
      {name: '${QUEUE_PRS}', desc: 'Actionable PRs count'},
      {name: '${ISSUE_LIST}', desc: 'Formatted issue list'},
      {name: '${PR_LIST}', desc: 'Formatted PR list'},
      {name: '${GH_AUTH}', desc: 'GitHub auth instructions'},
      {name: '${PROJECT_ORG}', desc: 'Organization name'},
      {name: '${PROJECT_PRIMARY_REPO}', desc: 'Primary repo path'},
      {name: '${PROJECT_REPOS_LIST}', desc: 'Comma-separated repos'},
      {name: '${HIVE_REPO}', desc: 'Hive repository path'},
      {name: '${HIVE_ID}', desc: 'Hive instance ID'},
      {name: '${KNOWLEDGE}', desc: 'Knowledge primer section'},
      {name: '${MERGE_ELIGIBLE}', desc: 'Merge-eligible PR list'},
      {name: '${AUTHORIZED_REPOS}', desc: 'Monitored repos section'},
      {name: '${AGENT_LIST}', desc: 'List of enabled agents'},
    ];

    var _promptTemplateEditorMode = 'edit';

    function renderAgentPromptTemplate() {
      const agentName = _configState.agent;
      const editorId = 'prompt-template-editor';
      const previewId = 'prompt-template-preview';
      const srcId = 'prompt-source-files';
      if (agentName) {
        fetch(`/api/config/agent/${agentName}/prompt`)
          .then(r => r.json())
          .then(data => {
            const srcEl = document.getElementById(srcId);
            // data.template: the scheduler's own resolution of the configured
            // kick_template (#7390). A dangling name used to render as an
            // empty editor with a 404 repo link and nothing else — and text
            // typed into that empty box, once saved, became a LIVE override.
            const tpl = data.template || null;
            const dangling = !!(tpl && tpl.kickTemplate && !tpl.resolved);
            if (srcEl && data.sourceFiles && (data.sourceFiles || []).length > 0) {
              let items = '';
              for (const sf of (data.sourceFiles || [])) {
                const note = sf.note ? ' <span class="text-muted">(' + esc(sf.note) + ')</span>' : '';
                // No link for a path that does not exist in the repo: the
                // server omits url when pkg/policies/defaults has no such file.
                const pathHtml = sf.url
                  ? '<a href="' + esc(sf.url) + '" target="_blank" rel="noopener" style="color:var(--cyan);text-decoration:none;font-family:var(--font-mono);font-size:var(--fs-sm)">' + esc(sf.path) + '</a>'
                  : '<span style="font-family:var(--font-mono);font-size:var(--fs-sm);color:var(--muted);text-decoration:line-through">' + esc(sf.path) + '</span>';
                items += '<li style="margin:3px 0"><strong style="color:var(--muted);min-width:80px;display:inline-block">' + esc(sf.label) + ':</strong> '
                  + pathHtml + note + '</li>';
              }
              if (dangling) {
                items += '<li style="margin:var(--sp-3) var(--sp-0) var(--sp-1);color:var(--yellow,#e6b800);font-size:var(--fs-sm)">⚠ template not found: <code>' + esc(tpl.kickTemplate) + '</code>'
                  + ' — kicks fall back to <strong>' + esc(tpl.fallback || 'the built-in kick') + '</strong>.'
                  + ' Saving text here creates <code>' + esc((data.templateFile && data.templateFile.savePath) || (tpl.pathsTried || [])[0] || tpl.kickTemplate) + '</code> as a live override; clear <code>kick_template</code> in General to keep the fallback.'
                  + (tpl.pathsTried && tpl.pathsTried.length ? '<div class="text-muted fs-xs mt-1">looked in: ' + esc(tpl.pathsTried.join(', ')) + '</div>' : '')
                  + '</li>';
              } else if (tpl && tpl.kickTemplate && tpl.source) {
                items += '<li style="margin:3px 0;color:var(--muted);font-size:0.7rem">resolved from: ' + esc(tpl.source) + '</li>';
              }
              // data.templateFile (#13005): a shipped template that cannot be
              // written in place is flagged before editing, with the override
              // path a Save will create instead.
              const tf = data.templateFile || null;
              if (tf && tf.override) {
                items += '<li class="text-muted fs-xs">user override: <code>' + esc(tf.path) + '</code>'
                  + (tf.readOnly ? ' (shipped default <code>' + esc(tf.defaultPath) + '</code> is read-only)' : '') + '</li>';
              } else if (tf && tf.readOnly) {
                items += '<li class="text-warn fs-xs">shipped default <code>' + esc(tf.defaultPath) + '</code> is read-only — saving writes a user override to <code>' + esc(tf.savePath) + '</code>, which agent runs use instead.</li>';
              }
              srcEl.innerHTML = '<ul style="list-style:none;padding:var(--sp-0);margin:var(--sp-0)">' + items + '</ul>';
              srcEl.style.display = 'block';
            }
            const previewEl = document.getElementById(previewId);
            if (previewEl) {
              const raw = data.prompt || '';
              if (!raw && dangling) {
                previewEl.innerHTML = '<span style="color:var(--yellow,#e6b800);font-style:italic">⚠ template not found: ' + esc(tpl.kickTemplate) + ' (falling back to ' + esc(tpl.fallback || 'the built-in kick') + ')</span>';
              } else if (!raw) { previewEl.innerHTML = '<span style="color:var(--muted);font-style:italic">(no template found)</span>'; }
              else { previewEl.innerHTML = formatMarkdownPrompt(raw); }
            }
          })
          .catch(() => {});

        // A pending (unsaved) edit outlives a tab switch: re-entering the tab
        // restores the operator's in-progress text rather than clobbering it
        // with the server copy. Only fetch-and-fill when nothing is pending.
        if (_configState.promptTemplateDirty) {
          const pending = _configState.promptTemplateValue || '';
          setTimeout(function() {
            const el = document.getElementById(editorId);
            if (el) { el.value = pending; }
          }, 0);
        } else {
          fetch(`/api/config/agent/${agentName}/prompt?raw=1`)
            .then(r => r.json())
            .then(data => {
              const el = document.getElementById(editorId);
              // A race: the operator may have started typing while this was
              // in flight. Never overwrite a pending edit.
              if (el && !_configState.promptTemplateDirty) { el.value = data.prompt || ''; }
            })
            .catch(() => {});
        }
      }

      const varPickerItems = TEMPLATE_VARS.map(v =>
        '<button class="hv-btn btn-primary btn-sm" data-action="insertTemplateVar" data-arg0="' + esc(v.name) + '" title="' + esc(v.desc) + '">'
        + '<code style="color:var(--cyan);font-size:var(--fs-xs)">' + esc(v.name) + '</code>'
        + '<span class="text-muted">' + esc(v.desc) + '</span>'
        + '</button>'
      ).join('');

      const gen = (_configState.data && _configState.data.general) || {};
      const ps = gen.promptSource || {};
      return `
        <div id="prompt-template-tab" style="display:flex;flex-direction:column;height:100%;min-height:0;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain">
        <div id="${srcId}" style="display:none;background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:10px 14px;margin-bottom:10px;flex-shrink:0">
          <div style="font-size:0.7rem;font-weight:600;color:var(--text);margin-bottom:var(--sp-3)">Source files <span style="font-weight:400;color:var(--muted)">(edit these on your fork)</span></div>
        </div>
        <details style="margin-bottom:10px;flex-shrink:0;background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:var(--sp-4) var(--sp-5)"${(ps.owner || ps.repo || ps.path) ? ' open' : ''}>
          <summary style="font-size:0.72rem;font-weight:600;color:var(--text);cursor:pointer">Import from repo</summary>
          <p style="font-size:0.65rem;color:var(--muted);margin:var(--sp-3) var(--sp-0)">Pull this agent's kick prompt from a file in a GitHub repo. The repo must be on the operator's allowlist. Owner, repo, and path are all required.</p>
          <div class="config-field-row">
            <div class="config-field"><label style="font-size:0.7rem">Owner</label><input type="text" id="pt-ps-owner" value="${esc(ps.owner || '')}" placeholder="e.g. kubestellar"></div>
            <div class="config-field"><label style="font-size:0.7rem">Repo</label><input type="text" id="pt-ps-repo" value="${esc(ps.repo || '')}" placeholder="e.g. hive"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label style="font-size:0.7rem">File path</label><input type="text" id="pt-ps-path" value="${esc(ps.path || '')}" placeholder="e.g. prompts/scanner.md"></div>
            <div class="config-field"><label style="font-size:0.7rem">Ref <span style="font-weight:400;color:var(--muted)">(optional)</span></label><input type="text" id="pt-ps-ref" value="${esc(ps.ref || '')}" placeholder="branch, tag, or SHA"></div>
          </div>
          <label style="display:flex;align-items:center;gap:var(--sp-4);margin:var(--sp-3) var(--sp-0);font-size:0.72rem;cursor:pointer;font-weight:400">
            <input type="checkbox" id="pt-ps-keep-linked" style="width:auto;margin:var(--sp-0)"${ps.owner ? ' checked' : ''}>
            <span>Keep linked to repo (live) <span class="text-muted">— resolve the prompt at kick time so repo edits propagate. Unchecked: fetch once and bake a static copy.</span></span>
          </label>
          <button class="hv-btn btn-primary btn-sm" data-action="importPromptFromRepo">Import Prompt</button>
        </details>
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:var(--sp-4);flex-shrink:0">Kick template with <code>$​{VAR}</code> placeholders. Edit the raw template or preview with variables expanded.</p>
        <div style="display:flex;gap:var(--sp-3);margin-bottom:var(--sp-4);flex-shrink:0">
          <button class="hv-btn btn-primary btn-sm" id="prompt-btn-edit" data-action="switchPromptMode" data-arg0="edit">Edit</button>
          <button class="hv-btn btn-primary btn-sm" id="prompt-btn-preview" data-action="switchPromptMode" data-arg0="preview">Preview</button>
          <div style="flex:1"></div>
          <span style="font-size:var(--fs-xs);color:var(--muted);align-self:center">Edits are saved by the <strong class="text-default">Save</strong> button below.</span>
        </div>
        <textarea id="${editorId}" data-input-action="markPromptTemplateDirty" data-arg-types="t" style="flex:1;min-height:300px;font-family:var(--font-mono);font-size:var(--fs-sm);padding:var(--sp-5);background:var(--bg);color:var(--text);border:1px solid var(--border);border-radius:var(--r);resize:vertical;white-space:pre;tab-size:2;overscroll-behavior:contain;overscroll-behavior-y:contain" placeholder="Enter your agent's kick template here. Use \${VARIABLE} placeholders from the picker below.">${agentName ? '' : ''}</textarea>
        <div id="${previewId}" class="config-pre config-pre-fill" style="display:none;flex:1;min-height:300px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain"></div>
        <div style="margin-top:var(--sp-4);flex-shrink:0">
          <div style="font-size:0.7rem;font-weight:600;color:var(--text);margin-bottom:var(--sp-3)">Variable Picker <span style="font-weight:400;color:var(--muted)">(click to insert at cursor)</span></div>
          <div style="display:flex;flex-wrap:wrap;gap:var(--sp-2);max-height:120px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain;padding:var(--sp-3);background:var(--surface);border:1px solid var(--border);border-radius:var(--r)">${varPickerItems}</div>
        </div>
        </div>`;
    }

    function switchPromptMode(mode) {
      _promptTemplateEditorMode = mode;
      var editor = document.getElementById('prompt-template-editor');
      var preview = document.getElementById('prompt-template-preview');
      var btnEdit = document.getElementById('prompt-btn-edit');
      var btnPreview = document.getElementById('prompt-btn-preview');
      if (!editor || !preview) return;
      if (mode === 'edit') {
        editor.style.display = '';
        preview.style.display = 'none';
        btnEdit.style.background = 'var(--green)';
        btnEdit.style.color = '#fff';
        btnPreview.style.background = 'var(--surface)';
        btnPreview.style.color = 'var(--text)';
      } else {
        editor.style.display = 'none';
        preview.style.display = '';
        btnEdit.style.background = 'var(--surface)';
        btnEdit.style.color = 'var(--text)';
        btnPreview.style.background = 'var(--green)';
        btnPreview.style.color = '#fff';
        fetch('/api/config/agent/' + _configState.agent + '/prompt')
          .then(function(r) { return r.json(); })
          .then(function(data) {
            var raw = data.prompt || '';
            preview.innerHTML = raw ? formatMarkdownPrompt(raw) : '<span class="text-muted">(no template)</span>';
          })
          .catch(function() {});
      }
    }

    function insertTemplateVar(varName) {
      var editor = document.getElementById('prompt-template-editor');
      if (!editor) return;
      var start = editor.selectionStart;
      var end = editor.selectionEnd;
      var text = editor.value;
      editor.value = text.substring(0, start) + varName + text.substring(end);
      var cursorPos = start + varName.length;
      editor.selectionStart = cursorPos;
      editor.selectionEnd = cursorPos;
      editor.focus();
    }

    // importPromptFromRepo reads the Prompt Template tab's repo fields and PUTs
    // them to the prompt endpoint. Keep-linked persists a live prompt_source
    // (resolved at kick time); unchecked bakes a one-time static copy. The server
    // validates the repo against the seed-only allowlist and returns a clear error
    // for a non-allowlisted repo.
    function importPromptFromRepo() {
      var agentName = _configState.agent;
      if (!agentName) { showToast('Save the agent first', 'error'); return; }
      var owner = ((document.getElementById('pt-ps-owner') || {}).value || '').trim();
      var repo = ((document.getElementById('pt-ps-repo') || {}).value || '').trim();
      var path = ((document.getElementById('pt-ps-path') || {}).value || '').trim();
      var ref = ((document.getElementById('pt-ps-ref') || {}).value || '').trim();
      var keepLinked = !!(document.getElementById('pt-ps-keep-linked') || {}).checked;
      if (!owner || !repo || !path) { showToast('Owner, repo, and path are required', 'error'); return; }
      fetch('/api/config/agent/' + agentName + '/prompt', {
        method: 'PUT',
        headers: _inceptionAuthHeaders(),
        body: JSON.stringify({ promptSource: { owner: owner, repo: repo, path: path, ref: ref }, keepLinked: keepLinked })
      })
        .then(function(r) { return r.json().then(function(d) { return {ok: r.ok, data: d}; }); })
        .then(function(result) {
          if (result.ok) {
            showToast(keepLinked ? 'Prompt linked to repo (live)' : 'Prompt imported (baked copy)', 'success');
            // Refresh the editor/preview with the newly fetched content.
            // The import already persisted server-side, so the freshly fetched
            // text is the saved truth: drop any stale pending edit and adopt it
            // as the editor's content. A subsequent footer Save is then a
            // no-op for the template unless the operator edits it again.
            var editor = document.getElementById('prompt-template-editor');
            _configState.promptTemplateDirty = false;
            fetch('/api/config/agent/' + agentName + '/prompt?raw=1')
              .then(function(r) { return r.json(); })
              .then(function(data) {
                _configState.promptTemplateValue = data.prompt || '';
                if (editor) editor.value = data.prompt || '';
              })
              .catch(function() {});
          } else {
            showToast(result.data.error || 'Import failed', 'error');
          }
        })
        .catch(function(err) { showToast('Network error: ' + err.message, 'error'); });
    }

    // markPromptTemplateDirty records an in-progress kick-template edit on
    // _configState. There is deliberately no "Save Template" button any more
    // (#2766: two save buttons — a green "Save Template" above the editor and
    // the orange footer "Save" — meant operators clicked the wrong one). The
    // footer Save is now the single commit point for the whole dialog, so the
    // edited text has to live somewhere that survives a tab switch.
    function markPromptTemplateDirty(el) {
      if (!el) return;
      _configState.promptTemplateDirty = true;
      _configState.promptTemplateValue = el.value;
    }

    // capturePromptTemplate snapshots the live editor into _configState. Called
    // before persisting so a save issued while the Preview pane is showing (the
    // textarea is display:none but still holds the edited text) still commits
    // what the operator typed.
    function capturePromptTemplate() {
      var editor = document.getElementById('prompt-template-editor');
      if (editor && _configState.promptTemplateDirty) {
        _configState.promptTemplateValue = editor.value;
      }
    }

    // savePromptTemplate persists the kick template via the same PUT endpoint
    // the removed "Save Template" button used. It resolves true on success (or
    // when there is nothing to save) and false on failure, so saveConfig() can
    // fold it into the dialog's single save and keep the dialog OPEN on error.
    // It intentionally raises no success toast of its own — saveConfig() emits
    // one "Configuration saved" for the whole dialog.
    function savePromptTemplate() {
      capturePromptTemplate();
      var agentName = _configState.agent;
      if (!agentName || !_configState.promptTemplateDirty) return Promise.resolve(true);
      var value = _configState.promptTemplateValue || '';
      return fetch('/api/config/agent/' + agentName + '/prompt', {
        method: 'PUT',
        headers: _inceptionAuthHeaders(),
        body: JSON.stringify({ template: value })
      })
        .then(function(r) { return r.json().catch(function() { return {}; }).then(function(d) { return {ok: r.ok, data: d}; }); })
        .then(function(result) {
          if (result.ok) {
            _configState.promptTemplateDirty = false;
            return true;
          }
          showToast((result.data && result.data.error) || 'Failed to save prompt template', 'error');
          return false;
        })
        .catch(function(err) { showToast('Network error saving prompt template: ' + err.message, 'error'); return false; });
    }

    function renderAgentExport() {
      var agentName = _configState.agent;
      var yamlId = 'export-yaml-content';
      if (agentName) {
        fetch('/api/config/agent/' + agentName + '/export')
          .then(function(r) { return r.json(); })
          .then(function(data) {
            var el = document.getElementById(yamlId);
            if (el) { el.value = data.yaml || ''; }
          })
          .catch(function() {});
      }
      return '<div style="display:flex;flex-direction:column;height:100%;min-height:0;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain">'
        + '<p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:var(--sp-4);flex-shrink:0">Portable agent definition in YAML format. Use this to share agents across hive instances.</p>'
        + '<div style="display:flex;gap:var(--sp-3);margin-bottom:var(--sp-4);flex-shrink:0">'
        + '<button class="hv-btn btn-primary btn-sm" data-action="copyExportYaml">Copy to Clipboard</button>'
        + '<button class="hv-btn btn-primary btn-sm" data-action="downloadExportYaml">Download YAML</button>'
        + '</div>'
        + '<textarea id="' + yamlId + '" readonly style="flex:1;min-height:300px;font-family:var(--font-mono);font-size:var(--fs-sm);padding:var(--sp-5);background:var(--terminal-bg);color:var(--terminal-fg);border:1px solid var(--terminal-line);border-radius:var(--r);resize:vertical;white-space:pre;tab-size:2;overscroll-behavior:contain;overscroll-behavior-y:contain">Loading...</textarea>'
        + '</div>';
    }

    function copyExportYaml() {
      var el = document.getElementById('export-yaml-content');
      if (!el) return;
      navigator.clipboard.writeText(el.value)
        .then(function() { showToast('Copied to clipboard', 'success'); })
        .catch(function() { showToast('Copy failed', 'error'); });
    }

    function downloadExportYaml() {
      var el = document.getElementById('export-yaml-content');
      if (!el) return;
      var agentName = _configState.agent || 'agent';
      hiveExportViewer({title: agentName + '.yaml', filename: agentName + '.yaml', text: el.value, mime: 'text/yaml'});
    }

    function importAgentFromURL() {
      var urlInput = document.getElementById('import-url-input');
      if (!urlInput || !urlInput.value.trim()) {
        showToast('Enter a URL to import from', 'error');
        return;
      }
      _doAgentImport('url', urlInput.value.trim(), '');
    }

    function importAgentFromPaste() {
      var pasteInput = document.getElementById('import-paste-input');
      if (!pasteInput || !pasteInput.value.trim()) {
        showToast('Paste an agent definition to import', 'error');
        return;
      }
      _doAgentImport('paste', '', pasteInput.value.trim());
    }

    function _doAgentImport(source, url, content) {
      fetch('/api/agents/import', {
        method: 'POST',
        headers: _inceptionAuthHeaders(),
        body: JSON.stringify({ source: source, url: url, content: content })
      })
        .then(function(r) { return r.json().then(function(d) { return {ok: r.ok, data: d}; }); })
        .then(function(result) {
          if (result.ok) {
            showToast('Agent "' + (result.data.name || '') + '" imported', 'success');
            closeConfigDialog();
            fetch('/api/status').then(function(r) { return r.json(); }).then(render).catch(function() {});
          } else {
            showToast(result.data.error || 'Import failed', 'error');
          }
        })
        .catch(function(err) { showToast('Network error: ' + err.message, 'error'); });
    }

    function formatMarkdownPrompt(text) {
      const lines = text.split('\n');
      let html = '';
      let inCode = false;
      for (let i = 0; i < lines.length; i++) {
        const ln = lines[i];
        if (ln.trimStart().startsWith('```')) {
          if (inCode) {
            html += '</pre>';
            inCode = false;
          } else {
            html += '<pre style="margin:var(--sp-4) var(--sp-0);padding:10px 12px;background:var(--bg);border:1px solid var(--border);border-radius:var(--r);font-family:var(--font-mono);font-size:0.72rem;white-space:pre-wrap;overflow-x:auto;color:var(--text)">';
            inCode = true;
          }
          continue;
        }
        if (inCode) {
          html += esc(ln) + '\n';
          continue;
        }
        if (ln.startsWith('# ') && !ln.startsWith('## ')) {
          html += '<div style="font-weight:700;font-size:var(--fs-md);color:var(--text);margin:var(--sp-6) var(--sp-0) var(--sp-4)">' + esc(ln.slice(2)) + '</div>';
        } else if (ln.startsWith('## ')) {
          html += '<div style="font-weight:600;font-size:var(--fs-base);color:var(--text);margin:14px 0 6px;border-bottom:1px solid var(--border);padding-bottom:var(--sp-2)">' + esc(ln.slice(3)) + '</div>';
        } else if (/^\d+\.\s/.test(ln)) {
          html += '<div style="margin:3px 0 3px 12px;color:var(--text)">' + esc(ln) + '</div>';
        } else if (/^\s*[-*]\s/.test(ln)) {
          html += '<div style="margin:var(--sp-1) var(--sp-0) var(--sp-1) var(--sp-7);color:var(--muted)">' + esc(ln.trim()) + '</div>';
        } else if (ln.trim() === '') {
          html += '<div style="height:6px"></div>';
        } else {
          html += '<div style="margin:var(--sp-1) var(--sp-0);color:var(--muted)">' + esc(ln) + '</div>';
        }
      }
      if (inCode) html += '</pre>';
      return html;
    }

    // ── Agent Prompt History ──
    //
    // Shows the prompts (kicks) this agent actually received over a selectable
    // window, newest first, so an owner can answer "what was my agent asked to
    // do, and when?" without reading server logs. Data comes from
    // GET /api/prompt-history, which is role-gated exactly like /api/audit.

    // Selectable windows, in hours. "Last day" and "last week" are the two the
    // feature request named; 1h is a convenience for watching a live hive.
    const PROMPT_HISTORY_WINDOWS = [
      { hours: 1, label: 'Last hour' },
      { hours: 24, label: 'Last day' },
      { hours: 168, label: 'Last week' },
    ];
    const PROMPT_HISTORY_DEFAULT_WINDOW_HOURS = 24;

    // Per-agent memory of the chosen window, so switching tabs or reopening the
    // dialog keeps the selection.
    const _promptHistoryWindow = {};
    // Newest-first entries currently rendered, indexed by the list's data-idx.
    let _promptHistoryEntries = [];

    function renderAgentPromptHistoryShell() {
      const agent = _configState.agent || '';
      const selected = _promptHistoryWindow[agent] || PROMPT_HISTORY_DEFAULT_WINDOW_HOURS;
      const opts = PROMPT_HISTORY_WINDOWS.map(function(w) {
        return '<option value="' + w.hours + '"' + (w.hours === selected ? ' selected' : '') + '>' + esc(w.label) + '</option>';
      }).join('');
      return `<div style="display:flex;flex-direction:column;height:100%;min-height:0;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain">
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:var(--sp-4);flex-shrink:0">Prompts sent to this agent — fully expanded, all variables resolved and pipeline data injected. Newest first.</p>
        <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-4);flex-shrink:0">
          <label class="fs-sm text-muted" for="prompt-history-window">Window</label>
          <select id="prompt-history-window" style="padding:var(--sp-2) var(--sp-4);background:var(--bg);border:1px solid var(--border);border-radius:var(--r);color:var(--text);font-size:var(--fs-sm)">${opts}</select>
          <span id="prompt-history-count" style="font-size:0.7rem;color:var(--muted)"></span>
        </div>
        <div id="prompt-history-list" class="config-pre-fill" style="flex:1;min-height:200px;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain">
          <div style="color:var(--muted);font-style:italic;padding:var(--sp-4)">Loading prompts…</div>
        </div>
        </div>`;
    }

    function initAgentPromptHistory() {
      const sel = document.getElementById('prompt-history-window');
      if (sel) {
        sel.addEventListener('change', function() {
          const agent = _configState.agent || '';
          _promptHistoryWindow[agent] = parseInt(sel.value, 10) || PROMPT_HISTORY_DEFAULT_WINDOW_HOURS;
          loadAgentPromptHistory();
        });
      }
      loadAgentPromptHistory();
    }

    async function loadAgentPromptHistory() {
      const agent = _configState.agent || '';
      const list = document.getElementById('prompt-history-list');
      if (!list) return;
      const hours = _promptHistoryWindow[agent] || PROMPT_HISTORY_DEFAULT_WINDOW_HOURS;
      try {
        const resp = await fetch('/api/prompt-history?agent=' + encodeURIComponent(agent) + '&windowHours=' + hours);
        if (resp.status === 403) {
          list.innerHTML = '<div style="color:var(--muted);padding:var(--sp-4)">You do not have access to prompt history.</div>';
          return;
        }
        if (!resp.ok) {
          list.innerHTML = '<div style="color:var(--muted);padding:var(--sp-4)">Could not load prompt history.</div>';
          return;
        }
        const data = await resp.json();
        // Guard: a failed or partial response can leave entries undefined.
        _promptHistoryEntries = capDashboardArray((data && data.entries) || [], DASHBOARD_LOG_CAP);
        renderAgentPromptHistoryList();
      } catch (e) {
        list.innerHTML = '<div style="color:var(--muted);padding:var(--sp-4)">Could not load prompt history.</div>';
      }
    }

    function promptHistoryRelativeTime(iso) {
      const t = Date.parse(iso);
      if (isNaN(t)) return '—';
      const secs = Math.floor((Date.now() - t) / 1000);
      const MIN = 60, HOUR = 3600, DAY = 86400;
      if (secs < MIN) return 'just now';
      if (secs < HOUR) return Math.floor(secs / MIN) + 'm ago';
      if (secs < DAY) return Math.floor(secs / HOUR) + 'h ago';
      return Math.floor(secs / DAY) + 'd ago';
    }

    // Trigger badge colors. Keys match the triggers the agent manager records.
    const PROMPT_TRIGGER_COLORS = {
      'startup': 'var(--blue)',
      'governor-eval': 'var(--green)',
      'dashboard-api': 'var(--purple)',
      'send-kick': 'var(--purple)',
      'manual': 'var(--amber)',
    };

    function renderAgentPromptHistoryList() {
      const list = document.getElementById('prompt-history-list');
      const countEl = document.getElementById('prompt-history-count');
      if (!list) return;
      const entries = _promptHistoryEntries || [];
      if (countEl) countEl.textContent = entries.length ? entries.length + (entries.length === 1 ? ' prompt' : ' prompts') : '';
      if (!entries.length) {
        // Genuine empty state — this agent has not been kicked in the window.
        list.innerHTML = '<div style="color:var(--muted);font-style:italic;padding:var(--sp-4)">(awaiting first kick)</div>';
        return;
      }
      list.innerHTML = entries.map(function(e, i) {
        const trigger = (e && e.trigger) || 'unknown';
        const color = PROMPT_TRIGGER_COLORS[trigger] || 'var(--muted)';
        const ts = (e && e.ts) || '';
        const absolute = ts ? formatDashboardDateTime(ts) : '';
        // Every field below is untrusted: prompt bodies embed repo content,
        // issue titles and knowledge-base text. esc() here escapes & < > " '
        // and backticks; the body uses escPreserveNewlines so line structure
        // survives inside the <pre>. No inline handlers anywhere — the toggle
        // is wired by data-idx + addEventListener below.
        const truncNote = (e && e.truncated)
          ? '<span style="font-size:0.65rem;color:var(--amber);margin-left:var(--sp-3)">truncated</span>'
          : '';
        return '<div style="border:1px solid var(--border);border-radius:var(--r);margin-bottom:var(--sp-3);overflow:hidden">' +
          '<button type="button" class="prompt-history-toggle" data-idx="' + i + '" ' +
            'style="width:100%;display:flex;align-items:center;gap:var(--sp-4);padding:6px 10px;background:var(--bg);border:none;cursor:pointer;text-align:left;color:var(--text);font-size:var(--fs-sm)">' +
            '<span class="prompt-history-chevron" style="color:var(--muted);flex-shrink:0">▶</span>' +
            '<span title="' + esc(absolute) + '" style="color:var(--muted);white-space:nowrap">' + esc(promptHistoryRelativeTime(ts)) + '</span>' +
            '<span style="padding:1px 6px;border-radius:var(--r-sm);font-size:0.65rem;border:1px solid ' + color + ';color:' + color + ';white-space:nowrap">' + esc(trigger) + '</span>' +
            truncNote +
          '</button>' +
          // max-height + overflow-y make the opened body its own bounded
          // scroller. Without a bound it is a scroll container (overflow-x
          // forces computed overflow-y:auto) that can NEVER scroll, and the
          // #2766 containment on `.config-overlay pre` turns it into a chain
          // boundary: wheel/touch over the opened prompt latches there and
          // goes nowhere — or, on short viewports, leaks to the dashboard
          // behind the modal instead of scrolling the field (#4801).
          '<pre class="prompt-history-body" data-idx="' + i + '" style="display:none;margin:var(--sp-0);padding:10px 12px;background:var(--bg);border-top:1px solid var(--border);font-family:var(--font-mono);font-size:0.72rem;line-height:1.5;white-space:pre-wrap;overflow-x:auto;max-height:60vh;overflow-y:auto;overscroll-behavior:contain;overscroll-behavior-y:contain;color:var(--text)">' +
            escPreserveNewlines((e && e.prompt) || '') +
          '</pre>' +
          '</div>';
      }).join('');

      // Collapse/expand per entry. Prompts run to tens of kilobytes, so the
      // list stays scannable and only the opened one is shown in full.
      list.querySelectorAll('.prompt-history-toggle').forEach(function(btn) {
        btn.addEventListener('click', function() {
          const idx = btn.getAttribute('data-idx');
          const body = list.querySelector('.prompt-history-body[data-idx="' + idx + '"]');
          if (!body) return;
          const open = body.style.display !== 'none';
          body.style.display = open ? 'none' : 'block';
          const chev = btn.querySelector('.prompt-history-chevron');
          if (chev) chev.textContent = open ? '▶' : '▼';
        });
      });
    }

    // The explain-mode default is three values, not one (#4712): what is
    // CONFIGURED ('' = nothing set), what is actually IN FORCE, and WHERE that
    // came from. The last two matter because a hosted owner cannot read the
    // deployment's HIVE_EXPLAIN_MODE, so a default they did not set has to be
    // visible in the form or they are back to hunting for it.
    //
    // Split out of renderGovGeneral so saving can refresh this ONE line rather
    // than repainting the tab, which would discard a half-typed Hive ID or eval
    // interval sitting unsaved beside it.
    function explainDefaultNoteHtml(genAdv) {
      genAdv = genAdv || {};
      const effective = genAdv.explain_mode_effective || 'off';
      const source = genAdv.explain_mode_source || '';
      let note;
      if (source === 'config') note = 'Set here.';
      else if (source) note = 'Inherited from the ' + source.replace(/^env:/, '') + ' deployment environment variable — set a value above to override it.';
      else note = 'No hive default set, so agents that leave Explain Mode unset do not explain.';
      return 'In force now: <strong class="text-default">' + esc(effective) + '</strong>. ' + esc(note) + ' Applies from the next kick &mdash; no restart needed.';
    }
