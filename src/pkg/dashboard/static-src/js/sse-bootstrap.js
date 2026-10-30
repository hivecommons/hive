    var _eventSource = null;
    var _eventSourceReconnectTimer = null;
    var _sseLastFrameAtMs = 0;
    var _sseOpenedAtMs = 0;
    function noteSSEFrame() { _sseLastFrameAtMs = Date.now(); }
    // sseWatchdogTick recycles a silent stream (see sseStreamIdle). `force`
    // (tab became visible again) always refetches /api/status: Safari and
    // Chrome throttle hidden-tab timers to once a minute or less, so the tick
    // may not have run while the tab was in the background and the socket may
    // have died during a sleep the watchdog never saw.
    function sseWatchdogTick(force) {
      const idle = sseStreamIdle(Date.now(), _sseLastFrameAtMs, _sseOpenedAtMs, SSE_IDLE_RECONNECT_MS);
      if (idle) {
        console.warn('[hive] /api/events silent for >' + Math.round(SSE_IDLE_RECONNECT_MS / 1000) + 's — reconnecting');
        connect();
      }
      if (idle || force) refreshStatus();
    }
    function connect() {
      if (_eventSourceReconnectTimer) { clearTimeout(_eventSourceReconnectTimer); _eventSourceReconnectTimer = null; }
      if (_eventSource) { _eventSource.onerror = null; _eventSource.onmessage = null; _eventSource.close(); _eventSource = null; }
      const source = new EventSource('/api/events');
      _eventSource = source;
      _sseOpenedAtMs = Date.now();
      _sseLastFrameAtMs = 0;
      const conn = document.getElementById('conn');

      source.onopen = noteSSEFrame;
      // Server-side idle keepalive (sseKeepaliveInterval); carries no state,
      // it only proves the socket is alive.
      source.addEventListener('keepalive', noteSSEFrame);

      source.onmessage = (e) => {
        noteSSEFrame();
        try {
          const data = JSON.parse(e.data);
          render(data);
          if (conn) {
            conn.className = 'connection live';
            conn.innerHTML = '<span class="dot-live">●</span> live';
          }
          // Append to local history for instant sparkline updates
          const snap = {
            t: Date.now(),
            govIssues: data.governor?.issues || 0,
            govPrs: data.governor?.prs || 0,
            govTotal: (data.governor?.issues || 0) + (data.governor?.prs || 0),
            govHold: data.hold?.total || 0,
            govActive: data.governor?.active ? 1 : 0,
            repos: {},
            agents: {},
          };
          for (const r of (data.repos || [])) snap.repos[r.name] = { issues: r.issues || 0, prs: r.prs || 0 };
          for (const a of (data.agents || [])) snap.agents[a.name] = { busy: a.busy === 'working' ? 1 : 0, restarts: a.restarts || 0 };
          // Token sparkline data
          snap.tokens = {};
          snap.tokenTotal = 0;
          const _tt = data.tokens?.totals || {};
          snap.tokenInput = _tt.input || 0;
          snap.tokenOutput = _tt.output || 0;
          snap.tokenCacheRead = _tt.cacheRead || 0;
          snap.tokenCacheCreate = _tt.cacheCreate || 0;
          snap.tokenMessages = _tt.messages || 0;
          const _ba = data.tokens?.byAgent || {};
          for (const [name, stats] of Object.entries(_ba)) {
            const t = (stats.input || 0) + (stats.output || 0) + (stats.cacheRead || 0);
            snap.tokens[name] = t;
            snap.tokenTotal += t;
          }
          // Per-model token data
          snap.tokenModels = {};
          const _bm = data.tokens?.byModel || {};
          for (const [name, stats] of Object.entries(_bm)) {
            snap.tokenModels[name] = (stats.input || 0) + (stats.output || 0) + (stats.cacheRead || 0);
          }
          pushDashboardRing(historyData, snap, DASHBOARD_HISTORY_CAP);
          persistSparklineHistory();
        } catch (err) {
          console.error('SSE parse error:', err);
        }
      };

      // A status publish just rebuilt the server-side stage tracker: refetch
      // now (ETag keeps an unchanged body to one 304) rather than on the next
      // STAGE_TRACKER_POLL_MS tick. Only once the panel has loaded itself.
      source.addEventListener('stage-tracker', () => {
        noteSSEFrame();
        if (_stageTrackerSig || _stageTrackerEtag) stageTrackerNudge();
      });

      source.addEventListener('agent-status', (e) => {
        noteSSEFrame();
        try {
          const payload = JSON.parse(e.data);
          noteAgentNowUpdate(payload.timestamp);
          window._lastAgents = payload.agents || [];
          window._configuredAgents = payload.configuredAgents || window._configuredAgents || [];
          if (window._lastStatus) {
            window._lastStatus.agents = payload.agents || [];
          }
          ocUpdateSidebarAgents();
          if (_ocSelectedAgent) {
            ocRenderAgentDetail();
          }
          const lastData = window._lastStatus || {};
          if (lastData.agents) {
            applyPinOverrides(lastData.agents);
            renderAgents(lastData.agents);
            // Keep the cost table's "running agents" column live as agents
            // switch models or pause/resume between cost polls.
            if (_lastCost) renderCost(_lastCost);
          }
          ocUpdateFocusedState();
          const _lvl = (lastData.acmmLevel != null) ? lastData.acmmLevel : 0;
          const _pa = lastData.acmmPackAgents || null;
          applyACMMVisibility(_lvl, _pa);
          refreshWelcomeAutoChecks(lastData);
        } catch (err) {
          console.error('SSE agent-status parse error:', err);
        }
      });

      source.onerror = () => {
        if (conn) {
          conn.className = 'connection';
          conn.innerHTML = '<span class="dot-dead">●</span> reconnecting...';
        }
        source.close();
        if (_eventSource === source) _eventSource = null;
        if (!_eventSourceReconnectTimer) {
          _eventSourceReconnectTimer = setTimeout(function() { _eventSourceReconnectTimer = null; connect(); }, 3000);
        }
      };
    }

    // Set project name and repo from API
    fetch("/api/config").then(r => r.json()).then(cfg => {
      const ghBase = cfg.github_base_url || 'https://github.com';
      window._githubBaseUrl = ghBase;
      // Org slug, used by the Installation-ID hint to render this hive's own
      // installed-app settings URL rather than a generic placeholder.
      window._githubOrg = cfg.org || '';
      window._repoIssueBandConfig = normalizeIssueBandConfig(cfg.dashboard_issue_bands || {});
      if (window._lastStatus && Array.isArray(window._lastStatus.repos)) repaintReposForWidth();
      else renderRepoLegend();
      // The banner may have rendered before this fetch resolved, in which case
      // its hint shows the fallback host/org. Refresh it now that both are known.
      if (typeof renderGitHubAppInstallIdHint === 'function') {
        renderGitHubAppInstallIdHint(window._lastStatus || null);
      }
      if (window._lastStatus && Array.isArray(window._lastStatus.systemAlerts)) {
        renderSystemAlerts(window._lastStatus.systemAlerts);
      }
      // Who agents author PRs/commits as — the App bot ("<slug>[bot]") when this
      // hive runs App-authored mode, else the configured ai_author. Surfaced in
      // the user menu so operators can find the write identity near their own.
      // ai_author_effective already incorporates ai_author (else the App bot,
      // else empty). Don't fall back to a raw ai_author — an App-less hive has
      // no author and should render "—", not a stale personal value.
      window._aiAuthor = cfg.ai_author_effective || '';
      renderPRAuthorInMenu();
      // Hub My Hives page for the user-menu shortcut (#9696). Empty on a
      // standalone hive, which keeps the entry hidden.
      window._myHivesURL = cfg.my_hives_url || '';
      renderMyHivesMenuItem();
      const repoDisplay = cfg.org && cfg.primaryRepo && !cfg.primaryRepo.includes('/')
        ? cfg.org + '/' + cfg.primaryRepo
        : (cfg.primaryRepo || '');
      const el = document.getElementById("project-name");
      if (el && repoDisplay) {
        el.textContent = "for " + repoDisplay;
        if (!(window.hivePopoutMode && window.hivePopoutMode())) document.title = "Hive Dashboard for " + repoDisplay;
      }
      const ocEl = document.getElementById("oc-project-name");
      if (ocEl && repoDisplay) ocEl.textContent = repoDisplay;
      if (repoDisplay) window._primaryRepo = repoDisplay;
      // Read-only issue-filter note under Repositories. Present in /api/config
      // only when project.issue_filter is configured (edit it on Settings
      // → Labels, "Required labels"); textContent (never
      // innerHTML) because label names are repo-controlled text.
      const iflt = cfg.issue_filter || null;
      const ifltReq = (iflt && iflt.require_labels) || [];
      const rt = (iflt && iflt.reporter_trust) || null;
      const rtOn = !!(rt && rt.enabled);
      const noteParts = [];
      if (ifltReq.length) {
        noteParts.push('🏷️ Issue filter active — agents only work issues labeled: ' + ifltReq.join(', '));
      }
      if (rtOn) {
        // #9665: say the reporter rule in one line. Lists fall back to the
        // server defaults when the operator has not overridden them.
        const trusted = (rt.trusted_associations && rt.trusted_associations.length) ? rt.trusted_associations : ['OWNER', 'MEMBER', 'COLLABORATOR'];
        const triage = (rt.untrusted_require_labels && rt.untrusted_require_labels.length) ? rt.untrusted_require_labels : ['triage/accepted'];
        noteParts.push('👤 Reporter trust active — issues from ' + trusted.map(a => String(a).toLowerCase()).join('/') + ' reporters are worked as-is; anyone else\'s need a maintainer to add ' + triage.join(' or ') + ' first, and their PRs are held for a maintainer at every level');
      }
      if (noteParts.length) {
        const note = document.getElementById('repos-issue-filter');
        if (note) {
          note.textContent = noteParts.join('  ·  ');
          note.style.display = '';
        }
      }
    }).catch(() => {});

    // Initial fetch then SSE — these two are critical for first paint
    tickNavbarClock();
    dashboardSetInterval('navbar-clock', tickNavbarClock, NAVBAR_CLOCK_TICK_MS);
    fetch('/api/status').then(r => r.json()).then(render).catch(console.error);
    dashboardSetInterval('sse-watchdog', function() { sseWatchdogTick(false); }, SSE_WATCHDOG_TICK_MS);
    document.addEventListener('visibilitychange', function() {
      if (document.visibilityState === 'visible') sseWatchdogTick(true);
      if (document.visibilityState === 'visible') { agentLogsResumeIfSkipped(); stageTrackerResumeIfSkipped(); }
      if (typeof stageTrackerNoteVisibility === 'function') stageTrackerNoteVisibility(document.visibilityState !== 'visible');
    });
    // Fetch persistent trends for sparklines (day/week)
    window._trendData = [];
    function applyTrends(d) {
      // Trends API returns {evals: [...], tokenHistory: [...]}
      const evals = d && d.evals ? d.evals : (Array.isArray(d) ? d : []);
      window._trendData = evals;
      // Cache token history from server for merging into historyData
      window._serverTokenHistory = d && d.tokenHistory ? d.tokenHistory : [];
    }
    // fetch('/api/trends') + fetchTimeline() — deferred to _deferredInit
    const TREND_REFRESH_MS = 900000;
    window._timelineData = [];
    // Returns its promise. _deferredInit calls this as
    // `fetchTimeline().then(...)`, and without the return that is `.then` on
    // undefined: an uncaught TypeError on every load which left the whole
    // deferred chain unresolved forever (#6581). Every other entry in that
    // list is an `async function`, which returns a promise for free; this one
    // is not, so the return has to be written.
    function fetchTimeline() {
      return fetch('/api/timeline').then(r => r.json()).then(d => { window._timelineData = d; }).catch(() => {});
    }
    const TIMELINE_REFRESH_MS = 60000;
