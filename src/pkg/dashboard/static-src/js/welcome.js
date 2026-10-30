    // ── Welcome / Getting Started dialog ──────────────────────────────
    // A living checklist for new hive owners: opens on every dashboard
    // load (write-access users only) until dismissed, and stays reachable
    // from the sidebar Resources list and the topbar Guide button.

    // Per-browser persistence blob: {dismissed: bool, checks: {stepId: bool}}
    // (same localStorage pattern as hive-layout-mode / hive-section-collapsed-*).
    const WELCOME_LS_KEY = 'hive-welcome-state-v1';
    // X-Hive-Role value the hub proxies read-only preview users in with —
    // the same /api/role signal the avatar menu uses to gate owner actions.
    const HIVE_ROLE_READ = 'read';
    // Role assumed when the header is absent (standalone spoke = owner);
    // mirrors the /api/role server-side default.
    const HIVE_ROLE_DEFAULT = 'owner';

    function loadWelcomeState() {
      try {
        const raw = localStorage.getItem(WELCOME_LS_KEY);
        if (raw) {
          const s = JSON.parse(raw);
          return { dismissed: s.dismissed === true, checks: (s && typeof s.checks === 'object' && s.checks) || {} };
        }
      } catch (e) { /* corrupted blob — start fresh */ }
      return { dismissed: false, checks: {} };
    }
    function saveWelcomeState() {
      try { localStorage.setItem(WELCOME_LS_KEY, JSON.stringify(_welcomeState)); } catch (e) { /* storage blocked — non-fatal */ }
    }
    let _welcomeState = loadWelcomeState();
    let _welcomeWriteAccess = false;

    async function initWelcomeDialog() {
      // Write-access gating: read-only previews never see the checklist or
      // its entry points. On fetch failure (e.g. static snapshot) stay hidden.
      let role = HIVE_ROLE_READ;
      try {
        const resp = await fetch('/api/role');
        const data = await resp.json();
        role = data.role || HIVE_ROLE_DEFAULT;
      } catch (e) { /* API unreachable — treat as read-only */ }
      if (role === HIVE_ROLE_READ) return;
      _welcomeWriteAccess = true;
      const navItem = document.getElementById('welcome-nav-item');
      if (navItem) navItem.style.display = '';
      const topBtn = document.getElementById('welcome-topbar-btn');
      if (topBtn) topBtn.style.display = '';
      // Kiosk displays (#15594) are unattended: nothing can dismiss a dialog.
      if (!_welcomeState.dismissed && !(window.hiveKioskMode && window.hiveKioskMode())) openWelcomeDialog();
    }

    function openWelcomeDialog() {
      // Populate the bee-to-hive banner from the shared helper (idempotent — the
      // ACMM overlay reuses the same markup). Only inject once to avoid
      // restarting the CSS animations on every open.
      const beeStage = document.getElementById('welcome-bee-stage');
      if (beeStage && !beeStage.firstChild) beeStage.innerHTML = beeStageHtml();
      // Refresh the primary-repo signal: window._primaryRepo is set by the
      // /api/config fetch at boot, so a repo saved through the config dialog
      // would otherwise stay stale until a full page reload.
      fetch('/api/config').then(r => r.json()).then(cfg => {
        const repoDisplay = cfg.org && cfg.primaryRepo && !cfg.primaryRepo.includes('/')
          ? cfg.org + '/' + cfg.primaryRepo
          : (cfg.primaryRepo || '');
        if (repoDisplay) window._primaryRepo = repoDisplay;
        refreshWelcomeAutoChecks(window._lastStatus || null);
      }).catch(() => {});
      renderWelcomeDialog();
      const overlay = document.getElementById('welcome-overlay');
      // Clear any in-flight genie state so the panel opens at full size.
      overlay.classList.remove('welcome-genie-overlay');
      const panel = overlay.querySelector('.welcome-modal');
      if (panel) {
        panel.classList.remove('welcome-genie');
        panel.style.transform = '';
        panel.style.transformOrigin = '';
        panel.style.opacity = '';
      }
      overlay.classList.remove('hidden');
      hiveRememberModalOpen(overlay);
    }
    // Genie-minimize timing. GENIE_MS must match --welcome-genie-ms in CSS;
    // it is the fallback deadline in case transitionend never fires (the
    // panel can be display:none'd mid-flight if the tab is backgrounded).
    const WELCOME_GENIE_MS = 400;
    const WELCOME_GENIE_FALLBACK_MS = WELCOME_GENIE_MS + 80;
    // Scale the panel down to roughly the Guide button's footprint as it flies in.
    const WELCOME_GENIE_MIN_SCALE = 0.06;

    function _prefersReducedMotion() {
      return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    }

    // Finalize a genie animation: hide the overlay and reset the animation
    // state so the next open starts clean. Idempotent.
    function _finishWelcomeGenie(overlay, panel) {
      if (!overlay || overlay.classList.contains('hidden')) return;
      overlay.classList.add('hidden');
      overlay.classList.remove('welcome-genie-overlay');
      if (panel) {
        panel.classList.remove('welcome-genie');
        panel.style.transform = '';
        panel.style.transformOrigin = '';
        panel.style.opacity = '';
      }
    }

    function closeWelcomeDialog() {
      const overlay = document.getElementById('welcome-overlay');
      if (!overlay || overlay.classList.contains('hidden')) return;
      const panel = overlay.querySelector('.welcome-modal');
      const target = document.getElementById('welcome-topbar-btn');
      // Instant hide when motion is reduced, the panel/target are missing, or
      // the reopen affordance is not visible (nothing to fly toward).
      if (_prefersReducedMotion() || !panel || !target ||
          target.offsetParent === null) {
        _finishWelcomeGenie(overlay, panel);
        return;
      }
      const panelRect = panel.getBoundingClientRect();
      const targetRect = target.getBoundingClientRect();
      if (!panelRect.width || !panelRect.height) { _finishWelcomeGenie(overlay, panel); return; }
      // Translate the panel's centre to the Guide button's centre, then shrink it.
      const panelCX = panelRect.left + panelRect.width / 2;
      const panelCY = panelRect.top + panelRect.height / 2;
      const targetCX = targetRect.left + targetRect.width / 2;
      const targetCY = targetRect.top + targetRect.height / 2;
      const dx = targetCX - panelCX;
      const dy = targetCY - panelCY;
      panel.style.transformOrigin = 'center center';
      panel.classList.add('welcome-genie');
      // Kick the transform on the next frame so the transition actually runs.
      requestAnimationFrame(function () {
        panel.style.transform = 'translate(' + dx + 'px,' + dy + 'px) scale(' + WELCOME_GENIE_MIN_SCALE + ')';
        panel.style.opacity = '0.2';
        overlay.classList.add('welcome-genie-overlay');
      });
      let done = false;
      const finish = function () {
        if (done) return;
        done = true;
        _finishWelcomeGenie(overlay, panel);
        // Pulse the Guide button once so the eye lands on where it went.
        target.classList.remove('welcome-target-pulse');
        void target.offsetWidth; // restart the animation
        target.classList.add('welcome-target-pulse');
        setTimeout(function () { target.classList.remove('welcome-target-pulse'); }, WELCOME_GENIE_FALLBACK_MS);
      };
      panel.addEventListener('transitionend', function te(e) {
        if (e.target === panel && e.propertyName === 'transform') {
          panel.removeEventListener('transitionend', te);
          finish();
        }
      });
      setTimeout(finish, WELCOME_GENIE_FALLBACK_MS);
    }
    function dismissWelcomeDialog() {
      _welcomeState.dismissed = true;
      saveWelcomeState();
      closeWelcomeDialog();
    }

    /** Same needs-login formula as the agent-card Auth badge. */
    function _welcomeAgentNeedsLogin(a) {
      return _agentNeedsLogin(a);
    }

    // Checklist steps. `auto(data)` derives done-state live from the latest
    // status payload (re-evaluated on every render pass, so checks flip as
    // the hive changes); steps without `auto` are manual click-to-check
    // boxes persisted in hive-welcome-state-v1. Content is static markup —
    // the dialog renders once on open, never in the hot render path.
    function _welcomeAgents(data) {
      return (data && Array.isArray(data.agents)) ? data.agents : [];
    }
    function _welcomeConfiguredValue(v) {
      const s = (v || '').trim().toLowerCase();
      return s !== '' && s !== 'auto' && s !== 'default';
    }
    function _welcomeAgentBackend(a) {
      return ((a && (a.cli || a.govBackend)) || '').trim();
    }
    function _welcomeAgentHasConcreteModel(a) {
      const backend = _welcomeAgentBackend(a);
      if (SINGLE_OPTION_BACKENDS.includes(backend) && modelsForBackend(backend).length === 1) return true;
      const model = ((a && (a.model || a.govModel)) || '').trim();
      if (!_welcomeConfiguredValue(model)) return false;
      return !isAutoModelSentinel(backend, model);
    }
    const WELCOME_STEPS = [
      {
        id: 'welcome',
        title: 'Welcome to your hive',
        // Reading this step is step one — opening the dialog completes it.
        auto: () => true,
        body: 'Hive is a <strong>co-maintainer</strong> for your project that never sleeps. It watches the things that pile up — issues, pull requests, failing CI, security alerts — and sends <strong>agents</strong> (AI coding tools such as Claude, Copilot or Codex, each run for one task and then stopped) to deal with them: triage, fixes, reviews. You decide how much they are allowed to do; humans keep every decision that matters. The <strong>governor</strong> is the scheduler that paces the agents as the workload changes (idle → quiet → busy → surge). This checklist covers the one-time choices that make the hive productive, and it updates itself as you complete them.'
      },
      {
        id: 'github-app',
        title: 'Install the Forge App (GitHub App)',
        // An operator-side credential failure is not an unfinished user step.
        // Return null ("cannot tell") rather than false, so the checklist does
        // not mark the user as having something left to do when the missing
        // piece is a key the hub operator supplies.
        auto: (data) => {
          if (!data) return null;
          if (ghAppIsOperatorSide(data.githubAppState)) return null;
          return data.githubAppRequired !== true;
        },
        body: 'The <strong>Forge App</strong> is the app Hive installs on your forge (your source control system, e.g., GitHub, GitHub Enterprise, GitLab, or Gitea) — on GitHub.com and GitHub Enterprise it is a GitHub App. It unlocks issues, advisory reports, and PRs on your primary project; without it the hive can only observe. ⏰ <strong>Do this in your first session:</strong> hosted hives that never finish this step are reclaimed (reaped) on a timer — the hive disappears from the hub and its URL stops working. If that happens, request a new hive from the hub and complete this step right away. On GitHub Enterprise the install page lives on your enterprise host, not github.com. Full instructions: <a href="https://github.com/hivecommons/hive/blob/v5/src/docs/github-app-setup.md" target="_blank" rel="noopener noreferrer">Forge App (GitHub App) setup</a>.',
        autoNote: 'Checks itself once the app is detected on your project.',
        actions: [
          { label: 'Install GitHub App', action: 'welcomeInstallGitHubApp' },
          { label: 'Re-check', action: 'welcomeRecheckGitHubApp', actionArgTypes: 'e' }
        ]
      },
      {
        id: 'methods',
        title: 'Pick a method for each agent',
        auto: (data) => {
          const agents = _welcomeAgents(data);
          if (agents.length === 0) return false;
          return agents.every(a => _welcomeConfiguredValue(_welcomeAgentBackend(a)));
        },
        body: 'Each agent card has a <strong>method dropdown</strong> — the method is the tool that actually runs the agent. <strong>claude / copilot / codex / goose / gemini / agy / omp / bob</strong> are command-line tools you log into (or give an API key), while <strong>vllm / llm-d / litellm / openrouter / watsonx</strong> talk to an inference endpoint — a server that answers model requests — set up under Settings → Model Gateways (endpoint + API key). Mix and match per agent.',
        autoNote: 'Checks itself once every configured agent has a resolved method/backend.',
        actions: [{ label: 'Show me', action: 'welcomeShowAgentCard' }]
      },
      {
        id: 'logins',
        title: 'Log in once per method',
        auto: (data) => {
          if (!data || !Array.isArray(data.agents) || data.agents.length === 0) return null;
          return data.agents.every(a => !_welcomeAgentNeedsLogin(a));
        },
        body: 'Credentials are shared <strong>per method, not per agent</strong> — one copilot device-code login covers every copilot agent, and claude uses a paste-the-code flow. Inference methods (vllm / llm-d / litellm / openrouter / watsonx) never log in; they use the endpoint + key from Settings → Model Gateways instead.',
        autoNote: 'Checks itself once every agent’s method is authenticated.',
        actions: [{ label: 'Show me', action: 'welcomeShowLogin' }]
      },
      {
        id: 'terminal',
        title: 'Watch an agent in its terminal',
        auto: null,
        body: 'The cyan <strong>▶ terminal</strong> button on each agent card opens the agent’s live tmux session: watch it work in real time, scroll its history, or type <strong>/login</strong> in the CLI when a login needs interaction. Etiquette: it is the agent’s working session — observe, don’t drive, except for logins.',
        actions: [{ label: '▶ Open a terminal', action: 'welcomeOpenTerminal' }]
      },
      {
        id: 'models',
        title: 'Choose models',
        auto: (data) => {
          const agents = _welcomeAgents(data);
          if (agents.length === 0) return false;
          return agents.every(_welcomeAgentHasConcreteModel);
        },
        body: 'The <strong>model dropdown</strong> on each agent card lists models auto-discovered for its method. 📌 pinning a model stops the governor from auto-selecting a different one — it never blocks your own changes.',
        autoNote: 'Checks itself once every configured agent has a concrete non-default model, or uses a backend with no model choice.',
        actions: [{ label: 'Show me', action: 'welcomeShowAgentCard' }]
      },
      {
        id: 'repos',
        title: 'Add projects &amp; mark the primary',
        auto: () => window._primaryRepo ? true : false,
        body: 'Settings → <strong>Projects</strong>: add every project the agents should watch, and set the <strong>primary</strong> project — it hosts the advisory issue and default markings.',
        autoNote: 'Checks itself once a primary project is set.',
        actions: [{ label: 'Show me', action: 'welcomeShowConfigTab', actionArg: 'Repos' }]
      },
      {
        id: 'cadences',
        title: 'Tune cadences',
        auto: null,
        body: 'Each agent’s ⚙️ config has a <strong>Cadences</strong> tab: per-mode kick intervals (idle / quiet / busy / surge), on-demand vs scheduled, and pausing. Set them per agent — the intensity bar on the Governor panel shows which mode is active right now.',
        actions: [{ label: 'Show me', action: 'welcomeShowCadences' }]
      },
      {
        id: 'add-agent',
        title: 'Add a custom agent',
        auto: null,
        body: 'The built-in agents are a starting point, not a limit. Use <strong>＋ Add agent</strong> in the Agents sidebar, the Agents section header, or this guide to open the create dialog. Start from the scanner / reviewer / quality / CI maintainer / guide templates, then pick the method, model, prompt template, and cadences — or <strong>Import</strong> one from the community registry.',
        actions: [{ label: '＋ Add an agent', action: 'welcomeAddAgent' }]
      },
      {
        id: 'acmm',
        title: 'Set your ACMM level',
        auto: (data) => !!(data && data.acmmLevelConfigured === true),
        body: 'The amber badge in the sidebar opens the <strong>maturity-level dialog</strong>. ACMM (AI Codebase Maturity Model) is the dial for how much the agents may do on their own: L1 (advise only — agents report, never change anything) → L6 (autonomous — agents may merge on green CI), each level adding to the last. A default is preset — confirm it matches how much autonomy you want.',
        autoNote: 'Checks itself once an ACMM level is explicitly saved on the hive.',
        actions: [{ label: 'Show me', action: 'welcomeShowACMM' }]
      }
    ];

    // Settings tab hosting the Trajectory Review controls (toggle,
    // reviewer endpoint/model, on-divergence mode) — see renderGovFeatures().
    // Named so the Getting Started deep link cannot drift from the tab list
    // in GOVERNOR_CONFIG_TABS. It must be REFERENCED, never re-quoted: an
    // actionArg of '${TRAJECTORY_CONFIG_TAB}' inside a single-quoted string
    // does not interpolate, and openConfigDialog() then silently falls back
    // to tabs[0] (#7452). static_index_test.go guards that.
    const TRAJECTORY_CONFIG_TAB = 'Features';

    // Collapsed "More to explore" one-liners under the core steps. These are
    // OPTIONAL/advanced capabilities, deliberately kept out of WELCOME_STEPS:
    // updateWelcomeProgress() counts only WELCOME_STEPS, so nothing listed
    // here can inflate the "N of M done" counter or read as a required step.
    const WELCOME_MORE_ITEMS = [
      { id: 'budget', label: 'Budget', desc: 'set a weekly token budget and watch the burn rate (Settings → Budget).', action: 'welcomeShowConfigTab', actionArg: 'Budget' },
      { id: 'notifications', label: 'Notifications', desc: 'route alerts to your channels (Settings → Notifications).', action: 'welcomeShowConfigTab', actionArg: 'Notifications' },
      { id: 'knowledge', label: 'Knowledge Base', desc: 'seed facts and patterns the agents reuse across passes.', action: 'welcomeShowSection', actionArg: 'knowledge-section' },
      { id: 'inception', label: 'Inception', desc: 'turn a raw idea (or an existing project) into knowledge-base facts and a starter project scaffold, step by step.', action: 'welcomeShowSection', actionArg: 'inception-section' },
      { id: 'contributors', label: 'Contributor agent (ClankeR)', desc: 'let outside contributors lend their own agents to your hive, with a leaderboard.', action: 'welcomeShowSection', actionArg: 'contributors-section' },
      { id: 'hub', label: 'Hub visibility', desc: 'make this hive public or private on the hub (Settings → Hub).', action: 'welcomeShowConfigTab', actionArg: 'Hub' },
      { id: 'backup', label: 'Backups', desc: 'set a backup encryption key, then download an encrypted archive of this hive from your account menu. No deployment or cluster access needed — the key is set right here (Settings → Security → Backup). Backups are refused while no key is set, so an archive is never written unencrypted.', action: 'welcomeShowConfigTab', actionArg: 'Security' },
      { id: 'experiment-lab', label: 'Experiment Lab <span class="welcome-more-tag">experimental</span>', desc: 'ask a research question about your hive (for example “does a stricter review prompt cut PR churn?”), let the lab collect a baseline, then test hypotheses against it. Off by default — switch it on under Settings → Dashboard.', action: 'welcomeShowConfigTab', actionArg: 'Dashboard' },
      { id: 'trajectory', label: 'Trajectory Review <span class="welcome-more-tag">advanced</span>', desc: 'an LLM reviewer periodically reads each running agent’s transcript and pauses or alerts you if the agent drifts from the task it was given. Needs one OpenAI-compatible reviewer endpoint (LiteLLM, vLLM, or llm-d) — without it the lane stays inert (Settings → Features).', action: 'welcomeShowConfigTab', actionArg: TRAJECTORY_CONFIG_TAB },
      { id: 'theme', label: 'Light / Dark mode', desc: 'switch layouts any time from the toggle in the top bar.', action: 'toggleLayout' }
    ];

    /** Done-state for one step: auto steps compute from the payload, manual
        steps read the persisted checkbox. */
    function welcomeStepDone(step, data) {
      if (step.auto) return step.auto(data) === true;
      return _welcomeState.checks[step.id] === true;
    }

    // Steps the user expanded this session (first undone step opens by default).
    const _welcomeOpenSteps = new Set();

    function renderWelcomeDialog() {
      const body = document.getElementById('welcome-body');
      if (!body) return;
      const data = window._lastStatus || null;
      if (_welcomeOpenSteps.size === 0) {
        const firstUndone = WELCOME_STEPS.find(s => !welcomeStepDone(s, data));
        _welcomeOpenSteps.add(firstUndone ? firstUndone.id : WELCOME_STEPS[0].id);
      }
      let html = '';
      html += '<div class="welcome-intro">New to Hive? Start here → <a href="https://docs.hivecommons.dev/docs/hive/getting-started" target="_blank" rel="noopener noreferrer" style="color:var(--accent);font-weight:600">Zero to Automation: Your Getting Started Guide</a> · <a class="text-accent fw-semibold" href="https://hivecommons.dev/discord" target="_blank" rel="noopener">Join our Discord</a><br><span style="font-size:0.85em;color:var(--muted)">⏰ Heads up: hosted hives that stay unconfigured or inactive are reaped on a timer. Install the Forge App — the app for your source control system (e.g., GitHub, GitHub Enterprise, GitLab, or Gitea) — in your first session (step 2), and if your hive ever disappears or its URL times out, request a new one from the hub and set it up promptly.</span></div>';
      let n = 0;
      for (const step of WELCOME_STEPS) {
        n++;
        const done = welcomeStepDone(step, data);
        const open = _welcomeOpenSteps.has(step.id);
        const checkCls = step.auto ? 'welcome-check' : 'welcome-check manual';
        const checkTitle = step.auto
          ? 'Checked automatically from the hive’s live status'
          : 'Click to mark this step done (saved in this browser)';
        const checkAttrs = step.auto ? '' : ` data-action="toggleWelcomeCheck" data-arg1="${step.id}" data-arg-types="e,s" role="checkbox" aria-checked="${done}" tabindex="0" data-keydown-action="toggleWelcomeCheck" data-arg1="${step.id}" data-arg-types="e,s" data-keys="Enter, "`;
        html += `<div class="welcome-step${done ? ' done' : ''}${open ? ' open' : ''}" id="welcome-step-${step.id}">`
          + `<div class="welcome-step-header" data-action="toggleWelcomeStep" data-arg0="${step.id}">`
          + `<span class="${checkCls}" title="${checkTitle}"${checkAttrs}>${done ? '✓' : ''}</span>`
          + `<span class="welcome-step-title">${n}. ${step.title}</span>`
          + `<span class="welcome-step-chevron">▶</span>`
          + `</div>`
          + `<div class="welcome-step-body">${step.body}`
          + (step.autoNote ? `<span class="welcome-auto-note">${step.autoNote}</span>` : '')
          + (step.actions ? `<div class="welcome-step-actions">` + step.actions.map(a => `<button class="hv-btn btn-secondary btn" data-action="${a.action}"${a.actionArg !== undefined ? ` data-arg0="${a.actionArg}"` : ''}${a.actionArgTypes ? ` data-arg-types="${a.actionArgTypes}"` : ''}>${a.label}</button>`).join('') + `</div>` : '')
          + `</div></div>`;
      }
      html += '<div class="welcome-more-title">More to explore</div>';
      for (const item of WELCOME_MORE_ITEMS) {
        html += `<div class="welcome-more-item"><span class="welcome-more-link" id="welcome-more-${item.id}" data-action="${item.action}"${item.actionArg !== undefined ? ` data-arg0="${item.actionArg}"` : ''}>${item.label}</span> — ${item.desc}</div>`;
      }
      body.innerHTML = html;
      updateWelcomeProgress();
    }

    function toggleWelcomeStep(id) {
      if (_welcomeOpenSteps.has(id)) _welcomeOpenSteps.delete(id); else _welcomeOpenSteps.add(id);
      const el = document.getElementById('welcome-step-' + id);
      if (el) el.classList.toggle('open', _welcomeOpenSteps.has(id));
    }

    function toggleWelcomeCheck(ev, id) {
      ev.stopPropagation();
      const step = WELCOME_STEPS.find(s => s.id === id);
      if (!step || step.auto) return;
      _welcomeState.checks[id] = _welcomeState.checks[id] !== true;
      saveWelcomeState();
      _syncWelcomeStepEl(step, window._lastStatus || null);
      updateWelcomeProgress();
    }

    /** Update one rendered step's check/done visuals in place. */
    function _syncWelcomeStepEl(step, data) {
      const el = document.getElementById('welcome-step-' + step.id);
      if (!el) return;
      const done = welcomeStepDone(step, data);
      el.classList.toggle('done', done);
      const check = el.querySelector('.welcome-check');
      if (check) {
        check.textContent = done ? '✓' : '';
        if (!step.auto) check.setAttribute('aria-checked', String(done));
      }
    }

    function updateWelcomeProgress() {
      const el = document.getElementById('welcome-progress');
      if (!el) return;
      const data = window._lastStatus || null;
      const done = WELCOME_STEPS.filter(s => welcomeStepDone(s, data)).length;
      el.textContent = done + ' of ' + WELCOME_STEPS.length + ' done';
    }

    // ── Welcome deep links ("Show me" wiring) ─────────────────────────
    // Every action reuses an existing entry point: openConfigDialog(tab),
    // openACMMDialog(), ocSelectAgent() agent focus, terminalUrl(), and the
    // GitHub App banner's install URL + recheckGitHubApp().

    /** First agent matching pred (else first agent) from the last payload. */
    function _welcomeFirstAgentName(pred) {
      const agents = (window._lastStatus && window._lastStatus.agents) || [];
      const match = pred ? agents.find(pred) : null;
      return (match || agents[0] || {}).name || null;
    }
    /** Focus the first agent card (method/model dropdowns live there). */
    function welcomeShowAgentCard() {
      closeWelcomeDialog();
      const name = _welcomeFirstAgentName();
      if (name) ocSelectAgent(name);
    }
    /** Focus the first agent whose method still needs a login. */
    function welcomeShowLogin() {
      closeWelcomeDialog();
      const name = _welcomeFirstAgentName(_welcomeAgentNeedsLogin);
      if (name) ocSelectAgent(name);
    }
    /** Open the first agent's live tmux session in a new tab. */
    function welcomeOpenTerminal() {
      const name = _welcomeFirstAgentName();
      if (!name) { showToast('No agents yet — terminals open from each agent card', 'info'); return; }
      openTerminal(name);
    }
    /** Open the GitHub App install page (same URL the banner links to). */
    function welcomeInstallGitHubApp() {
      // Never send the user to an install page when the block is operator-side:
      // installing again cannot fix a key they do not control.
      if (window._lastStatus && ghAppIsOperatorSide(window._lastStatus.githubAppState)) {
        showToast('This hive is waiting on GitHub App credentials from the hub operator — installing the App again will not help. Contact your hub administrator.', 'info');
        return;
      }
      const url = (window._lastStatus && window._lastStatus.githubAppInstallURL) || '';
      if (url) { hiveExportViewer({title:'GitHub App install URL',text:url,sourceURL:url,allowOpen:true}); return; }
      const installed = !!(window._lastStatus) && window._lastStatus.githubAppRequired !== true;
      showToast(installed ? 'GitHub App already installed ✓' : 'Install URL not available yet — waiting for the next status refresh', 'info');
    }
    /** Re-check installation via the banner action, then flip the
        auto-check immediately instead of waiting for the next SSE tick. */
    async function welcomeRecheckGitHubApp(ev) {
      const btn = ev ? ev.target : null;
      if (btn) { btn.disabled = true; btn.textContent = 'Checking…'; }
      await recheckGitHubApp();
      // recheckGitHubApp() hides the banner when the app is detected —
      // mirror that outcome onto the cached payload.
      const banner = document.getElementById('gh-app-install-banner');
      const installed = !!(banner && banner.hasAttribute('hidden'));
      if (installed && window._lastStatus) window._lastStatus.githubAppRequired = false;
      if (btn) { btn.disabled = false; btn.textContent = installed ? 'Installed ✓' : 'Re-check'; }
      refreshWelcomeAutoChecks(window._lastStatus || null);
    }
    /** Jump to a Settings tab (Repos, Agents, Budget, …). */
    function welcomeShowConfigTab(tab) {
      closeWelcomeDialog();
      openConfigDialog('governor', null, tab);
    }
    /** Open the per-agent config dialog on the Cadences tab for an available
        agent — per-mode kick intervals are a per-agent setting, so this lands
        the user where they actually edit them (not the Governor → Agents list). */
    function welcomeShowCadences() {
      closeWelcomeDialog();
      const name = _welcomeFirstAgentName();
      if (!name) { showToast('No agents yet — cadences are set from each agent’s ⚙️ config', 'info'); return; }
      openConfigDialog('agent', name, 'Cadences');
    }
    /** Open the Create Agent dialog (config dialog in create mode). */
    function welcomeAddAgent() {
      closeWelcomeDialog();
      openAddAgentDialog();
    }
    /** Open the ACMM maturity-level dialog. */
    function welcomeShowACMM() {
      closeWelcomeDialog();
      openACMMDialog();
    }
    /** Navigate to a dashboard section (knowledge, contributors, …). */
    function welcomeShowSection(section) {
      closeWelcomeDialog();
      ocNavigate(section);
    }

    /** Called from render() on every status refresh so auto-checks flip
        live. No-op unless the dialog is actually open. */
    function refreshWelcomeAutoChecks(data) {
      if (!_welcomeWriteAccess) return;
      const overlay = document.getElementById('welcome-overlay');
      if (!overlay || overlay.classList.contains('hidden')) return;
      for (const step of WELCOME_STEPS) {
        if (step.auto) _syncWelcomeStepEl(step, data);
      }
      updateWelcomeProgress();
    }

    checkGHAuth();
    dashboardSetInterval('welcome-gh-auth', checkGHAuth, GH_AUTH_CHECK_INTERVAL_MS);
    initWelcomeDialog();
    initNPSSurvey();

