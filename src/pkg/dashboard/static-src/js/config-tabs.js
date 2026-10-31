    /* renderConfigLoadError replaces the dialog body with an explicit
       "nothing loaded" state. No tab renders (a tab would read the
       placeholder data and show it as real values) and Save is disabled
       through syncRepoSaveGuard. */
    function renderConfigLoadError() {
      const body = document.getElementById('config-body');
      if (!body) return;
      const expired = _configState.loadError === 'session';
      const detail = expired ? SESSION_EXPIRED_MESSAGE : escapeHtml(_configState.loadErrorDetail || 'unknown error');
      body.innerHTML = '<div class="config-load-error" style="text-align:center;padding:40px">' +
        '<div style="color:var(--red);font-weight:600">Could not load configuration — not showing values</div>' +
        '<div style="color:var(--muted);margin-top:var(--sp-4);font-size:0.8rem">' + detail + '</div>' +
        '<div class="mt-6">' +
        (expired
          ? '<button class="hv-btn btn-secondary btn" data-action="signInAgain">Sign in</button>'
          : '<button class="hv-btn btn-secondary btn" data-action="retryConfigLoad">Retry</button>') +
        '</div></div>';
      if (typeof syncRepoSaveGuard === 'function') syncRepoSaveGuard();
    }

    function retryConfigLoad() {
      if (!_configModalOpen || _configState.isCreate || !_configState.type) return;
      openConfigDialog(_configState.type, _configState.agent, _configState.initialTabOverride);
    }

    function renderConfigTab(tabId) {
      if (_configState.type === 'governor' && _settingsSearch.active) { renderSettingsSearchResults(_settingsSearch.query); return; }
      const body = document.getElementById('config-body');
      // A tab click after a failed load must not render the placeholder data
      // as if it were the agent's config (#7405).
      if (_configState.loadError) { renderConfigLoadError(); return; }
      if (_configState.type === 'agent') {
        body.innerHTML = renderAgentTab(tabId);
        // The Prompts tab renders a shell synchronously, then loads this
        // agent's kick history over the selected time window.
        if (tabId === AGENT_PROMPTS_TAB) initAgentPromptHistory();
      } else {
        body.innerHTML = renderGovernorTab(tabId);
        settingsAnnotateRows(body);
        if (tabId === 'Hub') { fetchQueueCount(); fetchAutoUpdateStatus(); }
        // The escalation breaker lives at the top level of hive.yaml, so the
        // Health tab loads it from its own endpoint.
        if (tabId === 'Health') { loadEscalationConfig(); loadWatchdogActivity(); }
        // Opening the Model Gateways tab loads the live gateway list and
        // renders the manager into its container.
        if (tabId === 'Model Gateways') {
          loadGateways();
          loadInferenceAuth();
        }
        // The Forge App tab renders a shell synchronously, then loads the
        // credential inventory (fingerprints only) from the spoke.
        if (tabId === 'Forge App') loadForgeApps();
        // The Features tab's Linear Agent panel loads install/session state
        // from its own endpoint (owner-only; the render is a static shell).
        if (tabId === 'Features') loadLinearAgentStatus();
        // The same tab carries the upstream-watch editor and divergence view
        // (#9969, #11211); it loads from its own endpoint.
        if (tabId === 'Features') loadUpstreamWatch();
        if (tabId === 'Features') loadEventTriggers();
        // Review Gate → Reviewers: the eligibility summary and engine warning
        // are derived from the rendered rows and the fan-out/combined switches.
        if (tabId === 'Features') refreshReviewerAgentSummary();
        if (tabId === 'Appearance') loadThemeConfig();
        if (tabId === 'Knowledge') { renderPublicKnowledgeControls(); loadKnowledgeConnectors(); loadKnowledgePipeline(); }
        if (tabId === 'Compliance') loadComplianceStatus();
      }
      // Keep the Save button's enabled state in lockstep with the repos/default
      // invariant on every render (requirement #4). Cheap and idempotent — a
      // no-op on tabs/dialogs without a repos list.
      if (typeof syncRepoSaveGuard === 'function') syncRepoSaveGuard();
    }

    function renderAgentImport() {
      var exampleURL = 'https://github.com/hivecommons/hive/blob/HEAD/src/examples/agents/customized-agent.yaml';
      var rawURL = 'https://raw.githubusercontent.com/hivecommons/hive/HEAD/src/examples/agents/customized-agent.yaml';
      // Field-by-field reference on the docs site (documents the same v2 schema
      // as the example, and explains the ref+path "v2/src" URL). See #1929/#1930.
      var docsURL = 'https://docs.kubestellar.io/hive/agent-definition-yaml';
      var quickStarts = AGENT_QUICK_START_TEMPLATES.map(function(t) {
        return '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="applyAgentQuickStart" data-arg0="' + esc(t.id) + '" title="' + esc(t.description) + '">' + esc(t.emoji) + ' ' + esc(t.displayName) + '</button>';
      }).join('');
      return '<div style="max-width:600px;margin:0 auto">' +
        '<div style="background:color-mix(in srgb, var(--accent) 7%, transparent);border:1px solid color-mix(in srgb, var(--accent) 22%, transparent);border-radius:var(--r);padding:var(--sp-5);margin-bottom:var(--sp-6)">' +
          '<div style="font-weight:700;color:var(--text);margin-bottom:var(--sp-2)">Start with a template, then tune it</div>' +
          '<p class="fs-base text-muted" style="margin:var(--sp-0) var(--sp-0) var(--sp-4)">Pick a common agent role to prefill the create form, or import/paste a full definition below.</p>' +
          '<div style="display:flex;flex-wrap:wrap;gap:var(--sp-2)">' + quickStarts + '</div>' +
        '</div>' +
        '<p class="fs-base text-muted mb-6">Import an agent from a YAML definition file. <a class="text-accent" href="' + docsURL + '" target="_blank" rel="noopener">Field reference ↗</a> · <a class="text-accent" href="' + exampleURL + '" target="_blank" rel="noopener">example definition ↗</a></p>' +
        '<div class="config-field mb-5">' +
          '<label>Import from URL</label>' +
          '<div style="display:flex;gap:var(--sp-4)"><input type="text" id="import-url-input" placeholder="' + rawURL + '" style="flex:1"><button class="hv-btn btn-primary" data-action="previewImportFromURL">Preview</button></div>' +
          '<label style="display:flex;align-items:center;gap:var(--sp-4);margin-top:var(--sp-4);font-size:0.78rem;cursor:pointer;font-weight:400">' +
            '<input type="checkbox" id="import-keep-linked" style="width:auto;margin:var(--sp-0)">' +
            '<span>Keep linked to repo (live) <span class="text-muted">— re-fetch this definition on reload so repo edits propagate. Keep-linked URL imports support GitHub blob URLs and raw.githubusercontent.com file URLs only; gist URLs are one-shot imports, so uncheck this box for gists. Only presentation/behavior fields are re-applied; security and privilege fields are never changed. The repo must be on the operator\'s allowlist. (URL import only.)</span></span>' +
          '</label>' +
        '</div>' +
        '<div class="config-field mb-5">' +
          '<label>Or paste agent definition</label>' +
          '<textarea id="import-paste-input" rows="20" style="width:100%;font-family:var(--font-mono);font-size:var(--fs-sm);resize:vertical;min-height:300px;overscroll-behavior:contain;overscroll-behavior-y:contain" placeholder="apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\n..."></textarea>' +
          '<div style="text-align:right;margin-top:var(--sp-3)"><button class="hv-btn btn-primary" data-action="previewImportFromPaste">Preview</button></div>' +
        '</div>' +
        '<div id="import-preview" style="display:none;background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:var(--sp-6);margin-top:var(--sp-6)">' +
          '<div style="font-weight:600;margin-bottom:var(--sp-5)">Preview</div>' +
          '<div id="import-preview-content"></div>' +
          '<div style="display:flex;justify-content:flex-end;gap:var(--sp-4);margin-top:var(--sp-6)">' +
            '<button class="hv-btn btn-icon btn-sm" data-action="gh30">Cancel</button>' +
            '<button class="hv-btn btn-primary" data-action="confirmImport">Create Agent</button>' +
          '</div>' +
        '</div>' +
      '</div>';
    }

    var _pendingImportData = null;

    function applyAgentQuickStart(templateID) {
      var t = AGENT_QUICK_START_TEMPLATES.find(function(x) { return x.id === templateID; });
      if (!t || !_configState.isCreate) return;
      if (!_configState.data.general) _configState.data.general = {};
      Object.assign(_configState.data.general, {
        displayName: t.displayName,
        description: t.description,
        emoji: t.emoji,
        role: t.role,
        kickTemplate: t.kickTemplate,
        cliPinValue: t.backend,
        cliPinned: true,
        launchCmd: 'agent-launch.sh --backend ' + t.backend,
      });
      markDirty('_create', 'name', t.name);
      markDirty('general', 'displayName', t.displayName);
      markDirty('general', 'description', t.description);
      markDirty('general', 'emoji', t.emoji);
      markDirty('general', 'role', t.role);
      markDirty('general', 'kickTemplate', t.kickTemplate);
      markDirty('general', 'cliPinValue', t.backend);
      markDirty('general', 'cliPinned', true);
      markDirty('general', 'launchCmd', 'agent-launch.sh --backend ' + t.backend);
      switchConfigTab('General');
      var nameInput = document.getElementById('cfg-create-name');
      if (nameInput) nameInput.value = t.name;
      updateConfigAgentName(t.displayName);
      showToast(t.displayName + ' template loaded — review the fields, then save', 'success');
    }

    async function previewImportFromURL() {
      var url = (document.getElementById('import-url-input') || {}).value;
      if (!url) { showToast('Enter a URL', 'error'); return; }
      // Keep-linked persists the ORIGINAL (blob) URL as the source pointer, but
      // the preview/bake fetch uses the raw URL. The server derives owner/repo/
      // path/ref from whichever URL it receives, so keep the raw form here and
      // let the server round-trip it back in _importBody.
      var keepLinked = !!(document.getElementById('import-keep-linked') || {}).checked;
      // Org migration: rewrite blob->raw for BOTH the new (hivecommons) org and
      // the legacy (kubestellar) one, so pre-transfer links pasted from old
      // docs/bookmarks still import; raw.githubusercontent follows the repo
      // transfer for the old spelling.
      url = url.replace('github.com/hivecommons/hive/blob/', 'raw.githubusercontent.com/hivecommons/hive/');
      url = url.replace('github.com/kubestellar/hive/blob/', 'raw.githubusercontent.com/kubestellar/hive/');
      try {
        var resp = await fetch('/api/agents/import', {method:'POST', headers:_inceptionAuthHeaders(), body:JSON.stringify({source:'url',url:url,preview:true,keepLinked:keepLinked})});
        var d = await resp.json();
        if (!resp.ok) { showToast(d.error || 'Import failed', 'error'); return; }
        showImportPreview(d);
      } catch(e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function previewImportFromPaste() {
      var content = (document.getElementById('import-paste-input') || {}).value;
      if (!content) { showToast('Paste a definition', 'error'); return; }
      try {
        var resp = await fetch('/api/agents/import', {method:'POST', headers:_inceptionAuthHeaders(), body:JSON.stringify({source:'paste',content:content,preview:true})});
        var d = await resp.json();
        if (!resp.ok) { showToast(d.error || 'Import failed', 'error'); return; }
        showImportPreview(d);
      } catch(e) { showToast('Error: ' + e.message, 'error'); }
    }

    function showImportPreview(d) {
      _pendingImportData = d;
      var p = d.parsed || {};
      var meta = p.metadata || {};
      var spec = p.spec || {};
      var html = '<table style="width:100%;font-size:0.8rem;border-collapse:collapse">';
      html += '<tr><td class="py-2-px-4 text-muted">Name</td><td style="padding:var(--sp-2) var(--sp-4);font-weight:600">' + (meta.emoji || '') + ' ' + (meta.name || '?') + '</td></tr>';
      html += '<tr><td class="py-2-px-4 text-muted">Display</td><td class="py-2-px-4">' + (meta.displayName || '') + '</td></tr>';
      html += '<tr><td class="py-2-px-4 text-muted">Backend</td><td class="py-2-px-4">' + (spec.backend || '?') + ' / ' + (spec.model || '?') + '</td></tr>';
      html += '<tr><td class="py-2-px-4 text-muted">Mode</td><td class="py-2-px-4">' + (spec.mode || '?') + '</td></tr>';
      html += '<tr><td class="py-2-px-4 text-muted">Role</td><td class="py-2-px-4">' + (spec.role || '') + '</td></tr>';
      if (spec.promptTemplate) html += '<tr><td class="py-2-px-4 text-muted">Template</td><td style="padding:var(--sp-2) var(--sp-4);font-size:0.7rem;color:var(--muted)">' + spec.promptTemplate.substring(0,120) + '...</td></tr>';
      if (spec.channels && spec.channels.length) html += '<tr><td class="py-2-px-4 text-muted">Channels</td><td class="py-2-px-4">' + spec.channels.map(function(c){return c.type}).join(', ') + '</td></tr>';
      if (spec.tools) html += '<tr><td class="py-2-px-4 text-muted">Tools</td><td class="py-2-px-4">' + (spec.tools.preset || 'custom rules') + '</td></tr>';
      if (spec.connections && spec.connections.length) html += '<tr><td class="py-2-px-4 text-muted">Connections</td><td class="py-2-px-4">' + spec.connections.map(function(c){return c.name}).join(', ') + '</td></tr>';
      html += '</table>';
      document.getElementById('import-preview-content').innerHTML = html;
      document.getElementById('import-preview').style.display = 'block';
    }

    async function confirmImport() {
      if (!_pendingImportData) return;
      try {
        var body = _pendingImportData._importBody || {};
        body.preview = false;
        var resp = await fetch('/api/agents/import', {method:'POST', headers:_inceptionAuthHeaders(), body:JSON.stringify(body)});
        var d = await resp.json();
        if (!resp.ok) { showToast(d.error || 'Import failed', 'error'); return; }
        showToast('Agent "' + (d.name || '?') + '" created!', 'success');
        closeConfigDialog();
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch(e) { showToast('Error: ' + e.message, 'error'); }
    }

    function renderAgentTab(tab) {
      const d = _configState.data;
      switch (tab) {
        case 'Import': return renderAgentImport();
        case 'General': return renderAgentGeneral(d.general || {});
        case 'Cadences': return renderAgentCadences(d.cadences || {});
        case 'Pipeline': return renderAgentPipeline(d.pipeline || {});
        case 'Hooks': return renderAgentHooks(d.hooks || {});
        case 'Restrictions': return renderAgentRestrictions(d.restrictions || {});
        case 'Channels': return renderAgentChannels(d.channels || []);
        case 'Tools': return renderAgentTools(d.tools || null);
        case 'Connections': return renderAgentConnections(d.connections || []);
        case 'Variables': return renderAgentVariables();
        case 'Stats': return renderAgentStatsTab(d.stats || []);
        case 'Prompt Template': return renderAgentPromptTemplate();
        case AGENT_PROMPTS_TAB: return renderAgentPromptHistoryShell();
        case 'Export': return renderAgentExport();
        default: return '';
      }
    }

    function renderGovernorTab(tab) {
      const d = _configState.data;
      switch (tab) {
        case 'General': return renderGovGeneral();
        case 'Appearance': return renderGovAppearance();
        case 'Dashboard': return renderGovDashboard(d.dashboard || {});
        case 'Agents': return renderGovAgents(d.agents || []);
        case 'Thresholds': return renderGovThresholds(d) + renderGovSLATargets(d);
        case 'Labels': return renderGovLabels(d.labels || [], d.holdLabels || [], d.requireLabels || []);
        case 'Repos': return renderGovRepos(d.repos || []);
        case 'Budget': return renderGovBudget(d.budget || {}, d.health || {});
        case 'Notifications': return renderGovNotifications(d.notifications || {});
        case 'Health': return renderGovHealth(d.health || {});
        case 'Sensing': return renderGovSensing(d.sensing || {});
        case 'Logging': return renderGovLogging(d.logging || {});
        case 'Security': return renderGovSecurity(d.security || {});
        case 'Access': return renderGovSecurity(d.security || {});
        case 'Features': return renderGovFeatures(d.features || {}, d.auto_merge || {}, d.review || {});
        case 'Knowledge': return renderGovKnowledgeSharing();
        case 'Project Observability': return renderGovProjectObservability(d.project_observability || {});
        case 'Advisory': return renderGovAdvisory(d.advisory || {});
        case 'Work Source': return renderGovWorkSource(d.work_source || {});
        case 'Hub': return renderGovHub(d.hub || {});
        case 'Forge App': return renderGovForgeApp();
        case 'Model Gateways': return renderGovGateways();
        case GOVERNOR_BOB_TAB: return renderGovBob();
        case 'Variables': return renderGovVariables();
        case 'Compliance': return renderGovCompliance();
        default: return '';
      }
    }

    function renderGovKnowledgeSharing() {
      return '<div class="config-section">' +
        '<h3>Knowledge sharing</h3>' +
        '<p class="text-muted fs-base">Control whether the read-only MCP endpoint can serve operational knowledge facts without dashboard credentials. ' + yamlHint('knowledge.public.enabled') + ' Applies without a restart.</p>' +
        '<div id="public-knowledge-settings-control" data-public-knowledge-control="settings">' + renderPublicKnowledgeControl('settings') + '</div>' +
      '</div>' +
        renderKnowledgePipelineSection() +
        renderKnowledgeConnectorsSection();
    }

    // ---- Settings → Knowledge → Connectors (#11075) ----
    const KB_CONNECTOR_TYPE_ORDER = ['confluence', 'context7', 'document', 'git', 'github-wiki', 'google-drive', 'notion', 'repo-wiki', 'sharepoint'];
    const KC_TYPE_INFO = {
      'confluence': { label: 'Confluence', description: 'Sync Confluence Cloud or Data Center pages into knowledge facts.' },
      'context7': { label: 'Context7', description: 'Sync documentation for default Context7 libraries. The API key is a reference to an environment variable or secret file; the value is never shown.' },
      'document': { label: 'Document', description: 'Import one URL, local file path, or Context7 library document as facts.' },
      'git': { label: 'Git repository', description: 'Clone and index markdown from a Git repository or subdirectory.' },
      'github-wiki': { label: 'GitHub Wiki', description: 'Clone and index one or more GitHub wiki repositories.' },
      'google-drive': { label: 'Google Drive', description: 'Sync Google Docs, Sheets, markdown, and text files from Drive folders or shared drives.' },
      'notion': { label: 'Notion', description: 'Sync Notion pages and database rows visible to an internal integration.' },
      'repo-wiki': { label: 'Repo wiki', description: 'Sync .hive/wiki (or another directory) from one or more GitHub repositories.' },
      'sharepoint': { label: 'SharePoint / OneDrive', description: 'Sync markdown, text, and HTML files from SharePoint document libraries or OneDrive drives.' }
    };
    const KC_TYPE_ICONS = {
      // Vendor marks: simple-icons (CC0 1.0) paths, 24x24 viewBox, rendered with currentColor.
      'obsidian': 'M19.355 18.538a68.967 68.959 0 0 0 1.858-2.954.81.81 0 0 0-.062-.9c-.516-.685-1.504-2.075-2.042-3.362-.553-1.321-.636-3.375-.64-4.377a1.707 1.707 0 0 0-.358-1.05l-3.198-4.064a3.744 3.744 0 0 1-.076.543c-.106.503-.307 1.004-.536 1.5-.134.29-.29.6-.446.914l-.31.626c-.516 1.068-.997 2.227-1.132 3.59-.124 1.26.046 2.73.815 4.481.128.011.257.025.386.044a6.363 6.363 0 0 1 3.326 1.505c.916.79 1.744 1.922 2.415 3.5zM8.199 22.569c.073.012.146.02.22.02.78.024 2.095.092 3.16.29.87.16 2.593.64 4.01 1.055 1.083.316 2.198-.548 2.355-1.664.114-.814.33-1.735.725-2.58l-.01.005c-.67-1.87-1.522-3.078-2.416-3.849a5.295 5.295 0 0 0-2.778-1.257c-1.54-.216-2.952.19-3.84.45.532 2.218.368 4.829-1.425 7.531zM5.533 9.938c-.023.1-.056.197-.098.29L2.82 16.059a1.602 1.602 0 0 0 .313 1.772l4.116 4.24c2.103-3.101 1.796-6.02.836-8.3-.728-1.73-1.832-3.081-2.55-3.831zM9.32 14.01c.615-.183 1.606-.465 2.745-.534-.683-1.725-.848-3.233-.716-4.577.154-1.552.7-2.847 1.235-3.95.113-.235.223-.454.328-.664.149-.297.288-.577.419-.86.217-.47.379-.885.46-1.27.08-.38.08-.72-.014-1.043-.095-.325-.297-.675-.68-1.06a1.6 1.6 0 0 0-1.475.36l-4.95 4.452a1.602 1.602 0 0 0-.513.952l-.427 2.83c.672.59 2.328 2.316 3.335 4.711.09.21.175.43.253.653z',
      'confluence': 'M.87 18.257c-.248.382-.53.875-.763 1.245a.764.764 0 0 0 .255 1.04l4.965 3.054a.764.764 0 0 0 1.058-.26c.199-.332.454-.763.733-1.221 1.967-3.247 3.945-2.853 7.508-1.146l4.957 2.337a.764.764 0 0 0 1.028-.382l2.364-5.346a.764.764 0 0 0-.382-1 599.851 599.851 0 0 1-4.965-2.361C10.911 10.97 5.224 11.185.87 18.257zM23.131 5.743c.249-.405.531-.875.764-1.25a.764.764 0 0 0-.256-1.034L18.675.404a.764.764 0 0 0-1.058.26c-.195.335-.451.763-.734 1.225-1.966 3.246-3.945 2.85-7.508 1.146L4.437.694a.764.764 0 0 0-1.027.382L1.046 6.422a.764.764 0 0 0 .382 1c1.039.49 3.105 1.467 4.965 2.361 6.698 3.246 12.392 3.029 16.738-4.04z',
      'git': 'M13.09 23.549a1.54 1.54 0 0 1-2.18 0L.451 13.089a1.54 1.54 0 0 1 0-2.179l7.191-7.19 2.733 2.733a1.85 1.85 0 0 0 .964 2.326v6.66a1.849 1.849 0 1 0 1.54 0V8.957l2.508 2.508a1.85 1.85 0 1 0 1.09-1.09l-2.634-2.634a1.85 1.85 0 0 0-2.378-2.377L8.73 2.63 10.91.451a1.54 1.54 0 0 1 2.179 0l10.459 10.46a1.54 1.54 0 0 1 0 2.179z',
      'github-wiki': 'M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12',
      'google-drive': 'M12.01 1.485c-2.082 0-3.754.02-3.743.047.01.02 1.708 3.001 3.774 6.62l3.76 6.574h3.76c2.081 0 3.753-.02 3.742-.047-.005-.02-1.708-3.001-3.775-6.62l-3.76-6.574zm-4.76 1.73a789.828 789.861 0 0 0-3.63 6.319L0 15.868l1.89 3.298 1.885 3.297 3.62-6.335 3.618-6.33-1.88-3.287C8.1 4.704 7.255 3.22 7.25 3.214zm2.259 12.653-.203.348c-.114.198-.96 1.672-1.88 3.287a423.93 423.948 0 0 1-1.698 2.97c-.01.026 3.24.042 7.222.042h7.244l1.796-3.157c.992-1.734 1.85-3.23 1.906-3.323l.104-.167h-7.249z',
      'notion': 'M4.459 4.208c.746.606 1.026.56 2.428.466l13.215-.793c.28 0 .047-.28-.046-.326L17.86 1.968c-.42-.326-.981-.7-2.055-.607L3.01 2.295c-.466.046-.56.28-.374.466zm.793 3.08v13.904c0 .747.373 1.027 1.214.98l14.523-.84c.841-.046.935-.56.935-1.167V6.354c0-.606-.233-.933-.748-.887l-15.177.887c-.56.047-.747.327-.747.933zm14.337.745c.093.42 0 .84-.42.888l-.7.14v10.264c-.608.327-1.168.514-1.635.514-.748 0-.935-.234-1.495-.933l-4.577-7.186v6.952L12.21 19s0 .84-1.168.84l-3.222.186c-.093-.186 0-.653.327-.746l.84-.233V9.854L7.822 9.76c-.094-.42.14-1.026.793-1.073l3.456-.233 4.764 7.279v-6.44l-1.215-.139c-.093-.514.28-.887.747-.933zM1.936 1.035l13.31-.98c1.634-.14 2.055-.047 3.082.7l4.249 2.986c.7.513.934.653.934 1.213v16.378c0 1.026-.373 1.634-1.68 1.726l-15.458.934c-.98.047-1.448-.093-1.962-.747l-3.129-4.06c-.56-.747-.793-1.306-.793-1.96V2.667c0-.839.374-1.54 1.447-1.632z',
      'sharepoint': 'M24 13.5q0 1.242-.475 2.332-.474 1.09-1.289 1.904-.814.815-1.904 1.29-1.09.474-2.332.474-.762 0-1.523-.2-.106.997-.557 1.858-.451.862-1.154 1.494-.704.633-1.606.99-.902.358-1.91.358-1.09 0-2.045-.416-.955-.416-1.664-1.125-.709-.709-1.125-1.664Q6 19.84 6 18.75q0-.188.018-.375.017-.188.04-.375H.997q-.41 0-.703-.293T0 17.004V6.996q0-.41.293-.703T.996 6h3.54q.14-1.277.726-2.373.586-1.096 1.488-1.904Q7.652.914 8.807.457 9.96 0 11.25 0q1.395 0 2.625.533T16.02 1.98q.914.915 1.447 2.145T18 6.75q0 .188-.012.375-.011.188-.035.375 1.242 0 2.344.469 1.101.468 1.928 1.277.826.809 1.3 1.904Q24 12.246 24 13.5zm-12.75-12q-.973 0-1.857.34-.885.34-1.577.943-.691.604-1.154 1.43Q6.2 5.039 6.06 6h4.945q.41 0 .703.293t.293.703v4.945l.21-.035q.212-.75.61-1.424.399-.673.944-1.218.545-.545 1.213-.944.668-.398 1.43-.61.093-.503.093-.96 0-1.09-.416-2.045-.416-.955-1.125-1.664-.709-.709-1.664-1.125Q12.34 1.5 11.25 1.5zM6.117 15.902q.54 0 1.06-.111.522-.111.932-.37.41-.257.662-.679.252-.422.252-1.055 0-.632-.263-1.054-.264-.422-.662-.703-.399-.282-.856-.463l-.855-.34q-.399-.158-.662-.334-.264-.176-.264-.445 0-.2.14-.323.141-.123.335-.193.193-.07.404-.094.21-.023.351-.023.598 0 1.055.152.457.153.95.457V8.543q-.282-.082-.522-.14-.24-.06-.475-.1-.234-.041-.486-.059-.252-.017-.557-.017-.515 0-1.054.117-.54.117-.979.375-.44.258-.715.68-.275.421-.275 1.03 0 .598.263.997.264.398.663.68.398.28.855.474l.856.363q.398.17.662.358.263.187.263.457 0 .222-.123.351-.123.13-.31.2-.188.07-.393.087-.205.018-.369.018-.703 0-1.248-.234-.545-.235-1.107-.621v1.875q1.195.468 2.472.468zM11.25 22.5q.773 0 1.453-.293t1.19-.803q.51-.51.808-1.195.299-.686.299-1.459 0-.668-.223-1.277-.222-.61-.62-1.096-.4-.486-.95-.826-.55-.34-1.207-.48v1.933q0 .41-.293.703t-.703.293H7.57q-.07.375-.07.75 0 .773.293 1.459t.803 1.195q.51.51 1.195.803.686.293 1.459.293zM18 18q.926 0 1.746-.352.82-.351 1.436-.966.615-.616.966-1.43.352-.815.352-1.752 0-.926-.352-1.746-.351-.82-.966-1.436-.616-.615-1.436-.966Q18.926 9 18 9t-1.74.357q-.815.358-1.43.973t-.973 1.43q-.357.814-.357 1.74 0 .129.006.258t.017.258q.551.27 1.02.65t.838.855q.369.475.627 1.026.258.55.387 1.148Q17.18 18 18 18Z',
      // Generic glyphs (no vendor): document sheet and open book, drawn in-house.
      'context7': 'M4 4h3v16H4zm5 0h3v16H9zm5.2.6 2.9-.8 3.8 15.5-2.9.8z',
      'document': 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zm4 18H6V4h7v5h5zM8 12h8v1.6H8zm0 3.2h8v1.6H8zm0 3.2h5V20H8z',
      'repo-wiki': 'M4 3h7.2c1.1 0 2 .55 2.8 1.3.8-.75 1.7-1.3 2.8-1.3H20v16h-3.2c-1 0-1.95.4-2.8 1.1V22h-4v-1.9c-.85-.7-1.8-1.1-2.8-1.1H4zm2 2v12h1.2c1.3 0 2.5.4 3.6 1.1V6.2C10.4 5.55 9.6 5 8.8 5zm12 0h-1.2c-.8 0-1.6.55-2 1.2v12.1c1.1-.7 2.3-1.1 3.6-1.1H18z'
    };
    function kbIntegrationIcon(type) {
      const d = KC_TYPE_ICONS[type];
      if (!d) return '<span class="kb-integration-icon" aria-hidden="true">🔌</span>';
      return '<svg class="kb-integration-icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false" data-kb-icon="' + escapeHtml(type) + '"><path fill="currentColor" d="' + d + '"/></svg>';
    }
    const KC_SCOPE_FIELDS = {
      'confluence': [['base_url', 'Base URL', 'https://acme.atlassian.net/wiki'], ['deployment', 'Deployment', 'cloud or datacenter'], ['email', 'Cloud account email', 'hive-bot@acme.com'], ['spaces', 'Spaces', 'ENG, OPS'], ['root_page_ids', 'Root page IDs', '12345, 67890'], ['include_labels', 'Include labels', 'runbook,decision'], ['exclude_labels', 'Exclude labels', 'draft'], ['include_attachments', 'Include attachments', 'false'], ['max_pages', 'Max pages', '2000'], ['full_sync_every', 'Full sync every', '10']],
      'context7': [['library_ids', 'Default library IDs (comma-separated)', '/vercel/next.js, /golang/go'], ['topic', 'Topic (optional)', 'routing']],
      'document': [['url', 'Document URL', 'https://example.org/guide.html'], ['file_path', 'File path', '/data/knowledge/guide.pdf'], ['context7_id', 'Context7 ID', '/org/project']],
      'git': [['url', 'Repository URL', 'https://github.com/org/repo'], ['branch', 'Branch', 'main'], ['subpath', 'Subpath', 'docs/']],
      'github-wiki': [['repos', 'Repositories (comma-separated)', 'org/repo-a, org/repo-b'], ['branch', 'Branch', 'master']],
      'google-drive': [['folder_ids', 'Folder IDs', 'folder123, folder456'], ['shared_drive_ids', 'Shared drive IDs', 'drive123'], ['include_mime', 'Include MIME groups', 'docs,md,txt']],
      'notion': [['root_page_ids', 'Root page IDs', '1a2b3c4d5e6f47a8b9c0d1e2f3a4b5c6'], ['database_ids', 'Database IDs', '0f1e2d3c-4b5a-4968-8776-655443322110'], ['include_archived', 'Include archived', 'false'], ['max_pages', 'Max pages', '2000'], ['full_sync_every', 'Full sync every', '10']],
      'repo-wiki': [['repos', 'Repositories (comma-separated)', 'org/repo-a, org/repo-b'], ['branch', 'Branch', 'v5'], ['dir', 'Wiki directory', '.hive/wiki']],
      'sharepoint': [['drive_ids', 'Drive IDs', 'b!drive-id'], ['site_ids', 'Site IDs', 'contoso.sharepoint.com,site-id,web-id'], ['folder_path', 'Folder path', '/Docs/Sub'], ['include_files', 'Include file extensions', '.md,.txt,.html']]
    };
    let _kcState = { loaded: false, error: '', connectors: [], rows: [], types: [], layers: ['personal', 'project', 'org', 'community'], runtime: false, publish: null };

    async function loadKnowledgeConnectors() {
      try {
        const [cfgRes, stRes] = await Promise.all([fetch('/api/config/knowledge/connectors'), fetch('/api/config/knowledge/connectors/status')]);
        const cfg = await cfgRes.json().catch(() => ({}));
        if (!cfgRes.ok) throw new Error(cfg.error || ('HTTP ' + cfgRes.status));
        const st = stRes.ok ? await stRes.json().catch(() => ({})) : {};
        _kcState = Object.assign(_kcState, { loaded: true, error: '', connectors: cfg.connectors || [], types: cfg.types || [], ctx7AutoImport: cfg.context7_auto_import || [], layers: cfg.layers || _kcState.layers, rows: st.connectors || [], runtime: !!st.runtime, publish: st.publish || null });
      } catch (e) {
        _kcState.loaded = true;
        _kcState.error = e.message;
      }
      renderKnowledgeConnectorsInto();
    }

    function renderKnowledgeConnectorsInto() {
      const el = document.getElementById('kc-connectors-root');
      if (el) el.innerHTML = renderKnowledgeConnectorsBody();
    }

    function kcAgo(iso) {
      if (!iso) return '—';
      const t = Date.parse(iso);
      if (isNaN(t)) return '—';
      const s = Math.max(0, Math.round((Date.now() - t) / 1000));
      if (s < 60) return s + 's ago';
      if (s < 3600) return Math.round(s / 60) + 'm ago';
      if (s < 86400) return Math.round(s / 3600) + 'h ago';
      return Math.round(s / 86400) + 'd ago';
    }

    function renderKnowledgeConnectorsBody() {
      if (!_kcState.loaded) return '<p class="text-muted fs-base">Loading connectors…</p>';
      if (_kcState.error) return '<p class="kc-error">Failed to load connectors: ' + esc(_kcState.error) + '</p>';
      const owner = viewerIsOwner();
      const byName = {};
      (_kcState.rows || []).forEach(r => { byName[r.name] = r; });
      let html = '';
      if (owner) html += '<div class="kc-toolbar"><button type="button" class="hv-btn btn-primary btn-sm" data-action="kcOpenEditor" data-arg0="">Add connector</button></div>';
      if (!_kcState.connectors.length) {
        return html + '<p class="text-muted fs-base">No connectors configured.' + (owner ? '' : ' Only the hive owner can add connectors.') + '</p>' + renderKnowledgePublishBlock(owner);
      }
      html += '<table class="kc-table"><thead><tr><th>Name</th><th>Type</th><th>Layer</th><th>Status</th><th>Last sync</th><th>Pages / facts</th><th>Last error</th>' + (owner ? '<th>Actions</th>' : '') + '</tr></thead><tbody>';
      _kcState.connectors.forEach(c => {
        const r = byName[c.name] || {};
        const status = r.status || (c.enabled ? 'pending' : 'disabled');
        const err = r.last_error || '';
        const n = esc(c.name);
        html += '<tr data-connector="' + n + '"><td><code>' + n + '</code></td><td>' + esc(c.type) + '</td><td>' + esc(c.layer) + '</td>'
          + '<td><span class="kc-pill" data-status="' + esc(status) + '">' + esc(status) + '</span></td>'
          + '<td title="' + esc(r.last_sync || '') + '">' + esc(kcAgo(r.last_sync)) + '</td>'
          + '<td>' + (r.pages || 0) + ' / ' + (r.facts || 0) + '</td>'
          + '<td class="kc-error-cell" title="' + esc(err) + '">' + (err ? esc(err.length > 80 ? err.slice(0, 80) + '…' : err) : '—') + '</td>';
        if (owner) {
          html += '<td class="kc-actions">'
            + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kcSyncNow" data-arg0="' + n + '"' + (status === 'syncing' ? ' disabled' : '') + '>Sync now</button>'
            + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kcOpenEditor" data-arg0="' + n + '">Edit</button>'
            + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kcToggleEnabled" data-arg0="' + n + '">' + (c.enabled ? 'Disable' : 'Enable') + '</button>'
            + '<button type="button" class="hv-btn btn-danger btn-sm" data-action="kcRemove" data-arg0="' + n + '">Remove</button></td>';
        }
        html += '</tr>';
      });
      html += '</tbody></table>';
      if (!_kcState.runtime) html += '<p class="text-muted fs-base">The connector syncer is not running, so status and Sync now are unavailable until the hive restarts with these connectors.</p>';
      return html + renderKnowledgePublishBlock(owner);
    }

    // Publish mirror (knowledge.publish, #11076): read-only status plus an
    // owner-only "Publish now". Hidden when publishing is not configured.
    function renderKnowledgePublishBlock(owner) {
      const p = _kcState.publish;
      if (!p) return '';
      const status = !p.active ? 'disabled' : p.running ? 'syncing' : p.last_error ? 'error' : p.last_success ? 'ok' : 'pending';
      const err = p.last_error || '';
      const row = (k, v) => '<dt>' + esc(k) + '</dt><dd>' + v + '</dd>';
      let html = '<div class="kc-publish" data-section="knowledge-publish"><div class="kc-publish-head"><h4>Publish mirror</h4>';
      if (owner) html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="kcPublishNow"' + (!p.active || p.running ? ' disabled' : '') + '>Publish now</button>';
      html += '</div><dl class="kc-publish-grid">'
        + row('Status', '<span class="kc-pill" data-status="' + esc(status) + '">' + esc(status) + '</span>' + (p.dry_run ? ' <span class="text-muted">dry run</span>' : ''))
        + row('Connector', '<code>' + esc(p.connector || '') + '</code>')
        + row('Layers', esc((p.layers || []).join(', ') || '—'))
        + row('Root', esc(p.root || '—'))
        + row('Last run', '<span title="' + esc(p.last_run || '') + '">' + esc(kcAgo(p.last_run)) + '</span>')
        + row('Pages', (p.pages || 0) + ' published · last batch: ' + (p.pages_created || 0) + ' created, ' + (p.pages_updated || 0) + ' updated, ' + (p.pages_deprecated || 0) + ' deprecated, ' + (p.pages_unchanged || 0) + ' unchanged')
        + row('Collisions', (p.collisions || []).length ? esc(p.collisions.join('; ')) : '—')
        + row('Last error', err ? '<span class="kc-error" title="' + esc(err) + '">' + esc(err.length > 120 ? err.slice(0, 120) + '…' : err) + '</span>' : '—')
        + '</dl>';
      if (!p.active) html += '<p class="text-muted fs-base">The publish mirror is not running. Check the hive log for "knowledge publish mirror disabled", or restart the hive after configuring knowledge.publish.</p>';
      return html + '</div>';
    }

    function renderKnowledgeConnectorsSection() {
      return '<div class="config-section" data-section="knowledge-connectors">'
        + '<h3>Connectors</h3>'
        + '<p class="text-muted fs-base">External knowledge systems synced into the vault. Credentials are referenced by environment variable name or secret file path, never stored here.</p>'
        + '<div id="kc-connectors-root">' + renderKnowledgeConnectorsBody() + '</div></div>';
    }

    // ---- Settings → Knowledge → Knowledge pipeline ----
    // The scalar knowledge.bead_synthesizer, curator, code_maps and primer
    // keys, saved through PUT /api/config/knowledge/pipeline. The bead
    // synthesizer switch applies at once; everything else is read at boot, so
    // it applies after a restart. The engine, vault paths (directories the
    // hive writes to) and primer merge strategy are display-only.
    let _kpState = { loaded: false, error: '', data: {} };

    async function loadKnowledgePipeline() {
      try {
        const r = await fetch('/api/config/knowledge/pipeline');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _kpState = { loaded: true, error: '', data: data };
      } catch (e) {
        _kpState = { loaded: true, error: e.message, data: {} };
      }
      const el = document.getElementById('kp-pipeline-root');
      if (el) el.innerHTML = renderKnowledgePipelineBody();
    }

    function renderKnowledgePipelineSection() {
      return '<div class="config-section" data-section="knowledge-pipeline">'
        + '<h3>Knowledge pipeline</h3>'
        + '<p class="text-muted fs-base">How completed beads become wiki facts, how facts are promoted between layers, generated repository code maps, and the size of the knowledge primer each kick receives.</p>'
        + '<div id="kp-pipeline-root">' + renderKnowledgePipelineBody() + '</div></div>';
    }

    function renderKnowledgePipelineBody() {
      if (!_kpState.loaded) return '<p class="text-muted fs-base">Loading knowledge pipeline settings…</p>';
      if (_kpState.error) return '<p class="kc-error">Failed to load knowledge pipeline settings: ' + esc(_kpState.error) + '</p>';
      const d = _kpState.data || {};
      const bs = d.bead_synthesizer || {};
      const rp = bs.retention_policy || {};
      const cu = d.curator || {};
      const cm = d.code_maps || {};
      const pr = d.primer || {};
      const def = d.defaults || {};
      const owner = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const dis = owner ? '' : 'disabled';
      const restart = ' Requires restart.';
      const mark = (label, tip) => settingHelpMark('settings', 'knowledge', label, tip);
      const num = (id, label, path, value, placeholder, step, max, help, tip) => `
          <div class="config-field">
            <label for="kp-${id}">${esc(label)} ${yamlHint(path)} ${mark(label, tip)}</label>
            <input id="kp-${id}" type="number" min="0" ${max ? `max="${max}"` : ''} step="${step}" value="${value ? esc(String(value)) : ''}" placeholder="${esc(String(placeholder))}" ${dis}>
            <div class="config-help">${help}${restart}</div>
          </div>`;
      const choice = (id, label, path, value, values, emptyLabel, help, tip) => `
          <div class="config-field">
            <label for="kp-${id}">${esc(label)} ${yamlHint(path)} ${mark(label, tip)}</label>
            <select id="kp-${id}" ${dis}>
              <option value="" ${value ? '' : 'selected'}>${esc(emptyLabel)}</option>
              ${values.map(v => `<option value="${esc(v)}" ${value === v ? 'selected' : ''}>${esc(v)}</option>`).join('')}
            </select>
            <div class="config-help">${help}${restart}</div>
          </div>`;
      const toggle = (id, label, path, on, help, live, tip) => `
          <div class="config-toggle mt-3">
            <div id="kp-${id}" class="config-toggle-switch ${on ? 'on' : ''} ${owner ? '' : 'disabled'}" data-action="${owner ? 'kpToggle' : ''}" data-arg-types="t" role="switch" aria-checked="${on ? 'true' : 'false'}"></div>
            <span class="config-toggle-label">${esc(label)} ${yamlHint(path)} ${mark(label, tip)} <span class="text-muted">— ${help}${live ? ' Applies without a restart.' : restart}</span></span>
          </div>`;
      const roRow = (label, path, value, fallback) => `<div class="config-field"><label>${esc(label)} ${yamlHint(path)}</label><div class="fs-sm break-all">${value ? '<code>' + esc(String(value)) + '</code>' : '<span class="text-muted">' + esc(fallback) + '</span>'} <span class="text-muted">— set in hive.yaml; requires restart</span></div></div>`;
      const pub = bs.publish || {};
      const directCommitOK = Number((window._lastStatus && window._lastStatus.acmmLevel) || 0) >= 5;
      const pubText = (id, label, path, value, placeholder, help) => `
          <div class="config-field">
            <label for="kp-${id}">${esc(label)} ${yamlHint(path)}</label>
            <input id="kp-${id}" type="text" value="${esc(String(value || ''))}" placeholder="${esc(placeholder)}" ${dis}>
            <div class="config-help">${help}${restart}</div>
          </div>`;
      const pubModes = directCommitOK ? ['pr', 'commit'] : ['pr'];
      const pubModeField = `
          <div class="config-field">
            <label for="kp-pub-mode">Mode ${yamlHint('knowledge.bead_synthesizer.publish.mode')}</label>
            <select id="kp-pub-mode" ${directCommitOK ? dis : 'disabled'}>
              ${pubModes.map(v => `<option value="${v}" ${(directCommitOK ? pub.mode : 'pr') === v ? 'selected' : ''}>${v === 'pr' ? 'Pull request' : 'Direct commit'}</option>`).join('')}
            </select>
            <div class="config-help">${directCommitOK ? 'Pull request opens a PR for review; direct commit pushes to the base branch.' : 'Pull request only: this hive\'s policy level does not allow direct commits to the base branch.'}${restart}</div>
          </div>`;
      const schedules = d.schedules || ['hourly', 'daily'];
      const layers = d.layers || ['personal', 'project', 'org', 'community'];
      return `
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Bead synthesizer</div>
          ${toggle('bs-enabled', 'Synthesize completed beads into wiki facts', 'knowledge.bead_synthesizer.enabled', bs.enabled, 'on by default; starts or stops the running synthesizer.', true, 'Turns completed beads (and their merged PRs) into wiki facts on a schedule. On unless set to false. Saving starts or stops the running synthesizer at once.')}
          <div class="config-field-row">
            ${choice('bs-schedule', 'Schedule', 'knowledge.bead_synthesizer.schedule', bs.schedule, schedules, 'Default (' + (def.bead_synthesizer_schedule || 'hourly') + ')', 'How often a synthesis cycle runs.', 'hourly or daily. Empty restores the default (hourly).')}
            ${choice('bs-target_layer', 'Target layer', 'knowledge.bead_synthesizer.target_layer', bs.target_layer, layers, 'Default (' + (def.bead_synthesizer_target_layer || 'project') + ')', 'Layer the synthesizer vault is registered at with the primer.', 'The primer layer the synthesized facts are served from. Empty restores the default (project).')}
            ${num('bs-min_confidence', 'Minimum confidence', 'knowledge.bead_synthesizer.min_confidence', bs.min_confidence, def.bead_synthesizer_min_confidence || 0.5, '0.05', '1', 'Facts classified below this confidence (0–1) are skipped.', 'Facts whose classification confidence falls below this are not written. Between 0 and 1; 0 restores the default (0.5).')}
          </div>
          <div class="config-field-row">
            ${num('bs-max_facts_per_cycle', 'Max facts per cycle', 'knowledge.bead_synthesizer.max_facts_per_cycle', bs.max_facts_per_cycle, def.bead_synthesizer_max_facts_per_cycle || 20, '1', '', 'Cap on facts written per cycle after deduplication.', 'Cap on facts written per synthesis cycle after deduplication. 0 restores the default (20).')}
            ${num('bs-max_api_calls_per_cycle', 'Max GitHub API calls per cycle', 'knowledge.bead_synthesizer.max_api_calls_per_cycle', bs.max_api_calls_per_cycle, def.bead_synthesizer_max_api_calls_per_cycle || 40, '1', '', 'Cap on charged PR GETs per cycle; HIVE_BEAD_SYNTH_MAX_API_CALLS still overrides.', 'Cap on charged GitHub PR GETs per synthesis cycle. PR reads are conditional, so a 304 reuses the cached PR and does not count. The HIVE_BEAD_SYNTH_MAX_API_CALLS environment variable overrides this. 0 restores the default (40).')}
          </div>
          ${roRow('Vault path', 'knowledge.bead_synthesizer.vault_path', bs.vault_path, 'default ' + (def.bead_synthesizer_vault_path || ''))}
          <div class="settings-subsection-title mt-4">Publish to repository</div>
          ${toggle('pub-enabled', 'Publish synthesized facts to a repository', 'knowledge.bead_synthesizer.publish.enabled', pub.enabled, 'off by default; only owners can change this.', false, 'Opt in to committing synthesized wiki facts into a repository path. Disabled unless you turn it on.')}
          <div class="config-field-row">
            ${pubText('pub-repo', 'Target repository', 'knowledge.bead_synthesizer.publish.repo', pub.repo, 'Default (hive primary repository)', 'owner/name. Empty uses the hive primary repository.')}
            ${pubText('pub-path', 'Path', 'knowledge.bead_synthesizer.publish.path', pub.path, 'docs/knowledge/', 'Relative directory in the repository, no .. components.')}
            ${pubText('pub-base_branch', 'Base branch', 'knowledge.bead_synthesizer.publish.base_branch', pub.base_branch, 'Default (repository default branch)', 'Branch the PR targets or the commit lands on.')}
          </div>
          <div class="config-field-row">
            ${pubModeField}
            ${choice('pub-cadence', 'Cadence', 'knowledge.bead_synthesizer.publish.cadence', pub.cadence, ['per_run', 'daily'], 'Default (per_run)', 'How often facts are published.', 'per_run publishes after each synthesis cycle; daily batches them once a day. Empty restores the default (per_run).')}
          </div>
          <div class="settings-subsection-title mt-4">Bead retention</div>
          <div class="config-field-row">
            ${num('rp-max_beads', 'Max beads', 'knowledge.bead_synthesizer.retention_policy.max_beads', rp.max_beads, 5000, '1', '', 'Archiving starts above this many beads.', 'The bead lifecycle manager archives the lowest-scored beads once a store holds more than this, down to 80% of it. 0 uses the default (5000).')}
            ${num('rp-archive_after_synth_days', 'Archive after synthesis (days)', 'knowledge.bead_synthesizer.retention_policy.archive_after_synth_days', rp.archive_after_synth_days, 7, '1', '', 'Synthesized beads older than this become archive candidates.', 'Days after a bead has been synthesized into a fact before it may be archived. 0 uses the default (7).')}
            ${num('rp-high_priority_retain_days', 'Keep high-priority beads (days)', 'knowledge.bead_synthesizer.retention_policy.high_priority_retain_days', rp.high_priority_retain_days, 30, '1', '', 'High-priority beads are kept at least this long.', 'Days a high-priority bead is kept before it may be archived. 0 uses the default (30).')}
          </div>
          ${toggle('rp-preserve_with_deps', 'Never archive beads with open dependents', 'knowledge.bead_synthesizer.retention_policy.preserve_with_deps', rp.preserve_with_deps, 'on when the retention block is absent.', false, 'A bead that other open beads depend on is never archived. On when no retention_policy block is set.')}
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Curator (scheduled promotion)</div>
          ${toggle('cu-enabled', 'Promote facts on a schedule', 'knowledge.curator.enabled', cu.enabled, 'off by default; promotes verified facts with no human review.', false, 'Opt-in. On each run, verified facts in the source layer whose confidence reaches the threshold are copied into the target layer without human review. A schedule alone does not start it.')}
          <div class="config-field-row">
            ${choice('cu-schedule', 'Schedule', 'knowledge.curator.schedule', cu.schedule, schedules, 'Default (' + (def.curator_schedule || 'daily') + ')', 'How often the promotion sweep runs.', 'hourly or daily. Empty uses daily once the curator is enabled.')}
            ${num('cu-auto_promote_threshold', 'Auto-promote threshold', 'knowledge.curator.auto_promote_threshold', cu.auto_promote_threshold, def.curator_auto_promote_threshold || 0.9, '0.05', '1', 'Minimum confidence (0–1) a verified fact needs to be promoted.', 'A verified fact is promoted only when its confidence is at least this. Between 0 and 1; 0 restores the default (0.9).')}
          </div>
          <div class="config-field-row">
            ${choice('cu-promote_from', 'Promote from', 'knowledge.curator.promote_from', cu.promote_from, layers, 'Default (' + (def.curator_promote_from || 'project') + ')', 'Source layer for the sweep.', 'The layer facts are promoted from. Must differ from the target layer. Empty uses project.')}
            ${choice('cu-promote_to', 'Promote to', 'knowledge.curator.promote_to', cu.promote_to, layers, 'Default (' + (def.curator_promote_to || 'org') + ')', 'Target layer for the sweep.', 'The layer promoted facts are copied into. Must differ from the source layer. Empty uses org.')}
          </div>
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Repository code maps</div>
          ${toggle('cm-enabled', 'Generate repository code maps', 'knowledge.code_maps.enabled', cm.enabled, 'off by default; the repositories to map stay yaml-only.', false, 'Opt-in. Writes a generated, size-capped summary of each repository listed under knowledge.code_maps in hive.yaml and regenerates it when HEAD moves.')}
          <div class="config-field-row">
            ${choice('cm-schedule', 'Schedule', 'knowledge.code_maps.schedule', cm.schedule, schedules, 'Default (hourly)', 'How often HEAD is checked.', 'hourly or daily. Empty uses hourly.')}
            ${choice('cm-layer', 'Layer', 'knowledge.code_maps.layer', cm.layer, layers, 'Default (project)', 'Knowledge layer recorded on each map.', 'The layer recorded on each generated map. Empty uses project.')}
            ${num('cm-max_bytes', 'Max bytes per map', 'knowledge.code_maps.max_bytes', cm.max_bytes, 16384, '1', '', 'Cap on the rendered size of each map.', 'Cap on the rendered size of each map, in bytes. 0 uses the generator default (16384).')}
          </div>
          ${roRow('Vault path', 'knowledge.code_maps.vault_path', cm.vault_path, 'default /data/vaults/code-maps')}
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Kick primer</div>
          <div class="config-field-row">
            ${num('pr-max_facts', 'Max facts', 'knowledge.primer.max_facts', pr.max_facts, def.primer_max_facts || 25, '1', '', 'Most facts placed in one kick primer.', 'The most knowledge facts placed in one kick primer. 0 restores the default (25).')}
            ${num('pr-max_bytes', 'Max bytes', 'knowledge.primer.max_bytes', pr.max_bytes, def.primer_max_bytes || 4096, '1', '', 'Cap on the rendered primer; facts past it are dropped whole.', 'Cap on the rendered knowledge primer per kick. Facts past it are dropped whole and a marker says so. 0 restores the default (4096).')}
          </div>
          ${roRow('Merge strategy', 'knowledge.primer.merge_strategy', pr.merge_strategy, 'default ' + (def.primer_merge_strategy || 'precedence'))}
          ${roRow('Engine', 'knowledge.engine', d.engine, 'unset')}
          ${owner ? '<button class="hv-btn btn-primary mt-4" id="kp-save-btn" data-action="saveKnowledgePipeline">Save</button>' : '<div class="config-help mt-3">Owner role required to change these settings.</div>'}
        </div>`;
    }

    function kpToggle(el) {
      if (!el || el.classList.contains('disabled')) return;
      const on = el.classList.toggle('on');
      el.setAttribute('aria-checked', on ? 'true' : 'false');
    }

    async function saveKnowledgePipeline() {
      const d = _kpState.data || {};
      const cur = {
        bead_synthesizer: d.bead_synthesizer || {},
        publish: (d.bead_synthesizer || {}).publish || {},
        retention_policy: (d.bead_synthesizer || {}).retention_policy || {},
        curator: d.curator || {},
        code_maps: d.code_maps || {},
        primer: d.primer || {}
      };
      const fields = [
        ['bead_synthesizer', 'enabled', 'bs-enabled', 'bool'],
        ['bead_synthesizer', 'schedule', 'bs-schedule', 'str'],
        ['bead_synthesizer', 'target_layer', 'bs-target_layer', 'str'],
        ['bead_synthesizer', 'min_confidence', 'bs-min_confidence', 'frac'],
        ['bead_synthesizer', 'max_facts_per_cycle', 'bs-max_facts_per_cycle', 'int'],
        ['bead_synthesizer', 'max_api_calls_per_cycle', 'bs-max_api_calls_per_cycle', 'int'],
        ['publish', 'enabled', 'pub-enabled', 'bool'],
        ['publish', 'repo', 'pub-repo', 'str'],
        ['publish', 'path', 'pub-path', 'str'],
        ['publish', 'base_branch', 'pub-base_branch', 'str'],
        ['publish', 'mode', 'pub-mode', 'str'],
        ['publish', 'cadence', 'pub-cadence', 'str'],
        ['retention_policy', 'max_beads', 'rp-max_beads', 'int'],
        ['retention_policy', 'archive_after_synth_days', 'rp-archive_after_synth_days', 'int'],
        ['retention_policy', 'high_priority_retain_days', 'rp-high_priority_retain_days', 'int'],
        ['retention_policy', 'preserve_with_deps', 'rp-preserve_with_deps', 'bool'],
        ['curator', 'enabled', 'cu-enabled', 'bool'],
        ['curator', 'schedule', 'cu-schedule', 'str'],
        ['curator', 'auto_promote_threshold', 'cu-auto_promote_threshold', 'frac'],
        ['curator', 'promote_from', 'cu-promote_from', 'str'],
        ['curator', 'promote_to', 'cu-promote_to', 'str'],
        ['code_maps', 'enabled', 'cm-enabled', 'bool'],
        ['code_maps', 'schedule', 'cm-schedule', 'str'],
        ['code_maps', 'layer', 'cm-layer', 'str'],
        ['code_maps', 'max_bytes', 'cm-max_bytes', 'int'],
        ['primer', 'max_facts', 'pr-max_facts', 'int'],
        ['primer', 'max_bytes', 'pr-max_bytes', 'int']
      ];
      const body = {};
      const put = (group, key, v) => {
        if (group === 'retention_policy' || group === 'publish') {
          body.bead_synthesizer = body.bead_synthesizer || {};
          body.bead_synthesizer[group] = body.bead_synthesizer[group] || {};
          body.bead_synthesizer[group][key] = v;
        } else {
          body[group] = body[group] || {};
          body[group][key] = v;
        }
      };
      for (const [group, key, id, kind] of fields) {
        const el = document.getElementById('kp-' + id);
        if (!el) continue;
        const was = cur[group][key];
        if (kind === 'bool') {
          const on = el.classList.contains('on');
          if (on !== !!was) put(group, key, on);
        } else if (kind === 'str') {
          const v = (el.value || '').trim();
          if (v !== (was || '')) put(group, key, v);
        } else {
          const raw = (el.value || '').trim();
          const v = raw === '' ? 0 : (kind === 'frac' ? parseFloat(raw) : parseInt(raw, 10));
          if (isNaN(v) || v < 0 || (kind === 'frac' && v > 1)) { showToast(key + (kind === 'frac' ? ' must be between 0 and 1' : ' must be a non-negative number'), 'error'); return; }
          if (v !== (was || 0)) put(group, key, v);
        }
      }
      if (!Object.keys(body).length) { showToast('Nothing to save', 'error'); return; }
      const btn = document.getElementById('kp-save-btn');
      if (btn) { btn.disabled = true; btn.textContent = 'Saving…'; }
      try {
        const res = await fetch('/api/config/knowledge/pipeline', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body)
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
        _kpState = { loaded: true, error: '', data: data };
        const bsKeys = Object.keys(body.bead_synthesizer || {});
        const liveOnly = Object.keys(body).length === 1 && bsKeys.length === 1 && bsKeys[0] === 'enabled';
        showToast(liveOnly ? 'Knowledge settings saved' : 'Knowledge settings saved — restart the hive to apply them', 'success');
        const el = document.getElementById('kp-pipeline-root');
        if (el) el.innerHTML = renderKnowledgePipelineBody();
      } catch (e) {
        showToast('Save failed: ' + e.message, 'error');
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Save'; }
      }
    }

    async function kcSave(list, extra) {
      const r = await fetch('/api/config/knowledge/connectors', {
        method: 'PUT',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(Object.assign({connectors: list}, extra || {}))
      });
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      _kcState.connectors = data.connectors || [];
      await loadKnowledgeConnectors();
    }

    function kcFind(name) { return _kcState.connectors.find(c => c.name === name); }

    function kcToggleEnabled(name) {
      const list = _kcState.connectors.map(c => c.name === name ? Object.assign({}, c, { enabled: !c.enabled }) : c);
      kcSave(list).then(() => showToast('Connector ' + name + ' updated', 'success')).catch(e => showToast('Connector update failed: ' + e.message, 'error'));
    }

    async function kcRemove(name) {
      const ok = await hiveConfirm({ title: 'Remove connector?', message: 'Remove connector "' + name + '"? Facts it already synced stay in the vault.', confirmLabel: 'Remove', danger: true });
      if (!ok) return;
      kcSave(_kcState.connectors.filter(c => c.name !== name)).then(() => showToast('Connector ' + name + ' removed', 'success')).catch(e => showToast('Connector removal failed: ' + e.message, 'error'));
    }

    async function kcSyncNow(name) {
      try {
        const r = await fetch('/api/config/knowledge/connectors/' + encodeURIComponent(name) + '/sync', { method: 'POST' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        showToast('Sync started for ' + name, 'success');
        setTimeout(loadKnowledgeConnectors, 1500);
      } catch (e) {
        showToast('Sync failed to start: ' + e.message, 'error');
      }
    }

    async function kcPublishNow() {
      try {
        const r = await fetch('/api/knowledge/publish/sync', { method: 'POST' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        showToast('Publish started', 'success');
        setTimeout(loadKnowledgeConnectors, 1500);
      } catch (e) {
        showToast('Publish failed to start: ' + e.message, 'error');
      }
    }

    function kcScopeFieldsHTML(type, scope) {
      const known = KC_SCOPE_FIELDS[type];
      scope = scope || {};
      if (known) {
        return known.map(f => '<label class="hive-dialog-label">' + esc(f[1]) + '<input class="hive-dialog-input" type="text" data-kc-scope="' + esc(f[0]) + '" value="' + esc(scope[f[0]] || '') + '" placeholder="' + esc(f[2]) + '" autocomplete="off"></label>').join('');
      }
      const lines = Object.keys(scope).map(k => k + '=' + scope[k]).join('\n');
      return '<label class="hive-dialog-label">Scope (key=value per line)<textarea class="hive-dialog-input" rows="4" data-kc-scope-raw>' + esc(lines) + '</textarea></label>';
    }

    function kcReadForm(overlay) {
      const val = sel => { const el = overlay.querySelector(sel); return el ? el.value.trim() : ''; };
      const scope = {};
      overlay.querySelectorAll('[data-kc-scope]').forEach(el => { if (el.value.trim()) scope[el.dataset.kcScope] = el.value.trim(); });
      const raw = overlay.querySelector('[data-kc-scope-raw]');
      if (raw) raw.value.split('\n').forEach(line => { const i = line.indexOf('='); if (i > 0 && line.slice(i + 1).trim()) scope[line.slice(0, i).trim()] = line.slice(i + 1).trim(); });
      const authKind = val('[data-kc-auth-kind]');
      const authVal = val('[data-kc-auth-value]');
      return {
        name: val('[data-kc-name]'),
        type: val('[data-kc-type]'),
        enabled: !overlay.querySelector('[data-kc-enabled]') || overlay.querySelector('[data-kc-enabled]').checked,
        layer: val('[data-kc-layer]'),
        interval: val('[data-kc-interval]'),
        scope: scope,
        auth: authKind === 'env' ? { env: authVal } : authKind === 'file' ? { file: authVal } : {}
      };
    }

    function kcOpenEditor(name) {
      const existing = name ? kcFind(name) : null;
      const c = existing || { name: '', type: (_kcState.types[0] || 'git'), enabled: true, layer: 'project', interval: '', scope: {}, auth: {} };
      const authKind = c.auth && c.auth.env ? 'env' : (c.auth && c.auth.file ? 'file' : '');
      const authVal = (c.auth && (c.auth.env || c.auth.file)) || '';
      const opts = (arr, sel) => arr.map(v => '<option value="' + esc(v) + '"' + (v === sel ? ' selected' : '') + '>' + esc(v) + '</option>').join('');
      const overlay = document.createElement('div');
      overlay.className = 'hive-dialog-overlay';
      overlay.innerHTML = '<div class="hive-dialog" role="dialog" aria-modal="true" aria-labelledby="kc-dialog-title">'
        + '<div id="kc-dialog-title" class="hive-dialog-title">' + (existing ? 'Edit connector' : 'Add connector') + '</div>'
        + '<label class="hive-dialog-label">Name<input class="hive-dialog-input" type="text" data-kc-name data-initial-focus value="' + esc(c.name) + '"' + (existing ? ' readonly' : '') + ' placeholder="team-wiki" autocomplete="off"></label>'
        + '<label class="hive-dialog-label">Type<select class="hive-dialog-input" data-kc-type' + (existing ? ' disabled' : '') + '>' + opts(_kcState.types, c.type) + '</select></label>'
        + '<label class="hive-dialog-label">Layer<select class="hive-dialog-input" data-kc-layer>' + opts(_kcState.layers, c.layer) + '</select></label>'
        + '<div data-kc-scope-box>' + kcScopeFieldsHTML(c.type, c.scope) + '</div>'
        + '<label class="hive-dialog-label">Credential source (name or path only, never the secret)<select class="hive-dialog-input" data-kc-auth-kind>'
        + '<option value=""' + (authKind === '' ? ' selected' : '') + '>None</option><option value="env"' + (authKind === 'env' ? ' selected' : '') + '>Environment variable name</option><option value="file"' + (authKind === 'file' ? ' selected' : '') + '>Secret file path</option></select></label>'
        + '<input class="hive-dialog-input" type="text" data-kc-auth-value value="' + esc(authVal) + '" placeholder="CONFLUENCE_TOKEN or /run/secrets/token" autocomplete="off">'
        + '<label class="hive-dialog-label">Sync interval (blank = 15m)<input class="hive-dialog-input" type="text" data-kc-interval value="' + esc(c.interval || '') + '" placeholder="15m" autocomplete="off"></label>'
        + '<label class="hive-dialog-label"><input type="checkbox" data-kc-enabled' + (c.enabled ? ' checked' : '') + '> Enabled</label>'
        + '<div class="hive-dialog-error" role="alert" data-kc-msg></div>'
        + '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn cancel">Cancel</button>'
        + '<button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-kc-validate>Validate</button>'
        + '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary">' + (existing ? 'Save' : 'Add') + '</button></div></div>';
      document.body.appendChild(overlay);
      const msg = overlay.querySelector('[data-kc-msg]');
      const setMsg = (text, ok) => { msg.textContent = text; msg.dataset.ok = ok ? 'true' : 'false'; };
      overlay.querySelector('[data-kc-type]').addEventListener('change', e => {
        overlay.querySelector('[data-kc-scope-box]').innerHTML = kcScopeFieldsHTML(e.target.value, {});
      });
      overlay.querySelector('[data-kc-validate]').addEventListener('click', async () => {
        setMsg('Validating…', true);
        try {
          const r = await fetch('/api/config/knowledge/connectors/validate', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(kcReadForm(overlay)) });
          const data = await r.json().catch(() => ({}));
          if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
          setMsg(data.ok ? 'Configuration is valid.' : (data.error || 'Invalid configuration'), !!data.ok);
        } catch (e) {
          setMsg('Validation failed: ' + e.message, false);
        }
      });
      hiveWireDialog(overlay, () => {}, null, '.primary', '.cancel', async close => {
        const next = kcReadForm(overlay);
        const list = existing ? _kcState.connectors.map(x => x.name === existing.name ? next : x) : _kcState.connectors.concat([next]);
        try {
          await kcSave(list);
          showToast('Connector ' + next.name + ' saved', 'success');
          close(true);
        } catch (e) {
          setMsg(e.message, false);
        }
      });
    }

    var _themeState = { loaded:false, theme:'hive', theme_overrides:{}, catalog:[], effective:null };
    const HONEYCOMB_WATERMARK = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='56' height='97' viewBox='0 0 56 97'%3E%3Cpath d='M28 1l27 15.5v31L28 63 1 47.5v-31zM28 34l27 15.5v31L28 96 1 80.5v-31z' fill='none' stroke='%23e0a33a' stroke-opacity='.09'/%3E%3C/svg%3E";

    function yamlHint(path) {
      return '<span class="settings-yaml-hint">YAML <code>' + esc(path) + '</code></span>';
    }

    function splitConfigLines(value) {
      return String(value || '').split(/\r?\n|,/).map(s => s.trim()).filter(Boolean);
    }

    function parseSettingsMap(value) {
      const out = {};
      String(value || '').split(/\r?\n/).forEach(line => {
        line = line.trim();
        if (!line) return;
        const idx = line.indexOf('=');
        const key = (idx >= 0 ? line.slice(0, idx) : line).trim();
        const val = (idx >= 0 ? line.slice(idx + 1) : '').trim();
        if (key && val) out[key] = val;
      });
      return out;
    }

    function settingsMapText(map) {
      return Object.keys(map || {}).sort().map(k => k + '=' + (map || {})[k]).join('\n');
    }

    function markDashboardList(key, value) {
      _configState.data.dashboard = _configState.data.dashboard || {};
      _configState.data.dashboard[key] = splitConfigLines(value);
      markDirty('dashboard', key, _configState.data.dashboard[key]);
    }

    function markDashboardNames(value) {
      const names = parseSettingsMap(value);
      _configState.data.dashboard = _configState.data.dashboard || {};
      _configState.data.dashboard.authorized_user_names = names;
      markDirty('dashboard', 'authorized_user_names', names);
    }

    function markDashboardIssueBands(key, value) {
      const d = _configState.data.dashboard = _configState.data.dashboard || {};
      const bands = d.issue_bands = Object.assign({}, d.issue_bands || {});
      bands[key] = key === 'stale_days' ? Math.max(0, parseInt(value || '0', 10) || 0) : splitConfigLines(value);
      markDirty('dashboard', 'issue_bands', bands);
    }

    function markBackendsPolicy(key, value) {
      _configState.data.backends = _configState.data.backends || {};
      _configState.data.backends[key] = splitConfigLines(value);
      markDirty('backends', key, _configState.data.backends[key]);
    }

    function markClassifierList(key, value) {
      _configState.data.classifier = _configState.data.classifier || {};
      _configState.data.classifier[key] = splitConfigLines(value);
      markDirty('classifier', key, _configState.data.classifier[key]);
    }

    function renderGovDashboard(dash) {
      const bands = dash.issue_bands || {};
      const authTokenText = dash.auth_token_configured ? 'Configured — enter a new token to rotate, or save an empty value to clear.' : 'Not configured.';
      return '<div class="config-section">' +
        '<h3>Dashboard runtime</h3>' +
        '<p class="text-muted fs-base">Settings from <code>dashboard.*</code>. Port, proxy, and token changes may require restart or re-authentication.</p>' +
        '<div class="config-field-row">' +
          '<div class="config-field"><label>Port ' + yamlHint('dashboard.port') + '</label><input type="number" min="1" max="65535" value="' + esc(String(dash.port || 3001)) + '" data-change-action="markDirty" data-arg0="dashboard" data-arg1="port" data-arg-types="s,s,N"><div class="config-help">Requires restart.</div></div>' +
          '<div class="config-field"><label>Agent poll interval (seconds) ' + yamlHint('dashboard.agent_poll_interval_s') + '</label><input type="number" min="1" value="' + esc(String(dash.agent_poll_interval_s || 0)) + '" data-change-action="markDirty" data-arg0="dashboard" data-arg1="agent_poll_interval_s" data-arg-types="s,s,N"></div>' +
        '</div>' +
        '<div class="config-toggle"><div class="config-toggle-switch ' + (dash.experiment_lab ? 'on' : '') + '" data-action="toggleConfigSwitch" data-section="dashboard" data-key="experiment_lab"></div><span class="config-toggle-label">Experiment Lab ' + yamlHint('dashboard.experiment_lab') + '</span></div>' +
        '<div class="config-toggle"><div class="config-toggle-switch ' + (dash.hub_proxied ? 'on' : '') + '" data-action="toggleConfigSwitch" data-section="dashboard" data-key="hub_proxied"></div><span class="config-toggle-label">Hub-proxied auth ' + yamlHint('dashboard.hub_proxied') + ' <span class="text-muted">Managed by hub on hosted hives.</span></span></div>' +
        '<div class="config-field"><label>Dashboard auth token ' + yamlHint('dashboard.auth_token') + '</label><input type="password" autocomplete="new-password" placeholder="' + esc(authTokenText) + '" data-change-action="markDirty" data-arg0="dashboard" data-arg1="auth_token" data-arg-types="s,s,v"><div class="config-help">Write-only; the current token is never echoed.</div></div>' +
        '<h3>Access allowlist</h3>' +
        '<div class="config-field"><label>Authorized users ' + yamlHint('dashboard.authorized_users[]') + '</label><textarea rows="5" data-change-action="markDashboardList" data-arg0="authorized_users" data-arg-types="s,v">' + esc((dash.authorized_users || []).join('\n')) + '</textarea><div class="config-help">One user per line; append <code>:read</code>, <code>:read-write</code>, or <code>:owner</code> for explicit roles.</div></div>' +
        '<div class="config-field"><label>Authorized user display names ' + yamlHint('dashboard.authorized_user_names.<key>') + '</label><textarea rows="4" placeholder="ibmid:123=Jane Doe" data-change-action="markDashboardNames" data-arg-types="v">' + esc(settingsMapText(dash.authorized_user_names || {})) + '</textarea></div>' +
        '<h3>Issue bands</h3>' +
        '<div class="config-field-row">' +
          '<div class="config-field"><label>Waiting labels ' + yamlHint('dashboard.issue_bands.waiting_labels[]') + '</label><textarea rows="4" data-change-action="markDashboardIssueBands" data-arg0="waiting_labels" data-arg-types="s,v">' + esc((bands.waiting_labels || []).join('\n')) + '</textarea></div>' +
          '<div class="config-field"><label>Done labels ' + yamlHint('dashboard.issue_bands.done_labels[]') + '</label><textarea rows="4" data-change-action="markDashboardIssueBands" data-arg0="done_labels" data-arg-types="s,v">' + esc((bands.done_labels || []).join('\n')) + '</textarea></div>' +
        '</div>' +
        '<div class="config-field"><label>Stale after days ' + yamlHint('dashboard.issue_bands.stale_days') + '</label><input type="number" min="0" value="' + esc(String(bands.stale_days || 0)) + '" data-change-action="markDashboardIssueBands" data-arg0="stale_days" data-arg-types="s,N"></div>' +
      '</div>';
    }

    function renderDashboardTimezoneSettings() {
      const pref = dashboardTimeZonePreference();
      const serverTz = validDashboardTimeZone(_dashboardServerTimeZone) || '';
      const zones = supportedDashboardTimeZones();
      const selectedCustom = pref && ![DASHBOARD_TIMEZONE_BROWSER, DASHBOARD_TIMEZONE_SERVER, 'UTC'].includes(pref);
      const fixed = [
        [DASHBOARD_TIMEZONE_BROWSER, 'Browser local (default) — ' + (browserDashboardTimeZone() || 'unknown')],
        [DASHBOARD_TIMEZONE_SERVER, 'Hive server' + (serverTz ? ' (' + serverTz + ')' : '')],
        ['UTC', 'UTC'],
      ];
      const zoneOptions = zones.filter(z => z !== 'UTC').map(z => `<option value="${esc(z)}"${pref === z ? ' selected' : ''}>${esc(z)}</option>`).join('');
      const options = fixed.map(([value, label]) => `<option value="${esc(value)}"${pref === value ? ' selected' : ''}>${esc(label)}</option>`).join('') + zoneOptions;
      const preview = formatDashboardDateTime(new Date());
      const datalist = zones.map(z => `<option value="${esc(z)}"></option>`).join('');
      return '<div class="config-section" id="dashboard-timezone-settings">' +
        '<h3>Display timezone</h3>' +
        '<p class="text-muted fs-base">Choose the timezone used for dashboard timestamps. Relative ages such as “1m ago” are unchanged.</p>' +
        '<div class="config-field-row"><div class="config-field"><label for="dashboard-timezone-select">Timezone</label>' +
        '<select id="dashboard-timezone-select" data-change-action="setDashboardTimeZonePreference" data-arg-types="v">' + options + '</select></div>' +
        '<div class="config-field"><label for="dashboard-timezone-custom">Search IANA timezone</label>' +
        '<input id="dashboard-timezone-custom" list="dashboard-timezone-list" value="' + esc(selectedCustom ? pref : '') + '" placeholder="America/Los_Angeles" data-change-action="setDashboardTimeZonePreference" data-arg-types="v">' +
        '<datalist id="dashboard-timezone-list">' + datalist + '</datalist></div></div>' +
        '<div class="text-muted fs-base">Preview: <strong id="dashboard-timezone-preview">' + esc(preview) + '</strong></div>' +
        '</div>';
    }

    function setDashboardTimeZonePreference(value) {
      const pref = value || DASHBOARD_TIMEZONE_BROWSER;
      if (pref !== DASHBOARD_TIMEZONE_BROWSER && pref !== DASHBOARD_TIMEZONE_SERVER && pref !== 'UTC' && !validDashboardTimeZone(pref)) {
        showToast('Unknown timezone: ' + pref, 'error');
        return;
      }
      try { localStorage.setItem(DASHBOARD_TIMEZONE_STORAGE_KEY, pref); } catch (e) {}
      _navbarClockTimeZone = selectedDashboardTimeZone();
      tickNavbarClock();
      if (window._lastStatus) render(window._lastStatus);
      const root = document.getElementById('theme-config-root');
      if (root) root.outerHTML = renderGovAppearance();
      showToast('Display timezone saved: ' + dashboardTimeZoneLabel(selectedDashboardTimeZone()), 'success');
    }

    function renderGovAppearance() {
      if (!_themeState.loaded) return '<div id="theme-config-root"><div style="text-align:center;padding:28px;color:var(--muted)">Loading appearance…</div></div>';
      const current = _themeState.theme || 'hive';
      const overrides = _themeState.theme_overrides || {};
      const bg = overrides.background || {};
      const fonts = overrides.fonts || {};
      const tokens = overrides.tokens || {};
      const customCSS = overrides.custom_css || '';
      const honeyOn = bg.image === HONEYCOMB_WATERMARK;
      const bgScope = bg.scope || 'page';
      const themeCard = (t) => {
        const tok = t.tokens || {};
        const colors = t.swatches || [tok['--surface-0'] || '#111', tok['--surface-2'] || '#222', tok['--accent'] || tok['--brand'] || tok['--status-info'] || '#80bfff', tok['--text'] || '#fff'];
        return '<div class="theme-card' + (t.id === current ? ' active' : '') + '" data-theme-id="' + esc(t.id) + '" data-mouseover-action="themePreview" data-mouseout-action="themePreviewClear">'
          + '<div class="theme-swatches">' + colors.map(c => '<span class="theme-swatch" style="background:' + esc(c) + '"></span>').join('') + '</div>'
          + '<div class="theme-title">' + esc(t.name || t.id) + (t.dark ? ' 🌙' : ' ☀') + '</div>'
          + '<div class="theme-desc">' + esc(t.description || '') + '</div>'
          + '<button class="hv-btn btn-secondary btn" data-action="applyThemePreset" data-theme-id="' + esc(t.id) + '" data-stop="1">Apply</button></div>';
      };
      const catalog = _themeState.catalog || [];
      const dashboardThemes = catalog.filter(t => !String(t.id || '').startsWith('contributor-'));
      const profileThemes = catalog.filter(t => String(t.id || '').startsWith('contributor-'));
      const cards = '<div class="theme-gallery-section"><div class="section-label">Dashboard themes</div><div class="theme-gallery">' + dashboardThemes.map(themeCard).join('') + '</div></div>'
        + (profileThemes.length ? '<div class="theme-gallery-section"><div class="section-label">Contributor profile styles</div><div class="theme-gallery">' + profileThemes.map(themeCard).join('') + '</div></div>' : '');
      return '<div id="theme-config-root">'
        + renderDashboardTimezoneSettings()
        + '<p style="font-size:.8rem;color:var(--muted);margin-bottom:var(--sp-5)">Choose a hive-wide dashboard theme. Owners can layer safe CSS tokens, a background image, and capped custom CSS on top of any preset.</p>'
        + '<div class="theme-actions"><button class="hv-btn btn-secondary btn" data-action="resetThemeToPreset">Reset to preset</button><button class="hv-btn btn-secondary btn" data-action="saveThemeOverrides">Apply background/CSS</button><span style="color:var(--muted);font-size:.76rem">Current: <code>' + esc(current) + '</code></span></div>'
        + cards
        + '<div class="config-field-row"><div class="config-field"><label>Background image URL ' + yamlHint('dashboard.theme_overrides.background.image') + '</label><input id="theme-bg-url" type="text" value="' + esc(bg.image || '') + '" placeholder="https://example.org/background.svg"></div>'
        + '<div class="config-field"><label>Background opacity: <span id="theme-bg-opacity-value">' + esc(String(bg.opacity || 0.08)) + '</span> ' + yamlHint('dashboard.theme_overrides.background.opacity') + '</label><input id="theme-bg-opacity" type="range" min="0" max="1" step="0.01" value="' + esc(String(bg.opacity || 0.08)) + '" data-input-action="updateThemeOpacityLabel"></div></div>'
        + '<div class="config-field-row4"><div class="config-field"><label for="theme-bg-scope">Watermark on ' + yamlHint('dashboard.theme_overrides.background.scope') + '</label><select id="theme-bg-scope"><option value="page"' + (bgScope === 'page' ? ' selected' : '') + '>Page</option><option value="cards"' + (bgScope === 'cards' ? ' selected' : '') + '>Cards</option><option value="both"' + (bgScope === 'both' ? ' selected' : '') + '>Both</option></select></div>'
        + '<div class="config-field"><label>Position ' + yamlHint('dashboard.theme_overrides.background.position') + '</label><input id="theme-bg-position" type="text" value="' + esc(bg.position || '') + '" placeholder="center center"></div>'
        + '<div class="config-field"><label>Size ' + yamlHint('dashboard.theme_overrides.background.size') + '</label><input id="theme-bg-size" type="text" value="' + esc(bg.size || '') + '" placeholder="cover"></div>'
        + '<div class="config-field"><label>Attachment ' + yamlHint('dashboard.theme_overrides.background.attachment') + '</label><select id="theme-bg-attachment"><option value=""' + (!bg.attachment ? ' selected' : '') + '>(default)</option><option value="fixed"' + (bg.attachment === 'fixed' ? ' selected' : '') + '>fixed</option><option value="scroll"' + (bg.attachment === 'scroll' ? ' selected' : '') + '>scroll</option></select></div></div>'
        + '<label class="config-toggle" style="margin:4px 0 14px"><span class="config-toggle-switch' + (honeyOn ? ' on' : '') + '" data-action="toggleHoneycombWatermark"></span><span class="config-toggle-label">Use honeycomb watermark</span></label>'
        + '<div class="config-field-row"><div class="config-field"><label>UI font ' + yamlHint('dashboard.theme_overrides.fonts.ui') + '</label><input id="theme-font-ui" type="text" value="' + esc(fonts.ui || '') + '" placeholder="Inter, system-ui, sans-serif"></div>'
        + '<div class="config-field"><label>Mono font ' + yamlHint('dashboard.theme_overrides.fonts.mono') + '</label><input id="theme-font-mono" type="text" value="' + esc(fonts.mono || '') + '" placeholder="JetBrains Mono, monospace"></div></div>'
        + '<div class="config-field"><label>CSS token overrides ' + yamlHint('dashboard.theme_overrides.tokens.<key>') + '</label><textarea id="theme-token-overrides" rows="5" placeholder="--accent=#e0a33a">' + esc(settingsMapText(tokens)) + '</textarea><div class="config-help">One allowlisted CSS token per line as <code>--token=value</code>.</div></div>'
        + '<div class="config-field"><label>Custom CSS ' + yamlHint('dashboard.theme_overrides.custom_css') + '</label><textarea id="theme-custom-css" rows="10" data-input-action="updateThemeByteCount" placeholder=".panel { border-radius: 2px; }">' + esc(customCSS) + '</textarea><div class="theme-byte-count"><span id="theme-css-bytes">' + utf8Bytes(customCSS) + '</span> / 32768 bytes</div></div>'
        + '<div id="theme-save-status" style="font-size:.76rem;color:var(--muted)"></div></div>';
    }

    async function loadThemeConfig() {
      const root = document.getElementById('theme-config-root');
      try {
        const res = await fetch('/api/config/dashboard/theme');
        const data = await res.json();
        if (!res.ok) throw new Error(data.error || 'failed to load theme config');
        const listRes = await fetch('/api/themes?scope=dashboard');
        const listData = listRes.ok ? await listRes.json() : { themes: data.catalog || [] };
        _themeState = Object.assign({loaded:true}, data, { catalog: listData.themes || data.catalog || [] });
        if (_configState.tab === 'Appearance') document.getElementById('config-body').innerHTML = renderGovAppearance();
      } catch (err) {
        if (root) root.innerHTML = '<div style="color:var(--red);padding:var(--sp-7)">Failed to load appearance: ' + esc(err.message) + '</div>';
      }
    }
    function utf8Bytes(s) { return new TextEncoder().encode(String(s || '')).length; }
    function updateThemeByteCount() { const el=document.getElementById('theme-custom-css'); const out=document.getElementById('theme-css-bytes'); if(out) out.textContent = utf8Bytes(el ? el.value : '0'); }
    function updateThemeOpacityLabel() { const el=document.getElementById('theme-bg-opacity'); const out=document.getElementById('theme-bg-opacity-value'); if(out&&el) out.textContent = el.value; }
    function selectedThemeOverrides() {
      const bgURL = (document.getElementById('theme-bg-url') || {}).value || '';
      const opacity = parseFloat((document.getElementById('theme-bg-opacity') || {}).value || '0.08');
      const scope = (document.getElementById('theme-bg-scope') || {}).value || 'page';
      const position = ((document.getElementById('theme-bg-position') || {}).value || '').trim();
      const size = ((document.getElementById('theme-bg-size') || {}).value || '').trim();
      const attachment = ((document.getElementById('theme-bg-attachment') || {}).value || '').trim();
      const fontUI = ((document.getElementById('theme-font-ui') || {}).value || '').trim();
      const fontMono = ((document.getElementById('theme-font-mono') || {}).value || '').trim();
      const tokens = parseSettingsMap((document.getElementById('theme-token-overrides') || {}).value || '');
      const css = (document.getElementById('theme-custom-css') || {}).value || '';
      const overrides = { custom_css: css };
      if (Object.keys(tokens).length) overrides.tokens = tokens;
      if (fontUI || fontMono) overrides.fonts = { ui: fontUI, mono: fontMono };
      if (bgURL.trim()) overrides.background = { image: bgURL.trim(), scope: scope, opacity: Number.isFinite(opacity) ? opacity : 0.08, position: position, size: size, attachment: attachment || 'fixed' };
      return overrides;
    }
    async function putTheme(theme, overrides) {
      const res = await fetch('/api/config/dashboard/theme', { method:'PUT', headers:{'Content-Type':'application/json'}, body:JSON.stringify({ theme, theme_overrides: overrides || {} }) });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data.error || 'theme update failed');
      _themeState = Object.assign({loaded:true, catalog:_themeState.catalog || []}, data);
      const link = document.getElementById('hive-theme-css');
      if (link) {
        _themeBaseHref = '/api/theme.css?v=' + Date.now();
        link.href = _themeBaseHref;
      }
      if (_configState.tab === 'Appearance') document.getElementById('config-body').innerHTML = renderGovAppearance();
      return data;
    }
    async function applyThemePreset(event) { const id=(this && this.dataset && this.dataset.themeId) || (event && event.currentTarget && event.currentTarget.dataset.themeId); try { await putTheme(id, _themeState.theme_overrides || {}); showToast('Theme applied', 'success'); } catch(e) { showToast(e.message, 'error'); } }
    async function saveThemeOverrides() { try { await putTheme(_themeState.theme || 'hive', selectedThemeOverrides()); showToast('Appearance saved', 'success'); } catch(e) { showToast(e.message, 'error'); } }
    async function resetThemeToPreset() { try { await putTheme(_themeState.theme || 'hive', {}); showToast('Theme overrides reset', 'success'); } catch(e) { showToast(e.message, 'error'); } }
    function toggleHoneycombWatermark() { const input=document.getElementById('theme-bg-url'); if(!input) return; input.value = input.value === HONEYCOMB_WATERMARK ? '' : HONEYCOMB_WATERMARK; const bg = input.value ? { image: input.value, scope: (document.getElementById('theme-bg-scope') || {}).value || 'page', opacity: parseFloat((document.getElementById('theme-bg-opacity') || {}).value || '0.08'), attachment:'fixed' } : null; _themeState.theme_overrides = Object.assign({}, _themeState.theme_overrides || {}, bg ? {background:bg} : {background:null}); document.getElementById('config-body').innerHTML = renderGovAppearance(); }
    const _themePreviewHrefs = new Map();
    let _themeBaseHref = '/api/theme.css';
    function themeStylesheetHref(id) {
      const key = id || '';
      if (!key) return _themeBaseHref;
      if (!_themePreviewHrefs.has(key)) _themePreviewHrefs.set(key, '/api/theme.css?theme=' + encodeURIComponent(key));
      return _themePreviewHrefs.get(key);
    }
    function setThemeStylesheet(id) {
      const link = document.getElementById('hive-theme-css');
      if (!link) return;
      const href = themeStylesheetHref(id);
      const current = new URL(link.getAttribute('href') || '', window.location.href);
      const next = new URL(href, window.location.href);
      if (current.pathname === next.pathname && current.search === next.search) return;
      link.href = href;
    }
    function themePreview() { const id=this && this.dataset && this.dataset.themeId; document.querySelectorAll('.theme-card').forEach(c => c.classList.toggle('preview', c.dataset.themeId === id)); if(id) setThemeStylesheet(id); }
    function themePreviewClear() { document.querySelectorAll('.theme-card.preview').forEach(c => c.classList.remove('preview')); setThemeStylesheet(''); }

    // looksLikeApiKeyValue mirrors the server-side guardrail: env var
    // NAMES are short with underscores; key values start with sk- or run
    // long without any underscore. Catches keys pasted into the NAME/PATH
    // fields (which would bake the key into hive.yaml and resolve to an
    // empty key at runtime — a real leak that hit a hosted user).
    function looksLikeApiKeyValue(v) {
      const API_KEY_LIKE_LENGTH = 40; // matches apiKeyLikeLength server-side
      return v.startsWith('sk-') || (v.length > API_KEY_LIKE_LENGTH && !v.includes('_'));
    }

    // litellmKeySourceLabel turns the API's keySource ("file:<path>" /
    // "env:<NAME>") into human copy. Never contains the key value.
    function litellmKeySourceLabel(src) {
      if (!src) return '';
      if (src.startsWith('env:')) return `environment variable ${src.slice(4)}`;
      const path = src.startsWith('file:') ? src.slice(5) : src;
      if (path.startsWith('/data/')) return `dashboard-saved key (${path})`;
      if (path.startsWith('/secrets/')) return `Kubernetes Secret mount (${path})`;
      return `key file ${path}`;
    }

    // litellmProbeMessage formats a probe result ({ok, models} or
    // {ok:false, error}) from the save/test endpoints. Live results only —
    // the server never counts static fallback aliases here.
    function litellmProbeMessage(probe) {
      // probe.route (#9945): the request agents on the litellm backend will
      // really make (POST URL, key fingerprint, config source) and a warning
      // when it is not what this Test just exercised. Appended to every
      // outcome so "OK" is never shown without saying what it proves.
      const route = probe.route ? '\n' + probe.route : '';
      if (probe.ok && probe.modelsListBlocked) {
        // /v1/models was refused by a proxy in front of the gateway, but a
        // 1-token inference call with the default model succeeded (#9945).
        return { ok: true, msg: `LiteLLM OK — inference with ${probe.inferenceModel} works, but model listing is blocked: ${probe.warning}` + route };
      }
      if (probe.ok) {
        const base = `LiteLLM OK — ${probe.models} model${probe.models === 1 ? '' : 's'} available`;
        // probe.usable: the gateway advertises the full catalog but this key's
        // team is scoped to fewer (the rest 403 at inference). Show both counts
        // so the smaller usable number is not a surprise later.
        const usable = (typeof probe.usable === 'number' && probe.usable < probe.models)
          ? ` (${probe.usable} usable with your key)` : '';
        return { ok: true, msg: base + usable + route };
      }
      return { ok: false, msg: `LiteLLM connection failed: ${probe.error}` + route };
    }

    // showLiteLLMTestResult writes a probe result to the tab's inline
    // result line (when the LiteLLM tab is open) and returns the
    // formatted message for toasts.
    function showLiteLLMTestResult(probe) {
      const r = litellmProbeMessage(probe);
      const el = document.getElementById('litellm-test-result');
      if (el) {
        el.textContent = r.msg;
        el.style.display = 'block';
        el.style.color = r.ok ? 'var(--green)' : 'var(--red)';
      }
      return r;
    }

    function onLitellmKeyEnvChange(input) {
      const v = input.value.trim();
      if (v && looksLikeApiKeyValue(v)) {
        showToast('This looks like an API key — paste it in the API Key field above; this field takes an environment variable NAME (e.g. HIVE_LITELLM_API_KEY)', 'error');
        input.value = '';
        return;
      }
      markDirty('litellm', 'apiKeyEnv', v);
    }

    function onLitellmKeyFileChange(input) {
      const v = input.value.trim();
      if (v && !v.startsWith('/') && looksLikeApiKeyValue(v)) {
        showToast('This looks like an API key — paste it in the API Key field above; this field takes a file PATH (e.g. /secrets/litellm_api_key)', 'error');
        input.value = '';
        return;
      }
      markDirty('litellm', 'apiKeyFile', v);
    }

    // Sentinel value for the "type a custom id" option in the Default Model
    // select — chosen so it can never collide with a real model id.
    const LITELLM_CUSTOM_MODEL_SENTINEL = '__custom__';

    // litellmDefaultModelFieldHtml renders the Governor→LiteLLM "Default
    // Model" control. When discovery has returned models for the litellm
    // backend it renders a <select> of the discovered ids (verbatim,
    // prefixes intact) with the current value preselected, plus a
    // "Custom…" escape hatch that reveals a free-text input. When
    // discovery is empty/unconfigured it falls back to a plain text input
    // so the field is never a dead end.
    function litellmDefaultModelFieldHtml(current) {
      const models = modelsForBackend('litellm');
      const cur = current || '';
      if (!models.length) {
        return `<input type="text" value="${esc(cur)}" placeholder="e.g. gpt-4o" data-change-action="markDirty" data-arg0="litellm" data-arg1="defaultModel" data-arg-types="s,s,v">`;
      }
      const inList = cur && models.some(m => m.value === cur);
      const options = models.map(m =>
        `<option value="${esc(m.value)}"${m.value === cur ? ' selected' : ''}>${esc(m.value)}</option>`).join('');
      // Current value is set but not among discovered ids → preselect a
      // "(custom)" option carrying it so the selection is never lost.
      const customOptLabel = inList ? 'Custom…' : `${esc(cur)} (custom)`;
      const customSelected = cur && !inList;
      const emptyOpt = cur ? '' : '<option value="" selected disabled>Select a discovered model…</option>';
      return `
        <select data-change-action="onLitellmDefaultModelSelect" data-arg-types="t">
          ${emptyOpt}${options}
          <option value="${LITELLM_CUSTOM_MODEL_SENTINEL}"${customSelected ? ' selected' : ''}>${customOptLabel}</option>
        </select>
        <input type="text" value="${customSelected ? esc(cur) : ''}" placeholder="e.g. Azure/gpt-4o" style="margin-top:var(--sp-3);display:${customSelected ? 'block' : 'none'}" data-litellm-custom-model data-change-action="markDirty" data-arg0="litellm" data-arg1="defaultModel" data-arg-types="s,s,v">`;
    }

    // onLitellmDefaultModelSelect handles the discovered-model <select>:
    // picking a real id records it; picking "Custom…" reveals the free-text
    // input and defers the value to its onchange.
    function onLitellmDefaultModelSelect(sel) {
      const custom = sel.parentElement.querySelector('[data-litellm-custom-model]');
      if (sel.value === LITELLM_CUSTOM_MODEL_SENTINEL) {
        if (custom) { custom.style.display = 'block'; custom.focus(); }
        return;
      }
      if (custom) custom.style.display = 'none';
      markDirty('litellm', 'defaultModel', sel.value);
    }

    // refreshLitellmDefaultModelField re-renders the Default Model control
    // in place after a fresh discovery lands (tab open / key save), so a
    // just-added key's models appear without a full reload. Preserves any
    // unsaved dirty value the user has already typed/picked.
    function refreshLitellmDefaultModelField() {
      const host = document.getElementById('litellm-default-model-field');
      if (!host) return;
      const dirtyVal = (_configState.dirty.litellm || {}).defaultModel;
      const saved = (_configState.data.litellm || {}).defaultModel;
      const current = dirtyVal !== undefined ? dirtyVal : (saved || '');
      host.innerHTML = litellmDefaultModelFieldHtml(current);
    }

    // ── Variables tab: operator-defined ${VAR} substitutions ──
    // Reads GET /api/config/variables (never returns secret values — only
    // name/type/scope/source + the exec/http gate flags). static and env
    // variables can be added/edited/deleted here; script and http variables are
    // shown read-only because they can execute code or reach the network and are
    // therefore seed-only (GitOps), enforced server-side.
    // ── Security tab / Access subsection: read-only view of who can log in to this hive ──
    // Reads GET /api/config/authorized-users. Access is granted on the HUB
    // (Manage Access) and propagated to the spoke via heartbeat, so this view is
    // intentionally read-only — it shows the current allowlist, not an editor.
    // ---- Bob tab -----------------------------------------------------------
    // The IBM bobshell ("bob") API key. This is hive-wide, not per-agent:
    // governor.bob.api_key_file is a single hive-scoped setting resolving to
    // ONE file, so every bob agent shares the same key. It lives here rather
    // than on an agent card so the scope the UI implies matches the scope the
    // storage actually has.
    function renderGovBob() {
      loadBobKeyStatus();
      return `
        <div style="font-size:var(--fs-base);color:var(--muted);margin-bottom:14px;line-height:1.5">
          The API key the <b>bob</b> (IBM bobshell) CLI backend authenticates with. It
          applies to the whole hive — every agent using the bob backend shares this one
          key. The key is stored in a private file on this hive and is never shown
          again, never written to hive.yaml, and never included in config downloads.
        </div>
        <div id="gov-bob-host"><div style="color:var(--muted);font-size:0.8rem">Loading…</div></div>`;
    }

    // loadBobKeyStatus fetches presence-only status and paints the bob panel.
    // The response carries `configured` (bool) and a safe `source` string
    // ("file:<path>" / "env:<NAME>") — never the key value.
    //
    // The DOM target is resolved AFTER the first await, never before: the
    // caller does `body.innerHTML = renderGovernorTab(tab)`, so this function
    // starts while the PREVIOUS tab is still mounted (see loadAuthorizedUsers).
    async function loadBobKeyStatus() {
      let host = null;
      try {
        const r = await fetch('/api/config/governor/bob');
        host = document.getElementById('gov-bob-host');
        if (!host) return;
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        renderBobKeyPanel(host, d.configured === true, d.source || '', d.keyName || '', d.sessionPrefix || '');
      } catch (e) {
        host = host || document.getElementById('gov-bob-host');
        if (host) host.innerHTML = '<div style="color:var(--red);font-size:0.8rem">Failed to load bob key status: ' + esc(e.message) + '</div>';
      }
    }

    // renderBobKeyPanel paints the bob credential card. `source` is the safe
    // source string only; the key value never reaches this function. `keyName`
    // is the operator-chosen LABEL for the key (also safe to show) — it lets a
    // manager see WHICH key this hive uses without the secret. Empty on hives
    // that never recorded a name, which we render as "(unnamed)".
    function renderBobKeyPanel(host, configured, source, keyName, sessionPrefix) {
      if (!host) return;
      const nameLabel = configured
        ? ' <strong class="text-default">' + esc(keyName || '(unnamed)') + '</strong>'
        : '';
      const statusLine = configured
        ? '<span class="text-success">✓ Key set</span>' + nameLabel + (source ? ' <span class="text-muted">(' + esc(source) + ')</span>' : '')
        : '<span class="text-muted">No key set — agents using bob cannot start.</span>';
      host.innerHTML = `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;background:var(--surface);max-width:640px">
          <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:var(--sp-4)">
            <span style="font-weight:600;font-size:0.9rem">bob</span>
            <span style="font-size:var(--fs-xs);font-weight:600;padding:var(--sp-1) var(--sp-4);border-radius:10px;border:1px solid #4589ff;color:#4589ff">IBM bobshell</span>
            <span style="flex:1"></span>
            ${configured ? '<button class="hv-btn btn-primary btn-sm" data-action="bobKeyTest" title="Validate the saved key against the bob backend — the key value never leaves this hive">Test</button>' : ''}
            ${configured ? '<button class="hv-btn btn-danger btn-sm" data-action="bobKeyClear">Clear key</button>' : ''}
            <button class="hv-btn btn-primary btn-sm" data-action="bobKeySet">${configured ? 'Replace key' : 'Set API key'}</button>
          </div>
          <div style="font-size:0.78rem;margin-bottom:var(--sp-4)">${statusLine}</div>
          <div class="config-field">
            <label>Bob session prefix ${settingHelpMark('backend-support-tiers', 'tagging-bob-sessions-for-bobalytics', 'Bob session prefix', `Optional prefix for Bob --instance-id labels shown by Bobalytics. It does not rename Hive agents.`)}</label>
            <input type="text" id="gov-bob-session-prefix" value="${esc(sessionPrefix || '')}" placeholder="e.g. hive-" data-change-action="markDirty" data-arg0="bob" data-arg1="sessionPrefix" data-arg-types="s,s,w">
            <div class="text-muted">Default off. When set, bob agents without their own label report as <code>${esc((sessionPrefix || 'hive-') + 'scanner')}</code> in Bobalytics; their Hive name, tmux session, HOME, workdir, beads, claims and token buckets do not change.</div>
          </div>
          <div style="font-size:0.74rem;color:var(--muted);line-height:1.5">
            bob cannot complete browser sign-in inside a pod (its callback resolves to
            the pod, not your machine) and offers no device-code flow, so an API key is
            its only headless credential. Saving a key takes effect immediately — no pod
            restart is needed — and any bob agent already parked for a missing key is
            restarted automatically as part of the save, in a fresh session that picks
            up the new key.
          </div>
          <div id="bob-key-inline-status" style="font-size:0.74rem;min-height:18px;margin-top:var(--sp-4)"></div>
        </div>`;

      // data-* + addEventListener rather than inline handlers. Bound here (not
      // via the agent-card dispatcher) because this panel is repainted on every
      // set/clear and owns its own buttons.
      const setBtn = host.querySelector('[data-action="bobKeySet"]');
      if (setBtn) setBtn.addEventListener('click', () => openBobKeyDialog(configured, source, keyName));
      const clearBtn = host.querySelector('[data-action="bobKeyClear"]');
      if (clearBtn) clearBtn.addEventListener('click', clearBobKey);
      // Test the SAVED key from the card: result goes to the card's inline
      // status line (green ok / red reason + backend detail).
      const testBtn = host.querySelector('[data-action="bobKeyTest"]');
      if (testBtn) testBtn.addEventListener('click', async () => {
        const statusEl = document.getElementById('bob-key-inline-status');
        const setStatus = (msg, color) => {
          if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || 'var(--muted)'; }
        };
        testBtn.disabled = true;
        await testBobKey('', setStatus);
        testBtn.disabled = false;
      });
    }

    function renderGovAccess() {
      loadAuthorizedUsers();
      return `
        <div class="var-intro">
          Users permitted to sign in to this hive, and their roles. Access is managed
          on the hub's <b>Manage Access</b> screen and syncs here automatically — this
          view is read-only.
        </div>
        <div id="access-users-status" style="font-size:0.72rem;color:var(--muted);margin-bottom:var(--sp-4)"></div>
        <div id="access-users-list" tabindex="-1"><div class="var-empty">Loading…</div></div>`;
    }

    async function loadAuthorizedUsers() {
      // Resolve the targets AFTER the first await, never before. The caller does
      // `body.innerHTML = renderGovernorTab(tab)`, so renderGovAccess() — and this
      // function with it — runs while the PREVIOUS tab is still mounted. Looking
      // the elements up synchronously captured null every time, so both the success
      // and the catch branch wrote into nothing and the panel sat on "Loading…"
      // forever regardless of what the API returned.
      let listEl = null, statusEl = null;
      try {
        const r = await fetch('/api/config/authorized-users');
        listEl = document.getElementById('access-users-list');
        statusEl = document.getElementById('access-users-status');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        if (statusEl) {
          statusEl.innerHTML = d.enforced
            ? 'This hive enforces the allowlist below on sign-in.'
            : 'Sign-in is gated by the hub proxy; the allowlist below is informational.';
        }
        const users = d.users || [];
        if (!users.length) {
          listEl.innerHTML = '<div class="var-empty">No authorized users.</div>';
          return;
        }
        listEl.innerHTML = users.map(function(u) {
          var roleCls = u.role === 'owner' ? 'role-owner' : u.role === 'merger' ? 'role-merger' : u.role === 'read-write' ? 'role-read-write' : 'role-read';
          // u.username is the raw identity key (the actual auth key / allowlist
          // match — NEVER altered here). u.display_name is an OPTIONAL cosmetic
          // name the hub resolved (see AuthorizedUserNames); absent for a user
          // who hasn't completed a login yet, or whose provider returned no
          // name claim, in which case the raw key is shown exactly as before.
          var hasName = !!(u.display_name && u.display_name !== u.username);
          var primary = hasName ? u.display_name : u.username;
          // The raw key stays reachable even when a friendly name is shown:
          // the avatar title carries it always, and a muted secondary line
          // shows it beside the name for support/debugging.
          var secondary = hasName
            ? '<span style="display:block;font-size:var(--fs-xs);color:var(--muted);word-break:break-word" title="Auth key">' + esc(u.username) + '</span>'
            : '';
          return '<div class="config-list-item var-row">' +
            accessRowAvatar(u.username, u.display_name || '', ACCESS_LIST_AVATAR_PX) +
            '<span style="flex:1;min-width:0;font-size:0.85rem" title="' + esc(u.username) + '">' + esc(primary) + secondary + '</span>' +
            '<span class="role-badge ' + roleCls + '" style="font-size:0.7rem">' + esc(u.role) + '</span>' +
            '</div>';
        }).join('');
      } catch (e) {
        // fetch() itself can reject before the lookups above ran, so re-resolve
        // here rather than trusting listEl to be set — otherwise a network failure
        // is indistinguishable from a hang.
        const el = listEl || document.getElementById('access-users-list');
        if (el) el.innerHTML = '<div class="text-muted fs-sm">Could not load authorized users.</div>';
      }
    }

    function renderGovVariables() {
      loadVariables();
      return `
        <div class="var-intro">
          Define your own <code>\${NAME}</code> substitutions for kick templates and config.
          Four types are supported: <b>static</b>, <b>env</b>, <b>script</b>, and <b>http</b>.
          <b>static</b> and <b>env</b> can be managed here; <b>script</b> and <b>http</b> (which run
          commands or fetch URLs) are seed-only — see <i>Advanced</i> below.
        </div>
        <div id="variables-status" style="font-size:0.72rem;color:var(--muted);margin-bottom:var(--sp-4)"></div>
        <div id="variables-list"><div class="var-empty">Loading variables…</div></div>
        <div class="var-section">
          <div class="var-heading">Add a variable</div>
          ${variableAddFormHtml('var', 'saveNewVariable', ['template', 'config', 'both'])}
        </div>
        <details class="var-section">
          <summary class="var-summary">Advanced — script &amp; http variables (seed config only)</summary>
          <div style="font-size:0.78rem;color:var(--muted);line-height:1.55;margin-top:var(--sp-4)">
            Two more variable types back a <code>\${NAME}</code> with a live value:
            <ul style="margin:6px 0 6px 18px;padding:var(--sp-0)">
              <li><b>script</b> — the trimmed stdout of a command (e.g. <code>git rev-parse</code>).</li>
              <li><b>http</b> — the body of an HTTP GET to an allowlisted web service.</li>
            </ul>
            These run commands / reach the network, so they are <b>off by default</b> and
            can only be enabled and defined in the <b>ConfigMap seed</b> — never the dashboard.
            <div style="background:color-mix(in srgb, var(--amber) 12%, transparent);border:1px solid color-mix(in srgb, var(--amber) 35%, transparent);border-radius:var(--r);padding:8px 10px;margin:var(--sp-4) var(--sp-0)">
              <b>Where this goes:</b> the platform <b>ConfigMap</b> that seeds <code>/etc/hive/hive.yaml</code>
              (edit it via GitOps / <code>kubectl</code> and roll the pod). It will <b>not</b> take effect if you
              put it in the dashboard-saved overlay (<code>/data/hive.yaml.dashboard</code> on the PVC) — the
              loader deliberately ignores <code>variables.security</code> and <code>script</code>/<code>http</code>
              definitions from the overlay, because that file is writable from the dashboard and enabling
              code execution there would be a security hole.
            </div>
            <b>To enable:</b>
            <ol style="margin:6px 0 6px 18px;padding:var(--sp-0)">
              <li>In the ConfigMap's <code>hive.yaml</code>, set <code>variables.security.allow_exec</code> and/or
                <code>allow_http: true</code> (and an <code>http_allowlist</code> for http).</li>
              <li>Add your <code>script</code>/<code>http</code> entries under <code>variables.defs</code>.</li>
              <li>Apply the ConfigMap and restart the hive pod. The variables then appear read-only in this
                tab, and the status line above shows <i>enabled</i>.</li>
            </ol>
            <pre style="background:var(--surface,#111);border:1px solid var(--border,#2a2a2a);border-radius:var(--r);padding:10px;overflow:auto;font-size:0.72rem;margin-top:var(--sp-4)"><code>variables:
  security:
    allow_exec: true              # gate; ConfigMap seed only
    allow_http: true
    http_allowlist: [vault.internal.example.com]
  defs:
    BUILD_SHA:
      type: script
      command: [git, rev-parse, --short, HEAD]
    DB_HOST:
      type: http
      url: "https://vault.internal.example.com/v1/config/db-host"
      headers: { Authorization: "Bearer \${HIVE_VAULT_TOKEN}" }</code></pre>
            <a class="text-accent" href="https://kubestellar.io/docs/hive/variable-substitution" target="_blank">Full documentation →</a>
          </div>
        </details>`;
    }

    // Shared "Add a variable" form for the hive-level Variables tab (prefix
    // 'var') and the per-agent editor's Variables tab (prefix 'agent-var').
    // Field ids are `${prefix}-add-*`; saveAction is the data-action the button
    // dispatches; scopes lists the allowed scope options (a per-agent variable
    // only feeds its kick prompt, so it has no 'config' scope).
    function variableAddFormHtml(prefix, saveAction, scopes) {
      const p = esc(prefix);
      const scopeOpts = scopes.map(sc => `<option value="${esc(sc)}">${esc(sc)}</option>`).join('');
      return `
          <div class="config-field-row">
            <div class="config-field"><label>Name</label><input id="${p}-add-name" placeholder="MY_VAR" class="var-upper"></div>
            <div class="config-field"><label>Type</label>
              <select id="${p}-add-type" data-change-action="onVarAddTypeChange" data-arg0="${p}">
                <option value="static">static (fixed value)</option>
                <option value="env">env (environment variable)</option>
              </select>
            </div>
            <div class="config-field"><label>Scope</label>
              <select id="${p}-add-scope">${scopeOpts}</select>
            </div>
          </div>
          <div class="config-field-row">
            <div class="config-field" id="${p}-add-value-field"><label>Value</label><input id="${p}-add-value" placeholder="production"></div>
            <div class="config-field" id="${p}-add-env-field" hidden><label>Env var name</label><input id="${p}-add-env" placeholder="HIVE_REGION"></div>
            <div class="config-field"><label>Default (optional)</label><input id="${p}-add-default" placeholder="fallback if unset"></div>
          </div>
          <button class="hv-btn config-btn btn-primary" data-action="${esc(saveAction)}">Add variable</button>`;
    }

    function onVarAddTypeChange(prefix) {
      const p = prefix || 'var';
      const t = document.getElementById(p + '-add-type').value;
      const vf = document.getElementById(p + '-add-value-field');
      const ef = document.getElementById(p + '-add-env-field');
      vf.hidden = t === 'env';
      ef.hidden = t !== 'env';
    }

    // Reads the shared add form into { name, body } for a PUT, or toasts and
    // returns null when the name is missing.
    function readVariableAddForm(prefix) {
      const name = (document.getElementById(prefix + '-add-name').value || '').trim().toUpperCase();
      const type = document.getElementById(prefix + '-add-type').value;
      const scope = document.getElementById(prefix + '-add-scope').value;
      const def = (document.getElementById(prefix + '-add-default').value || '').trim();
      if (!name) { showToast('Enter a variable name', 'error'); return null; }
      const body = { type, scope };
      if (def) body.default = def;
      if (type === 'env') body.env = (document.getElementById(prefix + '-add-env').value || '').trim() || name;
      else body.value = document.getElementById(prefix + '-add-value').value || '';
      return { name, body };
    }

    function resetVariableAddForm(prefix) {
      ['name', 'value', 'env', 'default'].forEach(f => {
        const el = document.getElementById(prefix + '-add-' + f);
        if (el) el.value = '';
      });
    }

    // One value-free variable row (name/type/scope/source) with caller-supplied
    // trailing actions; shared by the hive-level and per-agent lists.
    function variableRowHtml(v, actionsHtml) {
      return `<div class="config-list-item var-row">
          <code class="var-code">\${${esc(v.name)}}</code>
          <span class="var-meta">${esc(v.type)}</span>
          <span class="var-meta">${esc(v.scope)}</span>
          <span class="var-source" title="${esc(v.source)}">${esc(v.source)}</span>
          ${actionsHtml}
        </div>`;
    }

    async function loadVariables() {
      // Same ordering hazard as loadAuthorizedUsers(): renderGovVariables() calls
      // this before its markup is assigned into the DOM, so resolve after the await.
      let listEl = null, statusEl = null;
      try {
        const r = await fetch('/api/config/variables');
        listEl = document.getElementById('variables-list');
        statusEl = document.getElementById('variables-status');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        if (statusEl) {
          statusEl.innerHTML = `script execution: <b>${d.exec_enabled ? 'enabled' : 'disabled'}</b> · http fetch: <b>${d.http_enabled ? 'enabled' : 'disabled'}</b> <span style="opacity:0.7">(seed-only)</span>`;
        }
        renderVariablesList(d.variables || []);
      } catch (e) {
        if (listEl) listEl.innerHTML = '<div class="text-muted fs-sm">Could not load variables.</div>';
      }
    }

    function renderVariablesList(vars) {
      const el = document.getElementById('variables-list');
      if (!el) return;
      if (vars.length === 0) {
        el.innerHTML = '<div class="var-empty">No variables defined.</div>';
        return;
      }
      el.innerHTML = vars.map(v => {
        const seedOnly = (v.type === 'script' || v.type === 'http');
        const del = seedOnly
          ? '<span class="var-badge" title="script/http variables are managed in the seed config">seed-managed</span>'
          : `<button class="hv-btn config-btn btn-danger btn-sm" data-action="deleteVariable" data-arg0="${esc(v.name)}">delete</button>`;
        return variableRowHtml(v, del);
      }).join('');
    }

    async function saveNewVariable() {
      const form = readVariableAddForm('var');
      if (!form) return;
      const name = form.name;
      try {
        const r = await fetch('/api/config/variables/' + encodeURIComponent(name), {
          method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(form.body)
        });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast(d.error || ('Failed (HTTP ' + r.status + ')'), 'error'); return; }
        showToast('Variable ${' + name + '} saved', 'success');
        resetVariableAddForm('var');
        loadVariables();
      } catch (e) { showToast('Failed to save variable', 'error'); }
    }

    async function deleteVariable(name) {
      try {
        const r = await fetch('/api/config/variables/' + encodeURIComponent(name), { method: 'DELETE' });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast(d.error || ('Failed (HTTP ' + r.status + ')'), 'error'); return; }
        showToast('Variable ${' + name + '} deleted', 'success');
        loadVariables();
      } catch (e) { showToast('Failed to delete variable', 'error'); }
    }

    // ── Per-agent editor → Variables tab (#11458) ──
    // Same model as the hive-level Variables tab above and the same shared
    // form/row helpers, backed by GET/PUT/DELETE
    // /api/config/agent/{name}/variables[/{var}]. The agent's own static/env
    // variables are editable; hive-level variables are listed read-only as
    // inherited, flagged when this agent overrides them (the agent's
    // definition wins in its kick prompt). Values are never returned.
    function renderAgentVariables() {
      loadAgentVariables();
      return `
        <div class="var-intro">
          <code>\${NAME}</code> substitutions for <b>this agent's</b> kick prompt only. An agent variable
          overrides a hive-level variable of the same name; everything else is inherited from the hive
          <i>Variables</i> tab. Only <b>static</b> and <b>env</b> variables can be set per agent.
        </div>
        <div class="var-heading">Agent variables</div>
        <div id="agent-variables-list"><div class="var-empty">Loading variables…</div></div>
        <div class="var-section">
          <div class="var-heading">Add or update an agent variable</div>
          ${variableAddFormHtml('agent-var', 'saveNewAgentVariable', ['template', 'both'])}
        </div>
        <div class="var-section">
          <div class="var-heading">Inherited from the hive (read-only)</div>
          <div id="agent-variables-inherited"></div>
        </div>`;
    }

    async function loadAgentVariables() {
      // Same ordering hazard as loadVariables(): the tab markup is assigned
      // into the DOM after this starts, so resolve the elements after the await.
      const agent = _configState.agent || '';
      let listEl = null;
      try {
        const r = await fetch('/api/config/agent/' + encodeURIComponent(agent) + '/variables');
        listEl = document.getElementById('agent-variables-list');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        renderAgentVariablesList(d.variables || [], d.inherited || []);
      } catch (e) {
        if (listEl) listEl.innerHTML = '<div class="text-muted fs-sm">Could not load variables.</div>';
      }
    }

    function renderAgentVariablesList(vars, inherited) {
      const el = document.getElementById('agent-variables-list');
      const inhEl = document.getElementById('agent-variables-inherited');
      const empty = (msg) => `<div class="var-empty">${msg}</div>`;
      if (el) {
        el.innerHTML = vars.length === 0 ? empty('No agent variables — this agent uses the hive-level ones below.') : vars.map(v => {
          const envName = (v.type === 'env' && (v.source || '').startsWith('env:')) ? v.source.slice(4) : '';
          const actions = `<button class="hv-btn config-btn btn-sm" data-action="editAgentVariable" data-arg0="${esc(v.name)}" data-arg1="${esc(v.type)}" data-arg2="${esc(v.scope)}" data-arg3="${esc(envName)}">edit</button>
            <button class="hv-btn config-btn btn-danger btn-sm" data-action="deleteAgentVariable" data-arg0="${esc(v.name)}">delete</button>`;
          return variableRowHtml(v, actions);
        }).join('');
      }
      if (inhEl) {
        inhEl.innerHTML = inherited.length === 0 ? empty('No hive-level variables defined.') : inherited.map(v => {
          const tag = v.overridden
            ? '<span class="var-badge var-badge-warn" title="this agent defines a variable with the same name, which wins">overridden</span>'
            : '<span class="var-badge" title="edit on the hive Variables tab">inherited</span>';
          return variableRowHtml(v, tag);
        }).join('');
      }
    }

    // Prefills the add form from an existing agent variable. Values are never
    // returned by the API, so a static value has to be re-entered.
    function editAgentVariable(name, type, scope, envName) {
      const set = (f, v) => { const el = document.getElementById('agent-var-add-' + f); if (el) el.value = v; };
      set('name', name);
      set('type', type === 'env' ? 'env' : 'static');
      set('scope', scope === 'both' ? 'both' : 'template');
      set('env', envName || '');
      set('value', '');
      onVarAddTypeChange('agent-var');
      const focus = document.getElementById(type === 'env' ? 'agent-var-add-env' : 'agent-var-add-value');
      if (focus) focus.focus();
      showToast('Editing ${' + name + '} — the current value is not shown; enter the new one', 'info');
    }

    async function saveNewAgentVariable() {
      const agent = _configState.agent || '';
      const form = readVariableAddForm('agent-var');
      if (!form) return;
      const name = form.name;
      try {
        const r = await fetch('/api/config/agent/' + encodeURIComponent(agent) + '/variables/' + encodeURIComponent(name), {
          method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(form.body)
        });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast(d.error || ('Failed (HTTP ' + r.status + ')'), 'error'); return; }
        showToast('Variable ${' + name + '} saved for ' + agent, 'success');
        resetVariableAddForm('agent-var');
        loadAgentVariables();
      } catch (e) { showToast('Failed to save variable', 'error'); }
    }

    async function deleteAgentVariable(name) {
      const agent = _configState.agent || '';
      try {
        const r = await fetch('/api/config/agent/' + encodeURIComponent(agent) + '/variables/' + encodeURIComponent(name), { method: 'DELETE' });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast(d.error || ('Failed (HTTP ' + r.status + ')'), 'error'); return; }
        showToast('Variable ${' + name + '} deleted from ' + agent, 'success');
        loadAgentVariables();
      } catch (e) { showToast('Failed to delete variable', 'error'); }
    }

    // ---- Model Gateways manager ----------------------------------------
    // Manages MULTIPLE named, OpenAI-compatible model gateways (OpenRouter,
    // a LiteLLM proxy, vLLM, llm-d, custom). Self-contained CRUD: it fetches
    // /api/config/governor/gateways, and List/Test/Delete/Save go straight to
    // the gateway endpoints (not the generic dirty/saveConfig flow). A
    // legacy-only hive shows its synthesized "litellm" gateway as one row.

    // Preset endpoint + label per gateway kind. Blank endpoint means the user
    // fills it (self-hosted / custom). OpenRouter has a well-known base URL.
    const GATEWAY_PRESETS = [
      { kind: 'openrouter', label: 'OpenRouter', endpoint: 'https://openrouter.ai/api/v1' },
      { kind: 'groq',       label: 'Groq',       endpoint: 'https://api.groq.com/openai/v1' },
      { kind: 'litellm',    label: 'LiteLLM',    endpoint: '' },
      { kind: 'vllm',       label: 'vLLM',       endpoint: '' },
      { kind: 'llm-d',      label: 'llm-d',      endpoint: '' },
      // watsonx: the OpenAI-compatible model-gateway base. hive appends
      // /v1/models and /v1/chat/completions to reach .../ml/gateway/v1/...
      // Replace <region> with your watsonx region (us-south, eu-de, jp-tok, …).
      { kind: 'watsonx',    label: 'watsonx',    endpoint: 'https://us-south.ml.cloud.ibm.com/ml/gateway' },
      { kind: 'custom',     label: 'Custom',     endpoint: '' },
    ];

    // Human-facing badge label + accent color per kind.
    const GATEWAY_KIND_META = {
      openrouter: { label: 'OpenRouter', color: 'var(--cyan)' },
      litellm:    { label: 'LiteLLM',    color: 'var(--accent)' },
      vllm:       { label: 'vLLM',       color: 'var(--green)' },
      'llm-d':    { label: 'llm-d',      color: 'var(--purple, #a78bfa)' },
      watsonx:    { label: 'watsonx',    color: 'var(--blue, #3b82f6)' },
      custom:     { label: 'Custom',     color: 'var(--muted)' },
    };

    // Sentinel for the "type a custom id" option in the gateway Default Model
    // select — cannot collide with a real model id.
    const GATEWAY_CUSTOM_MODEL_SENTINEL = '__custom__';

    // In-memory list of gateways currently rendered (from the last fetch), and
    // the models discovered for the add/edit form's endpoint+key.
    let _gateways = [];
    let _gatewayFormModels = [];
    const _gatewayProbeResults = {};
    // Set by maybeOpenMethodConfig when a method switch jumps the user here to
    // configure a missing gateway; consumed once by loadGateways to auto-open a
    // preset add-form. null when the tab was opened normally.
    let _pendingGatewayPresetKind = null;

    function gatewayKindMeta(kind) {
      return GATEWAY_KIND_META[kind] || GATEWAY_KIND_META.custom;
    }

    // ---- Forge App tab -----------------------------------------------------
    // Inventory of every forge App credential this spoke holds: the active
    // github config plus every per-app-id key file on the PVC. Fingerprints
    // and paths ONLY — private key material never reaches this page. Only the
    // config whose forge matches the forge this hive's repos live on is
    // editable; everything else renders read-only.
    function renderGovForgeApp() {
      return `
        <div style="font-size:var(--fs-base);color:var(--muted);margin-bottom:14px">
          Every forge App credential this hive holds. The active config is editable only
          when its forge matches the forge this hive's repos live on; everything else is
          read-only. Private keys appear as fingerprints — key material never leaves the hive.
        </div>
        <div id="forge-app-host"><div style="color:var(--muted);font-size:0.8rem">Loading forge Apps…</div></div>`;
    }

    async function loadForgeApps() {
      const host = document.getElementById('forge-app-host');
      try {
        const res = await fetch('/api/config/github/forge-apps');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        renderForgeApps(await res.json());
      } catch (e) {
        if (host) host.innerHTML = `<div style="color:var(--red);font-size:0.8rem">Failed to load forge Apps: ${esc(e.message)}</div>`;
      }
    }

    function forgeAppRow(label, valueHtml) {
      return `<div style="display:flex;gap:10px;font-size:0.78rem;padding:3px 0"><span style="min-width:140px;color:var(--muted);flex-shrink:0">${esc(label)}</span><span style="word-break:break-all">${valueHtml}</span></div>`;
    }

    function renderForgeApps(data) {
      const host = document.getElementById('forge-app-host');
      if (!host) return;
      const a = data.active || {};
      // The classified auth state is a debugging surface: show the wire token
      // verbatim (not-installed / wrong-installation / key-invalid / ...).
      const stateHtml = a.auth_state
        ? `<code style="font-size:0.74rem;padding:1px 6px;border:1px solid ${a.auth_state === 'ok' ? 'var(--green)' : 'var(--yellow)'};border-radius:var(--r)">${esc(a.auth_state)}</code>`
        : '<span class="text-muted">— (not classified)</span>';
      const editBadge = a.editable
        ? '<span style="font-size:var(--fs-xs);font-weight:600;padding:var(--sp-1) var(--sp-4);border-radius:10px;border:1px solid var(--green);color:var(--green)">editable</span>'
        : `<span style="font-size:var(--fs-xs);font-weight:600;padding:var(--sp-1) var(--sp-4);border-radius:10px;border:1px solid var(--border);color:var(--muted)">read-only — repos are on ${esc(data.repos_forge || 'github.com')}</span>`;
      let html = `
        <div style="border:1px solid var(--border);border-radius:8px;padding:12px 14px;margin-bottom:var(--sp-5);background:var(--surface)">
          <div style="display:flex;align-items:center;gap:10px;margin-bottom:var(--sp-4);flex-wrap:wrap">
            <span style="font-weight:600;font-size:0.9rem">Active config — ${esc(a.forge || 'github.com')}</span>
            ${editBadge}
          </div>
          ${forgeAppRow('App ID', esc(String(a.app_id || 0)))}
          ${forgeAppRow('App slug', esc(a.app_slug || '—'))}
          ${forgeAppRow('API URL', esc(a.api_url || '—'))}
          ${forgeAppRow('Base URL', esc(a.base_url || '—'))}
          ${forgeAppRow('Installation ID', esc(String(a.installation_id || 0)))}
          ${forgeAppRow('Key file', esc(a.key_file || '—'))}
          ${forgeAppRow('Key fingerprint', a.key_fingerprint ? '<code style="font-size:0.74rem">' + esc(a.key_fingerprint) + '</code>' : '<span class="text-muted">— (no usable key)</span>')}
          ${forgeAppRow('Auth state', stateHtml)}
          ${forgeAppActions(a)}
          ${a.editable ? forgeAppEditForm(a) : ''}
        </div>`;
      html += '<div style="font-weight:600;font-size:0.85rem;margin:14px 0 6px">Per-app-id keys on disk</div>';
      const keys = data.held_keys || [];
      if (!keys.length) {
        html += '<div style="color:var(--muted);font-size:0.78rem">No per-app-id key files held on this hive.</div>';
      } else {
        html += keys.map(k => `
          <div style="border:1px solid var(--border);border-radius:8px;padding:var(--sp-4) var(--sp-5);margin-bottom:var(--sp-3);font-size:0.76rem;background:var(--surface)">
            <div style="display:flex;gap:10px;align-items:center;flex-wrap:wrap">
              <span style="font-weight:600">App ${esc(k.app_id)}</span>
              <span style="font-size:0.66rem;font-weight:600;padding:1px 7px;border-radius:10px;border:1px solid var(--border);color:var(--muted)">read-only</span>
            </div>
            <div style="color:var(--muted);word-break:break-all;margin-top:var(--sp-2)">${esc(k.path)}</div>
            <div class="mt-1">fingerprint: <code style="font-size:0.72rem">${esc(k.fingerprint)}</code></div>
          </div>`).join('');
      }
      html += renderForgeGitHubSettings(data.settings || {});
      host.innerHTML = html;
    }

    // renderForgeGitHubSettings shows the rest of the github: block. Identity
    // keys and the token are display-only: they decide which forge and
    // credentials the hive authenticates with, so they come from hub
    // provisioning (or hive.yaml on a standalone hive). The token is never
    // sent to the browser, only whether one is set.
    let _forgeGitHubSettings = {};
    function renderForgeGitHubSettings(st) {
      _forgeGitHubSettings = st;
      const d = st.defaults || {};
      const owner = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const managedNote = st.hub_managed
        ? 'managed by the hub — change it from the hub dashboard'
        : 'set in hive.yaml; requires restart';
      const roRow = (label, path, valueHtml) => `<div class="config-field"><label>${esc(label)} ${yamlHint(path)}</label><div class="fs-sm break-all">${valueHtml} <span class="text-muted">— ${esc(managedNote)}</span></div></div>`;
      const shown = v => v ? '<code>' + esc(String(v)) + '</code>' : '<span class="text-muted">unset</span>';
      const num = (key, label, path, placeholder, help, restart, mark) => `
          <div class="config-field">
            <label for="forge-gh-${key}">${esc(label)} ${yamlHint(path)} ${mark}</label>
            <input id="forge-gh-${key}" type="number" min="0" step="1" value="${st[key] ? esc(String(st[key])) : ''}" placeholder="${esc(String(placeholder))}" ${owner ? '' : 'disabled'}>
            <div class="config-help">${help}${restart ? ' Requires restart.' : ' Applies without a restart.'}</div>
          </div>`;
      const toggle = (key, label, path, help, restart, mark) => `
          <div class="config-toggle mt-3">
            <div id="forge-gh-${key}" class="config-toggle-switch ${st[key] ? 'on' : ''} ${owner ? '' : 'disabled'}" data-action="${owner ? 'forgeGitHubToggle' : ''}" data-arg-types="t" role="switch" aria-checked="${st[key] ? 'true' : 'false'}"></div>
            <span class="config-toggle-label">${esc(label)} ${yamlHint(path)} ${mark} <span class="text-muted">— ${help}${restart ? ' Requires restart.' : ' Applies without a restart.'}</span></span>
          </div>`;
      return `
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Connection identity</div>
          ${roRow('Forge', 'github.forge', shown(st.forge))}
          ${roRow('API URL', 'github.api_url', shown(st.api_url))}
          ${roRow('Docs org installation ID', 'github.docs_installation_id', shown(st.docs_installation_id))}
          ${roRow('Login OAuth client ID', 'github.oauth_client_id', shown(st.oauth_client_id))}
          ${roRow('Personal access token', 'github.token', st.token_set ? '<span>set (hidden)</span>' : '<span class="text-muted">not set</span>')}
          <div class="config-help">The App private key path is <code>github.key_file</code>, edited above. An empty forge or API URL is derived from the App config.</div>
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">API quota, PR scans and authorship</div>
          <div class="config-field-row">
            ${num('api_reserve', 'API reserve', 'github.api_reserve', 800, 'Remaining core REST quota below which Hive skips optional sweeps (conserve mode). 0 uses the default (800).', true, settingHelpMark('operator-reference', 'github-api-budget-governor', 'API reserve', 'Below this many remaining core REST calls the governor enters conserve mode: optional sweeps are skipped and the eval interval is multiplied. 0 restores the default (800).'))}
            ${num('api_critical', 'API critical floor', 'github.api_critical', 250, 'Lower floor where Hive enters critical mode and stretches the eval interval further. Must be below the reserve. 0 uses the default (250).', true, settingHelpMark('operator-reference', 'github-api-budget-governor', 'API critical floor', 'Below this many remaining calls the governor also sheds unstick steps and stretches the eval interval 4x. Must stay below the reserve; 0 restores the default (250).'))}
            ${num('agent_reserve_floor', 'Agent reserve floor', 'github.agent_reserve_floor', d.agent_reserve_floor || 400, 'Agent reads through the API proxy get a 429 once the shared core bucket drops below this, so daemon merge and scan work is never starved.', true, settingHelpMark('operator-reference', 'github-api-quota', 'Agent reserve floor', 'Protects the shared core bucket from agent traffic: once the latest rate-limit reading drops below the floor, agent reads through the API proxy are refused with a 429 until the bucket recovers. 0 uses the default.'))}
          </div>
          <div class="config-field-row">
            ${num('pr_detail_ttl_s', 'PR detail cache TTL (seconds)', 'github.pr_detail_ttl_s', d.pr_detail_ttl_s || 1800, 'Reuses a PR detail response while the PR list still reports the same head SHA and updated_at. 0 uses the default.', false, settingHelpMark('operator-reference', 'notable-fields', 'PR detail cache TTL', 'How long a GET /pulls/{number} detail response is reused while the cheap PR list still reports the same head SHA and updated_at and GitHub has resolved mergeable_state. 0 uses the default.'))}
            ${num('graphql_pr_batch_page_size', 'GraphQL PR batch page size', 'github.graphql_pr_batch_page_size', d.graphql_pr_batch_page_size || 50, 'Page size for the batched pullRequests query, 1–100. 0 uses the default.', false, settingHelpMark('operator-reference', 'notable-fields', 'GraphQL PR batch page size', 'Page size for the GraphQL pullRequests(first:) batch query. Values above GitHub&#39;s maximum of 100 are rejected; 0 uses the default.'))}
          </div>
          ${toggle('graphql_pr_batch', 'Batch open-PR scans with GraphQL', 'github.graphql_pr_batch', 'one paginated query per repo instead of per-PR detail and check-run reads.', false, settingHelpMark('operator-reference', 'notable-fields', 'Batch open-PR scans with GraphQL', 'On by default. One paginated GraphQL query per repository scan fills the PR detail and CI rollup caches, replacing most per-PR detail and check-run reads. Off returns to the REST-only scan path.'))}
          ${toggle('app_authored_prs', 'Author agent PRs as the App bot', 'github.app_authored_prs', 'agents open PRs and commit as &lt;slug&gt;[bot] with the App installation token when project.ai_author is empty.', true, settingHelpMark('github-app-setup', '', 'Author agent PRs as the App bot', 'With a GitHub App installed, agents open PRs and commit as &lt;slug&gt;[bot] using the installation token whenever project.ai_author is empty, so PR authorship is the App rather than a personal account.'))}
          ${toggle('app_signed_commits', 'Re-author agent PRs as GitHub-signed commits', 'github.app_signed_commits', 'needed when a base branch requires signed commits; rewrites the agent branch before the PR opens.', false, settingHelpMark('github-app-setup', 'signed-commits-on-agent-prs-app_signed_commits', 'Re-author agent PRs as GitHub-signed commits', 'Opt-in; needs an installed App. Before a PR opens, the watcher squashes the agent branch into one commit created through the GraphQL createCommitOnBranch mutation, which GitHub signs and shows as Verified, so a required_signatures ruleset accepts it. Unsigned follow-up commits are reconciled the same way.'))}
          ${owner ? '<button class="hv-btn btn-primary mt-4" id="forge-gh-save-btn" data-action="saveForgeGitHubSettings">Save</button>' : '<div class="config-help mt-3">Owner role required to change these settings.</div>'}
        </div>`;
    }

    function forgeGitHubToggle(el) {
      if (!el || el.classList.contains('disabled')) return;
      const on = el.classList.toggle('on');
      el.setAttribute('aria-checked', on ? 'true' : 'false');
    }

    async function saveForgeGitHubSettings() {
      const st = _forgeGitHubSettings || {};
      const body = {};
      for (const key of ['api_reserve', 'api_critical', 'agent_reserve_floor', 'pr_detail_ttl_s', 'graphql_pr_batch_page_size']) {
        const el = document.getElementById('forge-gh-' + key);
        if (!el) continue;
        const raw = (el.value || '').trim();
        const v = raw === '' ? 0 : parseInt(raw, 10);
        if (isNaN(v) || v < 0) { showToast(key + ' must be a non-negative number', 'error'); return; }
        if (v !== (st[key] || 0)) body[key] = v;
      }
      for (const key of ['graphql_pr_batch', 'app_authored_prs', 'app_signed_commits']) {
        const el = document.getElementById('forge-gh-' + key);
        if (!el) continue;
        const on = el.classList.contains('on');
        if (on !== !!st[key]) body[key] = on;
      }
      if (!Object.keys(body).length) { showToast('Nothing to save', 'error'); return; }
      const btn = document.getElementById('forge-gh-save-btn');
      if (btn) { btn.disabled = true; btn.textContent = 'Saving…'; }
      try {
        const res = await fetch('/api/config/github', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body)
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
        const restart = ['api_reserve', 'api_critical', 'agent_reserve_floor', 'app_authored_prs'].some(k => k in body);
        showToast(restart ? 'GitHub settings saved — restart the hive to apply the quota floors or authorship change' : 'GitHub settings saved', 'success');
        loadForgeApps();
      } catch (e) {
        showToast('Save failed: ' + e.message, 'error');
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Save'; }
      }
    }

    // forgeAppNeedsInstall: PRIMARY rule, checked FIRST and sufficient BY
    // ITSELF — a real App (app_id set) with no installation_id needs
    // installing, full stop. No auth-state classification or recheck is
    // required before offering the link: a blank id after a "Reset Forge App"
    // must light the install affordance immediately on page load. The
    // classified states are an ADDITIONAL trigger for the case where an id
    // exists but names the wrong (or a vanished) installation.
    function forgeAppNeedsInstall(a) {
      if (a.app_id && !a.installation_id) return true;
      return a.auth_state === GH_APP_STATE_NOT_INSTALLED
        || a.auth_state === GH_APP_STATE_WRONG_INSTALLATION;
    }

    // forgeAppActions renders the install/settings affordance under the
    // Active config table, so the tab is never a dead end. The URL is the
    // SERVER-built install_url (config.AppInstallURL(), the same builder the
    // install banner uses) — the tab must never compose its own URL scheme.
    // When an installation is present and nothing is classified as broken, a
    // subtle "View App settings" link renders instead (GitHub redirects the
    // installations/new page to the App's settings when already installed —
    // the same behavior the banner's View App Settings action relies on).
    function forgeAppActions(a) {
      const hostLabel = esc(String(a.base_url || 'https://github.com').replace(/^https?:\/\//, ''));
      const recheckBtn = '<button class="hv-btn btn-primary btn-sm" type="button" data-action="forgeAppRecheck" data-arg-types="t">Re-check</button>';
      if (forgeAppNeedsInstall(a)) {
        if (a.install_url) {
          return `<div id="forge-app-actions" style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;border-top:1px solid var(--border);margin-top:10px;padding-top:10px">
              <a href="${esc(a.install_url)}" target="_blank" rel="noopener noreferrer" style="display:inline-block;padding:7px 16px;border:none;border-radius:var(--r);background:var(--cyan);color:var(--bg);font-weight:600;font-size:0.8rem;text-decoration:none">Install GitHub App on ${hostLabel}</a>
              ${recheckBtn}
              <span style="font-size:0.74rem;color:var(--muted)">No installation is linked${a.installation_id ? '' : ' (Installation ID is 0)'} — install the App, then Re-check.</span>
            </div>`;
        }
        return `<div id="forge-app-actions" style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;border-top:1px solid var(--border);margin-top:10px;padding-top:10px">
            <span style="font-size:0.76rem;color:var(--yellow)">The GitHub App is not installed, and no install link can be built: the App slug for ${hostLabel} is not configured. Ask your hub operator to set github.app_slug for this cluster.</span>
            ${recheckBtn}
          </div>`;
      }
      if (a.install_url && a.installation_id) {
        return `<div id="forge-app-actions" style="border-top:1px solid var(--border);margin-top:10px;padding-top:10px">
            <a href="${esc(a.install_url)}" target="_blank" rel="noopener noreferrer" style="font-size:0.76rem;color:var(--muted);text-decoration:underline">View App settings on ${hostLabel}</a>
          </div>`;
      }
      return '';
    }

    // forgeAppRecheck reuses the banner's recheck endpoint, then re-renders
    // the tab from server truth (auth_state/installation_id may have moved).
    async function forgeAppRecheck(btn) {
      if (btn) { btn.disabled = true; btn.textContent = 'Checking…'; }
      try {
        await fetch('/api/github-app/recheck', { method: 'POST' });
      } catch (e) { /* the reload below reports current truth either way */ }
      loadForgeApps();
    }

    function forgeAppEditForm(a) {
      const inputStyle = 'width:100%;padding:var(--sp-3) var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg);font-size:0.78rem;box-sizing:border-box';
      return `
        <div style="border-top:1px solid var(--border);margin-top:10px;padding-top:10px">
          <div style="font-size:0.78rem;font-weight:600;margin-bottom:var(--sp-4)">Edit active config</div>
          <div style="display:grid;grid-template-columns:1fr 1fr;gap:var(--sp-4);margin-bottom:var(--sp-4)">
            <label style="font-size:0.72rem;color:var(--muted)">App ID<input id="forge-app-appid" type="number" value="${esc(String(a.app_id || ''))}" style="${inputStyle}"></label>
            <label style="font-size:0.72rem;color:var(--muted)">Installation ID<input id="forge-app-instid" type="number" value="${esc(String(a.installation_id || ''))}" style="${inputStyle}"></label>
          </div>
          <label style="font-size:0.72rem;color:var(--muted)">Key file path ${yamlHint('github.key_file')}<input id="forge-app-keyfile" type="text" value="${esc(a.key_file || '')}" style="${inputStyle};margin-bottom:var(--sp-4)"></label>
          <label style="font-size:0.72rem;color:var(--muted)">Private key (PEM — write-only, replaces the key file; never displayed back)<textarea id="forge-app-pem" rows="3" placeholder="-----BEGIN RSA PRIVATE KEY----- (leave empty to keep the current key)" style="${inputStyle};font-family:monospace;margin-bottom:var(--sp-4)"></textarea></label>
          <button class="hv-btn btn-primary" id="forge-app-save-btn" data-action="saveForgeApp">Save</button>
        </div>`;
    }

    async function saveForgeApp() {
      const btn = document.getElementById('forge-app-save-btn');
      const numVal = id => {
        const el = document.getElementById(id);
        const v = (el && el.value || '').trim();
        return v === '' ? null : parseInt(v, 10);
      };
      const appId = numVal('forge-app-appid');
      const instId = numVal('forge-app-instid');
      const keyFileEl = document.getElementById('forge-app-keyfile');
      const pemEl = document.getElementById('forge-app-pem');
      const keyFile = (keyFileEl && keyFileEl.value || '').trim();
      const pem = (pemEl && pemEl.value || '').trim();
      const body = {};
      if (appId !== null) {
        if (isNaN(appId) || appId <= 0) { showToast('App ID must be a positive number', 'error'); return; }
        body.app_id = appId;
      }
      if (instId !== null) {
        if (isNaN(instId) || instId < 0) { showToast('Installation ID must be a non-negative number', 'error'); return; }
        body.installation_id = instId;
      }
      if (keyFile) body.key_file = keyFile;
      if (pem) body.private_key = pem;
      if (!Object.keys(body).length) { showToast('Nothing to save', 'error'); return; }
      if (btn) { btn.disabled = true; btn.textContent = 'Saving…'; }
      try {
        // The existing config-github handler: persists, then live-reinits the
        // GitHub client when the config becomes usable.
        const res = await fetch('/api/config/github', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body)
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
        showToast(data.reinit === 'ok' ? 'Forge App config saved and reconnected' : 'Forge App config saved', 'success');
        loadForgeApps();
      } catch (e) {
        showToast('Save failed: ' + e.message, 'error');
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = 'Save'; }
      }
    }

    function renderGovGateways() {
      // Container the async loadGateways() fills; the initial paint shows a
      // loading line so the tab is never blank while the fetch is in flight.
      return `
        <div style="font-size:var(--fs-base);color:var(--muted);margin-bottom:14px">
          Configure the OpenAI-compatible model gateways this hive can route through
          (OpenRouter, a LiteLLM proxy, vLLM, llm-d, or a custom endpoint). An agent
          uses a gateway by naming it as its backend. Keys are stored privately on this
          hive — never in hive.yaml, logs, or config downloads.
        </div>
        <div data-copilot-backend-help style="border:1px solid var(--border);border-radius:8px;padding:12px 14px;margin-bottom:14px;background:var(--surface);font-size:0.8rem;line-height:1.5">
          <div style="font-weight:600;color:var(--fg);margin-bottom:3px">Looking for Copilot, Claude, Codex, or Gemini?</div>
          <div class="text-muted">
            Copilot is a subscription CLI backend, not a Model Gateway, so it will not
            appear in this list — the same is true for Claude, Codex, Gemini, and other
            agent CLI backends. Open an agent's <strong>⚙ General</strong> tab, enable
            <strong>CLI Pinned</strong>, choose <strong>Copilot</strong> under
            <strong>CLI Pin Value</strong>, and save. Then use <strong>Login</strong> on
            that agent's card to authenticate this hive.
            <a href="https://github.com/hivecommons/hive/blob/v4/docs/inference-backends.md" target="_blank" rel="noopener noreferrer" style="color:var(--cyan);font-weight:600;white-space:nowrap">Copilot setup guide ↗</a>
          </div>
        </div>
        <div style="margin-bottom:var(--sp-5);display:flex;gap:var(--sp-4);flex-wrap:wrap">
          <button class="hv-btn btn-primary" data-action="openGatewayForm">+ Add gateway</button>
          <button class="hv-btn btn-primary" data-action="openOpenRouterFund">⚡ Fund this hive with OpenRouter</button>
        </div>
        <div id="openrouter-fund-host"></div>
        <div id="gateways-form-host"></div>
        <div id="gateways-list-host"><div style="color:var(--muted);font-size:0.8rem">Loading gateways…</div></div>
        <div id="inference-auth-host"><div style="color:var(--muted);font-size:0.8rem">Loading inference auth…</div></div>
        ${renderGovLegacyLiteLLM()}`;
    }

    // renderGovLegacyLiteLLM exposes the legacy governor.litellm block — the
    // "legacy endpoint" route diagnostics name — on the Model Gateways tab.
    // Its standalone LiteLLM tab was retired, which left that endpoint, key
    // and default model editable only in hive.yaml while Test results still
    // told operators to reconcile it.
    function renderGovLegacyLiteLLM() {
      const litellm = (_configState.data && _configState.data.litellm) || {};
      const diverged = legacyLiteLLMDivergence(litellm);
      return `
        <details id="legacy-litellm-section" class="card-inset mt-4"${diverged ? ' open' : ''}>
          <summary class="fw-semibold">${diverged ? '⚠ ' : ''}Legacy LiteLLM endpoint (governor.litellm)${litellm.endpoint ? ' — ' + esc(litellm.endpoint) : ' — not set'}</summary>
          <div id="legacy-litellm-divergence">${renderLegacyLiteLLMDivergence(diverged)}</div>
          <p class="text-muted fs-sm mt-2">
            The pre-gateways LiteLLM settings for the built-in <code>litellm</code> backend. When a gateway named
            <code>litellm</code> exists in the list above, agents use that gateway and this block only backs older
            code paths; saving that gateway with a new endpoint repoints this block too. Otherwise this is the
            endpoint, key and default model agents on the <code>litellm</code> backend use. The
            <code>HIVE_LITELLM_ENDPOINT</code> environment variable overrides the endpoint here. Clear the endpoint
            if it names a gateway you no longer use. Changes apply when you press Save.
          </p>
          ${renderGovLiteLLM(litellm)}
        </details>`;
    }

    // legacyLiteLLMDivergence returns {legacy, gateway} when a gateway named
    // "litellm" is configured AND the legacy governor.litellm.endpoint points
    // somewhere else — the #9945 state, where pre-#11794 builds sent the
    // gateway's key to the legacy URL. null when they agree, when either side
    // is unset, or when the gateway list has not loaded yet.
    function legacyLiteLLMDivergence(litellm) {
      const legacyURL = String(litellm && litellm.endpoint || '').trim();
      if (!legacyURL) return null;
      const gw = (_gateways || []).find(g => g && String(g.name || '').toLowerCase() === 'litellm');
      const gatewayURL = gw ? String(gw.endpoint || '').trim() : '';
      if (!gatewayURL) return null;
      const norm = u => u.replace(/\/+$/, '').toLowerCase();
      if (norm(legacyURL) === norm(gatewayURL)) return null;
      return { legacy: legacyURL, gateway: gatewayURL };
    }

    function renderLegacyLiteLLMDivergence(diverged) {
      if (!diverged) return '';
      return `<div class="fs-sm mt-2 text-danger" data-legacy-litellm-diverged>
            ⚠ This legacy endpoint (<code>${esc(diverged.legacy)}</code>) is not the <code>litellm</code> gateway listed above
            (<code>${esc(diverged.gateway)}</code>). Hives on builds before #11794 send the gateway's key to this legacy URL, so
            inference fails with a 401 even though the gateway's Test passes. Set the Endpoint URL below to
            <code>${esc(diverged.gateway)}</code> or clear it, then press Save.
          </div>`;
    }

    // refreshLegacyLiteLLMDivergence re-evaluates the warning once the async
    // gateway list arrives (renderGovLegacyLiteLLM paints before loadGateways
    // resolves) and after a gateway save repoints the legacy block.
    function refreshLegacyLiteLLMDivergence() {
      const section = document.getElementById('legacy-litellm-section');
      if (!section) return;
      const litellm = (_configState.data && _configState.data.litellm) || {};
      const diverged = legacyLiteLLMDivergence(litellm);
      const host = document.getElementById('legacy-litellm-divergence');
      if (host) host.innerHTML = renderLegacyLiteLLMDivergence(diverged);
      const summary = section.querySelector('summary');
      if (summary) {
        const text = summary.textContent.replace(/^⚠\s*/, '');
        summary.textContent = (diverged ? '⚠ ' : '') + text;
      }
      if (diverged) section.open = true;
    }

    // ---- vLLM / llm-d discovery auth (#4217) --------------------------------
    // governor.vllm / governor.llm-d (InferenceAuthConfig) were YAML-only while
    // LiteLLM auth is exposed above. These sections take key REFERENCES only —
    // a header NAME, an env var NAME, and an optional endpoint override — never
    // a key value, matching the server's secret-free overlay contract.
    async function loadInferenceAuth() {
      const host = document.getElementById('inference-auth-host');
      if (!host) return;
      try {
        const res = await fetch('/api/config/governor/inference-auth');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        host.innerHTML = renderInferenceAuthSection('vllm', 'vLLM', data.vllm || {})
          + renderInferenceAuthSection('llmd', 'llm-d', data.llmd || {});
      } catch (e) {
        host.innerHTML = `<div style="color:var(--red);font-size:0.8rem">Failed to load inference auth: ${esc(e.message)}</div>`;
      }
    }

    function renderInferenceAuthSection(id, label, cfg) {
      const defEnv = id === 'vllm' ? 'VLLM_API_KEY' : 'LLMD_API_KEY';
      return `
        <div style="border:1px solid var(--border);border-radius:8px;padding:12px 14px;margin-bottom:10px;background:var(--surface)">
          <div style="font-weight:600;font-size:0.9rem;margin-bottom:var(--sp-2)">${esc(label)} discovery auth</div>
          <div style="font-size:0.74rem;color:var(--muted);margin-bottom:10px">
            Optional auth for the ${esc(label)} backend's <code>/v1/models</code> discovery. Plain ${esc(label)}
            servers need no key, but the endpoint may be a key-gated gateway. References only — the key value is
            never stored in config.
          </div>
          <div class="config-field" style="margin-bottom:10px">
            <label>API key header ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'API key header', `Header NAME the key is sent in. Leave blank for the default (Authorization).`)}</label>
            <input type="text" value="${esc(cfg.api_key_header || '')}" placeholder="Authorization" data-change-action="saveInferenceAuthField" data-arg0="${esc(id)}" data-arg1="api_key_header" data-arg-types="s,s,v" style="width:100%">
          </div>
          <div class="config-field" style="margin-bottom:10px">
            <label>API key env var ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'API key env var', `Environment variable NAME holding the key — never paste the key value here.`)}</label>
            <input type="text" value="${esc(cfg.api_key_env || '')}" placeholder="${esc(defEnv)}" data-change-action="saveInferenceAuthField" data-arg0="${esc(id)}" data-arg1="api_key_env" data-arg-types="s,s,v" style="width:100%">
          </div>
          <div class="config-field mb-2">
            <label>Endpoint override (optional) ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'Endpoint override (optional)', `OpenAI-compatible base URL used instead of the default for this backend. Leave blank to keep the default.`)}</label>
            <input type="text" value="${esc(cfg.endpoint || '')}" placeholder="https://…/v1" data-change-action="saveInferenceAuthField" data-arg0="${esc(id)}" data-arg1="endpoint" data-arg-types="s,s,v" style="width:100%">
          </div>
        </div>`;
    }

    async function saveInferenceAuthField(backend, field, value) {
      const payload = {};
      payload[backend] = {};
      payload[backend][field] = value.trim();
      try {
        const res = await fetch('/api/config/governor/inference-auth', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify(payload)
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          showToast(data.error || ('Failed to save (HTTP ' + res.status + ')'), 'error');
          return;
        }
        showToast((backend === 'vllm' ? 'vLLM' : 'llm-d') + ' auth saved', 'success');
      } catch (e) {
        showToast('Failed to save: ' + e.message, 'error');
      }
    }

    // ---- OpenRouter scan-to-fund -------------------------------------------
    // Fund THIS hive: pick a default model, then scan a QR (or click the link)
    // to authorize on openrouter.ai via OAuth PKCE. On return the hive stores a
    // scoped OpenRouter key as the "openrouter" gateway. The QR image is a
    // same-origin Go-generated PNG (CSP-clean). We poll /api/openrouter/credit
    // to detect the connected key and show remaining credit.
    let _openrouterPoll = null;

    async function openOpenRouterFund() {
      const host = document.getElementById('openrouter-fund-host');
      if (!host) return;
      // Curated + live model options for the picker.
      let models = { suggested: [], models: [], default: '' };
      try {
        const r = await fetch('/api/openrouter/models');
        if (r.ok) models = await r.json();
      } catch (e) { /* fall back to manual entry */ }
      const sug = (models.suggested || []);
      const opts = sug.map(m => `<option value="${esc(m.id)}">${esc(m.label)}</option>`).join('')
        + `<option value="__manual__">Enter a model id manually…</option>`;
      host.innerHTML = `
        <div style="border:1px solid var(--cyan);border-radius:8px;padding:14px;margin-bottom:var(--sp-5);background:var(--surface)">
          <div style="font-weight:600;font-size:0.9rem;margin-bottom:var(--sp-2)">⚡ Fund this hive with OpenRouter</div>
          <div style="font-size:0.76rem;color:var(--muted);margin-bottom:10px">Scan the QR (or open the link) to authorize on OpenRouter. A scoped key funded from your OpenRouter credits is stored as the <code>openrouter</code> gateway — the key never leaves this hive.</div>
          <label style="display:block;font-size:0.74rem;color:var(--muted);margin-bottom:var(--sp-2)">Default model for the new gateway</label>
          <select id="or-model-select" data-change-action="orModelSelectChange" style="width:100%;padding:7px;background:var(--bg);color:var(--fg);border:1px solid var(--border);border-radius:var(--r);margin-bottom:var(--sp-4)">${opts}</select>
          <input id="or-model-manual" placeholder="e.g. anthropic/claude-opus-4.8" style="display:none;width:100%;padding:7px;background:var(--bg);color:var(--fg);border:1px solid var(--border);border-radius:var(--r);margin-bottom:var(--sp-4);box-sizing:border-box">
          <div style="display:flex;gap:var(--sp-4);margin-top:var(--sp-2)">
            <button class="hv-btn btn-primary" data-action="orStartFlow">Generate QR</button>
            <button class="hv-btn btn-primary" data-action="closeOpenRouterFund">Cancel</button>
          </div>
          <div class="mt-5" id="or-qr-area"></div>
          <div id="or-status" style="margin-top:var(--sp-4);font-size:0.78rem"></div>
        </div>`;
    }

    function orModelSelectChange() {
      const sel = document.getElementById('or-model-select');
      const man = document.getElementById('or-model-manual');
      if (!sel || !man) return;
      man.style.display = sel.value === '__manual__' ? 'block' : 'none';
    }

    function orChosenModel() {
      const sel = document.getElementById('or-model-select');
      if (!sel) return '';
      if (sel.value === '__manual__') {
        const man = document.getElementById('or-model-manual');
        return (man && man.value.trim()) || '';
      }
      return sel.value;
    }

    async function orStartFlow() {
      const model = orChosenModel();
      const qrArea = document.getElementById('or-qr-area');
      const status = document.getElementById('or-status');
      if (qrArea) qrArea.innerHTML = '<span style="color:var(--muted);font-size:0.78rem">Preparing…</span>';
      try {
        const res = await fetch('/api/openrouter/connect/start?model=' + encodeURIComponent(model));
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        const authURL = data.authorize_url;
        const qrSrc = '/api/openrouter/qr?data=' + encodeURIComponent(authURL);
        if (qrArea) qrArea.innerHTML = `
          <div style="display:flex;gap:var(--sp-6);align-items:center;flex-wrap:wrap">
            <img src="${esc(qrSrc)}" alt="OpenRouter QR" width="180" height="180" style="border:6px solid #fff;border-radius:8px;background:#fff">
            <div style="font-size:0.78rem">
              <div class="mb-3">Scan with your phone, or on this device:</div>
              <a href="${esc(authURL)}" target="_blank" rel="noopener" style="color:var(--cyan);font-weight:600">Open OpenRouter authorization ↗</a>
            </div>
          </div>`;
        if (status) status.innerHTML = '<span class="text-muted">Waiting for authorization… this panel updates when the key is connected.</span>';
        orStartCreditPolling();
      } catch (e) {
        if (qrArea) qrArea.innerHTML = `<span style="color:var(--red);font-size:0.78rem">Failed to start: ${esc(e.message)}</span>`;
      }
    }

    function orStartCreditPolling() {
      if (_openrouterPoll) dashboardClearInterval('openrouter-credit');
      const OR_POLL_MS = 4000;
      _openrouterPoll = dashboardSetInterval('openrouter-credit', orCheckCredit, OR_POLL_MS);
    }

    async function orCheckCredit() {
      const status = document.getElementById('or-status');
      try {
        const res = await fetch('/api/openrouter/credit');
        if (!res.ok) return;
        const d = await res.json();
        if (!d.connected) return;
        if (_openrouterPoll) { dashboardClearInterval('openrouter-credit'); _openrouterPoll = null; }
        let creditLine = '';
        if (d.limit_remaining != null) creditLine = ' — $' + Number(d.limit_remaining).toFixed(2) + ' remaining';
        else if (d.limit == null) creditLine = ' — unlimited';
        if (status) status.innerHTML = '<span style="color:var(--green);font-weight:600">✓ Connected' + esc(creditLine) + '</span>';
        loadGateways();
      } catch (e) { /* keep polling */ }
    }

    function closeOpenRouterFund() {
      if (_openrouterPoll) { dashboardClearInterval('openrouter-credit'); _openrouterPoll = null; }
      const host = document.getElementById('openrouter-fund-host');
      if (host) host.innerHTML = '';
    }

    async function loadGateways() {
      const host = document.getElementById('gateways-list-host');
      try {
        const res = await fetch('/api/config/governor/gateways');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        _gateways = data.gateways || [];
        // Keep the backend/method pickers' gateway list in sync with the tab.
        _configuredGateways = _gateways
          .map(g => g.name)
          .filter(name => name);
        renderGatewaysList();
        refreshLegacyLiteLLMDivergence();
        // Guided jump from a method switch (maybeOpenMethodConfig): if we were
        // sent here to configure a specific kind, open a fresh add-form already
        // preset to it (endpoint/preset filled) so the user lands on the form,
        // not the empty list. One-shot: cleared after consuming.
        if (_pendingGatewayPresetKind) {
          const presetKind = _pendingGatewayPresetKind;
          _pendingGatewayPresetKind = null;
          openGatewayForm(null);
          applyGatewayPreset(presetKind);
        }
      } catch (e) {
        if (host) host.innerHTML = `<div style="color:var(--red);font-size:0.8rem">Failed to load gateways: ${esc(e.message)}</div>`;
      }
    }

    function renderGatewaysList() {
      const host = document.getElementById('gateways-list-host');
      if (!host) return;
      if (!_gateways.length) {
        host.innerHTML = '<div style="color:var(--muted);font-size:0.8rem;padding:var(--sp-5) var(--sp-0)">No gateways configured yet. Click "+ Add gateway" to add one.</div>';
        return;
      }
      host.innerHTML = _gateways.map(function(gw) {
        const meta = gatewayKindMeta(gw.kind);
        // keyName is the operator-chosen LABEL for the key (safe to show, never
        // the secret). When a key is set we surface it — a recorded name, or
        // "(unnamed)" for gateways that never recorded one — so a manager can
        // tell WHICH key each gateway uses without seeing the value.
        const keyHash = gw.keySHA256 ? ` · SHA256: <span title="${esc(gw.keySHA256)}">${esc(gw.keySHA256.slice(0, 12))}</span>` : '';
        const keyLine = gw.hasKey
          ? `key set · Using key: ${esc(gw.keyName || '(unnamed)')}${keyHash}`
          : 'no key';
        const persistedProbe = _gatewayProbeResults[gw.name] || (gw.health ? { ok: false, error: gatewayHealthText(gw.health) } : null);
        const testDisplay = persistedProbe ? 'block' : 'none';
        return `
        <div style="border:1px solid var(--border);border-radius:8px;padding:12px 14px;margin-bottom:10px;background:var(--surface)">
          <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap">
            <span style="font-weight:600;font-size:0.9rem">${esc(gw.name)}</span>
            <span style="font-size:var(--fs-xs);font-weight:600;padding:var(--sp-1) var(--sp-4);border-radius:10px;border:1px solid ${meta.color};color:${meta.color}">${esc(meta.label)}</span>
            <span style="flex:1"></span>
            <button class="hv-btn btn-primary btn-sm" data-action="testGateway" data-arg0="${esc(gw.name)}">Test</button>
            <button class="hv-btn btn-primary btn-sm" data-action="editGateway" data-arg0="${esc(gw.name)}">Edit</button>
            <button class="hv-btn btn-danger btn-sm" data-action="deleteGateway" data-arg0="${esc(gw.name)}">Delete</button>
          </div>
          <div style="font-size:0.74rem;color:var(--muted);margin-top:var(--sp-4);word-break:break-all">
            <div>Endpoint: ${esc(gw.endpoint || '—')}</div>
            <div>Default model: ${esc(gw.default_model || '—')} · ${keyLine}</div>
          </div>
          <div id="gateway-test-result-${esc(gw.name)}" style="display:${testDisplay};font-size:0.74rem;margin-top:var(--sp-3);white-space:pre-wrap">${persistedProbe ? gatewayProbeHTML(persistedProbe) : ''}</div>
        </div>`;
      }).join('');
    }

    async function testGateway(name) {
      const el = document.getElementById('gateway-test-result-' + name);
      if (el) { el.style.display = 'block'; el.style.color = 'var(--muted)'; el.textContent = 'Testing…'; }
      try {
        const res = await fetch('/api/config/governor/gateways/' + encodeURIComponent(name) + '/test', {
          method: 'POST', headers: _inceptionAuthHeaders()
        });
        const data = await res.json().catch(() => ({}));
        const probe = data.probe || (res.ok ? null : { ok: false, error: data.error || ('HTTP ' + res.status) });
        _gatewayProbeResults[name] = probe;
        showGatewayProbe(el, probe);
      } catch (e) {
        _gatewayProbeResults[name] = { ok: false, error: e.message };
        showGatewayProbe(el, _gatewayProbeResults[name]);
      }
    }

    function gatewayHealthText(health) {
      if (!health) return 'unknown error';
      if (health.error_class === 'auth' && health.http_status) return `gateway rejected the configured key (HTTP ${health.http_status})`;
      if (health.error_class === 'dns' || health.error_class === 'connect') return `gateway unreachable (${health.error_class})`;
      if (health.error_class === '5xx') return 'gateway returned 5xx';
      if (health.error_class === 'budget') return 'gateway budget/rate limited (HTTP 429)';
      return health.detail || 'gateway failing';
    }

    function gatewayProbeHTML(probe) {
      if (probe && probe.diagnostic) {
        return `<span class="${probe.ok ? 'text-success' : 'text-danger'}">${esc(probe.diagnostic)}</span>`;
      }
      if (probe && probe.ok) {
        const n = probe.model_count || 0;
        return `<span class="text-success">OK — ${n} model${n === 1 ? '' : 's'} available</span>`;
      }
      return `<span class="text-danger">Connection failed: ${esc((probe && probe.error) || 'unknown error')}</span>`;
    }

    // showGatewayProbe formats a {ok, model_count} / {ok:false, error} probe.
    function showGatewayProbe(el, probe) {
      if (!el) return;
      el.style.display = 'block';
      el.innerHTML = gatewayProbeHTML(probe);
    }

    // refreshMethodModelsAfterGatewaySave re-runs live model discovery for the
    // method a just-saved gateway backs, then repaints the model dropdown of
    // every agent already using that method. This is the "once the key is
    // entered, auto-discovery and population occur" half of the onboarding flow:
    // without it the operator saves the key and still sees a stale/empty model
    // list until a manual refresh. Re-uses fetchInferenceModels (force=true to
    // bypass its TTL cache, since the key that makes discovery succeed was only
    // just written) and the existing updateModelDropdown repaint path.
    async function refreshMethodModelsAfterGatewaySave(kind, name) {
      // A gateway is reachable from an agent under its NAME, and the built-in
      // methods share their name with their kind — refresh both spellings.
      const methods = [];
      [kind, name].forEach(m => {
        if (m && INFERENCE_BACKENDS.includes(m) && !methods.includes(m)) methods.push(m);
      });
      if (!methods.includes(name) && name && includesNameCI(_configuredGateways, name)) methods.push(name);
      for (const method of methods) {
        let models = [];
        try {
          models = await fetchInferenceModels(method, true);
        } catch (e) { continue; }
        if (!models.length) continue;
        const cached = _inferenceModelCache[method];
        if (cached && cached.fallback) {
          // Discovery still is not working (bad key/region/project): say so
          // rather than letting unverified aliases masquerade as discovered.
          _gatewayProbeResults[method] = { ok: false, error: 'model discovery did not return a live list — check the key and click Test on the gateway card.' };
          showToast(`${method} gateway saved, but model discovery did not return a live list — check the key and click Test on the gateway card.`, 'error');
          continue;
        }
        showToast(`${method}: ${models.length} model${models.length === 1 ? '' : 's'} discovered`, 'success');
        // Repaint the dropdown for every agent on this method so a discovered
        // model can be picked right away.
        for (const a of (window._lastAgents || []).filter(x => x.cli === method)) {
          await updateModelDropdown(a.name, method);
        }
      }
    }

    // Inline feedback for the add/edit form's live model discovery, rendered in
    // the same #gw-form-result strip the save-time probe uses so a discovery
    // failure and a save failure look alike. Kept separate from showGatewayProbe
    // because discovery runs while typing (debounced) and must not claim the
    // gateway was saved.
    function showGatewayFormProbeError(msg) {
      const el = document.getElementById('gw-form-result');
      showGatewayProbe(el, { ok: false, error: msg });
    }

    function clearGatewayFormProbeError() {
      const el = document.getElementById('gw-form-result');
      if (!el) return;
      el.style.display = 'none';
      el.textContent = '';
    }

    async function deleteGateway(name) {
      if (!await hiveConfirm({title: 'Delete gateway?', message: 'Delete gateway "' + name + '"? Agents that route through it must be repointed first.', confirmLabel: 'Delete', danger: true})) return;
      try {
        const res = await fetch('/api/config/governor/gateways/' + encodeURIComponent(name), {
          method: 'DELETE', headers: _inceptionAuthHeaders()
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          // 409 → in use by agents; surface the server's agent list verbatim.
          showToast(data.error || ('Failed to delete (HTTP ' + res.status + ')'), 'error');
          return;
        }
        showToast('Gateway "' + name + '" deleted', 'success');
        loadGateways();
      } catch (e) {
        showToast('Failed to delete gateway: ' + e.message, 'error');
      }
    }

    function editGateway(name) {
      const gw = _gateways.find(g => g.name === name);
      openGatewayForm(gw || null);
    }

    // openGatewayForm renders the inline add/edit form. When `existing` is set
    // the form is in edit mode (name is read-only, fields prefilled). The API
    // Key input always starts empty — typing replaces the stored key, leaving
    // it blank keeps it.
    function openGatewayForm(existing) {
      _gatewayFormModels = [];
      const host = document.getElementById('gateways-form-host');
      if (!host) return;
      const isEdit = !!existing;
      const gw = existing || { name: '', kind: 'custom', endpoint: '', default_model: '', hasKey: false, project_id: '', keyName: '' };
      const presetButtons = GATEWAY_PRESETS.map(function(p) {
        const active = gw.kind === p.kind;
        return `<button class="hv-btn btn-danger btn-sm" type="button" data-action="applyGatewayPreset" data-arg0="${p.kind}" data-gw-preset="${p.kind}" style="padding:5px 12px;border:1px solid ${active ? 'var(--cyan)' : 'var(--border)'};border-radius:var(--r);background:${active ? 'var(--cyan)' : 'var(--bg)'};color:${active ? 'var(--bg)' : 'var(--fg)'};cursor:pointer;font-size:0.74rem;font-weight:600">${esc(p.label)}</button>`;
      }).join('');
      host.innerHTML = `
        <div style="border:1px solid var(--cyan);border-radius:8px;padding:var(--sp-6);margin-bottom:14px;background:var(--surface)">
          <div style="font-weight:600;margin-bottom:var(--sp-5)">${isEdit ? 'Edit gateway: ' + esc(gw.name) : 'Add gateway'}</div>
          <div class="config-field mb-5">
            <label>Preset</label>
            <div style="display:flex;gap:var(--sp-3);flex-wrap:wrap">${presetButtons}</div>
          </div>
          <div class="config-field" style="margin-bottom:10px">
            <label>Name</label>
            <input type="text" id="gw-name" value="${esc(gw.name)}" placeholder="e.g. openrouter, corp-litellm" ${isEdit ? 'readonly style="opacity:0.7"' : ''}>
          </div>
          <input type="hidden" id="gw-kind" value="${esc(gw.kind || 'custom')}">
          <div class="config-field" style="margin-bottom:10px">
            <label>Endpoint URL</label>
            <input type="text" id="gw-endpoint" value="${esc(gw.endpoint || '')}" placeholder="https://…/v1" data-change-action="onGatewayEndpointOrKeyChange">
          </div>
          <div class="config-field" id="gw-project-id-field" style="margin-bottom:10px;display:${(gw.kind === 'watsonx') ? 'block' : 'none'}">
            <label>watsonx Project ID</label>
            <input type="text" id="gw-project-id" value="${esc(gw.project_id || '')}" placeholder="e.g. 63dc4cf1-252f-424b-b52d-5cdd9814987f" data-change-action="onGatewayEndpointOrKeyChange">
            <div style="font-size:0.7rem;margin-top:3px;color:var(--muted)">Required for watsonx — sent as the X-IBM-Project-ID header. Your API key is exchanged for a short-lived IAM token; the raw key is never sent.</div>
          </div>
          <div class="config-field" style="margin-bottom:10px">
            <label>API Key</label>
            <input type="password" id="gw-apikey" value="" autocomplete="new-password" placeholder="${gw.hasKey ? '(key is set — enter a new key to replace it)' : 'sk-…'}" data-change-action="onGatewayEndpointOrKeyChange">
            <div style="font-size:0.7rem;margin-top:3px;color:var(--muted)">Stored as a secret on this hive (owner-only file), never in config.</div>
          </div>
          <div class="config-field" style="margin-bottom:10px">
            <label>Key name <span style="color:var(--muted);font-weight:400">(optional label, not the secret)</span></label>
            <input type="text" id="gw-key-name" value="${esc(gw.keyName || '')}" autocomplete="off" spellcheck="false" maxlength="128" placeholder="e.g. Team inference key">
            <div style="font-size:0.7rem;margin-top:3px;color:var(--muted)">A plaintext label that records WHICH key this is so you can tell keys apart later — never the value. Leave blank for "(unnamed)".</div>
          </div>
          <div class="config-field" style="margin-bottom:10px">
            <label>Default Model</label>
            <div id="gw-default-model-field">${gatewayDefaultModelFieldHtml(gw.default_model || '')}</div>
          </div>
          <div style="display:flex;gap:var(--sp-4);justify-content:flex-end;margin-top:14px">
            <button class="hv-btn btn-secondary btn-sm" type="button" data-action="closeGatewayForm">Cancel</button>
            <button class="hv-btn btn-secondary btn-sm" type="button" id="gw-form-test" data-action="testGatewayForm" title="Probe the endpoint with the values entered above (a blank key uses the stored key)">Test</button>
            <button class="hv-btn btn-primary btn-sm" type="button" data-action="saveGateway" data-arg0="${isEdit ? 'true' : 'false'}" data-arg-types="j">Save</button>
          </div>
          <div id="gw-form-result" style="display:none;font-size:var(--fs-sm);margin-top:10px;white-space:pre-wrap"></div>
        </div>`;
      // Populate the model dropdown if endpoint (and possibly a stored key) are
      // already present in edit mode.
      onGatewayEndpointOrKeyChange();
    }

    function closeGatewayForm() {
      const host = document.getElementById('gateways-form-host');
      if (host) host.innerHTML = '';
    }

    // applyGatewayPreset sets the kind and fills the endpoint for the picked
    // preset, then focuses the key field. OpenRouter fills its base URL; the
    // others leave the endpoint for the user (self-hosted / custom).
    function applyGatewayPreset(kind) {
      const preset = GATEWAY_PRESETS.find(p => p.kind === kind) || GATEWAY_PRESETS[GATEWAY_PRESETS.length - 1];
      const kindEl = document.getElementById('gw-kind');
      const epEl = document.getElementById('gw-endpoint');
      if (kindEl) kindEl.value = preset.kind;
      // Only overwrite the endpoint when the preset carries one, so switching
      // to Custom doesn't wipe a URL the user already typed.
      if (epEl && preset.endpoint) epEl.value = preset.endpoint;
      // Default the (still-empty) name to the kind so a guided add-form is
      // ready to save — a gateway named after its kind is exactly what an
      // agent whose backend is that method resolves to. Never clobbers a name
      // the user already typed, nor the readonly name in edit mode.
      const nameEl = document.getElementById('gw-name');
      if (nameEl && !nameEl.readOnly && !nameEl.value.trim() && preset.kind !== 'custom') {
        nameEl.value = preset.kind;
      }
      // The watsonx project-id field is only relevant for the watsonx kind.
      const projField = document.getElementById('gw-project-id-field');
      if (projField) projField.style.display = (preset.kind === 'watsonx') ? 'block' : 'none';
      // Re-render the preset button row so the active highlight follows.
      document.querySelectorAll('[data-gw-preset]').forEach(function(btn) {
        const active = btn.getAttribute('data-gw-preset') === preset.kind;
        btn.style.borderColor = active ? 'var(--cyan)' : 'var(--border)';
        btn.style.background = active ? 'var(--cyan)' : 'var(--bg)';
        btn.style.color = active ? 'var(--bg)' : 'var(--fg)';
      });
      const keyEl = document.getElementById('gw-apikey');
      if (keyEl) keyEl.focus();
      onGatewayEndpointOrKeyChange();
    }

    // gatewayDefaultModelFieldHtml renders the Default Model control: a
    // <select> of discovered ids (once the endpoint has been probed) plus a
    // "Custom…" escape hatch, or a plain text input when none are discovered
    // (so the field is never a dead end).
    function gatewayDefaultModelFieldHtml(current) {
      const models = _gatewayFormModels;
      const cur = current || '';
      if (!models.length) {
        return `<input type="text" id="gw-default-model" value="${esc(cur)}" placeholder="e.g. anthropic/claude-opus-4.8">`;
      }
      const inList = cur && models.indexOf(cur) !== -1;
      const options = models.map(m =>
        `<option value="${esc(m)}"${m === cur ? ' selected' : ''}>${esc(m)}</option>`).join('');
      const customOptLabel = inList ? 'Custom…' : (cur ? esc(cur) + ' (custom)' : 'Custom…');
      const customSelected = cur && !inList;
      const emptyOpt = cur ? '' : '<option value="" selected disabled>Select a discovered model…</option>';
      return `
        <select id="gw-default-model-select" data-change-action="onGatewayDefaultModelSelect" data-arg-types="t">
          ${emptyOpt}${options}
          <option value="${GATEWAY_CUSTOM_MODEL_SENTINEL}"${customSelected ? ' selected' : ''}>${customOptLabel}</option>
        </select>
        <input type="text" id="gw-default-model" value="${customSelected ? esc(cur) : (inList ? esc(cur) : '')}" placeholder="e.g. anthropic/claude-opus-4.8" style="margin-top:var(--sp-3);display:${customSelected ? 'block' : 'none'}" data-gw-custom-model>`;
    }

    function onGatewayDefaultModelSelect(sel) {
      const custom = document.querySelector('[data-gw-custom-model]');
      if (sel.value === GATEWAY_CUSTOM_MODEL_SENTINEL) {
        if (custom) { custom.style.display = 'block'; custom.focus(); }
        return;
      }
      if (custom) { custom.style.display = 'none'; custom.value = sel.value; }
    }

    // gatewayCurrentDefaultModel reads whichever control (select vs custom
    // input) currently holds the chosen default model id.
    function gatewayCurrentDefaultModel() {
      const sel = document.getElementById('gw-default-model-select');
      const custom = document.getElementById('gw-default-model');
      if (sel && sel.value && sel.value !== GATEWAY_CUSTOM_MODEL_SENTINEL) return sel.value;
      return custom ? custom.value.trim() : '';
    }

    const GATEWAY_MODEL_PROBE_DEBOUNCE_MS = 500;
    let _gatewayModelProbeTimer = null;
    // onGatewayEndpointOrKeyChange debounces a live /v1/models probe of the
    // form's endpoint+key (via the discover endpoint, which never stores the
    // key) so the Default Model dropdown populates before the gateway is saved.
    // On failure the field stays a plain text input (manual-entry fallback), so
    // it is never a dead end.
    function onGatewayEndpointOrKeyChange() {
      if (_gatewayModelProbeTimer) clearTimeout(_gatewayModelProbeTimer);
      const epEl = document.getElementById('gw-endpoint');
      const endpoint = epEl ? epEl.value.trim() : '';
      if (!endpoint) return;
      _gatewayModelProbeTimer = setTimeout(function() {
        probeGatewayFormModels();
      }, GATEWAY_MODEL_PROBE_DEBOUNCE_MS);
    }

    // testGatewayForm is the form-level "Test": it probes the endpoint with
    // the values currently typed (a blank key falls back to the stored key of
    // this gateway, server-side) and reports OK/failure inline, so a bad key
    // or endpoint is diagnosable BEFORE saving. The card-level Test only
    // exists after a gateway is saved, which left the edit form with a toast
    // saying "try Test" and no Test to click (#9914).
    async function testGatewayForm() {
      const epEl = document.getElementById('gw-endpoint');
      if (!epEl || !epEl.value.trim()) { showGatewayFormProbeError('Enter an endpoint URL first.'); return; }
      const btn = document.getElementById('gw-form-test');
      const el = document.getElementById('gw-form-result');
      if (btn) btn.disabled = true;
      if (el) { el.style.display = 'block'; el.style.color = 'var(--muted)'; el.textContent = 'Testing…'; }
      try {
        const probe = await probeGatewayFormModels();
        if (el && (el.style.display !== 'block' || el.textContent === 'Testing…')) {
          el.style.display = 'block';
          el.style.color = '';
          // Prefer the server's diagnostic (#9945): it names the endpoint,
          // default model and key SHA that were actually tested plus the
          // route agents use, so success is never just "OK — N models".
          el.innerHTML = gatewayProbeHTML(probe && probe.ok ? probe : { ok: true, model_count: _gatewayFormModels.length });
        }
      } finally {
        if (btn) btn.disabled = false;
      }
    }

    // Resolves to the server's probe object ({ok, model_count, diagnostic,
    // route}) when discovery succeeded, else null.
    async function probeGatewayFormModels() {
      let probe = null;
      const nameEl = document.getElementById('gw-name');
      const epEl = document.getElementById('gw-endpoint');
      const keyEl = document.getElementById('gw-apikey');
      const kindEl = document.getElementById('gw-kind');
      const projEl = document.getElementById('gw-project-id');
      const endpoint = epEl ? epEl.value.trim() : '';
      if (!endpoint) return probe;
      // Preserve whatever the user has currently chosen before re-rendering.
      const current = gatewayCurrentDefaultModel();
      try {
        const res = await fetch('/api/config/governor/gateways/discover', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({
            name: nameEl ? nameEl.value.trim() : '',
            endpoint: endpoint,
            api_key: keyEl ? keyEl.value.trim() : '',
            default_model: gatewayCurrentDefaultModel(),
            key_name: (document.getElementById('gw-key-name') || {}).value || '',
            // watsonx discovery needs the kind + project id to mint the IAM
            // token and set X-IBM-Project-ID; harmless for other kinds.
            kind: kindEl ? kindEl.value : '',
            project_id: projEl ? projEl.value.trim() : ''
          })
        });
        const data = await res.json().catch(() => ({}));
        _gatewayFormModels = (res.ok && data.ok && Array.isArray(data.models)) ? data.models : [];
        // Never show an empty dropdown with no explanation: a bad key, wrong
        // region/project or an unreachable endpoint must say so, with the
        // server's own error text, rather than looking like "no models exist".
        if (!res.ok || !data.ok) {
          if (data.probe || data.diagnostic) {
            showGatewayProbe(document.getElementById('gw-form-result'), data.probe || { ok: false, error: data.error || 'Model discovery failed — check the endpoint and API key.', diagnostic: data.diagnostic });
          } else {
            showGatewayFormProbeError(data.error || 'Model discovery failed — check the endpoint and API key.');
          }
        } else if (!_gatewayFormModels.length) {
          showGatewayFormProbeError('Connected, but the gateway returned no models. Check the key\'s entitlements' + (kindEl && kindEl.value === 'watsonx' ? ' and the project id.' : '.'));
        } else {
          clearGatewayFormProbeError();
          if (data.probe && data.probe.ok) probe = data.probe;
        }
      } catch (e) {
        _gatewayFormModels = [];
        showGatewayFormProbeError('Network error while discovering models: ' + e.message);
      }
      const host = document.getElementById('gw-default-model-field');
      if (host) host.innerHTML = gatewayDefaultModelFieldHtml(current);
      return probe;
    }

    // saveGateway PUTs the form. A typed key is sent as api_key and stored
    // server-side as a secret file (only the path lands in hive.yaml). On
    // success the list refreshes and the server's save-time probe result shows.
    async function saveGateway(isEdit) {
      const name = (document.getElementById('gw-name') || {}).value || '';
      const kind = (document.getElementById('gw-kind') || {}).value || 'custom';
      const endpoint = (document.getElementById('gw-endpoint') || {}).value || '';
      const apiKey = (document.getElementById('gw-apikey') || {}).value || '';
      const projectId = (document.getElementById('gw-project-id') || {}).value || '';
      const keyName = (document.getElementById('gw-key-name') || {}).value || '';
      const defaultModel = gatewayCurrentDefaultModel();
      const resultEl = document.getElementById('gw-form-result');
      if (!name.trim()) { showToast('Gateway name is required', 'error'); return; }
      if (!endpoint.trim()) { showToast('Endpoint is required', 'error'); return; }
      if (kind === 'watsonx' && !projectId.trim()) { showToast('watsonx requires a Project ID', 'error'); return; }
      const payload = {
        name: name.trim(),
        kind: kind,
        endpoint: endpoint.trim(),
        default_model: defaultModel,
        // Always sent so clearing the field on a non-watsonx gateway persists;
        // omitempty on the server keeps it out of hive.yaml when blank.
        project_id: projectId.trim(),
        // The optional plaintext LABEL for the key (never the value). Always
        // sent (even empty) so blanking the field clears a stale name; the
        // server trims and length-bounds it, and omitempty keeps it out of
        // hive.yaml when blank.
        key_name: keyName.trim(),
      };
      // Persist the watsonx region (the preset derives the endpoint from it and
      // the server can too when the endpoint is blank).
      if (kind === 'watsonx') {
        const regionForEndpoint = (endpoint.trim().match(/^https:\/\/([^.]+)\.ml\.cloud\.ibm\.com/) || [])[1];
        if (regionForEndpoint) payload.region = regionForEndpoint;
      }
      // Only send api_key when the user typed one (empty = keep existing key).
      if (apiKey.trim()) payload.api_key = apiKey.trim();
      try {
        const res = await fetch('/api/config/governor/gateways', {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify(payload)
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) {
          showToast(data.error || ('Failed to save (HTTP ' + res.status + ')'), 'error');
          return;
        }
        // Surface the server's live save-time probe inline.
        if (data.probe) {
          _gatewayProbeResults[payload.name] = data.probe;
          if (resultEl) showGatewayProbe(resultEl, data.probe);
        }
        showToast('Gateway "' + payload.name + '" saved', 'success');
        closeGatewayForm();
        // The server carries the legacy governor.litellm block along with the
        // "litellm" gateway (gateways.go legacyFollowsGateway); mirror that in
        // the loaded config so the legacy section and its divergence warning
        // reflect the new endpoint without a page reload.
        if (payload.kind === 'litellm' && String(payload.name).toLowerCase() === 'litellm' && _configState.data) {
          _configState.data.litellm = Object.assign({}, _configState.data.litellm || {}, { endpoint: payload.endpoint });
          const legacyInput = document.querySelector('#legacy-litellm-section input[data-arg0="litellm"][data-arg1="endpoint"]');
          if (legacyInput) legacyInput.value = payload.endpoint;
        }
        loadGateways();
        // Close the loop back to the agent that sent the operator here: now
        // that the key is stored, re-run discovery for this method and push the
        // result into the agent model dropdowns, so a model can be picked
        // immediately with no manual refresh. Force-bypasses the TTL cache
        // because the key that makes discovery succeed was just written.
        refreshMethodModelsAfterGatewaySave(payload.kind, payload.name);
      } catch (e) {
        showToast('Failed to save gateway: ' + e.message, 'error');
      }
    }

    function renderGovLiteLLM(litellm) {
      // The server never returns the key VALUE — only hasKey, a masked
      // tail hint ("••••WMg"), and the store it resolved from. The API
      // Key input always starts EMPTY (never pre-filled): typing a value
      // replaces the stored key, leaving it blank keeps it.
      const keyHash = litellm.keySHA256 ? ` — SHA256: <span title="${esc(litellm.keySHA256)}">${esc(litellm.keySHA256.slice(0, 12))}</span>` : '';
      const keyLine = litellm.hasKey
        ? `Key detected: yes${keyHash} — from ${esc(litellmKeySourceLabel(litellm.keySource))}`
        : 'Key detected: no — paste your gateway key above';
      // Pre-guardrail leak remediation: a key VALUE previously pasted into
      // the env-name/file fields comes back MASKED from the server with a
      // LooksLikeKey flag. Render the input empty (never echo even the
      // masked form as an editable value) plus a warning to move the key.
      const keyEnvWarning = litellm.apiKeyEnvLooksLikeKey
        ? `<div style="font-size:0.72rem;margin-top:3px;color:var(--red)">The saved value here (${esc(litellm.apiKeyEnv || '')}) looks like a key VALUE, not an environment variable name — move it to the API Key field above and Save; storing a key clears this field automatically.</div>`
        : '';
      const keyFileWarning = litellm.apiKeyFileLooksLikeKey
        ? `<div style="font-size:0.72rem;margin-top:3px;color:var(--red)">The saved value here (${esc(litellm.apiKeyFile || '')}) looks like a key VALUE, not a file path — move it to the API Key field above and Save.</div>`
        : '';
      const keyEnvValue = litellm.apiKeyEnvLooksLikeKey ? '' : (litellm.apiKeyEnv || '');
      const keyFileValue = litellm.apiKeyFileLooksLikeKey ? '' : (litellm.apiKeyFile || '');
      return `
        <div class="config-field">
          <label>Endpoint URL ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'Endpoint URL', `Base URL of the LiteLLM proxy (e.g. https://litellm.example.com). Agents reach it through the hive's inference translator — the MITM proxy stays in the path. The HIVE_LITELLM_ENDPOINT env var overrides this at runtime.`)}</label>
          <input type="text" value="${esc(litellm.endpoint || '')}" placeholder="https://litellm.example.com" data-change-action="markDirty" data-arg0="litellm" data-arg1="endpoint" data-arg-types="s,s,v">
        </div>
        <div class="config-field">
          <label>API Key ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'API Key', `Paste the key from your gateway/provider. It is stored privately on this hive (owner-only file on the persistent volume, plus the hive's Kubernetes Secret when available) — never in hive.yaml, logs, or config downloads. Saving runs a live connection test.`)}</label>
          <input type="password" value="" placeholder="${litellm.hasKey ? '(key is set — enter a new key to replace it)' : 'sk-...'}" autocomplete="new-password" data-change-action="markDirty" data-arg0="litellm" data-arg1="apiKey" data-arg-types="s,s,w">
          <div style="font-size:0.72rem;margin-top:var(--sp-2);color:${litellm.hasKey ? 'var(--green)' : 'var(--red)'}">${keyLine}</div>
        </div>
        <div class="config-field">
          <label>Default Model ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'Default Model', `Model used when an agent on the litellm backend has no model selected. Populated from the models discovered on the configured endpoint — choose "Custom…" to type an id the gateway does not list yet.`)}</label>
          <div id="litellm-default-model-field">${litellmDefaultModelFieldHtml(litellm.defaultModel || '')}</div>
        </div>
        <details style="margin:var(--sp-2) var(--sp-0) var(--sp-4)">
          <summary style="cursor:pointer;font-size:var(--fs-sm);color:var(--muted)">Advanced — self-hosted key sources &amp; proxy options</summary>
          <div class="config-field" style="margin-top:10px">
            <label>Read Key From Environment Variable ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'Read Key From Environment Variable', `For self-hosted operators who manage the key outside the dashboard. Takes the NAME of an environment variable — never the key itself.`)}</label>
            <input type="text" value="${esc(keyEnvValue)}" placeholder="HIVE_LITELLM_API_KEY" data-change-action="onLitellmKeyEnvChange" data-arg-types="t">
            <div style="font-size:0.7rem;margin-top:3px;color:var(--muted)">Enter the NAME of an environment variable on the hive container that contains your key (self-hosted installs). Hosted users: paste your key in the API Key field above instead.</div>
            ${keyEnvWarning}
          </div>
          <div class="config-field">
            <label>Read Key From File Path ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'Read Key From File Path', `For self-hosted operators. Path to a mounted file containing the key (e.g. a Kubernetes Secret mount).`)}</label>
            <input type="text" value="${esc(keyFileValue)}" placeholder="/secrets/litellm_api_key" data-change-action="onLitellmKeyFileChange" data-arg-types="t">
            <div style="font-size:0.7rem;margin-top:3px;color:var(--muted)">Path to a file that contains your key, such as a Kubernetes Secret mount (self-hosted installs). Hosted users: paste your key in the API Key field above instead.</div>
            ${keyFileWarning}
          </div>
          <div class="config-field">
            <label>CA Bundle Path ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'CA Bundle Path', `Optional PEM file for a private CA (e.g. an internal VPC endpoint). Certificate verification is never disabled — the CA is added to the trust pool.`)}</label>
            <input type="text" value="${esc(litellm.caBundle || '')}" placeholder="/secrets/litellm-ca.pem" data-change-action="markDirty" data-arg0="litellm" data-arg1="caBundle" data-arg-types="s,s,v">
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${litellm.localProxy ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="litellm" data-key="localProxy"></div>
            <span class="config-toggle-label">Local Proxy Fallback ${settingHelpMark('hosted-hub', 'configure-model-gateways-and-keys', 'Local Proxy Fallback', `Run the bundled litellm binary as a local translator (config at /data/litellm/config.yaml). Off by default — the hive's Go translator forwards directly to the remote endpoint.`)}</span>
          </div>
        </details>
        <div class="mt-5">
          <button class="hv-btn btn-primary btn-sm" data-action="testLiteLLMConnection">Test Connection</button>
          <div id="litellm-test-result" style="display:none;font-size:var(--fs-sm);margin-top:var(--sp-4);white-space:pre-wrap"></div>
        </div>`;
    }

    async function testLiteLLMConnection() {
      const toast = showToast('Testing LiteLLM connection…', 'info', true);
      try {
        // Live probe only (/v1/models with the same key resolution the
        // translator uses). The old /api/inference/models path silently
        // substituted the static fallback aliases, reporting "OK — 7
        // models" against gateways that 401'd everything.
        const resp = await fetch('/api/config/governor/litellm/test', {
          method: 'POST',
          headers: _inceptionAuthHeaders()
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) {
          dismissToast(toast, data.error || 'LiteLLM endpoint not configured — save the endpoint first', 'error');
          return;
        }
        const r = showLiteLLMTestResult(data.probe || { ok: false, error: 'no probe result returned' });
        dismissToast(toast, r.msg, r.ok ? 'success' : 'error');
      } catch (e) {
        dismissToast(toast, 'LiteLLM test failed: ' + e.message, 'error');
      }
    }

    /* renderHubDashboardURLField (#7451): hub.dashboard_url is a SETTING only
       on a spoke that owns its own hostname. On a hub-managed spoke the hub's
       ingress terminates the host and the heartbeat pushes the vanity URL, so
       an edit here is reverted by the next beat — and until it is, the value
       is the second-choice OAuth callback origin (oauth_origin.go): a typo
       locks the operator out of the dashboard they typed it into. The server
       says who owns it (dashboard_url_owner) and refuses a hub-owned change
       with 409; this renders it read-only with the provenance spelled out,
       the way the auto-update block does, and points at dashboard.public_url —
       the operator-owned field that already wins for OAuth and is never
       overwritten — for anyone who genuinely needs a different host. */
    function renderHubDashboardURLField(hub, dashUrl) {
      if (hub.dashboard_url_owner !== 'hub') {
        return `
        <div class="config-field">
          <label>Dashboard URL ${settingHelpMark('hub-deployment', 'registering-spokes-to-your-hub', 'Dashboard URL', `Public URL of this hive's dashboard, as reported to the hub.`)}</label>
          <input type="text" value="${esc(dashUrl)}" data-change-action="markDirty" data-arg0="hub" data-arg1="dashboard_url" data-arg-types="s,s,v">
        </div>`;
      }
      const publicUrl = hub.public_url || '';
      return `
        <div class="config-field">
          <label>Dashboard URL ${settingHelpMark('hub-deployment', 'registering-spokes-to-your-hub', 'Dashboard URL', `The public URL the hub serves this dashboard at. Owned by the hub: it is delivered on the heartbeat and any local edit is reverted by the next one. To serve the dashboard from a different host, set dashboard.public_url — it takes precedence for OAuth and is never overwritten.`)}</label>
          <input type="text" id="hub-dashboard-url" value="${esc(dashUrl)}" readonly disabled style="opacity:0.7">
          <div style="font-size:0.72rem;color:var(--muted);margin-top:var(--sp-2)">Owned by the hub (heartbeat) — read-only here.${publicUrl ? ' Local override in effect for OAuth: dashboard.public_url = ' + esc(publicUrl) : ''}</div>
        </div>`;
    }

    function contribRepoShortName(repoFull) {
      return String(repoFull || '').split('/').pop();
    }

    function contribRepoWildcardMatch(text, pattern) {
      text = String(text || '').toLowerCase();
      pattern = String(pattern || '').toLowerCase();
      if (pattern === '*') return true;
      if (pattern.indexOf('*') < 0) return text.indexOf(pattern) >= 0;
      var parts = pattern.split('*');
      var idx = 0;
      for (var i = 0; i < parts.length; i++) {
        var part = parts[i];
        if (!part) continue;
        var found = text.indexOf(part, idx);
        if (found < 0) return false;
        idx = found + part.length;
      }
      if (pattern.charAt(0) !== '*' && text.indexOf(parts[0]) !== 0) return false;
      var last = parts[parts.length - 1];
      if (pattern.charAt(pattern.length - 1) !== '*' && last && text.slice(-last.length) !== last) return false;
      return true;
    }

    function contribRepoDisabledEntryMatches(repoFull, entry) {
      const full = String(repoFull || '');
      const short = contribRepoShortName(full);
      entry = String(entry || '').trim();
      if (!entry) return false;
      return contribRepoWildcardMatch(full, entry) || contribRepoWildcardMatch(short, entry);
    }

    function contribRepoMatchingDisabledEntries(repoFull, disabled) {
      return (disabled || []).filter(function(entry) { return contribRepoDisabledEntryMatches(repoFull, entry); });
    }
