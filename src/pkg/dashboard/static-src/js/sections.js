    // ── Collapsible sections (issue #1533) ──
    const SECTION_LS_PREFIX = 'hive-section-collapsed-';
    // Beginner-friendly defaults (#12014): a browser with NO prior dashboard
    // state starts with these non-critical cards collapsed so a new operator
    // first sees the cards that say whether the hive is alive and merging
    // (Overview, Governor, Projects, Review, Approvals, Agents, Agent activity).
    // The per-card value under SECTION_LS_PREFIX is '1' (collapsed) or '0'
    // (explicitly expanded); a missing key falls back to this default. The
    // marker key records whether the browser predates the defaults ('legacy',
    // keeps the historical all-expanded default forever) or is new ('v1').
    const DASHBOARD_DEFAULT_COLLAPSED_SECTIONS = Object.freeze([
      'pr-throughput-section', 'token-panel', 'cost-panel', 'advisory-section', 'knowledge-section',
      'contributors-section', 'inception-section', 'acmm-eval-section', 'debug-section', 'audit-section',
      'logs-section', 'nous-section', 'faq-section'
    ]);
    const DASHBOARD_COLLAPSE_DEFAULTS_KEY = 'hive.dashboard.collapseDefaults';
    const DASHBOARD_COLLAPSE_DEFAULTS_VERSION = 'v1';
    const DASHBOARD_COLLAPSE_DEFAULTS_LEGACY = 'legacy';
    // Any persisted dashboard key proves a prior visit; the auth token alone
    // does not (it can be pasted before the dashboard ever rendered).
    const DASHBOARD_PRIOR_STATE_KEY_PREFIX = 'hive';
    const DASHBOARD_PRIOR_STATE_IGNORED_KEYS = Object.freeze(['hive-token']);
    const AGENT_COMPACT_LS_PREFIX = 'hive-agent-compact-';
    const SECTION_TRANSITION_MS = 200;
    const SECTION_EXPANDED_MAX_HEIGHT = 'none';
    const SECTION_MAX_HEIGHT_PROPERTY = 'max-height';
    const NAV_JUMP_SETTLE_MS = 350;
    const NAV_JUMP_RESIZE_QUIET_MS = 60;
    const ALL_AGENTS_COMPACT_KEY = 'hive-all-agents-compact';

    const STATUS = Object.freeze({
      running: { label: 'running', status: 'ok' },
      working: { label: 'working', status: 'ok' },
      ready: { label: 'ready', status: 'ok' },
      passed: { label: 'passed', status: 'ok' },
      idle: { label: 'idle', status: 'neutral' },
      paused: { label: 'paused', status: 'warn' },
      pause: { label: 'paused', status: 'warn' },
      stopped: { label: 'stopped', status: 'error' },
      starting: { label: 'starting', status: 'info' },
      down: { label: 'down', status: 'error' },
      error: { label: 'error', status: 'error' },
      failed: { label: 'failed', status: 'error' },
      blocked: { label: 'blocked', status: 'error' },
      degraded: { label: 'degraded', status: 'warn' },
      needs_attention: { label: 'needs attention', status: 'attention' },
      needs_context: { label: 'needs context', status: 'warn' },
      needs_login: { label: 'needs login', status: 'attention' },
      on_demand: { label: 'on demand', status: 'info' },
      active: { label: 'active', status: 'info' },
      initializing: { label: 'initializing', status: 'info' },
      info: { label: 'info', status: 'info' },
      off: { label: 'off', status: 'neutral' },
      off_healthy: { label: 'no cadence', status: 'neutral' },
      disabled: { label: 'disabled', status: 'neutral' },
      unknown: { label: 'unknown', status: 'neutral' },
    });
    function statusKey(state) {
      return String(state || 'unknown').trim().toLowerCase().replace(/[-\s]+/g, '_');
    }
    function statusMeta(state) {
      return STATUS[statusKey(state)] || STATUS.unknown;
    }
    function statusAttr(state) {
      return statusMeta(state).status;
    }
    function statusBadgeHTML(state, label, title, extraClass) {
      const meta = statusMeta(state);
      const cls = extraClass ? ' ' + extraClass : '';
      const tip = title ? ` title="${esc(title)}"` : '';
      return `<span class="badge-status status-badge${cls}" data-status="${meta.status}"${tip}>${esc(label || meta.label)}</span>`;
    }
    function sectionSettingsGear(label, action, title, extraAttrs) {
      return `<button type="button" class="config-info section-settings-gear" data-section-settings="1" data-action="${esc(action)}" data-stop="1"${extraAttrs || ''} title="${esc(title || (label + ' settings'))}" aria-label="${esc(label)} settings"><svg class="section-settings-gear-icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path fill="currentColor" fill-rule="evenodd" d="M13.3 2.25h-2.6l-.42 2.66a7.46 7.46 0 0 0-1.24.52L6.86 3.84 5.02 5.68 6.61 7.86c-.22.4-.4.82-.52 1.25l-2.66.42v2.6l2.66.42c.13.43.3.85.52 1.25l-1.59 2.18 1.84 1.84 2.18-1.59c.4.22.81.39 1.24.52l.42 2.66h2.6l.42-2.66c.43-.13.85-.3 1.24-.52l2.18 1.59 1.84-1.84-1.59-2.18c.22-.4.4-.82.52-1.25l2.66-.42v-2.6l-2.66-.42a7.4 7.4 0 0 0-.52-1.25l1.59-2.18-1.84-1.84-2.18 1.59c-.4-.22-.81-.39-1.24-.52l-.42-2.66ZM12 14.9a2.9 2.9 0 1 0 0-5.8 2.9 2.9 0 0 0 0 5.8Z"/></svg></button>`;
    }

    // Pop out / full screen / return (hivecommons/hive#15829), rendered next to
    // the activity monitor's gear; handlers live in js/tracker-popout.js and
    // css/popout.css decides which of the three show in which mode.
    const STAGE_TRACKER_HEADER_TOOLS_HTML = '<button type="button" class="config-info section-settings-gear ham-header-tool ham-popout-btn" id="activity-monitor-popout" data-action="openStageTrackerPopout" data-stop="1" title="Pop out — open the activity monitor in its own window, for another monitor" aria-label="Pop out the Hive activity monitor into its own window">⧉</button>'
      + '<button type="button" class="config-info section-settings-gear ham-header-tool ham-fullscreen-btn" id="activity-monitor-fullscreen" data-action="toggleStageTrackerFullscreen" data-stop="1" aria-pressed="false" title="Full screen — fill the screen with the activity monitor (f, Esc exits)" aria-label="Full screen — fill the screen with the activity monitor (f, Esc exits)">⛶</button>'
      + '<button type="button" class="config-info section-settings-gear ham-header-tool ham-display-btn" id="activity-monitor-display" data-action="toggleStageTrackerDisplay" data-stop="1" aria-pressed="false" title="Display — show the activity monitor as a wall view with no row actions (d; Esc or d leaves)" aria-label="Display — show the activity monitor as a wall view with no row actions (d; Esc or d leaves)">📺</button>'
      + '<button type="button" class="ham-popout-return" id="activity-monitor-return" data-action="stageTrackerReturnToDashboard" data-stop="1" title="Focus the main dashboard window and close this pop-out">⤶ Return to dashboard</button>';
    const DASHBOARD_SECTION_CARD_CONFIG = Object.freeze({
      'overview-section': { title: '🍩 Overview', summary: 'overview', actionIds: ['overview-chart-settings'], settingsHtml: sectionSettingsGear('Overview', 'toggleOverviewChartSettings', 'Configure Overview chart types, carousel timing, and transitions', ' aria-haspopup="true" aria-expanded="false" aria-controls="overview-chart-settings"') },
      'governor': { title: '🚦 Governor', summary: 'idle', settingsHtml: sectionSettingsGear('Governor', 'openConfigDialog', 'Open Governor configuration — manage agents, thresholds, budget, notifications, health checks, and sensing patterns', ' data-arg0="governor"') },
      'advisory-section': { title: '📣 Advisory', summary: '0 advisories' },
      'advisory-digest-section': { title: '📋 Advisory Digest', summary: '0 findings' },
      'hive-advice-section': { title: '🧭 Hive Advice', summary: '0 recommendations', badgeHtml: '<span class="dash-card-badge">weekly</span>' },
      'fleet-report-section': { title: '📡 Fleet Report Preview', summary: '0 reports', badgeHtml: '<span class="dash-card-badge">dry-run</span>' },
      'acmm-reco-section': { title: '⬆️ Ready to level up?', summary: 'L— · stay', badgeHtml: '<span class="dash-card-badge">advisory</span>' },
      'lifecycle-section': { title: '🔀 Lifecycle Timeline', summary: '0 in flight · 0 merged' },
      'pr-throughput-section': { title: '📈 Throughput', summary: '0 merged · 0 opened (24h)', subtitleHtml: 'pull/merge requests across tracked forges' },
      'token-panel': { title: '🪙 Tokens', summary: '0 sessions' },
      'cost-panel': { title: '💵 Cost', summary: '$0' },
      'repos-section': { title: '📦 Projects', summary: '0 repos', settingsHtml: sectionSettingsGear('Projects', 'gh1', 'Manage monitored projects') },
      'acmm-eval-section': { title: '🎯 ACMM Eval', summary: 'L—' },
      'approvals-section': { title: '✋ Approvals', summary: '0 pending' },
      'audit-section': { title: '📜 Audit Log', summary: '0 events today' },
      'review-queue-section': { title: '🔎 Review', summary: '0 PRs' },
      'review-pipeline-subsection': { title: 'Pipeline', summary: '—' },
      'review-queue-subsection': { title: 'Queue', summary: '—' },
      'nous-section': { title: '🧪 Experiment Lab (nous)', summary: '—', settingsHtml: sectionSettingsGear('Experiment Lab', 'openNousConfig', 'Experiment Lab Configuration') },
      'inception-section': { title: '💡 Inception (spektacular)', summary: 'ready', badgeHtml: '<a class="dash-card-badge" href="https://github.com/jumppad-labs/spektacular" target="_blank" rel="noopener noreferrer" data-stop="1" title="Spektacular project">⚡ Powered by Spektacular</a>' },
      'knowledge-section': { title: '📚 Knowledge', summary: '0 facts' },
      'contributors-section': { title: '👥 Contributors', summary: '0 online / 0 registered' },
      'debug-section': { title: '🩺 Diagnostics', summary: 'health' },
      'logs-section': { title: '📋 Agent Logs', summary: 'logs' },
      'agents-section': { title: '🤖 Agents', summary: '0 running / 0 total' },
      'agent-activity-section': { title: '📡 Agent activity', summary: '0 working / 0 total' },
      'activity-monitor-section': { title: '📍 Hive activity monitor', summary: '0 items', actionIds: ['activity-monitor-settings'], settingsHtml: sectionSettingsGear('Hive activity monitor', 'toggleStageTrackerSettings', 'Configure Hive activity monitor sounds — open buzz, merge zap, your own WAV files, volume', ' aria-haspopup="true" aria-expanded="false" aria-controls="activity-monitor-settings"') + STAGE_TRACKER_HEADER_TOOLS_HTML },
      'faq-section': { title: '❓ FAQ', summary: 'questions' },
    });
    // Section help marks (#10915). Each entry's help sentence is word for word
    // the first line of its entry in src/docs/dashboard-sections.md, and the
    // link anchor is derived from its heading. Runs and Platform exist only
    // on the v6 line, so the help page documents them but this map does not.
    const DASHBOARD_HELP_PAGE = 'dashboard-sections';
    const DASHBOARD_HELP_CLICK_LINE = 'Click (or press Enter) for the full explanation';
    const DASHBOARD_DOCS_LINES = ['v5', 'v6'];
    const DASHBOARD_SECTION_HELP = Object.freeze({
      'overview-section': { heading: 'Overview', help: 'How many issues and pull requests Hive is tracking right now, and how many of them it can work on.' },
      'governor': { heading: 'Governor', help: 'How busy Hive thinks your projects are right now, and how often it wakes its workers as a result.' },
      'governor-pr-models-section': { heading: 'PRs by model', help: "Which AI models wrote the pull requests Hive opened, how well each model's work landed, and how the models rank against each other." },
      'advisory-section': { heading: 'Advisory', help: 'Reports, advice and suggestions Hive has written about your projects and about how it is running.' },
      'advisory-digest-section': { heading: 'Advisory Digest', help: "The open findings Hive's workers have written down, such as bugs, ideas and advice, grouped by who wrote them." },
      'hive-advice-section': { heading: 'Hive Advice', help: 'A short weekly list of things Hive recommends you do to keep work moving on your projects.' },
      'fleet-report-section': { heading: 'Fleet Report Preview', help: 'Problems Hive has noticed in itself that it would report to the Hive maintainers, and problems that have since gone away.' },
      'acmm-reco-section': { heading: 'Ready to level up?', help: 'Whether Hive thinks you could safely let it do more on its own, and what still has to be true first.' },
      'lifecycle-section': { heading: 'Lifecycle Timeline', help: 'A timeline of recent pull requests, from when they were opened to when they were merged or got stuck.' },
      'pr-throughput-section': { heading: 'Throughput', help: 'How many pull requests were opened, merged and closed over a chosen time window, and who did the work.' },
      'token-panel': { heading: 'Tokens', help: 'How much text the AI models read and wrote for Hive recently, broken down by model and by worker.' },
      'cost-panel': { heading: 'Cost', help: "What Hive's use of AI models would cost at public per-token list prices; an estimate, not a bill." },
      'repos-section': { heading: 'Projects', help: 'One card per repository Hive looks after, listing its open issues and pull requests grouped by what each one needs next.' },
      'acmm-eval-section': { heading: 'ACMM Eval', help: 'How ready your repositories are for Hive to work on them alone, scored against a published checklist.' },
      'approvals-section': { heading: 'Approvals', help: 'Actions Hive wants to take that are waiting for you to approve or reject them.' },
      'audit-section': { heading: 'Audit Log', help: 'A searchable record of everything Hive has done, newest first. The collapsed “sensitive” badge opens a 24h sensitive-only view, matching the server-side risk count.' },
      'review-queue-section': { heading: 'Review', help: "Review Pipeline shows pull requests by stage first; Review Queue lists the same open pull requests in Hive's suggested review order." },
      'review-pipeline-subsection': { heading: 'Review Pipeline', help: 'Each open PR in the stage the review pipeline derived for it (GET /api/review/pipeline), with reviewers, P0–P3 findings, the fix-loop counter and the next action.' },
      'review-queue-subsection': { heading: 'Queue', help: 'Every open PR across the governed repos in one deterministic rank order (GET /api/review/queue), regardless of author type, with the reasons for each position.' },
      'nous-section': { heading: 'Experiment Lab', help: 'An experimental planner that tries out code changes in your repositories and records what it learned.' },
      'inception-section': { heading: 'Inception', help: 'A step-by-step guide that turns a new project idea into a first set of files and planned work.' },
      'knowledge-section': { heading: 'Knowledge', help: 'The facts Hive has learned about your projects, where they came from, and how much Hive trusts each one.' },
      'contributors-section': { heading: 'Contributors', help: 'The people who lend their computers or AI helpers to your projects, and how far Hive trusts each of them.' },
      'debug-section': { heading: 'Diagnostics', help: 'Health checks for Hive itself, such as its connections, credentials and safety switches, to help you find out why something is not working.' },
      'logs-section': { heading: 'Agent Logs', help: 'The raw text output of each worker, for following what it is doing line by line.' },
      'agents-section': { heading: 'Agents', help: 'Each worker Hive runs, what it is doing now, and buttons to pause, restart or configure it.' },
      'agent-activity-section': { heading: 'Agent activity', help: 'One line per worker showing what it is doing now, how long since it last acted, and which issues it has reserved.' },
      'activity-monitor-section': { heading: 'Hive activity monitor', help: 'A progress strip per open issue and pull request showing which step of the pipeline it is on and who must act next.' },
      'faq-section': { heading: 'FAQ', help: 'Short answers to the questions people ask once a hive is running, each linking to the guide that explains it fully.' },
    });
    // The one place that decides where dashboard help lives: every help mark and
    // Overview "Learn more" link is built here, so moving them (for example to
    // docs.hivecommons.dev) is a single change. Builds that are not on a known
    // release line (forks, feature branches, unknown versions) open v5's help.
    function dashboardDocsHref(page, anchor) {
      const line = (typeof window !== 'undefined' && window._hiveDocsLine) || 'v5';
      return `https://github.com/hivecommons/hive/blob/${line}/src/docs/${page}.md${anchor ? '#' + anchor : ''}`;
    }
    function dashboardDocsHrefFromPath(path) {
      const m = /^\/docs\/([\w-]+)\.md(?:#([\w-]+))?$/.exec(String(path || ''));
      return m ? dashboardDocsHref(m[1], m[2] || '') : String(path || '');
    }
    function setDashboardDocsLine(branch) {
      const m = /^(v\d+)(?:\.\d+)*$/.exec(String(branch || '').trim());
      window._hiveDocsLine = m && DASHBOARD_DOCS_LINES.includes(m[1]) ? m[1] : '';
      document.querySelectorAll('a[data-docs-page]').forEach(a => { a.href = dashboardDocsHref(a.dataset.docsPage, a.dataset.docsAnchor || ''); });
    }
    function dashboardSectionHelpAnchor(heading) {
      return String(heading || '').toLowerCase().replace(/[^\w\s-]/g, '').trim().replace(/\s/g, '-');
    }
    function dashboardSectionHelpMark(sectionId) {
      const entry = DASHBOARD_SECTION_HELP[sectionId];
      if (!entry) return '';
      const anchor = dashboardSectionHelpAnchor(entry.heading);
      const tipId = sectionId + '-help-tip';
      return `<a class="config-info section-help-mark" href="${esc(dashboardDocsHref(DASHBOARD_HELP_PAGE, anchor))}" data-docs-page="${DASHBOARD_HELP_PAGE}" data-docs-anchor="${esc(anchor)}" target="_blank" rel="noopener noreferrer" data-action="gh3" data-stop="1" title="" aria-label="Help: ${esc(entry.heading)}" aria-describedby="${esc(tipId)}">?<span id="${esc(tipId)}" class="config-tooltip section-help-tip" role="tooltip">${esc(entry.help)}<span class="section-help-tip-action">${DASHBOARD_HELP_CLICK_LINE}</span></span></a>`;
    }
    // Settings help marks share the card "?" mark's markup and styling (#11213).
    // tipHtml is the setting's tooltip markup, inserted as-is like the inline
    // tooltips it replaced; callers escape any dynamic text inside it.
    let settingHelpSeq = 0;
    function settingHelpMark(page, anchor, label, tipHtml) {
      const tipId = 'setting-help-tip-' + (++settingHelpSeq);
      return `<a class="config-info section-help-mark" href="${esc(dashboardDocsHref(page, anchor))}" data-docs-page="${esc(page)}" data-docs-anchor="${esc(anchor)}" target="_blank" rel="noopener noreferrer" data-action="gh3" data-stop="1" title="" aria-label="Help: ${esc(label)}" aria-describedby="${tipId}">?<span id="${tipId}" class="config-tooltip" role="tooltip">${tipHtml}</span></a>`;
    }
    function fillSectionHelpMarks() {
      document.querySelectorAll('[data-section-help]').forEach(el => {
        if (!el.querySelector('.section-help-mark')) el.innerHTML = dashboardSectionHelpMark(el.dataset.sectionHelp);
      });
    }
    function sectionCardHeader(opts) {
      opts = opts || {};
      const id = opts.id || '';
      const title = opts.titleHtml || esc(opts.title || id || 'Section');
      const bodyId = opts.bodyId || (id + '-body');
      const action = opts.action || 'toggleSection';
      const keyAction = opts.keyAction || 'sectionHeaderKey';
      const actionAttrs = action === 'toggleSection'
        ? `data-action="toggleSection" data-keydown-action="${keyAction}" data-arg-types="e,s" data-arg0="${esc(id)}" data-arg1="${esc(id)}" data-keys="*"`
        : `data-action="${esc(action)}" data-keydown-action="${keyAction}" data-arg-types="e" data-keys="*"`;
      const gripLabel = (opts.gripLabel || String(opts.title || id || 'section').replace(/^[^A-Za-z0-9]+/, '').trim() || id || 'section');
      const grip = opts.grip === false ? '' : `<button type="button" class="dashboard-grip" data-dashboard-grip data-stop="1" aria-label="Move ${esc(gripLabel)} section" aria-pressed="false">⠿</button>`;
      const subtitle = opts.subtitleHtml ? `<span class="dash-card-subtitle">${opts.subtitleHtml}</span>` : '';
      const settings = opts.settingsHtml || '';
      const badges = opts.badgeHtml ? `<span class="dash-card-badges">${opts.badgeHtml}</span>` : '<span class="dash-card-badges"></span>';
      const actions = opts.actionsHtml ? `<span class="dash-card-actions">${opts.actionsHtml}</span>` : '<span class="dash-card-actions"></span>';
      const summaryText = textOrDash(opts.summaryHtml || opts.summary || '');
      const summary = `<span class="dash-card-summary ${opts.summaryClass || ''}" id="${esc(opts.summaryId || (id + '-summary'))}" title="${esc(opts.summaryTitle || summaryText || 'Collapsed summary')}" aria-label="${esc(opts.summaryTitle || summaryText || 'Collapsed summary')}">${visualSectionSummary(id, summaryText, opts.summaryTitle)}</span>`;
      const cls = ['dash-card-header','section-header-toggle',opts.headerClass || ''].filter(Boolean).join(' ');
      // opts.collapsed renders the persisted collapsed state into the markup
      // so a re-render never inserts an expanded body that is collapsed a
      // moment later (#12512).
      const chevronCls = 'section-chevron ' + [opts.chevronClass || '', opts.collapsed ? 'collapsed' : ''].filter(Boolean).join(' ');
      return `<div class="${cls}" data-section-card-header="1" role="button" tabindex="0" aria-expanded="${opts.collapsed ? 'false' : 'true'}" aria-controls="${esc(bodyId)}" ${actionAttrs} title="${esc(opts.toggleTitle || 'Collapse / expand this section')}">${grip}<span class="${chevronCls}" aria-hidden="true">▼</span><span class="dash-card-title">${title}${dashboardSectionHelpMark(id)}${settings}${subtitle}</span>${badges}${actions}${summary}</div>`;
    }
    function sectionCardShell(opts, bodyHtml) {
      opts = opts || {};
      const id = opts.id || '';
      const bodyId = opts.bodyId || (id + '-body');
      const panelClass = ['dash-card', opts.panelClass || '', opts.collapsed ? 'collapsed' : ''].filter(Boolean).join(' ');
      const bodyClass = ['dash-card-body','section-body', opts.bodyClass || '', opts.collapsed ? 'collapsed' : ''].filter(Boolean).join(' ');
      return `<div class="${panelClass}" data-section-card-shell="1">${sectionCardHeader(opts)}<div class="${bodyClass}" id="${esc(bodyId)}">${bodyHtml || ''}</div></div>`;
    }
    function sectionHeaderKey(ev, sectionId) {
      if (!ev || (ev.key !== 'Enter' && ev.key !== ' ' && ev.key !== 'Spacebar')) return;
      const header = this && this.nodeType === 1 ? this : null;
      if (header && ev.target && ev.target !== header && ev.target.closest && ev.target.closest('button,a,input,select,textarea,[role="button"]')) return;
      ev.preventDefault();
      toggleSection(sectionId);
    }
    function dashboardSectionDirectHeader(section) {
      if (!section) return null;
      for (const child of Array.from(section.children)) {
        if (child.classList && child.classList.contains('section-header-toggle')) return child;
        const nested = child.matches && child.matches('div') && child.querySelector && child.querySelector(':scope > .section-header-toggle');
        if (nested && !child.classList.contains('dash-card')) return nested;
      }
      return null;
    }
    function ensureSectionCard(sectionId) {
      const section = document.getElementById(sectionId);
      if (!section || section.querySelector(':scope > .dash-card') || sectionId === 'token-panel' || sectionId === 'cost-panel' || sectionId === 'governor') return;
      const cfg = DASHBOARD_SECTION_CARD_CONFIG[sectionId] || { title: sectionId, summary: '' };
      const oldHeader = dashboardSectionDirectHeader(section);
      if (!oldHeader) return;
      const body = oldHeader.parentElement === section
        ? (Array.from(section.children).find(el => el !== oldHeader && el.classList && el.classList.contains('section-body')) || null)
        : (oldHeader.parentElement.nextElementSibling && oldHeader.parentElement.nextElementSibling.classList && oldHeader.parentElement.nextElementSibling.classList.contains('section-body') ? oldHeader.parentElement.nextElementSibling : null);
      if (!body) return;
      const bodyId = sectionId + '-body';
      const template = document.createElement('template');
      template.innerHTML = sectionCardShell({ id: sectionId, title: cfg.title, summaryHtml: cfg.summary, bodyId: bodyId, badgeHtml: cfg.badgeHtml, subtitleHtml: cfg.subtitleHtml, settingsHtml: cfg.settingsHtml }, '');
      const card = template.content.firstElementChild;
      const newHeader = card.querySelector('.dash-card-header');
      const badges = newHeader.querySelector('.dash-card-badges');
      const actions = newHeader.querySelector('.dash-card-actions');
      const moveSource = oldHeader.parentElement === section ? oldHeader : oldHeader.parentElement;
      Array.from(moveSource.querySelectorAll('[id$="-badge"], #repos-host-badge, #repos-needs-human, #review-queue-count, #review-pipeline-count, #kb-status-badge, #contributors-status-badge')).forEach(el => badges.appendChild(el));
      Array.from(moveSource.querySelectorAll('button[data-stop="1"], a[data-stop="1"], a[data-action]')).forEach(el => {
        if (el.matches('[data-dashboard-grip]')) return;
        el.setAttribute('data-stop', '1');
        actions.appendChild(el);
      });
      const shellBody = card.querySelector('.dash-card-body');
      body.classList.add('dash-card-body');
      body.id = bodyId;
      shellBody.replaceWith(body);
      const insertion = oldHeader.parentElement === section ? oldHeader : oldHeader.parentElement;
      section.insertBefore(card, insertion);
      insertion.remove();
    }
    function normalizeSectionCardChrome(sectionId) {
      const card = document.querySelector(`#${sectionId} .dash-card`);
      if (!card) return;
      card.querySelectorAll('.dash-card-actions > *').forEach(el => el.classList.add('dash-card-action'));
      card.querySelectorAll('.dash-card-badges > *').forEach(el => el.classList.add('dash-card-badge'));
    }
    function refreshSectionCardShell(sectionId) {
      ensureSectionCard(sectionId);
      normalizeSectionCardChrome(sectionId);
      applySectionCollapse(sectionId);
    }
    function initializeDashboardSectionCards() {
      Object.keys(DASHBOARD_SECTION_CARD_CONFIG).forEach(refreshSectionCardShell);
      fillSectionHelpMarks();
    }
    const MINI_SUMMARY_HISTORY_PREFIX = 'hive_visual_summary_history:';
    const MINI_SUMMARY_HISTORY_POINTS = 48;
    const REVIEW_QUEUE_WINDOW_KEY = 'hive_review_queue_history_window';
    const REVIEW_QUEUE_MS_PER_HOUR = 3600e3;
    const REVIEW_QUEUE_HOURS_PER_DAY = 24;
    const REVIEW_QUEUE_DAYS_PER_WEEK = 7;
    const REVIEW_QUEUE_WINDOWS = { '24h': REVIEW_QUEUE_HOURS_PER_DAY * REVIEW_QUEUE_MS_PER_HOUR, '7d': REVIEW_QUEUE_DAYS_PER_WEEK * REVIEW_QUEUE_HOURS_PER_DAY * REVIEW_QUEUE_MS_PER_HOUR };
    const REVIEW_QUEUE_SPARK_W = 128;
    const REVIEW_QUEUE_SPARK_H = 24;
    const REVIEW_QUEUE_SPARK_PAD_Y = 2;
    let _reviewQueueWindow = (() => {
      try {
        const saved = localStorage.getItem(REVIEW_QUEUE_WINDOW_KEY);
        return Object.prototype.hasOwnProperty.call(REVIEW_QUEUE_WINDOWS, saved) ? saved : '24h';
      } catch { return '24h'; }
    })();
    function miniHiveKey() {
      return String(((window._lastStatus || {}).hiveId) || 'default').trim() || 'default';
    }
    function miniHistoryStoreKey() {
      return MINI_SUMMARY_HISTORY_PREFIX + miniHiveKey();
    }
    function miniLoadHistoryStore() {
      try { return JSON.parse(localStorage.getItem(miniHistoryStoreKey()) || '{}') || {}; } catch { return {}; }
    }
    function miniSaveHistoryStore(store) {
      try { localStorage.setItem(miniHistoryStoreKey(), JSON.stringify(store)); } catch {}
    }
    function miniHistorySeries(sectionId, metric, value) {
      const key = sectionId + ':' + metric;
      const store = miniLoadHistoryStore();
      const series = Array.isArray(store[key]) ? store[key].slice(-MINI_SUMMARY_HISTORY_POINTS) : [];
      const n = Number(value);
      if (Number.isFinite(n)) {
        const last = series[series.length - 1];
        if (!last || last.v !== n) series.push({ t: Date.now(), v: n });
        store[key] = series.slice(-MINI_SUMMARY_HISTORY_POINTS);
        miniSaveHistoryStore(store);
      }
      return series.map(p => Number(p && p.v) || 0);
    }
    function miniNumericParts(text) {
      return String(text || '').match(/-?\d+(?:\.\d+)?/g) || [];
    }
    function miniFirstNumber(text) {
      const nums = miniNumericParts(text);
      return nums.length ? Number(nums[0]) : 0;
    }
    function miniTitleTag(title) {
      return `<title>${escapeHtml(title || 'summary')}</title>`;
    }
    function miniWrap(sectionId, text, inner) {
      const label = textOrDash(text);
      return `<span class="mini-summary-strip" role="img" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}" data-mini-summary="${escapeHtml(sectionId)}">${inner}</span>`;
    }
    function miniSpark(series, color, label) {
      const values = (Array.isArray(series) ? series : []).map(Number).filter(Number.isFinite);
      const usable = values.length >= 2 ? values : [0, values[0] || 0];
      return renderSparkline(null, usable, { width: 74, height: 24, color: color || 'var(--cyan)', label: label || 'history', title: label || 'history', ariaLabel: label || 'history', svgClass: 'mini-spark', wrapperClass: 'mini-spark-wrap' });
    }
    const AUDIT_COLLAPSED_SPARK_WIDTH = 96;
    const AUDIT_COLLAPSED_SPARK_HEIGHT = 24;
    function auditDisplayActor(entry) {
      return String((entry && (entry.user_name || entry.user)) || '—');
    }
    function auditEntryTooltip(entry) {
      if (!entry) return 'No audit entries yet';
      const pieces = [
        auditDisplayActor(entry),
        String(entry.action || '—'),
        relativeTimeShort(entry.ts) || String(entry.ts || '—'),
        String(entry.ts || '—')
      ];
      const detail = String(entry.detail || '').trim();
      const agent = String(entry.agent || '').trim();
      if (agent) pieces.push(`agent ${agent}`);
      if (detail) pieces.push(detail);
      return pieces.join(' · ');
    }
    function renderAuditCollapsedSummary(text, title) {
      const summary = window._auditSummary || {};
      const histogram = Array.isArray(summary.histogram) ? summary.histogram.map(Number).filter(Number.isFinite) : [];
      const sparkValues = histogram.length ? histogram : [0, 0];
      const today = Number.isFinite(Number(summary.today)) ? Number(summary.today) : miniFirstNumber(text);
      const sensitive = Math.max(0, Number(summary.sensitive_24h) || 0);
      const last = summary.last || null;
      const lastLabel = last ? `${auditDisplayActor(last)} · ${String(last.action || '—')} · ${relativeTimeShort(last.ts) || '—'}` : 'No audit entries yet';
      const tooltip = auditEntryTooltip(last);
      const sparkTitle = 'Audit entries per hour over the last 24h';
      const spark = renderSparkline(null, sparkValues, {
        width: AUDIT_COLLAPSED_SPARK_WIDTH,
        height: AUDIT_COLLAPSED_SPARK_HEIGHT,
        color: 'var(--yellow)',
        label: 'audit entries per hour',
        title: sparkTitle,
        ariaLabel: sparkTitle,
        svgClass: 'mini-audit-rate audit-entry-rate-spark',
        wrapperClass: 'mini-audit-rate-wrap audit-entry-rate-spark'
      });
      const chipClass = sensitive > 0 ? 'warning' : 'muted';
      const todayLabel = `${formatCount(today)} ${plural(today, 'event')} today`;
      const label = title || text || todayLabel;
      const sensitiveTitle = sensitive + ' sensitive audit actions in the last 24h — show sensitive entries';
      return `<span class="mini-summary-strip audit-collapsed-summary" role="group" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}" data-mini-summary="audit-section">${spark}<span class="audit-last-entry" title="${escapeHtml(tooltip)}">${escapeHtml(lastLabel)}</span><span class="audit-today-count">${escapeHtml(todayLabel)}</span><span class="audit-sensitive-chip ${chipClass}" role="button" tabindex="0" data-action="showAuditSensitiveEntries" data-keydown-action="showAuditSensitiveEntries" data-keys="Enter, " data-stop="1" data-prevent="1" title="${escapeHtml(sensitiveTitle)}" aria-label="Show sensitive entries">${escapeHtml(formatCount(sensitive))} sensitive</span></span>`;
    }
    function miniGauge(value, max, color, label) {
      const v = Math.max(0, Number(value) || 0);
      const m = Math.max(1, Number(max) || v || 1);
      const pct = Math.max(0, Math.min(1, v / m));
      const startX = 8, startY = 21, r = 18;
      const endAngle = Math.PI * (1 - pct);
      const endX = startX + r + Math.cos(endAngle) * r;
      const endY = startY - Math.sin(endAngle) * r;
      const large = pct > 0.5 ? 1 : 0;
      const arc = `M ${startX} ${startY} A ${r} ${r} 0 ${large} 1 ${endX.toFixed(1)} ${endY.toFixed(1)}`;
      const title = label || `${fmtSparkVal(v)} of ${fmtSparkVal(m)}`;
      return `<svg class="mini-gauge" width="52" height="26" viewBox="0 0 52 26" role="img" aria-label="${escapeHtml(title)}">${miniTitleTag(title)}<path class="mini-gauge-track" d="M 8 21 A 18 18 0 0 1 44 21" fill="none" stroke-width="4"/><path class="mini-gauge-arc" d="${arc}" fill="none" stroke="${color || 'var(--cyan)'}" stroke-width="4"/><text x="26" y="20" text-anchor="middle" fill="currentColor" font-size="9" font-weight="700">${escapeHtml(fmtSparkVal(v))}</text></svg>`;
    }
    function miniDial(level, levels, label) {
      const names = Array.isArray(levels) && levels.length ? levels : ['0','1','2','3'];
      const idx = Math.max(0, Math.min(names.length - 1, typeof level === 'number' ? level : names.findIndex(x => String(x).toLowerCase() === String(level).toLowerCase())));
      const segW = 9, gap = 2, h = 24, w = names.length * segW + (names.length - 1) * gap;
      const rects = names.map((name, i) => `<rect class="mini-dial-seg${i <= idx ? ' active' : ''}" x="${i * (segW + gap)}" y="6" width="${segW}" height="12" rx="3"><title>${escapeHtml(name)}</title></rect>`).join('');
      const title = label || `${names[idx] || level}`;
      return `<svg class="mini-dial" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" role="img" aria-label="${escapeHtml(title)}">${miniTitleTag(title)}${rects}</svg>`;
    }
    function miniBar(parts, label) {
      const clean = (Array.isArray(parts) ? parts : []).map(p => ({ value: Math.max(0, Number(p && p.value) || 0), color: (p && p.color) || 'var(--muted)', label: (p && p.label) || '' }));
      const total = clean.reduce((n, p) => n + p.value, 0) || 1;
      let x = 1;
      const rects = clean.map(p => {
        const w = Math.max(p.value > 0 ? 2 : 0, (p.value / total) * 98);
        const out = `<rect x="${x.toFixed(1)}" y="7" width="${w.toFixed(1)}" height="10" rx="2" fill="${p.color}"><title>${escapeHtml(p.label)}: ${fmtSparkVal(p.value)}</title></rect>`;
        x += w;
        return out;
      }).join('');
      const title = label || clean.map(p => `${p.label || 'part'} ${fmtSparkVal(p.value)}`).join(', ');
      return `<svg class="mini-bar" width="104" height="24" viewBox="0 0 104 24" role="img" aria-label="${escapeHtml(title)}">${miniTitleTag(title)}<rect class="mini-bar-bg" x="1" y="7" width="98" height="10" rx="3"/>${rects}</svg>`;
    }
    function miniDelta(value, prev, label) {
      const v = Number(value) || 0;
      const p = Number(prev);
      const known = Number.isFinite(p);
      const up = known && v > p;
      const down = known && v < p;
      const cls = up ? 'good' : down ? 'bad' : 'flat';
      const arrow = up ? '▲' : down ? '▼' : '•';
      const title = label || (known ? `${fmtSparkVal(v)} from ${fmtSparkVal(p)}` : fmtSparkVal(v));
      return `<svg class="mini-delta ${cls}" width="58" height="24" viewBox="0 0 58 24" role="img" aria-label="${escapeHtml(title)}">${miniTitleTag(title)}<text x="2" y="16" fill="currentColor" font-size="11" font-weight="800">${arrow}</text><text x="18" y="16" fill="currentColor" font-size="11" font-weight="800">${escapeHtml(fmtSparkVal(v))}</text></svg>`;
    }
    function miniText(value, label) {
      const text = textOrDash(value);
      const title = label || text;
      const width = Math.max(74, Math.min(160, text.length * 7 + 8));
      return `<svg class="mini-text" width="${width}" height="24" viewBox="0 0 ${width} 24" role="img" aria-label="${escapeHtml(title)}">${miniTitleTag(title)}<text x="${width - 2}" y="16" text-anchor="end" fill="currentColor" font-size="11" font-weight="800">${escapeHtml(text)}</text></svg>`;
    }
    function reviewQueueHistoryEntries() {
      const cutoff = Date.now() - (REVIEW_QUEUE_WINDOWS[_reviewQueueWindow] || REVIEW_QUEUE_WINDOWS['24h']);
      const rows = Array.isArray(window._reviewQueueHistory) ? window._reviewQueueHistory : [];
      return rows.filter(row => {
        const t = Number(row && row.t);
        return Number.isFinite(t) && t >= cutoff;
      });
    }
    function reviewQueueSeries(entries, key) {
      return (entries || []).map(row => Number(row && row[key]) || 0);
    }
    function reviewQueueSparkPoints(values, min, max) {
      const series = (Array.isArray(values) ? values : []).map(v => Number(v) || 0);
      const range = (max - min) || max || 1;
      const plotH = REVIEW_QUEUE_SPARK_H - REVIEW_QUEUE_SPARK_PAD_Y * 2;
      if (series.length <= 1) {
        const y = REVIEW_QUEUE_SPARK_PAD_Y + plotH - (((series[0] || 0) - min) / range) * plotH;
        return `0,${y.toFixed(1)} ${REVIEW_QUEUE_SPARK_W.toFixed(1)},${y.toFixed(1)}`;
      }
      return series.map((v, i) => {
        const x = (i / (series.length - 1)) * REVIEW_QUEUE_SPARK_W;
        const y = REVIEW_QUEUE_SPARK_PAD_Y + plotH - ((v - min) / range) * plotH;
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      }).join(' ');
    }
    function reviewQueueRangeControls() {
      return `<span class="overview-kpi-range review-queue-collapsed-range" aria-label="Review Queue history range">${Object.keys(REVIEW_QUEUE_WINDOWS).map(win => `<button type="button" class="overview-kpi-range-btn${win === _reviewQueueWindow ? ' active' : ''}" data-action="setReviewQueueWindow" data-arg0="${escapeHtml(win)}" data-stop="1" aria-pressed="${win === _reviewQueueWindow ? 'true' : 'false'}" title="Show ${escapeHtml(win)} review queue history">${escapeHtml(win)}</button>`).join('')}</span>`;
    }
    function plural(n, singular, pluralForm) {
      return Number(n) === 1 ? singular : (pluralForm || singular + 's');
    }
    function formatCount(n) {
      const value = Number(n) || 0;
      return formatNumber(value);
    }
    function reviewQueueSparkline(entries) {
      const total = reviewQueueSeries(entries, 'total');
      const high = reviewQueueSeries(entries, 'high');
      const normal = reviewQueueSeries(entries, 'normal');
      const low = reviewQueueSeries(entries, 'low');
      const values = total.length ? total : [0, 0];
      const max = Math.max(1, ...values, ...high, ...normal, ...low);
      const latest = values[values.length - 1] || 0;
      const min = Math.min(0, ...values);
      const title = `min ${fmtSparkVal(min)} · max ${fmtSparkVal(max)} · latest ${fmtSparkVal(latest)}`;
      const after = [
        ['high', high, 'High priority'],
        ['normal', normal, 'Normal priority'],
        ['low', low, 'Low priority'],
      ].map(([key, series, label]) => series.length ? `<polyline class="review-queue-priority-line ${key}" points="${reviewQueueSparkPoints(series, min, max)}"><title>${escapeHtml(label)}: latest ${fmtSparkVal(series[series.length - 1] || 0)}</title></polyline>` : '').join('');
      return `<span class="review-queue-spark">${renderSparkline(null, values, {
        width: REVIEW_QUEUE_SPARK_W, height: REVIEW_QUEUE_SPARK_H, min, max, color: 'color-mix(in srgb, var(--purple) 60%, transparent)',
        title, ariaLabel: `Review Queue total: ${title}`, label: 'review queue total', id: 'review-queue-collapsed',
        dot: false, strokeWidth: 1, after, svgClass: 'mini-spark', wrapperClass: 'sparkline--flush', wrapperTitle: title,
      })}</span>`;
    }
    function reviewQueueCollapsedSummary(text) {
      const entries = reviewQueueHistoryEntries();
      const latest = entries[entries.length - 1] || {};
      const total = Number(latest.total);
      const shownTotal = Number.isFinite(total) ? total : miniFirstNumber(text);
      const hasPrioritySplit = Number.isFinite(Number(latest.high)) || Number.isFinite(Number(latest.normal)) || Number.isFinite(Number(latest.low));
      const high = Number(latest.high) || 0;
      const normal = hasPrioritySplit ? (Number(latest.normal) || 0) : shownTotal;
      const low = Number(latest.low) || 0;
      const first = entries[0] || {};
      const startTotal = Number(first.total);
      const delta = Number.isFinite(startTotal) ? shownTotal - startTotal : 0;
      const deltaCls = delta > 0 ? 'up' : delta < 0 ? 'down' : 'flat';
      const deltaText = delta > 0 ? `▲${delta}` : delta < 0 ? `▼${Math.abs(delta)}` : '•0';
      const chips = [
        ['high', 'H', high],
        ['normal', 'N', normal],
        ['low', 'L', low],
      ].map(([key, label, value]) => `<span class="review-queue-chip ${key}" title="${escapeHtml(label)} priority PRs">${escapeHtml(label)} ${fmtSparkVal(value)}</span>`).join('');
      const label = `${fmtSparkVal(shownTotal)} PRs · high ${high}, normal ${normal}, low ${low}, ${deltaText} vs range start`;
      const sparkEntries = entries.length ? entries : [{ t: Date.now(), total: shownTotal, high, normal, low }];
      const inner = `${reviewQueueRangeControls()}<span class="review-queue-collapsed-main"><span class="review-queue-collapsed-total">${fmtSparkVal(shownTotal)}</span>${chips}<span class="review-queue-delta ${deltaCls}" title="Delta vs range start">${deltaText}</span></span>${reviewQueueSparkline(sparkEntries)}`;
      return `<span class="mini-summary-strip review-queue-collapsed" role="img" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}" data-mini-summary="review-queue-section">${inner}</span>`;
    }
    function setReviewQueueWindow(win) {
      if (!Object.prototype.hasOwnProperty.call(REVIEW_QUEUE_WINDOWS, win)) return;
      _reviewQueueWindow = win;
      try { localStorage.setItem(REVIEW_QUEUE_WINDOW_KEY, win); } catch {}
      updateReviewSectionSummary();
    }
    window.setReviewQueueWindow = setReviewQueueWindow;
    function miniAvatarDots(contributors, total, label) {
      const list = Array.isArray(contributors) ? contributors.slice(0, 5) : [];
      const dots = list.map((c, i) => `<circle cx="${8 + i * 12}" cy="12" r="5" fill="${_getAgentColor((c && (c.github_username || c.login || c.name)) || String(i))}"><title>${escapeHtml((c && (c.github_username || c.login || c.name)) || 'contributor')}</title></circle>`).join('');
      const n = Number(total) || list.length || 0;
      const title = label || `${n} contributors`;
      return `<svg class="mini-avatars" width="92" height="24" viewBox="0 0 92 24" role="img" aria-label="${escapeHtml(title)}">${miniTitleTag(title)}${dots}<text x="90" y="16" text-anchor="end" fill="currentColor" font-size="10" font-weight="800">${escapeHtml(fmtSparkVal(n))}</text></svg>`;
    }
    function adviceSignalNumber(rec, names) {
      const wanted = new Set((Array.isArray(names) ? names : [names]).map(String));
      const signals = Array.isArray(rec && rec.signals) ? rec.signals : [];
      for (const sig of signals) {
        if (!sig || !wanted.has(String(sig.name || ''))) continue;
        const match = String(sig.value == null ? '' : sig.value).match(/\d+/);
        if (match) return Number(match[0]) || 0;
      }
      return 0;
    }
    function advisoryCollapsedImpact(rec) {
      const id = String((rec && rec.id) || '');
      const items = Array.isArray(rec && rec.items) ? rec.items.length : 0;
      const first = (names) => adviceSignalNumber(rec, names) || items || 1;
      if (id === 'fix-failing-check') return { n: first('failing_prs'), label: `−${first('failing_prs')} CI` };
      if (id === 'throttle-lane') return { n: first('blocked_from_lane'), label: `−${first('blocked_from_lane')} lane` };
      if (id === 'clear-human-gate-prs') return { n: first('needs_human'), label: `−${first('needs_human')} waits` };
      if (id === 'unblock-human-issues') return { n: first('needs_human'), label: `−${first('needs_human')} holds` };
      if (id === 'review-stale-prs') return { n: first('stale_blocked'), label: `−${first('stale_blocked')} stale` };
      if (id === 'confirm-and-close') return { n: first('agent_done'), label: `−${first('agent_done')} queue` };
      if (id === 'enable-disabled-agent') {
        const n = first('disabled_agents');
        return { n, label: `+${n} ${plural(n, 'lane')}` };
      }
      if (id === 'add-agent-cadence') {
        const n = first('no_cadence_agents');
        return { n, label: `+${n} ${plural(n, 'kick')}` };
      }
      if (id === 'fix-merge-blockers') return { n: first('open_holds'), label: `−${first('open_holds')} holds` };
      if (id === 'reduce-blocked-prs') return { n: first('blocked'), label: `−${first('blocked')} queue` };
      return { n: items || adviceSignalNumber(rec, ['governor_prs', 'governor_issues']) || 1, label: `−${items || adviceSignalNumber(rec, ['governor_prs', 'governor_issues']) || 1} queue` };
    }
    function advisoryCollapsedHeadline(rec, impact) {
      const id = String((rec && rec.id) || '');
      const title = String((rec && rec.title) || 'Act on advice').replace(/`/g, '');
      const n = Math.max(1, Number(impact && impact.n) || 1);
      const byId = {
        'reduce-blocked-prs': `Fix ${n} blocked PRs`,
        'fix-failing-check': title.replace(/^Fix check\s+/i, 'Fix '),
        'throttle-lane': title.replace(/^Throttle lane\s+/i, 'Throttle '),
        'clear-human-gate-prs': `Review ${n} waiting PRs`,
        'unblock-human-issues': `Decide ${n} held issues`,
        'review-stale-prs': `Review ${n} stale PRs`,
        'confirm-and-close': `Close ${n} done issues`,
        'enable-disabled-agent': `Enable ${n} disabled agent${n === 1 ? '' : 's'}`,
        'add-agent-cadence': `Set cadence on ${n} ${plural(n, 'agent')}`,
        'fix-merge-blockers': `Drain ${n} held PRs`,
        'throttle-pr-producing-lanes': 'Throttle PR-producing lanes',
        'raise-budget-if-starving': 'Raise agent budget',
        'watch-budget-before-cadence': 'Hold cadence changes',
        'rebalance-merge-side': 'Prioritize review lanes',
      };
      const raw = byId[id] || title;
      return raw.length <= 28 ? raw : raw.slice(0, 27).trimEnd() + '…';
    }
    function advisoryCollapsedRecommendations() {
      const source = (typeof _lastHiveAdvice !== 'undefined' && _lastHiveAdvice) || ((window._lastStatus || {}).hiveAdvice) || {};
      const recs = Array.isArray(source.recommendations) ? source.recommendations : [];
      const actionable = recs.filter(r => r && !r.cleared && r.id !== 'observe-more-signals' && r.id !== 'widen-hive-repos' && r.id !== 'raise-acmm-level' && r.id !== 'document-agents-conventions');
      return actionable.map((rec, index) => ({ rec, sourceIndex: recs.indexOf(rec), impact: advisoryCollapsedImpact(rec), score: Number(rec.score) || 0 }))
        .sort((a, b) => (b.impact.n - a.impact.n) || (b.score - a.score) || (a.sourceIndex - b.sourceIndex))
        .slice(0, 3);
    }
    function advisoryCollapsedAdviceHtml(text) {
      const picks = advisoryCollapsedRecommendations();
      if (!picks.length) {
        return `<span class="advisory-collapsed-advice" role="group" aria-label="This week: nothing to do — queue healthy" title="${escapeHtml(text || 'Nothing to do — queue healthy')}"><button type="button" class="advice-collapsed-pill muted" data-action="openAdvisoryAdvicePill" data-arg0="" data-stop="1"><span class="advice-pill-headline">Nothing to do — queue healthy</span></button></span>`;
      }
      const eyebrow = picks.length > 1 ? '<span class="advice-collapsed-eyebrow">This week</span>' : '';
      const pills = picks.map(p => {
        const headline = advisoryCollapsedHeadline(p.rec, p.impact);
        const full = [p.rec.title, p.rec.rationale].filter(Boolean).join('\n');
        return `<button type="button" class="advice-collapsed-pill" data-action="openAdvisoryAdvicePill" data-arg0="${escapeHtml(String(p.sourceIndex))}" data-stop="1" title="${escapeHtml(full || headline)}" aria-label="${escapeHtml(full || headline)}"><span class="advice-pill-headline">${escapeHtml(headline)}</span><span class="advice-pill-impact">${escapeHtml(p.impact.label)}</span></button>`;
      }).join('');
      return `<span class="advisory-collapsed-advice" role="group" aria-label="${escapeHtml(text || 'This week advice')}">${eyebrow}${pills}</span>`;
    }
    function acmmCollapsedDimensions(data) {
      const rows = Array.isArray(data && data.criteria_results) ? data.criteria_results : [];
      if (rows.length) {
        const byCat = new Map();
        rows.forEach(r => {
          const key = String((r && (r.category || r.Category)) || 'Criteria');
          const cur = byCat.get(key) || { name: key, passed: 0, total: 0 };
          cur.total++;
          if (r && r.passed) cur.passed++;
          byCat.set(key, cur);
        });
        return Array.from(byCat.values()).map(d => ({ name: d.name, passed: d.passed === d.total, detail: `${d.passed}/${d.total}` }));
      }
      const levels = Array.isArray(data && data.levels) ? data.levels : [];
      return levels.map(l => ({ name: `L${l.level} ${l.name || ''}`.trim(), passed: !!l.passed, detail: `${Number(l.matched) || 0}/${Number(l.total) || 0}` }));
    }
    function acmmCollapsedCurrentLevel(text) {
      const statusLevel = Number(window._lastStatus && window._lastStatus.acmmLevel);
      if (Number.isFinite(statusLevel) && statusLevel > 0) return statusLevel;
      const evalLevel = Number(typeof _acmmEvalData !== 'undefined' && _acmmEvalData && _acmmEvalData.operational_level);
      if (Number.isFinite(evalLevel) && evalLevel > 0) return evalLevel;
      const textMatch = String(text || '').match(/L\s*(\d+)/i);
      const textLevel = textMatch ? Number(textMatch[1]) : NaN;
      return Number.isFinite(textLevel) && textLevel > 0 ? textLevel : NaN;
    }
    function acmmCollapsedLevelLabel(level) {
      if (!Number.isFinite(level) || level <= 0) return 'not yet evaluated';
      if (typeof acmmLevelLabel === 'function') return acmmLevelLabel(level).replace(' ', ' · ');
      const levelNames = {1:'Inception',2:'Advisory',3:'Quality-Gated',4:'Security-Aware',5:'Semi-Autonomous',6:'Autonomous'};
      return `L${level}${levelNames[level] ? ' · ' + levelNames[level] : ''}`;
    }
    function acmmCollapsedGapDetail(data, level, dims, total, met) {
      const levels = Array.isArray(data && data.levels) ? data.levels : [];
      if (Number.isFinite(level) && levels.length) {
        const next = levels.find(l => Number(l.level) === level + 1);
        const current = levels.find(l => Number(l.level) === level);
        const target = next || current;
        if (target) {
          const targetTotal = Number(target.total) || 0;
          const targetMet = Number(target.matched) || 0;
          const gaps = Math.max(0, targetTotal - targetMet);
          if (gaps) return {
            detail: next ? `${gaps} gap${gaps === 1 ? '' : 's'} to L${level + 1}` : `${gaps} gap${gaps === 1 ? '' : 's'}`,
            tip: next ? `${gaps} open gap${gaps === 1 ? '' : 's'} toward L${level + 1}` : `${gaps} open gap${gaps === 1 ? '' : 's'} at current level L${level}`
          };
          return { detail: next ? `ready for L${level + 1}` : `${targetMet}/${targetTotal} dims`, tip: next ? `No open gaps toward L${level + 1}` : `Current level L${level} has ${targetMet}/${targetTotal}` };
        }
      }
      const gaps = Math.max(0, total - met);
      return {
        detail: total ? (gaps ? `${gaps} gap${gaps === 1 ? '' : 's'}` : `${met}/${total} dims`) : 'not yet evaluated',
        tip: total ? `${gaps} open gap${gaps === 1 ? '' : 's'} in the current evaluation` : 'ACMM evaluation has not reported criteria yet'
      };
    }
    function acmmCollapsedEvalHtml(text) {
      const data = (typeof _acmmEvalData !== 'undefined' && _acmmEvalData) ? ((typeof acmmGetActiveData === 'function' && acmmGetActiveData()) || _acmmEvalData) : null;
      const level = acmmCollapsedCurrentLevel(text);
      if (!data) {
        const label = acmmCollapsedLevelLabel(level);
        const title = Number.isFinite(level) ? `${label} · not yet evaluated` : 'ACMM not yet evaluated';
        return `<button type="button" class="acmm-collapsed-pill" data-action="ocNavigate" data-arg0="acmm-eval-section" data-stop="1" title="${escapeHtml(title)}" aria-label="${escapeHtml(title)}"><span>${escapeHtml(label)}</span><span class="acmm-pill-detail">not yet evaluated</span></button>`;
      }
      const dims = acmmCollapsedDimensions(data);
      const total = dims.length || Number(data.criteria_total) || 0;
      const met = dims.length ? dims.filter(d => d.passed).length : Number(data.criteria_passed) || 0;
      const gapDetail = acmmCollapsedGapDetail(data, level, dims, total, met);
      const detail = gapDetail.detail;
      const label = acmmCollapsedLevelLabel(level);
      const scope = (typeof _acmmSelectedRepo !== 'undefined' && _acmmSelectedRepo) ? `Showing ${_acmmSelectedRepo}` : 'Aggregate ACMM evaluation';
      const title = [`current level ${label}; ${gapDetail.tip}`, scope, 'Expand ACMM Eval for per-level details.'].join('\n');
      return `<button type="button" class="acmm-collapsed-pill" data-action="ocNavigate" data-arg0="acmm-eval-section" data-stop="1" title="${escapeHtml(title)}" aria-label="${escapeHtml(title)}"><span>${escapeHtml(label)}</span><span class="acmm-pill-detail">${escapeHtml(detail)}</span></button>`;
    }
    function knowledgeCollapsedSummary(text) {
      const history = typeof _factHistory !== 'undefined' && Array.isArray(_factHistory) ? _factHistory : [];
      const samples = history.filter(e => e && Number.isFinite(e.count) && e.count >= 0);
      const label = samples.length
        ? `Fact count · ${sparkSpanLabel(samples.map(e => e.t), samples.length)} · recorded history`
        : 'Fact count · no history yet';
      // Use recorded counts directly: zero is a real count, not a missing sample.
      const spark = samples.length ? renderSparkline(null, samples.map(e => e.count), {
        width: 96, height: 24, color: 'var(--blue)',
        label: 'Fact count', title: label, ariaLabel: label,
        id: 'knowledge:fact-count', svgClass: 'mini-knowledge-facts',
      }) : '';
      return miniWrap('knowledge-section', `${text} · ${label}`, spark + miniText(text, text));
    }
    function visualSectionSummary(sectionId, html, title) {
      const text = textOrDash(html);
      const data = window._lastStatus || {};
      const repos = Array.isArray(data.repos) ? data.repos : [];
      const nums = miniNumericParts(text).map(Number);
      const n0 = nums.length ? nums[0] : 0;
      const hist = (metric, value) => miniHistorySeries(sectionId, metric, value);
      let inner = '';
      if (sectionId === 'overview-section') {
        const issueTotal = repos.reduce((n, r) => n + ((r.actionableIssues || r.issues || []).length || Number(r.openIssues || r.open_issues || r.issues || 0) || 0), 0);
        const prTotal = repos.reduce((n, r) => n + ((r.openPrs || r.prs || []).length || Number(r.open_prs || r.openPRs || r.prs || 0) || 0), 0);
        inner = miniBar([{ label: 'issues', value: issueTotal, color: 'var(--blue)' }, { label: 'open PRs', value: prTotal, color: 'var(--purple)' }], text);
      } else if (sectionId === 'governor') {
        const gov = data.governor || {};
        const mode = String(gov.mode || text.split(/\s+/)[0] || 'idle').toLowerCase();
        const pressure = Number(gov.pressure || gov.total || ((gov.issues || 0) + (gov.prs || 0)) || 0) || 0;
        const max = Number(gov.surgeThreshold || gov.surge_threshold || gov.thresholds?.surge || 10) || 10;
        inner = miniDial(mode, ['idle','quiet','busy','surge'], text) + miniGauge(pressure, max, 'var(--yellow)', text);
      } else if (sectionId === 'advisory-section') {
        return advisoryCollapsedAdviceHtml(title || text);
      } else if (sectionId === 'hive-advice-section') {
        const series = hist('count', n0);
        inner = miniDelta(n0, series.length > 1 ? series[series.length - 2] : n0, text) + miniSpark(series, 'var(--blue)', text);
      } else if (sectionId === 'pr-throughput-section') {
        inner = miniBar([{ label: 'merged', value: nums[0] || 0, color: 'var(--green)' }, { label: 'opened', value: nums[1] || 0, color: 'var(--yellow)' }], text);
      } else if (sectionId === 'token-panel') {
        const sessions = nums.length > 1 ? nums[1] : Number(data.tokens?.totals?.sessions || 0);
        inner = miniText(text, text) + miniDelta(sessions, hist('sessions', sessions).slice(-2)[0], text);
      } else if (sectionId === 'cost-panel') {
        const costSeries = (typeof costHist === 'function' ? costHist() : []).map(e => Number(e.usd || e.total_usd) || 0);
        inner = miniSpark(costSeries.length ? costSeries : hist('usd', n0), 'var(--green)', text) + miniText(text, text);
      } else if (sectionId === 'repos-section') {
        const open = nums[1] || 0, issues = nums[2] || 0;
        const needsHuman = Number((document.getElementById('repos-needs-human') || {}).textContent) || 0;
        inner = typeof reposCollapsedTilesHtml === 'function' ? reposCollapsedTilesHtml(repos, data) : miniBar([{ label: 'eligible', value: Math.max(0, open - needsHuman), color: 'var(--green)' }, { label: 'blocked', value: needsHuman, color: 'var(--red)' }, { label: 'issues', value: issues, color: 'var(--blue)' }], text);
      } else if (sectionId === 'audit-section') {
        return renderAuditCollapsedSummary(text, title);
      } else if (sectionId === 'knowledge-section') {
        return knowledgeCollapsedSummary(text);
      } else if (sectionId === 'acmm-eval-section') {
        return acmmCollapsedEvalHtml(title || text);
      } else if (sectionId === 'acmm-reco-section') {
        inner = miniDial(n0, ['L0','L1','L2','L3','L4','L5','L6'], text);
      } else if (sectionId === 'review-queue-section') {
        return reviewQueueCollapsedSummary(text);
      } else if (sectionId === 'inception-section') {
        inner = miniBar([{ label: 'proposals', value: n0, color: 'var(--blue)' }, { label: 'in progress', value: nums[1] || 0, color: 'var(--green)' }], text);
      } else if (sectionId === 'contributors-section') {
        inner = miniAvatarDots((typeof _cachedContributors === 'undefined' ? [] : _cachedContributors), n0, text);
      } else if (sectionId === 'debug-section') {
        return (typeof debugQuotaWidgetHtml === 'function' && debugQuotaWidgetHtml(data)) || miniWrap(sectionId, title || text, miniText(text, text));
      } else if (sectionId === 'faq-section') {
        return '';
      } else if (sectionId === 'agents-section') {
        const agents = data.agents || window._lastAgents || [];
        inner = typeof agentsCollapsedTilesHtml === 'function' ? agentsCollapsedTilesHtml(agents, Date.now(), data.governor) : miniBar([{ label: 'agents', value: Array.isArray(agents) ? agents.length : n0, color: 'var(--green)' }], text);
      } else if (sectionId === 'lifecycle-section') {
        inner = miniBar([{ label: 'in flight', value: nums[0] || 0, color: 'var(--blue)' }, { label: 'merged', value: nums[1] || 0, color: 'var(--green)' }, { label: 'blocked', value: nums[2] || 0, color: 'var(--red)' }], text);
      } else {
        const series = hist('headline', n0);
        inner = miniDelta(n0, series.length > 1 ? series[series.length - 2] : n0, text);
      }
      if ((sectionId === 'agents-section' || sectionId === 'repos-section') && inner) return inner;
      return miniWrap(sectionId, title || text, inner || miniDelta(n0, n0, text));
    }

    function setSectionSummary(sectionId, html, title) {
      const el = document.getElementById(sectionId + '-summary');
      if (!el) return;
      const text = textOrDash(html);
      el.innerHTML = visualSectionSummary(sectionId, text, title);
      const label = title || text;
      el.title = label;
      el.setAttribute('aria-label', label);
    }
    function textOrDash(value) {
      const s = String(value == null ? '' : value).trim();
      return s || '—';
    }
    function relativeTimeShort(ts) {
      const t = new Date(ts || '').getTime();
      if (!Number.isFinite(t)) return '';
      const mins = Math.max(1, Math.floor((Date.now() - t) / 60000));
      if (mins < 60) return mins + 'm ago';
      const hrs = Math.floor(mins / 60);
      if (hrs < 48) return hrs + 'h ago';
      return Math.floor(hrs / 24) + 'd ago';
    }
    function agentsSectionSummary(agents) {
      agents = Array.isArray(agents) ? agents : [];
      const disabled = agents.filter(agentIsDisabled).length;
      const activeAgents = agents.filter(a => a && !agentIsDisabled(a));
      const off = activeAgents.filter(a => a && (a.paused || a.stopped || a.offByCadence || a.unscheduledInMode || a.state === 'stopped' || a.state === 'failed')).length;
      const on = Math.max(0, activeAgents.length - off);
      return `${agents.length} agents · ${on} on · ${off} off${disabled > 0 ? ` · ${disabled} disabled` : ''}`;
    }
    let _lastHiveAdvice = null;
    function agentCollapsedParseNextKick(value, nowMs) {
      const raw = String(value == null ? '' : value).trim();
      if (!raw) return null;
      const lower = raw.toLowerCase();
      if (lower === '—' || lower === '-' || lower === 'continuous' || lower === 'paused' || lower === 'off' || lower === 'on demand' || lower === 'no cadence') return null;
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      if (/^(due|now|overdue)/.test(lower)) return now;
      const rel = lower.match(/^(?:in\s+)?(?:(\d+)\s*(?:h|hr|hrs|hour|hours)(?:\s+(\d+)\s*(?:m|min|mins|minute|minutes))?|(\d+)\s*(?:m|min|mins|minute|minutes)|(\d+)\s*(?:s|sec|secs|second|seconds))$/);
      if (rel) {
        const hours = parseInt(rel[1] || '0', 10);
        const mins = parseInt(rel[2] || rel[3] || '0', 10);
        const secs = parseInt(rel[4] || '0', 10);
        return now + hours * 3600000 + mins * 60000 + secs * 1000;
      }
      const monthDay = raw.match(/^\d{1,2}\/\d{1,2}\s+/);
      if (monthDay) {
        const year = new Date(now).getFullYear();
        let stamped = Date.parse(`${year}/${raw}`);
        if (Number.isFinite(stamped) && stamped < now - 86400000) stamped = Date.parse(`${year + 1}/${raw}`);
        if (Number.isFinite(stamped)) return stamped;
      }
      const direct = Date.parse(raw);
      if (Number.isFinite(direct)) return direct;
      const clock = raw.match(/^(\d{1,2}):(\d{2})(?:\s*([ap])\.?m\.?)?$/i);
      if (clock) {
        const d = new Date(now);
        let h = parseInt(clock[1], 10);
        const m = parseInt(clock[2], 10);
        const meridiem = clock[3] && clock[3].toLowerCase();
        if (meridiem === 'p' && h < 12) h += 12;
        if (meridiem === 'a' && h === 12) h = 0;
        d.setHours(h, m, 0, 0);
        if (d.getTime() < now - 60000) d.setDate(d.getDate() + 1);
        return d.getTime();
      }
      return null;
    }
    const AGENTS_COLLAPSED_COUNTDOWN_TICK_MS = 30000;
    const AGENT_NAVBAR_EXIT_MS = 180;
    const AGENT_NAVBAR_PULSE_MS = 700;
    function formatKickCountdown(ms) {
      if (ms == null) return '—';
      const delta = Number(ms);
      if (!Number.isFinite(delta)) return '—';
      if (delta <= 0) return 'now';
      return `in ${Math.max(1, Math.ceil(delta / 60000))}m`;
    }
    function agentCollapsedNextKickMs(a, nowMs) {
      if (!a || agentIsDisabled(a) || a.paused === true || a.offByCadence === true || a.unscheduledInMode === true || a.noCadence === true || a.onDemand === true) return null;
      return agentCollapsedParseNextKick(a.nextKickAt || a.nextKickIn || a.nextKick, nowMs);
    }
    function agentKickDisplay(value, machineValue) {
      return machineValue ? formatDashboardDateTime(machineValue, { month:'numeric', day:'numeric', hour:'numeric', minute:'2-digit', hour12:true, timeZoneName:'short' }, value || '—') : (value || '—');
    }
    function agentCollapsedNextLabel(a, nowMs) {
      if (!a || agentIsDisabled(a)) return '—';
      if (agentTileIsRunning(a)) return 'now';
      const ms = agentCollapsedNextKickMs(a, nowMs);
      if (!Number.isFinite(ms)) return '—';
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      return formatKickCountdown(ms - now);
    }
    function agentTileIsRunning(a) {
      if (!a || agentIsDisabled(a)) return false;
      if (a.running === true) return true;
      const busy = String(a.busy || a.activity || '').toLowerCase();
      if (busy === 'working' || busy === 'running') return true;
      const state = String(a.state || '').toLowerCase();
      return state === 'working';
    }
    function agentTileRunningLabel(a, nowMs) {
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      const started = Date.parse((a && (a.busySince || a.runningSince || a.startedAt)) || '');
      if (!Number.isFinite(started) || started > now) return 'running';
      const mins = Math.max(1, Math.floor((now - started) / 60000));
      if (mins < 60) return `running ${mins}m`;
      const hrs = Math.floor(mins / 60);
      const rem = mins % 60;
      return rem ? `running ${hrs}h ${rem}m` : `running ${hrs}h`;
    }
    function agentTileCadenceLabel(a, nowMs) {
      const nextMs = agentCollapsedNextKickMs(a, nowMs);
      if (!Number.isFinite(nextMs)) return '';
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      if (nextMs <= now) return '';
      return formatKickCountdown(nextMs - now);
    }
    function agentTileGovernorQueue(governor) {
      const gov = governor || (typeof window !== 'undefined' && window._lastStatus && window._lastStatus.governor) || {};
      const raw = Array.isArray(gov.nextAgents) ? gov.nextAgents : (Array.isArray(gov.pendingKicks) ? gov.pendingKicks : []);
      return raw.map(name => String(name || '').trim()).filter(Boolean);
    }
    function agentCollapsedSortKey(a, index, nowMs) {
      const disabled = !a || agentIsDisabled(a);
      const active = agentTileIsRunning(a);
      const next = !disabled ? agentCollapsedNextKickMs(a, nowMs) : null;
      const continuous = !!a && !disabled && a.continuous === true;
      let group = 4;
      if (active) group = 0;
      else if (Number.isFinite(next)) group = 1;
      else if (continuous) group = 2;
      else if (!disabled) group = 3;
      return { group, next: Number.isFinite(next) ? next : Number.POSITIVE_INFINITY, index };
    }
    function agentTileStates(agents, nowMs, governor) {
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      const queued = agentTileGovernorQueue(governor);
      const rows = (Array.isArray(agents) ? agents : []).map((agent, index) => {
        const name = (agent && (agent.name || agent.displayName)) || `agent-${index}`;
        const disabled = !agent || agentIsDisabled(agent);
        const running = agentTileIsRunning(agent);
        const nextMs = !disabled ? agentCollapsedNextKickMs(agent, now) : null;
        const continuous = !!agent && !disabled && agent.continuous === true;
        const queuedIndex = queued.indexOf(String(name));
        const canBeNext = queuedIndex >= 0 && !running && !disabled;
        let group = 4;
        if (running) group = 0;
        else if (queuedIndex >= 0) group = 1;
        else if (Number.isFinite(nextMs)) group = 2;
        else if (continuous) group = 2;
        else if (!disabled) group = 3;
        return {
          agent,
          id: String(name),
          label: running ? 'running' : null,
          countdown: running ? agentTileRunningLabel(agent, now) : agentTileCadenceLabel(agent, now),
          nextMs: Number.isFinite(nextMs) ? nextMs : null,
          running,
          canBeNext,
          queuedIndex,
          sortKey: { group, next: queuedIndex >= 0 ? queuedIndex : (Number.isFinite(nextMs) ? nextMs : Number.POSITIVE_INFINITY), index }
        };
      }).sort((a, b) => (a.sortKey.group - b.sortKey.group) || (a.sortKey.next - b.sortKey.next) || (a.sortKey.index - b.sortKey.index));
      const nextName = queued.find(name => {
        const row = rows.find(item => item.id === name);
        return row && row.canBeNext;
      });
      if (nextName) {
        const next = rows.find(row => row.id === nextName);
        if (next) {
          next.label = 'next';
          if (!next.countdown) next.countdown = Number.isFinite(next.nextMs) && next.nextMs > now ? formatKickCountdown(next.nextMs - now) : 'queued';
        }
      }
      return rows;
    }
    function sortAgentsForCollapsedTiles(agents, nowMs) {
      return agentTileStates(agents, nowMs).map(item => item.agent);
    }
    function agentCollapsedAuthInfo(a, isDown) {
      const text = String((a && (a.backendAuthStatus || a.backendAuthLastError || a.statusEvidence)) || '').toLowerCase();
      if (text.includes('expired') || text.includes('token_expired')) return { label: 'expired', status: 'error', needsLogin: true };
      if (_agentNeedsLogin(a || {}, isDown)) return { label: 'logged out', status: 'error', needsLogin: true };
      return { label: 'authenticated', status: 'ok', needsLogin: false };
    }
    function agentCollapsedTimestampDetail(ms, fallback) {
      if (Number.isFinite(ms)) {
        const d = new Date(ms);
        return `${d.toISOString()} (${formatDashboardDateTime(d)})`;
      }
      const direct = Date.parse(fallback || '');
      if (Number.isFinite(direct)) {
        const d = new Date(direct);
        return `${d.toISOString()} (${formatDashboardDateTime(d)})`;
      }
      return String(fallback || '—');
    }
    function agentCollapsedTileTitle(a, name, power, auth, nextMs) {
      const lastMs = Date.parse((a && a.lastKickAt) || '');
      return [
        name,
        `power: ${power}`,
        `login: ${auth.label}`,
        `next kick: ${agentCollapsedTimestampDetail(nextMs, a && (a.nextKickAt || a.nextKick))}`,
        `last kick: ${agentCollapsedTimestampDetail(Number.isFinite(lastMs) ? lastMs : null, a && a.lastKick)}`,
      ].join('\n');
    }
    function agentCollapsedStateLabel(disabled, isDown, auth) {
      if (disabled) return 'disabled';
      if (auth && auth.needsLogin) return auth.label;
      if (isDown) return 'stopped';
      return '';
    }
    function agentTileLabelHtml(label) {
      if (label === 'running') return '<span class="agent-up-next running-now">running now</span>';
      if (label === 'next') return '<span class="agent-up-next">up next</span>';
      return '<span class="agent-up-next" aria-hidden="true"></span>';
    }
    function setAgentTileLabel(tile, label) {
      if (!tile) return;
      tile.classList.toggle('running-now', label === 'running');
      tile.classList.toggle('up-next', label === 'next');
      tile.setAttribute('data-agent-tile-label', label || '');
      const badge = tile.querySelector('.agent-up-next');
      if (badge) {
        badge.classList.toggle('running-now', label === 'running');
        badge.textContent = label === 'running' ? 'running now' : (label === 'next' ? 'up next' : '');
        badge.setAttribute('aria-hidden', label ? 'false' : 'true');
      }
    }
    function agentCollapsedTileHtml(a, nowMs, tileState) {
      tileState = tileState || {};
      const name = (a && (a.displayName || a.name)) || 'agent';
      const key = (a && a.name) || name;
      const disabled = agentIsDisabled(a);
      const isDown = a && a.state !== 'running';
      const active = tileState.running === true || agentTileIsRunning(a);
      const auth = agentCollapsedAuthInfo(a || {}, isDown);
      const power = disabled ? 'disabled' : (isDown ? 'stopped' : 'running');
      const nextMs = Number.isFinite(tileState.nextMs) ? tileState.nextMs : agentCollapsedNextKickMs(a, nowMs);
      const countdown = tileState.countdown || (active ? agentTileRunningLabel(a, nowMs) : agentTileCadenceLabel(a, nowMs));
      const state = agentCollapsedStateLabel(disabled, isDown, auth);
      const sortKey = tileState.sortKey || agentCollapsedSortKey(a, 0, nowMs);
      const label = tileState.label || null;
      const classes = ['agent-collapsed-tile', active ? 'active' : '', label === 'running' ? 'running-now' : '', disabled ? 'disabled' : '', isDown && !disabled ? 'stopped' : '', auth.needsLogin ? 'needs-login' : '', label === 'next' ? 'up-next' : ''].filter(Boolean).join(' ');
      const title = agentCollapsedTileTitle(a || {}, name, power, auth, nextMs);
      const labelHtml = agentTileLabelHtml(label);
      const stateHtml = state ? `<span class="agent-tile-state"> · ${esc(state)}</span>` : '<span class="agent-tile-state"></span>';
      return `<button type="button" class="${classes}" data-action="openAgentCollapsedTile" data-agent="${esc(key)}" data-agent-order-key="${esc(key)}" data-agent-next-kick-ms="${Number.isFinite(nextMs) ? String(nextMs) : ''}" data-agent-sort-group="${sortKey.group}" data-agent-queue-index="${tileState.queuedIndex >= 0 ? String(tileState.queuedIndex) : ''}" data-agent-running="${active ? '1' : ''}" data-agent-running-since="${esc((a && (a.busySince || a.runningSince || a.startedAt)) || '')}" data-agent-can-up-next="${tileState.canBeNext ? '1' : ''}" data-agent-tile-label="${esc(label || '')}" data-agent-active="${active ? '1' : ''}" data-stop="1" title="${esc(title)}" aria-label="${esc(title)}"><span class="agent-tile-name">${esc(name)}</span>${labelHtml}<span class="agent-tile-dot" data-status="${auth.status}" aria-hidden="true"></span><span class="agent-tile-next"><span class="agent-tile-countdown">${esc(countdown)}</span>${stateHtml}</span></button>`;
    }
    function refreshAgentsCollapsedCountdowns(nowMs) {
      const strip = document.querySelector('.agents-collapsed-tiles');
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      refreshAgentNavbarUpNextCountdowns(now);
      if (!strip) return;
      const tiles = Array.from(strip.querySelectorAll('.agent-collapsed-tile'));
      tiles.forEach(tile => {
        const rawNext = String(tile.getAttribute('data-agent-next-kick-ms') || '').trim();
        const nextMs = rawNext ? Number(rawNext) : NaN;
        let countdown = '';
        if (tile.getAttribute('data-agent-running') === '1') {
          countdown = agentTileRunningLabel({ busySince: tile.getAttribute('data-agent-running-since') || '' }, now);
        } else if (Number.isFinite(nextMs) && nextMs > now) {
          countdown = formatKickCountdown(nextMs - now);
        } else if (tile.getAttribute('data-agent-tile-label') === 'next') {
          countdown = 'queued';
        }
        const el = tile.querySelector('.agent-tile-countdown');
        if (el && el.textContent !== countdown) el.textContent = countdown;
      });
      const sortedTiles = tiles.sort((a, b) => {
        const ag = Number(a.getAttribute('data-agent-sort-group')) || 0;
        const bg = Number(b.getAttribute('data-agent-sort-group')) || 0;
        const ar = String(a.getAttribute('data-agent-next-kick-ms') || '').trim();
        const br = String(b.getAttribute('data-agent-next-kick-ms') || '').trim();
        const aq = Number(a.getAttribute('data-agent-queue-index'));
        const bq = Number(b.getAttribute('data-agent-queue-index'));
        const an = ar ? Number(ar) : NaN;
        const bn = br ? Number(br) : NaN;
        const aOrder = Number.isFinite(aq) ? aq : (Number.isFinite(an) ? an : Number.POSITIVE_INFINITY);
        const bOrder = Number.isFinite(bq) ? bq : (Number.isFinite(bn) ? bn : Number.POSITIVE_INFINITY);
        return (ag - bg) || (aOrder - bOrder);
      });
      let nextAssigned = false;
      sortedTiles.forEach(tile => {
        strip.appendChild(tile);
        const label = tile.getAttribute('data-agent-running') === '1' ? 'running' : (!nextAssigned && tile.getAttribute('data-agent-can-up-next') === '1' ? 'next' : null);
        if (label === 'next') nextAssigned = true;
        setAgentTileLabel(tile, label);
      });
    }
    function ensureAgentsCollapsedCountdownTicker() {
      if (window._agentsCollapsedCountdownInterval) return;
      window._agentsCollapsedCountdownInterval = setInterval(refreshAgentsCollapsedCountdowns, AGENTS_COLLAPSED_COUNTDOWN_TICK_MS);
    }
    function agentNavbarHasSchedule(a, nowMs) {
      return !!a && !agentIsDisabled(a) && (agentTileIsRunning(a) || Number.isFinite(agentCollapsedNextKickMs(a, nowMs)) || a.continuous === true);
    }
    function agentNavbarTileHtml(a, nowMs, tileState) {
      tileState = tileState || {};
      const name = (a && (a.displayName || a.name)) || 'agent';
      const key = (a && a.name) || name;
      const disabled = agentIsDisabled(a);
      const isDown = a && a.state !== 'running';
      const active = tileState.running === true || agentTileIsRunning(a);
      const auth = agentCollapsedAuthInfo(a || {}, isDown);
      const nextMs = Number.isFinite(tileState.nextMs) ? tileState.nextMs : agentCollapsedNextKickMs(a, nowMs);
      const countdown = tileState.countdown || (active ? agentTileRunningLabel(a, nowMs) : agentTileCadenceLabel(a, nowMs));
      const label = tileState.label || null;
      const classes = ['agent-navbar-tile', active ? 'active' : '', label === 'running' ? 'running-now' : '', isDown && !disabled ? 'stopped' : '', auth.needsLogin ? 'needs-login' : '', label === 'next' ? 'up-next' : ''].filter(Boolean).join(' ');
      const title = `${name}
next kick: ${agentCollapsedTimestampDetail(nextMs, a && (a.nextKickAt || a.nextKick))}`;
      const labelHtml = agentTileLabelHtml(label);
      return `<button type="button" class="${classes}" data-action="openAgentsUpNextPanel" data-agent-key="${esc(key)}" data-agent-next-kick-ms="${Number.isFinite(nextMs) ? String(nextMs) : ''}" data-agent-running="${active ? '1' : ''}" data-agent-running-since="${esc((a && (a.busySince || a.runningSince || a.startedAt)) || '')}" data-agent-can-up-next="${tileState.canBeNext ? '1' : ''}" data-agent-tile-label="${esc(label || '')}" data-agent-active="${active ? '1' : ''}" data-stop="1" title="${esc(title)}" aria-label="${esc(title)}"><span class="agent-tile-name">${esc(name)}</span><span class="agent-tile-dot" data-status="${auth.status}" aria-hidden="true"></span><span class="agent-tile-next"><span class="agent-tile-countdown">${esc(countdown)}</span></span>${labelHtml}</button>`;
    }
    function agentNavbarTileFromHtml(html) {
      const template = document.createElement('template');
      template.innerHTML = html.trim();
      return template.content.firstElementChild;
    }
    function animateAgentNavbarReorder(wrap, beforeRects) {
      if (!wrap || window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
      Array.from(wrap.querySelectorAll('.agent-navbar-tile')).forEach(tile => {
        const key = tile.getAttribute('data-agent-key') || '';
        const before = beforeRects.get(key);
        if (!before || tile.classList.contains('entering')) return;
        const after = tile.getBoundingClientRect();
        const dx = before.left - after.left;
        const dy = before.top - after.top;
        if (!dx && !dy) return;
        tile.style.transform = `translate(${dx}px, ${dy}px)`;
        tile.getBoundingClientRect();
        tile.style.transform = '';
      });
    }
    function renderAgentNavbarTileDiff(wrap, list, now) {
      const beforeRects = new Map();
      Array.from(wrap.querySelectorAll('.agent-navbar-tile')).forEach(tile => {
        const key = tile.getAttribute('data-agent-key') || '';
        if (key) beforeRects.set(key, tile.getBoundingClientRect());
      });
      const desired = new Set(list.map(item => item.id));
      Array.from(wrap.querySelectorAll('.agent-navbar-tile')).forEach(tile => {
        const key = tile.getAttribute('data-agent-key') || '';
        if (desired.has(key)) return;
        tile.classList.add('exiting');
        window.setTimeout(() => tile.remove(), AGENT_NAVBAR_EXIT_MS);
      });
      const previousUpNext = wrap.dataset.upNextKey || '';
      list.forEach((item, i) => {
        const key = item.id;
        const html = agentNavbarTileHtml(item.agent, now, item);
        let tile = Array.from(wrap.querySelectorAll('.agent-navbar-tile')).find(candidate => candidate.getAttribute('data-agent-key') === key);
        if (tile) {
          const nextTile = agentNavbarTileFromHtml(html);
          tile.className = nextTile.className;
          Array.from(nextTile.attributes).forEach(attr => tile.setAttribute(attr.name, attr.value));
          tile.innerHTML = nextTile.innerHTML;
        } else {
          tile = agentNavbarTileFromHtml(html);
          tile.classList.add('entering');
        }
        const currentAtIndex = wrap.children[i];
        if (currentAtIndex !== tile) wrap.insertBefore(tile, currentAtIndex || null);
        if (item.label === 'next' && previousUpNext && previousUpNext !== key) {
          tile.classList.add('up-next-pulse');
          window.setTimeout(() => tile.classList.remove('up-next-pulse'), AGENT_NAVBAR_PULSE_MS);
        }
        setAgentTileLabel(tile, item.label);
        if (tile.classList.contains('entering')) {
          requestAnimationFrame(() => tile.classList.remove('entering'));
        }
      });
      const nextItem = list.find(item => item.label === 'next');
      wrap.dataset.upNextKey = nextItem ? nextItem.id : '';
      animateAgentNavbarReorder(wrap, beforeRects);
    }
    function renderAgentNavbarUpNext(agents, nowMs) {
      const wrap = document.getElementById('agent-navbar-upnext');
      if (!wrap) return;
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      const list = agentTileStates(agents, now, window._lastStatus && window._lastStatus.governor).filter(item => agentNavbarHasSchedule(item.agent, now)).slice(0, 3);
      wrap.hidden = list.length === 0;
      wrap.dataset.agentSummary = list.length ? String(list.length) + ' agents ▾' : '';
      renderAgentNavbarTileDiff(wrap, list, now);
      if (list.length) ensureAgentsCollapsedCountdownTicker();
    }
    function refreshAgentNavbarUpNextCountdowns(nowMs) {
      const wrap = document.getElementById('agent-navbar-upnext');
      if (!wrap || wrap.hidden) return;
      const now = Number.isFinite(nowMs) ? nowMs : Date.now();
      Array.from(wrap.querySelectorAll('.agent-navbar-tile')).forEach(tile => {
        const rawNext = String(tile.getAttribute('data-agent-next-kick-ms') || '').trim();
        const nextMs = rawNext ? Number(rawNext) : NaN;
        let countdown = '';
        if (tile.getAttribute('data-agent-running') === '1') {
          countdown = agentTileRunningLabel({ busySince: tile.getAttribute('data-agent-running-since') || '' }, now);
        } else if (Number.isFinite(nextMs) && nextMs > now) {
          countdown = formatKickCountdown(nextMs - now);
        } else if (tile.getAttribute('data-agent-tile-label') === 'next') {
          countdown = 'queued';
        }
        const el = tile.querySelector('.agent-tile-countdown');
        if (el && el.textContent !== countdown) el.textContent = countdown;
      });
    }
    function openAgentsUpNextPanel() {
      navPeekRestore('agents-section', { instant: true });
      const expanded = navPeekExpand('agents-section');
      if (!expanded && isSectionCollapsed('agents-section')) toggleSection('agents-section', { collapsed: false, persist: false, navPeek: true });
      ocAfterNavLayoutSettles(['agents-section'], function() { ocScrollSectionIntoView('agents-section'); });
    }
    function agentsCollapsedTilesHtml(agents, nowMs, governor) {
      const list = agentTileStates(agents, nowMs, governor);
      if (!list.length) return '';
      ensureAgentsCollapsedCountdownTicker();
      return `<span class="agents-collapsed-tiles" data-mini-summary="agents-section">${list.map(item => agentCollapsedTileHtml(item.agent, nowMs, item)).join('')}</span>`;
    }
    const REPOS_COLLAPSED_TILE_LIMIT = 6;
    function repoDisplayParts(repo) {
      const full = String((repo && (repo.full || repo.name)) || '').trim();
      const parts = full.split('/');
      if (parts.length > 1) return { full, owner: parts[0] + '/', name: parts.slice(1).join('/') };
      return { full, owner: '', name: full || 'repo' };
    }
    function repoOpenWorkCount(repo) {
      repo = repo || {};
      const issues = Array.isArray(repo.actionableIssues) ? repo.actionableIssues.length : Number(repo.openIssues || repo.open_issues || repo.issues || 0) || 0;
      const prs = (Array.isArray(repo.openPrs) ? repo.openPrs.length : Number(repo.openPRs || repo.open_prs || repo.prs || 0) || 0)
        + (Array.isArray(repo.heldPrs) ? repo.heldPrs.length : 0);
      return issues + prs;
    }
    function repoHeldCount(repo) {
      repo = repo || {};
      const labelBlocked = item => {
        const labels = ((item && item.labels) || []).map(l => String(l).toLowerCase());
        return labels.some(l => l === 'blocked' || l === 'needs-human' || l.indexOf('hold') >= 0 || l.indexOf('pause') >= 0);
      };
      return (Array.isArray(repo.heldIssues) ? repo.heldIssues.length : 0)
        + (Array.isArray(repo.heldPrs) ? repo.heldPrs.length : 0)
        + (Array.isArray(repo.actionableIssues) ? repo.actionableIssues.filter(labelBlocked).length : 0)
        + (Array.isArray(repo.openPrs) ? repo.openPrs.filter(labelBlocked).length : 0);
    }
    function repoHasFailingCI(repo) {
      const prs = (Array.isArray(repo && repo.openPrs) ? repo.openPrs : []).concat(Array.isArray(repo && repo.heldPrs) ? repo.heldPrs : []);
      return prs.some(pr => String((pr && pr.ci_status) || '').toLowerCase() === 'failure' || ((pr && pr.failing_checks) || []).length);
    }
    function repoIsWorking(repo, data) {
      const full = repoDisplayParts(repo).full.toLowerCase();
      const activeSwarmRepo = String((((data || {}).swarm || {}).active || {}).repo || '').toLowerCase();
      if (full && activeSwarmRepo === full) return true;
      const agents = Array.isArray((data || {}).agents) ? data.agents : [];
      return agents.some(a => a && a.busy === 'working' && String(a.repo || a.fullRepo || '').toLowerCase() === full);
    }
    function repoCollapsedStatus(repo, data) {
      if (repo && repo.paused) return 'paused';
      if (repoHasFailingCI(repo)) return 'error';
      if (repoHeldCount(repo) > 0) return 'blocked';
      if (repoIsWorking(repo, data) || repoOpenWorkCount(repo) > 0) return 'healthy';
      return 'idle';
    }
    function repoCollapsedSortKey(repo, data) {
      const working = repoIsWorking(repo, data) ? 0 : 1;
      const paused = repo && repo.paused ? 1 : 0;
      return { working, paused, work: repoOpenWorkCount(repo), name: repoDisplayParts(repo).full.toLowerCase() };
    }
    function sortReposForCollapsedTiles(repos, data) {
      return (Array.isArray(repos) ? repos : []).slice().sort((a, b) => {
        const ak = repoCollapsedSortKey(a, data), bk = repoCollapsedSortKey(b, data);
        return (ak.working - bk.working) || (ak.paused - bk.paused) || (bk.work - ak.work) || ak.name.localeCompare(bk.name);
      });
    }
    function repoCollapsedTileTitle(repo, data) {
      const parts = repoDisplayParts(repo);
      const issueCount = Array.isArray(repo && repo.actionableIssues) ? repo.actionableIssues.length : Number(repo && (repo.openIssues || repo.open_issues || repo.issues)) || 0;
      const prCount = (Array.isArray(repo && repo.openPrs) ? repo.openPrs.length : Number(repo && (repo.openPRs || repo.open_prs || repo.prs)) || 0) + (Array.isArray(repo && repo.heldPrs) ? repo.heldPrs.length : 0);
      const held = repoHeldCount(repo);
      const status = repoCollapsedStatus(repo, data);
      const activeText = repoIsWorking(repo, data) ? 'working/active' : (repo && repo.paused ? 'paused' : (repoOpenWorkCount(repo) ? 'healthy' : 'idle'));
      const am = repo && repo.autoMerge !== false ? 'auto-merge on (AM/⚡)' : 'auto-merge off';
      const pause = repo && repo.paused ? '⏸ paused: agents do not write or receive work here' : '▶ active: agents may receive work here';
      const dot = status === 'error' ? 'red dot: CI red or repo error' : (status === 'blocked' ? 'amber dot: blocked/held work' : (status === 'paused' || status === 'idle' ? 'grey dot: paused/idle' : 'green dot: healthy/working'));
      return `${parts.full}\n${activeText}; ${pause}; ${am}; ${dot}\n${issueCount} actionable issues · ${prCount} open hive PRs · ${held} blocked/held`;
    }
    function repoCollapsedTileHtml(repo, data, extraCount) {
      if (extraCount > 0) {
        return `<button type="button" class="repo-collapsed-tile idle" data-action="openRepoCollapsedMore" data-stop="1" title="${esc(extraCount + ' more configured repos — expand Projects to see all')}" aria-label="${esc(extraCount + ' more configured repos — expand Projects to see all')}"><span class="agent-tile-name">+${extraCount} more</span><span class="agent-up-next" aria-hidden="true"></span><span class="agent-tile-dot" aria-hidden="true"></span><span class="agent-tile-next">expand Projects</span></button>`;
      }
      const parts = repoDisplayParts(repo);
      const issueCount = Array.isArray(repo && repo.actionableIssues) ? repo.actionableIssues.length : Number(repo && (repo.openIssues || repo.open_issues || repo.issues)) || 0;
      const prCount = (Array.isArray(repo && repo.openPrs) ? repo.openPrs.length : Number(repo && (repo.openPRs || repo.open_prs || repo.prs)) || 0) + (Array.isArray(repo && repo.heldPrs) ? repo.heldPrs.length : 0);
      const held = repoHeldCount(repo);
      const working = repoIsWorking(repo, data);
      const status = repoCollapsedStatus(repo, data);
      const classes = ['repo-collapsed-tile', status, working ? 'working' : ''].filter(Boolean).join(' ');
      const am = repo && repo.autoMerge !== false ? '<span class="repo-collapsed-am" title="Auto-merge enabled">AM</span>' : '';
      const play = repo && repo.paused ? '⏸' : '▶';
      const active = working ? '<span class="repo-collapsed-am" title="Currently being worked">ACTIVE</span>' : '';
      return `<button type="button" class="${classes}" data-action="openRepoCollapsedTile" data-arg0="${esc(parts.full)}" data-repo="${esc(parts.full)}" data-stop="1" title="${esc(repoCollapsedTileTitle(repo, data))}" aria-label="${esc(repoCollapsedTileTitle(repo, data))}"><span class="agent-tile-name"><span class="repo-collapsed-owner">${esc(parts.owner)}</span>${esc(parts.name)}</span><span class="repo-collapsed-icons"><span title="${repo && repo.paused ? 'Repo paused' : 'Repo active'}">${play}</span>${am}${active}</span><span class="agent-tile-dot" data-status="${esc(status)}" aria-hidden="true"></span><span class="agent-tile-next">${esc(issueCount + ' issues · ' + prCount + ' PRs · ' + held + ' hold' + (held === 1 ? '' : 's'))}</span></button>`;
    }
    function reposCollapsedTilesHtml(repos, data) {
      const list = sortReposForCollapsedTiles(repos, data);
      if (!list.length) return '';
      const visible = list.slice(0, REPOS_COLLAPSED_TILE_LIMIT);
      const more = Math.max(0, list.length - visible.length);
      return `<span class="repos-collapsed-tiles" data-mini-summary="repos-section">${visible.map(r => repoCollapsedTileHtml(r, data, 0)).join('')}${more ? repoCollapsedTileHtml(null, data, more) : ''}</span>`;
    }
    function contributorsSectionSummary(contributors, pool) {
      contributors = Array.isArray(contributors) ? contributors : [];
      const online = pool && pool.online != null ? (Number(pool.online) || 0) : contributors.filter(c => c && c.online).length;
      const total = contributors.length || Number(pool && pool.registered) || 0;
      const ranked = contributors.slice().sort((a, b) => (Number(b.total_tasks_completed || b.tasks_completed) || 0) - (Number(a.total_tasks_completed || a.tasks_completed) || 0));
      const top = ranked[0] && (ranked[0].github_username || ranked[0].login);
      if (top) return `${total} contributors · ${online} online · top: @${top}`;
      return total ? `${total} contributors · ${online} online` : '0 contributors';
    }
    function inceptionSectionSummary() {
      const campaigns = Array.isArray(_campaigns) ? _campaigns : [];
      const activeStatuses = { active: true, running: true, working: true, in_progress: true, started: true, open: true };
      let active = campaigns.filter(c => {
        const status = String((c && (c.status || c.current_stage)) || '').toLowerCase().replace(/[-\s]+/g, '_');
        return activeStatuses[status] || (!!status && status !== 'complete' && status !== 'completed' && status !== 'parked' && status !== 'archived');
      }).length;
      let proposals = campaigns.length;
      if (_inceptionState && _inceptionState.phase) {
        proposals = Math.max(1, proposals);
        active = Math.max(1, active);
      }
      if (!proposals && !active) return 'no active proposals';
      return `${proposals} proposal${proposals === 1 ? '' : 's'} · ${active} in progress`;
    }
    function refreshDashboardSectionSummaries(data) {
      data = data || window._lastStatus || {};
      const repos = data.repos || [];
      const openPrs = repos.reduce((n, r) => n + ((r.openPrs || r.prs || []).length || Number(r.open_prs || r.openPRs || 0) || 0), 0);
      const issues = repos.reduce((n, r) => n + ((r.actionableIssues || r.issues || []).length || Number(r.openIssues || r.open_issues || 0) || 0), 0);
      setSectionSummary('repos-section', `${repos.length} repos · ${openPrs} open PRs · ${issues} issues`);
      const adv = data.advisoryDigest || {};
      const advCount = Number(adv.open || adv.openAdvisories || adv.total || adv.count || 0) || ((adv.items || adv.advisories || []).length || 0);
      const advWhen = relativeTimeShort(adv.generatedAt || adv.updatedAt || adv.latest || data.timestamp);
      setSectionSummary('advisory-section', `${advCount} advisories${advWhen ? ' · ' + advWhen : ''}`);
      setSectionSummary('acmm-eval-section', `L${data.acmmLevel || '—'}${data.acmmEvalAt ? ' · last eval ' + relativeTimeShort(data.acmmEvalAt) : ''}`);
      const auditSummaryToday = Number(window._auditSummary && window._auditSummary.today);
      const auditToday = (Number.isFinite(auditSummaryToday) ? auditSummaryToday : (Number(data.auditEventsToday || 0) || document.querySelectorAll('#audit-panel .audit-entry').length || 0));
      setSectionSummary('audit-section', `${auditToday} events today`);
      if (typeof updateReviewSectionSummary === 'function') updateReviewSectionSummary();
      else { const rq = document.getElementById('review-queue-count'); setSectionSummary('review-queue-section', textOrDash(rq && rq.textContent)); }
      // The research question stays visible in the header in both states, so
      // the collapsed summary shows lab mode / experiment count instead of
      // repeating it.
      setSectionSummary('nous-section', nousSectionSummaryText(_nousCache));
      setSectionSummary('knowledge-section', kbFactsLabel(kbTotalFactsFromStats(window._kbCache || _kbCache || {})));
      const contrib = document.getElementById('contributors-status-badge');
      setSectionSummary('contributors-section', textOrDash(contrib && contrib.textContent));
      const health = document.getElementById('oc-health');
      setSectionSummary('debug-section', textOrDash(health && health.textContent));
      const agents = data.agents || window._lastAgents || [];
      setSectionSummary('agents-section', agentsSectionSummary(agents));
      const appr = document.getElementById('approvals-badge');
      setSectionSummary('approvals-section', `${textOrDash(appr && appr.textContent)} pending`);
      setSectionSummary('logs-section', `${agents.length} agents`);
      setSectionSummary('overview-section', data.governor && data.governor.mode ? `${data.governor.mode} · ${repos.length} repos` : `${repos.length} repos`);
      setSectionSummary('inception-section', inceptionSectionSummary());
      setSectionSummary('faq-section', '');
    }

    /** Has this browser ever persisted dashboard state? Decides legacy vs new-hive collapse defaults. */
    function dashboardHasPriorState() {
      try {
        for (let i = 0; i < localStorage.length; i++) {
          const k = localStorage.key(i) || '';
          if (k.indexOf(DASHBOARD_PRIOR_STATE_KEY_PREFIX) === 0 && DASHBOARD_PRIOR_STATE_IGNORED_KEYS.indexOf(k) < 0) return true;
        }
      } catch (e) { /* storage unavailable: treat as a fresh browser */ }
      return false;
    }
    /** One-time: record whether this browser predates the collapse defaults (#12014). Runs before any other dashboard key is written. */
    function dashboardSeedCollapseDefaults() {
      try {
        if (localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY)) return;
        localStorage.setItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY, dashboardHasPriorState() ? DASHBOARD_COLLAPSE_DEFAULTS_LEGACY : DASHBOARD_COLLAPSE_DEFAULTS_VERSION);
      } catch (e) { /* storage unavailable */ }
    }
    /** True when the new-hive defaults apply to this browser (not a legacy browser). */
    function dashboardCollapseDefaultsActive() {
      try { return localStorage.getItem(DASHBOARD_COLLAPSE_DEFAULTS_KEY) !== DASHBOARD_COLLAPSE_DEFAULTS_LEGACY; } catch (e) { return true; }
    }
    /** Default collapsed state for a section when the operator has not chosen one. */
    function dashboardDefaultCollapsed(sectionId) {
      sectionId = dashboardAliasSectionId(sectionId);
      return dashboardCollapseDefaultsActive() && DASHBOARD_DEFAULT_COLLAPSED_SECTIONS.indexOf(sectionId) >= 0;
    }
    /** Check whether a dashboard section is collapsed: explicit localStorage choice first, then the default. */
    function isSectionCollapsed(sectionId) {
      sectionId = dashboardAliasSectionId(sectionId);
      if (sectionId === 'cost-panel' && localStorage.getItem('hive-cost-collapsed') === '1') return true;
      if (sectionId === 'advisory-digest-section' && localStorage.getItem('hive-collapse-advisory-digest-main') === 'true') return true;
      const raw = localStorage.getItem(SECTION_LS_PREFIX + sectionId);
      if (raw === '1') return true;
      if (raw === '0') return false;
      return dashboardDefaultCollapsed(sectionId);
    }
    /** Write the per-section key: '1' collapsed, '0' explicitly expanded where the default is collapsed, removed otherwise. */
    function writeSectionCollapsedKey(sectionId, collapsed) {
      if (collapsed) {
        localStorage.setItem(SECTION_LS_PREFIX + sectionId, '1');
      } else if (dashboardDefaultCollapsed(sectionId)) {
        localStorage.setItem(SECTION_LS_PREFIX + sectionId, '0');
      } else {
        localStorage.removeItem(SECTION_LS_PREFIX + sectionId);
      }
    }
    /** Persist section collapsed state */
    function setSectionCollapsed(sectionId, collapsed) {
      sectionId = dashboardAliasSectionId(sectionId);
      writeSectionCollapsedKey(sectionId, collapsed);
      if (sectionId === 'cost-panel') {
        if (collapsed) localStorage.setItem('hive-cost-collapsed', '1');
        else localStorage.removeItem('hive-cost-collapsed');
      }
      if (sectionId === 'advisory-digest-section') localStorage.removeItem('hive-collapse-advisory-digest-main');
      dashboardUpdateLayoutMenuState();
    }
    /** Remove the expanded max-height cap after the max-height transition finishes. */
    function releaseSectionMaxHeightAfterTransition(body) {
      if (!body) return;
      let done = false;
      const release = () => {
        if (done) return;
        done = true;
        body.removeEventListener('transitionend', onEnd);
        if (!body.classList.contains('collapsed')) body.style.maxHeight = SECTION_EXPANDED_MAX_HEIGHT;
      };
      const onEnd = (event) => {
        if (event.target === body && event.propertyName === SECTION_MAX_HEIGHT_PROPERTY) release();
      };
      body.addEventListener('transitionend', onEnd);
      setTimeout(release, SECTION_TRANSITION_MS);
    }
    function sectionHeadlineSelector() {
      return '.sec-headline,[data-collapsed-keep]';
    }
    function clearCollapsedHeadlineVisibility(body) {
      if (!body || !body.querySelectorAll) return;
      body.classList.remove('collapsed-with-headline');
      body.querySelectorAll('.collapsed-hidden').forEach(el => el.classList.remove('collapsed-hidden'));
      body.querySelectorAll('.collapsed-control-hidden').forEach(el => el.classList.remove('collapsed-control-hidden'));
    }
    function applyCollapsedHeadlineVisibility(body, collapsed) {
      if (!body || !body.querySelectorAll) return false;
      const headlines = Array.from(body.querySelectorAll(sectionHeadlineSelector())).filter(el => el && el.closest && el.closest('.dash-card-body, .section-body') === body);
      clearCollapsedHeadlineVisibility(body);
      if (!collapsed || headlines.length === 0) return false;
      const keep = sectionHeadlineSelector();
      body.classList.add('collapsed-with-headline');
      const visit = (parent) => {
        Array.from(parent.children || []).forEach(child => {
          if (!child || !child.matches) return;
          const isHeadline = child.matches(keep);
          const containsHeadline = !!(child.querySelector && child.querySelector(keep));
          if (isHeadline) return;
          if (containsHeadline) {
            visit(child);
            return;
          }
          child.classList.add('collapsed-hidden');
        });
      };
      visit(body);
      body.querySelectorAll('.sec-headline [data-collapsed-control], .sec-headline .collapsed-control').forEach(el => el.classList.add('collapsed-control-hidden'));
      return true;
    }

    /** Reflect a section's collapsed state onto the body while keeping transitions animated. */
    function applySectionBodyCollapse(body, collapsed, animate) {
      if (!body) return;
      if (collapsed) {
        const hasHeadline = applyCollapsedHeadlineVisibility(body, true);
        if (animate && !hasHeadline) {
          body.style.maxHeight = body.scrollHeight + 'px';
          void body.offsetHeight;
        }
        body.classList.add('collapsed');
        body.style.maxHeight = hasHeadline ? SECTION_EXPANDED_MAX_HEIGHT : body.style.maxHeight;
      } else {
        clearCollapsedHeadlineVisibility(body);
        // Restore content visibility before reading layout or replaying sparklines.
        body.classList.remove('collapsed');
        if (animate) {
          body.style.maxHeight = body.scrollHeight + 'px';
          releaseSectionMaxHeightAfterTransition(body);
        } else {
          body.style.maxHeight = SECTION_EXPANDED_MAX_HEIGHT;
        }
      }
    }
    /** Toggle a section's collapsed state and animate */
    function toggleSection(sectionId, maybeSectionId, opts) {
      let options = opts || {};
      if (sectionId && typeof sectionId === 'object') {
        sectionId = maybeSectionId;
      } else if (maybeSectionId && typeof maybeSectionId === 'object') {
        options = maybeSectionId;
      }
      const section = document.getElementById(sectionId);
      const wasNavPeek = section && section.getAttribute('data-nav-peek') === '1';
      let collapsed;
      if (wasNavPeek && !options.navPeek) {
        section.removeAttribute('data-nav-peek');
        if (_dashboardNavPeekSectionId === sectionId) _dashboardNavPeekSectionId = null;
        collapsed = false;
      } else if (Object.prototype.hasOwnProperty.call(options, 'collapsed')) {
        collapsed = !!options.collapsed;
      } else {
        collapsed = !isSectionCollapsed(sectionId);
      }
      if (options.persist !== false) setSectionCollapsed(sectionId, collapsed);
      const body = document.querySelector(`#${sectionId} .dash-card-body, #${sectionId} .section-body`);
      const chevron = document.querySelector(`#${sectionId} .dash-card-header .section-chevron, #${sectionId} .section-chevron`);
      const card = document.querySelector(`#${sectionId} .dash-card`);
      const header = document.querySelector(`#${sectionId} .dash-card-header, #${sectionId} .section-header-toggle`);
      applySectionBodyCollapse(body, collapsed, options.animate !== false);
      const hasCollapsedHeadline = !!(body && body.classList && body.classList.contains('collapsed-with-headline'));
      if (chevron) chevron.classList.toggle('collapsed', collapsed);
      if (card) {
        card.classList.toggle('collapsed', collapsed);
        card.classList.toggle('has-collapsed-headline', hasCollapsedHeadline);
      }
      if (section) {
        section.classList.toggle('collapsed', collapsed);
        section.classList.toggle('has-collapsed-headline', hasCollapsedHeadline);
      }
      if (header) header.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      if (!collapsed && section) replaySparklinesIn(section, { force: true });
      if (sectionId === 'overview-section' && !collapsed) triggerOverviewAnimations('expand');
      if (sectionId === 'lifecycle-section' && !collapsed && typeof lcScheduleLayout === 'function') lcScheduleLayout();
      if (sectionId === 'logs-section' && !collapsed && typeof agentLogsResumeIfSkipped === 'function') agentLogsResumeIfSkipped();
    }
    /** Apply persisted collapsed state to a section after render */
    function applySectionCollapse(sectionId) {
      const section = document.getElementById(sectionId);
      const navPeek = section && section.getAttribute('data-nav-peek') === '1';
      const collapsed = navPeek ? false : isSectionCollapsed(sectionId);
      const body = document.querySelector(`#${sectionId} .dash-card-body, #${sectionId} .section-body`);
      const chevron = document.querySelector(`#${sectionId} .dash-card-header .section-chevron, #${sectionId} .section-chevron`);
      const card = document.querySelector(`#${sectionId} .dash-card`);
      const header = document.querySelector(`#${sectionId} .dash-card-header, #${sectionId} .section-header-toggle`);
      if (!navPeek && _dashboardNavPeekSectionId === sectionId) _dashboardNavPeekSectionId = null;
      normalizeSectionCardChrome(sectionId);
      applySectionBodyCollapse(body, collapsed, false);
      const hasCollapsedHeadline = !!(body && body.classList && body.classList.contains('collapsed-with-headline'));
      if (chevron) chevron.classList.toggle('collapsed', collapsed);
      if (card) {
        card.classList.toggle('collapsed', collapsed);
        card.classList.toggle('has-collapsed-headline', hasCollapsedHeadline);
      }
      if (section) {
        section.classList.toggle('collapsed', collapsed);
        section.classList.toggle('has-collapsed-headline', hasCollapsedHeadline);
      }
      if (header) header.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      if (!collapsed && section) replaySparklinesIn(section);
    }
    var _dashboardNavPeekSectionId = null;
    function navPeekSectionId() {
      let sectionId = _dashboardNavPeekSectionId;
      let section = sectionId ? document.getElementById(sectionId) : null;
      if (!section || section.getAttribute('data-nav-peek') !== '1') {
        section = document.querySelector('[data-nav-peek="1"]');
        sectionId = section && section.id;
      }
      return sectionId || null;
    }
    function navPeekExpand(sectionId) {
      const section = document.getElementById(sectionId);
      if (!section || section.getAttribute('data-nav-peek') === '1' || !isSectionCollapsed(sectionId)) return false;
      toggleSection(sectionId, { collapsed: false, persist: false, navPeek: true });
      section.setAttribute('data-nav-peek', '1');
      _dashboardNavPeekSectionId = sectionId;
      return true;
    }
    function navPeekRestore(nextSectionId, opts) {
      const options = opts || {};
      let sectionId = navPeekSectionId();
      let section = sectionId ? document.getElementById(sectionId) : null;
      if (!sectionId || sectionId === nextSectionId) return false;
      _dashboardNavPeekSectionId = null;
      if (!section || section.getAttribute('data-nav-peek') !== '1') return false;
      section.removeAttribute('data-nav-peek');
      if (!isSectionCollapsed(sectionId)) return false;
      toggleSection(sectionId, { collapsed: true, persist: false, navPeek: true, animate: !options.instant });
      return true;
    }
